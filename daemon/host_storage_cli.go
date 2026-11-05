package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	hostStorageCLIProber = func() hostStorageSource { return newWfHostStorageProber() }
	hostStorageCLIRun    = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return hiddenCommand(ctx, name, args...).CombinedOutput()
	}
	hostStorageCLIGetRequest = hostStorageCLIGet
	hostStorageCLIWSLVersion = func(ctx context.Context) ([]byte, error) {
		cmd := hiddenCommand(ctx, "wsl.exe", "--version")
		cmd.Env = wslInteropEnv("wsl.exe")
		return cmd.CombinedOutput()
	}
	hostStorageCLIWindowsScript = hostStorageCLIResolveWindowsScript
	hostStorageCLILedger        = func() *hostStorageLedger { return newHostStorageLedger(defaultDataDir()) }
)

type hostStorageCLIActivity struct {
	Running, HeavyRunning, RunningInvocations, RunningPanes int
}

func (a hostStorageCLIActivity) quiet() bool {
	return a.Running == 0 && a.HeavyRunning == 0 && a.RunningInvocations == 0 && a.RunningPanes == 0
}

type hostStorageCLIActivityError struct {
	Source string
	Err    error
}

func (e *hostStorageCLIActivityError) Error() string {
	return fmt.Sprintf("%s activity unavailable: %v", e.Source, e.Err)
}

