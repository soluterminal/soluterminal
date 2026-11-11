package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUpdateBusy(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Pane, *apiServer)
		want string
	}{
		{name: "idle"},
		{name: "terminal turn", edit: func(p *Pane, _ *apiServer) { p.LastActivityMs = time.Now().UnixMilli() }},
		{name: "native turn", edit: func(p *Pane, _ *apiServer) { p.Kind = "native"; p.nativeInTurn.Store(true) }, want: "Focus pane"},
		{name: "held", edit: func(p *Pane, _ *apiServer) { p.heldDeliveries = []*heldDelivery{{At: time.Now().UnixMilli()}} }, want: "held delivery"},
		{name: "expired held", edit: func(p *Pane, _ *apiServer) {
			p.heldDeliveries = []*heldDelivery{{At: time.Now().Add(-heldDeliveryLimit).UnixMilli()}}
		}},
		{name: "resolved held", edit: func(p *Pane, _ *apiServer) {
			p.heldDeliveries = []*heldDelivery{{At: time.Now().UnixMilli(), resolution: "discarded"}}
		}},
		{name: "exited held", edit: func(p *Pane, _ *apiServer) {
			p.Status = PaneExited
			p.heldDeliveries = []*heldDelivery{{At: time.Now().UnixMilli()}}
		}},
		{name: "respawning held", edit: func(p *Pane, _ *apiServer) {
			p.respawning.Store(true)
			p.heldDeliveries = []*heldDelivery{{At: time.Now().UnixMilli()}}
		}},
		{name: "old connection held", edit: func(p *Pane, _ *apiServer) {
			left, right := net.Pipe()
			t.Cleanup(func() { left.Close(); right.Close() })
			p.heldDeliveries = []*heldDelivery{{At: time.Now().UnixMilli(), connection: left}}
		}},
		{name: "put back", edit: func(p *Pane, _ *apiServer) { p.unsentMessage = &unsentPaneMessage{PutBack: true} }, want: "put-back receipt"},
		{name: "kept put back", edit: func(p *Pane, _ *apiServer) { p.unsentMessage = &unsentPaneMessage{PutBack: true, kept: true} }, want: "put-back receipt"},
		{name: "stale put back", edit: func(p *Pane, _ *apiServer) { p.unsentMessage = &unsentPaneMessage{PutBack: true} }},
		{name: "exited put back", edit: func(p *Pane, _ *apiServer) {
			p.Status = PaneExited
			p.unsentMessage = &unsentPaneMessage{PutBack: true}
		}},
		{name: "respawning put back", edit: func(p *Pane, _ *apiServer) {
			p.respawning.Store(true)
			p.unsentMessage = &unsentPaneMessage{PutBack: true}
		}},
		{name: "native put back", edit: func(p *Pane, _ *apiServer) { p.Kind = "native"; p.unsentMessage = &unsentPaneMessage{PutBack: true} }},
		{name: "other unsent", edit: func(p *Pane, _ *apiServer) { p.unsentMessage = &unsentPaneMessage{NotTyped: true} }},
		{name: "inflight", edit: func(_ *Pane, a *apiServer) { a.collabInflight.Store(2) }, want: "call is in flight"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := net.Pipe()
			t.Cleanup(func() { left.Close(); right.Close() })
			p := &Pane{ID: "p1", Status: PaneRunning, agentConn: left}
			a := &apiServer{mgr: testManagerWithPane(p)}
			if tc.edit != nil {
				tc.edit(p, a)
			}
			if p.unsentMessage != nil && tc.name != "stale put back" {
				p.unsentMessage.connection = p.agentConn
			}
			for _, h := range p.heldDeliveries {
				if tc.name != "old connection held" {
					h.connection = p.agentConn
				}
			}
			hold := a.updateBusy()
			busy, reason, paneID := hold != nil, "", ""
			if hold != nil {
				reason, paneID = hold.reason, hold.paneID
			}
			if busy != (tc.want != "") || (busy && !strings.Contains(reason, tc.want)) {
				t.Fatalf("busy=%v reason=%q, want %q", busy, reason, tc.want)
			}
			if busy && tc.name != "inflight" && paneID != p.ID {
				t.Fatalf("pane = %q", paneID)
			}
		})
	}
}

