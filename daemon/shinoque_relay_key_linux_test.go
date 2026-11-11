package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	relayKeyTestOwner  = "owner-token-for-relay-key-tests"
	relayKeyTestKey    = "c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff"
	relayKeyTestWrong  = "0badf00d0badf00d0badf00d0badf00d0badf00d0badf00d0badf00d0badf00d"
	relayKeyTestFile   = "legacy-file-token"
	relayKeyTestDevice = "dev_b71SY8ikOq3NEn-6MV-j9z"
	relayKeyTestExe    = "/usr/bin/node"
)

// relayKeyTestCore is a fake Shinoque core: it records what each forward
// carried and, once given its own key, refuses any other relay proof as the
// real core does.
type relayKeyTestCore struct {
	mu      sync.Mutex
	key     string
	headers []http.Header
}

func (c *relayKeyTestCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, r.Header.Clone())
	if c.key != "" {
		want := c.key
		if device := r.Header.Get("X-Shinoque-Private-Device"); device != "" {
			want = shinoquePrivateDeviceProof(c.key, device)
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Shinoque-Relay")), []byte(want)) != 1 {
			http.Error(w, "bad_relay", http.StatusForbidden)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (c *relayKeyTestCore) setKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key = key
}

func (c *relayKeyTestCore) last(t *testing.T) http.Header {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		t.Fatal("no forward reached the core")
	}
	return c.headers[len(c.headers)-1]
}

func (c *relayKeyTestCore) hits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.headers)
}

type relayKeyTest struct {
	home, port, discovery string
	core                  *relayKeyTestCore
	clock                 time.Time
}

// newRelayKeyTest starts a fake core with a legacy relay-token file beside
// its discovery, and stands in a traced same-uid caller for every push.
func newRelayKeyTest(t *testing.T) *relayKeyTest {
	t.Helper()
	home := shinoqueTestToken(t, relayKeyTestFile)
	core := &relayKeyTestCore{}
	server := httptest.NewServer(core)
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	discovery := shinoqueTestDiscovery(t, home, port)
	previous := shinoqueRelayKeyPeer
	shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return 4242, relayKeyTestExe, true }
	t.Cleanup(func() { shinoqueRelayKeyPeer = previous })
	return &relayKeyTest{home: home, port: port, discovery: discovery, core: core, clock: time.Unix(1_800_000_000, 0)}
}

// server is one solud process: a fresh token cache holds no pushed key.
func (s *relayKeyTest) server() *apiServer {
	tokens := &shinoqueTokenCache{}
	tokens.pushedKey.now = func() time.Time { return s.clock }
	return &apiServer{gate: &authGate{token: relayKeyTestOwner}, shinoqueTokens: tokens}
}

func (s *relayKeyTest) body(key string) string {
	return fmt.Sprintf(`{"key":%q,"home":%q,"port":%s}`, key, s.home, s.port)
}

func relayKeyPushRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, shinoqueRelayKeyPath, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:41000"
	r.Header.Set("Authorization", "Bearer "+relayKeyTestOwner)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func relayKeyServe(a *apiServer, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.shinoqueRelayKeyPush(w, r)
	return w
}

func (s *relayKeyTest) push(t *testing.T, a *apiServer, key string) {
	t.Helper()
	w := relayKeyServe(a, relayKeyPushRequest(s.body(key)))
	if w.Code != http.StatusOK || w.Body.String() != "{\"accepted\":true}\n" {
		t.Fatalf("push = %d %q, want 200 {\"accepted\":true}", w.Code, w.Body.String())
	}
}

// forward sends one relayed request through the tunnel's handler, or through
// a device's private channel when device is set.
func relayKeyForward(a *apiServer, device string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/shinoque/api/threads", nil)
	r.Header.Set("X-Forwarded-Prefix", "/m/m1")
	if device != "" {
		r = r.WithContext(withPrivateRelayDevice(r.Context(), device))
	}
	w := httptest.NewRecorder()
	markRelayTransport(newShinoqueRelayHandler(http.NotFoundHandler(), a.shinoqueTokens, nil)).ServeHTTP(w, r)
	return w
}

func relayKeyHealth(t *testing.T, a *apiServer) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	a.health(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func relayKeyMark(t *testing.T, home string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, shinoqueRelayKeyMarker), []byte("memory"), 0600); err != nil {
		t.Fatal(err)
	}
}

type relayKeyLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *relayKeyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *relayKeyLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *relayKeyLog) lines(prefix string) []string {
	var out []string
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, prefix) {
			out = append(out, line[strings.Index(line, prefix):])
		}
	}
	return out
}

func captureRelayKeyLog(t *testing.T) *relayKeyLog {
	t.Helper()
	l := &relayKeyLog{}
	previous := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(previous) })
	return l
}

func relayKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}

func TestShinoqueRelayKeyPushRefusesNonLocalCallers(t *testing.T) {
	s := newRelayKeyTest(t)
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request) *http.Request
		status int
		code   string
	}{
		{"relay tunnel", func(r *http.Request) *http.Request {
			return r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
		}, 403, "local_only"},
		{"private channel", func(r *http.Request) *http.Request {
			ctx := context.WithValue(r.Context(), relayTransportKey{}, true)
			return r.WithContext(withPrivateRelayDevice(ctx, relayKeyTestDevice))
		}, 403, "local_only"},
		{"private channel device alone", func(r *http.Request) *http.Request {
			return r.WithContext(withPrivateRelayDevice(r.Context(), relayKeyTestDevice))
		}, 403, "local_only"},
		{"cloud prefix", func(r *http.Request) *http.Request { r.Header.Set("X-Forwarded-Prefix", "/m/m1"); return r }, 403, "local_only"},
		{"backend router prefix", func(r *http.Request) *http.Request { r.Header.Set("X-Forwarded-Prefix", "/b/wsl"); return r }, 403, "local_only"},
		{"router marker", func(r *http.Request) *http.Request { r.Header.Set("X-Solu-Proxy-Auth", "router"); return r }, 403, "local_only"},
		{"x-forwarded-for", func(r *http.Request) *http.Request { r.Header.Set("X-Forwarded-For", "127.0.0.1"); return r }, 403, "local_only"},
		{"forwarded", func(r *http.Request) *http.Request { r.Header.Set("Forwarded", "for=127.0.0.1"); return r }, 403, "local_only"},
		{"signed device", func(r *http.Request) *http.Request {
			return r.WithContext(context.WithValue(r.Context(), localVerifiedDeviceKey{}, relayKeyTestDevice))
		}, 403, "local_only"},
		{"alerts device", func(r *http.Request) *http.Request {
			return r.WithContext(context.WithValue(r.Context(), localScopedDeviceKey{}, relayKeyTestDevice))
		}, 403, "local_only"},
		{"not loopback", func(r *http.Request) *http.Request { r.RemoteAddr = "192.168.1.20:41000"; return r }, 403, "local_only"},
		{"browser origin", func(r *http.Request) *http.Request { r.Header.Set("Origin", "http://127.0.0.1:8787"); return r }, 403, "local_only"},
		{"browser fetch", func(r *http.Request) *http.Request { r.Header.Set("Sec-Fetch-Site", "same-origin"); return r }, 403, "local_only"},
		{"no token", func(r *http.Request) *http.Request { r.Header.Del("Authorization"); return r }, 401, "owner_token_required"},
		{"wrong token", func(r *http.Request) *http.Request { r.Header.Set("Authorization", "Bearer nope"); return r }, 401, "owner_token_required"},
		{"two tokens", func(r *http.Request) *http.Request {
			r.Header.Add("Authorization", "Bearer "+relayKeyTestOwner)
			return r
		}, 401, "owner_token_required"},
		{"cookie only", func(r *http.Request) *http.Request {
			r.Header.Del("Authorization")
			r.AddCookie(&http.Cookie{Name: tokenCookie, Value: relayKeyTestOwner})
			return r
		}, 401, "owner_token_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := s.server()
			w := relayKeyServe(a, tc.mutate(relayKeyPushRequest(s.body(relayKeyTestKey))))
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != tc.status || body["error"] != tc.code {
				t.Fatalf("push = %d %q, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			if key, pushed := a.shinoqueTokens.pushedKey.held(s.home, s.port); key != "" || pushed {
				t.Fatal("a refused push was held")
			}
		})
	}

	t.Run("untraced caller", func(t *testing.T) {
		shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return 0, "", false }
		a := s.server()
		if w := relayKeyServe(a, relayKeyPushRequest(s.body(relayKeyTestKey))); w.Code != 403 {
			t.Fatalf("untraced push = %d", w.Code)
		}
		if _, pushed := a.shinoqueTokens.pushedKey.held(s.home, s.port); pushed {
			t.Fatal("an untraced push was held")
		}
	})

	t.Run("wrong method is never 404", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, shinoqueRelayKeyPath, nil)
		if w := relayKeyServe(s.server(), r); w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET = %d, want 405", w.Code)
		}
	})
}

