package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type hostDirUsage struct {
	Label     string     `json:"label"`
	Path      string     `json:"path"`
	Exists    bool       `json:"exists"`
	Bytes     int64      `json:"bytes"`
	Files     int        `json:"files"`
	DumpFiles int        `json:"dumpFiles"`
	DumpBytes int64      `json:"dumpBytes"`
	Complete  bool       `json:"complete"`
	NewestAt  *time.Time `json:"newestUtc,omitempty"`
}

type hostOffenderProber struct {
	resolvePS            func() (string, error)
	env                  func(string) []string
	run                  func(ctx context.Context, psExe string, env []string, script string) ([]byte, error)
	now                  func() time.Time
	timeout, minInterval time.Duration

	mu          sync.Mutex
	latest      []hostDirUsage
	latestAt    time.Time
	lastAttempt time.Time
	lastErr     string
	measuring   bool
}

func newWfHostOffenderProber() *hostOffenderProber {
	p := &hostOffenderProber{resolvePS: resolvePowerShell, env: wslInteropEnv, now: time.Now, timeout: 20 * time.Second, minInterval: 10 * time.Minute}
	p.run = func(ctx context.Context, psExe string, env []string, script string) ([]byte, error) {
		cmd := hiddenCommand(ctx, psExe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
		cmd.Env = env
		return cmd.Output()
	}
	return p
}

func cloneHostDirUsages(in []hostDirUsage) []hostDirUsage {
	return append([]hostDirUsage(nil), in...)
}

func (p *hostOffenderProber) Measure(parent context.Context) ([]hostDirUsage, bool) {
	if p == nil {
		return nil, false
	}
	now := p.now().UTC()
	p.mu.Lock()
	if !p.lastAttempt.IsZero() && now.Sub(p.lastAttempt) < p.minInterval {
		latest := cloneHostDirUsages(p.latest)
		p.mu.Unlock()
		return latest, false
	}
	if p.measuring {
		latest := cloneHostDirUsages(p.latest)
		p.mu.Unlock()
		return latest, false
	}
	p.measuring = true
	p.lastAttempt = now
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	ps, err := p.resolvePS()
	var out []byte
	if err == nil {
		out, err = p.run(ctx, ps, p.env(ps), hostOffenderScript())
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	var usages []hostDirUsage
	if err == nil {
		last := ""
		scan := bufio.NewScanner(strings.NewReader(string(out)))
		for scan.Scan() {
			if line := strings.TrimSpace(scan.Text()); line != "" {
				last = line
			}
		}
		if last == "" || json.Unmarshal([]byte(last), &usages) != nil {
			err = fmt.Errorf("invalid PowerShell offender output: %s", boundDetail(last))
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.measuring = false
	if err != nil {
		p.lastErr = boundDetail(redactSensitive(err.Error()))
		return cloneHostDirUsages(p.latest), false
	}
	for i := range usages {
		usages[i].Label = redactSensitive(usages[i].Label)
		usages[i].Path = redactSensitive(usages[i].Path)
	}
	p.latest, p.latestAt, p.lastErr = cloneHostDirUsages(usages), now, ""
	return cloneHostDirUsages(usages), true
}

func (p *hostOffenderProber) Latest() ([]hostDirUsage, time.Time, bool) {
	if p == nil {
		return nil, time.Time{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneHostDirUsages(p.latest), p.latestAt, !p.latestAt.IsZero()
}

func hostOffenderScript() string {
	return `$ErrorActionPreference='Continue';$candidates=@(@{label='%TEMP%\wsl-crashes';path=(Join-Path $env:TEMP 'wsl-crashes')},@{label='%LOCALAPPDATA%\Temp\wsl-crashes';path=(Join-Path $env:LOCALAPPDATA 'Temp\wsl-crashes')},@{label='%TEMP%';path=$env:TEMP},@{label='%SystemRoot%\Temp';path=(Join-Path $env:SystemRoot 'Temp')});$seen=@{};$rows=@();foreach($c in $candidates){$p=[string]$c.path;if([string]::IsNullOrWhiteSpace($p)){continue};try{$full=[IO.Path]::GetFullPath($p)}catch{$full=$p};$key=$full.ToLowerInvariant();if($seen.ContainsKey($key)){continue};$seen[$key]=$true;$exists=Test-Path -LiteralPath $p -PathType Container;$bytes=[int64]0;$count=0;$dumps=0;$dumpBytes=[int64]0;$newest=$null;$complete=$true;if($exists){try{$files=@(Get-ChildItem -LiteralPath $p -File -Recurse -Force -ErrorAction SilentlyContinue | Select-Object -First 20000);$count=$files.Count;$complete=$count -lt 20000;foreach($f in $files){$bytes+=[int64]$f.Length;if($f.Extension -eq '.dmp'){$dumps++;$dumpBytes+=[int64]$f.Length};if($null -eq $newest -or $f.LastWriteTimeUtc -gt $newest){$newest=$f.LastWriteTimeUtc}}}catch{$complete=$false}};$rows+=@{label=$c.label;path=$full;exists=$exists;bytes=$bytes;files=$count;dumpFiles=$dumps;dumpBytes=$dumpBytes;complete=$complete;newestUtc=$(if($null -eq $newest){$null}else{$newest.ToString('o')})}};$rows|ConvertTo-Json -Compress -Depth 3`
}

func pickHostOffender(usages []hostDirUsage, minBytes int64) *hostDirUsage {
	var largest int64
	for _, usage := range usages {
		if usage.Exists && usage.Bytes > largest {
			largest = usage.Bytes
		}
	}
	if largest < minBytes {
		return nil
	}
	candidates := make([]hostDirUsage, 0, len(usages))
	for _, usage := range usages {
		if usage.Exists && usage.Bytes >= minBytes && usage.Bytes*2 >= largest {
			candidates = append(candidates, usage)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	depth := func(path string) int { return strings.Count(path, `\`) + strings.Count(path, "/") }
	sort.SliceStable(candidates, func(i, j int) bool {
		di, dj := depth(candidates[i].Path), depth(candidates[j].Path)
		if di != dj {
			return di > dj
		}
		if (candidates[i].DumpFiles > 0) != (candidates[j].DumpFiles > 0) {
			return candidates[i].DumpFiles > 0
		}
		return candidates[i].Bytes > candidates[j].Bytes
	})
	chosen := candidates[0]
	return &chosen
}

// hostBytesText keeps the familiar "%.1f GB" wording for anything at or above
// a gigabyte, but drops to MB below it so a small dump total does not render as
// a useless "0.0 GB".
func hostBytesText(bytes int64) string {
	const gb = 1024 * 1024 * 1024
	if bytes >= gb {
		return fmt.Sprintf("%.1f GB", float64(bytes)/gb)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}

// hostOffenderText describes the winning directory. Dump bytes and directory
// bytes are separate measurements: a directory holding one stray .dmp is not a
// directory made of crash dumps, so only attribute the whole size to dumps when
// the dumps actually account for most of it.
func hostOffenderText(o *hostDirUsage) string {
	if o == nil {
		return ""
	}
	var text string
	switch {
	case o.DumpFiles > 0 && o.DumpBytes > 0 && o.DumpBytes*2 >= o.Bytes:
		text = fmt.Sprintf("%s of WSL crash dumps (%d .dmp files) in %s", hostBytesText(o.DumpBytes), o.DumpFiles, o.Label)
	case o.DumpFiles > 0 && o.DumpBytes > 0:
		text = fmt.Sprintf("%s (%d files) in %s, of which %s is WSL crash dumps (%d .dmp files)", hostBytesText(o.Bytes), o.Files, o.Label, hostBytesText(o.DumpBytes), o.DumpFiles)
	case o.DumpFiles > 0:
		// Older probes counted dumps without sizing them; report the count
		// only rather than inventing a dump size from the directory total.
		text = fmt.Sprintf("%s (%d files) in %s (incl. %d .dmp)", hostBytesText(o.Bytes), o.Files, o.Label, o.DumpFiles)
	default:
		text = fmt.Sprintf("%s (%d files) in %s", hostBytesText(o.Bytes), o.Files, o.Label)
	}
	if !o.Complete {
		text += " (measurement incomplete)"
	}
	return text
}