func TestUpdateBusyFocusOutlastsCollaborationGrace(t *testing.T) {
	p := &Pane{ID: "focus", Kind: "native"}
	p.nativeInTurn.Store(true)
	a := &apiServer{mgr: testManagerWithPane(p)}
	// A concurrent ask must not hide the higher-priority Focus gate.
	a.collabInflight.Store(1)
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.busyCheck = a.updateBusy
	u.busySince = time.Now().Add(-2 * updateBusyGrace)
	u.busyLastObserved = time.Now()
	if busy := u.checkBusy(); busy == nil || busy.cause != updateBusyCauseFocus || busy.paneID != p.ID {
		t.Fatalf("Focus gate after expired collaboration grace = %+v", busy)
	}
	p.nativeInTurn.Store(false)
	if busy := u.checkBusy(); busy != nil {
		t.Fatalf("settled Focus still blocked expired collaboration grace: %+v", busy)
	}
}

func TestUpdateBusyGraceSurvivesSlowDownload(t *testing.T) {
	for _, operation := range []string{"apply", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("INVOCATION_ID", "test-systemd")
			payload := capabilityScript("1.0.0", `{}`)
			var u *Updater
			var binaryURL string
			var static staticRoundTripper
			if operation == "apply" {
				u, binaryURL = leaseTestUpdater(t, payload)
				static = staticRoundTripper{binaryURL: payload}
			} else {
				u, _, _, static = rollbackFixture(t, "2.0.0", "1.0.0", payload)
				binaryURL = "https://cdn.test/solud"
			}
			u.commitSleepFn = func(time.Duration) {}
			u.busyCheck = func() *updateBusyError {
				return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "held delivery"}
			}
			u.busySince = time.Now().Add(-updateBusyGrace - time.Second)
			u.busyLastObserved = time.Now()
			downloaded := false
			u.httpClient = &http.Client{Transport: &drainProbeTransport{
				static: static, binaryURL: binaryURL, onBinary: func() {
					downloaded = true
					if !u.restartGate.Load() {
						t.Error("download has no restart reservation")
					}
					// Simulate elapsed download time without sleeping or running
					// a separate status sampler. The old pre-swap check rearmed
					// its grace and discarded the binary at this point.
					u.mu.Lock()
					u.busySince = u.busySince.Add(-2 * updateBusyObservationGap)
					u.busyLastObserved = time.Now().Add(-2 * updateBusyObservationGap)
					u.mu.Unlock()
				},
			}}
			reachedSwap := false
			stop := errors.New("test stopped at binary swap")
			u.swapBinaryFn = func(string, string) error { reachedSwap = true; return stop }
			var err error
			if operation == "apply" {
				err = u.Apply(context.Background(), func(int) {}, false)
			} else {
				err = u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0"}, func(int) {})
			}
			if !downloaded || !reachedSwap || !errors.Is(err, stop) {
				t.Fatalf("downloaded=%v reachedSwap=%v err=%v", downloaded, reachedSwap, err)
			}
		})
	}
}

func TestUpdateSwapLogsVolatileLossCounts(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	useTestExecutable(t, u)
	bin, _ := u.executable()
	u.commitSleepFn = func(time.Duration) {}
	u.restartLossCounts = func() (int, int, int64) { return 3, 1, 2 }
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(original)
	stop := errors.New("test swap")
	u.swapBinaryFn = func(string, string) error {
		for _, field := range []string{"heldDeliveries=3", "putBackReceipts=1", "collabInflight=2"} {
			if !strings.Contains(logs.String(), field) {
				t.Errorf("swap-time log lacks %s: %s", field, logs.String())
			}
		}
		return stop
	}
	if err := u.commitBinarySwap(bin, bin+".new", swapCommit{fromVersion: "1.0.0", toVersion: "2.0.0", intentReason: restartIntentAutoQuiet}, func(int) {}); !errors.Is(err, stop) {
		t.Fatal(err)
	}
}

