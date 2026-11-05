package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func humanProofTestServer(t *testing.T, mode string) *apiServer {
	t.Helper()
	t.Setenv(humanProofEnv, "")
	t.Setenv(previewOwnerEnv, "")
	stubWSLHost(t, false)
	a := trustedDevicesTestServer(t)
	a.humanProof = newHumanProof(t.TempDir())
	if mode != "" {
		if err := a.humanProof.set(mode, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// throughProof runs r through the local proof middleware and returns the
// request the API would see, and the response headers it set.
func throughProof(a *apiServer, r *http.Request) (*http.Request, http.Header) {
	var seen *http.Request
	w := httptest.NewRecorder()
	a.localDeviceProof(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r })).ServeHTTP(w, r)
	return seen, w.Header()
}

// stubTracedOutsider traces the caller to a process outside every pane and
// not started by the daemon (pid 1 with the proxy check off), as the
// caller_authority tests do.
func stubTracedOutsider(t *testing.T) {
	t.Helper()
	stubPeerPID(t, 1, true)
	prev := peerIsRelayProxy
	t.Cleanup(func() { peerIsRelayProxy = prev })
	peerIsRelayProxy = func(int) bool { return false }
}

func (d testDevice) localRequest(t *testing.T, a *apiServer, method, target, body string, at time.Time, nonce string) *http.Request {
	t.Helper()
	r := d.signedRequestFor(t, a.trustedDevices.localTarget(), method, target, body, at, nonce)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func TestUntracedCallerByMode(t *testing.T) {
	stubPeerPID(t, 0, false)
	for _, tc := range []struct {
		mode string
		want callerKind
	}{{humanProofOff, callerHuman}, {humanProofReport, callerHuman}, {humanProofRequire, callerUnverified}} {
		a := humanProofTestServer(t, tc.mode)
		approveTestDevice(t, a, newTestDevice(t)) // require needs a device to take effect
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		got := a.identifyCaller(r)
		if got.Kind != tc.want || !got.Unproven {
			t.Fatalf("%s: header-only caller = %+v, want %s and unproven", tc.mode, got, tc.want)
		}
	}
}

func TestSignedUntracedCallerIsThePerson(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	stubPeerPID(t, 0, false)
	r, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/x", `{"a":1}`, time.Now(), "n1"))
	got := a.identifyCaller(r)
	if got.Kind != callerHuman || got.Device != deviceID(d.spki) || got.Unproven {
		t.Fatalf("signed caller = %+v", got)
	}
	// A handler may ask twice; the proof stays with the request.
	if again := a.identifyCaller(r); again.Kind != callerHuman {
		t.Fatalf("second identifyCaller = %+v", again)
	}
	if b, _ := io.ReadAll(r.Body); string(b) != `{"a":1}` {
		t.Fatalf("body after verification = %q", b)
	}
	// Through a relay proxy (WSL init, the desktop router) too.
	stubPeerPID(t, 1, true)
	r, _ = throughProof(a, d.localRequest(t, a, "POST", "/api/x", "", time.Now(), "n2"))
	if got := a.identifyCaller(r); got.Kind != callerHuman || got.Device == "" {
		t.Fatalf("signed caller through a proxy = %+v", got)
	}
}

func TestLocalProofReplayAndWindow(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	stubPeerPID(t, 0, false)
	now := time.Now()
	first, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/x", "{}", now, "same"))
	if a.identifyCaller(first).Kind != callerHuman {
		t.Fatal("first use refused")
	}
	replay, h := throughProof(a, d.localRequest(t, a, "POST", "/api/x", "{}", now, "same"))
	if got := a.identifyCaller(replay); got.Kind == callerHuman || h.Get(humanProofHeader) != "replay" {
		t.Fatalf("replayed nonce = %+v (%q)", got, h.Get(humanProofHeader))
	}
	old, h := throughProof(a, d.localRequest(t, a, "POST", "/api/x", "{}", now.Add(-2*deviceSigMaxSkew), "fresh"))
	if got := a.identifyCaller(old); got.Kind == callerHuman || h.Get(humanProofHeader) != "stale" {
		t.Fatalf("request past the window = %+v (%q)", got, h.Get(humanProofHeader))
	}
	// Signed for a different action: not this one.
	r := d.localRequest(t, a, "POST", "/api/x", "{}", now, "moved")
	r.URL.Path = "/api/y"
	moved, _ := throughProof(a, r)
	if a.identifyCaller(moved).Kind == callerHuman {
		t.Fatal("a signature moved to another path counted")
	}
}

func TestLocalProofTargets(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	stubPeerPID(t, 0, false)

	// Another daemon's key (the other backend behind the desktop router).
	other, err := loadDaemonIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := d.signedRequestFor(t, localDeviceTargetFor(other.spki), "POST", "/api/x", "", time.Now(), "c1")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	seen, h := throughProof(a, r)
	if a.identifyCaller(seen).Kind == callerHuman || h.Get(humanProofHeader) != "wrong-target" {
		t.Fatalf("another daemon's signature counted (%q)", h.Get(humanProofHeader))
	}

	// A relay signature used locally is not a local proof.
	r = d.signedRequest(t, "POST", "/api/x", "", time.Now(), "c2")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	seen, _ = throughProof(a, r)
	if a.identifyCaller(seen).Kind == callerHuman {
		t.Fatal("a relay signature counted locally")
	}

	// A local signature sent over the relay is refused by the relay gate.
	if err := a.trustedDevices.setPolicy(remoteAccessApproved); err != nil {
		t.Fatal(err)
	}
	r = d.signedRequestFor(t, a.trustedDevices.localTarget(), "GET", "/api/panes", "", time.Now(), "c3")
	w := httptest.NewRecorder()
	a.relayDeviceGate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("served") })).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("local signature over the relay = %d", w.Code)
	}
}

func TestSignatureNeverUpgradesATracedAgent(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	cmd := startSleeper(t)
	a.mgr.panes["p-agent"] = &Pane{ID: "p-agent", Pid: cmd.Process.Pid}
	stubPeerPID(t, cmd.Process.Pid, true)
	r, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/x", "", time.Now(), "p1"))
	if got := a.identifyCaller(r); got.Kind != callerPane || got.PaneID != "p-agent" {
		t.Fatalf("pane with a stolen signature = %+v, want pane", got)
	}
	stubPeerPID(t, os.Getpid(), true)
	r, _ = throughProof(a, d.localRequest(t, a, "POST", "/api/x", "", time.Now(), "p2"))
	if got := a.identifyCaller(r); got.Kind != callerWorker {
		t.Fatalf("worker with a stolen signature = %+v, want worker", got)
	}
	// Any other traced program (outside panes and workers, e.g. a browser
	// missing from the name list) counts with an approved device's
	// signature, as it would untraced; without one, require refuses it on
	// every platform.
	stubTracedOutsider(t)
	prev := peerIsBrowser
	t.Cleanup(func() { peerIsBrowser = prev })
	peerIsBrowser = func(int) bool { return false }
	r, _ = throughProof(a, d.localRequest(t, a, "POST", "/api/x", "", time.Now(), "p3"))
	if got := a.identifyCaller(r); got.Kind != callerHuman || got.Device != deviceID(d.spki) {
		t.Fatalf("traced other program with a signature = %+v, want the person", got)
	}
	unsigned := httptest.NewRequest("POST", "/api/x", nil)
	unsigned.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, darwin := range []bool{false, true} {
		useTracedOutsiderRule(t, darwin)
		if got := a.identifyCaller(unsigned); got.Kind != callerUnverified {
			t.Fatalf("darwin=%t: traced other program without one = %+v, want unverified", darwin, got)
		}
	}
}

