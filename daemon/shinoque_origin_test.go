package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShinoqueRelayServingCloudOrigins(t *testing.T) {
	const app = "https://app.soluterminal.com"
	const relay = "wss://soluterminal-cloud.onrender.com/relay"
	const serving = "https://soluterminal-cloud.onrender.com"
	const private = "https://private.shinoque.invalid"
	shinoqueTestToken(t, "relay-token")
	seen := make(chan http.Header, 1)
	base, tokens, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/files/write" {
			http.NotFound(w, r)
			return
		}
		select {
		case seen <- r.Header.Clone():
		default:
			http.Error(w, "unexpected extra upstream request", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer closeServers()
	for _, tc := range []struct {
		name, app, relay, origin, site string
		referers                       []string
		private, offline, allowed      bool
	}{
		{name: "serving onrender", app: app, relay: relay, origin: serving, allowed: true},
		{name: "another onrender", app: app, relay: relay, origin: "https://another.onrender.com"},
		{name: "onrender suffix attack", app: app, relay: relay, origin: serving + ".evil.test"},
		{name: "public app", app: app, relay: relay, origin: app, allowed: true},
		{name: "canonical relay config", relay: "wss://SOLUTERMINAL-CLOUD.ONRENDER.COM:443/relay", origin: serving, allowed: true},
		{name: "canonical public config", app: "https://APP.SOLUTERMINAL.COM:443/", origin: app, allowed: true},
		{name: "noncanonical request port", relay: relay, origin: serving + ":443"},
		{name: "noncanonical request host", relay: relay, origin: "https://SOLUTERMINAL-CLOUD.ONRENDER.COM"},
		{name: "self hosted", relay: "wss://self.test:9443/relay", origin: "https://self.test:9443", allowed: true},
		{name: "self hosted wrong port", relay: "wss://self.test:9443/relay", origin: "https://self.test"},
		{name: "self hosted no default app fallback", relay: "wss://self.test/relay", origin: app},
		{name: "self hosted no provider fallback", relay: "wss://self.test/relay", origin: serving},
		{name: "public app without connection", app: app, origin: app, allowed: true},
		{name: "offline provider", app: app, relay: relay, origin: serving, offline: true},
		{name: "offline public app", app: app, relay: relay, origin: app, offline: true, allowed: true},
		{name: "no config fails closed", origin: serving},
		{name: "no config no origin", allowed: true},
		{name: "no origin still checks fetch metadata", site: "cross-site"},
		{name: "ws IPv4 loopback", relay: "ws://127.0.0.1:8080/relay", origin: "http://127.0.0.1:8080", allowed: true},
		{name: "ws IPv6 loopback", relay: "ws://[::1]:8080/relay", origin: "http://[::1]:8080", allowed: true},
		{name: "ws localhost", relay: "ws://LOCALHOST:8080/relay", origin: "http://localhost:8080", allowed: true},
		{name: "ws default port", relay: "ws://localhost:80/relay", origin: "http://localhost", allowed: true},
		{name: "ws exact loopback host", relay: "ws://127.0.0.1:8080/relay", origin: "http://localhost:8080"},
		{name: "ws public host refused", relay: "ws://self.test/relay", origin: "http://self.test"},
		{name: "ws public host https refused", relay: "ws://self.test/relay", origin: "https://self.test"},
		{name: "ws other loopback refused", relay: "ws://127.0.0.2/relay", origin: "http://127.0.0.2"},
		{name: "ws localhost suffix refused", relay: "ws://evil.localhost/relay", origin: "http://evil.localhost"},
		{name: "insecure public app alone refused", app: "http://localhost:8080", origin: "http://localhost:8080"},
		{name: "wss loopback requires https", relay: "wss://localhost/relay", origin: "http://localhost"},
		{name: "wss loopback https", relay: "wss://localhost/relay", origin: "https://localhost", allowed: true},
		{name: "invalid relay scheme", relay: "https://self.test/relay", origin: "https://self.test"},
		{name: "relay userinfo refused", relay: "wss://user@self.test/relay", origin: "https://self.test"},
		{name: "relay invalid port", relay: "wss://self.test:65536/relay", origin: "https://self.test:65536"},
		{name: "private app refused", app: app, relay: relay, origin: app, private: true},
		{name: "private serving origin refused", app: app, relay: relay, origin: serving, private: true},
		{name: "private dev origin refused", relay: "ws://localhost/relay", origin: "http://localhost", private: true},
		{name: "private virtual origin", app: app, relay: relay, origin: private, private: true, allowed: true},
		{name: "forged private claim on readable relay", app: app, relay: relay, origin: private},
		{name: "provider referer canonicalized", relay: relay, origin: serving, referers: []string{"https://SOLUTERMINAL-CLOUD.ONRENDER.COM:443/m/test"}, allowed: true},
		{name: "provider referer must match provider", app: app, relay: relay, origin: serving, referers: []string{app + "/m/test"}},
		{name: "app referer must match app", app: app, relay: relay, origin: app, referers: []string{serving + "/m/test"}},
		{name: "empty referer refused", app: app, origin: app, referers: []string{""}},
		{name: "duplicate referers refused", app: app, origin: app, referers: []string{app + "/", app + "/"}},
		{name: "ws referer", relay: "ws://localhost:8080/relay", origin: "http://localhost:8080", referers: []string{"http://LOCALHOST:8080/m/test"}, allowed: true},
		{name: "private referer", origin: private, private: true, referers: []string{private + "/home"}, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shinoqueTestPublicAppURL(t, tc.app)
			sup := &relaySupervisor{relayURL: tc.relay, state: cloudStatus{RelayState: relayStateOnline}}
			if tc.offline || tc.relay == "" {
				sup.state.RelayState = relayStateOffline
			}
			req := httptest.NewRequest(http.MethodPost, base+"/shinoque/api/files/write", nil)
			req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			req.Header.Set("Sec-Fetch-Site", tc.site)
			// A client must not select either an allowed host or a channel.
			req.Host = strings.TrimPrefix(strings.TrimPrefix(tc.origin, "https://"), "http://")
			req.Header.Set("X-Forwarded-Host", req.Host)
			req.Header.Set("Forwarded", "host="+req.Host)
			req.Header.Set("X-Shinoque-Private-Device", deviceID("forged-device"))
			if tc.referers != nil {
				req.Header["Referer"] = tc.referers
			}
			if tc.private {
				req = req.WithContext(withPrivateRelayDevice(req.Context(), deviceID("approved-device")))
			}
			w := httptest.NewRecorder()
			markRelayTransport(newShinoqueRelayHandler(http.NotFoundHandler(), tokens, sup.activeRelayURL)).ServeHTTP(w, req)
			want := http.StatusForbidden
			if tc.allowed {
				want = http.StatusNoContent
			}
			if w.Code != want {
				t.Fatalf("response = %d, want %d: %s", w.Code, want, w.Body.String())
			}
			select {
			case headers := <-seen:
				if !tc.allowed {
					t.Fatal("refused origin reached upstream")
				}
				if headers.Get("Origin") != "" || headers.Get("Sec-Fetch-Site") != "" {
					t.Fatal("browser authority headers reached upstream")
				}
			default:
				if tc.allowed {
					t.Fatal("allowed origin did not reach upstream")
				}
			}
		})
	}
}

