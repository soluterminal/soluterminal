package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func shortLinuxReattach(m *Manager, window time.Duration) {
	m.linuxReattach = linuxReattachTiming{window: window, first: 20 * time.Millisecond, max: 50 * time.Millisecond}
}

type persistLog struct {
	mu    sync.Mutex
	panes []*Pane
}

func (l *persistLog) put(p *Pane) { l.mu.Lock(); l.panes = append(l.panes, p); l.mu.Unlock() }
func (l *persistLog) last() *Pane {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.panes) == 0 {
		return nil
	}
	return l.panes[len(l.panes)-1]
}

// When the helper dies the pane's stream ends but its agent lives on: the
// pane is reattached to that same agent, never replaced or stopped.
func TestLinuxPaneReattachesAfterTheHelperDies(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 5*time.Second)
	var saved persistLog
	m.persistLinuxRecovery = saved.put
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	_ = far.Close() // the stream breaks; the fake agent stays alive
	<-p.done
	waitFor(t, "the pane reattached and saved", func() bool {
		cur, ok := m.Get(p.ID)
		return ok && cur != p && cur.Snapshot().Status == PaneRunning && saved.last() == cur
	})
	cur, _ := m.Get(p.ID)
	if cur.Linux.AgentID != p.Linux.AgentID || cur.Linux.StartTicks != p.Linux.StartTicks {
		t.Fatal("reattached to another agent")
	}
	if cur.Snapshot().RecoveryError != "" {
		t.Fatal("the live pane kept the reconnecting note")
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if len(ag.Kills()) != 0 {
		t.Fatal("the live agent was stopped")
	}
	_ = agentEnd(t, b, p.Linux.AgentID) // the new stream reached the agent
	if saved.last() != cur {
		t.Fatal("the reattached pane wasn't persisted")
	}
}

// An agent the helper lists as gone leaves an ordinary exited pane.
func TestLinuxPaneStaysExitedWhenItsAgentIsGone(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 5*time.Second)
	var saved persistLog
	m.persistLinuxRecovery = saved.put
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	ag.SetAlive(false)
	_ = far.Close()
	<-p.done
	waitFor(t, "the reconnect settled", func() bool { return saved.last() == p })
	if cur, _ := m.Get(p.ID); cur != p {
		t.Fatal("a gone agent was reattached or replaced")
	}
	if snap := p.Snapshot(); snap.Status != PaneExited || snap.RecoveryError != "" {
		t.Fatalf("status %s, note %q", snap.Status, snap.RecoveryError)
	}
}

// A helper that doesn't come back leaves a tile the person can Resume.
func TestLinuxPaneKeepsATileWhenTheHelperStaysDown(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 300*time.Millisecond)
	var saved persistLog
	m.persistLinuxRecovery = saved.put
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	b.mu.Lock()
	b.listErr = errors.New("helper down")
	b.mu.Unlock()
	_ = far.Close()
	<-p.done
	waitFor(t, "the reconnect gave up", func() bool { return saved.last() == p })
	if snap := p.Snapshot(); snap.RecoveryError != linuxReconnectFailed {
		t.Fatalf("note %q", snap.RecoveryError)
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if len(ag.Kills()) != 0 {
		t.Fatal("an agent the helper couldn't list was stopped")
	}
}

// A person's close never turns into a reconnect.
func TestLinuxPaneCloseDoesNotReconnect(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 5*time.Second)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	// Like a real agent: it answers the kill request by closing the stream,
	// with no exit frame, exactly what a dead helper looks like.
	go func() {
		for {
			ft, _, err := ReadFrame(far)
			if err != nil {
				return
			}
			if ft == FrameKillRequest {
				_ = far.Close()
				return
			}
		}
	}()
	if err := m.Kill(p.ID); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().RecoveryError != "" {
		t.Fatal("a closed pane was marked reconnecting")
	}
	time.Sleep(200 * time.Millisecond)
	if _, ok := m.Get(p.ID); ok {
		t.Fatal("a closed pane came back")
	}
}

// A close while the reattach is between listing and publishing wins: the
// closed pane never comes back.
func TestLinuxPaneCloseDuringReattachIsNotUndone(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 5*time.Second)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	gate := make(chan struct{})
	b.mu.Lock()
	b.openGate = gate
	b.mu.Unlock()
	_ = far.Close()
	<-p.done
	waitFor(t, "the reattach to open the stream", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.opens > 0
	})
	if err := m.Kill(p.ID); err != nil {
		t.Fatal(err)
	}
	close(gate)
	time.Sleep(300 * time.Millisecond)
	if cur, ok := m.Get(p.ID); ok {
		t.Fatalf("a closed pane came back (%s)", cur.Snapshot().Status)
	}
}

