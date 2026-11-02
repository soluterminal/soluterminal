package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startDeviceVerificationForTest(t *testing.T, s *trustedDevices, d testDevice, nonce string) trustedDeviceView {
	t.Helper()
	v, err := s.startVerificationV3(deviceID(d.spki), deviceCommitV3(deviceID(d.spki), nonce))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDeviceCodeV3VerificationPreservesApproval(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s, d := a.trustedDevices, newTestDevice(t)
	legacy, err := s.enroll("phone", d.spki, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.approve(legacy.ID); err != nil {
		t.Fatal(err)
	}
	id, nonce := deviceID(d.spki), strings.Repeat("01", 32)
	original := *s.find(id)
	if s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
		t.Fatal("V2 approval counted as V3 verification")
	}
	first := startDeviceVerificationForTest(t, s, d, nonce)
	if _, err := s.finishVerificationV3(id, first.EnrolmentID, false); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal("confirmed before reveal")
	}
	second := startDeviceVerificationForTest(t, s, d, nonce)
	if _, err := s.revealDeviceV3(id, first.EnrolmentID, "bad"); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	v, err := s.revealDeviceV3(id, second.EnrolmentID, nonce)
	if err != nil || v.Status != deviceStatusApproved || v.Verify == nil || v.Verify.State != "revealed" {
		t.Fatalf("verify reveal: %+v %v", v, err)
	}
	if v.Verify.Code != deviceCodeV3(d.spki, s.daemonSPKI(), second.EnrolmentID, nonce, second.DaemonNonce) {
		t.Fatal("verify code differs")
	}
	for _, reject := range []bool{false, true} {
		if _, err := s.finishVerificationV3(id, first.EnrolmentID, reject); !errors.Is(err, errDeviceEnrolment) {
			t.Fatal("accepted stale completion")
		}
	}
	if _, err := s.finishVerificationV3(id, second.EnrolmentID, false); err != nil {
		t.Fatal(err)
	}
	rec := s.find(id)
	if rec.Verify != nil || rec.Status != original.Status || !rec.ApprovedAt.Equal(*original.ApprovedAt) || rec.CodeVersion != original.CodeVersion {
		t.Fatal("verify changed approval")
	}
	if rec.V3Verified == nil || !s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
		t.Fatal("comparison was not recorded")
	}
	reloaded := newTrustedDevices(filepath.Dir(s.path)).find(id)
	if reloaded == nil || reloaded.Verify != nil || reloaded.V3Verified == nil || *reloaded.V3Verified != *rec.V3Verified {
		t.Fatal("verification proof was not persisted without attempt state")
	}
	proof := *rec.V3Verified
	changed := proof
	changed.DeviceSPKIHash = strings.Repeat("0", 64)
	rec.V3Verified = &changed
	if s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
		t.Fatal("accepted a different device fingerprint")
	}
	changed = proof
	changed.DaemonSPKIHash = strings.Repeat("0", 64)
	rec.V3Verified = &changed
	if s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
		t.Fatal("accepted a different computer fingerprint")
	}
	rec.V3Verified = &proof
	if s.v3VerifiedForChannel(id, newTestDevice(t).spki, s.daemonSPKI()) || s.v3VerifiedForChannel(id, d.spki, newTestDevice(t).spki) {
		t.Fatal("verification survived a channel key change")
	}
	third := startDeviceVerificationForTest(t, s, d, nonce)
	if _, err := s.revealDeviceV3(id, third.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishVerificationV3(id, third.EnrolmentID, true); err != nil {
		t.Fatal(err)
	}
	if s.find(id).Verify != nil || !s.isApproved(id) {
		t.Fatal("reject changed access")
	}
	if s.find(id).V3Verified != nil || s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
		t.Fatal("reject retained the previous verification")
	}
	if err := s.remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishVerificationV3(id, third.EnrolmentID, false); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal("confirm restored access")
	}
	if s.find(id) != nil || s.v3VerifiedForChannel(id, d.spki, s.daemonSPKI()) {
		t.Fatal("revoked device retained verification")
	}
}

func TestDeviceCodeV3VerificationFailureAndExpiry(t *testing.T) {
	a := trustedDevicesTestServer(t)
	s, d := a.trustedDevices, newTestDevice(t)
	approveTestDevice(t, a, d)
	id, nonce := deviceID(d.spki), strings.Repeat("01", 32)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	first := startDeviceVerificationForTest(t, s, d, nonce)
	if _, err := s.revealDeviceV3(id, first.EnrolmentID, strings.Repeat("02", 32)); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	if got := s.find(id).Verify; got == nil || got.State != deviceStatusFailed || got.EnrolmentID != "" || got.Commit != "" || got.DaemonNonce != "" {
		t.Fatalf("failed verify: %+v", got)
	}
	second := startDeviceVerificationForTest(t, s, d, nonce)
	now = now.Add(deviceCommitLifetime)
	if _, err := s.revealDeviceV3(id, second.EnrolmentID, nonce); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	if s.find(id).Verify != nil || !s.isApproved(id) {
		t.Fatal("commit expiry changed approval")
	}
	third := startDeviceVerificationForTest(t, s, d, nonce)
	if _, err := s.revealDeviceV3(id, third.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.revealDeviceV3(id, third.EnrolmentID, nonce); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal("second verify reveal accepted")
	}
	now = now.Add(deviceRevealLifetime)
	if _, err := s.finishVerificationV3(id, third.EnrolmentID, false); !errors.Is(err, errDeviceEnrolment) {
		t.Fatal(err)
	}
	if s.find(id).Verify != nil || s.find(id).V3Verified != nil || !s.isApproved(id) {
		t.Fatal("reveal expiry changed approval or verified")
	}
}

