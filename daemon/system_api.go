package main

import (
	"sort"
	"strconv"

	"net/http"
	"os"
	"runtime"

	"time"
)

type systemHealthSummaryDTO struct {
	Level  string `json:"level"`
	Reason string `json:"reason,omitempty"`
	// Every area that is not ok, worst first. Clients that hide an area (the
	// workflow engine on installs that do not use it) recompute the verdict
	// from these instead of showing a reason the user cannot act on.
	Reasons   []systemLevelReason `json:"reasons,omitempty"`
	SampledAt string              `json:"sampledAt,omitempty"`
}

type systemPlatformDTO struct {
	OS     string `json:"os"`
	Procfs bool   `json:"procfs"`
}

type systemDaemonDTO struct {
	Version   string    `json:"version"`
	Pid       int       `json:"pid"`
	UptimeSec int64     `json:"uptimeSec"`
	StartedAt time.Time `json:"startedAt"`
	// RSS stays null when the OS measurement is unavailable (for example,
	// procfs-less Unix hosts); guard accounting never fills in a fictitious RSS.
	RssMB *float64 `json:"rssMB"`
	// goHeapMB measures allocated heap objects. Older daemons incorrectly
	// returned committed Go runtime memory here; that measure is goRuntimeMB.
	GoHeapMB     *float64 `json:"goHeapMB"`
	GoRuntimeMB  *float64 `json:"goRuntimeMB"`
	GuardUsedMB  *float64 `json:"guardUsedMB"`
	Goroutines   *int     `json:"goroutines"`
	CpuPct       *float64 `json:"cpuPct"`
	MemGuardMB   uint64   `json:"memGuardMB"`
	WebVersion   string   `json:"webVersion"`
	BackendID    string   `json:"backendID"`
	ShellVersion string   `json:"shellVersion"`
}

type systemHostDTO struct {
	Cores          int                   `json:"cores"`
	CpuPct         *float64              `json:"cpuPct"`
	LoadAvg1       *float64              `json:"loadAvg1"`
	MemTotalMB     *int64                `json:"memTotalMB"`
	MemAvailableMB *int64                `json:"memAvailableMB"`
	MemFloorMB     int                   `json:"memFloorMB"`
	Provenance     string                `json:"provenance"`
	Pressure       *systemPressureDTO    `json:"pressure"`
	Windows        *systemWindowsHostDTO `json:"windows"`
}

type systemPressureDTO statsPressure

type systemWindowsHostDTO struct {
	Status     string     `json:"status"`
	FreeMB     *int64     `json:"freeMB,omitempty"`
	TotalMB    *int64     `json:"totalMB,omitempty"`
	SampledAt  *time.Time `json:"sampledAt,omitempty"`
	AgeSec     int64      `json:"ageSec,omitempty"`
	Error      string     `json:"error,omitempty"`
	Provenance string     `json:"provenance"`
}

type systemFootprintDTO struct {
	TotalRssMB   *float64 `json:"totalRssMB"`
	DaemonRssMB  *float64 `json:"daemonRssMB"`
	PanesRssMB   *float64 `json:"panesRssMB"`
	WorkersRssMB *float64 `json:"workersRssMB"`
	PaneCount    int      `json:"paneCount"`
	WorkerCount  int      `json:"workerCount"`
	Procs        int      `json:"procs"`
	CpuPct       *float64 `json:"cpuPct"`
	Complete     bool     `json:"complete"`
}

type systemPaneRowDTO struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Agent      string   `json:"agent"`
	Kind       string   `json:"kind"`
	Role       string   `json:"role"`
	Status     string   `json:"status"`
	State      string   `json:"state"`
	Pid        int      `json:"pid"`
	Procs      int      `json:"procs"`
	RssMB      *float64 `json:"rssMB"`
	CpuPct     *float64 `json:"cpuPct"`
	AgentRssMB *float64 `json:"agentRssMB"`
}

type systemWorkerRowDTO struct {
	TaskID    string    `json:"taskId"`
	TaskTitle string    `json:"taskTitle"`
	Stage     string    `json:"stage"`
	Role      string    `json:"role"`
	Runtime   string    `json:"runtime"`
	Pid       int       `json:"pid"`
	Procs     int       `json:"procs"`
	RssMB     *float64  `json:"rssMB"`
	CpuPct    *float64  `json:"cpuPct"`
	StartedAt time.Time `json:"startedAt"`
}

