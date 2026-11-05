package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func commitDeviceV3ForTest(t *testing.T, s *trustedDevices, d testDevice, nonce string) *trustedDevice {
	t.Helper()
	rec, _, err := s.commitDeviceV3("phone", d.spki, deviceCommitV3(deviceID(d.spki), nonce))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestDeviceCodeV3RevealAndApproval(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s, d := a.trustedDevices, newTestDevice(t)
	nonce := strings.Repeat("01", 32)
	first := commitDeviceV3ForTest(t, s, d, nonce)
	second := commitDeviceV3ForTest(t, s, d, nonce)
	if _, err := s.revealDeviceV3(first.ID, first.EnrolmentID, nonce); !errors.Is(err, errDeviceEnrolment) {
		t.Fatalf("stale reveal: %v", err)
	}
	view, err := s.revealDeviceV3(second.ID, second.EnrolmentID, nonce)
	if err != nil || view.Status != deviceStatusPending {
		t.Fatalf("current reveal: %+v, %v", view, err)
	}
	if view.Code != deviceCodeV3(d.spki, s.daemonSPKI(), second.EnrolmentID, nonce, second.DaemonNonce) {
		t.Fatal("comparison differs")
	}
	before := s.find(second.ID)
	if _, err := s.revealDeviceV3(second.ID, second.EnrolmentID, nonce); !errors.Is(err, errDeviceEnrolment) || s.find(second.ID) != before {
		t.Fatalf("second reveal: %v", err)
	}
	if _, err := s.approve(second.ID, first.EnrolmentID); !errors.Is(err, errDeviceEnrolment) {
		t.Fatalf("stale approve: %v", err)
	}
	if _, err := s.approve(second.ID); !errors.Is(err, errDeviceEnrolment) {
		t.Fatalf("unbound approve: %v", err)
	}
	approved, err := s.approve(second.ID, second.EnrolmentID)
	if err != nil || approved.Status != deviceStatusApproved {
		t.Fatalf("approve: %+v, %v", approved, err)
	}
	if approved.Commit != "" || approved.DeviceNonce != "" || approved.DaemonNonce != "" {
		t.Fatal("approval retained nonce state")
	}
	loaded := newTrustedDevices(filepath.Dir(s.path)).find(second.ID)
	if loaded == nil || loaded.Status != deviceStatusApproved || loaded.EnrolmentID != second.EnrolmentID || loaded.Commit != "" {
		t.Fatal("approval was not persisted")
	}
}

func TestDeviceCodeV3WrongRevealBurnsOnlyCurrentAttempt(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s, d := a.trustedDevices, newTestDevice(t)
	nonce := strings.Repeat("01", 32)
	first := commitDeviceV3ForTest(t, s, d, nonce)
	second := commitDeviceV3ForTest(t, s, d, strings.Repeat("02", 32))
	if _, err := s.revealDeviceV3(dID(d), first.EnrolmentID, "bad"); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	if s.find(second.ID).Status != deviceStatusCommitted {
		t.Fatal("stale request changed the current attempt")
	}
	if _, err := s.revealDeviceV3(second.ID, second.EnrolmentID, nonce); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	failed := s.find(second.ID)
	if failed.Status != deviceStatusFailed || failed.Commit != "" || failed.DaemonNonce != "" || failed.DeviceNonce != "" || failed.EnrolmentID != "" {
		t.Fatalf("failed record: %+v", failed)
	}
	if _, err := s.revealDeviceV3(second.ID, second.EnrolmentID, strings.Repeat("02", 32)); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal("burned attempt remained usable")
	}
	third := commitDeviceV3ForTest(t, s, d, nonce)
	if third.EnrolmentID == second.EnrolmentID || third.DaemonNonce == second.DaemonNonce {
		t.Fatal("restart reused an attempt")
	}
	if _, err := s.revealDeviceV3(third.ID, third.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	other := newTestDevice(t)
	otherRec := commitDeviceV3ForTest(t, s, other, strings.Repeat("03", 32))
	if _, err := s.revealDeviceV3(otherRec.ID, otherRec.EnrolmentID, nonce); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal("accepted another device's nonce")
	}
}

func dID(d testDevice) string { return deviceID(d.spki) }