func TestShinoqueRelayKeyPushBody(t *testing.T) {
	s := newRelayKeyTest(t)
	port, _ := strconv.Atoi(s.port)
	for name, body := range map[string]string{
		"empty":             ``,
		"not an object":     `[]`,
		"missing port":      fmt.Sprintf(`{"key":%q,"home":%q}`, relayKeyTestKey, s.home),
		"generation":        fmt.Sprintf(`{"key":%q,"home":%q,"port":%d,"generation":1}`, relayKeyTestKey, s.home, port),
		"duplicate key":     fmt.Sprintf(`{"key":%q,"key":%q,"home":%q,"port":%d}`, relayKeyTestKey, relayKeyTestWrong, s.home, port),
		"uppercase hex":     fmt.Sprintf(`{"key":%q,"home":%q,"port":%d}`, strings.ToUpper(relayKeyTestKey), s.home, port),
		"short key":         fmt.Sprintf(`{"key":%q,"home":%q,"port":%d}`, relayKeyTestKey[:63], s.home, port),
		"base64 key":        fmt.Sprintf(`{"key":%q,"home":%q,"port":%d}`, "wP/uwP/uwP/uwP/uwP/uwP/uwP/uwP/uwP/uwP/uwP8=", s.home, port),
		"null home":         fmt.Sprintf(`{"key":%q,"home":null,"port":%d}`, relayKeyTestKey, port),
		"relative home":     fmt.Sprintf(`{"key":%q,"home":"shinoque","port":%d}`, relayKeyTestKey, port),
		"string port":       fmt.Sprintf(`{"key":%q,"home":%q,"port":"%d"}`, relayKeyTestKey, s.home, port),
		"port out of range": fmt.Sprintf(`{"key":%q,"home":%q,"port":70000}`, relayKeyTestKey, s.home),
		"trailing data":     s.body(relayKeyTestKey) + `{}`,
		"over 1 KiB":        fmt.Sprintf(`{"key":%q,"home":%q,"port":%d}`, relayKeyTestKey, s.home+strings.Repeat("/x", 600), port),
	} {
		t.Run(name, func(t *testing.T) {
			a := s.server()
			w := relayKeyServe(a, relayKeyPushRequest(body))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_body") {
				t.Fatalf("push = %d %q, want 400 bad_body", w.Code, w.Body.String())
			}
			if _, pushed := a.shinoqueTokens.pushedKey.held(s.home, s.port); pushed {
				t.Fatal("a malformed push was held")
			}
		})
	}
}

func TestShinoqueRelayKeyPushMustMatchDiscovery(t *testing.T) {
	s := newRelayKeyTest(t)
	other, _ := strconv.Atoi(s.port)
	for name, body := range map[string]string{
		"other home": fmt.Sprintf(`{"key":%q,"home":%q,"port":%s}`, relayKeyTestKey, t.TempDir(), s.port),
		"other port": fmt.Sprintf(`{"key":%q,"home":%q,"port":%d}`, relayKeyTestKey, s.home, other%65535+1),
	} {
		t.Run(name, func(t *testing.T) {
			a := s.server()
			if w := relayKeyServe(a, relayKeyPushRequest(body)); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "discovery_mismatch") {
				t.Fatalf("push = %d %q, want 409", w.Code, w.Body.String())
			}
		})
	}
	t.Run("no discovery", func(t *testing.T) {
		if err := os.Remove(s.discovery); err != nil {
			t.Fatal(err)
		}
		if w := relayKeyServe(s.server(), relayKeyPushRequest(s.body(relayKeyTestKey))); w.Code != http.StatusConflict {
			t.Fatalf("push without discovery = %d, want 409", w.Code)
		}
	})
}

