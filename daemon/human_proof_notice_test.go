package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// approveFrom approves pending id with a request signed by approver at this
// computer.
func approveFrom(t *testing.T, a *apiServer, approver testDevice, id, nonce string) int {
	t.Helper()
	return approveRequest(t, a, id, func() *http.Request {
		return approver.localRequest(t, a, "POST", "/api/remote-devices/"+id+"/approve", "", time.Now(), nonce)
	})
}

// removeFrom removes id with a request signed by remover at this computer.
func removeFrom(t *testing.T, a *apiServer, remover testDevice, id, nonce string) int {
	t.Helper()
	r, _ := throughProof(a, remover.localRequest(t, a, "DELETE", "/api/remote-devices/"+id, "", time.Now(), nonce))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	a.handleRemoveRemoteDevice(w, r)
	return w.Code
}

// enrolTestDevice adds a pending device for d.
func enrolTestDevice(t *testing.T, a *apiServer, name string, d testDevice) string {
	t.Helper()
	pending, err := a.trustedDevices.enroll(name, d.spki, 2)
	if err != nil {
		t.Fatal(err)
	}
	return pending.ID
}

// bootstrapFirst approves first as the computer's first device.
func bootstrapFirst(t *testing.T, a *apiServer, first testDevice) {
	t.Helper()
	if w := bootstrap(t, a, bootstrapRequest(t, a, first, "boot")); w.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d %s", w.Code, w.Body.String())
	}
}

// stubTracedBrowser makes the next local requests come from a browser
// traced at this computer that no program drives.
func stubTracedBrowser(t *testing.T) {
	t.Helper()
	prev, prevAuto := peerIsBrowser, peerIsAutomationBrowser
	t.Cleanup(func() { peerIsBrowser, peerIsAutomationBrowser = prev, prevAuto })
	peerIsBrowser = func(int) bool { return true }
	peerIsAutomationBrowser = func(int) bool { return false }
	stubTracedOutsider(t)
}

func tracedRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

// X (flagged) approves Y: Y inherits X's chain and can't vouch for X, and
// X, no longer the only device ever approved, can't vouch for itself.
func TestDeviceApprovedByAFlaggedDeviceCantClearIt(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	x, y := newTestDevice(t), newTestDevice(t)
	bootstrapFirst(t, a, x)
	yID := enrolTestDevice(t, a, "Y", y)
	if code := approveFrom(t, a, x, yID, "a1"); code != http.StatusOK {
		t.Fatalf("X approving Y = %d", code)
	}
	if got := a.trustedDevices.find(yID); got.ApprovedBy != deviceID(x.spki) || len(got.Lineage) != 1 || got.Lineage[0] != deviceID(x.spki) {
		t.Fatalf("Y's chain = by %q lineage %v", got.ApprovedBy, got.Lineage)
	}
	if code := ackAs(t, a, y, "a2"); code != http.StatusForbidden {
		t.Fatalf("Y clearing X's notice = %d", code)
	}
	if code := ackAs(t, a, x, "a3"); code != http.StatusForbidden {
		t.Fatalf("X clearing its own notice after approving Y = %d", code)
	}
	if n := a.humanProofView("").Notices; len(n) != 1 || n[0].ID != deviceID(x.spki) {
		t.Fatalf("notices = %+v", n)
	}

	// Y's own approvals carry the whole chain, even once X is gone.
	z := newTestDevice(t)
	zID := enrolTestDevice(t, a, "Z", z)
	if code := approveFrom(t, a, y, zID, "a4"); code != http.StatusOK {
		t.Fatalf("Y approving Z = %d", code)
	}
	if code := removeFrom(t, a, y, deviceID(x.spki), "a5"); code != http.StatusNoContent {
		t.Fatalf("Y removing X = %d", code)
	}
	if code := ackAs(t, a, z, "a6"); code != http.StatusForbidden {
		t.Fatalf("Z (approved through X) clearing X's notice = %d", code)
	}

	// A person traced at this computer clears them; Y and Z are then clean.
	stubTracedBrowser(t)
	r, _ := throughProof(a, tracedRequest("POST", "/api/human-proof/acknowledge"))
	w := httptest.NewRecorder()
	a.handleAcknowledgeHumanProof(w, r)
	if w.Code != http.StatusOK || len(a.humanProofView("").Notices) != 0 {
		t.Fatalf("traced person acknowledging = %d %s", w.Code, w.Body.String())
	}
}