// A browser missing from the name list (Arc, other Chromium builds) is a
// traced program outside every pane. On macOS it is judged like an
// untraceable caller: browser headers make an unproven person in Off/Report
// and are refused under require. On Linux and Windows it is unverified
// without an approved device's signature, so person-only acts are refused.
// Either way a signature makes it the person.
func TestUnknownBrowserPerPlatform(t *testing.T) {
	prev := peerIsBrowser
	t.Cleanup(func() { peerIsBrowser = prev })
	peerIsBrowser = func(int) bool { return false }
	headers := func() *http.Request {
		r := httptest.NewRequest("POST", "/api/x", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}
	for _, darwin := range []bool{false, true} {
		useTracedOutsiderRule(t, darwin)
		for _, mode := range []string{humanProofOff, humanProofReport} {
			a := humanProofTestServer(t, mode)
			stubTracedOutsider(t)
			got := a.identifyCaller(headers())
			if darwin && (got.Kind != callerHuman || !got.Unproven || got.PID != 1) {
				t.Fatalf("darwin %s: unknown browser, headers only = %+v, want an unproven person", mode, got)
			}
			if !darwin {
				if got.Kind != callerUnverified {
					t.Fatalf("linux/windows %s: unknown browser, headers only = %+v, want unverified", mode, got)
				}
				w := httptest.NewRecorder()
				if a.requireHuman(w, headers(), "change agent permissions") || w.Code != http.StatusForbidden {
					t.Fatalf("linux/windows %s: person-only act from an unknown browser = %d, want refused", mode, w.Code)
				}
			}
			if got := a.identifyCaller(httptest.NewRequest("POST", "/api/x", nil)); got.Kind != callerUnverified {
				t.Fatalf("darwin=%t %s: unknown program without browser headers = %+v, want unverified", darwin, mode, got)
			}
		}
		a := humanProofTestServer(t, humanProofRequire)
		d := newTestDevice(t)
		approveTestDevice(t, a, d)
		stubTracedOutsider(t)
		if got := a.identifyCaller(headers()); got.Kind != callerUnverified {
			t.Fatalf("darwin=%t require: unknown browser, headers only = %+v, want refused", darwin, got)
		}
		signed, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/x", "", time.Now(), "u1"))
		if got := a.identifyCaller(signed); got.Kind != callerHuman || got.Device != deviceID(d.spki) {
			t.Fatalf("darwin=%t require: unknown browser, signed = %+v, want the person", darwin, got)
		}
	}
}

// On macOS the same unknown browser can approve itself as the first device in
// every mode (require counts as report until a device is approved), and its
// signed requests are the person's from then on. On Linux and Windows it
// can't, as before: only a known browser or an untraceable one can.
func TestUnknownBrowserFirstDevicePerPlatform(t *testing.T) {
	for _, darwin := range []bool{false, true} {
		for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
			t.Run(fmt.Sprintf("darwin=%t/%s", darwin, mode), func(t *testing.T) {
				useTracedOutsiderRule(t, darwin)
				a := humanProofTestServer(t, mode)
				stubTracedOutsider(t)
				prev := peerIsBrowser
				t.Cleanup(func() { peerIsBrowser = prev })
				peerIsBrowser = func(int) bool { return false }
				d := newTestDevice(t)
				w := bootstrap(t, a, bootstrapRequest(t, a, d, "f1"))
				if !darwin {
					if w.Code != http.StatusForbidden || a.trustedDevices.isApproved(deviceID(d.spki)) {
						t.Fatalf("first device from an unknown browser = %d %s, want refused", w.Code, w.Body)
					}
					return
				}
				if w.Code != http.StatusOK {
					t.Fatalf("first device from an unknown browser = %d %s", w.Code, w.Body)
				}
				if got := a.trustedDevices.find(deviceID(d.spki)); got == nil || got.Status != deviceStatusApproved {
					t.Fatalf("bootstrapped device = %+v", got)
				}
				signed, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/x", "", time.Now(), "f2"))
				if got := a.identifyCaller(signed); got.Kind != callerHuman || got.Device != deviceID(d.spki) {
					t.Fatalf("signed request after bootstrap = %+v, want the person", got)
				}
			})
		}
	}
}

// Pane ancestry still decides first on every platform: a process inside a
// pane is that pane, whatever headers it sends and whatever the mode.
func TestPaneProcessWithBrowserHeadersStaysThePane(t *testing.T) {
	prev := peerIsBrowser
	t.Cleanup(func() { peerIsBrowser = prev })
	for _, darwin := range []bool{false, true} {
		useTracedOutsiderRule(t, darwin)
		for _, browser := range []bool{false, true} {
			peerIsBrowser = func(int) bool { return browser }
			for _, mode := range []string{humanProofOff, humanProofReport} {
				a := humanProofTestServer(t, mode)
				cmd := startSleeper(t)
				a.mgr.panes["p-agent"] = &Pane{ID: "p-agent", Pid: cmd.Process.Pid}
				stubPeerPID(t, cmd.Process.Pid, true)
				r := httptest.NewRequest("POST", "/api/x", nil)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				if got := a.identifyCaller(r); got.Kind != callerPane || got.PaneID != "p-agent" {
					t.Fatalf("darwin=%t %s browser=%t: pane process with browser headers = %+v, want the pane", darwin, mode, browser, got)
				}
			}
		}
	}
}

// macOS's WebKit network service carries any app's web view (Safari, the
// desktop app, a WKWebView a program creates), so it is judged like an
// untraceable connection: an approved device's signature decides, and under
// require headers alone are refused.
func TestSharedNetworkServiceIsJudgedLikeUntraceable(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	stubTracedOutsider(t)
	prevShared, prevBrowser := peerIsSharedNetworkService, peerIsBrowser
	t.Cleanup(func() { peerIsSharedNetworkService, peerIsBrowser = prevShared, prevBrowser })
	peerIsSharedNetworkService = func(int) bool { return true }
	peerIsBrowser = func(int) bool { return true }
	headers := httptest.NewRequest("POST", "/api/x", nil)
	headers.Header.Set("Sec-Fetch-Site", "same-origin")
	if got := a.identifyCaller(headers); got.Kind != callerUnverified || !got.Unproven {
		t.Fatalf("WebKit network service, headers only, under require = %+v", got)
	}
	signed, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/x", "", time.Now(), "wk1"))
	if got := a.identifyCaller(signed); got.Kind != callerHuman || got.Device == "" {
		t.Fatalf("WebKit network service, signed = %+v", got)
	}
}

