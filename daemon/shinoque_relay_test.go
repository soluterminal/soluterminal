package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func shinoqueTestToken(t *testing.T, token string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SHINOQUE_HOME", home)
	if token != "" {
		if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte(token+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func shinoqueTestProxy(t *testing.T, upstream http.Handler) (string, *shinoqueTokenCache, func()) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	shinoqueTestDiscovery(t, os.Getenv("SHINOQUE_HOME"), port)
	tokens := &shinoqueTokenCache{}
	proxy := httptest.NewServer(newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil))
	return proxy.URL, tokens, func() { proxy.Close(); server.Close() }
}

func shinoqueTestDiscovery(t *testing.T, home, port string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("relay capabilities require Linux discovery")
	}
	for _, key := range []string{"SHINOQUE_HOME", "SHINOQUE_RELAY_PORT"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(os.Getenv("XDG_STATE_HOME"), "shinoque", "relay.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	data, err := json.Marshal(map[string]any{"version": 1, "home": home, "port": n, "relayAuth": 2, "privateDeviceProof": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func shinoqueTestPublicAppURL(t *testing.T, origin string) {
	t.Helper()
	previous := publicAppURL
	publicAppURL = origin
	t.Cleanup(func() { publicAppURL = previous })
}

func TestShinoqueRelayHeadersAndPrefixes(t *testing.T) {
	shinoqueTestPublicAppURL(t, "https://cloud.test")
	shinoqueTestToken(t, "server-token")
	seen := make(chan *http.Request, 2)
	base, tokens, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
		w.Header().Set("Set-Cookie", "bad=1; Path=/")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	defer closeServers()
	for _, prefix := range []string{"/m/machine_1", "/m/machine_1/b/wsl"} {
		req, _ := http.NewRequest(http.MethodPost, base+"/shinoque/api/write?x=1&access_token=jwt", strings.NewReader("body"))
		req.Header.Set("X-Forwarded-Prefix", prefix)
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		req.Header.Set("X-Forwarded-Host", "forged.test")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Shinoque-Relay", "forged")
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Cookie", "solu_session=secret")
		req.Header.Set("Origin", "https://cloud.test")
		req.Header.Set("Referer", "https://cloud.test/page")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Sec-Fetch-Future", "untrusted")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Last-Event-ID", "42")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated || string(body) != "ok" {
			t.Fatalf("response = %d %q", resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Security-Policy") != "sandbox allow-scripts" || resp.Header.Get("X-Upstream") != "yes" || resp.Header.Get("Set-Cookie") != "" {
			t.Fatalf("response headers = %v", resp.Header)
		}
		config, _ := tokens.discovery.resolve()
		got := <-seen
		if got.Method != http.MethodPost || got.Host != "127.0.0.1:"+config.port || got.URL.Path != "/api/write" || got.URL.RawQuery != "x=1" {
			t.Fatalf("request = %s %s host=%s query=%q", got.Method, got.URL.Path, got.Host, got.URL.RawQuery)
		}
		if got.Header.Get("X-Forwarded-Prefix") != prefix+"/shinoque" || got.Header.Get("X-Shinoque-Relay") != "server-token" {
			t.Fatalf("relay headers = %v", got.Header)
		}
		for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Authorization", "Cookie", "Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User", "Sec-Fetch-Future"} {
			if got.Header.Get(name) != "" {
				t.Errorf("forwarded %s", name)
			}
		}
		for name, want := range map[string]string{"Referer": "https://cloud.test/page", "Accept": "text/event-stream", "Last-Event-Id": "42"} {
			if got.Header.Get(name) != want {
				t.Errorf("%s = %q, want %q", name, got.Header.Get(name), want)
			}
		}
	}
}

func TestShinoqueRelayPrivateDeviceHeader(t *testing.T) {
	// Shared byte for byte with Shinoque production.test.ts on
	// test/private-device-vector at ee31f8e3, through the real Rewrite path.
	for _, vector := range []struct{ name, token, proof string }{
		{"fixture key", "private-relay-fixture", "aa393a1e81259f25ae7887a28f04a8a53a1d09d89d094afc86b98762bd1299db"},
		{"literal hex key", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "2c961468cdb9589ed0846501f24dbb6fa62f5f68881fbaf4c5a5ce4cabca15a9"},
	} {
		t.Run(vector.name, func(t *testing.T) {
			const channelDevice = "dev_b71SY8ikOq3NEn-6MV-j9z"
			token, channelProof := vector.token, vector.proof
			home := shinoqueTestToken(t, token)
			seen := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			_, port, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			shinoqueTestDiscovery(t, home, port)
			proxy := newShinoqueRelayHandler(http.NotFoundHandler(), &shinoqueTokenCache{}, nil)
			forgedDevice := deviceID("forged-device")
			for _, tc := range []struct {
				name          string
				relay         bool
				privateDevice string
				clientDevices []string
				refuse        bool
			}{
				{name: "no channel"},
				{name: "no channel with forged header", clientDevices: []string{forgedDevice}},
				{name: "readable relay", relay: true},
				{name: "readable relay with forged header", relay: true, clientDevices: []string{forgedDevice}},
				{name: "readable relay with duplicate headers", relay: true, clientDevices: []string{forgedDevice, channelDevice}},
				{name: "private channel", relay: true, privateDevice: channelDevice},
				{name: "private channel with forged header", relay: true, privateDevice: channelDevice, clientDevices: []string{forgedDevice}},
				{name: "private channel with duplicate headers", relay: true, privateDevice: channelDevice, clientDevices: []string{forgedDevice, "invalid"}},
				{name: "short channel id", relay: true, privateDevice: "dev_short", refuse: true},
				{name: "long channel id", relay: true, privateDevice: channelDevice + "x", refuse: true},
				{name: "wrong channel id prefix", relay: true, privateDevice: "DEV_" + strings.TrimPrefix(channelDevice, "dev_"), refuse: true},
				{name: "channel id newline", relay: true, privateDevice: channelDevice + "\n", refuse: true},
				{name: "channel id punctuation", relay: true, privateDevice: "dev_" + strings.Repeat("!", 22), clientDevices: []string{forgedDevice}, refuse: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodPost, "/shinoque/api/files/write", nil)
					req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
					req.Header.Set("X-Shinoque-Relay", "forged-proof")
					for _, device := range tc.clientDevices {
						req.Header.Add("X-Shinoque-Private-Device", device)
					}
					if tc.privateDevice != "" {
						req = req.WithContext(withPrivateRelayDevice(req.Context(), tc.privateDevice))
					}
					handler := proxy
					if tc.relay {
						handler = markRelayTransport(handler)
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, req)
					if tc.refuse {
						if w.Code != http.StatusForbidden {
							t.Fatalf("invalid channel id response = %d, want 403", w.Code)
						}
						select {
						case <-seen:
							t.Fatal("invalid channel id reached upstream")
						default:
						}
						return
					}
					if w.Code != http.StatusNoContent {
						t.Fatalf("proxy response = %d: %s", w.Code, w.Body.String())
					}
					headers := <-seen
					devices := headers.Values("X-Shinoque-Private-Device")
					if tc.privateDevice == "" {
						if len(devices) != 0 {
							t.Fatalf("device claim escaped without private channel: %q", devices)
						}
					} else if len(devices) != 1 || devices[0] != tc.privateDevice {
						t.Fatalf("private device = %q, want only %q", devices, tc.privateDevice)
					}
					wantRelay := token
					if tc.privateDevice != "" {
						wantRelay = channelProof
					}
					if headers.Get("X-Shinoque-Relay") != wantRelay || headers.Get("X-Forwarded-Prefix") != "/m/test/b/wsl/shinoque" {
						t.Fatal("device identity lost the relay authentication contract")
					}
				})
			}
		})
	}
}

