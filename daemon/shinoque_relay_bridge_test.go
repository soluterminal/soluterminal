package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	bridgeTestKey      = "c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff"
	bridgeTestCorePort = 4760
	bridgeTestDevice   = "dev_b71SY8ikOq3NEn-6MV-j9z"
	bridgeTestImage    = `C:\Users\tester\AppData\Local\Shinoque\Shinoque.exe`
)

// The vectors Shinoque's core pins too (@lead confirmed both, 2026-11-10).
func TestShinoqueRelayV2Vector(t *testing.T) {
	key := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	for _, c := range []struct{ device, want string }{
		{"", "v2.1794300000.AAECAwQFBgcICQoLDA0ODw.5VLvb8SJ_iflMln2_WReXNDuHVqKcG7JqPJ_bm_4Dzc"},
		{"dev_b71SY8ikOq3NEn-6MV-j9z", "v2.1794300000.AAECAwQFBgcICQoLDA0ODw.gurbKcFwE5izgzFh5wPOH6Q7snOIA6ZPQB8DaCzmN7Q"},
	} {
		got := shinoqueRelayV2Value(key, 1794300000, nonce, "GET", "/api/mail?q=a%2Fb&x=1", "/m/mach_1/b/host/shinoque", c.device)
		if got != c.want {
			t.Errorf("device %q: got %s, want %s", c.device, got, c.want)
		}
	}
	if got := shinoqueRelayV2Message("1", "n", "GET", "/", "/m/a/shinoque", ""); got != "shinoque-relay-v2\n1\nn\nGET\n/\n/m/a/shinoque\n" {
		t.Fatalf("message = %q", got)
	}
}

var bridgeTestV2 = regexp.MustCompile(`^v2\.([1-9][0-9]{9,10})\.([A-Za-z0-9_-]{22})\.([A-Za-z0-9_-]{43})$`)

// bridgeTestCore stands in for the desktop proxy and the core behind it: it
// verifies v2 as Shinoque's core does and records each forward.
type bridgeTestCore struct {
	clock func() time.Time
	date  atomic.Pointer[time.Time] // reply Date override, for skew
	// noProof answers without a valid proof: on route checks, or on others.
	noCheckProof, noProof atomic.Bool
	checks                atomic.Int32

	mu      sync.Mutex
	seen    map[string]bool
	headers []http.Header
	targets []string
	refused []string
}

func (c *bridgeTestCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The core's clock is the test clock, not the wall clock.
	date := c.clock()
	if override := c.date.Load(); override != nil {
		date = *override
	}
	w.Header().Set("Date", date.UTC().Format(http.TimeFormat))
	c.mu.Lock()
	defer c.mu.Unlock()
	check := r.Header.Get("X-Forwarded-Prefix") == shinoqueRelayBridgeProbePrefix
	if check {
		c.checks.Add(1)
	} else {
		c.headers = append(c.headers, r.Header.Clone())
		c.targets = append(c.targets, r.RequestURI)
	}
	refuse := func(reason string) {
		c.refused = append(c.refused, reason)
		http.Error(w, reason, http.StatusUnauthorized)
	}
	values := r.Header.Values(shinoqueRelayV2Header)
	if len(values) != 1 || len(r.Header.Values("X-Shinoque-Relay")) != 0 {
		refuse("shape")
		return
	}
	if r.Host != "127.0.0.1:"+strconv.Itoa(bridgeTestCorePort) {
		refuse("host")
		return
	}
	m := bridgeTestV2.FindStringSubmatch(values[0])
	if m == nil {
		refuse("shape")
		return
	}
	ts, _ := strconv.ParseInt(m[1], 10, 64)
	if d := c.clock().Unix() - ts; d > 60 || d < -60 {
		refuse("relay_skew")
		return
	}
	if c.seen[m[2]] {
		refuse("replay")
		return
	}
	c.seen[m[2]] = true
	mac := hmac.New(sha256.New, []byte(bridgeTestKey))
	mac.Write([]byte(shinoqueRelayV2Message(m[1], m[2], r.Method, r.RequestURI,
		r.Header.Get("X-Forwarded-Prefix"), r.Header.Get("X-Shinoque-Private-Device"))))
	if !hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), []byte(m[3])) {
		refuse("mac")
		return
	}
	if check && r.URL.Path != "/api/health" {
		refuse("check path")
		return
	}
	proof := shinoqueRelayV2Proof(bridgeTestKey, m[1], m[2], http.StatusOK)
	if (check && c.noCheckProof.Load()) || (!check && c.noProof.Load()) {
		proof = shinoqueRelayV2Proof(bridgeTestKey, m[1], m[2], http.StatusTeapot)
	}
	w.Header().Set(shinoqueRelayProofHeader, proof)
	w.WriteHeader(http.StatusOK)
	if check {
		w.Write([]byte(`{"ok":true}`))
	}
}

