package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeFleetBinaryPlatform struct {
	mu        sync.Mutex
	target    fleetTargetBinary
	processes map[int]fleetProcessBinary
}

func (f *fakeFleetBinaryPlatform) Supported() bool { return true }
func (f *fakeFleetBinaryPlatform) Target(string) (fleetTargetBinary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target, nil
}
func (f *fakeFleetBinaryPlatform) Process(pid int, reported string) (fleetProcessBinary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	proc, ok := f.processes[pid]
	if !ok {
		return fleetProcessBinary{}, fmt.Errorf("pid %d unavailable", pid)
	}
	if proc.Version == "" {
		proc.Version = reported
	}
	return proc, nil
}

func fleetTestPane(mgr *Manager, id, conversation string, pid int) *Pane {
	p := &Pane{ID: id, Title: id, Status: PaneRunning, Agent: agentMuse,
		MuseSessionID: conversation, Pid: pid, agentReportedPID: pid, Cols: 100, Rows: 30,
		buffer: NewRingBuffer(1024), subs: map[int64]*subEntry{}, done: make(chan struct{})}
	mgr.mu.Lock()
	mgr.panes[id] = p
	mgr.mu.Unlock()
	return p
}

func fleetTestInspector(mgr *Manager, platform *fakeFleetBinaryPlatform) *fleetBinaryInspector {
	return &fleetBinaryInspector{targetExecutable: platform.target.Path, servingVersion: "1.0.0", mgr: mgr, platform: platform}
}

func TestAgentProtoStaleTreatsUnreportedAsBaseline(t *testing.T) {
	t.Parallel()
	if agentProtoStale(0) {
		t.Fatal("a pre-proto agent (no report) must be treated as the baseline, not stale")
	}
	if agentProtoStale(agentProtoVersion) || agentProtoStale(agentProtoVersion+1) {
		t.Fatal("a current or newer agent protocol must not be stale")
	}
	if agentProtoVersion > agentProtoBaseline && !agentProtoStale(agentProtoVersion-1) {
		t.Fatal("an older agent protocol must be stale")
	}
}

// A pane agent on a different binary is NOT stale unless its agent protocol is
// older: nearly every release changes the binary, almost none change the relay.
func TestFleetInventoryPaneStalenessFollowsAgentProtoNotBinary(t *testing.T) {
	mgr := NewManager(t.TempDir())
	fleetTestPane(mgr, "pane-a", "muse-a", 101)
	old := fleetTestPane(mgr, "pane-b", "muse-b", 102)
	old.agentProto = agentProtoVersion - 1 // 0 = unreported baseline
	target := fleetTargetBinary{Path: "/installed/solud", Version: "2.0.0", Identity: fleetFileIdentity{Device: 8, Inode: 20}}
	platform := &fakeFleetBinaryPlatform{target: target, processes: map[int]fleetProcessBinary{
		os.Getpid(): {PID: os.Getpid(), Identity: target.Identity, Executable: target.Path, Version: target.Version},
		101:         {PID: 101, Identity: fleetFileIdentity{Device: 8, Inode: 19}, Executable: "/old/solud", Version: "1.0.0"},
		102:         {PID: 102, Identity: fleetFileIdentity{Device: 8, Inode: 18}, Executable: "/older/solud", Version: "0.9.0"},
		999:         {PID: 999, Identity: fleetFileIdentity{Device: 8, Inode: 17}, Executable: "/unowned/solud", Version: "0.8.0"},
	}}
	status := fleetTestInspector(mgr, platform).Status()
	if status.InventoryError != "" || len(status.Components) != 3 {
		t.Fatalf("manager-scoped inventory = %+v", status)
	}
	if status.Components[0].Role != "daemon" || status.Components[1].PaneID != "pane-a" || status.Components[2].PaneID != "pane-b" {
		t.Fatalf("inventory order/scope = %+v", status.Components)
	}
	for _, c := range status.Components[1:] {
		if !c.BinaryDiffers {
			t.Fatalf("differing inode not reported for diagnosis: %+v", c)
		}
	}
	// agentProtoVersion is 1 and a 0 report is the baseline, so today no
	// pane is stale even though every pane binary differs.
	if agentProtoVersion == agentProtoBaseline {
		if status.StaleCount != 0 || status.Components[1].Stale || status.Components[2].Stale {
			t.Fatalf("binary-only difference flagged stale: %+v", status.Components)
		}
	}
	mgr.panes["pane-a"].agentProto = agentProtoVersion
	if c := fleetTestInspector(mgr, platform).Status().Components[1]; c.Stale || c.AgentProto != agentProtoVersion {
		t.Fatalf("current-proto pane = %+v", c)
	}
}

