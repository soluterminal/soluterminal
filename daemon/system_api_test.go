package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"

	"sort"
	"strings"
	"testing"
	"time"
)

func TestSystemReleaseSurfacesWorkflowUpdateBackoff(t *testing.T) {
	t.Parallel()
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.mu.Lock()
	u.status.Latest = "2.0.0"
	u.status.Available = true
	u.deferAt = time.Now().Add(-time.Minute)
	u.deferInflight = 3
	u.deferAttempts = 2
	u.nextRetryTime = time.Now().Add(10 * time.Minute)
	u.mu.Unlock()
	got := (&apiServer{ver: "1.0.0", updater: u}).systemRelease()
	if got.WorkflowDeferState != "backoff" || got.WorkflowInflight != 3 || got.WorkflowDeferAttempts != 2 || got.WorkflowNextRetryMs <= time.Now().UnixMilli() {
		t.Fatalf("release update state = %+v", got)
	}
}

func TestSystemStatusJSONFieldContract(t *testing.T) {
	raw, err := json.Marshal(systemStatusDTO{})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	wantTop := []string{"automation", "daemon", "disk", "engine", "footprint", "generatedAt", "host", "level", "panes", "platform", "release", "runtimes", "sampleIntervalSec", "sampledAt", "surface", "treeSampledAt", "workers"}
	if got := sortedJSONKeys(body); !equalStrings(got, wantTop) {
		t.Fatalf("system status fields = %q, want %q", got, wantTop)
	}
	var disk map[string]json.RawMessage
	if err := json.Unmarshal(body["disk"], &disk); err != nil {
		t.Fatal(err)
	}
	wantDisk := []string{"anyLow", "blockingLayer", "breakdown", "breakdownAt", "floorMB", "freeBytes", "freeMB", "fresh", "goCacheBytes", "host", "layer", "low", "measuredAt", "measuring", "nextMeasureAfter", "path", "reclaimableBytes", "reclaimableTmpBytes", "reclaimableWorktreeBytes", "reclaimedBytes", "totalBytes", "totalMB", "usedBytes", "worktreesBytes"}
	if got := sortedJSONKeys(disk); !equalStrings(got, wantDisk) {
		t.Fatalf("disk fields = %q, want %q", got, wantDisk)
	}
}

