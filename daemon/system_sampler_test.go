package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSystemPaneRefsPreserveRuntimeState(t *testing.T) {
	now := time.Now()
	busy := newCacheTestPane("pane-busy")
	busy.Pid = os.Getpid()
	atomic.StoreInt64(&busy.LastActivityMs, now.UnixMilli())
	idle := newCacheTestPane("pane-idle")
	idle.Pid = os.Getpid()
	atomic.StoreInt64(&idle.LastActivityMs, now.Add(-10*time.Second).UnixMilli())
	mgr := NewManager("")
	mgr.panes[busy.ID], mgr.panes[idle.ID] = busy, idle

	refs := systemPaneRefs(mgr)
	states := map[string]string{}
	for _, ref := range refs {
		states[ref.ID] = ref.State
	}
	if states[busy.ID] != string(StateBusy) || states[idle.ID] != string(StateIdle) {
		t.Fatalf("runtime states = %#v, want busy and idle", states)
	}
}

func TestSystemLevelsThresholdsHysteresisAndUnknown(t *testing.T) {
	now := time.Unix(100, 0)
	free, total := int64(1024), int64(100_000)
	available := int64(900)
	rss, cpu := 5000.0, 70.0
	sample := systemSample{DiskFreeMB: &free, DiskTotalMB: &total, MemAvailableMB: &available, DaemonRssMB: &rss, DaemonCpuPct: &cpu, DiskFloorMB: 1024, MemFloorMB: 1024, MemGuardMB: 6144}
	first, state := systemLevels(sample, now, levelState{})
	if first.Overall != "ok" {
		t.Fatalf("first sampled breach = %q, want hysteresis hold at ok", first.Overall)
	}
	second, state := systemLevels(sample, now.Add(systemFastInterval), state)
	if second.Overall != "critical" || second.Areas["disk"] != "critical" || second.Areas["daemonCpu"] != "warn" {
		t.Fatalf("confirmed levels = %+v", second)
	}
	free, available, rss, cpu = 50_000, 10_000, 100, 2
	recovered, _ := systemLevels(sample, now.Add(2*systemFastInterval), state)
	if recovered.Overall != "ok" {
		t.Fatalf("recovery = %+v, want immediate ok", recovered)
	}
	unknown, _ := systemLevels(systemSample{}, now, levelState{})
	if unknown.Overall != "unknown" || unknown.Areas["disk"] != "unknown" {
		t.Fatalf("unknown probes = %+v", unknown)
	}
	discrete, _ := systemLevels(systemSample{EngineWarn: true}, now, levelState{})
	if discrete.Areas["engine"] != "warn" {
		t.Fatalf("discrete engine state = %+v", discrete)
	}
}