func TestDeviceCodeV3ExpiryAndLegacyPending(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s := a.trustedDevices
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	nonce := strings.Repeat("01", 32)
	legacy, _ := s.enroll("legacy", newTestDevice(t).spki, 2)
	d := newTestDevice(t)
	committed := commitDeviceV3ForTest(t, s, d, nonce)
	now = now.Add(deviceCommitLifetime)
	if _, err := s.revealDeviceV3(committed.ID, committed.EnrolmentID, nonce); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	if s.find(committed.ID) != nil {
		t.Fatal("expired commitment retained")
	}
	revealed := commitDeviceV3ForTest(t, s, d, nonce)
	if _, err := s.revealDeviceV3(revealed.ID, revealed.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	now = now.Add(deviceRevealLifetime)
	if _, err := s.approve(revealed.ID, revealed.EnrolmentID); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	if s.find(revealed.ID) != nil {
		t.Fatal("expired reveal retained")
	}
	// The legacy (V2) request has waited longer than devicePendingLifetime by
	// now: gone too, like the V3 ones.
	if _, err := s.approve(legacy.ID); err == nil || s.find(legacy.ID) != nil {
		t.Fatalf("V2 pending past its lifetime approved or retained: %v", err)
	}
	late, _ := s.enroll("late", newTestDevice(t).spki, 2)
	now = now.Add(devicePendingLifetime - time.Second)
	s.mu.Lock()
	_ = s.pruneCommittedV3Locked()
	s.mu.Unlock()
	if s.find(late.ID) == nil {
		t.Fatal("V2 pending expired before its lifetime")
	}
	now = now.Add(time.Second)
	now = now.Add(devicePendingLifetime)
	s.mu.Lock()
	err := s.pruneCommittedV3Locked()
	s.mu.Unlock()
	if err != nil || s.find(late.ID) != nil {
		t.Fatalf("V2 pending past its lifetime retained: %v", err)
	}
}

func deviceV3Request(t *testing.T, handler http.HandlerFunc, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	r.Host = "127.0.0.1:8787"
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestDeviceCodeV3HTTPOrderingAndApproval(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	nonce := strings.Repeat("01", 32)
	id := deviceID(d.spki)
	enroll := deviceV3Request(t, a.handleEnrollRemoteDevice, remoteDevicesEnrollPath, map[string]any{"name": "phone", "publicKey": d.spki, "codeVersion": 3, "commit": deviceCommitV3(id, nonce)}, nil)
	if enroll.Code != 200 {
		t.Fatalf("enroll: %d %s", enroll.Code, enroll.Body.String())
	}
	var committed trustedDeviceView
	if err := json.Unmarshal(enroll.Body.Bytes(), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Status != deviceStatusCommitted || committed.DaemonNonce == "" || strings.Contains(enroll.Body.String(), `"code":`) {
		t.Fatal(enroll.Body.String())
	}
	loaded := newTrustedDevices(filepath.Dir(a.trustedDevices.path)).find(id)
	if loaded == nil || loaded.Commit == "" || loaded.DaemonNonce != committed.DaemonNonce {
		t.Fatal("nonce returned before persistence")
	}
	for _, rec := range []*httptest.ResponseRecorder{enroll} {
		if strings.Contains(rec.Body.String(), nonce) || strings.Contains(rec.Body.String(), `"commit":`) {
			t.Fatal("response exposed device nonce state")
		}
	}
	reveal := deviceV3Request(t, a.handleRevealRemoteDevice, remoteDevicesEnrollPath+"/reveal", map[string]string{"id": id, "enrolmentId": committed.EnrolmentID, "nonce": nonce}, nil)
	if reveal.Code != 200 {
		t.Fatalf("reveal: %d %s", reveal.Code, reveal.Body.String())
	}
	if !strings.Contains(reveal.Body.String(), `"code":`) || strings.Contains(reveal.Body.String(), `"daemonNonce":`) || strings.Contains(reveal.Body.String(), nonce) {
		t.Fatal(reveal.Body.String())
	}
	stubPeerPID(t, 0, false)
	approve := func(relay bool) *httptest.ResponseRecorder {
		return deviceV3Request(t, a.handleApproveRemoteDevice, "/api/remote-devices/"+id+"/approve", map[string]string{"enrolmentId": committed.EnrolmentID}, func(r *http.Request) {
			r.SetPathValue("id", id)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			if relay {
				*r = *r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
			}
		})
	}
	if w := approve(true); w.Code != 403 {
		t.Fatalf("relay approve: %d", w.Code)
	}
	if w := approve(false); w.Code != 200 {
		t.Fatalf("local approve: %d %s", w.Code, w.Body.String())
	}
	if !relayDeviceExempt(httptest.NewRequest("POST", remoteDevicesEnrollPath+"/reveal", nil)) {
		t.Fatal("reveal not reachable before approval")
	}
}

func TestDeviceCodeV3ConcurrentRefreshAndReveal(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	nonce := strings.Repeat("01", 32)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, _, err := a.trustedDevices.commitDeviceV3("phone", d.spki, deviceCommitV3(dID(d), nonce))
			if errors.Is(err, errDevicePendingKept) {
				return // another refresh already revealed it: it waits as it is
			}
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = a.trustedDevices.revealDeviceV3(rec.ID, rec.EnrolmentID, nonce)
		}()
	}
	wg.Wait()
	if len(a.trustedDevices.record.Devices) != 1 {
		t.Fatal("concurrent refresh duplicated a device")
	}
	if _, err := os.ReadFile(a.trustedDevices.path); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceCodeV3RevealAndApprovePersistenceFailure(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s, d := a.trustedDevices, newTestDevice(t)
	nonce := strings.Repeat("01", 32)
	rec := commitDeviceV3ForTest(t, s, d, nonce)
	path := s.path
	s.path = filepath.Join(t.TempDir(), "missing", "record.json")
	if _, err := s.revealDeviceV3(rec.ID, rec.EnrolmentID, nonce); err == nil {
		t.Fatal("reveal returned success without persistence")
	}
	if s.find(rec.ID).Status != deviceStatusCommitted || s.find(rec.ID).Code != "" {
		t.Fatal("failed write changed reveal state")
	}
	s.path = path
	if _, err := s.revealDeviceV3(rec.ID, rec.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(t.TempDir(), "missing", "record.json")
	if _, err := s.approve(rec.ID, rec.EnrolmentID); err == nil {
		t.Fatal("approve returned success without persistence")
	}
	if s.find(rec.ID).Status != deviceStatusPending {
		t.Fatal("failed write approved the device")
	}
}