func (c *bridgeTestCore) count() (forwards, refusals int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.headers), len(c.refused)
}

type bridgeTest struct {
	core      *bridgeTestCore
	proxyPort int
	api       *apiServer
	bridge    *shinoqueRelayBridge
	clock     atomic.Int64 // unix nanos
	dials     atomic.Int32
	owner     atomic.Pointer[shinoqueRelayOwner]
	ownerFail atomic.Pointer[string]
}

func (s *bridgeTest) now() time.Time { return time.Unix(0, s.clock.Load()) }

func (s *bridgeTest) advance(d time.Duration) { s.clock.Add(int64(d)) }

// newBridgeTest is a host-backend solud whose push peer and dialed owner are
// the verified desktop unless a test says otherwise.
func newBridgeTest(t *testing.T) *bridgeTest {
	t.Helper()
	s := &bridgeTest{}
	s.clock.Store(time.Unix(1_800_000_000, 0).UnixNano())
	s.core = &bridgeTestCore{clock: s.now, seen: map[string]bool{}}
	server := httptest.NewServer(s.core)
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	s.proxyPort, _ = strconv.Atoi(port)
	s.owner.Store(&shinoqueRelayOwner{pid: 5150, created: 1, image: bridgeTestImage})

	bridge := newShinoqueRelayBridge()
	bridge.now = s.now
	bridge.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		s.dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	bridge.ownerOf = func(net.Conn) (shinoqueRelayOwner, string) {
		if reason := s.ownerFail.Load(); reason != nil {
			return shinoqueRelayOwner{}, *reason
		}
		return *s.owner.Load(), ""
	}
	t.Cleanup(bridge.transport.CloseIdleConnections)
	s.bridge = bridge
	tokens := &shinoqueTokenCache{}
	tokens.discovery.bridge = bridge
	tokens.pushedKey.now = s.now
	s.api = &apiServer{backendID: shinoqueRelayBridgeHost, shinoqueTokens: tokens}

	restoreClock := shinoqueRelayV2Now
	shinoqueRelayV2Now = s.now
	t.Cleanup(func() { shinoqueRelayV2Now = restoreClock })
	previousPeer := shinoqueRelayKeyPeer
	shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return 5150, bridgeTestImage, true }
	t.Cleanup(func() { shinoqueRelayKeyPeer = previousPeer })
	return s
}

func (s *bridgeTest) body(relayAuth int) string {
	return fmt.Sprintf(`{"key":%q,"home":"/home/tester/.shinoque","port":%d,"proxyPort":%d,"relayAuth":%d,"privateDeviceProof":1}`,
		bridgeTestKey, bridgeTestCorePort, s.proxyPort, relayAuth)
}

// bridgePushRequest is the desktop carrying the core's push: loopback, the
// listener's own Host, no token and no browser headers.
func bridgePushRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, shinoqueRelayKeyPath, strings.NewReader(body))
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8787}
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
	r.RemoteAddr = "127.0.0.1:41000"
	r.Host = local.String()
	r.Header.Set("Content-Type", "application/json")
	return r
}

func (s *bridgeTest) pushWith(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.api.shinoqueRelayKeyPush(w, r)
	return w
}

func (s *bridgeTest) push(t *testing.T) {
	t.Helper()
	if w := s.pushWith(bridgePushRequest(s.body(2))); w.Code != http.StatusOK || w.Body.String() != "{\"accepted\":true}\n" {
		t.Fatalf("push = %d %q", w.Code, w.Body.String())
	}
}

func (s *bridgeTest) forward(target, device string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("X-Forwarded-Prefix", "/m/m1/b/host")
	r.Header.Set("X-Shinoque-Relay", "client-supplied")
	r.Header.Set(shinoqueRelayV2Header, "client-supplied")
	if device != "" {
		r = r.WithContext(withPrivateRelayDevice(r.Context(), device))
	}
	w := httptest.NewRecorder()
	markRelayTransport(newShinoqueRelayHandler(http.NotFoundHandler(), s.api.shinoqueTokens, nil)).ServeHTTP(w, r)
	return w
}

