package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	crashDumpKeepNewest     = 2
	crashDumpRotateInterval = time.Hour
)

type crashDumpFile struct {
	Path       string    `json:"path"`
	Bytes      int64     `json:"bytes"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type crashDumpListing struct {
	Root  string          `json:"root"`
	Files []crashDumpFile `json:"files"`
}

var runHostMaintenancePowerShell = func(ctx context.Context, script string) ([]byte, error) {
	ps, err := resolvePowerShell()
	if err != nil {
		return nil, err
	}
	cmd := hiddenCommand(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = wslInteropEnv(ps)
	return cmd.Output()
}

func crashDumpRoot(localAppData string) (string, bool) {
	canonical, ok := canonicalWindowsPath(localAppData)
	if !ok {
		return "", false
	}
	return canonical + `\Temp\wsl-crashes`, true
}

func crashDumpListScript(root string) string {
	quoted := strings.ReplaceAll(root, "'", "''")
	return crashDumpNoReparseScript + fmt.Sprintf(`$r=[IO.Path]::GetFullPath('%s'); $f=@(); if(Test-Path -LiteralPath $r){Assert-NoReparse $r; $f=Get-ChildItem -LiteralPath $r -File -Filter '*.dmp' | ForEach-Object {Assert-NoReparse $_.FullName; @{path=$_.FullName;bytes=$_.Length;modifiedAt=$_.LastWriteTimeUtc.ToString('o')}}}; @{root=$r;files=@($f)} | ConvertTo-Json -Compress -Depth 4`, quoted)
}

// Refuse junctions/symlinks at the target and every ancestor. Repeat immediately
// before deletion: lexical containment alone does not bound Windows reparse
// targets. This is a conservative refusal, not a claim of atomic path locking.
const crashDumpNoReparseScript = `$ErrorActionPreference='Stop'; function Assert-NoReparse([string]$p) { while($p) { $item=Get-Item -LiteralPath $p -Force -ErrorAction Stop; if($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Crash-dump path contains a reparse point' }; $parent=Split-Path -LiteralPath $p; if($parent -eq $p) { break }; $p=$parent } }; `

func crashDumpDeleteScript(file crashDumpFile) string {
	return crashDumpNoReparseScript + fmt.Sprintf(`$p='%s'; Assert-NoReparse $p; $item=Get-Item -LiteralPath $p -Force -ErrorAction Stop; if($item.PSIsContainer -or $item.Length -ne %d -or $item.LastWriteTimeUtc.Ticks -ne ([DateTimeOffset]::Parse('%s')).UtcDateTime.Ticks) { throw 'Crash-dump file changed since enumeration' }; Remove-Item -LiteralPath $p -Force -ErrorAction Stop`, strings.ReplaceAll(file.Path, "'", "''"), file.Bytes, file.ModifiedAt.UTC().Format(time.RFC3339Nano))
}

func (e *hostStorageService) rotateWSLCrashDumps(ctx context.Context, localAppData string) (int64, error) {
	if e == nil {
		return 0, nil
	}
	// Serialize API and janitor rotations together, not just async dispatches.
	e.hostCrashDumpMu.Lock()
	defer e.hostCrashDumpMu.Unlock()
	if !e.resolveCrashDumpRotation().Value {
		return 0, nil
	}
	root, ok := crashDumpRoot(localAppData)
	if !ok {
		return 0, errors.New("WSL crash-dump root is not canonical")
	}
	out, err := runHostMaintenancePowerShell(ctx, crashDumpListScript(root))
	if err != nil {
		return 0, fmt.Errorf("enumerate WSL crash dumps: %w", err)
	}
	var listing crashDumpListing
	if err := json.Unmarshal(out, &listing); err != nil {
		return 0, fmt.Errorf("parse WSL crash dumps: %w", err)
	}
	listedRoot, ok := canonicalWindowsPath(listing.Root)
	if !ok || !strings.EqualFold(root, listedRoot) {
		return 0, errors.New("out-of-scope crash-dump root")
	}
	seen := make(map[string]bool, len(listing.Files))
	for i, file := range listing.Files {
		path, ok := canonicalWindowsPath(file.Path)
		slash := strings.LastIndex(path, `\`)
		if !ok || slash < 0 || !strings.EqualFold(path[:slash], root) || !strings.HasSuffix(strings.ToLower(path), ".dmp") {
			return 0, errors.New("out-of-scope crash-dump path")
		}
		key := strings.ToLower(path)
		if seen[key] || file.Bytes < 0 || file.ModifiedAt.IsZero() {
			return 0, errors.New("duplicate or invalid crash-dump metadata")
		}
		seen[key] = true
		listing.Files[i].Path = path
	}
	sort.Slice(listing.Files, func(i, j int) bool { return listing.Files[i].ModifiedAt.After(listing.Files[j].ModifiedAt) })
	var reclaimed int64
	for i := len(listing.Files) - 1; i >= crashDumpKeepNewest; i-- {
		if err := ctx.Err(); err != nil {
			return reclaimed, err
		}
		if !e.resolveCrashDumpRotation().Value {
			return reclaimed, errors.New("crash-dump rotation consent was revoked")
		}
		file := listing.Files[i]
		if _, err := runHostMaintenancePowerShell(ctx, crashDumpDeleteScript(file)); err != nil {
			return reclaimed, fmt.Errorf("delete WSL crash dump: %w", err)
		}
		reclaimed += max(int64(0), file.Bytes)
		if e.hostLedger != nil {
			// Listing size is an estimate; no host-free measurement was made.
			estimate := file.Bytes
			e.hostLedger.AppendCleanupOperation(storageCleanupOperation{At: e.clockNow(), Kind: "rotate_crash_dumps", Source: "host-maintenance", Layer: "host", Executed: true, Complete: true, EstimatedBytes: &estimate})
		}
	}
	return reclaimed, nil
}

func (e *hostStorageService) hostMaintenanceOnSample(ctx context.Context, p diskPressureState) {
	if e == nil || !p.HostKnown || !e.resolveCrashDumpRotation().Value || p.LocalAppData == "" || (!p.HostLow && (p.HostOffender == nil || p.HostOffender.DumpFiles <= crashDumpKeepNewest)) {
		return
	}
	e.hostCrashDumpMu.Lock()
	now := e.clockNow()
	if !e.hostCrashDumpAt.IsZero() && now.Sub(e.hostCrashDumpAt) < crashDumpRotateInterval {
		e.hostCrashDumpMu.Unlock()
		return
	}
	e.hostCrashDumpAt = now
	e.hostCrashDumpMu.Unlock()
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := e.rotateWSLCrashDumps(c, p.LocalAppData); err != nil && e.hostLedger != nil {
		e.hostLedger.AppendMaintenance(e.bootID, "rotate-crash-dumps", "error="+boundDetail(err.Error()))
	}
}

// dispatchHostMaintenanceOnSample keeps slow Windows interop off the janitor's
// select loop, so environment wakeups continue while a rotation is in flight.
func (e *hostStorageService) dispatchHostMaintenanceOnSample(ctx context.Context, p diskPressureState) {
	if e == nil || !p.HostKnown || !e.resolveCrashDumpRotation().Value || p.LocalAppData == "" || (!p.HostLow && (p.HostOffender == nil || p.HostOffender.DumpFiles <= crashDumpKeepNewest)) {
		return
	}
	if !e.hostCrashDumpRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.hostCrashDumpRunning.Store(false)
		e.hostMaintenanceOnSample(ctx, p)
	}()
}

func (a *apiServer) rotateHostCrashDumps(w http.ResponseWriter, r *http.Request) {
	e := a.hostStorageServiceOr503(w)
	if e == nil {
		return
	}
	if !e.resolveCrashDumpRotation().Value {
		jsonError(w, http.StatusConflict, "crash-dump rotation is not consented; set resources.rotateWSLCrashDumps or SOLU_WORKFLOW_ROTATE_WSL_CRASH_DUMPS=1")
		return
	}
	p := e.hostDiskLatestPressure("generic")
	if p.LocalAppData == "" {
		jsonError(w, http.StatusConflict, "host storage has not resolved %LOCALAPPDATA%; refresh host storage first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	reclaimed, err := e.rotateWSLCrashDumps(ctx, p.LocalAppData)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deletedBytesEstimate": reclaimed, "reclaimedBytes": nil, "verified": false})
}
