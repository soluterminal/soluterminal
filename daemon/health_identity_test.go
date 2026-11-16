package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

type sentinelFleetPlatform struct {
	calls atomic.Int32
}

func (p *sentinelFleetPlatform) Supported() bool {
	p.calls.Add(1)
	return true
}

func (p *sentinelFleetPlatform) Target(string) (fleetTargetBinary, error) {
	p.calls.Add(1)
	return fleetTargetBinary{}, errors.New("sentinel fleet target invoked")
}

func (p *sentinelFleetPlatform) Process(int, string) (fleetProcessBinary, error) {
	p.calls.Add(1)
	return fleetProcessBinary{}, errors.New("sentinel fleet process invoked")
}

func TestHealthIdentityIsAuthenticatedAndSkipsFullHealthDecoration(t *testing.T) {
	rollback, err := newUpdateRollbackStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := rollback.SetHold(rollbackHold{Version: "0.0.1131"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	updater := NewUpdater("0.0.1137", "", "", nil, nil)
	updater.rollback = rollback

	platform := &sentinelFleetPlatform{}
	mgr := NewManager(t.TempDir())
	inspector := newFleetBinaryInspector("/sentinel/solud", "0.0.1137", mgr)
	inspector.platform = platform
	a := &apiServer{
		ver:       "0.0.1137",
		backendID: "wsl",
		startT:    time.Now(),
		mgr:       mgr,
		updater:   updater,
		fleet:     inspector,
	}
	handler := (&authGate{token: "desktop-secret"}).middleware(http.HandlerFunc(a.health))

	request := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://daemon.test/api/health?identity=1", nil)
		req.RemoteAddr = "192.0.2.10:43123"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for _, token := range []string{"", "wrong-secret"} {
		if rec := request(token); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d, want 401; body=%s", token, rec.Code, rec.Body.String())
		}
	}
	rec := request("desktop-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated identity status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["version"] != "0.0.1137" || body["backendID"] != "wsl" {
		t.Fatalf("identity = %#v", body)
	}
	if body["pid"] != float64(os.Getpid()) {
		t.Fatalf("identity pid = %#v, want %d", body["pid"], os.Getpid())
	}
	if body["updateHoldVersion"] != "0.0.1131" {
		t.Fatalf("identity rollback hold = %#v", body["updateHoldVersion"])
	}
	if body["goos"] != runtime.GOOS {
		t.Fatalf("identity goos = %#v, want %q", body["goos"], runtime.GOOS)
	}
	if len(body) != 6 {
		t.Fatalf("identity included full-health fields: %#v", body)
	}
	if calls := platform.calls.Load(); calls != 0 {
		t.Fatalf("identity invoked fleet inspector %d times", calls)
	}
}

func TestOrdinaryHealthRetainsFullPayloadWithoutFleet(t *testing.T) {
	platform := &sentinelFleetPlatform{}
	mgr := NewManager(t.TempDir())
	inspector := newFleetBinaryInspector("/sentinel/solud", "0.0.1137", mgr)
	inspector.platform = platform
	a := &apiServer{
		ver:       "0.0.1137",
		backendID: "wsl",
		startT:    time.Now(),
		mgr:       mgr,
		updater:   NewUpdater("0.0.1137", "", "", nil, nil),
		fleet:     inspector,
	}
	rec := httptest.NewRecorder()
	a.health(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"status", "version", "pid", "backendID", "goos", "uptimeSec", "panes", "shellVersion", "binaryPath"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("ordinary health omitted %q: %#v", field, body)
		}
	}
	// The fleet inventory is debug-only (GET /api/debug/fleet); the health
	// poll must neither carry it nor run it.
	if _, ok := body["fleet"]; ok {
		t.Fatalf("ordinary health carried fleet: %#v", body["fleet"])
	}
	if calls := platform.calls.Load(); calls != 0 {
		t.Fatalf("health invoked fleet inspector %d times", calls)
	}
}