func TestShinoqueRelayKeySignsForwardsWithPushedKeyOnly(t *testing.T) {
	s := newRelayKeyTest(t)
	a := s.server()

	// An older core pushes nothing: the file still signs, as before.
	if w := relayKeyForward(a, ""); w.Code != http.StatusOK || s.core.last(t).Get("X-Shinoque-Relay") != relayKeyTestFile {
		t.Fatalf("legacy forward = %d", w.Code)
	}
	if body := relayKeyHealth(t, a); body["shinoqueRelay"] != true || body["shinoqueRelayKey"] != false {
		t.Fatalf("legacy health = %v/%v", body["shinoqueRelay"], body["shinoqueRelayKey"])
	}

	s.core.setKey(relayKeyTestKey)
	s.push(t, a, relayKeyTestKey)
	if w := relayKeyForward(a, ""); w.Code != http.StatusOK || s.core.last(t).Get("X-Shinoque-Relay") != relayKeyTestKey {
		t.Fatalf("pushed-key forward = %d", w.Code)
	}
	if w := relayKeyForward(a, relayKeyTestDevice); w.Code != http.StatusOK ||
		s.core.last(t).Get("X-Shinoque-Relay") != shinoquePrivateDeviceProof(relayKeyTestKey, relayKeyTestDevice) {
		t.Fatalf("device forward = %d", w.Code)
	}
	if body := relayKeyHealth(t, a); body["shinoqueRelay"] != true || body["shinoqueRelayKey"] != true {
		t.Fatalf("pushed health = %v/%v", body["shinoqueRelay"], body["shinoqueRelayKey"])
	}

	// A rewritten file is never read again in this process.
	path := filepath.Join(s.home, "relay-token")
	if err := os.WriteFile(path, []byte("rewritten-file-token"), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(path, later, later)
	if w := relayKeyForward(a, ""); w.Code != http.StatusOK || s.core.last(t).Get("X-Shinoque-Relay") != relayKeyTestKey {
		t.Fatalf("forward after file rewrite = %d", w.Code)
	}
	if a.shinoqueTokens.token != "" {
		t.Fatal("the file token stayed in memory beside the pushed key")
	}

	// A new push replaces the key.
	s.clock = s.clock.Add(10 * time.Second)
	s.core.setKey(relayKeyTestWrong)
	s.push(t, a, relayKeyTestWrong)
	if w := relayKeyForward(a, ""); w.Code != http.StatusOK || s.core.last(t).Get("X-Shinoque-Relay") != relayKeyTestWrong {
		t.Fatalf("forward after replacement = %d", w.Code)
	}
}

func TestShinoqueRelayKeyWrongKeyIsRejectedByCore(t *testing.T) {
	s := newRelayKeyTest(t)
	s.core.setKey(relayKeyTestKey)
	a := s.server()
	s.push(t, a, relayKeyTestWrong)
	for _, device := range []string{"", relayKeyTestDevice} {
		if w := relayKeyForward(a, device); w.Code != http.StatusForbidden {
			t.Fatalf("forward with a wrong pushed key (device %q) = %d, want the core's 403", device, w.Code)
		}
	}
}

// @secure: a proof for a device id solud never enrolled is never made. Only
// the private channel's authenticated device is signed; a client's claim on
// the readable relay gets the plain key, never a device proof.
func TestShinoqueRelayKeyNeverSignsUnenrolledDevice(t *testing.T) {
	s := newRelayKeyTest(t)
	a := s.server()
	s.push(t, a, relayKeyTestKey)
	forged := deviceID("never-enrolled")
	r := httptest.NewRequest(http.MethodGet, "/shinoque/api/threads", nil)
	r.Header.Set("X-Forwarded-Prefix", "/m/m1")
	r.Header.Set("X-Shinoque-Private-Device", forged)
	w := httptest.NewRecorder()
	markRelayTransport(newShinoqueRelayHandler(http.NotFoundHandler(), a.shinoqueTokens, nil)).ServeHTTP(w, r)
	got := s.core.last(t)
	if w.Code != http.StatusOK || len(got.Values("X-Shinoque-Private-Device")) != 0 ||
		got.Get("X-Shinoque-Relay") == shinoquePrivateDeviceProof(relayKeyTestKey, forged) {
		t.Fatalf("an unenrolled device claim was signed: %d %v", w.Code, got)
	}
}

// K1: the relay header is set only by the tunnel and private-channel
// handlers. The local listener, including routes a router forwards from
// /b/<backend>, never reaches a core with it, even while a key is held.
func TestShinoqueRelayKeyNeverSignsLocalListener(t *testing.T) {
	s := newRelayKeyTest(t)
	a := s.server()
	s.push(t, a, relayKeyTestKey)
	local := denyLocalShinoque(newShinoqueRelayHandler(http.NotFoundHandler(), a.shinoqueTokens, nil))
	for _, prefix := range []string{"", "/b/wsl", "/b/win", "/m/m1", "/m/m1/b/wsl"} {
		for _, path := range []string{"/shinoque", "/shinoque/", "/shinoque/api/threads"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.RemoteAddr = "127.0.0.1:41000"
			if prefix != "" {
				r.Header.Set("X-Forwarded-Prefix", prefix)
			}
			w := httptest.NewRecorder()
			local.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Errorf("local %s with prefix %q = %d, want 404", path, prefix, w.Code)
			}
		}
	}
	if s.core.hits() != 0 {
		t.Fatalf("the local listener reached the core %d times", s.core.hits())
	}
}