func TestLocalProofLargeBodyIsUnsignedButIntact(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	stubPeerPID(t, 0, false)
	big := strings.Repeat("x", humanProofMaxBody+10)
	for _, chunked := range []bool{false, true} {
		r := d.localRequest(t, a, "POST", "/api/x", big, time.Now(), "big"+strconv.FormatBool(chunked))
		if chunked {
			r.ContentLength = -1
		}
		seen, h := throughProof(a, r)
		if a.identifyCaller(seen).Kind == callerHuman || h.Get(humanProofHeader) != "too-large" {
			t.Fatalf("chunked=%t: oversize body counted as proof (%q)", chunked, h.Get(humanProofHeader))
		}
		if b, _ := io.ReadAll(seen.Body); len(b) != len(big) {
			t.Fatalf("chunked=%t: handler got %d bytes, want %d", chunked, len(b), len(big))
		}
	}
}

func TestRequireWithoutDevicesActsAsReport(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	if got := a.humanProofMode(); got != humanProofReport {
		t.Fatalf("require with no approved device = %s, want report", got)
	}
}

func stubWSLHost(t *testing.T, wsl bool) {
	t.Helper()
	prev := humanProofWSLHost
	humanProofWSLHost = func() bool { return wsl }
	t.Cleanup(func() { humanProofWSLHost = prev })
}

func TestHumanProofEnvOffAndOutsideChanges(t *testing.T) {
	stubWSLHost(t, false)
	dir := t.TempDir()
	t.Setenv(previewOwnerEnv, "")
	t.Setenv(humanProofEnv, "off")
	rec := humanProofRecord{Mode: humanProofOff, Confirmed: humanProofRequire}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, humanProofFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHumanProof(dir)
	if !h.envOff || h.lowered != "require → off" {
		t.Fatalf("env/lowered = %t %q", h.envOff, h.lowered)
	}
	a := trustedDevicesTestServer(t)
	a.humanProof = h
	approveTestDevice(t, a, newTestDevice(t))
	if err := h.set(humanProofRequire, time.Now()); err != nil {
		t.Fatal(err)
	}
	if a.humanProofMode() != humanProofOff {
		t.Fatal("SOLU_HUMAN_PROOF=off did not win over the file")
	}
	if h.lowered != "" {
		t.Fatal("a change made in the app still reads as made outside it")
	}

	t.Setenv(humanProofEnv, "")
	t.Setenv(previewOwnerEnv, "1")
	if got := newHumanProof(t.TempDir()).configuredMode(); got != humanProofReport {
		t.Fatalf("Preview default = %s, want report", got)
	}
	t.Setenv(previewOwnerEnv, "")
	if got := newHumanProof(t.TempDir()).configuredMode(); got != humanProofOff {
		t.Fatalf("default = %s, want off", got)
	}
	stubWSLHost(t, true)
	if got := newHumanProof(t.TempDir()).configuredMode(); got != humanProofReport {
		t.Fatalf("default under WSL = %s, want report", got)
	}
}

func putHumanProof(t *testing.T, a *apiServer, r *http.Request) int {
	t.Helper()
	r, _ = throughProof(a, r)
	w := httptest.NewRecorder()
	a.handlePutHumanProof(w, r)
	return w.Code
}

func TestChangingTheMode(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	d := newTestDevice(t)
	stubPeerPID(t, 0, false)
	headerOnly := func(mode string) *http.Request {
		r := httptest.NewRequest("PUT", "/api/human-proof", strings.NewReader(`{"mode":"`+mode+`"}`))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}
	signed := func(mode, nonce string) *http.Request {
		return d.localRequest(t, a, "PUT", "/api/human-proof", `{"mode":"`+mode+`"}`, time.Now(), nonce)
	}
	if code := putHumanProof(t, a, headerOnly(humanProofRequire)); code != http.StatusForbidden {
		t.Fatalf("require from an unapproved browser = %d", code)
	}
	r, _ := throughProof(a, headerOnly(humanProofOff))
	w := httptest.NewRecorder()
	a.handlePutHumanProof(w, r)
	var refusal map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusForbidden || refusal["error"] != "device_required" {
		t.Fatalf("lowering report → off by headers alone = %d, %v", w.Code, refusal)
	}
	wantRecovery := "Lower the device check from an approved browser or app. If none is left, add SOLU_HUMAN_PROOF=off to ~/.solu/env (WSL, Linux or macOS) or %USERPROFILE%\\.solu\\env (Windows-native), then restart SoluTerminal's background service. If your desktop tray has Restart background service, choose it (on Windows it restarts only the WSL service; if it refuses, use the steps below). Otherwise: with WSL, quit SoluTerminal from the tray icon, run wsl --shutdown in Windows PowerShell (this stops everything running in WSL, including your agents) and reopen SoluTerminal; on Linux run systemctl --user restart soluterminal.service, or if you start it yourself, stop and start it again; on macOS run launchctl kickstart -k gui/$(id -u)/com.soluterminal.daemon, or if you start it yourself, stop and start it again; on Windows without WSL, quit SoluTerminal from the tray icon and reopen it. Then approve a device in Settings › Privacy & security, remove the SOLU_HUMAN_PROOF=off line, and restart the same way again. While that line is there, any browser or program on this computer can approve or remove devices."
	if refusal["message"] != wantRecovery {
		t.Fatalf("lowering refusal lost the background-service recovery steps: %v", refusal["message"])
	}
	approveTestDevice(t, a, d)
	if code := putHumanProof(t, a, signed(humanProofRequire, "m1")); code != http.StatusOK {
		t.Fatalf("require from an approved device = %d", code)
	}
	if code := putHumanProof(t, a, headerOnly(humanProofOff)); code != http.StatusForbidden {
		t.Fatalf("lowering require unsigned = %d", code)
	}
	relayed := signed(humanProofOff, "m2")
	relayed = relayed.WithContext(context.WithValue(relayed.Context(), relayTransportKey{}, true))
	if code := putHumanProof(t, a, relayed); code != http.StatusForbidden {
		t.Fatalf("lowering over the relay = %d", code)
	}
	// A traced browser is known only by its name: it can't lower require
	// unsigned, but signed it is an approved device like any other, and
	// from report it may lower the mode on its own.
	prev := peerIsBrowser
	prevAuto := peerIsAutomationBrowser
	t.Cleanup(func() { peerIsBrowser, peerIsAutomationBrowser = prev, prevAuto })
	peerIsBrowser = func(int) bool { return true }
	peerIsAutomationBrowser = func(int) bool { return false }
	stubTracedOutsider(t)
	if code := putHumanProof(t, a, headerOnly(humanProofReport)); code != http.StatusForbidden {
		t.Fatalf("lowering require from an unsigned traced browser = %d", code)
	}
	if code := putHumanProof(t, a, signed(humanProofReport, "m3")); code != http.StatusOK {
		t.Fatalf("lowering require from a signed traced browser = %d", code)
	}
	if code := putHumanProof(t, a, headerOnly(humanProofOff)); code != http.StatusOK {
		t.Fatalf("lowering report from a traced browser = %d", code)
	}
	if code := putHumanProof(t, a, signed(humanProofReport, "m4")); code != http.StatusOK {
		t.Fatalf("raising to report from an approved device = %d", code)
	}
	stubPeerPID(t, 0, false)
	if code := putHumanProof(t, a, signed(humanProofOff, "m5")); code != http.StatusOK {
		t.Fatalf("lowering from an approved device = %d", code)
	}
}

