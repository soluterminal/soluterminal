package main

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	hp "soluterminal/helperproto"
)

const (
	testAttestAgentID = "0123456789abcdef0123456789abcdef"
	testAttestPaneID  = "pane_0123456789abcdef"
	testAttestUID     = 1000
)

type fakeAttestProcess struct {
	ppid  int
	start uint64
}

// fakeAttestWorld is a process table and an agent index.
type fakeAttestWorld struct {
	procs  map[int]fakeAttestProcess
	agents map[int]hp.AgentInfo
	// onRead runs after each start-time read, so a test can change the table
	// in the middle of a walk.
	onRead func(pid int)
	// peerExited makes the peer's pidfd report that the connector is gone.
	peerExited bool
	closed     int
}

// peer is the kernel's view of a connection from pid.
func (w *fakeAttestWorld) peer(uid, pid int) helperPeer {
	return helperPeer{UID: uid, PID: pid,
		alive: func() bool { return !w.peerExited && w.closed == 0 },
		close: func() { w.closed++ },
	}
}

func (w *fakeAttestWorld) AgentByPid(pid int) (hp.AgentInfo, bool) {
	ref, ok := w.agents[pid]
	if !ok {
		return hp.AgentInfo{}, false
	}
	// The real index checks the live process against the record.
	if p, live := w.procs[pid]; !live || p.start != ref.StartTicks {
		return hp.AgentInfo{}, false
	}
	return ref, true
}

func (w *fakeAttestWorld) attestor() *helperAttestor {
	return &helperAttestor{index: w, proc: helperAttestProc{
		peerOf: func(net.Conn) (helperPeer, error) { return helperPeer{}, errors.New("unused") },
		startTime: func(pid int) (uint64, bool) {
			p, ok := w.procs[pid]
			if w.onRead != nil {
				w.onRead(pid)
			}
			return p.start, ok && p.start > 0
		},
		ppid: func(pid int) int { return w.procs[pid].ppid },
		euid: func() int { return testAttestUID },
	}}
}

// newFakeAttestWorld: init(1) → agent(100) → shell(200) → claude(300) →
// mcp-bridge(400); a second pane's agent(500) → shell(600); a detached
// process(700) reparented to init.
func newFakeAttestWorld() *fakeAttestWorld {
	return &fakeAttestWorld{
		procs: map[int]fakeAttestProcess{
			1:   {0, 1},
			100: {1, 1000},
			200: {100, 1010},
			300: {200, 1020},
			400: {300, 1030},
			500: {1, 2000},
			600: {500, 2010},
			700: {1, 3000},
		},
		agents: map[int]hp.AgentInfo{
			100: {AgentID: testAttestAgentID, PaneID: testAttestPaneID, Pid: 100, StartTicks: 1000},
			500: {AgentID: "fedcba9876543210fedcba9876543210", PaneID: "pane_fedcba9876543210", Pid: 500, StartTicks: 2000},
		},
	}
}

func TestHelperAttestFindsTheAgentAboveADeepDescendant(t *testing.T) {
	w := newFakeAttestWorld()
	a, err := w.attestor().attestPeer(w.peer(testAttestUID, 400))
	if err != nil {
		t.Fatalf("attestPeer: %v", err)
	}
	if a.PaneID != testAttestPaneID || a.Agent.AgentID != testAttestAgentID || a.PID != 400 || a.Start != 1030 || a.UID != testAttestUID {
		t.Fatalf("attestation = %+v", a)
	}
	want := hp.FwdAttest{Agent: testAttestAgentID, PID: 400, Start: 1030, UID: testAttestUID}
	if a.wire() != want {
		t.Fatalf("wire = %+v, want %+v", a.wire(), want)
	}
}

func TestHelperAttestNamesTheOtherPaneForItsOwnProcess(t *testing.T) {
	w := newFakeAttestWorld()
	a, err := w.attestor().attestPeer(w.peer(testAttestUID, 600))
	if err != nil || a.PaneID != "pane_fedcba9876543210" {
		t.Fatalf("attestPeer = %+v, %v", a, err)
	}
}