func TestShinoqueRelayOriginTracksActiveConnection(t *testing.T) {
	shinoqueTestPublicAppURL(t, "")
	shinoqueTestToken(t, "relay-token")
	base, tokens, closeServers := shinoqueTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer closeServers()
	sup := &relaySupervisor{}
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), tokens, sup.activeRelayURL)
	for _, step := range []struct {
		name, relay, state, origin string
		allowed                    bool
	}{
		{"first relay connecting", "wss://first.test/relay", relayStateConnecting, "https://first.test", false},
		{"first relay online", "wss://first.test/relay", relayStateOnline, "https://first.test", true},
		{"second relay rejects first origin", "wss://second.test/relay", relayStateOnline, "https://first.test", false},
		{"second relay accepts second origin", "wss://second.test/relay", relayStateOnline, "https://second.test", true},
		{"disconnected relay", "wss://second.test/relay", relayStateOffline, "https://second.test", false},
	} {
		t.Run(step.name, func(t *testing.T) {
			sup.mu.Lock()
			sup.relayURL, sup.state.RelayState = step.relay, step.state
			sup.mu.Unlock()
			req := httptest.NewRequest(http.MethodPost, base+"/shinoque/api/files/write", nil)
			req.Header.Set("X-Forwarded-Prefix", "/m/test")
			req.Header.Set("Origin", step.origin)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			want := http.StatusForbidden
			if step.allowed {
				want = http.StatusNoContent
			}
			if w.Code != want {
				t.Fatalf("response = %d, want %d", w.Code, want)
			}
		})
	}
}