func TestLastDeviceCannotBeRemovedUnderRequire(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	stubPeerPID(t, 0, false)
	remove := func(nonce string) int {
		r := d.localRequest(t, a, "DELETE", "/api/remote-devices/"+deviceID(d.spki), "", time.Now(), nonce)
		r.SetPathValue("id", deviceID(d.spki))
		r, _ = throughProof(a, r)
		w := httptest.NewRecorder()
		a.handleRemoveRemoteDevice(w, r)
		return w.Code
	}
	if code := remove("r1"); code != http.StatusConflict {
		t.Fatalf("removing the last device under require = %d", code)
	}
	second := newTestDevice(t)
	approveTestDevice(t, a, second)
	if code := remove("r2"); code != http.StatusNoContent {
		t.Fatalf("removing one of two devices = %d", code)
	}
}

func bootstrapRequest(t *testing.T, a *apiServer, d testDevice, nonce string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": "Edge on Windows", "publicKey": d.spki})
	return d.localRequest(t, a, "POST", "/api/human-proof/bootstrap", string(body), time.Now(), nonce)
}

func bootstrap(t *testing.T, a *apiServer, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	r, _ = throughProof(a, r)
	w := httptest.NewRecorder()
	a.handleBootstrapHumanProof(w, r)
	return w
}

func TestFirstDeviceBootstrap(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	d := newTestDevice(t)

	// Signed by a different key than the one enrolled: refused.
	impostor := newTestDevice(t)
	body, _ := json.Marshal(map[string]string{"name": "x", "publicKey": d.spki})
	r := impostor.localRequest(t, a, "POST", "/api/human-proof/bootstrap", string(body), time.Now(), "b0")
	if w := bootstrap(t, a, r); w.Code != http.StatusBadRequest {
		t.Fatalf("bootstrap signed by another key = %d", w.Code)
	}
	// Unsigned: refused.
	r = httptest.NewRequest("POST", "/api/human-proof/bootstrap", bytes.NewReader(body))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if w := bootstrap(t, a, r); w.Code != http.StatusBadRequest {
		t.Fatalf("unsigned bootstrap = %d", w.Code)
	}
	// Signed, but by a program without browser headers: not the person.
	r = bootstrapRequest(t, a, d, "b1")
	r.Header.Del("Sec-Fetch-Site")
	if w := bootstrap(t, a, r); w.Code != http.StatusForbidden {
		t.Fatalf("bootstrap from a non-browser = %d", w.Code)
	}
	if w := bootstrap(t, a, bootstrapRequest(t, a, d, "b2")); w.Code != http.StatusOK {
		t.Fatalf("first device bootstrap = %d %s", w.Code, w.Body)
	}
	got := a.trustedDevices.find(deviceID(d.spki))
	if got == nil || got.Status != deviceStatusApproved || got.ApprovedVia != "first-device" || got.Surface != deviceSurfaceLocal {
		t.Fatalf("bootstrapped device = %+v", got)
	}
	if n := a.humanProofView("").Notices; len(n) != 1 || n[0].ID != got.ID || n[0].Via != "first-device" {
		t.Fatalf("first-device notice = %+v", n)
	}
	// Closed once any device is approved.
	if w := bootstrap(t, a, bootstrapRequest(t, a, newTestDevice(t), "b3")); w.Code != http.StatusConflict {
		t.Fatalf("second bootstrap = %d", w.Code)
	}

	// The notice clears only from an approved device.
	ack := httptest.NewRequest("POST", "/api/human-proof/acknowledge", nil)
	ack.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	a.handleAcknowledgeHumanProof(w, ack)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unsigned acknowledge = %d", w.Code)
	}
	signedAck, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/human-proof/acknowledge", "", time.Now(), "b4"))
	w = httptest.NewRecorder()
	a.handleAcknowledgeHumanProof(w, signedAck)
	if w.Code != http.StatusOK || len(a.humanProofView("").Notices) != 0 {
		t.Fatalf("signed acknowledge = %d", w.Code)
	}
}

func TestBootstrapClosedWhenARelayDeviceIsApproved(t *testing.T) {
	a := humanProofTestServer(t, humanProofOff)
	stubPeerPID(t, 0, false)
	approveTestDevice(t, a, newTestDevice(t)) // the phone
	if w := bootstrap(t, a, bootstrapRequest(t, a, newTestDevice(t), "z1")); w.Code != http.StatusConflict {
		t.Fatalf("bootstrap with a phone approved = %d", w.Code)
	}
}

