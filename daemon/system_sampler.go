package main

// The System sampler is deliberately bounded: a fast pass performs a handful
// of small /proc/stat, /proc/meminfo and statfs reads every 15 seconds; a tree
// pass performs one status+stat read per live descendant every 60 seconds. It
// performs zero directory walks unless the System page explicitly asks for a
// breakdown. The request path only reads the atomic snapshot.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	systemFastInterval     = 15 * time.Second
	systemTreeInterval     = 60 * time.Second
	systemBreakdownTTL     = 10 * time.Minute
	systemBreakdownEntryTO = 2 * time.Second
	systemBreakdownTotalTO = 60 * time.Second
	procUserHZ             = 100
)

type systemPaneRef struct {
	ID, Title, Agent, Kind, Role, Status, State string
	Pid                                         int
	AgentRssMB                                  int64
}

type systemSamplerDeps struct {
	dataDir                  string
	clock                    func() time.Time
	listPanes                func() []systemPaneRef
	listWorkers              func() []systemWorkerProcess
	memAvailKB, memTotalKB   func() (int64, bool)
	diskUsage                func(string) (int64, int64, bool)
	selfMemMB, goHeapMB      func() uint64
	guardUsedMB, goRuntimeMB func() uint64
	memGuardMB               uint64
	diskFloorMB, memFloorMB  func() int
	procTicks                func(int) (uint64, uint64, bool)
	hostTicks                func() (uint64, uint64, bool)
	loadAvg                  func() (float64, bool)
	pressure                 func() pressureSample
	hostMem                  func() hostMemSample
	platformProcfs           func() bool
	processTree              func() processTreeSnapshot
	readRSS                  func(int) (int, int, bool)
	dirSize                  func(context.Context, string) (int64, bool)
	breakdownEntryTimeout    time.Duration
	breakdownTotalTimeout    time.Duration
	decorate                 func(*systemSample)
	hostDisk                 func() *systemHostDiskSample
	scanInventory            func(context.Context, storageScanRequest) storageInventory
	onInventory              func(storageInventory)
	repoPaths                func() []string
}

const (
	systemInventoryInitialDelay = 2 * time.Minute
	systemInventoryInterval     = 6 * time.Hour
)

// systemPaneRefs adapts manager panes into sampler rows while preserving the
// finer runtime state exposed by /api/panes. Agent metadata supplies the same
// Codex/Muse prompt-mode hint without adding a second process-tree walk to the
// bounded sampler pass.
func systemPaneRefs(mgr *Manager) []systemPaneRef {
	if mgr == nil {
		return nil
	}
	out := make([]systemPaneRef, 0)
	for _, pane := range mgr.List() {
		snap := pane.Snapshot()
		state := StateExited
		if snap.Status == PaneRunning {
			agent := effectiveAgentKind(snap.Agent)
			state = pane.computeStateWithHasCodex(agent == agentCodex || agent == agentMuse)
		}
		out = append(out, systemPaneRef{
			ID: snap.ID, Title: snap.Title, Agent: snap.Agent, Kind: snap.Kind,
			Role: snap.Role, Status: string(snap.Status), State: string(state),
			Pid: snap.Pid, AgentRssMB: pane.agentRssMB.Load(),
		})
	}
	return out
}

type systemProcessSample struct {
	Procs    int      `json:"procs"`
	RssMB    *float64 `json:"rssMB"`
	CpuPct   *float64 `json:"cpuPct"`
	Complete bool     `json:"complete"`
}

type systemPaneSample struct {
	Ref  systemPaneRef
	Cost systemProcessSample
}

type systemWorkerSample struct {
	Ref  systemWorkerProcess
	Cost systemProcessSample
}

type systemDiskBreakdown struct {
	Name           string     `json:"name"`
	Label          string     `json:"label,omitempty"`
	Bytes          int64      `json:"bytes"`
	Complete       bool       `json:"complete"`
	Category       string     `json:"category,omitempty"`
	ApparentBytes  int64      `json:"apparentBytes,omitempty"`
	AllocatedBytes *int64     `json:"allocatedBytes,omitempty"`
	Measured       bool       `json:"measured"`
	Protection     string     `json:"protection,omitempty"`
	Cleanup        string     `json:"cleanup,omitempty"`
	TargetCount    int        `json:"targetCount,omitempty"`
	ScannedAt      *time.Time `json:"scannedAt,omitempty"`
	Reasons        []string   `json:"reasons,omitempty"`
}