func TestUpdateBusyGrace(t *testing.T) {
	busy := true
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.busyCheck = func() *updateBusyError {
		if !busy {
			return nil
		}
		return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "held", paneID: "p1"}
	}
	now := time.Now()
	lastSample := time.Duration(0)
	for _, tc := range []struct {
		offset     time.Duration
		busy, want bool
	}{
		{0, true, true},
		{updateBusyGrace - time.Nanosecond, true, true},
		{updateBusyGrace, true, false},
		{2 * updateBusyGrace, true, false},
		{3 * updateBusyGrace, false, false},
		{4 * updateBusyGrace, true, true},
	} {
		busy = tc.busy
		// Production observes active holds every pollInterval (or faster).
		if busy {
			for at := lastSample + pollInterval; at < tc.offset; at += pollInterval {
				u.checkBusyAt(now.Add(at))
			}
		}
		lastSample = tc.offset
		if got := u.checkBusyAt(now.Add(tc.offset)) != nil; got != tc.want {
			t.Fatalf("at %s: busy=%v, want %v", tc.offset, got, tc.want)
		}
	}
}

func TestUpdateBusyGraceObservationGap(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.busyCheck = func() *updateBusyError {
		return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "held", paneID: "p1"}
	}
	now := time.Now()
	// Exhaust the first grace with continuous observations.
	for at := time.Duration(0); at <= updateBusyGrace; at += pollInterval {
		u.checkBusyAt(now.Add(at))
	}
	if busy := u.checkBusyAt(now.Add(updateBusyGrace)); busy != nil {
		t.Fatal("first grace did not expire")
	}
	// With no applicable update, nothing may sample this gate for hours. The
	// next busy observation must not inherit the old, already-exhausted grace.
	fresh := now.Add(updateBusyGrace + updateBusyObservationGap + time.Nanosecond)
	if busy := u.checkBusyAt(fresh); busy == nil {
		t.Fatal("fresh episode inherited expired grace")
	}
	if !u.busySince.Equal(fresh) {
		t.Fatalf("start = %v, want %v", u.busySince, fresh)
	}
}

func TestCollabInflightConcurrentWaits(t *testing.T) {
	p := testPaneWithTail(t, "p1", "shell", "")
	a := &apiServer{mgr: testManagerWithPane(p)}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan struct{}, 2)
	original := collabPaneState
	collabPaneState = func(*Pane) PaneRuntimeState { entered <- struct{}{}; <-release; return StateExited }
	defer func() { collabPaneState = original }()
	for i := 0; i < 2; i++ {
		go func() { a.mcpToolWaitForPane(map[string]any{"pane_id": "p1"}, "caller"); done <- struct{}{} }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("wait did not enter")
		}
	}
	if got := a.collabInflight.Load(); got != 2 {
		t.Fatalf("inflight = %d, want 2", got)
	}
	release <- struct{}{}
	<-done
	if got := a.collabInflight.Load(); got != 1 {
		t.Fatalf("inflight = %d, want 1", got)
	}
	release <- struct{}{}
	<-done
	if got := a.collabInflight.Load(); got != 0 {
		t.Fatalf("inflight = %d, want 0", got)
	}
	// Validation failures must release their count too.
	a.mcpToolAskPane(map[string]any{}, "caller")
	a.mcpToolWaitForPane(map[string]any{}, "caller")
	if got := a.collabInflight.Load(); got != 0 {
		t.Fatalf("inflight after invalid calls = %d", got)
	}
}
