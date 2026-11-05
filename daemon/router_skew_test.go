package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The pre-update router rewrote Host and stamped only service-preview routes.
// ReverseProxy itself supplies X-Forwarded-For from its accepted connection.
func oldRouterTestProxy(target *url.URL, prefix, token string) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(target)
	director := p.Director
	p.Director = func(r *http.Request) {
		director(r)
		r.Host = target.Host
		if prefix != "" {
			if r.URL.Path == prefix {
				r.URL.Path = "/"
			} else if strings.HasPrefix(r.URL.Path, prefix+"/") {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
			}
		}
		r.Header.Del("X-Forwarded-Prefix")
		r.Header.Del("X-Solu-Proxy-Auth")
		if strings.HasPrefix(r.URL.Path, "/api/proxy/") {
			r.Header.Set("X-Forwarded-Prefix", prefix)
			r.Header.Set("X-Solu-Proxy-Auth", "router")
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return p
}

func TestLegacyRouterUpdateWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name, method, origin, referer, site, bearer, marker string
		socket, relay, cookie, duplicate, emptyOrigin       bool
		want                                                bool
	}{
		{name: "write", method: "POST", origin: legacyRouterOrigin, want: true},
		{name: "read", method: "GET", origin: legacyRouterOrigin, want: true},
		{name: "read referer", method: "GET", referer: legacyRouterOrigin + "/?solu_router=1", want: true},
		{name: "head referer", method: "HEAD", referer: legacyRouterOrigin + "/", want: true},
		{name: "socket", method: "GET", origin: legacyRouterOrigin, socket: true, want: true},
		{name: "write requires origin", method: "POST", referer: legacyRouterOrigin + "/"},
		{name: "socket requires origin", method: "GET", referer: legacyRouterOrigin + "/", socket: true},
		{name: "another port", method: "POST", origin: "http://127.0.0.1:28788"},
		{name: "another referer port", method: "GET", referer: "http://127.0.0.1:28788/"},
		{name: "same site refused", method: "POST", origin: legacyRouterOrigin, site: "same-site"},
		{name: "cross site refused", method: "POST", origin: legacyRouterOrigin, site: "cross-site"},
		{name: "opaque never falls back", method: "GET", origin: "null", referer: legacyRouterOrigin + "/"},
		{name: "malformed never falls back", method: "GET", origin: "://invalid", referer: legacyRouterOrigin + "/"},
		{name: "empty never falls back", method: "GET", referer: legacyRouterOrigin + "/", emptyOrigin: true},
		{name: "origin wins", method: "GET", origin: "http://127.0.0.1:28788", referer: legacyRouterOrigin + "/"},
		{name: "duplicate origin", method: "POST", origin: legacyRouterOrigin, duplicate: true},
		{name: "duplicate referer", method: "GET", referer: legacyRouterOrigin + "/", duplicate: true},
		{name: "wrong bearer", method: "POST", origin: legacyRouterOrigin, bearer: "wrong"},
		{name: "cookie is not router bearer", method: "POST", origin: legacyRouterOrigin, cookie: true},
		{name: "current marker cannot qualify", method: "POST", origin: legacyRouterOrigin, marker: "router"},
		{name: "external marker cannot qualify", method: "POST", origin: legacyRouterOrigin, marker: "forged"},
		{name: "relay cannot qualify", method: "POST", origin: legacyRouterOrigin, relay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &authGate{token: "test-backend"}
			mux := http.NewServeMux()
			reached := false
			mux.HandleFunc("/api/compatibility", func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if tc.socket && !upgrader.CheckOrigin(r) {
					t.Error("socket lost authenticated compatibility decision")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			a := &apiServer{gate: gate}
			var h http.Handler = gate.middleware(a.apiHandler(mux))
			if tc.relay {
				h = markRelayTransport(h)
			}
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:41001/api/compatibility", strings.NewReader(`{}`))
			if tc.origin != "" || tc.emptyOrigin {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("Referer", tc.referer)
			site := tc.site
			if site == "" {
				site = "same-origin"
			}
			r.Header.Set("Sec-Fetch-Site", site)
			bearer := tc.bearer
			if bearer == "" {
				bearer = "test-backend"
			}
			if tc.cookie {
				r.AddCookie(&http.Cookie{Name: tokenCookie, Value: "test-backend"})
			} else {
				r.Header.Set("Authorization", "Bearer "+bearer)
			}
			r.Header.Set("X-Solu-Proxy-Auth", tc.marker)
			if tc.duplicate {
				if tc.origin != "" {
					r.Header.Add("Origin", tc.origin)
				} else {
					r.Header.Add("Referer", tc.referer)
				}
			}
			if tc.socket {
				r.Header.Set("Upgrade", "websocket")
				r.Header.Set("Connection", "Upgrade")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := http.StatusForbidden
			var count uint64
			if tc.want {
				want, count = http.StatusNoContent, 1
			}
			if w.Code != want || reached != tc.want || gate.routerSkewRequests.Load() != count {
				t.Fatalf("status=%d reached=%v count=%d", w.Code, reached, gate.routerSkewRequests.Load())
			}
			if got := w.Header().Get("X-Solu-Router-Restart") == "required"; got != tc.want {
				t.Fatalf("restart notice=%v", got)
			}
		})
	}
}

func TestLegacyRouterHealthCounter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &apiServer{gate: &authGate{token: "test-backend"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	h := a.gate.middleware(a.apiHandler(mux))
	for _, legacy := range []bool{true, false} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:41001/api/health?identity=1", nil)
		r.Header.Set("Authorization", "Bearer test-backend")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if legacy {
			r.Header.Set("Referer", legacyRouterOrigin+"/")
		} else {
			r.Header.Set("Origin", "http://127.0.0.1:41001")
			r.Header.Set("X-Solu-Proxy-Auth", "router")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var body map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["routerSkewRequests"] != float64(1) {
			t.Fatalf("legacy=%v status=%d body=%s", legacy, w.Code, w.Body.String())
		}
		if (w.Header().Get("X-Solu-Router-Restart") == "required") != legacy {
			t.Fatal("restart notice did not reflect the current router")
		}
	}
}

func TestLegacyRouterSocketHandoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &apiServer{gate: &authGate{token: "test-backend"}}
	mux := http.NewServeMux()
	for _, path := range []string{"/api/panes/example/io", "/api/panes/example/native/ws"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err == nil {
				conn.Close()
			}
		})
	}
	backend := httptest.NewServer(a.gate.middleware(a.apiHandler(mux)))
	defer backend.Close()
	for _, path := range []string{"/api/panes/example/io", "/api/panes/example/native/ws"} {
		headers := http.Header{"Authorization": {"Bearer test-backend"}, "Origin": {legacyRouterOrigin}, "Sec-Fetch-Site": {"same-origin"}}
		conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(backend.URL, "http")+path, headers)
		if err != nil {
			if resp != nil {
				resp.Body.Close()
			}
			t.Fatalf("%s handshake failed: %v", path, err)
		}
		conn.Close()
	}
	if a.gate.routerSkewRequests.Load() != 2 {
		t.Fatalf("socket compatibility count=%d", a.gate.routerSkewRequests.Load())
	}
}