// The first device can't remove the others and then clear its own notice;
// the removal it made is announced too.
func TestRemovingTheOthersThenClearingOwnNoticeIsRefused(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	x, phone := newTestDevice(t), newTestDevice(t)
	bootstrapFirst(t, a, x)
	phoneID := enrolTestDevice(t, a, "Phone", phone)
	if code := approveFrom(t, a, x, phoneID, "b1"); code != http.StatusOK {
		t.Fatalf("approving the phone = %d", code)
	}
	if code := removeFrom(t, a, x, phoneID, "b2"); code != http.StatusNoContent {
		t.Fatalf("X removing the phone = %d", code)
	}
	n := a.humanProofView("").Notices
	if len(n) != 2 || n[1].ID != deviceID(x.spki) || n[1].Via != "removed" || n[1].Removed != "Phone" {
		t.Fatalf("notices after the removal = %+v", n)
	}
	if code := ackAs(t, a, x, "b3"); code != http.StatusForbidden {
		t.Fatalf("X, now alone, clearing its own notice = %d", code)
	}
	if got := a.humanProofView("").Notices; len(got) != 2 {
		t.Fatalf("notices = %+v", got)
	}
}

// A device approved by one Y descends from leaves a notice when it removes
// another device, even with no notice of its own.
func TestTaintedDeviceRemovalIsAnnounced(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	phone := newTestDevice(t)
	approveTestDevice(t, a, phone) // approved earlier, with no notice
	a.humanProof.envOff = true
	x := newTestDevice(t)
	xID := enrolTestDevice(t, a, "Recovered", x)
	if code := approveRequest(t, a, xID, func() *http.Request { return tracedRequest("POST", "/api/remote-devices/"+xID+"/approve") }); code != http.StatusOK {
		t.Fatalf("env-recovery approval = %d", code)
	}
	a.humanProof.envOff = false
	y := newTestDevice(t)
	yID := enrolTestDevice(t, a, "Y", y)
	if code := approveFrom(t, a, x, yID, "c1"); code != http.StatusOK {
		t.Fatalf("X approving Y = %d", code)
	}
	if code := removeFrom(t, a, y, deviceID(phone.spki), "c2"); code != http.StatusNoContent {
		t.Fatalf("Y removing the phone = %d", code)
	}
	n := a.humanProofView("").Notices
	if len(n) != 2 || n[1].ID != yID || n[1].Via != "removed" || n[1].Removed != "phone" {
		t.Fatalf("notices = %+v", n)
	}
}

