package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"strings"
	"testing"
	"time"
)

func TestHostStorageStatusSeparatesCapacityAndCleanupOperations(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	src := &fakeHostStorageSource{sample: hostStorageSample{At: base, Available: true, State: hostStorageOK, Distro: "Ubuntu", Volume: `C:\`, FreeBytes: 100 << 30, TotalBytes: 2 << 40}}
	e := hostPressureEngine(src, 667*1024)
	e.hostLedger = newHostStorageLedger(t.TempDir())
	e.hostLedger.now = func() time.Time { return base }
	e.hostLedger.AppendBoot("boot-a", "1")
	e.hostLedger.AppendSample("boot-a", diskPressureState{HostKnown: true, HostState: hostStorageOK, HostFreeMB: 100000, HostTotalMB: 2000000, VHDXPhysicalMB: 500000})
	e.hostLedger.AppendCleanupOperation(storageCleanupOperation{At: base, Kind: "worktree_gc", Source: "manual", Layer: "guest", Executed: true, Complete: true})
	a := &apiServer{storage: e}
	w := httptest.NewRecorder()
	a.hostStorageStatus(w, httptest.NewRequest(http.MethodGet, "/api/system/host-storage?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		CapacitySamples []hostStorageEvent        `json:"capacitySamples"`
		Operations      []storageCleanupOperation `json:"operations"`
		Events          []hostStorageEvent        `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	hasSample := false
	for _, ev := range body.CapacitySamples {
		if ev.Kind == "sample" {
			hasSample = true
		}
	}
	if !hasSample || len(body.Operations) != 1 || body.Operations[0].Kind != "worktree_gc" || !body.Operations[0].Executed || len(body.Events) == 0 {
		t.Fatalf("capacity/operation split missing: %+v", body)
	}
}

func TestHostStorageStatusIncludesLauncherMaintenanceOperation(t *testing.T) {
	root := t.TempDir()
	previousWSLPath := hostStorageWSLPath
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) { return []byte(root + "\n"), nil }
	t.Cleanup(func() { hostStorageWSLPath = previousWSLPath })
	dir := filepath.Join(root, "soluterminal")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	op := validCompactOperation("succeeded")
	row, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host-disk.jsonl"), append(row, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	src := &fakeHostStorageSource{sample: hostStorageSample{At: time.Now(), Available: true, State: hostStorageOK, Distro: "Ubuntu", LocalAppData: `C:\Users\owner\AppData\Local`, Volume: `C:\`, FreeBytes: 100 << 30, TotalBytes: 2 << 40}}
	e := hostPressureEngine(src, 667*1024)
	e.hostLedger = newHostStorageLedger(t.TempDir())
	a := &apiServer{storage: e}
	w := httptest.NewRecorder()
	a.hostStorageStatus(w, httptest.NewRequest(http.MethodGet, "/api/system/host-storage?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Operations []storageCleanupOperation `json:"operations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Operations) != 1 || body.Operations[0].Kind != "windows_maintenance" || !body.Operations[0].Executed || !body.Operations[0].Verified || body.Operations[0].ReclaimedBytes == nil || *body.Operations[0].ReclaimedBytes != 200 {
		t.Fatalf("launcher maintenance missing from operations: %+v", body.Operations)
	}
}

func TestHostStorageStatusAPIReportsHostBlockingLayerAndPolicy(t *testing.T) {
	t.Setenv("SOLU_WORKFLOW_MIN_FREE_HOST_DISK_MB", "16384")
	t.Setenv("SOLU_WORKFLOW_MIN_FREE_HOST_DISK_PCT", "3")
	src := &fakeHostStorageSource{sample: hostStorageSample{At: time.Now(), Available: true, State: hostStorageOK, Distro: "Ubuntu", Volume: `C:\`, FreeBytes: 40 << 30, TotalBytes: 2 << 40}}
	e := hostPressureEngine(src, 667*1024)
	e.hostLedger = newHostStorageLedger(t.TempDir())
	a := &apiServer{storage: e}
	w := httptest.NewRecorder()
	a.hostStorageStatus(w, httptest.NewRequest(http.MethodGet, "/api/system/host-storage?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Available bool `json:"available"`
		State     string
		Sample    systemHostDiskDTO
		Pressure  struct {
			GuestLow, HostLow bool
			BlockingLayer     string
		}
		Policy map[string]statsResolvedCap
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Available || body.State != hostStorageOK || !body.Sample.Low || body.Pressure.GuestLow || !body.Pressure.HostLow || body.Pressure.BlockingLayer != "host" {
		t.Fatalf("payload=%+v sample=%+v pressure=%+v", body, body.Sample, body.Pressure)
	}
	if body.Policy["floorMB"].Value != 16384 || body.Policy["floorPct"].Value != 3 {
		t.Fatalf("policy=%+v", body.Policy)
	}
}

func TestHostStorageStatusAPINonWSL(t *testing.T) {
	a := &apiServer{storage: hostPressureEngine(nil, 667*1024)}
	w := httptest.NewRecorder()
	a.hostStorageStatus(w, httptest.NewRequest(http.MethodGet, "/api/system/host-storage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["available"] != false || body["state"] != hostStorageNotWSL {
		t.Fatalf("body=%v", body)
	}
}

func TestHostStorageShortAliasRoutesToSystemPayload(t *testing.T) {
	a := &apiServer{storage: hostPressureEngine(nil, 667*1024)}
	mux := http.NewServeMux()
	a.routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/host-storage", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"not_wsl"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHostStorageRefreshAttributesLowHostOffender(t *testing.T) {
	src := &fakeHostStorageSource{sample: hostStorageSample{At: time.Now(), Available: true, State: hostStorageOK, Distro: "Ubuntu", Volume: `C:\`, FreeBytes: 40 << 30, TotalBytes: 2 << 40}}
	e := hostPressureEngine(src, 667*1024)
	e.hostLedger = newHostStorageLedger(t.TempDir())
	prober, _ := testOffenderProber(offenderFixture)
	e.hostOffender = prober
	a := &apiServer{storage: e}
	w := httptest.NewRecorder()
	a.hostStorageRefresh(w, httptest.NewRequest(http.MethodPost, "/api/system/host-storage/refresh", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Sample   systemHostDiskDTO `json:"sample"`
		Pressure struct {
			Offender string `json:"offender"`
		} `json:"pressure"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Sample.Offender == nil || body.Sample.Offender.Label != `%TEMP%\wsl-crashes` || body.Pressure.Offender != `51.0 GB of WSL crash dumps (10 .dmp files) in %TEMP%\wsl-crashes` {
		t.Fatalf("payload=%+v", body)
	}
}

func TestHostStorageStatusMergesLauncherAndDaemonEventsChronologically(t *testing.T) {
	orig := hostStorageWSLPath
	defer func() { hostStorageWSLPath = orig }()
	root := t.TempDir()
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) { return []byte(root + "\n"), nil }
	launcherDir := filepath.Join(root, "soluterminal")
	if err := os.MkdirAll(launcherDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launcherDir, "host-disk.jsonl"), []byte(`{"at":"2026-08-25T10:01:00Z","kind":"preflight","verdict":"warn"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	src := &fakeHostStorageSource{sample: hostStorageSample{At: base, Available: true, State: hostStorageOK, Distro: "Ubuntu", Volume: `C:\`, FreeBytes: 100 << 30, TotalBytes: 2 << 40, LocalAppData: `C:\Users\alice\AppData\Local`}}
	e := hostPressureEngine(src, 667*1024)
	e.hostLedger = newHostStorageLedger(t.TempDir())
	now := base
	e.hostLedger.now = func() time.Time { return now }
	e.hostLedger.AppendBoot("boot-a", "1")
	now = base.Add(2 * time.Minute)
	e.hostLedger.AppendMaintenance("boot-a", "dry-run", "done")
	a := &apiServer{storage: e}
	w := httptest.NewRecorder()
	a.hostStorageStatus(w, httptest.NewRequest(http.MethodGet, "/api/system/host-storage?limit=10", nil))
	var body struct {
		Events []hostStorageEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 3 || body.Events[0].Kind != "boot" || body.Events[1].Kind != "preflight" || body.Events[1].Source != "launcher" || body.Events[2].Kind != "maintenance" {
		t.Fatalf("merged events=%+v", body.Events)
	}
}