func TestAutomationBrowsers(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	prev, prevAuto := peerIsBrowser, peerIsAutomationBrowser
	t.Cleanup(func() { peerIsBrowser, peerIsAutomationBrowser = prev, prevAuto })
	peerIsBrowser = func(int) bool { return true }
	peerIsAutomationBrowser = func(int) bool { return true }
	stubTracedOutsider(t)

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if got := a.identifyCaller(r); got.Kind != callerUnverified || !got.Automation {
		t.Fatalf("automation browser under require = %+v", got)
	}
	signed, _ := throughProof(a, d.localRequest(t, a, "POST", "/", "", time.Now(), "a1"))
	if got := a.identifyCaller(signed); got.Kind != callerHuman {
		t.Fatalf("automation browser with an approved device's signature = %+v", got)
	}
	if err := a.humanProof.set(humanProofReport, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := a.identifyCaller(r); got.Kind != callerHuman || !got.Unproven {
		t.Fatalf("automation browser under report = %+v", got)
	}

	// Never enrolled or bootstrapped.
	enrol := httptest.NewRequest("POST", "/api/remote-devices/enroll", strings.NewReader(`{"name":"x","publicKey":"`+newTestDevice(t).spki+`"}`))
	w := httptest.NewRecorder()
	a.handleEnrollRemoteDevice(w, enrol)
	if w.Code != http.StatusForbidden {
		t.Fatalf("enrolment from an automation browser = %d", w.Code)
	}
	b := humanProofTestServer(t, humanProofOff)
	if w := bootstrap(t, b, bootstrapRequest(t, b, newTestDevice(t), "a2")); w.Code != http.StatusForbidden {
		t.Fatalf("bootstrap from an automation browser = %d", w.Code)
	}
}

func TestPeerIsAutomationBrowserRejectsThisTestProcess(t *testing.T) {
	// Not a browser at all, whatever started the test run.
	if peerIsAutomationBrowser(os.Getpid()) {
		t.Fatal("the Go test binary was taken for an automation browser")
	}
}

func TestPromptGateHoldsUnverifiedUnderRequire(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	target := choicePromptPane(t, "pane_target")
	a.mgr.panes[target.ID] = target
	stubPeerPID(t, 0, false)
	req := httptest.NewRequest("GET", "/api/panes/pane_target/io", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if a.promptInputGate(req)(target) {
		t.Fatal("an unproven socket could type into a permission prompt under require")
	}
	// The person's own socket is signed in its URL.
	u := "/api/panes/pane_target/io"
	unix := strconv.FormatInt(time.Now().Unix(), 10)
	msg := deviceSigMessage(a.trustedDevices.localTarget(), "GET", u, "", unix, "ws1", nil)
	q := "?" + deviceQueryID + "=" + deviceID(d.spki) + "&" + deviceQueryTime + "=" + unix + "&" + deviceQueryNonce + "=ws1&" +
		deviceQueryTarget + "=" + a.trustedDevices.localTarget() + "&" + deviceQuerySig + "=" + d.sign(t, msg)
	signed, _ := throughProof(a, httptest.NewRequest("GET", u+q, nil))
	if !a.promptInputGate(signed)(target) {
		t.Fatal("the person's signed socket was held back")
	}
	if signed.URL.RawQuery != "" {
		t.Fatalf("signature fields left in the query: %q", signed.URL.RawQuery)
	}
	if err := a.humanProof.set(humanProofReport, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !a.promptInputGate(req)(target) {
		t.Fatal("an untraceable caller was held back outside require")
	}
}

func TestApprovalRecordsProvenance(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	approver := newTestDevice(t)
	approveTestDevice(t, a, approver)
	stubPeerPID(t, 0, false)
	pending := newTestDevice(t)
	rec, err := a.trustedDevices.enroll("Chrome", pending.spki, 2)
	if err != nil {
		t.Fatal(err)
	}
	approve := func(r *http.Request) int {
		r.SetPathValue("id", rec.ID)
		r, _ = throughProof(a, r)
		w := httptest.NewRecorder()
		a.handleApproveRemoteDevice(w, r)
		return w.Code
	}
	headerOnly := httptest.NewRequest("POST", "/api/remote-devices/"+rec.ID+"/approve", nil)
	headerOnly.Header.Set("Sec-Fetch-Site", "same-origin")
	if code := approve(headerOnly); code != http.StatusForbidden {
		t.Fatalf("approval by headers alone under require = %d", code)
	}
	if code := approve(approver.localRequest(t, a, "POST", "/api/remote-devices/"+rec.ID+"/approve", "", time.Now(), "v1")); code != http.StatusOK {
		t.Fatalf("approval from an approved device = %d", code)
	}
	if got := a.trustedDevices.find(rec.ID).ApprovedVia; got != "device:"+deviceID(approver.spki) {
		t.Fatalf("approvedVia = %q", got)
	}

	// From the phone over the relay: allowed only with the phone's verified
	// signature.
	third := newTestDevice(t)
	rec, _ = a.trustedDevices.enroll("Firefox", third.spki, 2)
	relayed := httptest.NewRequest("POST", "/api/remote-devices/"+rec.ID+"/approve", nil)
	relayed = relayed.WithContext(context.WithValue(context.WithValue(relayed.Context(), relayTransportKey{}, true), relayVerifiedDeviceKey{}, deviceID(approver.spki)))
	if code := approve(relayed); code != http.StatusOK {
		t.Fatalf("approval from the approved phone over the relay = %d", code)
	}
	if got := a.trustedDevices.find(rec.ID).ApprovedVia; got != "relay-device:"+deviceID(approver.spki) {
		t.Fatalf("approvedVia over the relay = %q", got)
	}
}

func TestReportModeCountsUnprovenActs(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if !a.requireHuman(httptest.NewRecorder(), r, "answer a test") {
		t.Fatal("report mode refused")
	}
	u := a.humanProofView("").Unproven
	if u == nil || u.Count != 1 || u.Action != "answer a test" {
		t.Fatalf("unproven = %+v", u)
	}
}

func TestRequireRefusalAsksToApproveThisBrowser(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	approveTestDevice(t, a, newTestDevice(t))
	stubPeerPID(t, 0, false)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	if a.requireHuman(w, r, "answer a test") {
		t.Fatal("require accepted headers alone")
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body["error"] != "device_required" {
		t.Fatalf("refusal = %v", body)
	}
	want := "This screen isn't an approved device yet, so it can't answer a test. Open Settings → Privacy & security → Ask to approve this browser, then approve it from a browser or app you've already approved. If that button isn't shown here, use an already-approved device instead. If you're locked out, recovery steps are in Settings → Privacy & security on the computer."
	if body["message"] != want {
		t.Fatalf("refusal does not explain enrolment: %v", body["message"])
	}
}

// A socket outlives a change of mode: one opened from browser headers alone
// before require must be held at prompts once require is on.
func TestPromptGateFollowsTheModeOfAnOpenSocket(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	approveTestDevice(t, a, newTestDevice(t))
	target := choicePromptPane(t, "pane_target")
	a.mgr.panes[target.ID] = target
	stubPeerPID(t, 0, false)
	req := httptest.NewRequest("GET", "/api/panes/pane_target/io", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	gate := a.promptInputGate(req) // opened under report
	if !gate(target) {
		t.Fatal("held back under report")
	}
	if err := a.humanProof.set(humanProofRequire, time.Now()); err != nil {
		t.Fatal(err)
	}
	if gate(target) {
		t.Fatal("a socket opened before require could still answer a permission prompt")
	}
	idle := &Pane{ID: "pane_idle", Status: PaneRunning, buffer: NewRingBuffer(1 << 10), subs: map[int64]*subEntry{}}
	if !gate(idle) {
		t.Fatal("held back from a pane with no prompt")
	}
}

// Two removals racing for the last two devices leave one.
func TestConcurrentRemovalsKeepOneDevice(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	devs := []testDevice{newTestDevice(t), newTestDevice(t)}
	for _, d := range devs {
		approveTestDevice(t, a, d)
	}
	stubPeerPID(t, 0, false)
	codes := make(chan int, 2)
	for i, d := range devs {
		r := d.localRequest(t, a, "DELETE", "/api/remote-devices/"+deviceID(d.spki), "", time.Now(), "race"+strconv.Itoa(i))
		r.SetPathValue("id", deviceID(d.spki))
		r, _ = throughProof(a, r)
		go func(r *http.Request) {
			w := httptest.NewRecorder()
			a.handleRemoveRemoteDevice(w, r)
			codes <- w.Code
		}(r)
	}
	got := []int{<-codes, <-codes}
	if a.approvedDeviceCount() != 1 {
		t.Fatalf("approved devices left = %d (codes %v)", a.approvedDeviceCount(), got)
	}
	if got[0]+got[1] != http.StatusConflict+http.StatusNoContent {
		t.Fatalf("codes = %v, want one 204 and one 409", got)
	}
}

// A signed socket loses its say at prompts when its device is removed.
func TestSignedSocketLosesPromptsWhenItsDeviceIsRemoved(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	d, other := newTestDevice(t), newTestDevice(t)
	approveTestDevice(t, a, d)
	approveTestDevice(t, a, other)
	target := choicePromptPane(t, "pane_target")
	a.mgr.panes[target.ID] = target
	stubPeerPID(t, 0, false)
	r, _ := throughProof(a, d.localRequest(t, a, "GET", "/api/panes/pane_target/io", "", time.Now(), "s1"))
	gate := a.promptInputGate(r)
	if !gate(target) {
		t.Fatal("an approved device's socket was held back")
	}
	if err := a.trustedDevices.remove(deviceID(d.spki)); err != nil {
		t.Fatal(err)
	}
	if gate(target) {
		t.Fatal("a removed device's socket could still answer a permission prompt")
	}
	if err := a.humanProof.set(humanProofReport, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !gate(target) {
		t.Fatal("held back outside require")
	}
}

// Racing headers-only approvals while no device is approved: only one can
// become the first device.
func TestRacingFirstApprovalsOverHTTP(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	ids := []string{}
	for i := 0; i < devicePendingLimit; i++ { // a full list refuses more
		d, err := a.trustedDevices.enroll("Racer", newTestDevice(t).spki, 2)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	codes := make(chan int, len(ids))
	for _, id := range ids {
		go func(id string) {
			codes <- approveRequest(t, a, id, func() *http.Request {
				r := httptest.NewRequest("POST", "/api/remote-devices/"+id+"/approve", nil)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				return r
			})
		}(id)
	}
	ok := 0
	for range ids {
		if <-codes == http.StatusOK {
			ok++
		}
	}
	if ok != 1 || a.approvedDeviceCount() != 1 {
		t.Fatalf("approved %d (count %d), want exactly one first device", ok, a.approvedDeviceCount())
	}
}

func approveRequest(t *testing.T, a *apiServer, id string, mutate func() *http.Request) int {
	t.Helper()
	r := mutate()
	r.SetPathValue("id", id)
	r, _ = throughProof(a, r)
	w := httptest.NewRecorder()
	a.handleApproveRemoteDevice(w, r)
	return w.Code
}

// Once any device is approved, browser headers alone never approve or
// remove a device, whatever the mode.
func TestHeaderOnlyDeviceManagementOnceADeviceExists(t *testing.T) {
	for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
		t.Run(mode, func(t *testing.T) {
			a := humanProofTestServer(t, mode)
			approver := newTestDevice(t)
			approveTestDevice(t, a, approver)
			stubPeerPID(t, 0, false)
			pending, _ := a.trustedDevices.enroll("Agent key", newTestDevice(t).spki, 2)
			headerOnly := func(method, path string) func() *http.Request {
				return func() *http.Request {
					r := httptest.NewRequest(method, path, nil)
					r.Header.Set("Sec-Fetch-Site", "same-origin")
					return r
				}
			}
			if code := approveRequest(t, a, pending.ID, headerOnly("POST", "/api/remote-devices/"+pending.ID+"/approve")); code != http.StatusForbidden {
				t.Fatalf("headers-only approval with a device approved = %d", code)
			}
			if a.trustedDevices.isApproved(pending.ID) {
				t.Fatal("approved on headers alone")
			}
			r, _ := throughProof(a, headerOnly("DELETE", "/api/remote-devices/"+deviceID(approver.spki))())
			r.SetPathValue("id", deviceID(approver.spki))
			w := httptest.NewRecorder()
			a.handleRemoveRemoteDevice(w, r)
			if w.Code != http.StatusForbidden || !a.trustedDevices.isApproved(deviceID(approver.spki)) {
				t.Fatalf("headers-only removal with a device approved = %d", w.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			message, _ := body["message"].(string)
			if body["error"] != "device_required" || !strings.Contains(message, "Open Settings → Privacy & security → Ask to approve this browser, then approve it from a browser or app you've already approved. If that button isn't shown here, use an already-approved device instead. If you're locked out, recovery steps are in Settings → Privacy & security on the computer.") {
				t.Fatalf("removal refusal lost the code or enrolment steps: %v", body)
			}
			// Signed by the approved device: allowed.
			if code := approveRequest(t, a, pending.ID, func() *http.Request {
				return approver.localRequest(t, a, "POST", "/api/remote-devices/"+pending.ID+"/approve", "", time.Now(), "ok-"+mode)
			}); code != http.StatusOK {
				t.Fatalf("signed approval = %d", code)
			}
		})
	}
}

func TestDeviceManagementProofsThatCount(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	approver := newTestDevice(t)
	approveTestDevice(t, a, approver)

	// A traced browser at this computer.
	traced, _ := a.trustedDevices.enroll("Chrome", newTestDevice(t).spki, 2)
	prev, prevAuto := peerIsBrowser, peerIsAutomationBrowser
	t.Cleanup(func() { peerIsBrowser, peerIsAutomationBrowser = prev, prevAuto })
	peerIsBrowser = func(int) bool { return true }
	peerIsAutomationBrowser = func(int) bool { return false }
	stubTracedOutsider(t)
	headerOnly := func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+traced.ID+"/approve", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}
	if code := approveRequest(t, a, traced.ID, headerOnly); code != http.StatusOK {
		t.Fatalf("approval from a traced browser = %d", code)
	}
	if got := a.trustedDevices.find(traced.ID).ApprovedVia; got != "traced" {
		t.Fatalf("approvedVia = %q", got)
	}
	// A traced automation browser is no proof.
	peerIsAutomationBrowser = func(int) bool { return true }
	auto, _ := a.trustedDevices.enroll("Driven", newTestDevice(t).spki, 2)
	if code := approveRequest(t, a, auto.ID, func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+auto.ID+"/approve", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}); code != http.StatusForbidden {
		t.Fatalf("approval from an automation browser = %d", code)
	}

	// The approved phone over the relay.
	stubPeerPID(t, 0, false)
	phoneApproved, _ := a.trustedDevices.enroll("Firefox", newTestDevice(t).spki, 2)
	if code := approveRequest(t, a, phoneApproved.ID, func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+phoneApproved.ID+"/approve", nil)
		return r.WithContext(context.WithValue(context.WithValue(r.Context(), relayTransportKey{}, true), relayVerifiedDeviceKey{}, deviceID(approver.spki)))
	}); code != http.StatusOK {
		t.Fatalf("approval from the approved phone = %d", code)
	}
}

// A phone on its private channel proved itself in the handshake, and its
// requests skip the relay signature gate: it manages devices like a phone
// that signs over the relay. The channel mark counts only on the relay.
func TestPrivateChannelDeviceManagesDevices(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	phone := newTestDevice(t)
	approveTestDevice(t, a, phone)
	stubPeerPID(t, 0, false)
	onChannel := func(r *http.Request) *http.Request {
		return r.WithContext(withPrivateRelayDevice(context.WithValue(r.Context(), relayTransportKey{}, true), deviceID(phone.spki)))
	}

	other, _ := a.trustedDevices.enroll("Laptop", newTestDevice(t).spki, 2)
	if code := approveRequest(t, a, other.ID, func() *http.Request {
		return onChannel(httptest.NewRequest("POST", "/api/remote-devices/"+other.ID+"/approve", nil))
	}); code != http.StatusOK {
		t.Fatalf("approval from the phone's private channel = %d", code)
	}
	if got := a.trustedDevices.find(other.ID).ApprovedVia; got != "relay-device:"+deviceID(phone.spki) {
		t.Fatalf("approvedVia = %q", got)
	}

	if err := a.humanProof.noteApprovalNotice(&trustedDevice{ID: "dev_x", Name: "x"}, "first-device", time.Now()); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.handleAcknowledgeHumanProof(w, onChannel(httptest.NewRequest("POST", "/api/human-proof/acknowledge", nil)))
	if w.Code != http.StatusOK || len(a.humanProofView("").Notices) != 0 {
		t.Fatalf("acknowledge from the phone's private channel = %d", w.Code)
	}

	r := onChannel(httptest.NewRequest("DELETE", "/api/remote-devices/"+other.ID, nil))
	r.SetPathValue("id", other.ID)
	w = httptest.NewRecorder()
	a.handleRemoveRemoteDevice(w, r)
	if w.Code != http.StatusNoContent || a.trustedDevices.find(other.ID) != nil {
		t.Fatalf("removal from the phone's private channel = %d", w.Code)
	}

	// The channel mark without the relay transport proves nothing.
	again, _ := a.trustedDevices.enroll("Tablet", newTestDevice(t).spki, 2)
	if code := approveRequest(t, a, again.ID, func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+again.ID+"/approve", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r.WithContext(withPrivateRelayDevice(r.Context(), deviceID(phone.spki)))
	}); code != http.StatusForbidden {
		t.Fatalf("approval with a channel mark off the relay = %d", code)
	}
}

// With no device approved, a headers-only approval is the computer's first
// device: recorded as such and announced until acknowledged.
func TestHeaderOnlyFirstApprovalIsAnnounced(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	phone, _ := a.trustedDevices.enroll("Phone", newTestDevice(t).spki, 2)
	if code := approveRequest(t, a, phone.ID, func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+phone.ID+"/approve", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}); code != http.StatusOK {
		t.Fatalf("first approval = %d", code)
	}
	if got := a.trustedDevices.find(phone.ID).ApprovedVia; got != "first-device" {
		t.Fatalf("approvedVia = %q", got)
	}
	if n := a.humanProofView("").Notices; len(n) != 1 || n[0].ID != phone.ID || n[0].Via != "first-device" {
		t.Fatalf("first-device notice = %+v", n)
	}
	// Racing first approvals: only one gets in.
	b := humanProofTestServer(t, humanProofReport)
	x, _ := b.trustedDevices.enroll("X", newTestDevice(t).spki, 2)
	y, _ := b.trustedDevices.enroll("Y", newTestDevice(t).spki, 2)
	if _, err := b.trustedDevices.approveWith(x.ID, "", true, "first-device"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.trustedDevices.approveWith(y.ID, "", true, "first-device"); !errors.Is(err, errDeviceAlreadyBootstrapped) {
		t.Fatalf("second first-device approval = %v", err)
	}
}

// The remote-access policy and V3 comparisons outlive a later switch to
// require, so once a device is approved they take the same proof as
// approving one, in every mode; never over the relay.
func TestPolicyAndComparisonsNeedDeviceProof(t *testing.T) {
	type call struct {
		name   string
		method string
		path   string
		body   string
		run    func(a *apiServer, w http.ResponseWriter, r *http.Request)
	}
	calls := []call{
		{"policy", "PUT", "/api/remote-devices/policy", `{"policy":"any"}`, func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handlePutRemoteAccessPolicy(w, r) }},
		{"confirm", "POST", "/api/remote-devices/dev_x/verify/confirm", `{"enrolmentId":"enr_x"}`, func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleConfirmDeviceVerification(w, r) }},
		{"reject", "POST", "/api/remote-devices/dev_x/verify/reject", `{"enrolmentId":"enr_x"}`, func(a *apiServer, w http.ResponseWriter, r *http.Request) { a.handleRejectDeviceVerification(w, r) }},
	}
	gateRefused := func(w *httptest.ResponseRecorder) bool { return w.Code == http.StatusForbidden }
	for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
		for _, c := range calls {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				a := humanProofTestServer(t, mode)
				d := newTestDevice(t)
				approveTestDevice(t, a, d)
				stubPeerPID(t, 0, false)
				send := func(r *http.Request) *httptest.ResponseRecorder {
					r.SetPathValue("id", "dev_x")
					r, _ = throughProof(a, r)
					w := httptest.NewRecorder()
					c.run(a, w, r)
					return w
				}
				headers := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
				headers.Header.Set("Sec-Fetch-Site", "same-origin")
				if w := send(headers); !gateRefused(w) {
					t.Fatalf("headers only = %d %s", w.Code, w.Body)
				}
				if w := send(d.localRequest(t, a, c.method, c.path, c.body, time.Now(), "s-"+mode+c.name)); gateRefused(w) {
					t.Fatalf("signed by an approved device = %d %s", w.Code, w.Body)
				}
				relayed := d.localRequest(t, a, c.method, c.path, c.body, time.Now(), "r-"+mode+c.name)
				relayed = relayed.WithContext(context.WithValue(context.WithValue(relayed.Context(), relayTransportKey{}, true), relayVerifiedDeviceKey{}, deviceID(d.spki)))
				if w := send(relayed); w.Code != http.StatusForbidden {
					t.Fatalf("over the relay = %d, want refused (at the computer only)", w.Code)
				}
			})
		}
	}
}

