package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const alertsBody = `{"client":"Desktop alerts","ttlSec":600}`

func alertsTestServer(t *testing.T, mode string) *apiServer {
	t.Helper()
	a := humanProofTestServer(t, mode)
	a.uiBus = newUIBus()
	return a
}

// approveAlertsDevice enrols d as an alerts-only device (V3: commit, reveal,
// approve) and returns its id.
func approveAlertsDevice(t *testing.T, a *apiServer, d testDevice) string {
	t.Helper()
	s := a.trustedDevices
	nonce := strings.Repeat("03", 32)
	rec, _, err := s.commitDeviceV3Scoped("Desktop window", d.spki, deviceCommitV3(deviceID(d.spki), nonce), false, deviceScopeAlerts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.revealDeviceV3(rec.ID, rec.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	approved, err := s.approve(rec.ID, rec.EnrolmentID)
	if err != nil || approved.Status != deviceStatusApprovedAlerts {
		t.Fatalf("approve alerts device: %+v, %v", approved, err)
	}
	return rec.ID
}

// nativeSigned is a request signed at this computer by d with no browser
// headers, as a desktop window sends it, after the local proof middleware.
func (d testDevice) nativeSigned(t *testing.T, a *apiServer, method, target, body string) *http.Request {
	t.Helper()
	r, _ := throughProof(a, d.signedRequestFor(t, a.trustedDevices.localTarget(), method, target, body, time.Now(), newNonce(t)))
	return r
}

var testNonces atomic.Int64

func newNonce(t *testing.T) string {
	return t.Name() + "-" + time.Now().Format("150405.000000000") + "-" + string(rune('a'+testNonces.Add(1)%26))
}

func leaseRelayRequest(method, target, body, device string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx := context.WithValue(r.Context(), relayTransportKey{}, true)
	if device != "" {
		ctx = context.WithValue(ctx, relayVerifiedDeviceKey{}, device)
	}
	return r.WithContext(ctx)
}

func leaseCall(a *apiServer, r *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	switch r.Method {
	case http.MethodPut:
		a.handlePutNotificationsHandler(w, r)
	case http.MethodDelete:
		a.handleDeleteNotificationsHandler(w, r)
	default:
		a.handleGetNotificationsHandler(w, r)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// nextLeaseEvent returns the next notifications-handler event on ch.
func nextLeaseEvent(t *testing.T, ch chan uiEvent) uiEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Type == alertsLeaseEventType {
				if e.Lease == nil {
					t.Fatalf("lease event without its payload: %+v", e)
				}
				return e
			}
		case <-timeout:
			t.Fatal("no notifications-handler event")
		}
	}
}

func noLeaseEvent(t *testing.T, ch chan uiEvent) {
	t.Helper()
	for {
		select {
		case e := <-ch:
			if e.Type == alertsLeaseEventType {
				t.Fatalf("unexpected lease event %+v", e.Lease)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

// Criteria 1, 7, 8a/8b: panes (even with Agent permissions on and a stolen
// key of either kind), workers, unverified programs, browser headers alone
// and an unsigned relay request can't take the lease.
func TestAlertsLeaseRefusesAgentsAndUnprovenCallers(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	approveAlertsDevice(t, a, window)
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	refused := func(name string, r *http.Request, caller callerKind) {
		t.Helper()
		w, out := leaseCall(a, r)
		if w.Code != http.StatusForbidden || out["error"] != "device_required" || out["caller"] != string(caller) {
			t.Fatalf("%s: PUT = %d %v, want 403 device_required for caller %s", name, w.Code, out, caller)
		}
	}

	cmd := startSleeper(t)
	a.mgr.panes["p-agent"] = &Pane{ID: "p-agent", Pid: cmd.Process.Pid}
	stubPeerPID(t, cmd.Process.Pid, true)
	signed, _ := throughProof(a, person.localRequest(t, a, "PUT", "/api/notifications/handler", alertsBody, time.Now(), "pane-1"))
	refused("pane signing with the person's key", signed, callerPane)
	refused("pane signing with the alerts key", window.nativeSigned(t, a, "PUT", "/api/notifications/handler", alertsBody), callerPane)
	refused("pane, unsigned", httptest.NewRequest("PUT", "/api/notifications/handler", strings.NewReader(alertsBody)), callerPane)

	stubPeerPID(t, os.Getpid(), true)
	refused("worker", window.nativeSigned(t, a, "PUT", "/api/notifications/handler", alertsBody), callerWorker)

	stubPeerPID(t, 0, false)
	refused("unverified", httptest.NewRequest("PUT", "/api/notifications/handler", strings.NewReader(alertsBody)), callerUnverified)
	headers := httptest.NewRequest("PUT", "/api/notifications/handler", strings.NewReader(alertsBody))
	headers.Header.Set("Sec-Fetch-Site", "same-origin")
	refused("browser headers alone", headers, callerHuman)
	w, _ := leaseCall(a, leaseRelayRequest("PUT", "/api/notifications/handler", alertsBody, ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("unsigned relay PUT = %d", w.Code)
	}
	if a.alertsLease().skipsFCM() {
		t.Fatal("a refused caller left a lease")
	}
}

// Criterion 8c: the alerts device signing here, the person's device signing
// here or over the relay, and a device on its private channel all take it.
func TestAlertsLeaseAcceptsApprovedDevices(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	windowID := approveAlertsDevice(t, a, window)
	personID := deviceID(person.spki)
	stubPeerPID(t, 0, false)
	for _, tc := range []struct {
		name   string
		device string
		req    func(method string) *http.Request
	}{
		{"alerts device signed here", windowID, func(m string) *http.Request {
			return window.nativeSigned(t, a, m, "/api/notifications/handler", map[string]string{"PUT": alertsBody}[m])
		}},
		{"person's device signed here", personID, func(m string) *http.Request {
			r, _ := throughProof(a, person.localRequest(t, a, m, "/api/notifications/handler", map[string]string{"PUT": alertsBody}[m], time.Now(), newNonce(t)))
			return r
		}},
		{"person's device signed over the relay", personID, func(m string) *http.Request {
			return leaseRelayRequest(m, "/api/notifications/handler", map[string]string{"PUT": alertsBody}[m], personID)
		}},
		{"person's device on its private channel", personID, func(m string) *http.Request {
			r := httptest.NewRequest(m, "/api/notifications/handler", strings.NewReader(map[string]string{"PUT": alertsBody}[m]))
			return r.WithContext(withPrivateRelayDevice(context.WithValue(r.Context(), relayTransportKey{}, true), personID))
		}},
	} {
		w, out := leaseCall(a, tc.req("PUT"))
		if w.Code != http.StatusOK || out["deviceId"] != tc.device || out["active"] != true || out["client"] != "Desktop alerts" || out["ttlSec"] != float64(600) {
			t.Fatalf("%s: PUT = %d %v", tc.name, w.Code, out)
		}
		if !a.alertsLease().skipsFCM() {
			t.Fatalf("%s: lease not live", tc.name)
		}
		w, out = leaseCall(a, tc.req("DELETE"))
		if w.Code != http.StatusOK || out["released"] != true || a.alertsLease().skipsFCM() {
			t.Fatalf("%s: DELETE = %d %v", tc.name, w.Code, out)
		}
	}
}

// Criteria 2 and the events: the holder renews quietly; another approved
// device takes over, announced; the TTL is clamped to 60–600 s.
func TestAlertsLeaseRenewTakeoverAndEvents(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	windowID := approveAlertsDevice(t, a, window)
	personID := deviceID(person.spki)
	stubPeerPID(t, 0, false)
	ch := a.uiBus.subscribe()
	defer a.uiBus.unsubscribe(ch)

	w, out := leaseCall(a, window.nativeSigned(t, a, "PUT", "/api/notifications/handler", alertsBody))
	if w.Code != http.StatusOK || out["takenOver"] != false {
		t.Fatalf("take = %d %v", w.Code, out)
	}
	e := nextLeaseEvent(t, ch)
	if e.Lease.Event != alertsLeaseAcquired || e.Lease.DeviceID != windowID || e.Lease.DeviceName != "Desktop window" || e.Lease.ExpiresAtMs != int64(out["expiresAtMs"].(float64)) {
		t.Fatalf("acquired event = %+v", e.Lease)
	}

	for _, tc := range []struct{ ask, want int }{{5, 60}, {0, 600}, {9999, 600}, {120, 120}} {
		body := `{"client":"Desktop alerts","ttlSec":` + strings.TrimSpace(jsonInt(tc.ask)) + `}`
		w, out = leaseCall(a, window.nativeSigned(t, a, "PUT", "/api/notifications/handler", body))
		if w.Code != http.StatusOK || out["ttlSec"] != float64(tc.want) || out["takenOver"] != false {
			t.Fatalf("renew ttl %d = %d %v, want %d", tc.ask, w.Code, out, tc.want)
		}
	}
	noLeaseEvent(t, ch) // renewals aren't announced

	w, out = leaseCall(a, leaseRelayRequest("PUT", "/api/notifications/handler", alertsBody, personID))
	if w.Code != http.StatusOK || out["takenOver"] != true || out["deviceId"] != personID {
		t.Fatalf("takeover = %d %v", w.Code, out)
	}
	e = nextLeaseEvent(t, ch)
	if e.Lease.Event != alertsLeaseTakenOver || e.Lease.DeviceID != personID || e.Lease.DeviceName != "phone" || e.Lease.PreviousDeviceID != windowID || e.Lease.ExpiresAtMs == 0 {
		t.Fatalf("taken_over event = %+v", e.Lease)
	}
	// The former holder now holds nothing: its DELETE on exit is a no-op.
	w, out = leaseCall(a, window.nativeSigned(t, a, "DELETE", "/api/notifications/handler", ""))
	if w.Code != http.StatusOK || out["released"] != false || !a.alertsLease().skipsFCM() {
		t.Fatalf("former holder's DELETE = %d %v", w.Code, out)
	}

	// The events carry ids, names and times only.
	raw, _ := json.Marshal(e)
	for _, secret := range []string{person.spki, window.spki, "publicKey", "signature"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("lease event carries %q: %s", secret, raw)
		}
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Criterion 3 and the revoke events: the holder releases (twice is fine);
// another device can't release it, nor a pane or an unsigned relay request;
// the person at this computer takes alerts back without a signature.
func TestAlertsLeaseRelease(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	windowID := approveAlertsDevice(t, a, window)
	stubPeerPID(t, 0, false)
	ch := a.uiBus.subscribe()
	defer a.uiBus.unsubscribe(ch)
	take := func() {
		t.Helper()
		if w, out := leaseCall(a, window.nativeSigned(t, a, "PUT", "/api/notifications/handler", alertsBody)); w.Code != http.StatusOK {
			t.Fatalf("take = %d %v", w.Code, out)
		}
		if e := nextLeaseEvent(t, ch); e.Lease.Event != alertsLeaseAcquired {
			t.Fatalf("event = %+v", e.Lease)
		}
	}

	take()
	for i, want := range []bool{true, false} {
		w, out := leaseCall(a, window.nativeSigned(t, a, "DELETE", "/api/notifications/handler", ""))
		if w.Code != http.StatusOK || out["released"] != want {
			t.Fatalf("holder DELETE %d = %d %v", i, w.Code, out)
		}
	}
	if e := nextLeaseEvent(t, ch); e.Lease.Event != alertsLeaseRevoked || e.Lease.Cause != alertsLeaseCauseReleased || e.Lease.DeviceID != windowID || e.Lease.DeviceName != "Desktop window" {
		t.Fatalf("release event = %+v", e.Lease)
	}

	take()
	if w, out := leaseCall(a, leaseRelayRequest("DELETE", "/api/notifications/handler", "", deviceID(person.spki))); w.Code != http.StatusOK || out["released"] != false {
		t.Fatalf("another device's DELETE over the relay = %d %v", w.Code, out)
	}
	if w, _ := leaseCall(a, leaseRelayRequest("DELETE", "/api/notifications/handler", "", "")); w.Code != http.StatusForbidden {
		t.Fatalf("unsigned relay DELETE = %d", w.Code)
	}
	if w, _ := leaseCall(a, httptest.NewRequest("DELETE", "/api/notifications/handler", nil)); w.Code != http.StatusForbidden {
		t.Fatalf("unverified DELETE = %d", w.Code)
	}
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cmd := startSleeper(t)
	a.mgr.panes["p-agent"] = &Pane{ID: "p-agent", Pid: cmd.Process.Pid}
	stubPeerPID(t, cmd.Process.Pid, true)
	if w, _ := leaseCall(a, httptest.NewRequest("DELETE", "/api/notifications/handler", nil)); w.Code != http.StatusForbidden {
		t.Fatalf("pane DELETE with Agent permissions on = %d", w.Code)
	}
	if !a.alertsLease().skipsFCM() {
		t.Fatal("a refused DELETE released the lease")
	}

	// A local full-access device that doesn't hold it (a former holder
	// exiting) and the person without ?takeBack release nothing.
	stubPeerPID(t, 0, false)
	formerHolder, _ := throughProof(a, person.localRequest(t, a, "DELETE", "/api/notifications/handler", "", time.Now(), newNonce(t)))
	if w, out := leaseCall(a, formerHolder); w.Code != http.StatusOK || out["released"] != false || !a.alertsLease().skipsFCM() {
		t.Fatalf("local full-access non-holder DELETE = %d %v", w.Code, out)
	}
	plain := httptest.NewRequest("DELETE", "/api/notifications/handler", nil)
	plain.Header.Set("Sec-Fetch-Site", "same-origin")
	if w, out := leaseCall(a, plain); w.Code != http.StatusOK || out["released"] != false || !a.alertsLease().skipsFCM() {
		t.Fatalf("person's DELETE without takeBack = %d %v", w.Code, out)
	}
	// Not over the relay, even from an approved device.
	if w, _ := leaseCall(a, leaseRelayRequest("DELETE", "/api/notifications/handler?takeBack=1", "", deviceID(person.spki))); w.Code != http.StatusOK || !a.alertsLease().skipsFCM() {
		t.Fatalf("takeBack over the relay released the lease (%d)", w.Code)
	}
	// "Take alerts back" at this computer: the person's signed browser, as
	// the Settings button sends it.
	takeBack, _ := throughProof(a, person.localRequest(t, a, "DELETE", "/api/notifications/handler?takeBack=1", "", time.Now(), newNonce(t)))
	if w, out := leaseCall(a, takeBack); w.Code != http.StatusOK || out["released"] != true || a.alertsLease().skipsFCM() {
		t.Fatalf("take alerts back = %d %v", w.Code, out)
	}
	if e := nextLeaseEvent(t, ch); e.Lease.Event != alertsLeaseRevoked || e.Lease.Cause != alertsLeaseCauseTakenBack || e.Lease.DeviceID != windowID {
		t.Fatalf("take-back event = %+v", e.Lease)
	}
}

// Criteria 4 and 8e: removing the holder device ends the lease at once and
// FCM resumes; the end is announced.
func TestAlertsLeaseEndsWhenItsDeviceIsRemoved(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	windowID := approveAlertsDevice(t, a, window)
	stubPeerPID(t, 0, false)
	ch := a.uiBus.subscribe()
	defer a.uiBus.unsubscribe(ch)
	if w, out := leaseCall(a, window.nativeSigned(t, a, "PUT", "/api/notifications/handler", alertsBody)); w.Code != http.StatusOK {
		t.Fatalf("take = %d %v", w.Code, out)
	}
	nextLeaseEvent(t, ch)
	if err := a.trustedDevices.remove(windowID); err != nil {
		t.Fatal(err)
	}
	if a.alertsLease().skipsFCM() {
		t.Fatal("FCM still skipped after the holder device was removed")
	}
	if e := nextLeaseEvent(t, ch); e.Lease.Event != alertsLeaseRevoked || e.Lease.Cause != alertsLeaseCauseUnapproved || e.Lease.DeviceID != windowID || e.Lease.DeviceName != "Desktop window" {
		t.Fatalf("revoke event = %+v", e.Lease)
	}
	if w, out := leaseCall(a, httptest.NewRequest("GET", "/api/notifications/handler", nil)); w.Code != http.StatusOK || out["active"] != false {
		t.Fatalf("GET after removal = %d %v", w.Code, out)
	}
}

// Criteria 4 and 8f: expiry and a restart resume FCM; expiry is announced
// once; any error on the push path counts as no lease.
func TestAlertsLeaseExpiryRestartAndErrorsSendFCM(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	window := newTestDevice(t)
	windowID := approveAlertsDevice(t, a, window)
	s := newAlertsLeaseStore(a.trustedDevices, a.uiBus)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	s.now = func() time.Time { return time.Unix(0, now.Load()) }
	fire := make(chan time.Time)
	s.after = func(time.Duration) <-chan time.Time { return fire }
	ch := a.uiBus.subscribe()
	defer a.uiBus.unsubscribe(ch)

	if _, ok := s.take(windowID, "Desktop alerts", clampAlertsLeaseTTL(60)); !ok || !s.skipsFCM() {
		t.Fatal("take")
	}
	nextLeaseEvent(t, ch)
	now.Add(int64(61 * time.Second))
	if s.skipsFCM() {
		t.Fatal("an expired lease still skips FCM")
	}
	// The watcher may already have seen the clock pass expiry on its first
	// look and ended the lease itself, without waiting on its timer.
	go func() {
		select {
		case fire <- time.Now():
		case <-time.After(5 * time.Second):
		}
	}()
	e := nextLeaseEvent(t, ch)
	if e.Lease.Event != alertsLeaseExpired || e.Lease.DeviceID != windowID || e.Lease.DeviceName != "Desktop window" || e.Lease.ExpiresAtMs == 0 {
		t.Fatalf("expiry event = %+v", e.Lease)
	}
	if _, ok := s.current(); ok {
		t.Fatal("expired lease still current")
	}
	noLeaseEvent(t, ch) // announced once

	// A restart starts with no lease.
	if _, ok := s.take(windowID, "Desktop alerts", alertsLeaseMaxTTL); !ok || !s.skipsFCM() {
		t.Fatal("retake")
	}
	if newAlertsLeaseStore(a.trustedDevices, a.uiBus).skipsFCM() || alertsTestServer(t, humanProofReport).alertsLease().skipsFCM() {
		t.Fatal("a new daemon starts with a lease")
	}
	// A failing approval check counts as no lease.
	broken := newAlertsLeaseStore(nil, nil)
	broken.lease = &alertsLease{holder: windowID, expires: time.Now().Add(time.Hour)}
	broken.approved = func(string) bool { panic("store unavailable") }
	if broken.skipsFCM() {
		t.Fatal("an error on the push path skipped FCM")
	}
	var nilStore *alertsLeaseStore
	if nilStore.skipsFCM() {
		t.Fatal("no store skipped FCM")
	}
}

// Criteria 5 and 8g: only Code alerts carry skipFcm while a lease is live;
// device, enrolment and security notices, schedules, agents and workflows
// are always sent to FCM; a lapsed lease sends Code alerts to FCM too.
func TestPushSkipsFCMOnlyForCodeAlertsWhileLeased(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sent":1}`))
	}))
	defer srv.Close()
	p := NewPusher(srv.URL, "machine-token", nil, nil, nil)
	var leased atomic.Bool
	leased.Store(true)
	p.alertsHandled = leased.Load
	p.private = func() bool { return true } // minimisePushBody is unchanged and composes
	send := func(kind string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"paneId": "p1", "state": kind, "title": "t"})
		if !p.deliver(body, "p1", kind) {
			t.Fatalf("deliver %s failed", kind)
		}
		mu.Lock()
		defer mu.Unlock()
		return bodies[len(bodies)-1]
	}
	for kind := range codeAlertKinds {
		if got := send(kind); got["skipFcm"] != true || got["title"] != "SoluTerminal" {
			t.Fatalf("Code alert %s while leased = %v", kind, got)
		}
	}
	for _, kind := range []string{
		"device-enrolment", "remote-device.pending", "device-approved", "device-removed", "first-device", "env-recovery",
		"security-notice", "auth-notice", "schedule-error", "schedule-fired", "agent-error", "agent-report",
		"workflow-needs-you", "provider-billing", "clear",
	} {
		if got := send(kind); got["skipFcm"] != nil {
			t.Fatalf("%s while leased carries skipFcm: %v", kind, got)
		}
	}
	leased.Store(false)
	for kind := range codeAlertKinds {
		if got := send(kind); got["skipFcm"] != nil {
			t.Fatalf("Code alert %s without a lease = %v", kind, got)
		}
	}
	if string(withSkipFCM([]byte("not json"))) != "not json" {
		t.Fatal("an unreadable body was changed")
	}
}

// Criterion 8h: the client label is display text, cleaned and capped.
func TestAlertsClientLabelIsSanitised(t *testing.T) {
	for in, want := range map[string]string{
		"Desktop alerts":                 "Desktop alerts",
		"  spaced \t\n out  ":            "spaced out",
		"bell\x07\x1b[31mred":            "bell[31mred",
		"rtl\u202eevil\u200b":            "rtlevil",
		"bad\xffutf8":                    "badutf8",
		strings.Repeat("x", 100):         strings.Repeat("x", alertsLeaseClientMax),
		strings.Repeat("é", 70):          strings.Repeat("é", alertsLeaseClientMax),
		strings.Repeat("a", 63) + " b c": strings.Repeat("a", 63),
		"":                               "",
	} {
		if got := sanitizeAlertsClient(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	a := alertsTestServer(t, humanProofReport)
	window := newTestDevice(t)
	approveAlertsDevice(t, a, window)
	stubPeerPID(t, 0, false)
	w, out := leaseCall(a, window.nativeSigned(t, a, "PUT", "/api/notifications/handler", `{"client":"\u202eAlerts\u0000`+strings.Repeat("z", 80)+`"}`))
	if w.Code != http.StatusOK || len([]rune(out["client"].(string))) != alertsLeaseClientMax || strings.ContainsAny(out["client"].(string), "\u202e\x00") {
		t.Fatalf("PUT label = %d %v", w.Code, out)
	}
	if w, out := leaseCall(a, httptest.NewRequest("GET", "/api/notifications/handler", nil)); w.Code != http.StatusOK || out["deviceId"] != nil || out["deviceName"] != "Desktop window" || out["expiresAtMs"] == nil {
		t.Fatalf("GET = %d %v (holder name and expiry only)", w.Code, out)
	}
	if w, _ := leaseCall(a, leaseRelayRequest("GET", "/api/notifications/handler", "", "")); w.Code != http.StatusForbidden {
		t.Fatalf("unsigned relay GET = %d", w.Code)
	}
}

// Ruling A1: an alerts-only device's signature is never the person. It
// changes nothing about who the caller is, and approve, remove,
// acknowledge, settings, prompt answers, the relay gate and the private
// channel all refuse it.
func TestAlertsDeviceIsNeverThePerson(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	windowID := approveAlertsDevice(t, a, window)
	personID := deviceID(person.spki)
	stubPeerPID(t, 0, false)

	if id := a.identifyCaller(window.nativeSigned(t, a, "POST", "/api/x", "")); id.Kind != callerUnverified || id.Device != "" || id.ScopedDevice != windowID {
		t.Fatalf("alerts-signed caller = %+v", id)
	}
	if a.approvedDeviceCount() != 1 || a.trustedDevices.isApproved(windowID) {
		t.Fatal("the alerts device counts as one of the person's")
	}

	pending, err := a.trustedDevices.enroll("Laptop", newTestDevice(t).spki, 2)
	if err != nil {
		t.Fatal(err)
	}
	approve := window.nativeSigned(t, a, "POST", "/api/remote-devices/"+pending.ID+"/approve", "")
	approve.SetPathValue("id", pending.ID)
	w := httptest.NewRecorder()
	a.handleApproveRemoteDevice(w, approve)
	if w.Code != http.StatusForbidden || a.trustedDevices.find(pending.ID).Status != deviceStatusPending {
		t.Fatalf("approve with the alerts key = %d", w.Code)
	}
	remove := window.nativeSigned(t, a, "DELETE", "/api/remote-devices/"+personID, "")
	remove.SetPathValue("id", personID)
	w = httptest.NewRecorder()
	a.handleRemoveRemoteDevice(w, remove)
	if w.Code != http.StatusForbidden || !a.trustedDevices.isApproved(personID) {
		t.Fatalf("remove with the alerts key = %d", w.Code)
	}
	if err := a.humanProof.noteApprovalNotice(&trustedDevice{ID: "dev_x", Name: "x"}, "first-device", time.Now()); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.handleAcknowledgeHumanProof(w, window.nativeSigned(t, a, "POST", "/api/human-proof/acknowledge", "{}"))
	if w.Code == http.StatusOK || len(a.humanProofView("").Notices) == 0 {
		t.Fatalf("acknowledge with the alerts key = %d", w.Code)
	}
	for _, tc := range []struct {
		name, path, body string
		h                http.HandlerFunc
	}{
		{"agent permissions", "/api/settings/agent-authority", `{"panes":true}`, a.handlePutAgentAuthority},
		{"device check", "/api/human-proof", `{"mode":"off"}`, a.handlePutHumanProof},
		{"remote access", "/api/remote-devices/policy", `{"policy":"any"}`, a.handlePutRemoteAccessPolicy},
	} {
		w := httptest.NewRecorder()
		tc.h(w, window.nativeSigned(t, a, "PUT", tc.path, tc.body))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s with the alerts key = %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if a.agentAuthority.active(time.Now()) || a.humanProofMode() != humanProofReport {
		t.Fatal("a setting changed")
	}
	target := choicePromptPane(t, "pane_target")
	a.mgr.panes[target.ID] = target
	if a.promptInputGate(window.nativeSigned(t, a, "GET", "/api/panes/pane_target/io", ""))(target) {
		t.Fatal("the alerts key answered a permission prompt")
	}

	// Over the relay its signature is refused under approved devices only,
	// and is no device at all otherwise.
	if err := a.trustedDevices.setPolicy(remoteAccessApproved); err != nil {
		t.Fatal(err)
	}
	reached := ""
	gate := a.relayDeviceGate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { reached = "device=" + relayVerifiedDevice(r) }))
	relayed := func() *http.Request {
		r := window.signedRequest(t, "PUT", "/api/notifications/handler", alertsBody, time.Now(), newNonce(t))
		return r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
	}
	rw := httptest.NewRecorder()
	gate.ServeHTTP(rw, relayed())
	if rw.Code != http.StatusUnauthorized || reached != "" {
		t.Fatalf("relay gate under approved = %d %q", rw.Code, reached)
	}
	if err := a.trustedDevices.setPolicy(remoteAccessAny); err != nil {
		t.Fatal(err)
	}
	gate.ServeHTTP(httptest.NewRecorder(), relayed())
	if reached != "device=" {
		t.Fatalf("relay gate under any: %q", reached)
	}
	if _, err := a.trustedDevices.verifyRequest(window.signedRequest(t, "GET", "/api/x", "", time.Now(), newNonce(t))); !errors.Is(err, errDeviceScoped) {
		t.Fatalf("verifyRequest = %v", err)
	}

	// The last of the person's devices is kept under require even with an
	// alerts device approved, and removing the alerts device is never
	// refused as the last.
	if _, err := a.trustedDevices.removeDevice(personID, true); !errors.Is(err, errLastApprovedDevice) {
		t.Fatalf("remove the person's last device: %v", err)
	}
	if _, err := a.trustedDevices.removeDevice(windowID, true); err != nil {
		t.Fatalf("remove the alerts device: %v", err)
	}
}

func TestAlertsDeviceCannotOpenThePrivateChannel(t *testing.T) {
	a, _, srv := privateRelayFixture(t)
	a.uiBus = newUIBus()
	window := newTestDevice(t)
	windowID := approveAlertsDevice(t, a, window)
	ws := dialPrivateRelay(t, srv)
	if _, err := privateClientHandshake(ws, windowID, testMachine+"/wsl", window.signer(t), a.trustedDevices.daemonSPKI(), time.Now()); err == nil {
		t.Fatal("an alerts-only device opened the private channel")
	}
}

// Ruling A1 enrolment: V3 only, at this computer only, one scope; the scope
// shows on the record and stays as enrolled.
func TestAlertsDeviceEnrolment(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	d := newTestDevice(t)
	nonce := strings.Repeat("05", 32)
	enrol := func(r *http.Request) (*httptest.ResponseRecorder, map[string]any) {
		w := httptest.NewRecorder()
		a.handleEnrollRemoteDevice(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out
	}
	body := func(scope string, version int) string {
		b, _ := json.Marshal(map[string]any{"name": "Desktop window", "publicKey": d.spki, "codeVersion": version, "commit": deviceCommitV3(deviceID(d.spki), nonce), "scope": scope})
		return string(b)
	}
	for _, tc := range []struct {
		name, scope string
		version     int
		relay       bool
		code        int
		err         string
	}{
		{"V2", deviceScopeAlerts, 2, false, http.StatusBadRequest, "scope_needs_v3"},
		{"V1", deviceScopeAlerts, 0, false, http.StatusBadRequest, "scope_needs_v3"},
		{"unknown scope", "admin", 3, false, http.StatusBadRequest, "unsupported_scope"},
		{"over the relay", deviceScopeAlerts, 3, true, http.StatusForbidden, "scope_local_only"},
		{"through /m/", deviceScopeAlerts, 3, false, http.StatusForbidden, "scope_local_only"},
	} {
		r := httptest.NewRequest("POST", "/api/remote-devices/enroll", strings.NewReader(body(tc.scope, tc.version)))
		if tc.relay {
			r = r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
		}
		if tc.name == "through /m/" {
			r.Header.Set("X-Forwarded-Prefix", "/m/mach_test01")
		}
		if w, out := enrol(r); w.Code != tc.code || out["error"] != tc.err {
			t.Fatalf("%s: enrol = %d %v", tc.name, w.Code, out)
		}
	}
	if a.trustedDevices.find(deviceID(d.spki)) != nil {
		t.Fatal("a refused enrolment left a record")
	}
	w, out := enrol(httptest.NewRequest("POST", "/api/remote-devices/enroll", strings.NewReader(body(deviceScopeAlerts, 3))))
	if w.Code != http.StatusOK || out["scope"] != deviceScopeAlerts || out["status"] != deviceStatusCommitted {
		t.Fatalf("local V3 alerts enrolment = %d %v", w.Code, out)
	}
	rec := a.trustedDevices.find(deviceID(d.spki))
	if _, err := a.trustedDevices.revealDeviceV3(rec.ID, rec.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := a.trustedDevices.approve(rec.ID, rec.EnrolmentID); err != nil {
		t.Fatal(err)
	}
	a.trustedDevices.mu.Lock()
	v := viewOfDevice(a.trustedDevices.find(rec.ID))
	a.trustedDevices.mu.Unlock()
	if v.Status != deviceStatusApproved || v.Scope != deviceScopeAlerts || a.trustedDevices.find(rec.ID).Status != deviceStatusApprovedAlerts {
		t.Fatalf("approved alerts device view = %+v", v)
	}
	// Same key, same scope: asking again without it (V3 or V2) is refused and
	// leaves the record; asking again with it returns the approved record.
	for _, version := range []int{3, 2} {
		again, _ := json.Marshal(map[string]any{"name": "x", "publicKey": d.spki, "codeVersion": version, "commit": deviceCommitV3(deviceID(d.spki), strings.Repeat("06", 32))})
		if w, out := enrol(httptest.NewRequest("POST", "/api/remote-devices/enroll", strings.NewReader(string(again)))); w.Code != http.StatusConflict || out["error"] != "scope_mismatch" {
			t.Fatalf("re-enrol V%d without the scope = %d %v", version, w.Code, out)
		}
	}
	if w, out := enrol(httptest.NewRequest("POST", "/api/remote-devices/enroll", strings.NewReader(body(deviceScopeAlerts, 3)))); w.Code != http.StatusOK || out["scope"] != deviceScopeAlerts || out["status"] != deviceStatusApproved {
		t.Fatalf("re-enrol with the scope = %d %v", w.Code, out)
	}
	if got := a.trustedDevices.find(rec.ID); got.Status != deviceStatusApprovedAlerts || got.Name != "Desktop window" {
		t.Fatalf("re-enrolment changed the alerts device: %+v", got)
	}
	// Nor does a full device become alerts-only, or bootstrap take a scoped key.
	full := newTestDevice(t)
	approveTestDevice(t, a, full)
	if _, _, err := a.trustedDevices.commitDeviceV3Scoped("x", full.spki, deviceCommitV3(deviceID(full.spki), nonce), false, deviceScopeAlerts); !errors.Is(err, errDeviceScopeMismatch) {
		t.Fatalf("full device asking as alerts-only: %v", err)
	}
	pendingScoped := newTestDevice(t)
	if _, _, err := a.trustedDevices.commitDeviceV3Scoped("x", pendingScoped.spki, deviceCommitV3(deviceID(pendingScoped.spki), nonce), false, deviceScopeAlerts); err != nil {
		t.Fatal(err)
	}
	if _, err := a.trustedDevices.bootstrapLocal("x", pendingScoped.spki, deviceSurfaceLocal); !errors.Is(err, errDeviceScopeMismatch) {
		t.Fatalf("bootstrap of a scoped key: %v", err)
	}
	// A V1/V2 approval can't carry a scope.
	legacy, _ := a.trustedDevices.enroll("Old", newTestDevice(t).spki, 2)
	a.trustedDevices.mu.Lock()
	legacy.Scope = deviceScopeAlerts
	a.trustedDevices.mu.Unlock()
	if _, err := a.trustedDevices.approve(legacy.ID); !errors.Is(err, errDeviceEnrolment) {
		t.Fatalf("V2 approval with a scope: %v", err)
	}
}

// The one "this computer" test: alerts-only enrolment, the human-proof view
// and the first-device bootstrap refuse exactly the same requests, and a
// Windows app reaching the daemon through WSL's loopback forwarding (an
// untraceable 127.0.0.1 peer) is at this computer for all of them.
func TestAlertsEnrolmentUsesTheHumanProofComputerCheck(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	for _, tc := range []struct {
		name  string
		pid   int
		known bool
		relay bool
		mark  string
		local bool
	}{
		{"traced local program", 1, true, false, "", true},
		{"Windows app via WSL forwarding", 0, false, false, "", true},
		{"proxy pid (WSL init / desktop router)", 1, true, false, "", true},
		{"relay tunnel", 0, false, true, "", false},
		{"/m/ forwarded", 0, false, false, "/m/mach_test01", false},
		{"relay and /m/", 0, false, true, "/m/mach_test01", false},
	} {
		stubPeerPID(t, tc.pid, tc.known)
		mk := func(method, path, body string) *http.Request {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.RemoteAddr = "127.0.0.1:50123"
			if tc.relay {
				r = r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
			}
			if tc.mark != "" {
				r.Header.Set("X-Forwarded-Prefix", tc.mark)
			}
			return r
		}
		if got := atThisComputer(mk("GET", "/", "")); got != tc.local {
			t.Fatalf("%s: atThisComputer = %t, want %t", tc.name, got, tc.local)
		}
		w := httptest.NewRecorder()
		a.handleGetHumanProof(w, mk("GET", "/api/human-proof", ""))
		viewLocal := w.Code == http.StatusOK
		w = httptest.NewRecorder()
		a.handleBootstrapHumanProof(w, mk("POST", "/api/human-proof/bootstrap", "{}"))
		bootstrapLocal := !(w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "at the computer itself"))
		d := newTestDevice(t)
		enrolBody, _ := json.Marshal(map[string]any{"name": "w", "publicKey": d.spki, "codeVersion": 3, "commit": deviceCommitV3(deviceID(d.spki), strings.Repeat("07", 32)), "scope": deviceScopeAlerts})
		w = httptest.NewRecorder()
		a.handleEnrollRemoteDevice(w, mk("POST", "/api/remote-devices/enroll", string(enrolBody)))
		enrolLocal := !strings.Contains(w.Body.String(), "scope_local_only")
		surfaceLocal := deviceSurface(mk("GET", "/", "")) == deviceSurfaceLocal
		if viewLocal != tc.local || bootstrapLocal != tc.local || enrolLocal != tc.local || surfaceLocal != tc.local {
			t.Fatalf("%s: view %t, bootstrap %t, alerts enrolment %t, surface %t; want all %t", tc.name, viewLocal, bootstrapLocal, enrolLocal, surfaceLocal, tc.local)
		}
	}
}

// @secure 1b: an alerts-only device is never approved as the first device or
// on browser headers alone, even with no device approved, and trying neither
// uses up nor changes the first-device bootstrap. An approved device, a
// traced browser or env recovery may approve it.
func TestAlertsDeviceNeedsDeviceProofToBeApproved(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	pendingAlerts := func() (*trustedDevice, testDevice) {
		t.Helper()
		d := newTestDevice(t)
		nonce := strings.Repeat("08", 32)
		rec, _, err := a.trustedDevices.commitDeviceV3Scoped("Desktop window", d.spki, deviceCommitV3(deviceID(d.spki), nonce), false, deviceScopeAlerts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.trustedDevices.revealDeviceV3(rec.ID, rec.EnrolmentID, nonce); err != nil {
			t.Fatal(err)
		}
		return a.trustedDevices.find(rec.ID), d
	}
	approve := func(rec *trustedDevice, r *http.Request) int {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"enrolmentId": rec.EnrolmentID})
		if r == nil {
			r = httptest.NewRequest("POST", "/api/remote-devices/"+rec.ID+"/approve", strings.NewReader(string(b)))
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		r.SetPathValue("id", rec.ID)
		w := httptest.NewRecorder()
		a.handleApproveRemoteDevice(w, r)
		return w.Code
	}
	rec, _ := pendingAlerts()
	if code := approve(rec, nil); code != http.StatusForbidden {
		t.Fatalf("headers-only approval with no device approved = %d", code)
	}
	if a.trustedDevices.find(rec.ID).Status != deviceStatusPending || len(a.humanProofView("").Notices) != 0 {
		t.Fatal("a refused alerts approval changed the record or left a notice")
	}
	if _, err := a.trustedDevices.approveWith(rec.ID, rec.EnrolmentID, true, "first-device"); !errors.Is(err, errDeviceScopeNeedsProof) {
		t.Fatalf("store first-device approval of an alerts device: %v", err)
	}
	// The bootstrap is still there for the person's first device.
	person := newTestDevice(t)
	if _, err := a.trustedDevices.bootstrapLocal("Chrome", person.spki, deviceSurfaceLocal); err != nil {
		t.Fatalf("first-device bootstrap after the refusals: %v", err)
	}
	// From the person's approved device it is approved, as an alerts device.
	b, _ := json.Marshal(map[string]string{"enrolmentId": rec.EnrolmentID})
	signed, _ := throughProof(a, person.localRequest(t, a, "POST", "/api/remote-devices/"+rec.ID+"/approve", string(b), time.Now(), newNonce(t)))
	if code := approve(rec, signed); code != http.StatusOK || a.trustedDevices.find(rec.ID).Status != deviceStatusApprovedAlerts {
		t.Fatalf("approval from an approved device = %d", code)
	}
	if a.approvedDeviceCount() != 1 {
		t.Fatal("the alerts device counts as the person's")
	}
	// Under env recovery, browser headers at the computer may approve it.
	a.humanProof.envOff = true
	t.Cleanup(func() { a.humanProof.envOff = false })
	rec2, _ := pendingAlerts()
	if code := approve(rec2, nil); code != http.StatusOK || a.trustedDevices.find(rec2.ID).ApprovedVia != "env-recovery" {
		t.Fatalf("env-recovery approval = %d via %q", code, a.trustedDevices.find(rec2.ID).ApprovedVia)
	}
}

// @secure 1a: main's device-file loader (bcb715c65 newTrustedDevices,
// copied verbatim below) reads a file with an approved-alerts entry without
// an error, keeps every device, and the entry is not approved under main's
// checks (isApproved, verifySigned, approvedDeviceCount and the private
// channel all compare with "approved"; pruning drops only expired committed
// and pending requests).
func TestOlderDaemonTreatsAlertsDeviceAsNotApproved(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	windowID := approveAlertsDevice(t, a, window)
	raw, err := os.ReadFile(a.trustedDevices.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"status": "approved-alerts"`) && !strings.Contains(string(raw), `"status":"approved-alerts"`) {
		t.Fatalf("file lacks the alerts status: %s", raw)
	}
	mainLoad := func(b []byte) (trustedDevicesRecord, bool) {
		// bcb715c65 daemon/trusted_devices.go newTrustedDevices, the file part.
		var rec trustedDevicesRecord
		if json.Unmarshal(b, &rec) != nil {
			return rec, false
		}
		if rec.Policy != remoteAccessApproved {
			rec.Policy = remoteAccessAny
		}
		kept := rec.Devices[:0]
		for _, d := range rec.Devices {
			if key, err := parseDevicePublicKey(d.PublicKey); err == nil {
				d.key = key
				kept = append(kept, d)
			}
		}
		rec.Devices = kept
		return rec, true
	}
	rec, ok := mainLoad(raw)
	if !ok || len(rec.Devices) != 2 {
		t.Fatalf("main's loader: ok %t, %d devices", ok, len(rec.Devices))
	}
	approvedOnMain := 0
	for _, d := range rec.Devices {
		if d.Status == "approved" { // main's only approved status
			approvedOnMain++
			if d.ID != deviceID(person.spki) {
				t.Fatalf("main would approve %s", d.ID)
			}
		}
	}
	if approvedOnMain != 1 {
		t.Fatalf("main approves %d devices, want the person's one", approvedOnMain)
	}
	// Main's pruning keeps it (the same function is unchanged here).
	old := newTrustedDevices(t.TempDir())
	old.record = rec
	if err := old.pruneCommittedV3Locked(); err != nil || len(old.record.Devices) != 2 {
		t.Fatalf("pruning: %v, %d devices", err, len(old.record.Devices))
	}
	// A record an older daemon saved again (without the scope) is still an
	// alerts device here.
	var stripped trustedDevicesRecord
	_ = json.Unmarshal(raw, &stripped)
	for _, d := range stripped.Devices {
		d.Scope = ""
	}
	b, _ := json.Marshal(stripped)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/"+trustedDevicesFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	back := newTrustedDevices(dir)
	if d := back.find(windowID); d == nil || d.scope() != deviceScopeAlerts || back.isApproved(windowID) {
		t.Fatalf("after a round trip through an older daemon: %+v", d)
	}
	if v := viewOfDevice(back.find(windowID)); v.Scope != deviceScopeAlerts || v.Status != deviceStatusApproved {
		t.Fatalf("view after round trip = %+v", v)
	}
}

// @secure 3: the request to the cloud carries skipFcm only for the four Code
// alert kinds, and only while the lease is live and its holder is still
// approved at send time.
func TestPushSkipFCMFollowsTheLiveLease(t *testing.T) {
	var mu sync.Mutex
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = nil
		_ = json.Unmarshal(raw, &last)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sent":1}`))
	}))
	defer srv.Close()
	a := alertsTestServer(t, humanProofReport)
	window := newTestDevice(t)
	windowID := approveAlertsDevice(t, a, window)
	p := NewPusher(srv.URL, "machine-token", nil, nil, nil)
	p.alertsHandled = a.alertsLease().skipsFCM
	skipped := func(kind string) bool {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"paneId": "p1", "state": kind})
		if !p.deliver(body, "p1", kind) {
			t.Fatalf("deliver %s", kind)
		}
		mu.Lock()
		defer mu.Unlock()
		return last["skipFcm"] == true
	}
	all := []string{"choice-prompt", "awaiting-input", "reply-complete", "user-ask", "clear", "schedule-error", "agent-error", "device-approved", "security-notice"}
	check := func(stage string, live bool) {
		t.Helper()
		for _, kind := range all {
			if got, want := skipped(kind), live && codeAlertKinds[kind]; got != want {
				t.Fatalf("%s: %s skipFcm = %t, want %t", stage, kind, got, want)
			}
		}
	}
	check("no lease", false)
	s := a.alertsLease()
	if _, ok := s.take(windowID, "Desktop alerts", alertsLeaseMaxTTL); !ok {
		t.Fatal("take")
	}
	check("live lease", true)
	if err := a.trustedDevices.remove(windowID); err != nil {
		t.Fatal(err)
	}
	check("holder removed", false)
	window2 := newTestDevice(t)
	id2 := approveAlertsDevice(t, a, window2)
	if _, ok := s.take(id2, "Desktop alerts", alertsLeaseMinTTL); !ok {
		t.Fatal("retake")
	}
	check("new live lease", true)
	s.mu.Lock()
	s.now = func() time.Time { return time.Now().Add(alertsLeaseMinTTL + time.Second) }
	s.mu.Unlock()
	check("expired lease", false)
}

