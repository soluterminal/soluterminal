package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServiceRestartAuthorityAndSingleDispatch(t *testing.T) {
	old := identifyCallerHook
	defer func() { identifyCallerHook = old }()
	for _, tc := range []struct {
		name           string
		kind           callerKind
		remote, origin string
		relay          bool
		code           int
	}{
		{"local human", callerHuman, "127.0.0.1:1234", "http://127.0.0.1:8787", false, 202},
		{"agent", callerPane, "127.0.0.1:1234", "http://127.0.0.1:8787", false, 403},
		{"unverified", callerUnverified, "127.0.0.1:1234", "http://127.0.0.1:8787", false, 403},
		{"phone relay", callerHuman, "127.0.0.1:1234", "http://127.0.0.1:8787", true, 403},
		{"LAN human", callerHuman, "192.168.1.4:1234", "http://127.0.0.1:8787", false, 403},
		{"foreign origin", callerHuman, "127.0.0.1:1234", "http://evil.test", false, 403},
		{"no origin", callerHuman, "127.0.0.1:1234", "", false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: tc.kind} }
			stops, proofs := 0, 0
			a := &apiServer{serviceRestartGate: new(atomic.Bool), stopService: func() { stops++ }, inspectService: func() (serviceRestartProof, error) {
				proofs++
				return serviceRestartProof{"wsl-supervisor", "stable"}, nil
			}}
			handler := http.Handler(http.HandlerFunc(a.restartService))
			if tc.relay {
				handler = markRelayTransport(handler)
			}
			call := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/service/restart", strings.NewReader(fmt.Sprintf(`{"pid":%d}`, os.Getpid())))
				r.RemoteAddr = tc.remote
				r.Header.Set("Origin", tc.origin)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			if w := call(); w.Code != tc.code {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.code == 202 {
				if stops != 1 || proofs != 2 {
					t.Fatalf("stops=%d proofs=%d", stops, proofs)
				}
				if w := call(); w.Code != 409 || stops != 1 {
					t.Fatal("duplicate restart admitted")
				}
			} else if stops != 0 || proofs != 0 {
				t.Fatal("unauthorized request reached service proof/stop")
			}
		})
	}
}

func TestServiceRestartRefusesChangedOwnershipAndUpdate(t *testing.T) {
	old := identifyCallerHook
	defer func() { identifyCallerHook = old }()
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	for _, busy := range []bool{false, true} {
		gate := new(atomic.Bool)
		gate.Store(busy)
		calls := 0
		stops := 0
		a := &apiServer{serviceRestartGate: gate, stopService: func() { stops++ }, inspectService: func() (serviceRestartProof, error) {
			calls++
			if calls > 1 {
				return serviceRestartProof{}, errors.New("lost lock")
			}
			return serviceRestartProof{"wsl-supervisor", "stable"}, nil
		}}
		r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/service/restart", strings.NewReader(fmt.Sprintf(`{"pid":%d}`, os.Getpid())))
		r.RemoteAddr = "127.0.0.1:5000"
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		w := httptest.NewRecorder()
		a.restartService(w, r)
		if w.Code != 409 || stops != 0 {
			t.Fatalf("busy=%v status=%d stops=%d", busy, w.Code, stops)
		}
	}
}

func TestServiceRestartOwnOrigin(t *testing.T) {
	for _, c := range []struct {
		origin       string
		signed, want bool
	}{
		{"http://127.0.0.1:8787", false, true}, {"http://127.0.0.1:18787", false, false},
		{"http://127.0.0.1:18787", true, true}, {"http://127.0.0.1:28787", true, true},
		{"http://127.0.0.1:39703", true, false}, {"http://evil.test", true, false}, {"", true, false},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/service/restart", nil)
		r.Header.Set("Origin", c.origin)
		if got := serviceOwnOrigin(r, c.signed); got != c.want {
			t.Fatalf("%+v got %v", c, got)
		}
	}
}

func TestServiceRestartBodyOnlyAcceptsOnePID(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{`{"pid":42}`, true}, {`{"pid":42,"force":true}`, false}, {`{"pid":42,"pid":42}`, false},
		{`{"pid":42} {}`, false}, {`{"pid":42}garbage`, false}, {`{"pid":"42"}`, false},
		{`{"pid":42}` + strings.Repeat(" ", 1024) + `{}`, false},
	} {
		r := httptest.NewRequest("POST", "/api/service/restart", strings.NewReader(c.body))
		pid, err := serviceExpectedPID(r)
		if (err == nil) != c.want || (c.want && pid != 42) {
			t.Fatalf("body %q: pid=%d err=%v", c.body, pid, err)
		}
	}
}

func TestServiceRestartSystemctlArgvAndShellUpdateRefusal(t *testing.T) {
	old := identifyCallerHook
	defer func() { identifyCallerHook = old }()
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	for _, shellBusy := range []bool{false, true} {
		var argv []string
		a := &apiServer{serviceRestartGate: new(atomic.Bool), stopService: func() { t.Fatal("systemd must own the stop") }, inspectService: func() (serviceRestartProof, error) { return serviceRestartProof{"systemd-user", "stable"}, nil }, runServiceCommand: func(args ...string) (string, error) { argv = append([]string{}, args...); return "", nil }, updater: &Updater{shellApplying: shellBusy}}
		r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/service/restart", strings.NewReader(fmt.Sprintf(`{"pid":%d}`, os.Getpid())))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		w := httptest.NewRecorder()
		a.restartService(w, r)
		if shellBusy {
			if w.Code != 409 || len(argv) != 0 {
				t.Fatal("shell update was interrupted")
			}
		} else if w.Code != 202 || strings.Join(argv, " ") != "/usr/bin/systemctl --user --no-block restart soluterminal.service" {
			t.Fatalf("status=%d argv=%q", w.Code, argv)
		}
	}
}

func TestServiceRestartCountsAndStableIdentity(t *testing.T) {
	id, err := loadDaemonIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &apiServer{trustedDevices: &trustedDevices{daemonKey: func() *daemonIdentity { return id }}, mgr: &Manager{panes: map[string]*Pane{"a": {heldDeliveries: []*heldDelivery{{}, {}}, unsentMessage: &unsentPaneMessage{PutBack: true}}}}}
	held, receipts := a.serviceLossCounts()
	if held != 2 || receipts != 1 {
		t.Fatalf("counts %d/%d", held, receipts)
	}
	identity := a.serviceDaemonIdentity()
	if identity == "" || a.liveHealthIdentity()["daemonIdentity"] != identity {
		t.Fatal("health lost stable identity")
	}
	b := &apiServer{trustedDevices: &trustedDevices{daemonKey: func() *daemonIdentity { return id }}}
	if b.serviceDaemonIdentity() != identity {
		t.Fatal("replacement identity changed")
	}
}