func TestSystemStatusHostDiskLayer(t *testing.T) {
	now := time.Now().UTC()
	guestFree, guestTotal, hostFree, hostTotal, hostFloor := int64(667000), int64(1048576), int64(41000), int64(2050000), int64(61500)
	unexplained, growth := int64(51095), int64(0)
	sparse := false
	sample := systemSample{SampledAt: now, DiskFreeMB: &guestFree, DiskTotalMB: &guestTotal, DiskFloorMB: 10240, HostDisk: &systemHostDiskSample{State: hostStorageOK, Volume: `C:\`, Distro: "Ubuntu", FreeMB: &hostFree, TotalMB: &hostTotal, FloorMB: &hostFloor, FloorPct: 3, HysteresisMB: 4096, Low: true, VHDXPhysicalMB: &hostTotal, Sparse: &sparse, SampledAt: &now, BlockingLayer: "host", UnexplainedLossMB: &unexplained, VHDXGrowthMB: &growth, LossWindowMin: 43, Offender: &systemHostOffender{Label: `%TEMP%\wsl-crashes`, Path: `C:\Users\alice\AppData\Local\Temp\wsl-crashes`, MB: 51200, Files: 10, Dumps: 10, Complete: true, Text: `51.0 GB of WSL crash dumps (10 .dmp files) in %TEMP%\wsl-crashes`}}, Levels: systemLevelsDTO{Overall: "critical", Reason: "host disk critical", Areas: map[string]string{"disk": "ok", "hostDisk": "critical"}}}
	a := &apiServer{system: seededSystemSampler(sample), startT: now, dataDir: t.TempDir()}
	rec := httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	var body struct {
		Disk  systemDiskDTO   `json:"disk"`
		Level systemLevelsDTO `json:"level"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Disk.Layer != "guest" || body.Disk.Low || !body.Disk.AnyLow || body.Disk.BlockingLayer != "host" || body.Disk.Host == nil || !body.Disk.Host.Low || body.Level.Areas["hostDisk"] != "critical" {
		t.Fatalf("body=%+v host=%+v", body, body.Disk.Host)
	}
	if body.Disk.Host.UnexplainedLossMB == nil || *body.Disk.Host.UnexplainedLossMB != 51095 || body.Disk.Host.VHDXGrowthMB == nil || *body.Disk.Host.VHDXGrowthMB != 0 || body.Disk.Host.LossWindowMin != 43 || body.Disk.Host.Offender == nil || body.Disk.Host.Offender.Label != `%TEMP%\wsl-crashes` || !strings.HasPrefix(body.Disk.Host.Offender.Path, `~\`) || strings.Contains(strings.ToLower(body.Disk.Host.Offender.Path), "alice") {
		t.Fatalf("host attribution DTO=%+v", body.Disk.Host)
	}

	native := sample
	native.HostDisk = nil
	native.Levels.Areas = map[string]string{"disk": "ok"}
	a.system = seededSystemSampler(native)
	rec = httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Disk.Layer != "local" || body.Disk.Host != nil {
		t.Fatalf("native disk=%+v", body.Disk)
	}

	guestCritical := int64(100)
	native.DiskFreeMB = &guestCritical
	native.Levels.Areas = map[string]string{"disk": "critical"}
	a.system = seededSystemSampler(native)
	rec = httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Disk.Low || !body.Disk.AnyLow || body.Disk.BlockingLayer != "guest" || body.Disk.Host != nil {
		t.Fatalf("native guest-low payload is contradictory: %+v", body.Disk)
	}
}

func sortedJSONKeys(in map[string]json.RawMessage) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func seededSystemSampler(sample systemSample) *systemSampler {
	s := &systemSampler{}
	s.snap.Store(&sample)
	return s
}

func TestSystemStatusNilSafeAndUnknownIsNull(t *testing.T) {
	now := time.Now().UTC()
	levels := systemLevelsDTO{Overall: "unknown", Areas: map[string]string{"disk": "unknown"}, Reasons: []systemLevelReason{{Key: "disk", Level: "unknown", Text: "disk: unavailable"}}}
	a := &apiServer{system: seededSystemSampler(systemSample{SampledAt: now, TreeSampledAt: now, Levels: levels}), startT: now.Add(-time.Minute), ver: "1.2.3", dataDir: "/tmp/.solu"}
	rec := httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["engine"].(map[string]any)["available"] != false {
		t.Fatalf("engine = %#v", body["engine"])
	}
	if body["runtimes"] != nil {
		t.Fatalf("runtimes = %#v, want null", body["runtimes"])
	}
	daemon := body["daemon"].(map[string]any)
	if daemon["rssMB"] != nil || daemon["cpuPct"] != nil {
		t.Fatalf("unknown numbers must be null: %#v", daemon)
	}
}

func TestSystemStatusMeasureAndHealthSummary(t *testing.T) {
	now := time.Now().UTC()
	s := seededSystemSampler(systemSample{SampledAt: now, Levels: systemLevelsDTO{Overall: "ok", Areas: map[string]string{}}})
	s.deps = systemSamplerDeps{dataDir: t.TempDir(), clock: time.Now, dirSize: dirSizeBytesContext}
	a := &apiServer{system: s, mgr: NewManager(""), startT: now, ver: "1"}
	rec := httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status?measure=1", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var measured map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &measured); err != nil {
		t.Fatal(err)
	}
	if measured["disk"].(map[string]any)["measuring"] != true {
		t.Fatalf("measure response = %s, want disk.measuring=true", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	a.health(rec, httptest.NewRequest("GET", "/api/health", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["system"].(map[string]any)["level"] != "ok" {
		t.Fatalf("health system = %#v", body["system"])
	}
	bare := &apiServer{mgr: NewManager(""), startT: now}
	rec = httptest.NewRecorder()
	bare.health(rec, httptest.NewRequest("GET", "/api/health", nil))
	body = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["system"]; ok {
		t.Fatal("health exposed system without sampler")
	}
}

func TestSystemStatusWarmBreakdownReportsFreshness(t *testing.T) {
	now := time.Now().UTC()
	measuredAt := now.Add(-2 * time.Minute)
	s := seededSystemSampler(systemSample{
		SampledAt: now, BreakdownAt: &measuredAt,
		Breakdown: []systemDiskBreakdown{{Name: "workflow", Bytes: 10, Complete: true}},
		Levels:    systemLevelsDTO{Overall: "ok", Areas: map[string]string{}},
	})
	walks := 0
	s.deps = systemSamplerDeps{
		dataDir: t.TempDir(), clock: func() time.Time { return now },
		dirSize: func(context.Context, string) (int64, bool) { walks++; return 0, true },
	}
	a := &apiServer{system: s, startT: now, dataDir: t.TempDir()}
	rec := httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status?measure=1", nil))
	var body struct {
		Disk systemDiskDTO `json:"disk"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Disk.Measuring || !body.Disk.Fresh || body.Disk.NextMeasureAfter == nil || !body.Disk.NextMeasureAfter.Equal(measuredAt.Add(systemBreakdownTTL)) {
		t.Fatalf("warm measurement response = %+v", body.Disk)
	}
	time.Sleep(20 * time.Millisecond)
	if walks != 0 {
		t.Fatalf("warm request started %d directory walks", walks)
	}
}

func TestSystemStatusUsesSamplerHysteresisVerdict(t *testing.T) {
	now := time.Now().UTC()
	free, total, available := int64(50_000), int64(100_000), int64(10_000)
	rss, cpu := 100.0, 2.0
	sample := systemSample{
		SampledAt: now, DiskFreeMB: &free, DiskTotalMB: &total,
		MemAvailableMB: &available, DaemonRssMB: &rss, DaemonCpuPct: &cpu,
		Levels: systemLevelsDTO{
			Overall: "critical", Reason: "disk: sampled critical",
			Areas:   map[string]string{"disk": "critical", "hostMemory": "ok", "daemonMemory": "ok", "daemonCpu": "ok", "engine": "ok", "runtimes": "ok", "automation": "ok", "cloud": "ok"},
			Reasons: []systemLevelReason{{Key: "disk", Level: "critical", Text: "disk: sampled critical"}},
		},
	}
	a := &apiServer{system: seededSystemSampler(sample), startT: now, dataDir: t.TempDir()}
	rec := httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	var body struct {
		Level systemLevelsDTO `json:"level"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Level.Overall != "critical" || body.Level.Reason != "disk: sampled critical" {
		t.Fatalf("request recomputed cached verdict: %+v", body.Level)
	}
}

func TestHostStorageCLIDecodesLivePolicyWithoutFallback(t *testing.T) {
	t.Setenv("SOLU_WORKFLOW_MIN_FREE_HOST_DISK_MB", "16384")
	t.Setenv("SOLU_WORKFLOW_MIN_FREE_HOST_DISK_PCT", "3")
	t.Setenv("SOLU_WORKFLOW_HOST_DISK_ADAPTIVE_FLOOR", "")
	t.Setenv("SOLU_WORKFLOW_ROTATE_WSL_CRASH_DUMPS", "")
	e := hostPressureEngine(&fakeHostStorageSource{sample: hostStorageSample{Available: true, State: hostStorageOK, Distro: "Ubuntu", Volume: `C:\`, FreeBytes: 40 << 30, TotalBytes: 2 << 40}}, 667*1024)
	e.hostLedger = newHostStorageLedger(t.TempDir())
	a := &apiServer{storage: e}
	oldGet, oldProbe := hostStorageCLIGetRequest, hostStorageCLIProber
	t.Cleanup(func() { hostStorageCLIGetRequest, hostStorageCLIProber = oldGet, oldProbe })
	hostStorageCLIGetRequest = func(_ string, dst any) error {
		rec := httptest.NewRecorder()
		a.hostStorageStatus(rec, httptest.NewRequest("GET", "/api/system/host-storage?limit=10", nil))
		var contract struct {
			Policy map[string]statsResolvedCap  `json:"policy"`
			OptIns map[string]statsResolvedFlag `json:"optIns"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &contract); err != nil {
			return err
		}
		if len(contract.Policy) != 4 || contract.OptIns["adaptiveFloor"] != (statsResolvedFlag{Source: capSourceAdaptive}) || contract.OptIns["rotateWSLCrashDumps"] != (statsResolvedFlag{Source: capSourceAdaptive}) {
			t.Fatalf("policy/opt-ins contract: %+v", contract)
		}
		return json.Unmarshal(rec.Body.Bytes(), dst)
	}
	hostStorageCLIProber = func() hostStorageSource { t.Fatal("CLI fell back to local probe"); return nil }
	var out, stderr bytes.Buffer
	if code := runHostStorageCLI("status", nil, &out, &stderr); code != 0 || strings.Contains(stderr.String(), "daemon unavailable") || !strings.Contains(out.String(), "MB source env") {
		t.Fatalf("code=%d out=%s stderr=%s", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := runHostStorageCLI("status", []string{"--json"}, &out, &stderr); code != 0 || strings.Contains(stderr.String(), "daemon unavailable") || !strings.Contains(out.String(), `"floorMB"`) {
		t.Fatalf("json code=%d out=%s stderr=%s", code, out.String(), stderr.String())
	}
}

func TestHostStorageStatusSuggestsMeasuredAdaptiveFloor(t *testing.T) {
	for _, name := range []string{"SOLU_WORKFLOW_MIN_FREE_HOST_DISK_MB", "SOLU_WORKFLOW_MIN_FREE_HOST_DISK_PCT", "SOLU_WORKFLOW_HOST_DISK_ADAPTIVE_FLOOR"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
	ledger := newHostStorageLedger(t.TempDir())
	now := writeHostGrowthFixture(t, ledger.path, 50, true)
	ledger.now = func() time.Time { return now }
	sparse := true
	e := hostPressureEngine(&fakeHostStorageSource{sample: hostStorageSample{Available: true, State: hostStorageOK, Distro: "Ubuntu", VHDXPath: `C:\Users\test\ext4.vhdx`, LocalAppData: `C:\Users\test\AppData\Local`, VHDXSparse: &sparse, FreeBytes: 98000 << 20, TotalBytes: 1952386 << 20}}, 667*1024)
	e.hostLedger = ledger
	a := &apiServer{storage: e}
	rec := httptest.NewRecorder()
	a.hostStorageStatus(rec, httptest.NewRequest("GET", "/api/system/host-storage", nil))
	var body struct {
		UnblockOptions []systemHostDiskUnblockOption `json:"unblockOptions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	stats, ok := ledger.GrowthStats(30 * time.Minute)
	if !ok {
		t.Fatal("fixture has no growth stats")
	}
	want := int64(102400) - measuredHostDiskFloorMB(stats, true)
	if len(body.UnblockOptions) != 3 || body.UnblockOptions[2].Kind != "enable-adaptive-floor" || body.UnblockOptions[2].FloorDeltaMB == nil || *body.UnblockOptions[2].FloorDeltaMB != want || body.UnblockOptions[2].Consented {
		t.Fatalf("options=%+v want delta=%d", body.UnblockOptions, want)
	}
}