type systemSample struct {
	SampledAt, TreeSampledAt time.Time
	StartedAt                time.Time
	DaemonRssMB              *float64
	GoHeapMB                 *float64
	GuardUsedMB              *float64
	GoRuntimeMB              *float64
	Goroutines               *int
	DaemonCpuPct             *float64
	HostCpuPct               *float64
	LoadAvg1                 *float64
	MemTotalMB               *int64
	MemAvailableMB           *int64
	DiskFreeMB               *int64
	DiskTotalMB              *int64
	DiskFreeBytes            *int64
	DiskTotalBytes           *int64
	DiskUsedBytes            *int64
	DiskMeasuredAt           *time.Time
	HostDisk                 *systemHostDiskSample
	Inventory                *storageInventory
	MemGuardMB               uint64
	DiskFloorMB              int
	MemFloorMB               int
	Pressure                 *pressureSample
	HostMem                  *hostMemSample
	HostProvenance           string
	Panes                    []systemPaneSample
	Workers                  []systemWorkerSample
	Footprint                systemProcessSample
	Breakdown                []systemDiskBreakdown
	BreakdownAt              *time.Time
	BreakdownMeasuring       bool
	Procfs                   bool
	Levels                   systemLevelsDTO
	// Discrete whole-system inputs are populated by the API before it calls
	// systemLevels. They live here so threshold semantics remain single-sourced.
	EngineWarn, RuntimesWarn, AutomationWarn, CloudWarn          bool
	CloudCritical                                                bool
	EngineUnknown, RuntimesUnknown                               bool
	EngineResourceWait, EngineEnvironmentWait, EngineQuotaPaused int
	EngineDiskLow, EngineHostDiskLow                             bool
	EngineRollback                                               bool
	EngineRollbackCap                                            int
	EngineRollbackReason                                         string
	EngineEffectiveCap, EngineFloorCap                           int
	RuntimesUnhealthy                                            int
	AutomationBlocked, AutomationErrors24h                       int
	CloudRelayOffline                                            bool
}

type systemHostDiskSample struct {
	State, Detail, Volume, Distro   string
	FreeMB, TotalMB, FloorMB        *int64
	FreeBytes, TotalBytes           *int64
	VHDXAllocatedBytes              *int64
	VHDXApparentBytes               *int64
	DumpBytes                       *int64
	FloorPct                        int
	HysteresisMB                    int64
	Low                             bool
	VHDXPath                        string
	VHDXPhysicalMB, VHDXLogicalMB   *int64
	Sparse                          *bool
	SampledAt                       *time.Time
	OffenderScannedAt               *time.Time
	BlockingLayer                   string
	UnexplainedLossMB, VHDXGrowthMB *int64
	LossWindowMin                   int
	Offender                        *systemHostOffender
	WhyBlocked                      *systemHostDiskWhyBlocked
	UnblockOptions                  []systemHostDiskUnblockOption
}

type systemHostOffender struct {
	Label, Path  string
	MB           int64
	Files, Dumps int
	Complete     bool
	Text         string
}

type systemLevelReason struct {
	Key   string `json:"key"`
	Level string `json:"level"`
	Text  string `json:"text"`
	// Detail is the long tail behind a short Text (VHDX state, the largest
	// %TEMP% directory, unexplained loss). The status-bar chip shows Text and
	// puts Detail in its tooltip so a paragraph never fills the bar.
	Detail string `json:"detail,omitempty"`
}

type systemLevelsDTO struct {
	Overall string              `json:"overall"`
	Reason  string              `json:"reason,omitempty"`
	Reasons []systemLevelReason `json:"reasons"`
	Areas   map[string]string   `json:"areas"`
}

type levelState struct {
	Current map[string]string
	Worse   map[string]int
	Pending map[string]string
}

type systemSampler struct {
	deps     systemSamplerDeps
	snap     atomic.Pointer[systemSample]
	mu       sync.Mutex
	levels   levelState
	fastPID  map[int]systemProcTick
	treePID  map[int]systemProcTick
	host     *systemHostTick
	breakMu  sync.Mutex
	breaking bool
}

type systemProcTick struct {
	ticks uint64
	start uint64
	at    time.Time
}

type systemHostTick struct {
	busy, total uint64
	at          time.Time
}

func systemSamplerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_SYSTEM_SAMPLER"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func startSystemSampler(ctx context.Context, deps systemSamplerDeps) *systemSampler {
	if !systemSamplerEnabled() {
		return nil
	}
	if deps.clock == nil {
		deps.clock = time.Now
	}
	if deps.procTicks == nil {
		deps.procTicks = readProcCPUTicks
	}
	if deps.hostTicks == nil {
		deps.hostTicks = readHostCPUTicks
	}
	if deps.loadAvg == nil {
		deps.loadAvg = readLoadAvg1
	}
	if deps.processTree == nil {
		deps.processTree = snapshotProcessTree
	}
	if deps.readRSS == nil {
		deps.readRSS = systemReadRSS
	}
	if deps.dirSize == nil {
		deps.dirSize = dirSizeBytesContext
	}
	if deps.breakdownEntryTimeout <= 0 {
		deps.breakdownEntryTimeout = systemBreakdownEntryTO
	}
	if deps.breakdownTotalTimeout <= 0 {
		deps.breakdownTotalTimeout = systemBreakdownTotalTO
	}
	s := &systemSampler{deps: deps, fastPID: map[int]systemProcTick{}, treePID: map[int]systemProcTick{}}
	s.sampleFast()
	s.sampleTree()
	go s.run(ctx)
	return s
}