// Review: a removal landing between the watcher's approval check and its
// wait still wakes it, so the revoke is announced at once, not at expiry.
// take checks approval once; the watcher's first check is the second call,
// and the device is removed right after that check passes. Nothing else
// wakes the watcher here (no timer, no kick).
func TestAlertsLeaseWatcherSeesRemovalDuringItsCheck(t *testing.T) {
	var mu sync.Mutex
	changed := make(chan struct{})
	approved, calls := true, 0
	s := newAlertsLeaseStore(nil, nil)
	s.after = func(time.Duration) <-chan time.Time { return nil }
	s.changes = func() <-chan struct{} { mu.Lock(); defer mu.Unlock(); return changed }
	s.approved = func(string) bool {
		mu.Lock()
		defer mu.Unlock()
		calls++
		ok := approved
		if calls == 2 {
			approved = false
			close(changed)
			changed = make(chan struct{})
		}
		return ok
	}
	events := make(chan alertsLeaseEvent, 4)
	s.publish = func(e uiEvent) { events <- *e.Lease }
	if _, ok := s.take("dev_w", "Desktop alerts", alertsLeaseMaxTTL); !ok {
		t.Fatal("take")
	}
	if e := <-events; e.Event != alertsLeaseAcquired {
		t.Fatalf("event = %+v", e)
	}
	select {
	case e := <-events:
		if e.Event != alertsLeaseRevoked || e.Cause != alertsLeaseCauseUnapproved {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a removal during the watcher's check went unnoticed")
	}
}

// @secure gate (d): an approval's notice is withdrawn only when the device
// didn't become approved. An alerts-only device approved in memory after a
// failed save keeps it, as the person's device does.
func TestApprovalNoticeKeptForAnAlertsDeviceApprovedInMemory(t *testing.T) {
	a := alertsTestServer(t, humanProofReport)
	person, window := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, person)
	windowID := approveAlertsDevice(t, a, window)
	pending, err := a.trustedDevices.enroll("Laptop", newTestDevice(t).spki, 2)
	if err != nil {
		t.Fatal(err)
	}
	saveFailed := errors.New("could not save")
	for _, tc := range []struct {
		name string
		id   string
		err  error
		drop bool
	}{
		{"alerts device approved in memory, save failed", windowID, saveFailed, false},
		{"person's device approved in memory, save failed", deviceID(person.spki), saveFailed, false},
		{"still pending, save failed", pending.ID, saveFailed, true},
		{"approval not made", windowID, errDeviceScopeNeedsProof, true},
		{"approved and saved", windowID, nil, false},
	} {
		if got := a.approvalNoticeDropped(tc.id, tc.err); got != tc.drop {
			t.Fatalf("%s: dropped = %t, want %t", tc.name, got, tc.drop)
		}
	}
}