func TestShinoqueRelayKeyFailsClosedWithoutPush(t *testing.T) {
	expectClosed := func(t *testing.T, s *relayKeyTest, a *apiServer, why string) {
		t.Helper()
		before := s.core.hits()
		for _, device := range []string{"", relayKeyTestDevice} {
			w := relayKeyForward(a, device)
			if w.Code != http.StatusServiceUnavailable || w.Body.String() != shinoqueUnavailable+"\n" {
				t.Fatalf("%s: forward = %d %q, want 503 %q", why, w.Code, w.Body.String(), shinoqueUnavailable)
			}
		}
		if s.core.hits() != before {
			t.Fatalf("%s: a forward reached the core", why)
		}
		if body := relayKeyHealth(t, a); body["shinoqueRelay"] != false || body["shinoqueRelayKey"] != false {
			t.Fatalf("%s: health = %v/%v", why, body["shinoqueRelay"], body["shinoqueRelayKey"])
		}
	}

	t.Run("restart on a marked home", func(t *testing.T) {
		s := newRelayKeyTest(t)
		s.core.setKey(relayKeyTestKey)
		first := s.server()
		s.push(t, first, relayKeyTestKey)
		relayKeyMark(t, s.home)
		if w := relayKeyForward(first, ""); w.Code != http.StatusOK {
			t.Fatalf("forward before restart = %d", w.Code)
		}
		// The restarted process has no key; the readable file is still
		// there and must not be used.
		restarted := s.server()
		expectClosed(t, s, restarted, "restart")
		s.push(t, restarted, relayKeyTestKey)
		if w := relayKeyForward(restarted, ""); w.Code != http.StatusOK || s.core.last(t).Get("X-Shinoque-Relay") != relayKeyTestKey {
			t.Fatalf("forward after re-push = %d", w.Code)
		}
	})

	t.Run("pushed in this process, then the core moved port", func(t *testing.T) {
		s := newRelayKeyTest(t)
		a := s.server()
		s.push(t, a, relayKeyTestKey)
		// The core restarts on another port and has not pushed yet. The
		// home is unmarked and the file is present: the sticky rule alone
		// keeps the file closed.
		moved := httptest.NewServer(s.core)
		defer moved.Close()
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(moved.URL, "http://"))
		n, _ := strconv.Atoi(port)
		data, _ := json.Marshal(map[string]any{"version": 1, "home": s.home, "port": n, "relayAuth": 2, "privateDeviceProof": 1})
		if err := os.WriteFile(s.discovery, data, 0600); err != nil {
			t.Fatal(err)
		}
		expectClosed(t, s, a, "moved port")
	})

	for name, mark := range map[string]func(t *testing.T, home string){
		"marker file": relayKeyMark,
		"empty marker": func(t *testing.T, home string) {
			_ = os.WriteFile(filepath.Join(home, shinoqueRelayKeyMarker), nil, 0600)
		},
		"marker dir": func(t *testing.T, home string) { _ = os.Mkdir(filepath.Join(home, shinoqueRelayKeyMarker), 0700) },
		"dangling link": func(t *testing.T, home string) {
			_ = os.Symlink("/nonexistent", filepath.Join(home, shinoqueRelayKeyMarker))
		},
	} {
		t.Run("never pushed, "+name, func(t *testing.T) {
			s := newRelayKeyTest(t)
			mark(t, s.home)
			expectClosed(t, s, s.server(), name)
		})
	}

	t.Run("never pushed, discovery says relayKey", func(t *testing.T) {
		s := newRelayKeyTest(t)
		n, _ := strconv.Atoi(s.port)
		data, _ := json.Marshal(map[string]any{"version": 1, "home": s.home, "port": n, "relayAuth": 2, "privateDeviceProof": 1, "relayKey": 1})
		if err := os.WriteFile(s.discovery, data, 0600); err != nil {
			t.Fatal(err)
		}
		expectClosed(t, s, s.server(), "relayKey discovery")
	})
}