func (s *systemSampler) run(ctx context.Context) {
	fast := time.NewTicker(systemFastInterval)
	tree := time.NewTicker(systemTreeInterval)
	inventory := time.NewTicker(systemInventoryInterval)
	initial := time.NewTimer(systemInventoryInitialDelay)
	defer fast.Stop()
	defer tree.Stop()
	defer inventory.Stop()
	defer initial.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			s.sampleFast()
		case <-tree.C:
			s.sampleTree()
		case <-initial.C:
			s.KickInventory()
		case <-inventory.C:
			s.KickInventory()
		}
	}
}

func (s *systemSampler) Snapshot() systemSample {
	if s == nil {
		return systemSample{}
	}
	p := s.snap.Load()
	if p == nil {
		return systemSample{}
	}
	out := *p
	out.Panes = append([]systemPaneSample(nil), p.Panes...)
	out.Workers = append([]systemWorkerSample(nil), p.Workers...)
	out.Breakdown = append([]systemDiskBreakdown(nil), p.Breakdown...)
	s.breakMu.Lock()
	out.BreakdownMeasuring = s.breaking
	s.breakMu.Unlock()
	return out
}

func (s *systemSampler) store(mut func(*systemSample)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var next systemSample
	if old := s.snap.Load(); old != nil {
		next = *old
	}
	mut(&next)
	s.snap.Store(&next)
}

func floatPtr(v float64) *float64   { return &v }
func systemInt64Ptr(v int64) *int64 { return &v }

func (s *systemSampler) sampleFast() {
	if s == nil {
		return
	}
	now := s.deps.clock()
	s.store(func(next *systemSample) {
		if next.StartedAt.IsZero() {
			next.StartedAt = now
		}
		next.SampledAt = now
		next.Procfs = s.procfsAvailable()
		if s.deps.selfMemMB != nil {
			next.DaemonRssMB = nil
			if rss := s.deps.selfMemMB(); rss > 0 {
				next.DaemonRssMB = floatPtr(float64(rss))
			}
		}
		if s.deps.goHeapMB != nil {
			next.GoHeapMB = floatPtr(float64(s.deps.goHeapMB()))
		}
		if s.deps.guardUsedMB != nil {
			next.GuardUsedMB = floatPtr(float64(s.deps.guardUsedMB()))
		}
		if s.deps.goRuntimeMB != nil {
			next.GoRuntimeMB = floatPtr(float64(s.deps.goRuntimeMB()))
		}
		g := runtime.NumGoroutine()
		next.Goroutines = &g
		next.MemGuardMB = s.deps.memGuardMB
		if s.deps.diskFloorMB != nil {
			next.DiskFloorMB = s.deps.diskFloorMB()
		}
		if s.deps.memFloorMB != nil {
			next.MemFloorMB = s.deps.memFloorMB()
		}
		if s.deps.memTotalKB != nil {
			if kb, ok := s.deps.memTotalKB(); ok {
				next.MemTotalMB = systemInt64Ptr(kb / 1024)
			} else {
				next.MemTotalMB = nil
			}
		}
		if s.deps.memAvailKB != nil {
			if kb, ok := s.deps.memAvailKB(); ok {
				next.MemAvailableMB = systemInt64Ptr(kb / 1024)
			} else {
				next.MemAvailableMB = nil
			}
		}
		if s.deps.diskUsage != nil {
			if free, total, ok := s.deps.diskUsage(s.deps.dataDir); ok {
				next.DiskFreeMB, next.DiskTotalMB = systemInt64Ptr(free/(1024*1024)), systemInt64Ptr(total/(1024*1024))
				next.DiskFreeBytes, next.DiskTotalBytes = systemInt64Ptr(free), systemInt64Ptr(total)
				used := total - free
				if used < 0 {
					used = 0
				}
				next.DiskUsedBytes = systemInt64Ptr(used)
				at := now
				next.DiskMeasuredAt = &at
			} else {
				next.DiskFreeMB, next.DiskTotalMB = nil, nil
				next.DiskFreeBytes, next.DiskTotalBytes, next.DiskUsedBytes = nil, nil, nil
			}
		}
		if s.deps.hostDisk != nil {
			next.HostDisk = s.deps.hostDisk()
		}
		if s.deps.loadAvg != nil {
			if v, ok := s.deps.loadAvg(); ok {
				next.LoadAvg1 = floatPtr(v)
			} else {
				next.LoadAvg1 = nil
			}
		}
		next.HostProvenance = runtime.GOOS
		if s.deps.pressure != nil {
			p := s.deps.pressure()
			if !p.At.IsZero() {
				next.Pressure = &p
				next.HostProvenance = p.Provenance
			} else {
				next.Pressure = nil
			}
		}
		if s.deps.hostMem != nil {
			h := s.deps.hostMem()
			if h.Status != "" && h.Status != "n/a" {
				next.HostMem = &h
			} else {
				next.HostMem = nil
			}
		}
		if s.deps.procTicks != nil {
			pid := os.Getpid()
			if ticks, start, ok := s.deps.procTicks(pid); ok {
				if prev, found := s.fastPID[pid]; found && prev.start == start && now.After(prev.at) {
					next.DaemonCpuPct = floatPtr(systemCPUPercent(prev.ticks, ticks, now.Sub(prev.at)))
				} else {
					next.DaemonCpuPct = nil
				}
				s.fastPID[pid] = systemProcTick{ticks: ticks, start: start, at: now}
			} else {
				next.DaemonCpuPct = nil
			}
		}
		if s.deps.hostTicks != nil {
			if busy, total, ok := s.deps.hostTicks(); ok {
				if s.host != nil && total > s.host.total && busy >= s.host.busy {
					v := 100 * float64(busy-s.host.busy) / float64(total-s.host.total)
					next.HostCpuPct = floatPtr(v)
				} else {
					next.HostCpuPct = nil
				}
				s.host = &systemHostTick{busy: busy, total: total, at: now}
			} else {
				next.HostCpuPct = nil
			}
		}
		next.EngineWarn, next.RuntimesWarn, next.AutomationWarn = false, false, false
		next.CloudWarn, next.CloudCritical = false, false
		next.EngineUnknown, next.RuntimesUnknown = false, false
		next.EngineResourceWait, next.EngineEnvironmentWait, next.EngineQuotaPaused = 0, 0, 0
		next.EngineDiskLow, next.EngineHostDiskLow, next.CloudRelayOffline = false, false, false
		next.EngineRollback, next.EngineRollbackCap, next.EngineRollbackReason = false, 0, ""
		next.EngineEffectiveCap, next.EngineFloorCap = 0, 0
		next.RuntimesUnhealthy = 0
		next.AutomationBlocked, next.AutomationErrors24h = 0, 0
		if s.deps.decorate != nil {
			s.deps.decorate(next)
		}
		levels, state := systemLevels(*next, now, s.levels)
		s.levels = state
		next.Levels = levels
	})
}