func TestShinoqueRelayCannotConferSiblingHumanAuthority(t *testing.T) {
	shinoqueTestToken(t, "relay-token")
	sibling := authorityTestServer(t)
	// Model a peer daemon outside this daemon's process tree, and WSL's
	// untraceable Windows forwarding path. Both accept browser metadata.
	previousBrowser, previousProxy := peerIsBrowser, peerIsRelayProxy
	peerIsBrowser = func(int) bool { return false }
	peerIsRelayProxy = func(int) bool { return true }
	t.Cleanup(func() { peerIsBrowser, peerIsRelayProxy = previousBrowser, previousProxy })
	for _, traced := range []bool{false, true} {
		t.Run(fmt.Sprintf("traced=%t", traced), func(t *testing.T) {
			shinoqueTestToken(t, "relay-token")
			stubPeerPID(t, 99999999, traced)
			base, tokens, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if sibling.requireHuman(w, r, "change sibling settings") {
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer closeServers()
			config, _ := tokens.discovery.resolve()
			shinoqueTestPublicAppURL(t, "https://127.0.0.1:"+config.port)
			for _, fetchSite := range []string{"same-origin", ""} {
				req, err := http.NewRequest(http.MethodPost, base+"/shinoque/api/settings", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Forwarded-Prefix", "/m/test")
				req.Header.Set("Sec-Fetch-Site", fetchSite)
				// Match the rewritten Host to exercise the old-WebView Origin
				// fallback as well as Sec-Fetch-Site in browserOriginated.
				req.Header.Set("Origin", "https://127.0.0.1:"+config.port)
				probe := req.Clone(req.Context())
				probe.Host = "127.0.0.1:" + config.port
				if sibling.identifyCaller(probe).Kind != callerHuman {
					t.Fatal("fixture does not confer browser authority before stripping")
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("sibling accepted relayed browser authority: %d", resp.StatusCode)
				}
			}
		})
	}
}

func TestShinoqueRelayChecksBrowserHeadersBeforeStripping(t *testing.T) {
	shinoqueTestPublicAppURL(t, "https://cloud.test")
	shinoqueTestToken(t, "relay-token")
	base, tokens, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.Header {
			if strings.EqualFold(name, "Origin") || strings.HasPrefix(strings.ToLower(name), "sec-fetch-") {
				t.Errorf("browser authority header reached upstream: %s", name)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer closeServers()
	for _, tc := range []struct {
		name, method, path, site, mode, dest, origin, referer, upgrade string
		privateDevice, cloudOrigin                                     string
		origins                                                        []string
		allowed                                                        bool
	}{
		{name: "token only", method: "POST", path: "/api/write", allowed: true},
		{name: "same origin", method: "POST", path: "/api/write", site: "same-origin", origin: "https://cloud.test", referer: "https://cloud.test/page", allowed: true},
		{name: "cross-site API", method: "POST", path: "/api/write", site: "cross-site"},
		{name: "same-site API", method: "GET", path: "/api/read", site: "same-site"},
		{name: "API navigation", method: "GET", path: "/api", site: "cross-site", mode: "navigate", dest: "document"},
		{name: "API path navigation", method: "GET", path: "/api/read", site: "cross-site", mode: "navigate", dest: "document"},
		{name: "page navigation", method: "GET", path: "/home", site: "cross-site", mode: "navigate", dest: "document", allowed: true},
		{name: "HEAD navigation", method: "HEAD", path: "/", site: "same-site", mode: "navigate", allowed: true},
		{name: "iframe", method: "GET", path: "/", site: "cross-site", mode: "navigate", dest: "iframe"},
		{name: "subresource", method: "GET", path: "/", site: "cross-site", mode: "no-cors", dest: "document"},
		{name: "POST navigation", method: "POST", path: "/", site: "cross-site", mode: "navigate", dest: "document"},
		{name: "cross-site websocket", method: "GET", path: "/ws", site: "cross-site", mode: "navigate", dest: "document", upgrade: "websocket"},
		{name: "same origin websocket", method: "GET", path: "/ws", site: "same-origin", origin: "https://cloud.test", upgrade: "websocket", allowed: true},
		{name: "http origin", method: "POST", path: "/api/write", origin: "http://cloud.test"},
		{name: "null origin", method: "POST", path: "/api/write", origin: "null"},
		{name: "malformed origin", method: "POST", path: "/api/write", origin: "https://%"},
		{name: "origin path", method: "POST", path: "/api/write", origin: "https://cloud.test/path"},
		{name: "origin credentials", method: "POST", path: "/api/write", origin: "https://user@cloud.test"},
		{name: "other referer", method: "POST", path: "/api/write", origin: "https://cloud.test", referer: "https://other.test/"},
		{name: "malformed referer", method: "POST", path: "/api/write", origin: "https://cloud.test", referer: "https://%"},
		{name: "default referer port", method: "POST", path: "/api/write", origin: "https://cloud.test", referer: "https://CLOUD.test:443/page", allowed: true},
		{name: "other https origin", method: "POST", path: "/api/write", site: "same-origin", origin: "https://other.test"},
		{name: "cloud subdomain", method: "POST", path: "/api/write", origin: "https://preview.cloud.test"},
		{name: "cloud lookalike", method: "POST", path: "/api/write", origin: "https://cloud.test.evil.test"},
		{name: "cloud port", method: "POST", path: "/api/write", origin: "https://cloud.test:8443"},
		{name: "duplicate origins", method: "POST", path: "/api/write", origins: []string{"https://cloud.test", "https://other.test"}},
		{name: "duplicate identical origins", method: "POST", path: "/api/write", origins: []string{"https://cloud.test", "https://cloud.test"}},
		{name: "empty origin", method: "POST", path: "/api/write", origins: []string{""}},
		{name: "empty first origin", method: "POST", path: "/api/write", origins: []string{"", "https://cloud.test"}},
		{name: "empty last origin", method: "POST", path: "/api/write", origins: []string{"https://cloud.test", ""}},
		{name: "configured public origin", method: "POST", path: "/api/write", origin: "https://custom.test:8443", cloudOrigin: "https://custom.test:8443", allowed: true},
		{name: "old public origin", method: "POST", path: "/api/write", origin: "https://cloud.test", cloudOrigin: "https://custom.test"},
		{name: "private origin on readable relay", method: "POST", path: "/api/write", origin: "https://private.shinoque.invalid"},
		{name: "private origin", method: "POST", path: "/api/write", origin: "https://private.shinoque.invalid", privateDevice: deviceID("approved-device"), allowed: true},
		{name: "private no origin", method: "POST", path: "/api/write", privateDevice: deviceID("approved-device"), allowed: true},
		{name: "public origin on private channel", method: "POST", path: "/api/write", origin: "https://cloud.test", privateDevice: deviceID("approved-device")},
		{name: "other private origin", method: "POST", path: "/api/write", origin: "https://other.test", privateDevice: deviceID("approved-device")},
		{name: "private origin port", method: "POST", path: "/api/write", origin: "https://private.shinoque.invalid:8443", privateDevice: deviceID("approved-device")},
		{name: "private websocket", method: "GET", path: "/ws", site: "same-origin", origin: "https://private.shinoque.invalid", privateDevice: deviceID("approved-device"), upgrade: "websocket", allowed: true},
		{name: "private bad referer", method: "POST", path: "/api/write", origin: "https://private.shinoque.invalid", referer: "https://cloud.test/", privateDevice: deviceID("approved-device")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cloudOrigin != "" {
				shinoqueTestPublicAppURL(t, tc.cloudOrigin)
			}
			req, err := http.NewRequest(tc.method, base+"/shinoque"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Forwarded-Prefix", "/m/test")
			// Client claims must neither select the private origin nor replace
			// the build's hosted app origin with a forwarded host.
			req.Header.Set("X-Shinoque-Private-Device", deviceID("forged-device"))
			req.Header.Set("X-Forwarded-Host", strings.TrimPrefix(tc.origin, "https://"))
			if tc.privateDevice != "" {
				req = req.WithContext(withPrivateRelayDevice(req.Context(), tc.privateDevice))
			}
			for name, value := range map[string]string{
				"Sec-Fetch-Site": tc.site, "Sec-Fetch-Mode": tc.mode, "Sec-Fetch-Dest": tc.dest,
				"Origin": tc.origin, "Referer": tc.referer, "Upgrade": tc.upgrade,
			} {
				if value != "" {
					req.Header.Set(name, value)
				}
			}
			if tc.upgrade != "" {
				req.Header.Set("Connection", "Upgrade")
			}
			if tc.origins != nil {
				req.Header["Origin"] = tc.origins
			}
			w := httptest.NewRecorder()
			markRelayTransport(newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)).ServeHTTP(w, req)
			want := http.StatusForbidden
			if tc.allowed {
				want = http.StatusNoContent
			}
			if w.Code != want {
				t.Fatalf("response = %d, want %d", w.Code, want)
			}
		})
	}
}

func TestShinoqueRelayOnlyAndAvailability(t *testing.T) {
	home := shinoqueTestToken(t, "")
	hits := 0
	base, tokens, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.WriteHeader(200) }))
	defer closeServers()
	req, _ := http.NewRequest(http.MethodGet, base+"/shinoque/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/m/m1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(body), shinoqueUnavailable) || hits != 0 {
		t.Fatalf("missing token: %d %q hits=%d", resp.StatusCode, body, hits)
	}
	if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if tokens.read() != "first" {
		t.Fatal("token did not appear")
	}
	path := filepath.Join(home, "relay-token")
	if err := os.WriteFile(path, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	nextTime := time.Now().Add(time.Second)
	if err := os.Chtimes(path, nextTime, nextTime); err != nil {
		t.Fatal(err)
	}
	if tokens.read() != "rotated" {
		t.Fatal("token rotation not observed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if tokens.read() != "" {
		t.Fatal("removed token still available")
	}

	local := denyLocalShinoque(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	for _, path := range []string{"/shinoque", "/shinoque/", "/shinoque/ws"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Forwarded-Prefix", "/m/m1")
		r.Header.Set("X-Shinoque-Relay", "forged")
		w := httptest.NewRecorder()
		local.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("local %s = %d", path, w.Code)
		}
	}
	bad := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
	bad.Header.Set("X-Forwarded-Prefix", "/m/m1/evil")
	w := httptest.NewRecorder()
	newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil).ServeHTTP(w, bad)
	if w.Code != 400 {
		t.Fatalf("invalid prefix = %d", w.Code)
	}
}

func TestShinoqueRelaySSEFlush(t *testing.T) {
	shinoqueTestToken(t, "stream-token")
	release := make(chan struct{})
	base, _, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = fmt.Fprint(w, "data: second\n\n")
	}))
	defer closeServers()
	defer close(release)
	req, _ := http.NewRequest(http.MethodGet, base+"/shinoque/events", nil)
	req.Header.Set("X-Forwarded-Prefix", "/m/m1")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first SSE event = %q, %v", line, err)
	}
}