type hostStorageCLITimerStatus struct {
	Available bool   `json:"available"`
	Enabled   bool   `json:"enabled"`
	Active    bool   `json:"active"`
	Skipped   bool   `json:"skipped"`
	Condition string `json:"condition,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type hostStorageCLIPolicyCap struct {
	Value  int    `json:"value"`
	Source string `json:"source"`
}

type hostStorageCLIPayload struct {
	Available bool               `json:"available"`
	State     string             `json:"state"`
	Sample    *systemHostDiskDTO `json:"sample"`
	Pressure  struct {
		GuestFreeMB   int64  `json:"guestFreeMB"`
		GuestFloorMB  int64  `json:"guestFloorMB"`
		GuestLow      bool   `json:"guestLow"`
		HostLow       bool   `json:"hostLow"`
		BlockingLayer string `json:"blockingLayer"`
	} `json:"pressure"`
	Policy map[string]hostStorageCLIPolicyCap `json:"policy"`
	Events []hostStorageEvent                 `json:"events"`
	// CapacitySamples and Operations are the separated projections. Events is
	// retained for compatibility with older daemons.
	CapacitySamples []hostStorageEvent        `json:"capacitySamples"`
	Operations      []storageCleanupOperation `json:"operations"`
}

func runHostStorageCommand() int {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: solud host-storage status|compact|prepare-trim [--execute]")
		return 2
	}
	return runHostStorageCLI(os.Args[2], os.Args[3:], os.Stdout, os.Stderr)
}

func runHostStorageCLI(sub string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("host-storage "+sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", "127.0.0.1:8787", "running solud daemon address")
	jsonOut := flags.Bool("json", false, "print JSON")
	dryRun := flags.Bool("dry-run", true, "show the maintenance plan without changing the guest or host")
	execute := flags.Bool("execute", false, "execute the explicit trim-preparation operation")
	ignoreActive := flags.Bool("ignore-active", false, "deprecated unsafe bypass (always refused)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	dryRunExplicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "dry-run" {
			dryRunExplicit = true
		}
	})
	base := strings.TrimRight(*addr, "/")
	if !strings.HasPrefix(base, "http") {
		base = "http://" + base
	}
	switch sub {
	case "status":
		return hostStorageCLIStatus(base, *jsonOut, stdout, stderr)
	case "compact":
		if *execute && dryRunExplicit && *dryRun {
			fmt.Fprintln(stderr, "--execute and --dry-run cannot be combined")
			return 2
		}
		if *execute {
			fmt.Fprintln(stderr, "compaction cannot execute inside WSL; use the separately gated elevated Windows command from the plan")
			return 2
		}
		if *ignoreActive {
			fmt.Fprintln(stderr, "--ignore-active is not a safety override; maintenance planning requires known-quiet activity")
			return 2
		}
		return hostStorageCLICompact(base, false, *jsonOut, stdout, stderr)
	case "prepare-trim":
		if *execute && dryRunExplicit && *dryRun {
			fmt.Fprintln(stderr, "--execute and --dry-run cannot be combined")
			return 2
		}
		if *ignoreActive {
			fmt.Fprintln(stderr, "--ignore-active is not a safety override; trim preparation requires known-quiet activity")
			return 2
		}
		return hostStorageCLIPrepareTrim(base, *execute, *jsonOut, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: solud host-storage status|compact|prepare-trim [--execute]")
		return 2
	}
}

func hostStorageCLIStatus(base string, jsonOut bool, stdout, stderr io.Writer) int {
	var payload hostStorageCLIPayload
	if err := hostStorageCLIGetRequest(base+"/api/system/host-storage?limit=10", &payload); err != nil {
		s := hostStorageCLIProber().Sample(context.Background())
		absCap, pctCap := resolveHostDiskFloorMBOverride(nil), resolveHostDiskFloorPctOverride(nil)
		hysteresis := resolveHostDiskHysteresisMBOverride(nil)
		browserReserve := resolveBrowserHostReserveMBOverride(nil)
		hostFreeMB, hostTotalMB := s.FreeBytes/(1024*1024), s.TotalBytes/(1024*1024)
		hostFloorMB := hostDiskEffectiveFloorMB(hostTotalMB, absCap.Value, pctCap.Value)
		guestFloor := resolveDiskFloorMBOverride(nil).Value
		guestFreeBytes, guestKnown := diskFreeBytes(defaultDataDir())
		guestFreeMB := guestFreeBytes / (1024 * 1024)
		guestLow := guestKnown && guestFloor > 0 && guestFreeMB < int64(guestFloor)
		hostLow := s.Available && hostFloorMB > 0 && hostFreeMB < hostFloorMB
		blocking := "none"
		switch {
		case guestLow && hostLow:
			blocking = "both"
		case guestLow:
			blocking = "guest"
		case hostLow:
			blocking = "host"
		}
		p := diskPressureState{GuestKnown: guestKnown, GuestLow: guestLow, GuestFreeMB: guestFreeMB, GuestFloorMB: int64(guestFloor), HostKnown: s.Available, HostLow: hostLow, HostState: s.State, HostVolume: s.Volume, HostDistro: s.Distro, HostFreeMB: hostFreeMB, HostTotalMB: hostTotalMB, HostFloorMB: hostFloorMB, HostFloorPct: pctCap.Value, HostHysteresisMB: int64(hysteresis.Value), HostSampledAt: s.At, VHDXPath: s.VHDXPathRaw, VHDXPhysicalMB: s.VHDXPhysicalBytes / (1024 * 1024), VHDXLogicalMB: s.VHDXLogicalBytes / (1024 * 1024), VHDXSparse: s.VHDXSparse, BlockingLayer: blocking}
		payload.Available, payload.State, payload.Sample = s.Available, s.State, systemHostDiskDTOFromSample(systemHostDiskFromPressure(p))
		payload.Pressure.GuestFreeMB, payload.Pressure.GuestFloorMB, payload.Pressure.GuestLow, payload.Pressure.HostLow, payload.Pressure.BlockingLayer = guestFreeMB, int64(guestFloor), guestLow, hostLow, blocking
		payload.Policy = map[string]hostStorageCLIPolicyCap{"floorMB": {Value: absCap.Value, Source: absCap.Source}, "floorPct": {Value: pctCap.Value, Source: pctCap.Source}, "hysteresisMB": {Value: hysteresis.Value, Source: hysteresis.Source}, "browserReserveMB": {Value: browserReserve.Value, Source: browserReserve.Source}}
		if s.VHDXPathRaw != "" {
			payload.Sample.VHDXPath = s.VHDXPathRaw
		}
		fmt.Fprintf(stderr, "daemon unavailable; using bounded local WSL probe: %v\n", err)
	}
	if jsonOut {
		_ = json.NewEncoder(stdout).Encode(payload)
		return 0
	}
	if payload.Sample == nil {
		fmt.Fprintf(stderr, "host storage unavailable: %s\n", payload.State)
		return 1
	}
	h := payload.Sample
	fmt.Fprintf(stdout, "Windows host storage (%s, distro %s)\n", h.Volume, h.Distro)
	fmt.Fprintf(stdout, "  host:  %s free of %s\n", hostStorageCLIMB(h.FreeMB), hostStorageCLIMB(h.TotalMB))
	absPolicy, pctPolicy := payload.Policy["floorMB"], payload.Policy["floorPct"]
	fmt.Fprintf(stdout, "  floor: %s (%d%% policy; MB source %s, %% source %s)\n", hostStorageCLIMB(h.FloorMB), h.FloorPct, absPolicy.Source, pctPolicy.Source)
	fmt.Fprintf(stdout, "  VHDX:  %s\n         %s physical / %s logical; sparse=%s\n", h.VHDXPath, hostStorageCLIMB(h.VHDXPhysicalMB), hostStorageCLIMB(h.VHDXLogicalMB), hostStorageCLIBool(h.Sparse))
	fmt.Fprintf(stdout, "  guest: %d MB free; floor %d MB\n  blocking layer: %s\n", payload.Pressure.GuestFreeMB, payload.Pressure.GuestFloorMB, payload.Pressure.BlockingLayer)
	capacity := payload.CapacitySamples
	if len(capacity) == 0 {
		for _, ev := range payload.Events {
			if ev.Kind == "sample" || ev.Kind == "boot" || ev.Kind == "crossing" || ev.Kind == "host_loss" {
				capacity = append(capacity, ev)
			}
		}
	}
	if len(capacity) > 0 {
		fmt.Fprintln(stdout, "  capacity samples:")
		for _, ev := range capacity {
			fmt.Fprintf(stdout, "    %s %s hostFreeMB=%d vhdxPhysicalMB=%d\n", ev.At.Format(time.RFC3339), ev.Kind, ev.HostFreeMB, ev.VHDXPhysicalMB)
		}
	}
	ops := payload.Operations
	if len(ops) == 0 {
		for _, ev := range payload.Events {
			if op, ok := normalizeLegacyMaintenanceEvent(ev); ok {
				ops = append(ops, op)
			}
		}
	}
	if len(ops) > 0 {
		fmt.Fprintln(stdout, "  operations:")
		for _, op := range ops {
			preview := "preview"
			if op.Executed {
				preview = "executed"
			}
			fmt.Fprintf(stdout, "    %s %s %s layer=%s\n", op.At.Format(time.RFC3339), op.Kind, preview, op.Layer)
		}
	}
	return 0
}

func hostStorageCLICompact(base string, ignoreActive, jsonOut bool, stdout, stderr io.Writer) int {
	if ignoreActive {
		fmt.Fprintln(stderr, "active-work bypass is disabled for host maintenance")
		return 2
	}
	prober := hostStorageCLIProber()
	s := prober.Refresh(context.Background())
	path := s.VHDXPathRaw
	if path == "" {
		path = s.VHDXPath
	}
	if s.State != hostStorageOK || s.FreeBytes < 0 || s.TotalBytes <= 0 {
		fmt.Fprintf(stderr, "refusing unsafe or unresolved VHDX target %q (state %s, registrations %d)\n", redactSensitive(path), s.State, s.RegistrationCount)
		return 2
	}
	if err := validateRegisteredVHDXTarget(s, path); err != nil {
		fmt.Fprintf(stderr, "refusing unsafe or unresolved VHDX target %q: %v\n", redactSensitive(path), err)
		return 2
	}
	activity, err := hostStorageCLIProbeActivity(base)
	if err != nil {
		fmt.Fprintf(stderr, "refusing maintenance while activity is unknown: %v\n", err)
		return 3
	}
	if !activity.quiet() {
		fmt.Fprintf(stderr, "finish or pause workflows first: running=%d heavy=%d invocations=%d panes=%d\n", activity.Running, activity.HeavyRunning, activity.RunningInvocations, activity.RunningPanes)
		return 3
	}
	timer := hostStorageCLIInspectTrimTimer(context.Background())
	versionCtx, cancelVersion := context.WithTimeout(context.Background(), 8*time.Second)
	versionOut, versionErr := hostStorageCLIWSLVersion(versionCtx)
	cancelVersion()
	_, used, dfOK := hostStorageCLIDF()
	scriptPath, err := hostStorageCLIWindowsScript(context.Background())
	if err != nil || strings.TrimSpace(scriptPath) == "" {
		fmt.Fprintf(stderr, "cannot resolve the repository maintenance script safely: %v\n", err)
		return 2
	}
	command := fmt.Sprintf(`powershell.exe -ExecutionPolicy Bypass -File "%s" -Distro %s -Vhdx "%s" -Mode Optimize -Execute`, scriptPath, s.Distro, path)
	if jsonOut {
		payload := map[string]any{"dryRun": true, "mutationFree": true, "distro": s.Distro, "vhdx": redactSensitive(path), "ownerSid": s.OwnerSID, "registrationCount": s.RegistrationCount, "timer": timer, "windowsCommand": command}
		if versionErr == nil {
			payload["wslVersion"] = strings.TrimSpace(string(versionOut))
		}
		if dfOK {
			payload["guestUsedBytes"] = used
		}
		_ = json.NewEncoder(stdout).Encode(payload)
		return 0
	}
	fmt.Fprintln(stdout, "WSL VHDX maintenance plan (read-only; no trim, shutdown, attachment, compaction, or telemetry was performed)")
	fmt.Fprintf(stdout, "  registered target: %s (distro %s, owner %s)\n", redactSensitive(path), s.Distro, s.OwnerSID)
	fmt.Fprintf(stdout, "  observed: host free %d bytes; VHDX physical %d, logical %d, sparse=%s\n", s.FreeBytes, s.VHDXPhysicalBytes, s.VHDXLogicalBytes, hostStorageCLIBool(s.VHDXSparse))
	if dfOK {
		fmt.Fprintf(stdout, "  guest used: %d bytes (observed; not a reclaim prediction)\n", used)
	} else {
		fmt.Fprintln(stdout, "  guest used: unavailable")
	}
	if versionErr != nil {
		fmt.Fprintf(stdout, "  WSL version: unavailable (%v)\n", versionErr)
	} else {
		fmt.Fprintf(stdout, "  WSL version: %s\n", strings.TrimSpace(string(versionOut)))
	}
	hostStorageCLIPrintTimer(timer, stdout)
	fmt.Fprintln(stdout, "  prepare guest free blocks explicitly: solud host-storage prepare-trim --execute")
	fmt.Fprintf(stdout, "  after preparation, run from elevated Windows PowerShell:\n    %s\n", command)
	fmt.Fprintln(stdout, "The Windows script keeps the typed SHUTDOWN/attended launcher gate and performs Full mode only through a verified read-only attachment.")
	return 0
}

func hostStorageCLIPrepareTrim(base string, execute, jsonOut bool, stdout, stderr io.Writer) int {
	prober := hostStorageCLIProber()
	s := prober.Refresh(context.Background())
	path := s.VHDXPathRaw
	if path == "" {
		path = s.VHDXPath
	}
	if s.State != hostStorageOK || s.FreeBytes < 0 || s.TotalBytes <= 0 {
		fmt.Fprintf(stderr, "refusing trim preparation for unresolved registration (state %s, registrations %d)\n", s.State, s.RegistrationCount)
		return 2
	}
	if err := validateRegisteredVHDXTarget(s, path); err != nil {
		fmt.Fprintf(stderr, "refusing unsafe trim target: %v\n", err)
		return 2
	}
	activity, err := hostStorageCLIProbeActivity(base)
	if err != nil {
		fmt.Fprintf(stderr, "refusing trim preparation while activity is unknown: %v\n", err)
		return 3
	}
	if !activity.quiet() {
		fmt.Fprintf(stderr, "finish or pause workflows first: running=%d heavy=%d invocations=%d panes=%d\n", activity.Running, activity.HeavyRunning, activity.RunningInvocations, activity.RunningPanes)
		return 3
	}
	timer := hostStorageCLIInspectTrimTimer(context.Background())
	_, beforeUsed, beforeDFOK := hostStorageCLIDF()
	if !execute {
		if jsonOut {
			payload := map[string]any{"execute": false, "mutationFree": true, "distro": s.Distro, "vhdx": redactSensitive(path), "timer": timer, "ready": true}
			if beforeDFOK {
				payload["guestUsedBytes"] = beforeUsed
			}
			_ = json.NewEncoder(stdout).Encode(payload)
			return 0
		}
		fmt.Fprintln(stdout, "Trim preparation prerequisites are satisfied; no command was executed and no telemetry was written.")
		hostStorageCLIPrintTimer(timer, stdout)
		fmt.Fprintln(stdout, "Run explicitly while the board remains quiet: solud host-storage prepare-trim --execute")
		return 0
	}

	record := newHostStorageMaintenanceOperation("prepare-trim", s)
	record.RequestedMode = "fstrim"
	record.Status = "failed"
	if beforeDFOK {
		record.Before = &hostStorageMaintenanceMeasurement{PhysicalAllocationBytes: nonnegativeInt64Ptr(s.VHDXPhysicalBytes), GuestUsedBytes: nonnegativeInt64Ptr(beforeUsed), HostFreeBytes: nonnegativeInt64Ptr(s.FreeBytes)}
	} else {
		record.Before = &hostStorageMaintenanceMeasurement{PhysicalAllocationBytes: nonnegativeInt64Ptr(s.VHDXPhysicalBytes), HostFreeBytes: nonnegativeInt64Ptr(s.FreeBytes), UnavailableReason: "guest usage unavailable before trim"}
	}
	fail := func(stage, code string, err error) int {
		record.Failure = &hostStorageMaintenanceFailure{Stage: stage, Code: code, Message: err.Error()}
		after := hostStorageCLIProber().Refresh(context.Background())
		_, afterUsed, afterDFOK := hostStorageCLIDF()
		record.After = &hostStorageMaintenanceMeasurement{PhysicalAllocationBytes: nonnegativeInt64Ptr(after.VHDXPhysicalBytes), HostFreeBytes: nonnegativeInt64Ptr(after.FreeBytes)}
		if afterDFOK {
			record.After.GuestUsedBytes = nonnegativeInt64Ptr(afterUsed)
		} else {
			record.After.UnavailableReason = "guest usage unavailable after failed trim"
		}
		record.CompletedAt = time.Now().UTC()
		if appendErr := hostStorageCLILedger().AppendOperation(record); appendErr != nil {
			fmt.Fprintf(stderr, "trim preparation record rejected: %v\n", appendErr)
		}
		fmt.Fprintf(stderr, "trim preparation failed at %s: %v\n", stage, err)
		return 4
	}
	run := func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return hostStorageCLIRun(ctx, name, args...)
	}
	if _, err := run(time.Minute, "sync"); err != nil {
		return fail("sync-before", "sync_failed", err)
	}
	record.ActualMode = "fstrim"
	trimOut, trimErr := run(10*time.Minute, "fstrim", "-v", "/")
	_, syncAfterErr := run(time.Minute, "sync")
	if trimErr != nil {
		return fail("fstrim", "fstrim_failed", trimErr)
	}
	if syncAfterErr != nil {
		return fail("sync-after", "sync_failed", syncAfterErr)
	}
	record.Status = "succeeded"
	record.Failure = nil
	record.CompletedAt = time.Now().UTC()
	after := hostStorageCLIProber().Refresh(context.Background())
	_, afterUsed, afterDFOK := hostStorageCLIDF()
	record.After = &hostStorageMaintenanceMeasurement{PhysicalAllocationBytes: nonnegativeInt64Ptr(after.VHDXPhysicalBytes), HostFreeBytes: nonnegativeInt64Ptr(after.FreeBytes)}
	if afterDFOK {
		record.After.GuestUsedBytes = nonnegativeInt64Ptr(afterUsed)
	} else {
		record.After.UnavailableReason = "guest usage unavailable after trim"
	}
	if err := hostStorageCLILedger().AppendOperation(record); err != nil {
		fmt.Fprintf(stderr, "trim succeeded but its operation record was rejected: %v\n", err)
		return 5
	}
	if jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"operationId": record.OperationID, "status": record.Status, "trimOutput": strings.TrimSpace(string(trimOut))})
	} else {
		fmt.Fprintf(stdout, "Trim preparation completed (operation %s): %s\n", record.OperationID, strings.TrimSpace(string(trimOut)))
	}
	return 0
}

func hostStorageCLIResolveWindowsScript(ctx context.Context) (string, error) {
	var starts []string
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	if home, err := os.UserHomeDir(); err == nil {
		starts = append(starts, filepath.Join(home, "wsl-projects", "soluterminal"))
	}
	seen := map[string]bool{}
	for _, start := range starts {
		for dir := filepath.Clean(start); dir != "." && !seen[dir]; dir = filepath.Dir(dir) {
			seen[dir] = true
			script := filepath.Join(dir, "scripts", "wsl-vhdx-compact.ps1")
			if st, err := os.Stat(script); err == nil && !st.IsDir() {
				probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				out, err := hiddenCommand(probeCtx, "wslpath", "-w", script).Output()
				cancel()
				if err != nil {
					return "", err
				}
				return strings.TrimSpace(string(out)), nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}
	return "", fmt.Errorf("scripts/wsl-vhdx-compact.ps1 not found from cwd, executable, or standard checkout")
}

func hostStorageCLIGet(url string, dst any) error {
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst)
}

func hostStorageCLIProbeActivity(base string) (hostStorageCLIActivity, error) {
	var counts struct {
		Running     *int `json:"running"`
		Heavy       *int `json:"heavyRunning"`
		Invocations *int `json:"runningInvocations"`
		Panes       *int `json:"runningPanes"`
	}
	if err := hostStorageCLIGetRequest(base+"/api/system/activity", &counts); err != nil {
		return hostStorageCLIActivity{}, &hostStorageCLIActivityError{Source: "system activity", Err: err}
	}
	if counts.Running == nil || counts.Heavy == nil || counts.Invocations == nil || counts.Panes == nil {
		return hostStorageCLIActivity{}, &hostStorageCLIActivityError{Source: "system activity", Err: errors.New("required activity fields missing")}
	}
	a := hostStorageCLIActivity{Running: *counts.Running, HeavyRunning: *counts.Heavy, RunningInvocations: *counts.Invocations, RunningPanes: *counts.Panes}
	if a.Running < 0 || a.HeavyRunning < 0 || a.RunningInvocations < 0 || a.RunningPanes < 0 {
		return hostStorageCLIActivity{}, &hostStorageCLIActivityError{Source: "activity", Err: errors.New("negative activity count")}
	}

	return a, nil
}

// hostStorageCLIActive returns the four-value activity summary consumed by the
// retained daemon/host_maintenance.go hostMaintenanceBoardActivity caller, whose
// only use is running+heavy+invocations+panes > 0 in weeklyGuestFstrim. Unknown
// activity therefore reports 1,0,0,0 so the weekly trim can never fail open
// while the board state is unproven. This four-int form must survive the
// richer hostStorageCLIProbeActivity accessor.
func hostStorageCLIActive(base string) (running, heavy, invocations, panes int) {
	a, err := hostStorageCLIProbeActivity(base)
	if err != nil {
		return 1, 0, 0, 0
	}
	return a.Running, a.HeavyRunning, a.RunningInvocations, a.RunningPanes
}

func validateRegisteredVHDXTarget(s hostStorageSample, supplied string) error {
	if s.RegistrationCount != 1 {
		return fmt.Errorf("expected exactly one current-owner registration, got %d", s.RegistrationCount)
	}
	if strings.TrimSpace(s.OwnerSID) == "" {
		return errors.New("current owner SID is unavailable")
	}
	if s.ReparsePoint {
		return errors.New("registered base or target is a reparse point")
	}
	local, ok := canonicalWindowsPath(s.LocalAppData)
	if !ok {
		return errors.New("canonical LocalAppData is unsafe")
	}
	base, ok := canonicalWindowsPath(s.BasePath)
	if !ok {
		return errors.New("canonical registered BasePath is unsafe")
	}
	target, ok := canonicalWindowsPath(s.VHDXPathRaw)
	if !ok {
		target, ok = canonicalWindowsPath(s.VHDXPath)
	}
	if !ok {
		return errors.New("canonical registered target is unsafe")
	}
	given, ok := canonicalWindowsPath(supplied)
	if !ok || !strings.EqualFold(given, target) {
		return errors.New("caller target does not exactly match the registration")
	}
	if !strings.EqualFold(target, base+`\ext4.vhdx`) {
		return errors.New("registered target is not exactly BasePath\\ext4.vhdx")
	}
	if !allowedLocalAppDataLayout(local, target) {
		return errors.New("registered target is outside the permitted current-owner LocalAppData layouts")
	}
	return nil
}

var windowsDrivePath = regexp.MustCompile(`^[A-Za-z]:\\`)

func canonicalWindowsPath(path string) (string, bool) {
	if path == "" || path != strings.TrimSpace(path) || strings.IndexByte(path, 0) >= 0 {
		return "", false
	}
	p := strings.ReplaceAll(path, "/", `\`)
	low := strings.ToLower(p)
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(low, `\\?\`) || strings.HasPrefix(low, `\\.\`) || !windowsDrivePath.MatchString(p) {
		return "", false
	}
	if strings.Count(p, ":") != 1 || strings.HasSuffix(p, `\`) {
		return "", false
	}
	parts := strings.Split(p[3:], `\`)
	if len(parts) == 0 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") || strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") {
			return "", false
		}
	}
	return strings.ToUpper(p[:1]) + p[1:], true
}

func allowedLocalAppDataLayout(localAppData, target string) bool {
	local, ok := canonicalWindowsPath(localAppData)
	if !ok {
		return false
	}
	target, ok = canonicalWindowsPath(target)
	if !ok {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(target), strings.ToLower(local)+`\`) {
		return false
	}
	rel := target[len(local)+1:]
	parts := strings.Split(rel, `\`)
	if len(parts) == 3 && strings.EqualFold(parts[0], "wsl") && safeWindowsComponent(parts[1]) && strings.EqualFold(parts[2], "ext4.vhdx") {
		return true
	}
	return len(parts) == 4 && strings.EqualFold(parts[0], "Packages") && safeWindowsComponent(parts[1]) && strings.EqualFold(parts[2], "LocalState") && strings.EqualFold(parts[3], "ext4.vhdx")
}

func safeWindowsComponent(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `<>:"/\|?*`) || strings.HasSuffix(value, " ") || strings.HasSuffix(value, ".") {
		return false
	}
	return true
}