func systemCPUPercent(oldTicks, newTicks uint64, elapsed time.Duration) float64 {
	if newTicks < oldTicks || elapsed <= 0 {
		return 0
	}
	return 100 * (float64(newTicks-oldTicks) / procUserHZ) / elapsed.Seconds()
}

func (s *systemSampler) procfsAvailable() bool {
	if s != nil && s.deps.platformProcfs != nil {
		return s.deps.platformProcfs()
	}
	return runtime.GOOS == "linux"
}

func (s *systemSampler) sampleTree() {
	if s == nil {
		return
	}
	now := s.deps.clock()
	tree := s.deps.processTree()
	panes := []systemPaneRef{}
	workers := []systemWorkerProcess{}
	if s.deps.listPanes != nil {
		panes = s.deps.listPanes()
	}
	if s.deps.listWorkers != nil {
		workers = s.deps.listWorkers()
	}
	nextTicks := map[int]systemProcTick{}
	measure := func(root int) systemProcessSample {
		if root <= 0 {
			return systemProcessSample{Complete: false}
		}
		pids := tree.descendantPids(root)
		cost := systemProcessSample{Procs: len(pids), Complete: true}
		var rssKB int64
		var cpu float64
		cpuKnown := len(pids) > 0 && s.procfsAvailable()
		rssKnown := len(pids) > 0 && s.procfsAvailable()
		for _, pid := range pids {
			if s.deps.readRSS != nil {
				if rss, _, ok := s.deps.readRSS(pid); ok {
					rssKB += int64(rss)
				} else {
					cost.Complete = false
					rssKnown = false
				}
			}
			if s.deps.procTicks != nil {
				if ticks, start, ok := s.deps.procTicks(pid); ok {
					if prev, found := s.treePID[pid]; found && prev.start == start && now.After(prev.at) {
						cpu += systemCPUPercent(prev.ticks, ticks, now.Sub(prev.at))
					} else {
						cpuKnown = false
					}
					nextTicks[pid] = systemProcTick{ticks: ticks, start: start, at: now}
				} else {
					cost.Complete = false
					cpuKnown = false
				}
			}
		}
		if rssKnown {
			cost.RssMB = floatPtr(float64(rssKB) / 1024)
		}
		if cpuKnown {
			cost.CpuPct = floatPtr(cpu)
		}
		return cost
	}
	paneRows := make([]systemPaneSample, 0, len(panes))
	workerRows := make([]systemWorkerSample, 0, len(workers))
	for _, p := range panes {
		if p.Status == string(PaneRunning) && p.Pid > 0 {
			paneRows = append(paneRows, systemPaneSample{Ref: p, Cost: measure(p.Pid)})
		}
	}
	for _, w := range workers {
		if w.Pid > 0 {
			workerRows = append(workerRows, systemWorkerSample{Ref: w, Cost: measure(w.Pid)})
		}
	}
	s.treePID = nextTicks
	s.store(func(next *systemSample) {
		next.TreeSampledAt = now
		next.Panes = paneRows
		next.Workers = workerRows
		fp := systemProcessSample{Complete: true, Procs: 1}
		var rss, cpu float64
		rssKnown := next.DaemonRssMB != nil
		cpuKnown := next.DaemonCpuPct != nil
		if rssKnown {
			rss = *next.DaemonRssMB
		}
		if cpuKnown {
			cpu = *next.DaemonCpuPct
		}
		for _, row := range paneRows {
			fp.Procs += row.Cost.Procs
			fp.Complete = fp.Complete && row.Cost.Complete
			if row.Cost.RssMB != nil {
				rss += *row.Cost.RssMB
			} else {
				rssKnown = false
			}
			if row.Cost.CpuPct != nil {
				cpu += *row.Cost.CpuPct
			} else {
				cpuKnown = false
			}
		}
		for _, row := range workerRows {
			fp.Procs += row.Cost.Procs
			fp.Complete = fp.Complete && row.Cost.Complete
			if row.Cost.RssMB != nil {
				rss += *row.Cost.RssMB
			} else {
				rssKnown = false
			}
			if row.Cost.CpuPct != nil {
				cpu += *row.Cost.CpuPct
			} else {
				cpuKnown = false
			}
		}
		if rssKnown {
			fp.RssMB = floatPtr(rss)
		}
		if cpuKnown {
			fp.CpuPct = floatPtr(cpu)
		}
		next.Footprint = fp
	})
}