func TestSystemLevelsBoundariesAndDiscreteAreas(t *testing.T) {
	healthy := func() systemSample {
		free, total, available := int64(50_000), int64(100_000), int64(10_000)
		rss, cpu := 100.0, 2.0
		return systemSample{
			DiskFreeMB: &free, DiskTotalMB: &total, DiskFloorMB: 1024,
			MemAvailableMB: &available, MemFloorMB: 1024,
			DaemonRssMB: &rss, DaemonCpuPct: &cpu, MemGuardMB: 6144,
		}
	}
	setInt64 := func(p **int64, v int64) { *p = &v }
	setFloat := func(p **float64, v float64) { *p = &v }
	tests := []struct {
		name, area, want string
		mutate           func(*systemSample)
	}{
		{"disk critical absolute", "disk", "critical", func(s *systemSample) { setInt64(&s.DiskFreeMB, 2047); setInt64(&s.DiskTotalMB, 50_000) }},
		{"disk critical small ratio and little free", "disk", "critical", func(s *systemSample) { setInt64(&s.DiskFreeMB, 4500); setInt64(&s.DiskTotalMB, 300_000) }},
		{"disk warning absolute", "disk", "warn", func(s *systemSample) { setInt64(&s.DiskFreeMB, 10_239); setInt64(&s.DiskTotalMB, 50_000) }},
		{"disk warning small ratio and under 20 GB", "disk", "warn", func(s *systemSample) { setInt64(&s.DiskFreeMB, 15_000); setInt64(&s.DiskTotalMB, 400_000) }},
		{"disk ok on a large disk with plenty free", "disk", "ok", func(s *systemSample) { setInt64(&s.DiskFreeMB, 71_700); setInt64(&s.DiskTotalMB, 1_006_900) }},
		{"disk ok small ratio but over 20 GB free", "disk", "ok", func(s *systemSample) { setInt64(&s.DiskFreeMB, 25_000); setInt64(&s.DiskTotalMB, 1_000_000) }},
		{"host memory critical", "hostMemory", "critical", func(s *systemSample) { setInt64(&s.MemAvailableMB, 1023) }},
		{"host memory warning", "hostMemory", "warn", func(s *systemSample) { setInt64(&s.MemAvailableMB, 2047) }},
		{"daemon guard warning", "daemonMemory", "warn", func(s *systemSample) { setFloat(&s.DaemonRssMB, 3072) }},
		{"daemon guard critical", "daemonMemory", "critical", func(s *systemSample) { setFloat(&s.DaemonRssMB, 4916) }},
		{"daemon guard off fallback", "daemonMemory", "critical", func(s *systemSample) { s.MemGuardMB = 0; setFloat(&s.DaemonRssMB, 4915) }},
		{"daemon cpu warning at half of one core", "daemonCpu", "warn", func(s *systemSample) { s.Cores = 1; setFloat(&s.DaemonCpuPct, 50) }},
		{"daemon cpu warning at half of eight cores", "daemonCpu", "warn", func(s *systemSample) { s.Cores = 8; setFloat(&s.DaemonCpuPct, 400) }},
		{"daemon cpu one busy core is fine on eight", "daemonCpu", "ok", func(s *systemSample) { s.Cores = 8; setFloat(&s.DaemonCpuPct, 120) }},
		{"daemon cpu just under half of eight cores", "daemonCpu", "ok", func(s *systemSample) { s.Cores = 8; setFloat(&s.DaemonCpuPct, 399) }},
		{"daemon cpu unknown core count counts as one", "daemonCpu", "warn", func(s *systemSample) { s.Cores = 0; setFloat(&s.DaemonCpuPct, 60) }},
		{"engine warning", "engine", "warn", func(s *systemSample) { s.EngineWarn = true }},
		{"runtime warning beats partial", "runtimes", "warn", func(s *systemSample) { s.RuntimesWarn, s.RuntimesUnknown = true, true }},
		{"automation warning", "automation", "warn", func(s *systemSample) { s.AutomationWarn = true }},
		{"cloud credential critical", "cloud", "critical", func(s *systemSample) { s.CloudWarn, s.CloudCritical = true, true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := healthy()
			tt.mutate(&s)
			first, state := systemLevels(s, time.Unix(1, 0), levelState{})
			got := first
			if tt.area == "disk" || tt.area == "hostMemory" || tt.area == "daemonMemory" || tt.area == "daemonCpu" {
				got, _ = systemLevels(s, time.Unix(16, 0), state)
			}
			if got.Areas[tt.area] != tt.want {
				t.Fatalf("%s = %q, want %q (all=%+v)", tt.area, got.Areas[tt.area], tt.want, got.Areas)
			}
		})
	}
}

func TestSystemLevelsReasonsCarryDrivingValues(t *testing.T) {
	free, total, available := int64(50_000), int64(100_000), int64(10_000)
	rss, cpu := 100.0, 2.0
	sample := systemSample{
		DiskFreeMB: &free, DiskTotalMB: &total, DiskFloorMB: 1024,
		MemAvailableMB: &available, MemFloorMB: 1024,
		DaemonRssMB: &rss, DaemonCpuPct: &cpu, MemGuardMB: 6144,
		AutomationWarn: true, AutomationBlocked: 1, AutomationErrors24h: 2,
	}
	levels, _ := systemLevels(sample, time.Unix(1, 0), levelState{})
	if levels.Reason != "automation: 2 failed runs in 24h · 1 blocked agent" {
		t.Fatalf("automation reason = %q", levels.Reason)
	}
}

