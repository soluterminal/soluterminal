package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// listCanManage is canManageDevices as GET /api/remote-devices reports it
// to r's caller.
func listCanManage(t *testing.T, a *apiServer, r *http.Request) bool {
	t.Helper()
	r, _ = throughProof(a, r)
	w := httptest.NewRecorder()
	a.handleListRemoteDevices(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	var body struct {
		CanManageDevices *bool `json:"canManageDevices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.CanManageDevices == nil {
		t.Fatalf("list without canManageDevices: %s", w.Body.String())
	}
	return *body.CanManageDevices
}

// The device list's canManageDevices is the approve route's own decision,
// caller by caller: a screen is told it can approve exactly when its
// approval would go through (ST-9).
func TestCanManageDevicesMatchesApproval(t *testing.T) {
	type setup struct {
		a        *apiServer
		approver testDevice
	}
	headers := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}
	viaRelay := func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
	}
	cases := []struct {
		name     string
		mode     string
		devices  bool // one device is approved already
		prepare  func(t *testing.T, s *setup)
		request  func(t *testing.T, s *setup, method, path, nonce string) *http.Request
		wantHint bool
	}{
		{name: "no device yet, browser headers (first device)", mode: humanProofReport,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request { return headers(m, p) }, wantHint: true},
		{name: "no device yet, unsigned over the relay", mode: humanProofReport,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request {
				return viaRelay(httptest.NewRequest(m, p, nil))
			}},
		{name: "browser headers once a device is approved", mode: humanProofReport, devices: true,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request { return headers(m, p) }},
		{name: "signed by an approved device at this computer", mode: humanProofRequire, devices: true,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(t *testing.T, s *setup, m, p, nonce string) *http.Request {
				return s.approver.localRequest(t, s.a, m, p, "", time.Now(), nonce)
			}, wantHint: true},
		{name: "traced browser at this computer", mode: humanProofReport, devices: true,
			prepare: func(t *testing.T, s *setup) { stubTracedBrowser(t) },
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request { return headers(m, p) }, wantHint: true},
		{name: "traced browser under require", mode: humanProofRequire, devices: true,
			prepare: func(t *testing.T, s *setup) { stubTracedBrowser(t) },
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request { return headers(m, p) }},
		{name: "traced automation browser", mode: humanProofReport, devices: true,
			prepare: func(t *testing.T, s *setup) {
				stubTracedBrowser(t)
				peerIsAutomationBrowser = func(int) bool { return true }
			},
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request { return headers(m, p) }},
		{name: "approved device signed over the relay", mode: humanProofRequire, devices: true,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(_ *testing.T, s *setup, m, p, _ string) *http.Request {
				r := viaRelay(httptest.NewRequest(m, p, nil))
				return r.WithContext(context.WithValue(r.Context(), relayVerifiedDeviceKey{}, deviceID(s.approver.spki)))
			}, wantHint: true},
		{name: "approved device on its private channel", mode: humanProofRequire, devices: true,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(_ *testing.T, s *setup, m, p, _ string) *http.Request {
				r := viaRelay(httptest.NewRequest(m, p, nil))
				return r.WithContext(withPrivateRelayDevice(r.Context(), deviceID(s.approver.spki)))
			}, wantHint: true},
		{name: "unsigned over the relay once a device is approved", mode: humanProofReport, devices: true,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request {
				return viaRelay(httptest.NewRequest(m, p, nil))
			}},
		{name: "forwarded for a remote device on headers (hosted Code, unsigned)", mode: humanProofReport, devices: true,
			prepare: func(t *testing.T, s *setup) { stubPeerPID(t, 0, false) },
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request {
				r := headers(m, p)
				r.Header.Set("X-Forwarded-Prefix", "/m/"+testMachine)
				return r
			}},
		{name: "browser headers under SOLU_HUMAN_PROOF=off", mode: humanProofReport, devices: true,
			prepare: func(t *testing.T, s *setup) {
				stubPeerPID(t, 0, false)
				s.a.humanProof.envOff = true
			},
			request: func(_ *testing.T, _ *setup, m, p, _ string) *http.Request { return headers(m, p) }, wantHint: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &setup{a: humanProofTestServer(t, tc.mode), approver: newTestDevice(t)}
			if tc.devices {
				approveTestDevice(t, s.a, s.approver)
			}
			tc.prepare(t, s)
			pending, err := s.a.trustedDevices.enroll("Laptop", newTestDevice(t).spki, 2)
			if err != nil {
				t.Fatal(err)
			}
			hint := listCanManage(t, s.a, tc.request(t, s, "GET", "/api/remote-devices", "list"))
			if hint != tc.wantHint {
				t.Fatalf("canManageDevices = %t, want %t", hint, tc.wantHint)
			}
			path := "/api/remote-devices/" + pending.ID + "/approve"
			code := approveRequest(t, s.a, pending.ID, func() *http.Request { return tc.request(t, s, "POST", path, "approve") })
			if approved := code == http.StatusOK; approved != hint || s.a.trustedDevices.isApproved(pending.ID) != hint {
				t.Fatalf("canManageDevices = %t but the approval answered %d", hint, code)
			}
		})
	}
}

// The hint is a read the screen polls: it writes no log lines, so only
// the acts themselves are logged.
func TestCanManageDevicesPollIsQuiet(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	approveTestDevice(t, a, newTestDevice(t))
	stubPeerPID(t, 0, false)
	var buf bytes.Buffer
	prev, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(prevFlags) })
	for _, forwarded := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/api/remote-devices", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if forwarded {
			r.Header.Set("X-Forwarded-Prefix", "/m/"+testMachine)
		}
		if listCanManage(t, a, r) {
			t.Fatal("headers under require can manage devices")
		}
	}
	// A traced browser under require, unsigned.
	stubTracedBrowser(t)
	r := httptest.NewRequest("GET", "/api/remote-devices", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if listCanManage(t, a, r) {
		t.Fatal("an unsigned traced browser under require can manage devices")
	}
	if buf.Len() != 0 {
		t.Fatalf("the device list logged:\n%s", buf.String())
	}
	// The act is still logged.
	r = httptest.NewRequest("POST", "/api/remote-devices/x/approve", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	a.identifyCaller(r)
	if buf.Len() == 0 {
		t.Fatal("identifyCaller stopped logging the acts it classifies")
	}
}