type systemDiskDTO struct {
	Layer                    string                `json:"layer"`
	BlockingLayer            string                `json:"blockingLayer"`
	AnyLow                   bool                  `json:"anyLow"`
	Host                     *systemHostDiskDTO    `json:"host"`
	Path                     string                `json:"path"`
	FreeMB                   *int64                `json:"freeMB"`
	TotalMB                  *int64                `json:"totalMB"`
	FreeBytes                *int64                `json:"freeBytes"`
	UsedBytes                *int64                `json:"usedBytes"`
	TotalBytes               *int64                `json:"totalBytes"`
	Growth                   *systemDiskGrowthDTO  `json:"growth,omitempty"`
	Cleanup                  *systemCleanupDTO     `json:"cleanup,omitempty"`
	FloorMB                  int                   `json:"floorMB"`
	Low                      bool                  `json:"low"`
	MeasuredAt               *time.Time            `json:"measuredAt"`
	Breakdown                []systemDiskBreakdown `json:"breakdown"`
	BreakdownAt              *time.Time            `json:"breakdownAt"`
	Measuring                bool                  `json:"measuring"`
	Fresh                    bool                  `json:"fresh"`
	NextMeasureAfter         *time.Time            `json:"nextMeasureAfter"`
	GoCacheBytes             *int64                `json:"goCacheBytes"`
	WorktreesBytes           *int64                `json:"worktreesBytes"`
	ReclaimableBytes         *int64                `json:"reclaimableBytes"`
	ReclaimableTmpBytes      *int64                `json:"reclaimableTmpBytes"`
	ReclaimableWorktreeBytes *int64                `json:"reclaimableWorktreeBytes"`
	ReclaimedBytes           *int64                `json:"reclaimedBytes"`
}

type systemHostDiskFloorBreakdown struct {
	BaseMB           int64  `json:"baseMB"`
	BrowserReserveMB int64  `json:"browserReserveMB"`
	HysteresisMB     int64  `json:"hysteresisMB,omitempty"`
	Source           string `json:"source"`
}

type systemHostDiskWhyBlocked struct {
	Layer          string                       `json:"layer"`
	Class          string                       `json:"class"`
	FreeMB         int64                        `json:"freeMB"`
	FloorMB        int64                        `json:"floorMB"`
	FloorBreakdown systemHostDiskFloorBreakdown `json:"floorBreakdown"`
	DeficitMB      int64                        `json:"deficitMB"`
}

type systemHostDiskUnblockOption struct {
	Kind              string `json:"kind"`
	ExpectedReclaimMB *int64 `json:"expectedReclaimMB,omitempty"`
	FloorDeltaMB      *int64 `json:"floorDeltaMB,omitempty"`
	Consented         bool   `json:"consented"`
	Action            string `json:"action,omitempty"`
	Command           string `json:"command,omitempty"`
}

type systemHostDiskDTO struct {
	Available          bool                          `json:"available"`
	State              string                        `json:"state"`
	Detail             string                        `json:"detail"`
	Volume             string                        `json:"volume"`
	Distro             string                        `json:"distro"`
	FreeMB             *int64                        `json:"freeMB"`
	TotalMB            *int64                        `json:"totalMB"`
	FreeBytes          *int64                        `json:"freeBytes"`
	TotalBytes         *int64                        `json:"totalBytes"`
	VHDXAllocatedBytes *int64                        `json:"vhdxAllocatedBytes"`
	VHDXApparentBytes  *int64                        `json:"vhdxApparentBytes"`
	DumpBytes          *int64                        `json:"dumpBytes"`
	OffenderScannedAt  *time.Time                    `json:"offenderScannedAt,omitempty"`
	FloorMB            *int64                        `json:"floorMB"`
	FloorPct           int                           `json:"floorPct"`
	HysteresisMB       int64                         `json:"hysteresisMB"`
	Low                bool                          `json:"low"`
	VHDXPath           string                        `json:"vhdxPath"`
	VHDXPhysicalMB     *int64                        `json:"vhdxPhysicalMB"`
	VHDXLogicalMB      *int64                        `json:"vhdxLogicalMB"`
	Sparse             *bool                         `json:"sparse"`
	SampledAt          *time.Time                    `json:"sampledAt"`
	Runbook            string                        `json:"runbook"`
	UnexplainedLossMB  *int64                        `json:"unexplainedLossMB"`
	VHDXGrowthMB       *int64                        `json:"vhdxGrowthMB"`
	LossWindowMin      int                           `json:"lossWindowMin"`
	Offender           *systemHostOffenderDTO        `json:"offender"`
	WhyBlocked         *systemHostDiskWhyBlocked     `json:"whyBlocked,omitempty"`
	UnblockOptions     []systemHostDiskUnblockOption `json:"unblockOptions,omitempty"`
}

type systemHostOffenderDTO struct {
	Label    string `json:"label"`
	Path     string `json:"path"`
	MB       int64  `json:"mb"`
	Files    int    `json:"files"`
	Dumps    int    `json:"dumps"`
	Complete bool   `json:"complete"`
	Text     string `json:"text"`
}