// A clean device approved after the notice, from a browser traced at this
// computer, clears it; so does the phone on its private channel.
func TestCleanDevicesClearNotices(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	x := newTestDevice(t)
	bootstrapFirst(t, a, x)
	later := newTestDevice(t)
	laterID := enrolTestDevice(t, a, "Later", later)
	func() {
		stubTracedBrowser(t)
		if code := approveRequest(t, a, laterID, func() *http.Request { return tracedRequest("POST", "/api/remote-devices/"+laterID+"/approve") }); code != http.StatusOK {
			t.Fatalf("traced approval = %d", code)
		}
	}()
	stubPeerPID(t, 0, false)
	if got := a.trustedDevices.find(laterID); got.ApprovedVia != "traced" || len(got.Lineage) != 0 {
		t.Fatalf("later device = via %q lineage %v", got.ApprovedVia, got.Lineage)
	}
	if code := ackAs(t, a, later, "d1"); code != http.StatusOK || len(a.humanProofView("").Notices) != 0 {
		t.Fatalf("clean later device acknowledging = %d", code)
	}

	// The phone, approved with no notice and on its private channel.
	b := humanProofTestServer(t, humanProofReport)
	phone := newTestDevice(t)
	approveTestDevice(t, b, phone)
	if err := b.humanProof.noteApprovalNotice(&trustedDevice{ID: "dev_x", Name: "x"}, "env-recovery", time.Now()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/human-proof/acknowledge", nil)
	r = r.WithContext(withPrivateRelayDevice(context.WithValue(r.Context(), relayTransportKey{}, true), deviceID(phone.spki)))
	w := httptest.NewRecorder()
	b.handleAcknowledgeHumanProof(w, r)
	if w.Code != http.StatusOK || len(b.humanProofView("").Notices) != 0 {
		t.Fatalf("phone on its private channel = %d", w.Code)
	}
}

// The approval and its notice never come apart: a failed approval leaves
// no notice behind.
func TestFailedApprovalLeavesNoNotice(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	a.humanProof.envOff = true
	d := newTestDevice(t)
	id := enrolTestDevice(t, a, "V3 without its enrolment", d)
	a.trustedDevices.mu.Lock()
	a.trustedDevices.find(id).CodeVersion = 3
	a.trustedDevices.mu.Unlock()
	if code := approveRequest(t, a, id, func() *http.Request { return tracedRequest("POST", "/api/remote-devices/"+id+"/approve") }); code == http.StatusOK {
		t.Fatal("approved a V3 device without its enrolment id")
	}
	if n := a.humanProofView("").Notices; len(n) != 0 {
		t.Fatalf("notices = %+v", n)
	}
}

// Only an unsigned, traced browser is the person in its own right: a signed
// request from a traced browser is judged as its device, and a browser run
// by automation is no proof.
func TestTracedPersonAcknowledgementIsUnsignedAndNotAutomation(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	x, y := newTestDevice(t), newTestDevice(t)
	bootstrapFirst(t, a, x)
	yID := enrolTestDevice(t, a, "Y", y)
	if code := approveFrom(t, a, x, yID, "e1"); code != http.StatusOK {
		t.Fatalf("X approving Y = %d", code)
	}
	stubTracedBrowser(t)
	if code := ackAs(t, a, y, "e2"); code != http.StatusForbidden {
		t.Fatalf("a traced browser signed by tainted Y = %d", code)
	}
	peerIsAutomationBrowser = func(int) bool { return true }
	r, _ := throughProof(a, tracedRequest("POST", "/api/human-proof/acknowledge"))
	w := httptest.NewRecorder()
	a.handleAcknowledgeHumanProof(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an automation browser = %d", w.Code)
	}
	if n := a.humanProofView("").Notices; len(n) != 1 {
		t.Fatalf("notices = %+v", n)
	}
}

// A V3 save keeps the approval history the bootstrap exception relies on.
func TestV3SaveKeepsApprovalHistory(t *testing.T) {
	dir := t.TempDir()
	s := newTrustedDevices(dir)
	now := time.Now().UTC().Truncate(time.Second)
	s.mu.Lock()
	s.record.ApprovedEver, s.record.LastApprovedRemoval = 3, &now
	err := s.saveDevicesV3Locked(append([]*trustedDevice(nil), s.record.Devices...))
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	again := newTrustedDevices(dir)
	if again.record.ApprovedEver != 3 || again.record.LastApprovedRemoval == nil || !again.record.LastApprovedRemoval.Equal(now) {
		t.Fatalf("after reload: ever %d, last removal %v", again.record.ApprovedEver, again.record.LastApprovedRemoval)
	}
}

// An acknowledgement waits for an approval under way, so it never sees the
// new notice without the approval that goes with it.
func TestAcknowledgementWaitsForApprovalsUnderWay(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	x := newTestDevice(t)
	bootstrapFirst(t, a, x)
	a.noticeMu.Lock() // an approval under way
	done := make(chan int, 1)
	go func() { done <- ackAs(t, a, x, "f1") }()
	select {
	case code := <-done:
		t.Fatalf("acknowledged during an approval: %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	a.noticeMu.Unlock()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("acknowledge after the approval = %d", code)
	}
}

// If saving an approval fails after the device was approved in memory, its
// notice stays.
func TestNoticeStaysWhenTheApprovalSaveFails(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	phone := newTestDevice(t)
	approveTestDevice(t, a, phone)
	a.humanProof.envOff = true
	d := newTestDevice(t)
	id := enrolTestDevice(t, a, "Recovered", d)
	path := a.trustedDevices.path
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil { // the save's rename now fails
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Clean(path)) })
	if code := approveRequest(t, a, id, func() *http.Request { return tracedRequest("POST", "/api/remote-devices/"+id+"/approve") }); code != http.StatusInternalServerError {
		t.Fatalf("approval with a failing save = %d", code)
	}
	if !a.trustedDevices.isApproved(id) {
		t.Skip("the store rolled the approval back; nothing to announce")
	}
	if n := a.humanProofView("").Notices; len(n) != 1 || n[0].ID != id {
		t.Fatalf("notices = %+v", n)
	}
}

// An approval by a device removed since it signed is refused, so it can't
// pass on a chain that no longer names that device's own approvers.
func TestApprovalByARemovedDeviceIsRefused(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	y, z := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, y)
	zID := enrolTestDevice(t, a, "Z", z)
	if err := a.trustedDevices.remove(deviceID(y.spki)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.trustedDevices.approveWith(zID, "", false, "device:"+deviceID(y.spki)); !errors.Is(err, errApproverGone) {
		t.Fatalf("approval by a removed device = %v", err)
	}
	if a.trustedDevices.isApproved(zID) {
		t.Fatal("Z approved")
	}
}

// A flagged device's removal is refused when its notice can't be saved,
// so a removal never goes unannounced.
func TestRemovalWaitsForItsNotice(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	x, phone := newTestDevice(t), newTestDevice(t)
	bootstrapFirst(t, a, x)
	phoneID := enrolTestDevice(t, a, "Phone", phone)
	if code := approveFrom(t, a, x, phoneID, "g1"); code != http.StatusOK {
		t.Fatalf("approving the phone = %d", code)
	}
	path := a.humanProof.path
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil { // the notice's save now fails
		t.Fatal(err)
	}
	if code := removeFrom(t, a, x, phoneID, "g2"); code != http.StatusInternalServerError {
		t.Fatalf("removal with a failing notice save = %d", code)
	}
	if !a.trustedDevices.isApproved(phoneID) {
		t.Fatal("removed without its notice")
	}
}