func TestSystemLevelsDaemonCPUReadsInCores(t *testing.T) {
	for _, tc := range []struct {
		pct    float64
		cores  int
		text   string
		detail string
	}{
		// Reasons list only areas that are not ok, so each case warns.
		{100, 2, "SoluTerminal CPU: 1.0 core", "of 2 cores on this machine; warns when it holds 1.0 or more for about 30 s"},
		{420, 8, "SoluTerminal CPU: 4.2 cores", "of 8 cores on this machine; warns when it holds 4.0 or more for about 30 s"},
		{60, 1, "SoluTerminal CPU: 0.6 cores", "of 1 core on this machine; warns when it holds 0.5 or more for about 30 s"},
	} {
		pct := tc.pct
		sample := systemSample{DaemonCpuPct: &pct, Cores: tc.cores}
		_, state := systemLevels(sample, time.Unix(1, 0), levelState{})
		levels, _ := systemLevels(sample, time.Unix(16, 0), state)
		var got *systemLevelReason
		for i := range levels.Reasons {
			if levels.Reasons[i].Key == "daemonCpu" {
				got = &levels.Reasons[i]
			}
		}
		if got == nil {
			t.Fatalf("%v%% on %d cores: no daemonCpu reason in %+v", tc.pct, tc.cores, levels.Reasons)
		}
		if got.Text != tc.text || got.Detail != tc.detail {
			t.Fatalf("%v%% on %d cores: reason = %q / %q, want %q / %q", tc.pct, tc.cores, got.Text, got.Detail, tc.text, tc.detail)
		}
	}
	waiting, _ := systemLevels(systemSample{}, time.Unix(1, 0), levelState{})
	for _, r := range waiting.Reasons {
		if r.Key == "daemonCpu" && r.Text != "SoluTerminal CPU: awaiting sample" {
			t.Fatalf("unsampled daemon CPU text = %q", r.Text)
		}
	}
}

func TestSystemCPUPercent(t *testing.T) {
	if got := systemCPUPercent(100, 250, 3*time.Second); got != 50 {
		t.Fatalf("cpu = %v, want 50", got)
	}
	if got := systemCPUPercent(250, 100, time.Second); got != 0 {
		t.Fatalf("counter reset = %v", got)
	}
}

func TestSystemSamplerTreeSumsAndMarksIncomplete(t *testing.T) {
	now := time.Unix(100, 0)
	ticks := map[int]uint64{10: 100, 11: 100, 20: 100}
	rss := map[int]int{10: 1024, 11: 2048, 20: 4096}
	s := &systemSampler{deps: systemSamplerDeps{clock: func() time.Time { return now }, listPanes: func() []systemPaneRef { return []systemPaneRef{{ID: "p", Status: string(PaneRunning), Pid: 10}} }, listWorkers: func() []systemWorkerProcess { return []systemWorkerProcess{{TaskID: "t", Pid: 20}} }, processTree: func() processTreeSnapshot { return processTreeSnapshot{children: map[int][]int{10: {11}}} }, readRSS: func(pid int) (int, int, bool) { v, ok := rss[pid]; return v, v, ok }, procTicks: func(pid int) (uint64, uint64, bool) { v, ok := ticks[pid]; return v, 1, ok }}, treePID: map[int]systemProcTick{}, fastPID: map[int]systemProcTick{}}
	s.sampleTree()
	snap := s.Snapshot()
	if len(snap.Panes) != 1 || snap.Panes[0].Cost.Procs != 2 || snap.Panes[0].Cost.RssMB == nil || *snap.Panes[0].Cost.RssMB != 3 {
		t.Fatalf("tree snapshot = %+v", snap.Panes)
	}
	delete(rss, 11)
	now = now.Add(time.Minute)
	ticks[10] += 100
	ticks[11] += 100
	ticks[20] += 100
	s.sampleTree()
	snap = s.Snapshot()
	if snap.Panes[0].Cost.Complete || snap.Panes[0].Cost.RssMB != nil {
		t.Fatalf("failed read reported complete: %+v", snap.Panes[0].Cost)
	}
	// A reused PID starts a new baseline rather than inheriting the previous
	// process's CPU counter.
	rss[11] = 2048
	now = now.Add(time.Minute)
	s.deps.procTicks = func(pid int) (uint64, uint64, bool) { return ticks[pid] + 100, uint64(pid), true }
	s.sampleTree()
	if got := s.Snapshot().Panes[0].Cost.CpuPct; got != nil {
		t.Fatalf("pid reuse cpu = %v, want unknown baseline", *got)
	}
}