func TestShinoqueRelayKeyRateLimit(t *testing.T) {
	s := newRelayKeyTest(t)
	a := s.server()
	for i := 0; i < shinoqueRelayKeyBurst; i++ {
		s.push(t, a, relayKeyTestKey)
	}
	w := relayKeyServe(a, relayKeyPushRequest(s.body(relayKeyTestKey)))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("push over the burst = %d Retry-After=%q %q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	s.clock = s.clock.Add(shinoqueRelayKeyRefill)
	s.push(t, a, relayKeyTestKey)

	// The core's 10 s cadence never meets the limit.
	for i := 0; i < 30; i++ {
		s.clock = s.clock.Add(10 * time.Second)
		s.push(t, a, relayKeyTestKey)
	}

	// Refused requests spend the bucket too.
	b := s.server()
	for i := 0; i < shinoqueRelayKeyBurst; i++ {
		if w := relayKeyServe(b, relayKeyPushRequest(`{}`)); w.Code != http.StatusBadRequest {
			t.Fatalf("bad push %d = %d", i, w.Code)
		}
	}
	if w := relayKeyServe(b, relayKeyPushRequest(s.body(relayKeyTestKey))); w.Code != http.StatusTooManyRequests {
		t.Fatalf("push after refused burst = %d, want 429", w.Code)
	}
}

func TestShinoqueRelayKeyLogLines(t *testing.T) {
	s := newRelayKeyTest(t)
	logs := captureRelayKeyLog(t)
	a := s.server()
	fp := relayKeyFingerprint(relayKeyTestKey)
	want := []string{fmt.Sprintf(`shinoque relay key: push pid=4242 exe="%s" fp=%s`, relayKeyTestExe, fp)}
	check := func(step string) {
		t.Helper()
		if got := logs.lines("shinoque relay key:"); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s: log lines\n%s\nwant\n%s", step, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	s.push(t, a, relayKeyTestKey)
	check("first push")
	for i := 0; i < 3; i++ {
		s.clock = s.clock.Add(10 * time.Second)
		s.push(t, a, relayKeyTestKey)
	}
	check("identical re-pushes")
	s.clock = s.clock.Add(shinoqueRelayKeySummary)
	s.push(t, a, relayKeyTestKey)
	want = append(want, fmt.Sprintf(`shinoque relay key: 4 unchanged re-pushes pid=4242 exe="%s" fp=%s`, relayKeyTestExe, fp))
	check("hourly summary")

	// The same key from another process: a full line, so a stray push and
	// the core's next one both show.
	s.clock = s.clock.Add(10 * time.Second)
	shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return 777, "/tmp/other", true }
	s.push(t, a, relayKeyTestKey)
	want = append(want, fmt.Sprintf(`shinoque relay key: push pid=777 exe="/tmp/other" fp=%s`, fp))
	s.clock = s.clock.Add(10 * time.Second)
	shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return 4242, relayKeyTestExe, true }
	s.push(t, a, relayKeyTestKey)
	want = append(want, fmt.Sprintf(`shinoque relay key: push pid=4242 exe="%s" fp=%s`, relayKeyTestExe, fp))
	check("caller moved")

	s.clock = s.clock.Add(10 * time.Second)
	s.push(t, a, relayKeyTestWrong)
	want = append(want, fmt.Sprintf(`shinoque relay key: push pid=4242 exe="%s" fp=%s`, relayKeyTestExe, relayKeyFingerprint(relayKeyTestWrong)))
	check("new key")

	// Refusals: one line per reason a minute.
	for i := 0; i < 2; i++ {
		s.clock = s.clock.Add(5 * time.Second)
		relayKeyServe(a, relayKeyPushRequest(`{}`))
	}
	want = append(want, fmt.Sprintf(`shinoque relay key: refused reason=bad_body pid=4242 exe="%s"`, relayKeyTestExe))
	check("refusals")
}

