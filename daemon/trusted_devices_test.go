package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
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

type testDevice struct {
	key  *ecdsa.PrivateKey
	spki string
}

func newTestDevice(t *testing.T) testDevice {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return testDevice{key: key, spki: base64.StdEncoding.EncodeToString(der)}
}

// sign produces a WebCrypto-style P1363 signature, as the web client will.
func (d testDevice) sign(t *testing.T, msg []byte) string {
	t.Helper()
	digest := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, d.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return base64.RawURLEncoding.EncodeToString(sig)
}

const testMachine = "mach_test01"

// The test daemon is the "wsl" backend of testMachine.
func (d testDevice) signedRequest(t *testing.T, method, target, body string, at time.Time, nonce string) *http.Request {
	t.Helper()
	return d.signedRequestFor(t, testMachine+"/wsl", method, target, body, at, nonce)
}

func (d testDevice) signedRequestFor(t *testing.T, daemon, method, target, body string, at time.Time, nonce string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	unix := strconv.FormatInt(at.Unix(), 10)
	msg := deviceSigMessage(daemon, method, r.URL.Path, r.URL.RawQuery, unix, nonce, []byte(body))
	r.Header.Set(deviceHeaderTarget, daemon)
	r.Header.Set(deviceHeaderID, deviceID(d.spki))
	r.Header.Set(deviceHeaderTime, unix)
	r.Header.Set(deviceHeaderNonce, nonce)
	r.Header.Set(deviceHeaderSig, d.sign(t, msg))
	return r
}