func TestFleetInventoryDaemonStalenessIsBinaryIdentity(t *testing.T) {
	t.Parallel()
	target := fleetTargetBinary{Path: "/installed/solud", Version: "2.0.0", Identity: fleetFileIdentity{Device: 1, Inode: 2}}
	proc := fleetProcessBinary{PID: 7, Version: "1.0.0", Identity: fleetFileIdentity{Device: 1, Inode: 1}}
	if c := classifyFleetComponent("daemon", "", "Daemon", proc, target, 0); !c.Stale || c.Reason != "running version 1.0.0; installed version is 2.0.0" {
		t.Fatalf("stale daemon = %+v", c)
	}
	proc.Identity = target.Identity
	if c := classifyFleetComponent("daemon", "", "Daemon", proc, target, 0); c.Stale {
		t.Fatalf("current daemon = %+v", c)
	}
}

func TestFleetInventoryReportsUnavailableTargetMetadata(t *testing.T) {
	mgr := NewManager(t.TempDir())
	platform := &fakeFleetBinaryPlatform{
		target: fleetTargetBinary{Path: "/installed/solud", Identity: fleetFileIdentity{Device: 1, Inode: 2}},
		processes: map[int]fleetProcessBinary{
			os.Getpid(): {PID: os.Getpid(), Identity: fleetFileIdentity{Device: 1, Inode: 2}, Executable: "/installed/solud", Version: "1.0.0"},
		},
	}
	if status := fleetTestInspector(mgr, platform).Status(); status.InventoryError == "" {
		t.Fatalf("unavailable target metadata status = %+v", status)
	}
}

func TestFleetResumeOptionsPreserveIdentityAndConfiguration(t *testing.T) {
	snap := PaneSnapshot{
		ID: "pane-stable", Agent: agentClaude, ClaudeSessionID: "claude-conversation",
		Provider: "claude", Role: paneRoleOrchestrator, ContextPack: "/context", Effort: "high",
		Cols: 120, Rows: 40, Title: "Release executor", TitleOverride: "Pinned title",
	}
	opts := resumeSpawnOpts(snap, snap.Cols, snap.Rows)
	if opts.ID != snap.ID || opts.ClaudeSessionID != snap.ClaudeSessionID || opts.Provider != snap.Provider ||
		opts.Role != snap.Role || opts.ContextPack != snap.ContextPack || opts.Effort != snap.Effort ||
		opts.Cols != snap.Cols || opts.Rows != snap.Rows || opts.Title != snap.Title || opts.TitleOverride != snap.TitleOverride {
		t.Fatalf("resume options lost stable identity/configuration:\n got %+v\nwant %+v", opts, snap)
	}
	if !reflect.DeepEqual(opts.Cmd, []string{"claude", "--resume", "claude-conversation"}) {
		t.Fatalf("Claude resume command = %#v", opts.Cmd)
	}
}

// The per-pane "older build" dot rides /api/panes and must not light for an
// agent that merely runs a different binary or never reported a protocol.
func TestPaneDTOAgentStaleFollowsAgentProto(t *testing.T) {
	mgr := NewManager(t.TempDir())
	p := fleetTestPane(mgr, "pane-a", "muse-a", 101)
	if dto := makePaneDTOWithProcessSnapshot(p, nil, nil, paneProcessSnapshot{}); dto.AgentStale || dto.AgentVersion != "" {
		t.Fatalf("unreported agent = version %q stale %v", dto.AgentVersion, dto.AgentStale)
	}
	p.mu.Lock()
	p.agentVersion, p.agentProto = "0.0.1142", 0
	p.mu.Unlock()
	if dto := makePaneDTOWithProcessSnapshot(p, nil, nil, paneProcessSnapshot{}); dto.AgentStale || dto.AgentVersion != "0.0.1142" {
		t.Fatalf("pre-proto agent = version %q stale %v, want baseline (not stale)", dto.AgentVersion, dto.AgentStale)
	}
	p.mu.Lock()
	p.agentProto = agentProtoVersion
	p.mu.Unlock()
	if dto := makePaneDTOWithProcessSnapshot(p, nil, nil, paneProcessSnapshot{}); dto.AgentStale {
		t.Fatal("current-proto agent flagged stale")
	}
}

func TestRestartAdmissionCloseIsBoundedAndIdempotent(t *testing.T) {
	b := newRestartAdmissionBarrier()
	release, err := b.admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := b.close(ctx); err == nil {
		t.Fatal("close unexpectedly crossed active invocation")
	}
	release()
	reopen, err := b.close(context.Background())
	if err != nil {
		t.Fatalf("barrier did not reopen after cancellation: %v", err)
	}
	reopen()
	reopen()
	if closed, crossing := b.snapshot(); closed || crossing != 0 {
		t.Fatalf("snapshot = closed %v crossing %d", closed, crossing)
	}
}