func TestServiceRestartRouterRequiresSignedPerson(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubPeerPID(t, 0, false)
	for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
		for _, tc := range []struct {
			name, host, origin, marker string
			direct                     bool
		}{
			{"preserved", "127.0.0.1:28787", legacyRouterOrigin, "router", false},
			{"fixed without marker", "127.0.0.1:28787", legacyRouterOrigin, "", false},
			{"byte proxy", "127.0.0.1:18787", "http://127.0.0.1:18787", "", false},
			{"custom preserved", "127.0.0.1:41001", "http://127.0.0.1:41001", "router", false},
			{"old rewritten", "127.0.0.1:41001", legacyRouterOrigin, "", false},
			{"direct bearer unchanged", "127.0.0.1:41001", "http://127.0.0.1:41001", "", true},
		} {
			for _, signed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/signed=%v", mode, tc.name, signed), func(t *testing.T) {
					a := humanProofTestServer(t, mode)
					a.backendID = "wsl"
					a.gate = &authGate{token: "test-backend"}
					a.serviceRestartGate = new(atomic.Bool)
					var stops atomic.Int32
					a.stopService = func() { stops.Add(1) }
					a.inspectService = func() (serviceRestartProof, error) { return serviceRestartProof{"wsl-supervisor", "test-owned"}, nil }
					device := newTestDevice(t)
					approveTestDevice(t, a, device)
					body := fmt.Sprintf(`{"pid":%d}`, os.Getpid())
					target := "http://" + tc.host + "/api/service/restart"
					r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
					if signed {
						r = device.localRequest(t, a, http.MethodPost, target, body, time.Now(), newNonce(t))
					}
					r.RemoteAddr = "127.0.0.1:41003"
					r.Header.Set("Origin", tc.origin)
					r.Header.Set("Sec-Fetch-Site", "same-origin")
					r.Header.Set("Sec-Fetch-Mode", "cors")
					r.Header.Set("Authorization", "Bearer test-backend")
					if tc.marker != "" {
						r.Header.Set("X-Solu-Proxy-Auth", tc.marker)
					}
					mux := http.NewServeMux()
					a.routes(mux)
					w := httptest.NewRecorder()
					a.gate.middleware(a.localDeviceProof(a.apiHandler(mux))).ServeHTTP(w, r)
					want := http.StatusForbidden
					var calls int32
					if signed || (tc.direct && mode != humanProofRequire) {
						want, calls = http.StatusAccepted, 1
					}
					if w.Code != want || stops.Load() != calls {
						t.Fatalf("status=%d stops=%d body=%s", w.Code, stops.Load(), w.Body.String())
					}
				})
			}
		}
	}
}