func TestHelperAttestRefuses(t *testing.T) {
	cases := []struct {
		name  string
		uid   int
		pid   int
		setup func(*fakeAttestWorld)
		want  error
	}{
		{name: "no pid", uid: testAttestUID, pid: 0, want: errHelperAttestNoPeer},
		{name: "another user", uid: 0, pid: 400, want: errHelperAttestOtherUser},
		{name: "peer gone", uid: testAttestUID, pid: 999, want: errHelperAttestNoStart},
		{name: "detached to init", uid: testAttestUID, pid: 700, want: errHelperAttestNoAgent},
		{
			// The pidfd says the connector exited after SO_PEERCRED, so the
			// pid's /proc entry may already be another process.
			name: "peer exited before its start was pinned", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) { w.peerExited = true },
			want:  errHelperAttestPeerGone,
		},
		{
			name: "peer exited during the walk", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) {
				w.onRead = func(pid int) {
					if pid == 100 {
						w.peerExited = true
					}
				}
			},
			want: errHelperAttestChanged,
		},
		{
			// claude(300) is reparented to init while shell(200) lingers as a
			// zombie with its start time unchanged.
			name: "reparented during the walk", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) {
				w.onRead = func(pid int) {
					if pid == 100 {
						w.procs[300] = fakeAttestProcess{1, 1020}
					}
				}
			},
			want: errHelperAttestChanged,
		},
		{
			name: "parent start unreadable", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) { w.procs[300] = fakeAttestProcess{200, 0} },
			want:  errHelperAttestNoStart,
		},
		{
			// shell(200) exited and its pid went to a newer process under
			// the other pane: the walk must not cross into pane 2.
			name: "reused parent pid", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) {
				w.procs[300] = fakeAttestProcess{200, 1020}
				w.procs[200] = fakeAttestProcess{500, 1025}
			},
			want: errHelperAttestReused,
		},
		{
			name: "agent record no longer matches the process", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) { w.procs[100] = fakeAttestProcess{1, 1001} },
			// The index no longer vouches for 100 (its start changed), so the
			// walk goes on past it to init and finds no agent.
			want: errHelperAttestNoAgent,
		},
		{
			name: "index vouches for another process", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) {
				ref := w.agents[100]
				ref.Pid = 101
				w.agents[100] = ref
			},
			want: errHelperAttestChanged,
		},
		{
			name: "bad pane id in the record", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) {
				ref := w.agents[100]
				ref.PaneID = "../pane"
				w.agents[100] = ref
			},
			want: errHelperAttestBadPaneRef,
		},
		{
			name: "bad agent id in the record", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) {
				ref := w.agents[100]
				ref.AgentID = "ABC"
				w.agents[100] = ref
			},
			want: errHelperAttestBadPaneRef,
		},
		{
			name: "chain changed during the walk", uid: testAttestUID, pid: 400,
			setup: func(w *fakeAttestWorld) {
				fired := false
				w.onRead = func(pid int) {
					if pid == 100 && !fired {
						fired = true
						w.procs[300] = fakeAttestProcess{200, 1021}
					}
				}
			},
			want: errHelperAttestChanged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newFakeAttestWorld()
			if tc.setup != nil {
				tc.setup(w)
			}
			a, err := w.attestor().attestPeer(w.peer(tc.uid, tc.pid))
			if !errors.Is(err, tc.want) {
				t.Fatalf("attestPeer = %+v, %v; want %v", a, err, tc.want)
			}
			if a.PaneID != "" {
				t.Fatalf("refused attestation still names pane %q", a.PaneID)
			}
		})
	}
}

func TestHelperAttestStopsAtTheDepthLimit(t *testing.T) {
	w := &fakeAttestWorld{procs: map[int]fakeAttestProcess{}, agents: map[int]hp.AgentInfo{}}
	// A chain longer than the limit with the agent at its top.
	top := 10 + helperAttestMaxDepth + 5
	for pid := 10; pid <= top; pid++ {
		w.procs[pid] = fakeAttestProcess{pid + 1, uint64(10_000 - pid)}
	}
	w.procs[top] = fakeAttestProcess{1, uint64(10_000 - top)}
	w.agents[top] = hp.AgentInfo{AgentID: testAttestAgentID, PaneID: testAttestPaneID, Pid: top, StartTicks: uint64(10_000 - top)}
	if _, err := w.attestor().attestPeer(w.peer(testAttestUID, 10)); !errors.Is(err, errHelperAttestNoAgent) {
		t.Fatalf("attestPeer past the depth limit: %v", err)
	}
}