// The key never appears in a log line, a response, the environment or a file,
// across pushes, forwards, refusals, fail-closed answers and health.
func TestShinoqueRelayKeyNeverLeaks(t *testing.T) {
	s := newRelayKeyTest(t)
	logs := captureRelayKeyLog(t)
	s.core.setKey(relayKeyTestKey)
	var responses []string
	record := func(w *httptest.ResponseRecorder) {
		var b strings.Builder
		_ = w.Header().Write(&b)
		responses = append(responses, b.String(), w.Body.String())
	}
	serveLogged := func(a *apiServer, r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		logMiddleware(http.HandlerFunc(a.shinoqueRelayKeyPush)).ServeHTTP(w, r)
		record(w)
		return w
	}

	relayKeyMark(t, s.home)
	a := s.server()
	record(relayKeyForward(a, ""))
	if w := serveLogged(a, relayKeyPushRequest(s.body(relayKeyTestKey))); w.Code != http.StatusOK {
		t.Fatalf("push = %d", w.Code)
	}
	for _, device := range []string{"", relayKeyTestDevice} {
		record(relayKeyForward(a, device))
	}
	for _, r := range []*http.Request{
		relayKeyPushRequest(fmt.Sprintf(`{"key":%q,"home":%q,"port":%s,"extra":1}`, relayKeyTestKey, s.home, s.port)),
		relayKeyPushRequest(fmt.Sprintf(`{"key":%q,"home":"/elsewhere","port":%s}`, relayKeyTestKey, s.port)),
	} {
		serveLogged(a, r)
	}
	wrongAuth := relayKeyPushRequest(s.body(relayKeyTestKey))
	wrongAuth.Header.Set("Authorization", "Bearer "+relayKeyTestKey)
	serveLogged(a, wrongAuth)
	restarted := s.server()
	record(relayKeyForward(restarted, ""))
	health := httptest.NewRecorder()
	a.health(health, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	record(health)

	if strings.Contains(logs.String(), "POST "+shinoqueRelayKeyPath+" 200") {
		t.Fatal("the access log recorded a successful push")
	}
	if !strings.Contains(logs.String(), "POST "+shinoqueRelayKeyPath+" 400") {
		t.Fatal("the access log dropped a refused push")
	}
	for _, needle := range []string{relayKeyTestKey, strings.ToUpper(relayKeyTestKey), relayKeyTestKey[:16]} {
		if strings.Contains(logs.String(), needle) {
			t.Fatalf("the key reached the log:\n%s", logs.String())
		}
		for _, response := range responses {
			if strings.Contains(response, needle) {
				t.Fatalf("the key reached a response: %q", response)
			}
		}
		for _, env := range os.Environ() {
			if strings.Contains(env, needle) {
				t.Fatal("the key reached the environment")
			}
		}
		for _, root := range []string{s.home, filepath.Dir(s.discovery)} {
			_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(needle)) {
					t.Fatalf("the key reached %s", path)
				}
				return nil
			})
		}
	}
	for _, response := range responses {
		if strings.Contains(response, relayKeyFingerprint(relayKeyTestKey)) {
			t.Fatalf("a response carried the key fingerprint: %q", response)
		}
	}
}

func TestShinoqueRelayKeyMarker(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "file")
	if err := os.WriteFile(notDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		home   string
		marked bool
	}{
		{"absent", dir, false},
		{"missing home", filepath.Join(dir, "missing"), false},
		{"no home", "", true},
		// Any error other than a confirmed absence counts as marked.
		{"home is a file", notDir, true},
	} {
		if got := shinoqueRelayKeyMarked(tc.home); got != tc.marked {
			t.Errorf("%s: marked = %t, want %t", tc.name, got, tc.marked)
		}
	}
}