func TestServiceRestartOldRouterMetadata(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubPeerPID(t, 0, false)
	for _, mode := range []string{humanProofOff, humanProofReport} {
		t.Run(mode, func(t *testing.T) {
			a := humanProofTestServer(t, mode)
			a.backendID = "wsl"
			a.gate = &authGate{token: "test-backend"}
			a.serviceRestartGate = new(atomic.Bool)
			var stops atomic.Int32
			a.stopService = func() { stops.Add(1) }
			a.inspectService = func() (serviceRestartProof, error) { return serviceRestartProof{"wsl-supervisor", "test-owned"}, nil }
			device := newTestDevice(t)
			approveTestDevice(t, a, device)
			mux := http.NewServeMux()
			a.routes(mux)
			h := a.gate.middleware(a.localDeviceProof(a.apiHandler(mux)))
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Forwarded-For") == "" {
					t.Error("old proxy did not append forwarding metadata")
				}
				h.ServeHTTP(w, r)
			}))
			defer backend.Close()
			target, _ := url.Parse(backend.URL)
			proxy := oldRouterTestProxy(target, "", "test-backend")
			// Reproduce the old hostname-only guard and Host-rewriting proxy.
			frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				source, err := url.Parse(r.Header.Get("Origin"))
				if err != nil || source.Hostname() != "127.0.0.1" || !safeLoopbackHost(r.Host) {
					http.Error(w, "old router guard", http.StatusForbidden)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer frontend.Close()
			body := fmt.Sprintf(`{"pid":%d}`, os.Getpid())
			for _, connection := range []string{"", "X-Forwarded-For"} {
				for _, signed := range []bool{false, true} {
					r, _ := http.NewRequest(http.MethodPost, frontend.URL+"/api/service/restart", strings.NewReader(body))
					if signed {
						r = device.localRequest(t, a, http.MethodPost, frontend.URL+"/api/service/restart", body, time.Now(), newNonce(t))
					}
					r.RequestURI = "" // Convert the signed server fixture to a client request.
					r.Header.Set("Origin", backend.URL)
					r.Header.Set("Sec-Fetch-Site", "same-origin")
					r.Header.Set("Connection", connection)
					r.Header.Set("X-Forwarded-For", "")
					resp, err := http.DefaultClient.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					want := http.StatusForbidden
					if signed {
						want = http.StatusAccepted
					}
					if resp.StatusCode != want {
						t.Fatalf("signed=%v connection=%q status=%d", signed, connection, resp.StatusCode)
					}
					a.serviceRestartGate.Store(false)
				}
			}
			if stops.Load() != 2 {
				t.Fatalf("restart dispatches=%d want 2 signed requests", stops.Load())
			}
		})
	}
}

func TestLegacyRouterPreviewRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("service received an owner credential")
		}
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, "window.previewReady=true;")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>preview ready</p>")
	}))
	defer upstream.Close()
	port := webTestPort(t, upstream.URL)
	a := &apiServer{gate: &authGate{token: "test-backend"}, ports: &PortRegistry{seen: map[string]*Port{"owned": port}}}
	t.Cleanup(func() {
		for _, transport := range a.ports.transports {
			transport.CloseIdleConnections()
		}
	})
	mux := http.NewServeMux()
	a.routes(mux)
	backend := httptest.NewServer(a.gate.middleware(a.apiHandler(mux)))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	for _, prefix := range []string{"/b/wsl", "/b/win"} {
		for _, old := range []bool{true, false} {
			var proxy http.Handler = oldRouterTestProxy(target, prefix, "test-backend")
			if !old {
				p := newPrefixProxy(target, prefix, "test-backend")
				proxy = routerGuard(routerPreviewHandler(p, p))
			}
			frontend := httptest.NewServer(proxy)
			for _, kind := range []string{"document", "script", "fetch"} {
				method, suffix := http.MethodGet, "/page"
				if kind == "script" {
					suffix = "/app.js"
				}
				if kind == "fetch" {
					method = http.MethodPost
				}
				r, _ := http.NewRequest(method, fmt.Sprintf("%s%s/api/proxy/%d%s", frontend.URL, prefix, port.Port, suffix), strings.NewReader(`{}`))
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set("Sec-Fetch-Mode", "cors")
				r.Header.Set("Referer", frontend.URL+"/")
				if kind == "document" {
					r.Header.Set("Referer", legacyRouterOrigin+"/")
					r.Header.Set("Sec-Fetch-Site", "same-site")
					r.Header.Set("Sec-Fetch-Mode", "navigate")
					r.Header.Set("Sec-Fetch-Dest", "iframe")
				}
				if kind == "script" {
					r.Header.Set("Sec-Fetch-Dest", "script")
					r.Header.Set("Sec-Fetch-Mode", "no-cors")
				}
				if kind == "fetch" {
					r.Header.Set("Origin", frontend.URL)
				}
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				want := http.StatusOK
				if old && kind != "document" {
					want = http.StatusForbidden
				}
				if resp.StatusCode != want {
					t.Fatalf("old=%v %s %s status=%d want=%d", old, prefix, kind, resp.StatusCode, want)
				}
			}
			frontend.Close()
		}
	}
}