func (s *systemSampler) KickBreakdown() {
	if s == nil {
		return
	}
	now := s.deps.clock()
	snap := s.Snapshot()
	if snap.BreakdownAt != nil && now.Sub(*snap.BreakdownAt) < systemBreakdownTTL {
		return
	}
	s.breakMu.Lock()
	if s.breaking {
		s.breakMu.Unlock()
		return
	}
	s.breaking = true
	s.breakMu.Unlock()
	go s.measureBreakdown()
}

func (s *systemSampler) measureBreakdown() {
	defer func() { s.breakMu.Lock(); s.breaking = false; s.breakMu.Unlock() }()
	if s.deps.scanInventory == nil {
		return
	}
	totalTimeout := s.deps.breakdownTotalTimeout
	if totalTimeout <= 0 {
		totalTimeout = systemBreakdownTotalTO
	}
	entryTimeout := s.deps.breakdownEntryTimeout
	if entryTimeout <= 0 {
		entryTimeout = systemBreakdownEntryTO
	}
	ctx, cancel := context.WithTimeout(context.Background(), totalTimeout)
	defer cancel()
	req := storageScanRequest{
		DataDir: s.deps.dataDir,
		Limits:  storageScanLimits{PerEntryTimeout: entryTimeout, TotalTimeout: totalTimeout},
	}
	if s.deps.repoPaths != nil {
		req.RepoPaths = s.deps.repoPaths()
	}
	inv := s.deps.scanInventory(ctx, req)
	at := s.deps.clock()
	inv.ScannedAt = at
	rows := make([]systemDiskBreakdown, 0, len(inv.Rows))
	for _, row := range inv.Rows {
		scanned := row.ScannedAt
		var scannedPtr *time.Time
		if !scanned.IsZero() {
			scannedPtr = &scanned
		}
		rows = append(rows, systemDiskBreakdown{
			Name: row.Category, Label: row.Label, Bytes: row.ApparentBytes, Complete: row.Complete,
			Category: row.Category, ApparentBytes: row.ApparentBytes, AllocatedBytes: row.AllocatedBytes,
			Measured: row.Measured, Protection: row.Protection, Cleanup: row.Cleanup,
			TargetCount: row.TargetCount, ScannedAt: scannedPtr, Reasons: row.Reasons,
		})
	}
	s.store(func(next *systemSample) { next.Breakdown = rows; next.BreakdownAt = &at; next.Inventory = &inv })
	if s.deps.onInventory != nil {
		s.deps.onInventory(inv)
	}
}