func trustedDevicesTestServer(t *testing.T) *apiServer {
	t.Helper()
	a := authorityTestServer(t)
	a.trustedDevices = newTrustedDevices(t.TempDir())
	a.trustedDevices.identity = func() (string, string) { return testMachine, "wsl" }
	dataDir := t.TempDir()
	a.trustedDevices.daemonKey = func() *daemonIdentity {
		id, err := loadDaemonIdentity(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	return a
}

func approveTestDevice(t *testing.T, a *apiServer, d testDevice) {
	t.Helper()
	rec, err := a.trustedDevices.enroll("phone", d.spki)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.trustedDevices.approve(rec.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedDeviceEnrollment(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	first, err := a.trustedDevices.enroll("Pixel", d.spki)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != deviceStatusPending || len(first.Code) != 19 || first.ID != deviceID(d.spki) {
		t.Fatalf("enrolment = %+v", first)
	}
	again, _ := a.trustedDevices.enroll("Pixel again", d.spki)
	if again != first {
		t.Fatal("re-enrolling the same key must return the same record")
	}
	if _, err := a.trustedDevices.enroll("rsa?", "bm90IGEga2V5"); err == nil {
		t.Fatal("a non-P-256 key was accepted")
	}
	// A full list refuses new requests; none waiting is dropped for them.
	for i := 1; i < devicePendingLimit+2; i++ {
		_, err := a.trustedDevices.enroll("spam", newTestDevice(t).spki)
		if full := i >= devicePendingLimit; full != errors.Is(err, errDevicePendingFull) || (!full && err != nil) {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if a.trustedDevices.find(first.ID) == nil {
		t.Fatal("the first waiting request was dropped for later ones")
	}
	pending := 0
	for _, rec := range a.trustedDevices.record.Devices {
		if rec.Status == deviceStatusPending {
			pending++
		}
	}
	if pending != devicePendingLimit {
		t.Fatalf("pending queue = %d, want capped at %d", pending, devicePendingLimit)
	}
	// Persisted and reloaded with its key.
	reloaded := newTrustedDevices(strings.TrimSuffix(a.trustedDevices.path, trustedDevicesFile))
	if len(reloaded.record.Devices) != pending || reloaded.record.Devices[0].key == nil {
		t.Fatalf("reloaded %d devices", len(reloaded.record.Devices))
	}
}

func TestRelayDeviceGate(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	now := time.Unix(1_800_000_000, 0)
	a.trustedDevices.now = func() time.Time { return now }

	var gotBody, gotQuery string
	h := a.relayDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotQuery = string(b), r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	serve := func(r *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := serve(httptest.NewRequest("POST", "/api/panes", strings.NewReader("{}"))); code != http.StatusOK {
		t.Fatalf("policy any must pass unsigned requests through, got %d", code)
	}
	if err := a.trustedDevices.setPolicy(remoteAccessApproved); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		req  func() *http.Request
		want int
	}{
		{"unsigned", func() *http.Request { return httptest.NewRequest("POST", "/api/panes", strings.NewReader("{}")) }, 401},
		{"enrolment is exempt", func() *http.Request {
			return httptest.NewRequest("POST", remoteDevicesEnrollPath, strings.NewReader("{}"))
		}, 200},
		{"health is exempt", func() *http.Request { return httptest.NewRequest("GET", "/api/health", nil) }, 200},
		{"signed", func() *http.Request {
			return d.signedRequest(t, "POST", "/api/panes?x=1", `{"cmd":["bash"]}`, now, "n1")
		}, 200},
		{"bare machine target on a named backend", func() *http.Request {
			return d.signedRequestFor(t, testMachine, "GET", "/api/panes", "", now, "n12")
		}, 401},
		{"replayed nonce", func() *http.Request {
			return d.signedRequest(t, "POST", "/api/panes?x=1", `{"cmd":["bash"]}`, now, "n1")
		}, 401},
		{"body changed in flight", func() *http.Request {
			r := d.signedRequest(t, "POST", "/api/panes", `{"cmd":["ls"]}`, now, "n2")
			r.Body = io.NopCloser(strings.NewReader(`{"cmd":["rm","-rf","/"]}`))
			return r
		}, 401},
		{"path changed in flight", func() *http.Request {
			r := d.signedRequest(t, "GET", "/api/panes", "", now, "n3")
			r.URL.Path = "/api/file"
			return r
		}, 401},
		{"stale", func() *http.Request {
			return d.signedRequest(t, "GET", "/api/panes", "", now.Add(-2*time.Minute), "n4")
		}, 401},
		{"unknown device", func() *http.Request {
			return newTestDevice(t).signedRequest(t, "GET", "/api/panes", "", now, "n5")
		}, 401},
		{"replayed from another machine", func() *http.Request {
			return d.signedRequestFor(t, "mach_other", "POST", "/api/panes", "{}", now, "n9")
		}, 401},
		{"named for another backend", func() *http.Request {
			return d.signedRequestFor(t, testMachine+"/win", "POST", "/api/panes", "{}", now, "n10")
		}, 401},
		{"named for this backend", func() *http.Request {
			return d.signedRequestFor(t, testMachine+"/wsl", "POST", "/api/panes", "{}", now, "n11")
		}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := serve(tc.req()); code != tc.want {
				t.Fatalf("got %d, want %d", code, tc.want)
			}
		})
	}

	// The handler sees the body and query the device signed.
	gotBody, gotQuery = "", ""
	if serve(d.signedRequest(t, "POST", "/api/panes?x=1", `{"a":1}`, now, "n6")) != 200 || gotBody != `{"a":1}` || gotQuery != "x=1" {
		t.Fatalf("handler saw body %q query %q", gotBody, gotQuery)
	}

	// WebSocket/EventSource form: fields in the query, removed before the handler.
	unix := strconv.FormatInt(now.Unix(), 10)
	msg := deviceSigMessage(testMachine+"/wsl", "GET", "/api/panes/p1/io", "cols=80", unix, "n7", nil)
	target := "/api/panes/p1/io?cols=80&" + deviceQueryID + "=" + deviceID(d.spki) + "&" + deviceQueryTime + "=" + unix +
		"&" + deviceQueryNonce + "=n7&" + deviceQueryTarget + "=" + testMachine + "/wsl&" + deviceQuerySig + "=" + d.sign(t, msg)
	if code := serve(httptest.NewRequest("GET", target, nil)); code != 200 || gotQuery != "cols=80" {
		t.Fatalf("query-signed upgrade: code %d, handler query %q", code, gotQuery)
	}

	// A pending device is not approved.
	p := newTestDevice(t)
	if _, err := a.trustedDevices.enroll("tablet", p.spki); err != nil {
		t.Fatal(err)
	}
	if code := serve(p.signedRequest(t, "GET", "/api/panes", "", now, "n8")); code != 401 {
		t.Fatalf("pending device got %d", code)
	}
}

func TestRemoteDeviceApprovalIsLocalAndHuman(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	rec, _ := a.trustedDevices.enroll("phone", d.spki)
	approve := func(mutate func(*http.Request)) int {
		r := httptest.NewRequest("POST", "/api/remote-devices/"+rec.ID+"/approve", nil)
		r.SetPathValue("id", rec.ID)
		r.Host = "127.0.0.1:8787"
		mutate(r)
		w := httptest.NewRecorder()
		a.handleApproveRemoteDevice(w, r)
		return w.Code
	}
	stubPeerPID(t, 0, false)
	if code := approve(func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		*r = *r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
	}); code != http.StatusForbidden {
		t.Fatalf("approval over the relay: %d", code)
	}
	if code := approve(func(*http.Request) {}); code != http.StatusForbidden {
		t.Fatalf("approval by a non-browser program: %d", code)
	}
	if code := approve(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }); code != http.StatusOK {
		t.Fatalf("approval by the person: %d", code)
	}
	if a.trustedDevices.find(rec.ID).Status != deviceStatusApproved {
		t.Fatal("device was not approved")
	}
}

// The approval code comes from the key, so swapping the key during enrolment
// changes the code the person sees.
// Clients that don't pin the computer's identity keep the device-key code.
func TestRemoteDeviceCodeVersionFollowsTheClient(t *testing.T) {
	a := trustedDevicesTestServer(t)
	old, cur := newTestDevice(t), newTestDevice(t)
	v1, _ := a.trustedDevices.enroll("old app", old.spki)
	v2, _ := a.trustedDevices.enroll("new app", cur.spki, 2)
	if v1.Code != deviceCode(old.spki, "") || viewOfDevice(v1).VerifiesComputer {
		t.Fatalf("v1 code = %q (verifies computer %v)", v1.Code, viewOfDevice(v1).VerifiesComputer)
	}
	if !viewOfDevice(v2).VerifiesComputer {
		t.Fatal("a V2 approval should be marked as verifying the computer")
	}
	if v2.Code != deviceCode(cur.spki, a.trustedDevices.daemonSPKI()) || v2.Code == deviceCode(cur.spki, "") {
		t.Fatalf("v2 code = %q", v2.Code)
	}
}

func TestRemoteDeviceCodeIsBoundToTheKey(t *testing.T) {
	a := trustedDevicesTestServer(t)
	real, swapped := newTestDevice(t), newTestDevice(t)
	rec, _ := a.trustedDevices.enroll("phone", real.spki, 2)
	if rec.Code != deviceCode(real.spki, a.trustedDevices.daemonSPKI()) || len(rec.Code) != 19 || strings.Count(rec.Code, "-") != 3 {
		t.Fatalf("code %q is not derived from the key", rec.Code)
	}
	if deviceCode(swapped.spki, a.trustedDevices.daemonSPKI()) == rec.Code {
		t.Fatal("two keys produced the same code")
	}
	// A relay that swaps the computer's key instead changes the code too.
	if other := newTestDevice(t); deviceCode(real.spki, other.spki) == rec.Code {
		t.Fatal("two keys produced the same code")
	}
}

func TestRemoteAccessPolicyIsLocalAndHuman(t *testing.T) {
	a := trustedDevicesTestServer(t)
	stubPeerPID(t, 0, false)
	put := func(policy string, mutate func(*http.Request)) int {
		r := httptest.NewRequest("PUT", "/api/remote-devices/policy", strings.NewReader(`{"policy":"`+policy+`"}`))
		r.Host = "127.0.0.1:8787"
		mutate(r)
		w := httptest.NewRecorder()
		a.handlePutRemoteAccessPolicy(w, r)
		return w.Code
	}
	browser := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }
	if code := put(remoteAccessApproved, func(*http.Request) {}); code != http.StatusForbidden {
		t.Fatalf("non-browser policy change: %d", code)
	}
	if code := put("open", browser); code != http.StatusBadRequest {
		t.Fatalf("invalid policy: %d", code)
	}
	if code := put(remoteAccessApproved, browser); code != http.StatusOK || a.trustedDevices.policy() != remoteAccessApproved {
		t.Fatalf("person policy change: %d %s", code, a.trustedDevices.policy())
	}
}

// The cloud re-encodes and sorts the query before forwarding; a signature
// over the client's own order must still verify.
func TestRelayDeviceGateSurvivesQueryReencoding(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	now := time.Unix(1_800_000_000, 0)
	a.trustedDevices.now = func() time.Time { return now }
	if err := a.trustedDevices.setPolicy(remoteAccessApproved); err != nil {
		t.Fatal(err)
	}
	h := a.relayDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := d.signedRequest(t, "GET", "/api/files/search?q=a+b%7E&dir=%2Ftmp&cols=80", "", now, "nq")
	r.URL.RawQuery = r.URL.Query().Encode() // what cloud/proxy.go forwards
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("re-encoded query broke the signature: %d", w.Code)
	}
	if got := canonicalDeviceQuery("b=2&a=1&a=0&x"); got != "a=1&a=0&b=2&x=" {
		t.Fatalf("canonical query = %q", got)
	}
}

// With only approved devices allowed, notifications leave the machine
// without session content but still say which pane needs the person.
func TestMinimisePushBody(t *testing.T) {
	in := []byte(`{"paneId":"p1","state":"needs-input","title":"deploy prod","cwd":"/home/u/secret","summary":"wants to run rm -rf","requestId":"r1","aiRisk":"high"}`)
	out := string(minimisePushBody(in))
	for _, leaked := range []string{"deploy prod", "/home/u/secret", "rm -rf", "high"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("minimised push still carries %q: %s", leaked, out)
		}
	}
	for _, kept := range []string{`"paneId":"p1"`, `"state":"needs-input"`, `"requestId":"r1"`} {
		if !strings.Contains(out, kept) {
			t.Fatalf("minimised push lost %s: %s", kept, out)
		}
	}
}