func TestPolicyFromATracedBrowserAndUnderEnvRecovery(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	approveTestDevice(t, a, newTestDevice(t))
	put := func() int {
		r := httptest.NewRequest("PUT", "/api/remote-devices/policy", strings.NewReader(`{"policy":"approved"}`))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r, _ = throughProof(a, r)
		w := httptest.NewRecorder()
		a.handlePutRemoteAccessPolicy(w, r)
		return w.Code
	}
	prev, prevAuto := peerIsBrowser, peerIsAutomationBrowser
	t.Cleanup(func() { peerIsBrowser, peerIsAutomationBrowser = prev, prevAuto })
	peerIsBrowser = func(int) bool { return true }
	peerIsAutomationBrowser = func(int) bool { return false }
	stubTracedOutsider(t)
	if code := put(); code != http.StatusOK {
		t.Fatalf("policy from a traced browser = %d", code)
	}
	// SOLU_HUMAN_PROOF=off: the person's recovery lets headers manage devices.
	stubPeerPID(t, 0, false)
	if code := put(); code != http.StatusForbidden {
		t.Fatalf("headers only = %d", code)
	}
	a.humanProof.envOff = true
	if code := put(); code != http.StatusOK {
		t.Fatalf("headers only under SOLU_HUMAN_PROOF=off = %d", code)
	}
	pending, _ := a.trustedDevices.enroll("Desktop", newTestDevice(t).spki, 2)
	if code := approveRequest(t, a, pending.ID, func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+pending.ID+"/approve", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}); code != http.StatusOK {
		t.Fatalf("approval under SOLU_HUMAN_PROOF=off = %d", code)
	}
	if got := a.trustedDevices.find(pending.ID).ApprovedVia; got != "env-recovery" {
		t.Fatalf("approvedVia under recovery = %q", got)
	}
	// Announced like a first device, and still after the env line is gone,
	// until an approved device acknowledges it.
	a.humanProof.envOff = false
	if n := a.humanProofView("").Notices; len(n) != 1 || n[0].ID != pending.ID || n[0].Via != "env-recovery" {
		t.Fatalf("env-recovery notice = %+v", n)
	}
}