func TestShinoqueRelayWebSocket(t *testing.T) {
	shinoqueTestPublicAppURL(t, "https://cloud.test")
	shinoqueTestToken(t, "ws-token")
	base, _, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket" || r.Header.Get("X-Shinoque-Relay") != "ws-token" {
			http.Error(w, "bad upgrade", 400)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		kind, msg, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(kind, msg)
		}
	}))
	defer closeServers()
	header := http.Header{"X-Forwarded-Prefix": []string{"/m/m1/b/wsl"}, "Origin": []string{"https://cloud.test"}}
	conn, resp, err := websocket.DefaultDialer.Dial(strings.Replace(base, "http://", "ws://", 1)+"/shinoque/socket", header)
	if err != nil {
		t.Fatalf("websocket dial: %v response=%v", err, resp)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil || string(msg) != "hello" {
		t.Fatalf("echo = %q, %v", msg, err)
	}
}

func TestShinoqueHealthCapability(t *testing.T) {
	home := shinoqueTestToken(t, "")
	a := &apiServer{shinoqueTokens: &shinoqueTokenCache{}}
	for _, present := range []bool{false, true} {
		if present {
			if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("token"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		w := httptest.NewRecorder()
		a.health(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["shinoqueRelay"] != present {
			t.Fatalf("capability = %v, want %t", body["shinoqueRelay"], present)
		}
	}
}