// A stream that dies before its first frame doesn't renew the window: a
// helper that lists the agent but can't reach it ends in a Resume tile.
func TestLinuxPaneFailingDialKeepsTheWindow(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 400*time.Millisecond)
	var saved persistLog
	m.persistLinuxRecovery = saved.put
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	b.mu.Lock()
	b.openDead = true
	b.mu.Unlock()
	_ = far.Close()
	<-p.done
	waitFor(t, "the reconnect to give up", func() bool {
		cur, ok := m.Get(p.ID)
		return ok && cur.Snapshot().Status == PaneExited && cur.Snapshot().RecoveryError == linuxReconnectFailed
	})
	b.mu.Lock()
	opens := b.opens
	b.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.opens != opens {
		t.Fatal("reattach attempts went on after the window")
	}
}

// While a person's Resume holds the pane, recovery waits for it instead of
// giving up, and reattaches once the helper is back.
func TestLinuxPaneReattachOutlastsAFailedResume(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 5*time.Second)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	b.mu.Lock()
	b.listErr = errors.New("helper down")
	b.mu.Unlock()
	if !p.tryAcquireRespawn() { // a person's Resume, which then fails
		t.Fatal("no lease")
	}
	_ = far.Close()
	<-p.done
	time.Sleep(200 * time.Millisecond)
	p.releaseRespawn()
	b.mu.Lock()
	b.listErr = nil
	b.mu.Unlock()
	waitFor(t, "the pane reattached", func() bool {
		cur, ok := m.Get(p.ID)
		return ok && cur != p && cur.Snapshot().Status == PaneRunning
	})
}

// A Resume whose tile a close removed first must not publish it again.
func TestLinuxPaneResumeOfAClosedTileIsRefused(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	info := persistedFromSnapshot(p.Snapshot())
	m2 := NewManager(t.TempDir())
	m2.linux = b
	info.RecoveryError = "the Linux pane's helper didn't answer"
	tile := m2.SuspendExisting(&info)
	if !tile.tryAcquireRespawn() {
		t.Fatal("no lease")
	}
	snap := tile.Snapshot()
	m2.mu.Lock()
	delete(m2.panes, tile.ID) // the person's close won the race
	m2.mu.Unlock()
	a := &apiServer{mgr: m2, dataDir: t.TempDir()}
	if got, err := a.respawnInPlace(snap, resumeSpawnOpts(snap, 80, 24)); err == nil || got != nil {
		t.Fatalf("resume of a closed tile: %v %v", got, err)
	}
	if _, ok := m2.Get(tile.ID); ok {
		t.Fatal("a closed tile came back")
	}
}

// A close refused because the stop couldn't be confirmed leaves the tile
// reconnecting, and it is reattached once the helper answers.
func TestLinuxPaneRefusedCloseStillReconnects(t *testing.T) {
	old := linuxPaneStopTimeout
	linuxPaneStopTimeout = 200 * time.Millisecond
	defer func() { linuxPaneStopTimeout = old }()
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 10*time.Second)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	b.mu.Lock()
	b.listErr = errors.New("helper down")
	b.killHang = true
	b.mu.Unlock()
	_ = far.Close()
	<-p.done
	var stop *linuxStopError
	if err := m.KillAsPerson(p.ID, ""); !errors.As(err, &stop) {
		t.Fatalf("close with the helper down: %v", err)
	}
	if p.closing.Load() {
		t.Fatal("a refused close still marks the tile closing")
	}
	b.mu.Lock()
	b.listErr, b.killHang = nil, false
	b.mu.Unlock()
	waitFor(t, "the pane reattached after the refused close", func() bool {
		cur, ok := m.Get(p.ID)
		return ok && cur != p && cur.Snapshot().Status == PaneRunning
	})
}

// Waiting for a listing counts against the window: a helper that holds the
// distro's listing can't keep a pane reconnecting past it.
func TestLinuxPaneListingWaitIsBounded(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	shortLinuxReattach(m, 300*time.Millisecond)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	// Something else holds the distro's listing for longer than the window.
	_, _ = m.listLinuxAgents("Ubuntu", true)
	m.linuxListsMu.Lock()
	e := m.linuxLists["Ubuntu"]
	m.linuxListsMu.Unlock()
	e.sem <- struct{}{}
	defer func() { <-e.sem }()
	start := time.Now()
	_ = far.Close()
	<-p.done
	waitFor(t, "the reconnect to give up", func() bool {
		return p.Snapshot().RecoveryError == linuxReconnectFailed
	})
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("gave up after %v with a 300ms window", took)
	}
}
