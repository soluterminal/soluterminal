package main

// resourceSettings is the explicit-override layer of resource governance.
// A nil field means unset: the resolver falls through to the environment
// variable and then to the adaptive/static default. The caps were env-only
// until stale emergency values silently serialized all heavy work for three
// days with no visibility and no runtime control.
type resourceSettings struct {
	MaxConcurrentTasks *int `json:"maxConcurrentTasks,omitempty"`
	MaxHeavyOps        *int `json:"maxHeavyOps,omitempty"`
	// HeavyCapFloor is an operator floor the adaptive controller may never
	// lower below. A pinned MaxHeavyOps remains the highest-precedence override.
	HeavyCapFloor *int `json:"heavyCapFloor,omitempty"`
	// MaxSuites bounds the suite command class inside MaxHeavyOps. Nil falls
	// through to SOLU_WORKFLOW_MAX_SUITES and then the core-count formula.
	MaxSuites *int `json:"maxSuites,omitempty"`
	// OverdueGrantSec tunes the global pacing between head-only overdue grants.
	// Precedence is Settings > SOLU_WORKFLOW_OVERDUE_GRANT_SEC >
	// overdueGrantIntervalDefault (15s), clamped [0, overdueGrantSecMax].
	// Zero disables the inter-grant wait so a fitting queue head is granted on
	// its next overdue check.
	OverdueGrantSec *int `json:"overdueGrantSec,omitempty"`
	// MaxAgentRuns and ReservedBookkeepingRuns govern the shared ScheduleStore
	// task-schedule pool, not the workflow engine. They inherit
	// SOLU_AGENT_MAX_RUNS and SOLU_AGENT_RESERVED_RUNS respectively.
	MaxAgentRuns            *int `json:"maxAgentRuns,omitempty"`
	ReservedBookkeepingRuns *int `json:"reservedBookkeepingRuns,omitempty"`
	// MemFloorMB/DiskFloorMB keep the documented env semantics, where an
	// explicit 0 disables that gate rather than meaning "unset".
	MemFloorMB  *int `json:"memFloorMB,omitempty"`
	DiskFloorMB *int `json:"diskFloorMB,omitempty"`
	// These govern only the Windows host layer behind a WSL distro VHDX and
	// are inert on native Linux, macOS, Windows, and disabled probes.
	HostDiskFloorMB      *int `json:"hostDiskFloorMB,omitempty"`
	HostDiskFloorPct     *int `json:"hostDiskFloorPct,omitempty"`
	HostDiskHysteresisMB *int `json:"hostDiskHysteresisMB,omitempty"`
	BrowserHostReserveMB *int `json:"browserHostReserveMB,omitempty"`
	// CostAwareAdmission toggles weight-aware heavy admission (default on).
	// nil means unset — the resolver falls through to SOLU_WORKFLOW_COST_AWARE
	// and then to the default, exactly like the caps above. False restores
	// pure slot counting.
	CostAwareAdmission *bool `json:"costAwareAdmission,omitempty"`
	// AdaptiveHeavy is the pressure controller kill switch. nil inherits
	// SOLU_WORKFLOW_ADAPTIVE_HEAVY and then defaults on. maxHeavyOps still wins.
	AdaptiveHeavy *bool `json:"adaptiveHeavy,omitempty"`
	// Explicit default-off opt-ins: nil lets the env layer opt in; otherwise
	// they cause no probe, deletion or admission change.
	AdaptiveHostDiskFloor *bool `json:"adaptiveHostDiskFloor,omitempty"`
	RotateWSLCrashDumps   *bool `json:"rotateWSLCrashDumps,omitempty"`
	// EnvironmentEscalateHours bounds newly-created machine-fault incidents.
	// Existing incidents retain their original persisted deadline.
	EnvironmentEscalateHours *int `json:"environmentEscalateHours,omitempty"`
}

func cloneWfResourceSettings(in *resourceSettings) *resourceSettings {
	if in == nil {
		return nil
	}
	out := *in
	if in.MaxConcurrentTasks != nil {
		v := *in.MaxConcurrentTasks
		out.MaxConcurrentTasks = &v
	}
	if in.MaxHeavyOps != nil {
		v := *in.MaxHeavyOps
		out.MaxHeavyOps = &v
	}
	if in.HeavyCapFloor != nil {
		v := *in.HeavyCapFloor
		out.HeavyCapFloor = &v
	}
	if in.MaxSuites != nil {
		v := *in.MaxSuites
		out.MaxSuites = &v
	}
	if in.OverdueGrantSec != nil {
		v := *in.OverdueGrantSec
		out.OverdueGrantSec = &v
	}
	if in.MaxAgentRuns != nil {
		v := *in.MaxAgentRuns
		out.MaxAgentRuns = &v
	}
	if in.ReservedBookkeepingRuns != nil {
		v := *in.ReservedBookkeepingRuns
		out.ReservedBookkeepingRuns = &v
	}
	if in.MemFloorMB != nil {
		v := *in.MemFloorMB
		out.MemFloorMB = &v
	}
	if in.DiskFloorMB != nil {
		v := *in.DiskFloorMB
		out.DiskFloorMB = &v
	}
	for src, dst := range map[*int]**int{
		in.HostDiskFloorMB: &out.HostDiskFloorMB, in.HostDiskFloorPct: &out.HostDiskFloorPct,
		in.HostDiskHysteresisMB: &out.HostDiskHysteresisMB, in.BrowserHostReserveMB: &out.BrowserHostReserveMB,
	} {
		if src != nil {
			v := *src
			*dst = &v
		}
	}
	if in.CostAwareAdmission != nil {
		v := *in.CostAwareAdmission
		out.CostAwareAdmission = &v
	}
	if in.AdaptiveHeavy != nil {
		v := *in.AdaptiveHeavy
		out.AdaptiveHeavy = &v
	}
	if in.AdaptiveHostDiskFloor != nil {
		v := *in.AdaptiveHostDiskFloor
		out.AdaptiveHostDiskFloor = &v
	}
	if in.RotateWSLCrashDumps != nil {
		v := *in.RotateWSLCrashDumps
		out.RotateWSLCrashDumps = &v
	}
	if in.EnvironmentEscalateHours != nil {
		v := *in.EnvironmentEscalateHours
		out.EnvironmentEscalateHours = &v
	}
	return &out
}

type resourceSettingsReader interface{ resourceOverrides() resourceSettings }