// KickInventory refreshes the storage inventory on the six-hour cadence. Manual
// Measure-now requests continue to use the 10-minute KickBreakdown cache.
func (s *systemSampler) KickInventory() {
	if s == nil {
		return
	}
	now := s.deps.clock()
	snap := s.Snapshot()
	if snap.BreakdownAt != nil && now.Sub(*snap.BreakdownAt) < systemInventoryInterval {
		return
	}
	s.KickBreakdown()
}

func levelRank(v string) int {
	switch v {
	case "critical":
		return 3
	case "warn":
		return 2
	case "unknown":
		return 1
	default:
		return 0
	}
}

func systemLevels(s systemSample, now time.Time, prev levelState) (systemLevelsDTO, levelState) {
	if prev.Current == nil {
		prev.Current = map[string]string{}
	}
	if prev.Worse == nil {
		prev.Worse = map[string]int{}
	}
	if prev.Pending == nil {
		prev.Pending = map[string]string{}
	}
	raw := map[string]string{}
	text := map[string]string{}
	sampled := map[string]bool{}
	if s.DiskFreeMB == nil || s.DiskTotalMB == nil || *s.DiskTotalMB <= 0 {
		raw["disk"] = "unknown"
		text["disk"] = "disk: unavailable"
	} else {
		floor := int64(s.DiskFloorMB)
		// Free space decides. A percentage alone flags a 1 TB disk with
		// 70 GB free, which is not a problem worth a warning; it only counts
		// when the absolute free space is also small.
		criticalMB := max(int64(2048), floor)
		warnMB := max(int64(10240), 2*floor)
		ratio := float64(*s.DiskFreeMB) / float64(*s.DiskTotalMB)
		free := *s.DiskFreeMB
		raw["disk"] = "ok"
		if free < criticalMB || (ratio < .02 && free < 5120) {
			raw["disk"] = "critical"
		} else if free < warnMB || (ratio < .05 && free < 20480) {
			raw["disk"] = "warn"
		}
		text["disk"] = "disk: " + formatSystemGB(*s.DiskFreeMB) + " free on ~/.solu volume"
		sampled["disk"] = true
	}
	detail := map[string]string{}
	if h := s.HostDisk; h != nil {
		available := (h.State == hostStorageOK || h.State == hostStorageVHDXUnresolved) && h.FreeMB != nil && h.TotalMB != nil && *h.TotalMB > 0
		if !available {
			raw["hostDisk"], text["hostDisk"] = "unknown", "host disk: unavailable ("+h.State+")"
		} else {
			floor := int64(0)
			if h.FloorMB != nil {
				floor = *h.FloorMB
			}
			raw["hostDisk"] = hostDiskAlarmLevel(*h.FreeMB, *h.TotalMB, floor)
			text["hostDisk"] = "host disk: " + formatSystemGB(*h.FreeMB) + " free on " + h.Volume
			pct := 100 * float64(*h.FreeMB) / float64(*h.TotalMB)
			parts := []string{fmt.Sprintf("%.1f%% of %s free", pct, formatSystemGB(*h.TotalMB))}
			if h.State != hostStorageVHDXUnresolved && h.VHDXPhysicalMB != nil {
				sparse := "sparse state unknown"
				if h.Sparse != nil && *h.Sparse {
					sparse = "sparse"
				} else if h.Sparse != nil {
					sparse = "not sparse"
				}
				parts = append(parts, "VHDX "+formatSystemGB(*h.VHDXPhysicalMB)+" physical, "+sparse)
			}
			if h.Offender != nil && h.Offender.Text != "" {
				parts = append(parts, h.Offender.Text)
			}
			if h.UnexplainedLossMB != nil {
				parts = append(parts, "unexplained host loss "+formatSystemGB(*h.UnexplainedLossMB))
			}
			detail["hostDisk"] = strings.Join(parts, " · ")
			sampled["hostDisk"] = true
		}
	}
	if s.MemAvailableMB == nil {
		raw["hostMemory"] = "unknown"
		text["hostMemory"] = "host memory: unavailable"
	} else {
		raw["hostMemory"] = "ok"
		floor := int64(s.MemFloorMB)
		if floor > 0 && *s.MemAvailableMB < floor {
			raw["hostMemory"] = "critical"
		} else if floor > 0 && *s.MemAvailableMB < 2*floor {
			raw["hostMemory"] = "warn"
		}
		prefix := "host memory: "
		if s.HostProvenance == "wsl" {
			prefix = "WSL VM memory: "
		}
		text["hostMemory"] = prefix + formatSystemGB(*s.MemAvailableMB) + " available (floor " + formatSystemGB(floor) + ")"
		sampled["hostMemory"] = true
	}
	if s.HostProvenance != "" && (s.Pressure == nil || !s.Pressure.MemPSI.OK) {
		raw["hostPressure"], text["hostPressure"] = "unknown", "host pressure: unavailable"
	} else if s.Pressure != nil {
		raw["hostPressure"] = "ok"
		if s.Pressure.MemPSI.Full10 > 1 {
			raw["hostPressure"] = "critical"
		} else if s.Pressure.MemPSI.Some10 > 10 || s.Pressure.SwapOutKnown && s.Pressure.SwapOutPerSec > 0 {
			raw["hostPressure"] = "warn"
		}
		text["hostPressure"] = "host pressure: memory PSI some10 " + formatSystemDecimal(s.Pressure.MemPSI.Some10) + "% · full10 " + formatSystemDecimal(s.Pressure.MemPSI.Full10) + "%"
		sampled["hostPressure"] = true
	}
	if s.HostMem != nil && s.HostMem.Status != "disabled" && s.HostMem.Status != "n/a" {
		h := s.HostMem
		raw["windowsHost"] = "unknown"
		if h.Status == "ok" {
			raw["windowsHost"] = "ok"
			if h.FreeMB < hostFreeRedMB {
				raw["windowsHost"] = "critical"
			} else if h.FreeMB < hostFreeGreenMB {
				raw["windowsHost"] = "warn"
			}
		}
		text["windowsHost"] = "Windows host memory: " + h.Status
		if h.FreeMB > 0 {
			text["windowsHost"] += " · " + formatSystemGB(h.FreeMB) + " free"
		}
		if !h.SampledAt.IsZero() {
			text["windowsHost"] += " · " + max(time.Duration(0), now.Sub(h.SampledAt)).Round(time.Second).String() + " old"
		}
		sampled["windowsHost"] = true
	}
	guardUsed := s.GuardUsedMB
	memoryLabel := "guard accounting"
	if guardUsed == nil {
		guardUsed = s.DaemonRssMB
		memoryLabel = "RSS"
	}
	if guardUsed == nil {
		raw["daemonMemory"] = "unknown"
		text["daemonMemory"] = "daemon memory: unavailable"
	} else {
		guard := float64(s.MemGuardMB)
		warn, critical := 3072.0, 4915.0
		if guard > 0 {
			warn, critical = .5*guard, .8*guard
		}
		raw["daemonMemory"] = "ok"
		if *guardUsed >= critical {
			raw["daemonMemory"] = "critical"
		} else if *guardUsed >= warn {
			raw["daemonMemory"] = "warn"
		}
		displayGuard := guard
		if displayGuard == 0 {
			displayGuard = critical
		}
		text["daemonMemory"] = "daemon memory: " + formatSystemGB(int64(*guardUsed)) + " " + memoryLabel + " of " + formatSystemGB(int64(displayGuard)) + " guard"
		sampled["daemonMemory"] = true
	}
	if s.DaemonCpuPct == nil {
		raw["daemonCpu"] = "unknown"
		text["daemonCpu"] = "daemon CPU: awaiting sample"
	} else {
		raw["daemonCpu"] = "ok"
		if *s.DaemonCpuPct >= 60 {
			raw["daemonCpu"] = "warn"
		}
		text["daemonCpu"] = "daemon CPU: " + formatSystemDecimal(*s.DaemonCpuPct) + "% sustained"
		sampled["daemonCpu"] = true
	}
	for key, on := range map[string]bool{"engine": s.EngineWarn, "runtimes": s.RuntimesWarn, "automation": s.AutomationWarn, "cloud": s.CloudWarn} {
		raw[key] = "ok"
		if on {
			raw[key] = "warn"
		}
	}
	engineParts := []string{}
	if s.EngineResourceWait > 0 {
		engineParts = append(engineParts, systemCountPhrase(s.EngineResourceWait, "resource-waited task", "resource-waited tasks"))
	}
	if s.EngineEnvironmentWait > 0 {
		engineParts = append(engineParts, systemCountPhrase(s.EngineEnvironmentWait, "environment-parked task", "environment-parked tasks"))
	}
	if s.EngineQuotaPaused > 0 {
		engineParts = append(engineParts, systemCountPhrase(s.EngineQuotaPaused, "quota-paused task", "quota-paused tasks"))
	}
	if s.EngineDiskLow {
		engineParts = append(engineParts, "disk floor active")
	}
	if s.EngineRollback {
		rollback := "recent advisory cap rollback to " + itoa64(int64(s.EngineRollbackCap)) + " (" + s.EngineRollbackReason + ")"
		if s.EngineEffectiveCap > 0 {
			rollback += " · effective cap " + itoa64(int64(s.EngineEffectiveCap))
		}
		if s.EngineFloorCap > s.EngineRollbackCap {
			rollback += " · configured minimum " + itoa64(int64(s.EngineFloorCap))
		}
		engineParts = append(engineParts, rollback)
	}
	if s.EngineHostDiskLow {
		engineParts = append(engineParts, "host disk floor active")
	}
	text["engine"] = "engine: " + strings.Join(engineParts, " · ")
	if len(engineParts) == 0 {
		text["engine"] = "engine: healthy"
	}
	text["runtimes"] = "runtimes: " + systemCountPhrase(s.RuntimesUnhealthy, "unhealthy runtime", "unhealthy runtimes")
	automationParts := []string{}
	if s.AutomationErrors24h > 0 {
		automationParts = append(automationParts, systemCountPhrase(s.AutomationErrors24h, "failed run in 24h", "failed runs in 24h"))
	}
	if s.AutomationBlocked > 0 {
		automationParts = append(automationParts, systemCountPhrase(s.AutomationBlocked, "blocked agent", "blocked agents"))
	}
	text["automation"] = "automation: " + strings.Join(automationParts, " · ")
	if len(automationParts) == 0 {
		text["automation"] = "automation: healthy"
	}
	text["cloud"] = "cloud: relay offline"
	if !s.CloudRelayOffline && s.CloudWarn {
		text["cloud"] = "cloud: connectivity degraded"
	}
	if s.EngineUnknown {
		raw["engine"], text["engine"] = "unknown", "engine: unavailable"
	}
	if s.RuntimesUnknown && !s.RuntimesWarn {
		raw["runtimes"], text["runtimes"] = "unknown", "runtimes: partial data"
	}
	if s.CloudCritical {
		raw["cloud"] = "critical"
		text["cloud"] = "cloud: credentials invalid"
	}
	for key, want := range raw {
		cur, exists := prev.Current[key]
		if !exists {
			cur = want
			if sampled[key] && levelRank(want) > levelRank("ok") {
				cur = "ok"
			}
		}
		if sampled[key] && levelRank(want) > levelRank(cur) {
			if prev.Pending[key] == want {
				prev.Worse[key]++
			} else {
				prev.Pending[key] = want
				prev.Worse[key] = 1
			}
			if prev.Worse[key] >= 2 {
				cur = want
				prev.Worse[key] = 0
				delete(prev.Pending, key)
			}
		} else {
			cur = want
			prev.Worse[key] = 0
			delete(prev.Pending, key)
		}
		prev.Current[key] = cur
	}
	reasons := []systemLevelReason{}
	overall := "ok"
	for key, level := range prev.Current {
		if level != "ok" {
			reasons = append(reasons, systemLevelReason{Key: key, Level: level, Text: text[key], Detail: detail[key]})
		}
		if levelRank(level) > levelRank(overall) {
			overall = level
		}
	}
	sort.Slice(reasons, func(i, j int) bool {
		ri, rj := levelRank(reasons[i].Level), levelRank(reasons[j].Level)
		if ri != rj {
			return ri > rj
		}
		return reasons[i].Key < reasons[j].Key
	})
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0].Text
	}
	return systemLevelsDTO{Overall: overall, Reason: reason, Reasons: reasons, Areas: cloneLevelMap(prev.Current)}, prev
}