// A device that asked under the older code and asks again after updating
// gets the V2 code it now shows, while still pending.
func TestEnrollUpgradesAPendingV1Request(t *testing.T) {
	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	v1, err := a.trustedDevices.enroll("phone", d.spki)
	if err != nil || v1.CodeVersion == 2 {
		t.Fatalf("v1 enroll = %+v, %v", v1, err)
	}
	v2, err := a.trustedDevices.enroll("phone", d.spki, 2)
	if err != nil {
		t.Fatal(err)
	}
	if v2.CodeVersion != 2 || v2.Code != deviceCode(d.spki, a.trustedDevices.daemonSPKI()) {
		t.Fatalf("pending V1 request not upgraded: %+v", v2)
	}
}

// The starting policy is decided once, on the first start of a daemon with
// this change: a brand-new data directory begins with approved devices only,
// anything else keeps "any", and the decision is never revisited.
func TestRemoteAccessPolicyIsDecidedOnceAtFirstStart(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	decide := func(t *testing.T, dir string, explicitToken bool) string {
		t.Helper()
		got, err := decideRemoteAccessPolicyOnce(dir, explicitToken)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	cases := []struct {
		name  string
		setup func(dir string)
		token bool
		want  string
	}{
		{"missing data directory", func(string) {}, false, remoteAccessApproved},
		{"empty data directory", func(dir string) { _ = os.MkdirAll(dir, 0o700) }, false, remoteAccessApproved},
		{"only what the installer leaves (solud --claim, service.env, logs)", func(dir string) {
			write(t, dir, "claim.json", `{"machineId":"m1","token":"t"}`)
			write(t, dir, "service.env", "RELAY_ARGS=--relay x\n")
			write(t, dir, "logs/solud.out.log", "")
		}, false, remoteAccessApproved},
		{"desktop first launch (shell-info.json, copied claim)", func(dir string) {
			write(t, dir, "shell-info.json", `{"shell":"desktop"}`)
			write(t, dir, "claim.json", `{"machineId":"m1","token":"t"}`)
		}, false, remoteAccessApproved},
		{"claim save cut off by a power loss", func(dir string) {
			write(t, dir, "claim.json", `{"machineId":"m1","token":"t"}`)
			write(t, dir, claimTempPrefix+"123456", `{"machineId"`)
		}, false, remoteAccessApproved},
		{"own policy write cut off by a power loss", func(dir string) {
			write(t, dir, "claim.json", `{"machineId":"m1","token":"t"}`)
			write(t, dir, trustedDevicesTempPrefix+"654321", `{"pol`)
		}, false, remoteAccessApproved},
		{"existing install", func(dir string) {
			write(t, dir, "claim.json", `{"machineId":"m1","token":"t"}`)
			write(t, dir, "workspaces.json", "{}")
		}, false, remoteAccessAny},
		{"service given --relay-token, no claim.json", func(dir string) { _ = os.MkdirAll(dir, 0o700) }, true, remoteAccessAny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			tc.setup(dir)
			if got := decide(t, dir, tc.token); got != tc.want {
				t.Fatalf("decided %q, want %q", got, tc.want)
			}
			if got := newTrustedDevices(dir).policy(); got != tc.want {
				t.Fatalf("loaded policy %q, want %q", got, tc.want)
			}
			// Re-pairing, updates and later restarts never decide again.
			write(t, dir, "claim.json", `{"machineId":"m9","token":"t2"}`)
			write(t, dir, "workspaces.json", "{}")
			if again := decide(t, dir, !tc.token); again != "" {
				t.Fatalf("decided again: %q", again)
			}
			if got := newTrustedDevices(dir).policy(); got != tc.want {
				t.Fatalf("after a restart the policy is %q, want %q", got, tc.want)
			}
		})
	}
	chosen := t.TempDir()
	write(t, chosen, trustedDevicesFile, `{"policy":"approved","devices":[]}`)
	if got := decide(t, chosen, true); got != "" {
		t.Fatalf("a recorded choice was overwritten: %q", got)
	}
}