func TestDeviceCodeV3VerificationGates(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	id, nonce := deviceID(d.spki), strings.Repeat("01", 32)
	start := func(channel string) int {
		return deviceV3Request(t, a.handleVerifyRemoteDevice, "/api/remote-devices/"+id+"/verify", map[string]any{"codeVersion": 3, "commit": deviceCommitV3(id, nonce)}, func(r *http.Request) {
			r.SetPathValue("id", id)
			*r = *r.WithContext(withPrivateRelayDevice(r.Context(), channel))
		}).Code
	}
	if start("") != 403 || start("dev_other") != 403 || a.trustedDevices.find(id).Verify != nil {
		t.Fatal("accepted a non-owning channel")
	}
	if got := start(id); got != 200 {
		t.Fatalf("owning channel: %d", got)
	}
	attempt := a.trustedDevices.find(id).Verify.EnrolmentID
	if _, err := a.trustedDevices.revealDeviceV3(id, attempt, nonce); err != nil {
		t.Fatal(err)
	}
	stubPeerPID(t, 0, false)
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []http.HandlerFunc{a.handleConfirmDeviceVerification, a.handleRejectDeviceVerification} {
		for _, caller := range []string{"relay", "agent", "stale"} {
			identifyCallerHook = nil
			if caller == "agent" {
				identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "p-v3-agent"} }
			}
			rawID := attempt
			if caller == "stale" {
				rawID = "enr_AAECAwQFBgcICQoLDA0ODw"
			}
			w := deviceV3Request(t, handler, "/api/remote-devices/"+id+"/verify/confirm", map[string]string{"enrolmentId": rawID}, func(r *http.Request) {
				r.SetPathValue("id", id)
				if caller != "agent" {
					r.Header.Set("Sec-Fetch-Site", "same-origin")
				}
				if caller == "relay" {
					*r = *r.WithContext(withPrivateRelayDevice(r.Context(), id))
				}
				if caller == "relay" {
					r.Header.Set("X-Forwarded-Prefix", "/m/"+testMachine)
				}
			})
			identifyCallerHook = nil
			want := 403
			if caller == "stale" {
				want = 409
			}
			if w.Code != want {
				t.Fatalf("%s completion: %d %s", caller, w.Code, w.Body.String())
			}
		}
	}
	if a.trustedDevices.find(id).Verify.EnrolmentID != attempt {
		t.Fatal("refused completion changed attempt")
	}
	if err := a.trustedDevices.remove(id); err != nil {
		t.Fatal(err)
	}
	if got := start(id); got != 404 || a.trustedDevices.find(id) != nil {
		t.Fatalf("revoked start: %d", got)
	}
}

func TestDeviceCodeV3VerificationViews(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	nonce := strings.Repeat("01", 32)
	v := startDeviceVerificationForTest(t, a.trustedDevices, d, nonce)
	view := viewOfDevice(a.trustedDevices.find(v.ID))
	if view.Verify == nil || view.Verify.Code != "" {
		t.Fatal("code visible before verify reveal")
	}
	if _, err := a.trustedDevices.revealDeviceV3(v.ID, v.EnrolmentID, nonce); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(viewOfDevice(a.trustedDevices.find(v.ID)))
	if err != nil || strings.Contains(string(b), nonce) || strings.Contains(string(b), `"daemonNonce":`) || strings.Contains(string(b), `"commit":`) {
		t.Fatalf("view: %s %v", b, err)
	}
}

func TestDeviceCodeV3VerificationThroughPrivateChannel(t *testing.T) {
	a, d, server := privateRelayFixture(t)
	inner := http.NewServeMux()
	inner.HandleFunc("POST /api/remote-devices/{id}/verify", a.handleVerifyRemoteDevice)
	inner.HandleFunc("POST /api/remote-devices/enroll/reveal", a.handleRevealRemoteDevice)
	inner.HandleFunc("POST /api/remote-devices/{id}/verify/confirm", a.handleConfirmDeviceVerification)
	a.privateRelayInner = markRelayTransport(inner)
	mux, _, client := openPrivateClient(t, a, d, server)
	t.Cleanup(mux.shutdown)
	id, nonce := deviceID(d.spki), strings.Repeat("01", 32)
	post := func(path string, body any, out any, want int) {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post("http://private"+path, "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			message, _ := io.ReadAll(resp.Body)
			t.Fatalf("private request: %d %s", resp.StatusCode, message)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var committed, revealed trustedDeviceView
	post("/api/remote-devices/"+id+"/verify", map[string]any{"codeVersion": 3, "commit": deviceCommitV3(id, nonce)}, &committed, 200)
	post(remoteDevicesEnrollPath+"/reveal", map[string]string{"id": id, "enrolmentId": committed.EnrolmentID, "nonce": nonce}, &revealed, 200)
	if revealed.Status != deviceStatusApproved || revealed.Verify == nil || revealed.Verify.State != "revealed" {
		t.Fatal("private verification did not reveal")
	}
	post("/api/remote-devices/"+id+"/verify/confirm", map[string]string{"enrolmentId": committed.EnrolmentID}, nil, 403)
	post("/api/remote-devices/dev_other/verify", map[string]any{"codeVersion": 3, "commit": deviceCommitV3(id, nonce)}, nil, 403)
}
