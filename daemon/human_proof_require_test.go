package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A browser is traced by its executable's name, which any program can take.
// Under require such a caller is the person only with an approved device's
// signature: unsigned, it can't lower the mode, approve or remove a device,
// acknowledge notices or take any person-only act. Off and report are
// unchanged. Checked with a stand-in browser and with a real renamed binary.
func TestRequireNeedsSignatureFromNameMatchedBrowser(t *testing.T) {
	checkRequireNeedsSignature(t, "stand-in", stubTracedBrowser)
}

func TestRequireNeedsSignatureFromRenamedBinary(t *testing.T) {
	pid := startRenamedBrowser(t)
	checkRequireNeedsSignature(t, "renamed binary", func(t *testing.T) { stubPeerPID(t, pid, true) })
}

// On macOS an unknown traced program is judged as untraced
// (tracedOutsiderJudgedUntraced), but a name-matched browser is still a known
// browser, and the require rules hold under the macOS rule too: a renamed
// binary is refused unsigned and can't lower the mode.
func TestRequireNeedsSignatureUnderDarwinRule(t *testing.T) {
	useTracedOutsiderRule(t, true)
	checkRequireNeedsSignature(t, "darwin rule, stand-in", stubTracedBrowser)
}

func TestRequireNeedsSignatureFromRenamedBinaryUnderDarwinRule(t *testing.T) {
	useTracedOutsiderRule(t, true)
	pid := startRenamedBrowser(t)
	checkRequireNeedsSignature(t, "darwin rule, renamed binary", func(t *testing.T) { stubPeerPID(t, pid, true) })
}