// A new enrolment is announced on this computer once; a device asking again
// while it waits is not.
func TestNewEnrolmentIsAnnouncedOnce(t *testing.T) {
	a := trustedDevicesTestServer(t)
	a.trustedDevices.record.Policy = remoteAccessApproved
	a.uiBus = newUIBus()
	events := a.uiBus.subscribe()
	d := newTestDevice(t)
	enrolKey := func(name, spki string) {
		body := `{"name":"` + name + `","publicKey":"` + spki + `","codeVersion":2}`
		w := httptest.NewRecorder()
		a.handleEnrollRemoteDevice(w, httptest.NewRequest("POST", remoteDevicesEnrollPath, strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("enrol: %d %s", w.Code, w.Body)
		}
	}
	defer func() {
		// A second new device inside the gap is not announced (it still waits
		// in Settings), and under "any" nothing is announced at all.
		enrolKey("Other phone", newTestDevice(t).spki)
		a.trustedDevices.record.Policy = remoteAccessAny
		a.trustedDevices.announcedAt = time.Time{}
		enrolKey("Third phone", newTestDevice(t).spki)
		if len(events) != 0 {
			t.Fatalf("unexpected announcement: %+v", <-events)
		}
	}()
	enrol := func() {
		body := `{"name":"Pixel 8a","publicKey":"` + d.spki + `","codeVersion":2}`
		w := httptest.NewRecorder()
		a.handleEnrollRemoteDevice(w, httptest.NewRequest("POST", remoteDevicesEnrollPath, strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("enrol: %d %s", w.Code, w.Body)
		}
	}
	enrol()
	enrol()
	var got []uiEvent
	for len(events) > 0 {
		got = append(got, <-events)
	}
	if len(got) != 1 || got[0].Type != "remote-device.pending" || got[0].Text != "Pixel 8a" {
		t.Fatalf("events = %+v, want one remote-device.pending for Pixel 8a", got)
	}
}

// Under approved-only, a preview frame's unsigned page load gets a page that
// explains why, while other refused requests keep the JSON error.
func TestRefusedPreviewPageExplainsItself(t *testing.T) {
	a := trustedDevicesTestServer(t)
	if err := a.trustedDevices.setPolicy(remoteAccessApproved); err != nil {
		t.Fatal(err)
	}
	h := a.relayDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	page := httptest.NewRequest("GET", "/api/proxy/5173/", nil)
	page.Header.Set("Accept", "text/html,application/xhtml+xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, page)
	if w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(w.Body.String(), "opens on the computer itself") {
		t.Fatalf("preview page: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	for _, r := range []*http.Request{
		httptest.NewRequest("GET", "/api/proxy/5173/app.js", nil),
		httptest.NewRequest("GET", "/api/panes", nil),
	} {
		r.Header.Set("Accept", "*/*")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "device_not_approved") {
			t.Fatalf("%s: %d %q", r.URL.Path, w.Code, w.Body.String())
		}
	}
}