// Thresholds for the Windows-disk alarm a person sees. They are deliberately
// separate from the host-disk floor (max(100 GB, 5%) by default), which only
// defers heavy operator work: 90 GB free on a 1 TB laptop is not "almost full".
const (
	hostDiskAlarmCriticalMB  = 20 * 1024
	hostDiskAlarmCriticalPct = 5
	hostDiskAlarmWarnMB      = 50 * 1024
	hostDiskAlarmWarnPct     = 10
)

// hostDiskAlarmLevel is critical only when free space is under both 20 GB and
// 5% of the drive, and warn under both 50 GB and 10%. An operator floor set
// lower than those (via settings or env) lowers the alarm with it; the default
// floor, being higher, never raises it.
func hostDiskAlarmLevel(freeMB, totalMB, floorMB int64) string {
	criticalMB := min(int64(hostDiskAlarmCriticalMB), totalMB*hostDiskAlarmCriticalPct/100)
	warnMB := min(int64(hostDiskAlarmWarnMB), totalMB*hostDiskAlarmWarnPct/100)
	if floorMB > 0 {
		criticalMB = min(criticalMB, floorMB)
		warnMB = min(warnMB, 2*floorMB)
	}
	switch {
	case freeMB < criticalMB:
		return "critical"
	case freeMB < warnMB:
		return "warn"
	}
	return "ok"
}

func cloneLevelMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func formatSystemGB(mb int64) string {
	v := float64(mb) / 1024
	// Small local formatting avoids importing fmt on the hot sampler path.
	whole := int64(v*10 + .5)
	return itoa64(whole/10) + "." + itoa64(whole%10) + " GB"
}

func formatSystemDecimal(v float64) string {
	whole := int64(v*10 + .5)
	return itoa64(whole/10) + "." + itoa64(whole%10)
}

func systemCountPhrase(count int, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return itoa64(int64(count)) + " " + label
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