func hostStorageCLIInspectTrimTimer(parent context.Context) hostStorageCLITimerStatus {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	out, err := hostStorageCLIRun(ctx, "systemctl", "show", "fstrim.timer", "--no-pager", "--property=UnitFileState,ActiveState,Result")
	if err != nil {
		return hostStorageCLITimerStatus{Detail: "fstrim.timer status unavailable: " + err.Error()}
	}
	status := hostStorageCLITimerStatus{Available: true}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "UnitFileState":
			status.Enabled = strings.EqualFold(value, "enabled")
		case "ActiveState":
			status.Active = strings.EqualFold(value, "active") || strings.EqualFold(value, "activating")
		case "Result":
			if strings.EqualFold(value, "skipped") {
				status.Skipped = true
			}
		}
	}
	catCtx, catCancel := context.WithTimeout(parent, 5*time.Second)
	defer catCancel()
	unit, unitErr := hostStorageCLIRun(catCtx, "systemctl", "cat", "fstrim.service", "--no-pager")
	if unitErr == nil {
		for _, line := range strings.Split(string(unit), "\n") {
			line = strings.TrimSpace(line)
			if strings.EqualFold(line, "ConditionVirtualization=!container") {
				status.Condition = "ConditionVirtualization=!container"
				if status.Enabled {
					status.Skipped = true
				}
				break
			}
		}
	}
	return status
}

