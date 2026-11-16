package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func registeredHostStorageSample(path string) hostStorageSample {
	return hostStorageSample{
		At: time.Now(), Available: true, State: hostStorageOK, Distro: "Ubuntu", Volume: `C:\`,
		VHDXPathRaw: path, BasePath: strings.TrimSuffix(path, `\ext4.vhdx`),
		LocalAppData: `C:\Users\alice\AppData\Local`, OwnerSID: "S-1-5-21-1000", RegistrationCount: 1,
		VHDXPhysicalBytes: 600 << 30, VHDXLogicalBytes: 1024 << 30, FreeBytes: 100 << 30, TotalBytes: 2 << 40,
	}
}

func quietHostStorageActivity(url string, dst any) error {
	return json.Unmarshal([]byte(`{"running":0,"heavyRunning":0,"runningInvocations":0,"runningPanes":0}`), dst)
}

func TestHostStorageStatusRendersHostGuestAndBlockingLayer(t *testing.T) {
	origGet := hostStorageCLIGetRequest
	defer func() { hostStorageCLIGetRequest = origGet }()
	hostStorageCLIGetRequest = func(_ string, dst any) error {
		return json.Unmarshal([]byte(`{"available":true,"state":"ok","sample":{"available":true,"state":"ok","volume":"C:\\\\","distro":"Ubuntu","freeMB":41000,"totalMB":2050000,"floorMB":102500,"floorPct":5,"hysteresisMB":4096,"low":true,"vhdxPath":"~\\\\AppData\\\\Local\\\\wsl\\\\{id}\\\\ext4.vhdx","vhdxPhysicalMB":566000,"vhdxLogicalMB":1048576,"sparse":false,"sampledAt":"2026-08-25T10:00:00Z","runbook":"docs/RUNBOOK-wsl-host-disk.md"},"policy":{"floorMB":{"value":102400,"source":"adaptive"},"floorPct":{"value":5,"source":"adaptive"}},"pressure":{"guestFreeMB":682000,"guestFloorMB":10240,"guestLow":false,"hostLow":true,"blockingLayer":"host"}}`), dst)
	}
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("status", nil, &out, &stderr); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	for _, want := range []string{"Windows host storage", "682000 MB free", "blocking layer: host", "MB source adaptive"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
}

func TestHostStorageStatusFallsBackToLocalProbe(t *testing.T) {
	origGet, origProber := hostStorageCLIGetRequest, hostStorageCLIProber
	defer func() { hostStorageCLIGetRequest, hostStorageCLIProber = origGet, origProber }()
	hostStorageCLIGetRequest = func(string, any) error { return errors.New("offline") }
	hostStorageCLIProber = func() hostStorageSource {
		return &fakeHostStorageSource{sample: hostStorageSample{At: time.Now(), Available: true, State: hostStorageOK, Distro: "Ubuntu", Volume: `C:\`, FreeBytes: 100 << 30, TotalBytes: 2 << 40, VHDXPathRaw: `C:\Users\alice\AppData\Local\wsl\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}\ext4.vhdx`}}
	}
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("status", nil, &out, &stderr); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "bounded local WSL probe") || !strings.Contains(out.String(), "floor:") || !strings.Contains(out.String(), "blocking layer:") {
		t.Fatalf("out=%s err=%s", out.String(), stderr.String())
	}
}

func TestHostStorageCompactRefusesUnsafeTarget(t *testing.T) {
	orig := hostStorageCLIProber
	defer func() { hostStorageCLIProber = orig }()
	src := &fakeHostStorageSource{sample: hostStorageSample{At: time.Now(), Available: true, State: hostStorageOK, VHDXPathRaw: `D:\stuff\disk.vhdx`, FreeBytes: 1, TotalBytes: 2}}
	hostStorageCLIProber = func() hostStorageSource { return src }
	var out, err bytes.Buffer
	if code := runHostStorageCLI("compact", []string{"--ignore-active"}, &out, &err); code != 2 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), err.String())
	}
}

func TestHostStorageCompactRefusesActiveWorkflows(t *testing.T) {
	origP, origG, origR := hostStorageCLIProber, hostStorageCLIGetRequest, hostStorageCLIRun
	defer func() { hostStorageCLIProber, hostStorageCLIGetRequest, hostStorageCLIRun = origP, origG, origR }()
	hostStorageCLIProber = func() hostStorageSource {
		return &fakeHostStorageSource{sample: registeredHostStorageSample(`C:\Users\alice\AppData\Local\wsl\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}\ext4.vhdx`)}
	}
	hostStorageCLIGetRequest = func(url string, dst any) error {
		return json.Unmarshal([]byte(`{"running":1,"heavyRunning":1,"runningInvocations":1,"runningPanes":1}`), dst)
	}
	hostStorageCLIRun = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("active workflows must be refused before invoking maintenance commands")
		return nil, nil
	}
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("compact", nil, &out, &stderr); code != 3 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "finish or pause workflows first") {
		t.Fatal(stderr.String())
	}
}

func TestHostStorageCompactDryRunIsMutationFree(t *testing.T) {
	origP, origR, origV, origScript, origGet, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIWSLVersion, hostStorageCLIWindowsScript, hostStorageCLIGetRequest, hostStorageCLILedger
	defer func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIWSLVersion, hostStorageCLIWindowsScript, hostStorageCLIGetRequest, hostStorageCLILedger = origP, origR, origV, origScript, origGet, origLedger
	}()
	path := `C:\Users\alice\AppData\Local\wsl\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}\ext4.vhdx`
	src := &fakeHostStorageSource{sample: registeredHostStorageSample(path)}
	hostStorageCLIProber = func() hostStorageSource { return src }
	hostStorageCLIGetRequest = quietHostStorageActivity
	ledgerDir := t.TempDir()
	hostStorageCLILedger = func() *hostStorageLedger { return newHostStorageLedger(ledgerDir) }
	var invoked []string
	hostStorageCLIRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		invoked = append(invoked, line)
		switch name {
		case "df":
			return []byte("1B-blocks Used\n1099511627776 429496729600\n"), nil
		case "systemctl":
			if len(args) > 0 && args[0] == "show" {
				return []byte("UnitFileState=enabled\nActiveState=inactive\nResult=success\n"), nil
			}
			return []byte("ConditionVirtualization=!container\n"), nil
		default:
			t.Fatalf("dry-run invoked unexpected command: %s", line)
			return nil, nil
		}
	}
	hostStorageCLIWSLVersion = func(context.Context) ([]byte, error) {
		return []byte("WSL version: 2.7.10.0"), nil
	}
	hostStorageCLIWindowsScript = func(context.Context) (string, error) { return `C:\repo\scripts\wsl-vhdx-compact.ps1`, nil }
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("compact", []string{"--addr", "127.0.0.1:1"}, &out, &stderr); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), `scripts\wsl-vhdx-compact.ps1`) || !strings.Contains(out.String(), "typed SHUTDOWN") || !strings.Contains(out.String(), "enabled but skipped") {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), `-File scripts\wsl-vhdx-compact.ps1`) {
		t.Fatalf("maintenance command uses a cwd-dependent script path: %s", out.String())
	}
	if strings.Contains(strings.ToLower(out.String()), "reclaim") && !strings.Contains(out.String(), "not a reclaim prediction") {
		t.Fatalf("plan emitted a reclaim estimate: %s", out.String())
	}
	for _, line := range invoked {
		name := strings.Fields(line)[0]
		if name == "fstrim" || name == "sync" {
			t.Fatalf("invoked forbidden mutating command: %s", line)
		}
		for _, forbidden := range []string{"--shutdown", "--manage", "--set-sparse", "Optimize-VHD"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("invoked forbidden command: %s", line)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(ledgerDir, "host_storage_events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dry run created ledger: %v", err)
	}
}

func TestHostStorageCompactNeverEstimatesReclaimWhenDFUnavailable(t *testing.T) {
	origP, origR, origV, origScript, origGet, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIWSLVersion, hostStorageCLIWindowsScript, hostStorageCLIGetRequest, hostStorageCLILedger
	defer func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIWSLVersion, hostStorageCLIWindowsScript, hostStorageCLIGetRequest, hostStorageCLILedger = origP, origR, origV, origScript, origGet, origLedger
	}()
	hostStorageCLIProber = func() hostStorageSource {
		return &fakeHostStorageSource{sample: registeredHostStorageSample(`C:\Users\alice\AppData\Local\wsl\Ubuntu\ext4.vhdx`)}
	}
	hostStorageCLIGetRequest = quietHostStorageActivity
	ledgerDir := t.TempDir()
	hostStorageCLILedger = func() *hostStorageLedger { return newHostStorageLedger(ledgerDir) }
	hostStorageCLIRun = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "df" {
			return nil, errors.New("df unavailable")
		}
		return nil, errors.New("timer unavailable")
	}
	hostStorageCLIWSLVersion = func(context.Context) ([]byte, error) { return []byte("WSL version: 2.7.10.0"), nil }
	hostStorageCLIWindowsScript = func(context.Context) (string, error) { return `C:\repo\scripts\wsl-vhdx-compact.ps1`, nil }
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("compact", []string{"--json"}, &out, &stderr); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	if strings.Contains(strings.ToLower(out.String()), "reclaim") {
		t.Fatalf("df failure produced reclaim output: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(ledgerDir, "host_storage_events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dry run created ledger: %v", err)
	}
}

func TestWfValidateRegisteredVHDXTargetLayoutsAndUnsafePaths(t *testing.T) {
	local := `C:\Users\owner\AppData\Local`
	valid := []string{
		local + `\wsl\Ubuntu\ext4.vhdx`,
		local + `\wsl\{5383f6f1-705c-4162-b2bd-40aea9d4a65d}\ext4.vhdx`,
		local + `\Packages\CanonicalGroupLimited.Ubuntu_79rhkp1fndgsc\LocalState\ext4.vhdx`,
	}
	for _, path := range valid {
		s := registeredHostStorageSample(path)
		s.LocalAppData = local
		if err := validateRegisteredVHDXTarget(s, path); err != nil {
			t.Errorf("valid path %q rejected: %v", path, err)
		}
	}
	base := registeredHostStorageSample(valid[0])
	base.LocalAppData = local
	for name, mutate := range map[string]func(*hostStorageSample) string{
		"duplicate":     func(s *hostStorageSample) string { s.RegistrationCount = 2; return s.VHDXPathRaw },
		"missing owner": func(s *hostStorageSample) string { s.OwnerSID = ""; return s.VHDXPathRaw },
		"reparse":       func(s *hostStorageSample) string { s.ReparsePoint = true; return s.VHDXPathRaw },
		"mismatch":      func(s *hostStorageSample) string { return local + `\wsl\Other\ext4.vhdx` },
		"outside": func(s *hostStorageSample) string {
			s.BasePath = `D:\Ubuntu`
			s.VHDXPathRaw = `D:\Ubuntu\ext4.vhdx`
			return s.VHDXPathRaw
		},
		"traversal": func(s *hostStorageSample) string {
			s.BasePath = local + `\wsl\..\Ubuntu`
			s.VHDXPathRaw = s.BasePath + `\ext4.vhdx`
			return s.VHDXPathRaw
		},
		"alternate stream": func(s *hostStorageSample) string { return s.VHDXPathRaw + `:stream` },
		"device":           func(s *hostStorageSample) string { return `\\?\C:\Users\owner\AppData\Local\wsl\Ubuntu\ext4.vhdx` },
		"unc":              func(s *hostStorageSample) string { return `\\server\share\ext4.vhdx` },
		"deeper": func(s *hostStorageSample) string {
			s.BasePath = local + `\wsl\team\Ubuntu`
			s.VHDXPathRaw = s.BasePath + `\ext4.vhdx`
			return s.VHDXPathRaw
		},
	} {
		s := base
		if err := validateRegisteredVHDXTarget(s, mutate(&s)); err == nil {
			t.Errorf("unsafe case %s was accepted: %+v", name, s)
		}
	}
}

func TestHostStorageActivityFailsClosedPerSourceAndCounter(t *testing.T) {
	orig := hostStorageCLIGetRequest
	defer func() { hostStorageCLIGetRequest = orig }()
	hostStorageCLIGetRequest = func(string, any) error { return errors.New("offline") }
	if _, err := hostStorageCLIProbeActivity("http://mock"); err == nil {
		t.Fatal("outage failed open")
	}
	fields := []string{"running", "heavyRunning", "runningInvocations", "runningPanes"}
	for _, field := range fields {
		for _, value := range []int{-1, 0, 1} {
			counts := map[string]int{"running": 0, "heavyRunning": 0, "runningInvocations": 0, "runningPanes": 0}
			counts[field] = value
			raw, _ := json.Marshal(counts)
			hostStorageCLIGetRequest = func(url string, dst any) error {
				if !strings.HasSuffix(url, "/api/system/activity") {
					t.Fatalf("retired endpoint: %s", url)
				}
				return json.Unmarshal(raw, dst)
			}
			a, err := hostStorageCLIProbeActivity("http://mock")
			if value < 0 && err == nil {
				t.Errorf("negative %s failed open", field)
			}
			if value >= 0 && (err != nil || a.quiet() != (value == 0)) {
				t.Errorf("counter %s=%d: %+v %v", field, value, a, err)
			}
		}
		counts := map[string]int{"running": 0, "heavyRunning": 0, "runningInvocations": 0, "runningPanes": 0}
		delete(counts, field)
		raw, _ := json.Marshal(counts)
		hostStorageCLIGetRequest = func(_ string, dst any) error { return json.Unmarshal(raw, dst) }
		if _, err := hostStorageCLIProbeActivity("http://mock"); err == nil {
			t.Errorf("missing %s failed open", field)
		}
	}
}

func TestHostStorageCLIActiveLegacyFourValueContract(t *testing.T) {
	// Compile-time guard for the retained daemon/host_maintenance.go
	// hostMaintenanceBoardActivity caller, which consumes the four-int
	// summary and must treat unknown activity as busy (fail closed).
	var legacy func(string) (int, int, int, int) = hostStorageCLIActive
	orig := hostStorageCLIGetRequest
	defer func() { hostStorageCLIGetRequest = orig }()

	hostStorageCLIGetRequest = func(string, any) error { return errors.New("offline") }
	if running, heavy, invocations, panes := legacy("http://mock"); running+heavy+invocations+panes <= 0 {
		t.Fatalf("activity error failed open: running=%d heavy=%d invocations=%d panes=%d", running, heavy, invocations, panes)
	}

	hostStorageCLIGetRequest = quietHostStorageActivity
	if running, heavy, invocations, panes := legacy("http://mock"); running != 0 || heavy != 0 || invocations != 0 || panes != 0 {
		t.Fatalf("quiet activity not preserved: running=%d heavy=%d invocations=%d panes=%d", running, heavy, invocations, panes)
	}

	hostStorageCLIGetRequest = func(url string, dst any) error {
		return json.Unmarshal([]byte(`{"running":1,"heavyRunning":1,"runningInvocations":1,"runningPanes":1}`), dst)
	}
	if running, heavy, invocations, panes := legacy("http://mock"); running != 1 || heavy != 1 || invocations != 1 || panes != 1 {
		t.Fatalf("active counts lost: running=%d heavy=%d invocations=%d panes=%d", running, heavy, invocations, panes)
	}
}

func TestHostStorageCLIInspectTrimTimer(t *testing.T) {
	tests := []struct {
		name    string
		showOut string
		showErr error
		catOut  string
		catErr  error
		want    hostStorageCLITimerStatus
	}{
		{
			name:    "status unavailable",
			showErr: errors.New("systemctl unavailable"),
			want:    hostStorageCLITimerStatus{Detail: "fstrim.timer status unavailable: systemctl unavailable"},
		},
		{
			name:    "enabled and active",
			showOut: "UnitFileState=enabled\nActiveState=active\nResult=success\n",
			want:    hostStorageCLITimerStatus{Available: true, Enabled: true, Active: true},
		},
		{
			name:    "activating counts as active",
			showOut: "UnitFileState=disabled\nActiveState=activating\nResult=success\n",
			want:    hostStorageCLITimerStatus{Available: true, Active: true},
		},
		{
			name:    "inactive and skipped",
			showOut: "UnitFileState=enabled\nActiveState=inactive\nResult=skipped\n",
			want:    hostStorageCLITimerStatus{Available: true, Enabled: true, Skipped: true},
		},
		{
			name:    "wsl virtualization condition",
			showOut: "UnitFileState=enabled\nActiveState=inactive\nResult=success\n",
			catOut:  "[Unit]\nConditionVirtualization=!container\n",
			want: hostStorageCLITimerStatus{
				Available: true,
				Enabled:   true,
				Skipped:   true,
				Condition: "ConditionVirtualization=!container",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig := hostStorageCLIRun
			t.Cleanup(func() { hostStorageCLIRun = orig })
			hostStorageCLIRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "systemctl" || len(args) == 0 {
					t.Fatalf("unexpected command: %s %v", name, args)
				}
				switch args[0] {
				case "show":
					return []byte(tc.showOut), tc.showErr
				case "cat":
					return []byte(tc.catOut), tc.catErr
				default:
					t.Fatalf("unexpected systemctl action: %v", args)
					return nil, nil
				}
			}

			if got := hostStorageCLIInspectTrimTimer(context.Background()); got != tc.want {
				t.Fatalf("timer status=%+v want %+v", got, tc.want)
			}
		})
	}
}

func TestHostStorageCLIPrintTimer(t *testing.T) {
	tests := []struct {
		name   string
		status hostStorageCLITimerStatus
		want   []string
	}{
		{
			name:   "unavailable",
			status: hostStorageCLITimerStatus{Detail: "systemctl unavailable"},
			want:   []string{"fstrim.timer prerequisite: unknown", "systemctl unavailable"},
		},
		{
			name: "enabled but skipped by virtualization condition",
			status: hostStorageCLITimerStatus{
				Available: true,
				Enabled:   true,
				Skipped:   true,
				Condition: "ConditionVirtualization=!container",
			},
			want: []string{"enabled but skipped", "ConditionVirtualization=!container", "does not satisfy preparation in WSL"},
		},
		{
			name:   "ordinary status",
			status: hostStorageCLITimerStatus{Available: true, Enabled: true, Active: true, Skipped: true},
			want:   []string{"fstrim.timer:", "enabled=true", "active=true", "skipped=true"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			hostStorageCLIPrintTimer(tc.status, &out)
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in %q", want, out.String())
				}
			}
		})
	}
}

func TestHostStoragePrepareTrimRejectsExplicitDryRunExecuteConflict(t *testing.T) {
	origP, origR, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLILedger
	t.Cleanup(func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLILedger = origP, origR, origLedger
	})
	hostStorageCLIProber = func() hostStorageSource {
		t.Fatal("conflicting flags reached the storage probe")
		return nil
	}
	hostStorageCLIRun = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("conflicting flags invoked a command")
		return nil, nil
	}
	hostStorageCLILedger = func() *hostStorageLedger {
		t.Fatal("conflicting flags reached the operation ledger")
		return nil
	}

	for _, args := range [][]string{
		{"--execute", "--dry-run"},
		{"--dry-run", "--execute"},
	} {
		var out, stderr bytes.Buffer
		if code := runHostStorageCLI("prepare-trim", args, &out, &stderr); code != 2 {
			t.Errorf("args=%v code=%d out=%s err=%s", args, code, out.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "--execute") || !strings.Contains(stderr.String(), "--dry-run") {
			t.Errorf("args=%v missing conflicting-flag diagnostic: %q", args, stderr.String())
		}
	}
}

func TestHostStoragePrepareTrimPlanAndExecute(t *testing.T) {
	origP, origR, origGet, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger
	defer func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger = origP, origR, origGet, origLedger
	}()
	sample := registeredHostStorageSample(`C:\Users\alice\AppData\Local\wsl\Ubuntu\ext4.vhdx`)
	hostStorageCLIProber = func() hostStorageSource { return &fakeHostStorageSource{sample: sample} }
	hostStorageCLIGetRequest = quietHostStorageActivity
	ledgerDir := t.TempDir()
	hostStorageCLILedger = func() *hostStorageLedger { return newHostStorageLedger(ledgerDir) }
	var calls []string
	hostStorageCLIRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch name {
		case "systemctl":
			if len(args) > 0 && args[0] == "show" {
				return []byte("UnitFileState=enabled\nActiveState=inactive\nResult=success\n"), nil
			}
			return []byte("[Unit]\nConditionVirtualization=!container\n"), nil
		case "df":
			return []byte("1B-blocks Used\n1000 600\n"), nil
		case "sync":
			return nil, nil
		case "fstrim":
			return []byte("/: 123 bytes trimmed"), nil
		default:
			return nil, fmt.Errorf("unexpected command %s", name)
		}
	}
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("prepare-trim", nil, &out, &stderr); code != 0 {
		t.Fatalf("plan code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "sync ") || strings.HasPrefix(call, "fstrim ") {
			t.Fatalf("plan mutated through %s", call)
		}
	}
	if !strings.Contains(out.String(), "enabled but skipped") {
		t.Fatalf("skipped timer was not actionable: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(ledgerDir, "host_storage_events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("plan wrote ledger: %v", err)
	}

	calls = nil
	out.Reset()
	stderr.Reset()
	if code := runHostStorageCLI("prepare-trim", []string{"--execute"}, &out, &stderr); code != 0 {
		t.Fatalf("execute code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	var mutations []string
	for _, call := range calls {
		if strings.HasPrefix(call, "sync ") || strings.HasPrefix(call, "fstrim ") {
			mutations = append(mutations, strings.TrimSpace(call))
		}
	}
	if got, want := strings.Join(mutations, ","), "sync,fstrim -v /,sync"; got != want {
		t.Fatalf("mutation order=%q want %q; all=%v", got, want, calls)
	}
	events := newHostStorageLedger(ledgerDir).Tail(10)
	if len(events) != 1 || events[0].SchemaVersion != 2 || events[0].Operation != "prepare-trim" || events[0].Status != "succeeded" || events[0].ActualMode != "fstrim" {
		t.Fatalf("trim record=%+v", events)
	}
}

func TestHostStoragePrepareTrimRecordsFreshAfterMeasurement(t *testing.T) {
	origP, origR, origGet, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger
	t.Cleanup(func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger = origP, origR, origGet, origLedger
	})

	path := `C:\Users\alice\AppData\Local\wsl\Ubuntu\ext4.vhdx`
	before := registeredHostStorageSample(path)
	after := before
	after.VHDXPhysicalBytes = 500 << 30
	after.FreeBytes = 90 << 30
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	// Seed a real prober inside its 30-second Refresh window. Reusing it for
	// the post-trim sample therefore returns the cached pre-trim measurement.
	cachedBefore := &hostStorageProber{
		now:         func() time.Time { return now },
		latest:      before,
		hasLatest:   true,
		lastRefresh: now,
	}
	probeCreations := 0
	hostStorageCLIProber = func() hostStorageSource {
		probeCreations++
		if probeCreations == 1 {
			return cachedBefore
		}
		return &fakeHostStorageSource{sample: after}
	}
	hostStorageCLIGetRequest = quietHostStorageActivity
	ledgerDir := t.TempDir()
	hostStorageCLILedger = func() *hostStorageLedger { return newHostStorageLedger(ledgerDir) }
	dfCalls := 0
	hostStorageCLIRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "systemctl":
			if len(args) > 0 && args[0] == "show" {
				return []byte("UnitFileState=enabled\nActiveState=active\nResult=success\n"), nil
			}
			return []byte("[Unit]\n"), nil
		case "df":
			dfCalls++
			if dfCalls == 1 {
				return []byte("1B-blocks Used\n1000 600\n"), nil
			}
			return []byte("1B-blocks Used\n1000 500\n"), nil
		case "sync":
			return nil, nil
		case "fstrim":
			return []byte("/: 123 bytes trimmed"), nil
		default:
			return nil, fmt.Errorf("unexpected command %s", name)
		}
	}

	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("prepare-trim", []string{"--execute"}, &out, &stderr); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	events := newHostStorageLedger(ledgerDir).Tail(10)
	if len(events) != 1 || events[0].After == nil {
		t.Fatalf("trim record=%+v", events)
	}
	measurement := events[0].After
	if measurement.PhysicalAllocationBytes == nil || *measurement.PhysicalAllocationBytes != after.VHDXPhysicalBytes ||
		measurement.HostFreeBytes == nil || *measurement.HostFreeBytes != after.FreeBytes {
		t.Fatalf("after measurement reused cached before sample: before=%+v after=%+v probes=%d", before, measurement, probeCreations)
	}
	if probeCreations < 2 {
		t.Fatalf("post-trim measurement did not use a fresh prober: creations=%d", probeCreations)
	}
}

func TestHostStoragePrepareTrimFailureRecordsFreshAfterMeasurement(t *testing.T) {
	origP, origR, origGet, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger
	t.Cleanup(func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger = origP, origR, origGet, origLedger
	})

	path := `C:\Users\alice\AppData\Local\wsl\Ubuntu\ext4.vhdx`
	before := registeredHostStorageSample(path)
	after := before
	after.VHDXPhysicalBytes = 550 << 30
	after.FreeBytes = 95 << 30
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cachedBefore := &hostStorageProber{
		now:         func() time.Time { return now },
		latest:      before,
		hasLatest:   true,
		lastRefresh: now,
	}
	probeCreations := 0
	hostStorageCLIProber = func() hostStorageSource {
		probeCreations++
		if probeCreations == 1 {
			return cachedBefore
		}
		return &fakeHostStorageSource{sample: after}
	}
	hostStorageCLIGetRequest = quietHostStorageActivity
	ledgerDir := t.TempDir()
	hostStorageCLILedger = func() *hostStorageLedger { return newHostStorageLedger(ledgerDir) }
	hostStorageCLIRun = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		switch name {
		case "systemctl":
			return nil, errors.New("unavailable")
		case "df":
			return []byte("1B-blocks Used\n1000 600\n"), nil
		case "sync":
			return nil, nil
		case "fstrim":
			return nil, errors.New("trim denied")
		default:
			return nil, fmt.Errorf("unexpected command %s", name)
		}
	}

	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("prepare-trim", []string{"--execute"}, &out, &stderr); code != 4 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	events := newHostStorageLedger(ledgerDir).Tail(10)
	if len(events) != 1 || events[0].After == nil || events[0].Status != "failed" {
		t.Fatalf("failed trim record=%+v", events)
	}
	measurement := events[0].After
	if measurement.PhysicalAllocationBytes == nil || *measurement.PhysicalAllocationBytes != after.VHDXPhysicalBytes ||
		measurement.HostFreeBytes == nil || *measurement.HostFreeBytes != after.FreeBytes {
		t.Fatalf("failed trim reused cached before sample: before=%+v after=%+v probes=%d", before, measurement, probeCreations)
	}
}

func TestHostStoragePrepareTrimFailureRecordedAndUnknownActivityRefused(t *testing.T) {
	origP, origR, origGet, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger
	defer func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger = origP, origR, origGet, origLedger
	}()
	sample := registeredHostStorageSample(`C:\Users\alice\AppData\Local\wsl\Ubuntu\ext4.vhdx`)
	hostStorageCLIProber = func() hostStorageSource { return &fakeHostStorageSource{sample: sample} }
	ledgerDir := t.TempDir()
	hostStorageCLILedger = func() *hostStorageLedger { return newHostStorageLedger(ledgerDir) }
	hostStorageCLIGetRequest = func(string, any) error { return errors.New("offline") }
	hostStorageCLIRun = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unknown activity reached a subprocess")
		return nil, nil
	}
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("prepare-trim", []string{"--execute"}, &out, &stderr); code != 3 {
		t.Fatalf("unknown activity code=%d err=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(ledgerDir, "host_storage_events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("refused operation wrote ledger: %v", err)
	}

	hostStorageCLIGetRequest = quietHostStorageActivity
	hostStorageCLIRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "systemctl":
			return nil, errors.New("unavailable")
		case "df":
			return []byte("1B-blocks Used\n1000 600\n"), nil
		case "sync":
			return nil, nil
		case "fstrim":
			return nil, errors.New("trim denied")
		}
		return nil, fmt.Errorf("unexpected %s %v", name, args)
	}
	out.Reset()
	stderr.Reset()
	if code := runHostStorageCLI("prepare-trim", []string{"--execute"}, &out, &stderr); code != 4 {
		t.Fatalf("trim failure code=%d err=%s", code, stderr.String())
	}
	events := newHostStorageLedger(ledgerDir).Tail(10)
	if len(events) != 1 || events[0].Status != "failed" || events[0].Failure == nil || events[0].Failure.Stage != "fstrim" {
		t.Fatalf("failed trim record=%+v", events)
	}
}

func TestHostStoragePrepareTrimReportsMissingReceipt(t *testing.T) {
	origP, origR, origGet, origLedger := hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger
	t.Cleanup(func() {
		hostStorageCLIProber, hostStorageCLIRun, hostStorageCLIGetRequest, hostStorageCLILedger = origP, origR, origGet, origLedger
	})
	sample := registeredHostStorageSample(`C:\Users\alice\AppData\Local\wsl\Ubuntu\ext4.vhdx`)
	hostStorageCLIProber = func() hostStorageSource { return &fakeHostStorageSource{sample: sample} }
	hostStorageCLIGetRequest = quietHostStorageActivity
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger := newHostStorageLedger(blockedParent)
	hostStorageCLILedger = func() *hostStorageLedger { return ledger }
	trimmed := false
	hostStorageCLIRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "systemctl":
			return []byte("UnitFileState=enabled\nActiveState=active\nResult=success\n"), nil
		case "df":
			return []byte("1B-blocks Used\n1000 600\n"), nil
		case "sync":
			return nil, nil
		case "fstrim":
			trimmed = true
			return []byte("/: 123 bytes trimmed"), nil
		default:
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		}
	}

	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("prepare-trim", []string{"--execute"}, &out, &stderr); code != 5 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	if !trimmed || !strings.Contains(stderr.String(), "operation record was rejected") {
		t.Fatalf("trimmed=%v out=%s err=%s", trimmed, out.String(), stderr.String())
	}
}