// A pending first-device notice written by an earlier build survives the
// move to the notice list.
func TestLegacyFirstDeviceNoticeIsKept(t *testing.T) {
	stubWSLHost(t, false)
	t.Setenv(humanProofEnv, "")
	dir := t.TempDir()
	legacy := `{"mode":"report","confirmed":"report","updatedAt":"2026-11-02T00:00:00Z","firstDevice":{"id":"dev_old","name":"Edge on Windows","at":"2026-11-02T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, humanProofFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHumanProof(dir)
	if n := h.record.Notices; len(n) != 1 || n[0].ID != "dev_old" || n[0].Via != "first-device" {
		t.Fatalf("notices = %+v", n)
	}
	if err := h.set(humanProofReport, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, humanProofFile))
	if strings.Contains(string(b), `"firstDevice"`) || !strings.Contains(string(b), `"dev_old"`) {
		t.Fatalf("saved = %s", b)
	}
}

// SOLU_HUMAN_PROOF=off lets the person remove a lost device on browser
// headers where nothing can be traced.
func TestEnvRecoveryRemovesALostDevice(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	lost := newTestDevice(t)
	approveTestDevice(t, a, lost)
	stubPeerPID(t, 0, false)
	remove := func() int {
		r := httptest.NewRequest("DELETE", "/api/remote-devices/"+deviceID(lost.spki), nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r, _ = throughProof(a, r)
		r.SetPathValue("id", deviceID(lost.spki))
		w := httptest.NewRecorder()
		a.handleRemoveRemoteDevice(w, r)
		return w.Code
	}
	if code := remove(); code != http.StatusForbidden {
		t.Fatalf("headers-only removal without the recovery = %d", code)
	}
	a.humanProof.envOff = true
	if code := remove(); code != http.StatusNoContent {
		t.Fatalf("removing the lost device under the recovery = %d", code)
	}
}

// A notice saved before any mode was chosen leaves the mode to the default.
func TestNoticeDoesNotChooseTheMode(t *testing.T) {
	stubWSLHost(t, false)
	t.Setenv(humanProofEnv, "")
	t.Setenv(previewOwnerEnv, "")
	dir := t.TempDir()
	h := newHumanProof(dir)
	if err := h.noteApprovalNotice(&trustedDevice{ID: "dev_x", Name: "x"}, "first-device", time.Now()); err != nil {
		t.Fatal(err)
	}
	stubWSLHost(t, true) // the default changes (e.g. an update)
	again := newHumanProof(dir)
	if got := again.configuredMode(); got != humanProofReport {
		t.Fatalf("mode after a notice = %s, want the current default (report)", got)
	}
	if len(again.record.Notices) != 1 {
		t.Fatal("notice lost")
	}
}

func TestHumanProofStatusIsNotServedOverTheRelay(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	r := httptest.NewRequest("GET", "/api/human-proof?device=dev_x", nil)
	r = r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
	w := httptest.NewRecorder()
	a.handleGetHumanProof(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("over the relay = %d", w.Code)
	}
}

// A device named in a notice can't acknowledge it while another approved
// device can; the first device alone still can.
func TestNoticeIsAcknowledgedFromAnotherDevice(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	ack := func(d testDevice, nonce string) int {
		r, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/human-proof/acknowledge", "", time.Now(), nonce))
		w := httptest.NewRecorder()
		a.handleAcknowledgeHumanProof(w, r)
		return w.Code
	}
	first := newTestDevice(t)
	if w := bootstrap(t, a, bootstrapRequest(t, a, first, "n0")); w.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d", w.Code)
	}
	// Alone, the first device acknowledges its own notice.
	if code := ack(first, "n1"); code != http.StatusOK || len(a.humanProofView("").Notices) != 0 {
		t.Fatalf("first device alone = %d", code)
	}

	// An env-recovery approval while another device exists.
	a.humanProof.envOff = true
	recovered := newTestDevice(t)
	pending, _ := a.trustedDevices.enroll("Recovered", recovered.spki, 2)
	if code := approveRequest(t, a, pending.ID, func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+pending.ID+"/approve", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}); code != http.StatusOK {
		t.Fatalf("env-recovery approval = %d", code)
	}
	a.humanProof.envOff = false
	if code := ack(recovered, "n2"); code != http.StatusForbidden || len(a.humanProofView("").Notices) != 1 {
		t.Fatalf("named device acknowledging itself = %d", code)
	}
	if code := ack(first, "n3"); code != http.StatusOK || len(a.humanProofView("").Notices) != 0 {
		t.Fatalf("another approved device = %d", code)
	}
}

// Two devices that each carry a notice can't vouch for each other: a
// program that approved its own key twice would otherwise clear both.
func TestNoticedDevicesCantClearEachOther(t *testing.T) {
	a := humanProofTestServer(t, humanProofReport)
	stubPeerPID(t, 0, false)
	first := newTestDevice(t)
	if w := bootstrap(t, a, bootstrapRequest(t, a, first, "m0")); w.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d", w.Code)
	}
	a.humanProof.envOff = true
	second := newTestDevice(t)
	pending, _ := a.trustedDevices.enroll("Second", second.spki, 2)
	if code := approveRequest(t, a, pending.ID, func() *http.Request {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+pending.ID+"/approve", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		return r
	}); code != http.StatusOK {
		t.Fatalf("env-recovery approval = %d", code)
	}
	a.humanProof.envOff = false
	if n := a.humanProofView("").Notices; len(n) != 2 {
		t.Fatalf("notices = %+v", n)
	}
	if code := ackAs(t, a, second, "m1"); code != http.StatusForbidden {
		t.Fatalf("second acknowledging = %d", code)
	}
	if code := ackAs(t, a, first, "m2"); code != http.StatusForbidden {
		t.Fatalf("first acknowledging = %d", code)
	}
	if n := a.humanProofView("").Notices; len(n) != 2 {
		t.Fatalf("notices after both tried = %+v", n)
	}
}

// ackAs acknowledges the notices from device d, signed at this computer.
func ackAs(t *testing.T, a *apiServer, d testDevice, nonce string) int {
	t.Helper()
	r, _ := throughProof(a, d.localRequest(t, a, "POST", "/api/human-proof/acknowledge", "", time.Now(), nonce))
	w := httptest.NewRecorder()
	a.handleAcknowledgeHumanProof(w, r)
	return w.Code
}