func (s *bridgeTest) healthy(t *testing.T) bool {
	t.Helper()
	w := httptest.NewRecorder()
	s.api.health(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body["shinoqueRelay"] == true
}

type bridgeTestLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *bridgeTestLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *bridgeTestLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureBridgeLog(t *testing.T) *bridgeTestLog {
	t.Helper()
	l := &bridgeTestLog{}
	previous := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(previous) })
	return l
}

func TestShinoqueRelayBridgeForwardsWithFreshV2Only(t *testing.T) {
	s := newBridgeTest(t)
	if s.healthy(t) {
		t.Fatal("health true before any push")
	}
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusServiceUnavailable || s.dials.Load() != 0 {
		t.Fatalf("forward before push = %d with %d dials", w.Code, s.dials.Load())
	}
	s.push(t)
	if !s.healthy(t) {
		t.Fatal("health false after push with a verified desktop")
	}
	for _, c := range []struct{ target, device string }{
		{"/shinoque/api/mail?q=a%2Fb&x=1", ""},
		{"/shinoque/api/mail?q=a%2Fb&x=1", ""},
		{"/shinoque/api/threads", bridgeTestDevice},
	} {
		if w := s.forward(c.target, c.device); w.Code != http.StatusOK {
			t.Fatalf("forward %s %q = %d %q", c.target, c.device, w.Code, w.Body.String())
		}
	}
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	if len(s.core.refused) != 0 || len(s.core.headers) != 3 {
		t.Fatalf("core refused %v of %d forwards", s.core.refused, len(s.core.headers))
	}
	if s.core.targets[0] != "/api/mail?q=a%2Fb&x=1" {
		t.Fatalf("target = %q", s.core.targets[0])
	}
	if s.core.headers[0].Get(shinoqueRelayV2Header) == s.core.headers[1].Get(shinoqueRelayV2Header) {
		t.Fatal("two forwards carried the same v2 value")
	}
	if got := s.core.headers[2].Get("X-Shinoque-Private-Device"); got != bridgeTestDevice {
		t.Fatalf("device header = %q", got)
	}
	for _, h := range s.core.headers {
		if h.Get("X-Forwarded-Prefix") != "/m/m1/b/host/shinoque" {
			t.Fatalf("prefix = %q", h.Get("X-Forwarded-Prefix"))
		}
		for name, values := range h {
			for _, v := range values {
				if strings.Contains(v, bridgeTestKey) {
					t.Fatalf("header %s carried the raw key", name)
				}
			}
		}
	}
}

func TestShinoqueRelayBridgePushRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(*http.Request) *http.Request
		status int
	}{
		{"origin", func(r *http.Request) *http.Request { r.Header.Set("Origin", "https://evil.example"); return r }, 403},
		{"referer", func(r *http.Request) *http.Request { r.Header.Set("Referer", "https://evil.example/"); return r }, 403},
		{"cookie", func(r *http.Request) *http.Request { r.Header.Set("Cookie", "solu_session=x"); return r }, 403},
		{"sec-fetch none", func(r *http.Request) *http.Request { r.Header.Set("Sec-Fetch-Site", "none"); return r }, 403},
		{"sec-fetch mode", func(r *http.Request) *http.Request { r.Header.Set("Sec-Fetch-Mode", "cors"); return r }, 403},
		{"forwarded", func(r *http.Request) *http.Request { r.Header.Set("X-Forwarded-For", "10.0.0.1"); return r }, 403},
		{"host", func(r *http.Request) *http.Request { r.Host = "localhost:8787"; return r }, 403},
		{"remote", func(r *http.Request) *http.Request { r.RemoteAddr = "192.168.1.5:41000"; return r }, 403},
		{"tunnel", func(r *http.Request) *http.Request {
			return r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
		}, 403},
		{"private device", func(r *http.Request) *http.Request {
			return r.WithContext(withPrivateRelayDevice(r.Context(), bridgeTestDevice))
		}, 403},
		// The desktop never attaches solud's token, so any Authorization refuses.
		{"authorization", func(r *http.Request) *http.Request { r.Header.Set("Authorization", "Bearer owner"); return r }, 403},
		{"empty authorization", func(r *http.Request) *http.Request { r.Header["Authorization"] = []string{""}; return r }, 403},
		{"signed device", func(r *http.Request) *http.Request {
			return r.WithContext(context.WithValue(r.Context(), localVerifiedDeviceKey{}, bridgeTestDevice))
		}, 403},
		{"alerts device", func(r *http.Request) *http.Request {
			return r.WithContext(context.WithValue(r.Context(), localScopedDeviceKey{}, bridgeTestDevice))
		}, 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newBridgeTest(t)
			if w := s.pushWith(c.edit(bridgePushRequest(s.body(2)))); w.Code != c.status {
				t.Fatalf("status = %d %q, want %d", w.Code, w.Body.String(), c.status)
			}
			if s.healthy(t) {
				t.Fatal("refused push made health true")
			}
		})
	}
	t.Run("untraced or unverified peer", func(t *testing.T) {
		s := newBridgeTest(t)
		shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return 0, "", false }
		if w := s.pushWith(bridgePushRequest(s.body(2))); w.Code != http.StatusForbidden {
			t.Fatalf("status = %d", w.Code)
		}
	})
	for _, backend := range []string{"win", "wsl", ""} {
		t.Run("backend "+backend, func(t *testing.T) {
			s := newBridgeTest(t)
			s.api.backendID = backend
			w := s.pushWith(bridgePushRequest(s.body(2)))
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not_host_backend") {
				t.Fatalf("status = %d %q", w.Code, w.Body.String())
			}
			if s.healthy(t) {
				t.Fatal("non-host backend reports relay health")
			}
		})
	}
	key := fmt.Sprintf("%q", bridgeTestKey)
	for name, body := range map[string]string{
		"missing proxyPort": `{"key":` + key + `,"home":"/h","port":1,"relayAuth":2,"privateDeviceProof":1}`,
		"windows home":      `{"key":` + key + `,"home":"C:\\h","port":1,"proxyPort":2,"relayAuth":2,"privateDeviceProof":1}`,
		"relative home":     `{"key":` + key + `,"home":"h","port":1,"proxyPort":2,"relayAuth":2,"privateDeviceProof":1}`,
		"unclean home":      `{"key":` + key + `,"home":"/h/../x","port":1,"proxyPort":2,"relayAuth":2,"privateDeviceProof":1}`,
		"extra field":       `{"key":` + key + `,"home":"/h","port":1,"proxyPort":2,"relayAuth":2,"privateDeviceProof":1,"x":1}`,
		"bad key":           `{"key":"ABC","home":"/h","port":1,"proxyPort":2,"relayAuth":2,"privateDeviceProof":1}`,
		"device proof 2":    `{"key":` + key + `,"home":"/h","port":1,"proxyPort":2,"relayAuth":2,"privateDeviceProof":2}`,
		"port 0":            `{"key":` + key + `,"home":"/h","port":0,"proxyPort":2,"relayAuth":2,"privateDeviceProof":1}`,
		"null":              `{"key":` + key + `,"home":"/h","port":1,"proxyPort":null,"relayAuth":2,"privateDeviceProof":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			// A fresh solud each: refusals count against the push rate limit.
			if w := newBridgeTest(t).pushWith(bridgePushRequest(body)); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", w.Code)
			}
		})
	}
}

// A dialed connection whose owner fails any check gets nothing written to
// it, and health stays false (@secure C1, C6).
func TestShinoqueRelayBridgeOwnerCheck(t *testing.T) {
	logs := captureBridgeLog(t)
	s := newBridgeTest(t)
	s.push(t)
	reason := "owner runs as another Windows user"
	s.ownerFail.Store(&reason)
	if s.healthy(t) {
		t.Fatal("health true with a refused owner")
	}
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("forward = %d", w.Code)
	}
	if forwards, _ := s.core.count(); forwards != 0 {
		t.Fatalf("%d requests reached a refused owner", forwards)
	}
	if !strings.Contains(logs.String(), reason) {
		t.Fatalf("log lacks the refusal: %s", logs)
	}
	s.ownerFail.Store(nil)
	if !s.healthy(t) {
		t.Fatal("health false once the owner checks out")
	}
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusOK {
		t.Fatalf("forward = %d", w.Code)
	}
}

// A v2 value captured off the wire and sent again, as is or with a body of
// its own, is refused by the core as a replay, and the refusal carries no
// proof solud would accept (@secure C6).
func TestShinoqueRelayBridgeReplayedV2Refused(t *testing.T) {
	s := newBridgeTest(t)
	s.push(t)
	if w := s.forward("/shinoque/api/mail?q=a%2Fb&x=1", bridgeTestDevice); w.Code != http.StatusOK {
		t.Fatalf("forward = %d", w.Code)
	}
	s.core.mu.Lock()
	captured, target := s.core.headers[0].Clone(), s.core.targets[0]
	s.core.mu.Unlock()
	v2 := captured.Get(shinoqueRelayV2Header)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	for _, body := range []string{"", `{"to":"someone@example.com"}`} {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(s.proxyPort)+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1:" + strconv.Itoa(bridgeTestCorePort)
		for _, name := range []string{"X-Forwarded-Prefix", "X-Shinoque-Private-Device", shinoqueRelayV2Header} {
			req.Header.Set(name, captured.Get(name))
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || shinoqueRelayProofValid(bridgeTestKey, v2, resp.StatusCode, resp.Header) {
			t.Fatalf("replay with body %q = %d, proof %q", body, resp.StatusCode, resp.Header.Get(shinoqueRelayProofHeader))
		}
	}
	s.core.mu.Lock()
	refused := append([]string(nil), s.core.refused...)
	s.core.mu.Unlock()
	if strings.Join(refused, ",") != "replay,replay" {
		t.Fatalf("core refused %v, want two replays", refused)
	}
}

// A new owner pid or creation time drops idle connections to the old one
// (@secure C4), and every request still gets its own v2.
func TestShinoqueRelayBridgeOwnerChangeDropsIdle(t *testing.T) {
	s := newBridgeTest(t)
	s.push(t)
	for i := 0; i < 2; i++ {
		if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusOK {
			t.Fatalf("forward = %d", w.Code)
		}
	}
	reused := s.dials.Load()
	s.owner.Store(&shinoqueRelayOwner{pid: 5150, created: 2, image: bridgeTestImage})
	s.advance(shinoqueRelayBridgeVerdictTTL + time.Second)
	if !s.healthy(t) { // the health probe dials and sees the new owner
		t.Fatal("health false")
	}
	before := s.dials.Load()
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusOK {
		t.Fatalf("forward = %d", w.Code)
	}
	if s.dials.Load() != before+1 {
		t.Fatalf("forward after an owner change reused an idle connection (dials %d → %d; first two used %d)", before, s.dials.Load(), reused)
	}
	if _, refused := s.core.count(); refused != 0 {
		t.Fatalf("core refused %d forwards", refused)
	}
}

// A core clock far from solud's holds the relay with health false and a
// logged reason, then lets traffic try again after the hold (@secure C3).
func TestShinoqueRelayBridgeClockSkew(t *testing.T) {
	logs := captureBridgeLog(t)
	s := newBridgeTest(t)
	s.push(t)
	skewed := s.now().Add(-90 * time.Second)
	s.core.date.Store(&skewed)
	s.forward("/shinoque/api/threads", "")
	if s.healthy(t) {
		t.Fatal("health true after a skewed reply")
	}
	forwards, _ := s.core.count()
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("forward during skew = %d", w.Code)
	}
	if after, _ := s.core.count(); after != forwards {
		t.Fatal("a forward went out during the skew hold")
	}
	if !strings.Contains(logs.String(), "clock skew") {
		t.Fatalf("log lacks the skew reason: %s", logs)
	}
	s.core.date.Store(nil)
	s.advance(shinoqueRelayBridgeSkewHold + time.Second)
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusOK {
		t.Fatalf("forward after the hold = %d", w.Code)
	}
	if !s.healthy(t) {
		t.Fatal("health false after the clocks agree")
	}
}

// The bridge is the only source: no env, default port or relay-token file,
// and a core without strict relay auth never gets a forward (@secure T11).
func TestShinoqueRelayBridgeNoOtherSource(t *testing.T) {
	s := newBridgeTest(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHINOQUE_HOME", home)
	t.Setenv("SHINOQUE_RELAY_PORT", strconv.Itoa(s.proxyPort))
	if s.healthy(t) || s.forward("/shinoque/api/threads", "").Code != http.StatusServiceUnavailable {
		t.Fatal("env source or relay-token file used through the bridge")
	}
	if w := s.pushWith(bridgePushRequest(s.body(0))); w.Code != http.StatusOK {
		t.Fatalf("push = %d", w.Code)
	}
	if s.healthy(t) {
		t.Fatal("health true for a core without strict relay auth")
	}
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("forward = %d", w.Code)
	}
	if forwards, _ := s.core.count(); forwards != 0 {
		t.Fatal("a forward reached a core without strict relay auth")
	}
}

func TestShinoqueRelayBridgeNeverLogsSecrets(t *testing.T) {
	logs := captureBridgeLog(t)
	s := newBridgeTest(t)
	s.push(t)
	s.forward("/shinoque/api/threads", bridgeTestDevice)
	reason := "owner is not the Shinoque app"
	s.ownerFail.Store(&reason)
	s.forward("/shinoque/api/threads", "")
	s.core.mu.Lock()
	var macs []string
	for _, h := range s.core.headers {
		if m := bridgeTestV2.FindStringSubmatch(h.Get(shinoqueRelayV2Header)); m != nil {
			macs = append(macs, m[3])
		}
	}
	s.core.mu.Unlock()
	if len(macs) == 0 {
		t.Fatal("no v2 forward to check")
	}
	out := logs.String()
	for _, secret := range append(macs, bridgeTestKey) {
		if strings.Contains(out, secret) {
			t.Fatalf("log carries a key or MAC: %s", out)
		}
	}
}

// Pushes stop when the core turns remote access off or loses the desktop;
// the relay then stops within the TTL instead of holding the key forever.
func TestShinoqueRelayBridgeKeyExpiresWithoutPushes(t *testing.T) {
	s := newBridgeTest(t)
	s.push(t)
	s.advance(shinoqueRelayBridgeKeyTTL - time.Second)
	if !s.healthy(t) {
		t.Fatal("health false within the TTL")
	}
	s.push(t)
	s.advance(shinoqueRelayBridgeKeyTTL + time.Second)
	if s.healthy(t) {
		t.Fatal("health true after pushes stopped")
	}
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("forward = %d", w.Code)
	}
	s.push(t)
	if !s.healthy(t) {
		t.Fatal("health false after pushes resumed")
	}
}

// Nothing but the route check goes on a connection until its reply proves
// the core holds the key; a failed check closes it and health stays false.
func TestShinoqueRelayBridgeRouteCheck(t *testing.T) {
	s := newBridgeTest(t)
	s.push(t)
	s.core.noCheckProof.Store(true)
	if s.healthy(t) {
		t.Fatal("health true without a proved route")
	}
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("forward = %d", w.Code)
	}
	if forwards, _ := s.core.count(); forwards != 0 {
		t.Fatalf("%d requests followed a failed route check", forwards)
	}
	if s.core.checks.Load() == 0 {
		t.Fatal("no route check was sent")
	}
	s.core.noCheckProof.Store(false)
	checks := s.core.checks.Load()
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusOK {
		t.Fatalf("forward = %d", w.Code)
	}
	if s.core.checks.Load() != checks+1 {
		t.Fatal("the new connection skipped its route check")
	}
}

// A reply without a valid proof never reaches the phone, and its connection
// is not reused.
func TestShinoqueRelayBridgeReplyProof(t *testing.T) {
	s := newBridgeTest(t)
	s.push(t)
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusOK || w.Header().Get(shinoqueRelayProofHeader) != "" {
		t.Fatalf("forward = %d, proof passed on: %q", w.Code, w.Header().Get(shinoqueRelayProofHeader))
	}
	s.core.noProof.Store(true)
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("forward with a bad proof = %d", w.Code)
	}
	s.core.noProof.Store(false)
	dials := s.dials.Load()
	if w := s.forward("/shinoque/api/threads", ""); w.Code != http.StatusOK {
		t.Fatalf("forward = %d", w.Code)
	}
	if s.dials.Load() != dials+1 {
		t.Fatal("a connection was reused after a proof failure")
	}
}

// Pushes through the desktop share ST-94's limit: a burst of six, then one
// per refill, refused pushes included.
func TestShinoqueRelayBridgePushRateLimit(t *testing.T) {
	s := newBridgeTest(t)
	for i := 0; i < shinoqueRelayKeyBurst; i++ {
		s.push(t)
	}
	w := s.pushWith(bridgePushRequest(s.body(2)))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("push past the burst = %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	s.advance(shinoqueRelayKeyRefill)
	s.push(t)
}

// Health asked first, before any forward: the probe's own route check sees
// the skew, so health is false although the route proved itself.
func TestShinoqueRelayBridgeHealthSeesSkewOnProbe(t *testing.T) {
	s := newBridgeTest(t)
	s.push(t)
	skewed := s.now().Add(45 * time.Second)
	s.core.date.Store(&skewed)
	if s.healthy(t) {
		t.Fatal("health true while the probe saw a skewed clock")
	}
	if s.core.checks.Load() != 1 {
		t.Fatalf("route checks = %d, want the probe's one", s.core.checks.Load())
	}
}