func TestSystemSamplerUnavailablePlatformStaysUnknown(t *testing.T) {
	now := time.Unix(100, 0)
	s := &systemSampler{
		deps: systemSamplerDeps{
			dataDir:        t.TempDir(),
			clock:          func() time.Time { return now },
			platformProcfs: func() bool { return false },
			selfMemMB:      func() uint64 { return 64 },
			memAvailKB:     func() (int64, bool) { return 0, false },
			memTotalKB:     func() (int64, bool) { return 0, false },
			diskUsage:      func(string) (int64, int64, bool) { return 0, 0, false },
			procTicks:      func(int) (uint64, uint64, bool) { return 0, 0, false },
			hostTicks:      func() (uint64, uint64, bool) { return 0, 0, false },
			loadAvg:        func() (float64, bool) { return 0, false },
			processTree:    func() processTreeSnapshot { return processTreeSnapshot{} },
			readRSS:        func(int) (int, int, bool) { return 0, 0, false },
			listPanes: func() []systemPaneRef {
				return []systemPaneRef{{ID: "p", Status: string(PaneRunning), State: string(StateIdle), Pid: 10}}
			},
		},
		fastPID: map[int]systemProcTick{}, treePID: map[int]systemProcTick{},
	}
	s.sampleFast()
	s.sampleTree()
	snap := s.Snapshot()
	if snap.Procfs || snap.DiskFreeMB != nil || snap.MemAvailableMB != nil || snap.Levels.Areas["disk"] != "unknown" {
		t.Fatalf("unavailable probes became values: %+v", snap)
	}
	if len(snap.Panes) != 1 || snap.Panes[0].Cost.RssMB != nil || snap.Panes[0].Cost.CpuPct != nil {
		t.Fatalf("non-procfs pane accounting = %+v", snap.Panes)
	}

	a := &apiServer{system: s, startT: now, dataDir: t.TempDir()}
	rec := httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["platform"].(map[string]any)["procfs"] != false || body["disk"].(map[string]any)["freeMB"] != nil {
		t.Fatalf("non-linux contract = %s", rec.Body.String())
	}
}

func TestSystemSamplerLazyBreakdownAndKillSwitch(t *testing.T) {
	t.Setenv("SOLU_SYSTEM_SAMPLER", "0")
	if got := startSystemSampler(context.Background(), systemSamplerDeps{}); got != nil {
		t.Fatal("disabled sampler started")
	}
	var nilSampler *systemSampler
	nilSampler.KickBreakdown()
	_ = nilSampler.Snapshot()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	s := &systemSampler{deps: systemSamplerDeps{dataDir: dir, clock: time.Now, scanInventory: func(context.Context, storageScanRequest) storageInventory {
		<-done
		return storageInventory{Complete: true, ScannedAt: time.Now().UTC()}
	}}}
	s.KickBreakdown()
	s.KickBreakdown()
	s.breakMu.Lock()
	running := s.breaking
	s.breakMu.Unlock()
	if !running {
		t.Fatal("breakdown did not enter single-flight")
	}
	close(done)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if snap := s.Snapshot(); snap.BreakdownAt != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("breakdown did not publish")
}

func TestSystemSamplerBreakdownTTLAndBudgets(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000, 0)
	calls := 0
	var captured storageScanRequest
	s := &systemSampler{deps: systemSamplerDeps{
		dataDir:               dir,
		clock:                 func() time.Time { return now },
		breakdownEntryTimeout: 50 * time.Millisecond,
		breakdownTotalTimeout: 125 * time.Millisecond,
		scanInventory: func(_ context.Context, req storageScanRequest) storageInventory {
			calls++
			captured = req
			return storageInventory{Complete: true, ScannedAt: now.UTC(), Rows: []storageCategoryRow{{Category: storageCatOtherSoluData, Measured: true, Complete: true, ApparentBytes: 7, Bytes: 7}}}
		},
	}}
	at := now
	s.snap.Store(&systemSample{BreakdownAt: &at})
	s.KickBreakdown()
	time.Sleep(20 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("fresh 10-minute cache started %d inventory scans", calls)
	}

	now = now.Add(systemBreakdownTTL)
	s.measureBreakdown()
	if calls != 1 {
		t.Fatalf("inventory scans = %d, want 1", calls)
	}
	if captured.Limits.PerEntryTimeout != 50*time.Millisecond || captured.Limits.TotalTimeout != 125*time.Millisecond {
		t.Fatalf("scan request limits = %+v", captured.Limits)
	}
	if snap := s.Snapshot(); snap.BreakdownAt == nil || len(snap.Breakdown) != 1 || snap.Breakdown[0].Category != storageCatOtherSoluData {
		t.Fatalf("published breakdown = %+v", snap.Breakdown)
	}
}