type systemGrowthDeltaDTO struct {
	Category      string `json:"category"`
	Label         string `json:"label"`
	BaselineBytes int64  `json:"baselineBytes"`
	LatestBytes   int64  `json:"latestBytes"`
	DeltaBytes    int64  `json:"deltaBytes"`
	Noise         bool   `json:"noise"`
}

type systemGrowthDTO struct {
	Window     string                 `json:"window"`
	Available  bool                   `json:"available"`
	Continuity string                 `json:"continuity"`
	BaselineAt *time.Time             `json:"baselineAt,omitempty"`
	LatestAt   *time.Time             `json:"latestAt,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
	Deltas     []systemGrowthDeltaDTO `json:"deltas,omitempty"`
}

type systemDiskGrowthDTO struct {
	Day  systemGrowthDTO `json:"day"`
	Week systemGrowthDTO `json:"week"`
}

type systemCleanupDTO struct {
	ScannedAt                *time.Time                `json:"scannedAt,omitempty"`
	Complete                 bool                      `json:"complete"`
	ReclaimableBytes         *int64                    `json:"reclaimableBytes,omitempty"`
	ReclaimableTmpBytes      *int64                    `json:"reclaimableTmpBytes,omitempty"`
	ReclaimableWorktreeBytes *int64                    `json:"reclaimableWorktreeBytes,omitempty"`
	ReclaimedBytes           *int64                    `json:"reclaimedBytes,omitempty"`
	PreviewOnly              bool                      `json:"previewOnly"`
	Scope                    string                    `json:"scope"`
	Operations               []storageCleanupOperation `json:"operations"`
	Skips                    []storageCleanupSkip      `json:"skips,omitempty"`
	State                    string                    `json:"state,omitempty"`
	Message                  string                    `json:"message,omitempty"`
}

func systemGrowthDTOFrom(win storageGrowthWindow) systemGrowthDTO {
	out := systemGrowthDTO{Window: win.Window, Available: win.Available, Continuity: win.Continuity, BaselineAt: win.BaselineAt, LatestAt: win.LatestAt, Reason: win.Reason}
	for _, delta := range win.Deltas {
		out.Deltas = append(out.Deltas, systemGrowthDeltaDTO{Category: delta.Category, Label: delta.Label, BaselineBytes: delta.BaselineBytes, LatestBytes: delta.LatestBytes, DeltaBytes: delta.DeltaBytes, Noise: delta.Noise})
	}
	return out
}

// systemCleanupDTOFrom renders at most 20 normalized operations and the derived
// guest-vs-host state. Zero workflow eligibility is described precisely rather
// than as whole-machine disposability.
func systemCleanupDTOFrom(summary storageCleanupSummary) *systemCleanupDTO {
	if summary.Operations == nil {
		summary.Operations = []storageCleanupOperation{}
	}
	ops := summary.Operations
	if len(ops) > 20 {
		ops = ops[len(ops)-20:]
	}
	out := &systemCleanupDTO{
		ScannedAt: summary.ScannedAt, Complete: summary.Complete,
		ReclaimableBytes: summary.ReclaimableBytes, ReclaimableTmpBytes: summary.ReclaimableTmpBytes,
		ReclaimableWorktreeBytes: summary.ReclaimableWorktreeBytes, ReclaimedBytes: summary.ReclaimedBytes,
		Operations: ops, Skips: summary.Skips, State: summary.State, Message: summary.Message,
		Scope: "Generated storage maintenance; retained workflow data is read-only",
	}
	for _, op := range out.Operations {
		if !op.Executed {
			out.PreviewOnly = true
			break
		}
	}
	return out
}

func systemHostDiskDTOFromSample(h *systemHostDiskSample) *systemHostDiskDTO {
	if h == nil {
		return nil
	}
	dto := &systemHostDiskDTO{Available: (h.State == hostStorageOK || h.State == hostStorageVHDXUnresolved) && h.FreeMB != nil && h.TotalMB != nil, State: h.State, Detail: redactSensitive(h.Detail), Volume: h.Volume, Distro: h.Distro, FreeMB: h.FreeMB, TotalMB: h.TotalMB, FreeBytes: h.FreeBytes, TotalBytes: h.TotalBytes, VHDXAllocatedBytes: h.VHDXAllocatedBytes, VHDXApparentBytes: h.VHDXApparentBytes, DumpBytes: h.DumpBytes, OffenderScannedAt: h.OffenderScannedAt, FloorMB: h.FloorMB, FloorPct: h.FloorPct, HysteresisMB: h.HysteresisMB, Low: h.Low, VHDXPath: redactSensitive(h.VHDXPath), VHDXPhysicalMB: h.VHDXPhysicalMB, VHDXLogicalMB: h.VHDXLogicalMB, Sparse: h.Sparse, SampledAt: h.SampledAt, Runbook: "docs/RUNBOOK-wsl-host-disk.md", UnexplainedLossMB: h.UnexplainedLossMB, VHDXGrowthMB: h.VHDXGrowthMB, LossWindowMin: h.LossWindowMin}
	dto.WhyBlocked, dto.UnblockOptions = h.WhyBlocked, h.UnblockOptions
	if h.Offender != nil {
		dto.Offender = &systemHostOffenderDTO{Label: h.Offender.Label, Path: redactSensitive(h.Offender.Path), MB: h.Offender.MB, Files: h.Offender.Files, Dumps: h.Offender.Dumps, Complete: h.Offender.Complete, Text: h.Offender.Text}
	}
	return dto
}

type systemEngineDTO struct {
	Available          bool       `json:"available"`
	RunningInvocations int        `json:"runningInvocations"`
	Decide             int        `json:"decide"`
	GCLastSweepAt      *time.Time `json:"gcLastSweepAt"`
}

type systemAutomationGroupDTO struct {
	Total      int        `json:"total"`
	Paused     int        `json:"paused"`
	Running    int        `json:"running"`
	Blocked    int        `json:"blocked"`
	Errors24h  int        `json:"errors24h"`
	Enabled    int        `json:"enabled"`
	Disabled   int        `json:"disabled"`
	NextRunAt  *time.Time `json:"nextRunAt"`
	NextFireAt *time.Time `json:"nextFireAt"`
}

type systemAutomationDTO struct {
	Agents    systemAutomationGroupDTO `json:"agents"`
	Schedules systemAutomationGroupDTO `json:"schedules"`
	// Builtins lists the built-in controllers of an autonomy overview, when
	// the build has one (empty otherwise).
	Builtins any `json:"builtins"`
	Autonomy struct {
		GlobalPaused bool `json:"globalPaused"`
		PausedCount  int  `json:"pausedCount"`
	} `json:"autonomy"`
}

type systemSurfaceDTO struct {
	Addr  string `json:"addr"`
	Ports struct {
		Total     int `json:"total"`
		PaneOwned int `json:"paneOwned"`
	} `json:"ports"`
	Previews struct {
		Active int `json:"active"`
	} `json:"previews"`
	Cloud cloudStatus `json:"cloud"`
}

type systemReleaseReadinessDTO struct {
	State                        string `json:"state"`
	UnreleasedTotal              int    `json:"unreleasedTotal"`
	FunctionalCommitCount        int    `json:"functionalCommitCount"`
	OldestFunctionalUnreleasedAt string `json:"oldestFunctionalUnreleasedAt,omitempty"`
	LastReleaseVersion           string `json:"lastReleaseVersion"`
}

type systemReleaseTrainDTO struct {
	Enabled bool   `json:"enabled"`
	Phase   string `json:"phase"`
}

type systemReleaseDTO struct {
	Running               string                     `json:"running"`
	Latest                string                     `json:"latest"`
	Available             bool                       `json:"available"`
	Applying              bool                       `json:"applying"`
	LastSuccessMs         int64                      `json:"lastSuccessMs"`
	LastError             string                     `json:"lastError"`
	ShellCurrent          string                     `json:"shellCurrent"`
	ShellLatest           string                     `json:"shellLatest"`
	ShellAvailable        bool                       `json:"shellAvailable"`
	WorkflowDeferState    string                     `json:"workflowDeferState,omitempty"`
	WorkflowNextRetryMs   int64                      `json:"workflowNextRetryMs,omitempty"`
	WorkflowInflight      int                        `json:"workflowInflight,omitempty"`
	WorkflowDeferAttempts int                        `json:"workflowDeferAttempts,omitempty"`
	Readiness             *systemReleaseReadinessDTO `json:"readiness"`
	Train                 *systemReleaseTrainDTO     `json:"train,omitempty"`
}

type systemStatusDTO struct {
	GeneratedAt       time.Time            `json:"generatedAt"`
	SampledAt         time.Time            `json:"sampledAt"`
	TreeSampledAt     time.Time            `json:"treeSampledAt"`
	SampleIntervalSec int                  `json:"sampleIntervalSec"`
	Platform          systemPlatformDTO    `json:"platform"`
	Level             systemLevelsDTO      `json:"level"`
	Daemon            systemDaemonDTO      `json:"daemon"`
	Host              systemHostDTO        `json:"host"`
	Footprint         systemFootprintDTO   `json:"footprint"`
	Panes             []systemPaneRowDTO   `json:"panes"`
	Workers           []systemWorkerRowDTO `json:"workers"`
	Disk              systemDiskDTO        `json:"disk"`
	Engine            systemEngineDTO      `json:"engine"`
	// Runtimes is the runtime health rows, when the build has runtimes.
	Runtimes   any                 `json:"runtimes"`
	Automation systemAutomationDTO `json:"automation"`
	Surface    systemSurfaceDTO    `json:"surface"`
	Release    systemReleaseDTO    `json:"release"`
}

func (a *apiServer) systemHealthSummary() *systemHealthSummaryDTO {
	if a == nil || a.system == nil {
		return nil
	}
	s := a.system.Snapshot()
	if s.SampledAt.IsZero() {
		return &systemHealthSummaryDTO{Level: "unknown"}
	}
	return &systemHealthSummaryDTO{
		Level:     s.Levels.Overall,
		Reason:    s.Levels.Reason,
		Reasons:   append([]systemLevelReason(nil), s.Levels.Reasons...),
		SampledAt: s.SampledAt.Format(time.RFC3339Nano),
	}
}

func (a *apiServer) systemStatus(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.system == nil {
		jsonError(w, http.StatusServiceUnavailable, "system sampler disabled")
		return
	}
	measureRequested := r.URL.Query().Get("measure") == "1"
	measurementKicked := false
	if measureRequested {
		before := a.system.Snapshot()
		measurementKicked = before.BreakdownAt == nil || time.Since(*before.BreakdownAt) >= systemBreakdownTTL
		a.system.KickBreakdown()
	}
	s := a.system.Snapshot()
	// A tiny directory can finish between KickBreakdown and Snapshot. The
	// triggering response still acknowledges the asynchronous measurement as
	// in progress; subsequent plain polls report the settled cache.
	if measurementKicked {
		s.BreakdownMeasuring = true
	}
	now := time.Now().UTC()
	web := CurrentWebProvenance()
	status := systemStatusDTO{GeneratedAt: now, SampledAt: s.SampledAt, TreeSampledAt: s.TreeSampledAt, SampleIntervalSec: int(systemFastInterval / time.Second), Platform: systemPlatformDTO{OS: runtime.GOOS, Procfs: s.Procfs}}
	status.Daemon = systemDaemonDTO{Version: a.ver, WebVersion: web.WebVersion, BackendID: a.backendID, ShellVersion: a.liveShellVersion(), Pid: os.Getpid(), UptimeSec: int64(time.Since(a.startT).Seconds()), StartedAt: a.startT, RssMB: s.DaemonRssMB, GoHeapMB: s.GoHeapMB, GoRuntimeMB: s.GoRuntimeMB, GuardUsedMB: s.GuardUsedMB, CpuPct: s.DaemonCpuPct, Goroutines: s.Goroutines, MemGuardMB: s.MemGuardMB}
	status.Host = systemHostDTO{Cores: runtime.NumCPU(), CpuPct: s.HostCpuPct, LoadAvg1: s.LoadAvg1, MemTotalMB: s.MemTotalMB, MemAvailableMB: s.MemAvailableMB, MemFloorMB: s.MemFloorMB, Provenance: s.HostProvenance}
	if s.Pressure != nil {
		p := systemPressureDTO(statsPressureFromSample(*s.Pressure, now))
		status.Host.Pressure = &p
	}
	if s.HostMem != nil {
		h := s.HostMem
		hostFreeKnown := h.Status == "ok" || h.Status == "stale"
		ageSec := int64(0)
		if !h.SampledAt.IsZero() {
			ageSec = int64(max(time.Duration(0), now.Sub(h.SampledAt)).Seconds())
		}
		status.Host.Windows = &systemWindowsHostDTO{Status: h.Status, FreeMB: func() *int64 {
			if !hostFreeKnown {
				return nil
			}
			v := h.FreeMB
			return &v
		}(), TotalMB: func() *int64 {
			if h.TotalMB <= 0 {
				return nil
			}
			v := h.TotalMB
			return &v
		}(), AgeSec: ageSec, Error: h.Err, Provenance: h.Provenance}
		if !h.SampledAt.IsZero() {
			v := h.SampledAt
			status.Host.Windows.SampledAt = &v
		}
	}
	status.Panes = make([]systemPaneRowDTO, 0, len(s.Panes))
	status.Workers = make([]systemWorkerRowDTO, 0, len(s.Workers))
	var panesRSS, workersRSS float64
	panesKnown, workersKnown := true, true
	for _, row := range s.Panes {
		var agent *float64
		if row.Ref.AgentRssMB > 0 {
			agent = floatPtr(float64(row.Ref.AgentRssMB))
		}
		status.Panes = append(status.Panes, systemPaneRowDTO{ID: row.Ref.ID, Title: row.Ref.Title, Agent: row.Ref.Agent, Kind: row.Ref.Kind, Role: row.Ref.Role, Status: row.Ref.Status, State: row.Ref.State, Pid: row.Ref.Pid, Procs: row.Cost.Procs, RssMB: row.Cost.RssMB, CpuPct: row.Cost.CpuPct, AgentRssMB: agent})
		if row.Cost.RssMB != nil {
			panesRSS += *row.Cost.RssMB
		} else {
			panesKnown = false
		}
	}
	for _, row := range s.Workers {
		status.Workers = append(status.Workers, systemWorkerRowDTO{TaskID: row.Ref.TaskID, TaskTitle: row.Ref.TaskTitle, Stage: row.Ref.Stage, Role: row.Ref.Role, Runtime: row.Ref.Runtime, Pid: row.Ref.Pid, Procs: row.Cost.Procs, RssMB: row.Cost.RssMB, CpuPct: row.Cost.CpuPct, StartedAt: row.Ref.StartedAt})
		if row.Cost.RssMB != nil {
			workersRSS += *row.Cost.RssMB
		} else {
			workersKnown = false
		}
	}
	sort.Slice(status.Panes, func(i, j int) bool { return ptrFloat(status.Panes[i].RssMB) > ptrFloat(status.Panes[j].RssMB) })
	sort.Slice(status.Workers, func(i, j int) bool { return ptrFloat(status.Workers[i].RssMB) > ptrFloat(status.Workers[j].RssMB) })
	status.Footprint = systemFootprintDTO{TotalRssMB: s.Footprint.RssMB, DaemonRssMB: s.DaemonRssMB, PaneCount: len(status.Panes), WorkerCount: len(status.Workers), Procs: s.Footprint.Procs, CpuPct: s.Footprint.CpuPct, Complete: s.Footprint.Complete}
	if panesKnown {
		status.Footprint.PanesRssMB = floatPtr(panesRSS)
	}
	if workersKnown {
		status.Footprint.WorkersRssMB = floatPtr(workersRSS)
	}
	status.Disk = systemDiskDTO{Layer: "local", BlockingLayer: "none", Path: a.dataDir, FreeMB: s.DiskFreeMB, TotalMB: s.DiskTotalMB, FreeBytes: s.DiskFreeBytes, TotalBytes: s.DiskTotalBytes, UsedBytes: s.DiskUsedBytes, FloorMB: s.DiskFloorMB, MeasuredAt: s.DiskMeasuredAt, Breakdown: s.Breakdown, BreakdownAt: s.BreakdownAt, Measuring: s.BreakdownMeasuring}
	if s.HostDisk != nil {
		status.Disk.Layer = "guest"
		status.Disk.Host = systemHostDiskDTOFromSample(s.HostDisk)
	}
	if s.BreakdownAt != nil {
		next := s.BreakdownAt.Add(systemBreakdownTTL)
		status.Disk.NextMeasureAfter = &next
		status.Disk.Fresh = now.Before(next)
	}
	if s.DiskFreeMB != nil {
		status.Disk.Low = s.DiskFloorMB > 0 && *s.DiskFreeMB < int64(s.DiskFloorMB)
	}
	hostLow := status.Disk.Host != nil && status.Disk.Host.Low
	status.Disk.AnyLow = status.Disk.Low || hostLow
	switch {
	case status.Disk.Low && hostLow:
		status.Disk.BlockingLayer = "both"
	case hostLow:
		status.Disk.BlockingLayer = "host"
	case status.Disk.Low:
		status.Disk.BlockingLayer = "guest"
	default:
		status.Disk.BlockingLayer = "none"
	}
	for _, row := range s.Breakdown {
		if row.Category == storageCatWorkflowWorktrees || row.Name == "workflow/worktrees" {
			v := row.Bytes
			status.Disk.WorktreesBytes = &v
		}
	}

	for _, hook := range systemStatusHooks {
		hook(a, &status)
	}
	status.Automation = a.systemAutomation(now)
	status.Surface = a.systemSurface()
	status.Release = a.systemRelease()
	// The sampler owns threshold hysteresis and the whole-system verdict. A
	// request must never re-run systemLevels with a fresh counter map: doing so
	// can recover or worsen a cached verdict between sampler ticks and make
	// /api/system/status disagree with /api/health.
	status.Level = s.Levels
	writeJSON(w, http.StatusOK, status)
}

func ptrFloat(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

// decorateSystemSample folds cache-only subsystem state into the sampler's
// 15-second verdict. It performs no probes, provider calls, git work or disk
// walks, so /api/health can remain a pure atomic snapshot read while still
// representing the whole system rather than resources alone.
func (a *apiServer) decorateSystemSample(sample *systemSample) {
	if a == nil || sample == nil {
		return
	}
	{
		sample.EngineUnknown = true
	}
	if systemRuntimeSample != nil {
		systemRuntimeSample(a, sample)
	} else {
		sample.RuntimesUnknown = true
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, hook := range systemSampleHooks {
		hook(a, sample, cutoff)
	}
	if a.scheds != nil {
		for _, schedule := range a.scheds.List() {
			for _, run := range schedule.Runs {
				if run.At.Before(cutoff) {
					break
				}
				if run.Status == "error" {
					sample.AutomationWarn = true
					sample.AutomationErrors24h++
				}
			}
		}
	}
	if a.cloudSup != nil {
		cloud := a.cloudSup.status()
		sample.CloudWarn = (cloud.Configured && cloud.HasCreds && cloud.RelayState == relayStateOffline) || cloud.CredsInvalid
		sample.CloudCritical = cloud.CredsInvalid
		sample.CloudRelayOffline = cloud.Configured && cloud.HasCreds && cloud.RelayState == relayStateOffline
	}
}

func (a *apiServer) systemAutomation(now time.Time) systemAutomationDTO {
	var out systemAutomationDTO
	cutoff := now.Add(-24 * time.Hour)
	out.Builtins = []any{}
	for _, hook := range systemAutomationHooks {
		hook(a, &out, cutoff)
	}
	if a.scheds != nil {
		for _, sched := range a.scheds.List() {
			if sched.isAgentRun() {
				continue
			}
			if sched.Enabled {
				out.Schedules.Enabled++
			} else {
				out.Schedules.Disabled++
			}
			if a.scheds.scheduleRunning(sched) {
				out.Schedules.Running++
			}
			if next := a.scheds.NextFire(sched.ID); !next.IsZero() && (out.Schedules.NextFireAt == nil || next.Before(*out.Schedules.NextFireAt)) {
				n := next
				out.Schedules.NextFireAt = &n
			}
			for _, run := range sched.Runs {
				if run.At.Before(cutoff) {
					break
				}
				if run.Status == "error" {
					out.Schedules.Errors24h++
				}
			}
		}
	}

	return out
}

func (a *apiServer) systemSurface() systemSurfaceDTO {
	var out systemSurfaceDTO
	out.Addr = a.addr
	if a.ports != nil {
		ports := a.ports.List()
		out.Ports.Total = len(ports)
		for _, p := range ports {
			if p.PaneID != "" {
				out.Ports.PaneOwned++
			}
		}
	}
	if a.previews != nil {
		out.Previews.Active = len(a.previews.List())
	}
	if a.cloudSup != nil {
		out.Cloud = a.cloudSup.status()
	} else {
		out.Cloud.RelayState = relayStateUnconfigured
	}
	return out
}

func (a *apiServer) systemRelease() systemReleaseDTO {
	out := systemReleaseDTO{Running: a.ver, ShellCurrent: a.liveShellVersion()}
	if a.updater != nil {
		if u := a.updater.Status(); u != nil {
			out.Latest = u.Latest
			out.Available = u.Available
			out.Applying = u.Applying
			out.LastSuccessMs = u.LastSuccessMs
			out.LastError = u.LastError
			out.ShellCurrent = u.ShellCurrent
			out.ShellLatest = u.ShellLatest
			out.ShellAvailable = u.ShellAvailable
			out.WorkflowDeferState = u.WorkflowDeferState
			out.WorkflowNextRetryMs = u.WorkflowNextRetryMs
			out.WorkflowInflight = u.WorkflowInflight
			out.WorkflowDeferAttempts = u.WorkflowDeferAttempts
		}
	}
	for _, hook := range systemReleaseHooks {
		hook(a, &out)
	}
	return out
}

func (a *apiServer) hostStorageStatus(w http.ResponseWriter, r *http.Request) {
	e := a.hostStorageServiceOr503(w)
	if e == nil {
		return
	}
	if e.hostStorage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "state": hostStorageNotWSL})
		return
	}
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(500, n)
	}
	writeJSON(w, http.StatusOK, a.hostStoragePayload(e, e.hostDiskLatestPressure("generic"), limit))
}

func (a *apiServer) hostStorageRefresh(w http.ResponseWriter, r *http.Request) {
	e := a.hostStorageServiceOr503(w)
	if e == nil {
		return
	}
	if e.hostStorage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "state": hostStorageNotWSL})
		return
	}
	s := e.hostStorage.Refresh(r.Context())
	if s.Source == "cache" && s.Detail == "refresh rate limited" {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"retryAfterSec": 30})
		return
	}
	p := e.diskPressureWithSample(e.guestDiskPressure("generic"), s, "generic")
	e.refreshHostOffender(r.Context(), &p)
	e.noteHostDiskCrossing(p)

	writeJSON(w, http.StatusOK, a.hostStoragePayload(e, p, 100))
}

func (a *apiServer) hostStoragePayload(e *hostStorageService, p diskPressureState, limit int) map[string]any {
	sample := systemHostDiskDTOFromSample(systemHostDiskFromPressure(p))
	a.decorateHostDiskDTO(sample, p)
	events := e.hostLedger.Tail(limit)
	if latest, ok := e.hostStorage.Latest(); ok {
		var guestUsed *int64
		if used, measured := diskUsedBytes(e.diskFloorPath()); measured {
			guestUsed = &used
		}
		events = append(events, hostStorageLauncherTailWithGuestUsage(latest.LocalAppData, limit, latest.Distro, guestUsed)...)
	}
	operations := e.hostLedger.TailOperations(limit)
	for _, ev := range events {
		if ev.Source != "launcher" {
			continue
		}
		if op, ok := normalizeLegacyMaintenanceEvent(ev); ok {
			operations = append(operations, op)
		}
	}
	sort.SliceStable(operations, func(i, j int) bool { return operations[i].At.Before(operations[j].At) })
	if len(operations) > limit {
		operations = operations[len(operations)-limit:]
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	// capacitySamples and operations are separate read-only projections. The
	// legacy events key remains for compatibility.
	capacitySamples := make([]hostStorageEvent, 0, len(events))
	for _, ev := range events {
		if ev.Kind == "sample" || ev.Kind == "boot" || ev.Kind == "crossing" ||
			ev.Kind == "admission_blocked" || ev.Kind == "host_loss" {
			capacitySamples = append(capacitySamples, ev)
		}
	}
	floor, pct, hyst, reserve := e.resolveHostDiskFloorMB(), e.resolveHostDiskFloorPct(), e.resolveHostDiskHysteresisMB(), e.resolveBrowserHostReserveMB()
	return map[string]any{
		"available": p.HostKnown, "state": p.HostState, "sample": sample,
		"capacitySamples": capacitySamples,
		"operations":      operations,
		"whyBlocked":      sample.WhyBlocked, "unblockOptions": sample.UnblockOptions,
		"policy": map[string]any{
			"floorMB":          statsResolvedCap{Value: floor.Value, Source: floor.Source},
			"floorPct":         statsResolvedCap{Value: pct.Value, Source: pct.Source},
			"hysteresisMB":     statsResolvedCap{Value: hyst.Value, Source: hyst.Source},
			"browserReserveMB": statsResolvedCap{Value: reserve.Value, Source: reserve.Source},
		},
		"optIns": map[string]statsResolvedFlag{
			"adaptiveFloor":       {Value: e.resolveAdaptiveHostDiskFloor().Value, Source: e.resolveAdaptiveHostDiskFloor().Source},
			"rotateWSLCrashDumps": {Value: e.resolveCrashDumpRotation().Value, Source: e.resolveCrashDumpRotation().Source},
		},
		"pressure": map[string]any{"guestFreeMB": p.GuestFreeMB, "guestFloorMB": p.GuestFloorMB, "guestLow": p.GuestLow, "hostLow": p.HostLow, "blockingLayer": p.BlockingLayer, "unexplainedLossMB": p.HostLoss.UnexplainedMB, "offender": hostOffenderText(p.HostOffender)},
		"events":   events,
	}
}

func (a *apiServer) decorateHostDiskDTO(sample *systemHostDiskDTO, p diskPressureState) {
	if a == nil || sample == nil || a.storage == nil {
		return
	}
	if p.HostLow {
		sample.WhyBlocked = systemHostDiskFromPressure(p).WhyBlocked
	} else {
		sample.WhyBlocked = nil
	}
	sample.UnblockOptions = a.storage.hostDiskUnblockOptions(p)
}

// systemActivity supplies a fresh maintenance interlock independently of the
// retired task engine. Missing live stores cannot prove the host is quiet.
func (a *apiServer) systemActivity(w http.ResponseWriter, r *http.Request) {
	if a.mgr == nil || a.scheds == nil {
		jsonError(w, http.StatusServiceUnavailable, "activity stores unavailable")
		return
	}
	invocations, heavy := 0, 0
	for _, hook := range systemActivityHooks {
		n, ok := hook(a)
		if !ok {
			jsonError(w, http.StatusServiceUnavailable, "activity stores unavailable")
			return
		}
		invocations += n.Invocations
		heavy += n.Heavy
	}
	a.scheds.mu.Lock()
	running := len(a.scheds.taskRunning) + len(a.scheds.agentRunning)
	a.scheds.mu.Unlock()
	panes := 0
	for _, pane := range a.mgr.List() {
		if pane.Snapshot().Status == PaneRunning {
			panes++
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"running": running, "heavyRunning": heavy, "runningInvocations": invocations, "runningPanes": panes})
}