func hostStorageCLIPrintTimer(status hostStorageCLITimerStatus, out io.Writer) {
	if !status.Available {
		fmt.Fprintf(out, "  fstrim.timer prerequisite: unknown (%s)\n", status.Detail)
		return
	}
	if status.Skipped && status.Condition == "ConditionVirtualization=!container" {
		fmt.Fprintln(out, "  fstrim.timer is enabled but skipped by ConditionVirtualization=!container; it does not satisfy preparation in WSL.")
		return
	}
	fmt.Fprintf(out, "  fstrim.timer: enabled=%t active=%t skipped=%t\n", status.Enabled, status.Active, status.Skipped)
}

func wSLVersionAtLeast2(out string) bool {
	re := regexp.MustCompile(`(?i)WSL[^0-9]+([0-9]+)\.([0-9]+)\.([0-9]+)`)
	m := re.FindStringSubmatch(out)
	if len(m) != 4 {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	return major >= 2
}

func hostStorageCLIDF() (total, used int64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := hostStorageCLIRun(ctx, "df", "-B1", "--output=size,used", "/")
	if err != nil {
		return
	}
	fields := strings.Fields(string(out))
	if len(fields) < 4 {
		return
	}
	total, e1 := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	used, e2 := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	return total, used, e1 == nil && e2 == nil
}

func hostStorageCLIMB(v *int64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f GB", float64(*v)/1024)
}
func hostStorageCLIBool(v *bool) string {
	if v == nil {
		return "unknown"
	}
	return strconv.FormatBool(*v)
}