func checkRequireNeedsSignature(t *testing.T, label string, asCaller func(*testing.T)) {
	t.Helper()
	unsigned := func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}
	type fixture struct {
		a                *apiServer
		approver, second testDevice
		pendingID        string
	}
	setUp := func(mode string) fixture {
		a := humanProofTestServer(t, mode)
		f := fixture{a: a, approver: newTestDevice(t), second: newTestDevice(t)}
		approveTestDevice(t, a, f.approver)
		approveTestDevice(t, a, f.second)
		f.pendingID = enrolTestDevice(t, a, "Pending", newTestDevice(t))
		if err := a.humanProof.noteApprovalNotice(&trustedDevice{ID: "dev_x", Name: "x"}, "first-device", time.Now()); err != nil {
			t.Fatal(err)
		}
		asCaller(t)
		return f
	}
	remove := func(f fixture, r *http.Request) int {
		r.SetPathValue("id", deviceID(f.second.spki))
		r, _ = throughProof(f.a, r)
		w := httptest.NewRecorder()
		f.a.handleRemoveRemoteDevice(w, r)
		return w.Code
	}
	acknowledge := func(f fixture, r *http.Request) int {
		r, _ = throughProof(f.a, r)
		w := httptest.NewRecorder()
		f.a.handleAcknowledgeHumanProof(w, r)
		return w.Code
	}
	approvePath := func(f fixture) string { return "/api/remote-devices/" + f.pendingID + "/approve" }
	removePath := func(f fixture) string { return "/api/remote-devices/" + deviceID(f.second.spki) }

	// Require, unsigned: refused for everything the person alone may do.
	f := setUp(humanProofRequire)
	if got := f.a.identifyCaller(unsigned("POST", "/api/x", "")); got.Kind != callerUnverified || !got.Unproven {
		t.Fatalf("%s require: unsigned browser = %+v, want unverified", label, got)
	}
	w := httptest.NewRecorder()
	if f.a.requireHuman(w, unsigned("POST", "/api/x", ""), "change agent permissions") || w.Code != http.StatusForbidden {
		t.Fatalf("%s require: person-only act unsigned = %d, want refused", label, w.Code)
	}
	for _, lower := range []string{humanProofReport, humanProofOff} {
		if code := putHumanProof(t, f.a, unsigned("PUT", "/api/human-proof", `{"mode":"`+lower+`"}`)); code != http.StatusForbidden {
			t.Fatalf("%s require: unsigned lowering to %s = %d, want refused", label, lower, code)
		}
	}
	if code := approveRequest(t, f.a, f.pendingID, func() *http.Request { return unsigned("POST", approvePath(f), "") }); code != http.StatusForbidden || f.a.trustedDevices.isApproved(f.pendingID) {
		t.Fatalf("%s require: unsigned approval = %d, want refused", label, code)
	}
	if code := remove(f, unsigned("DELETE", removePath(f), "")); code != http.StatusForbidden || !f.a.trustedDevices.isApproved(deviceID(f.second.spki)) {
		t.Fatalf("%s require: unsigned removal = %d, want refused", label, code)
	}
	if code := acknowledge(f, unsigned("POST", "/api/human-proof/acknowledge", "")); code != http.StatusForbidden || len(f.a.humanProofView("").Notices) == 0 {
		t.Fatalf("%s require: unsigned acknowledgement = %d, want refused", label, code)
	}

	// Require, signed by an approved device from the same process: allowed.
	signed := func(method, path, body, nonce string) *http.Request {
		return f.approver.localRequest(t, f.a, method, path, body, time.Now(), nonce)
	}
	s, _ := throughProof(f.a, signed("POST", "/api/x", "", "s1"))
	if got := f.a.identifyCaller(s); got.Kind != callerHuman || got.Device != deviceID(f.approver.spki) {
		t.Fatalf("%s require: signed browser = %+v, want the person", label, got)
	}
	if code := approveRequest(t, f.a, f.pendingID, func() *http.Request { return signed("POST", approvePath(f), "", "s2") }); code != http.StatusOK {
		t.Fatalf("%s require: signed approval = %d", label, code)
	}
	if code := remove(f, signed("DELETE", removePath(f), "", "s3")); code != http.StatusNoContent {
		t.Fatalf("%s require: signed removal = %d", label, code)
	}
	if code := acknowledge(f, signed("POST", "/api/human-proof/acknowledge", "", "s4")); code != http.StatusOK {
		t.Fatalf("%s require: signed acknowledgement = %d", label, code)
	}
	if code := putHumanProof(t, f.a, signed("PUT", "/api/human-proof", `{"mode":"report"}`, "s5")); code != http.StatusOK {
		t.Fatalf("%s require: signed lowering = %d", label, code)
	}

	// Off and report: a traced browser still counts on its name.
	for _, mode := range []string{humanProofOff, humanProofReport} {
		f := setUp(mode)
		if got := f.a.identifyCaller(unsigned("POST", "/api/x", "")); got.Kind != callerHuman || got.Unproven {
			t.Fatalf("%s %s: unsigned browser = %+v, want the person", label, mode, got)
		}
		if code := approveRequest(t, f.a, f.pendingID, func() *http.Request { return unsigned("POST", approvePath(f), "") }); code != http.StatusOK {
			t.Fatalf("%s %s: unsigned approval = %d, want allowed", label, mode, code)
		}
		if code := remove(f, unsigned("DELETE", removePath(f), "")); code != http.StatusNoContent {
			t.Fatalf("%s %s: unsigned removal = %d, want allowed", label, mode, code)
		}
		if code := acknowledge(f, unsigned("POST", "/api/human-proof/acknowledge", "")); code != http.StatusOK {
			t.Fatalf("%s %s: unsigned acknowledgement = %d, want allowed", label, mode, code)
		}
		if mode == humanProofReport {
			if code := putHumanProof(t, f.a, unsigned("PUT", "/api/human-proof", `{"mode":"off"}`)); code != http.StatusOK {
				t.Fatalf("%s report: unsigned lowering to off = %d, want allowed", label, code)
			}
		}
	}
}

// startRenamedBrowser runs a copy of sleep named chrome, orphaned (its shell
// exits) so that its parent is not this test, which would make it the
// daemon's own worker. It needs the /proc process names the daemon traces
// by; elsewhere the stand-in test covers the same rules.
func startRenamedBrowser(t *testing.T) int {
	t.Helper()
	if _, err := os.Stat("/proc/self/comm"); err != nil {
		t.Skip("needs /proc process names")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	name := "chrome"
	raw, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `"$0" 30 >/dev/null 2>&1 & echo $!`, bin)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SOLU_PANE_ID=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("renamed browser pid %q", out)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	for deadline := time.Now().Add(2 * time.Second); !peerIsBrowser(pid) || peerIsAutomationBrowser(pid); {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) does not read as a browser no program drives", name, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return pid
}