func TestHelperAttestStillValid(t *testing.T) {
	w := newFakeAttestWorld()
	h := w.attestor()
	a, err := h.attestPeer(w.peer(testAttestUID, 400))
	if err != nil {
		t.Fatal(err)
	}
	if !h.stillValid(a) {
		t.Fatal("fresh attestation is not valid")
	}
	// The peer's pid now names another process.
	w.procs[400] = fakeAttestProcess{300, 1031}
	if h.stillValid(a) {
		t.Fatal("valid after the peer's pid was reused")
	}
	w.procs[400] = fakeAttestProcess{300, 1030}
	// The agent exited.
	delete(w.procs, 100)
	if h.stillValid(a) {
		t.Fatal("valid after the agent exited")
	}
	// The agent's pid came back as a different agent record.
	w.procs[100] = fakeAttestProcess{1, 1000}
	w.agents[100] = hp.AgentInfo{AgentID: "fedcba9876543210fedcba9876543210", PaneID: testAttestPaneID, Pid: 100, StartTicks: 1000}
	if h.stillValid(a) {
		t.Fatal("valid for a different agent id")
	}
	// The connector exited, though its pid's start time reads the same.
	w.agents[100] = hp.AgentInfo{AgentID: testAttestAgentID, PaneID: testAttestPaneID, Pid: 100, StartTicks: 1000}
	if !h.stillValid(a) {
		t.Fatal("not valid after restoring the agent")
	}
	w.peerExited = true
	if h.stillValid(a) {
		t.Fatal("valid after the connector's pidfd reported it gone")
	}
	w.peerExited = false
	a.Close()
	if h.stillValid(a) || w.closed != 1 {
		t.Fatalf("after Close: valid=%t closed=%d", h.stillValid(a), w.closed)
	}
	if h.stillValid(helperAttestation{PID: 400, Start: 1030, Agent: a.Agent}) {
		t.Fatal("an attestation without a pinned peer is valid")
	}
}

func TestHelperAttestIdentifiers(t *testing.T) {
	for _, s := range []string{testAttestAgentID} {
		if !hp.ValidAgentID(s) {
			t.Errorf("agent id %q refused", s)
		}
	}
	for _, s := range []string{"", "0123456789ABCDEF0123456789ABCDEF", "0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdef0", "../0123456789abcdef0123456789abcd"} {
		if hp.ValidAgentID(s) {
			t.Errorf("agent id %q accepted", s)
		}
	}
	if !hp.ValidPaneID(testAttestPaneID) {
		t.Error("pane id refused")
	}
	for _, s := range []string{"", "pane_0123456789ABCDEF", "pane_0123456789abcde", "pane_0123456789abcdef0", "pane_0123456789abcdef\n", "xpane_0123456789abcdef"} {
		if hp.ValidPaneID(s) {
			t.Errorf("pane id %q accepted", s)
		}
	}
}

// lateIndex knows its agent only once ready is set: a just-started CLI can
// connect before the helper has written its agent's record.
type lateIndex struct {
	ready atomic.Bool
	ref   hp.AgentInfo
}

func (l *lateIndex) AgentByPid(pid int) (hp.AgentInfo, bool) {
	if l.ready.Load() && pid == l.ref.Pid {
		return l.ref, true
	}
	return hp.AgentInfo{}, false
}

func lateWorld() (*fakeAttestWorld, *lateIndex, *helperAttestor) {
	w := newFakeAttestWorld()
	idx := &lateIndex{ref: w.agents[100]}
	h := w.attestor()
	h.index = idx
	h.registerWait = 2 * time.Second
	h.proc.peerOf = func(net.Conn) (helperPeer, error) { return w.peer(testAttestUID, 400), nil }
	return w, idx, h
}

func TestHelperAttestWaitsForAJustStartedAgent(t *testing.T) {
	_, idx, h := lateWorld()
	go func() { time.Sleep(150 * time.Millisecond); idx.ready.Store(true) }()
	a, err := h.attestConn(nil)
	if err != nil || a.PaneID != testAttestPaneID {
		t.Fatalf("attestConn: %+v %v", a, err)
	}
}

func TestHelperAttestRegisterWaitIsBounded(t *testing.T) {
	w, _, h := lateWorld()
	h.registerWait = 100 * time.Millisecond
	start := time.Now()
	if _, err := h.attestConn(nil); !errors.Is(err, errHelperAttestNoAgent) || time.Since(start) < h.registerWait {
		t.Fatalf("never registered: %v after %v", err, time.Since(start))
	}
	if w.closed != 1 {
		t.Fatalf("pidfd closed %d times after a refusal", w.closed)
	}
	// Anything but "no agent yet" is refused at once.
	h.registerWait = 2 * time.Second
	h.proc.peerOf = func(net.Conn) (helperPeer, error) { return w.peer(0, 400), nil }
	start = time.Now()
	if _, err := h.attestConn(nil); !errors.Is(err, errHelperAttestOtherUser) || time.Since(start) > time.Second {
		t.Fatalf("other user: %v after %v", err, time.Since(start))
	}
}
