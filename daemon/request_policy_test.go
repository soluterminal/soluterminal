package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRelayPreviewRouteCompatibility(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOLU_PREVIEWS", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Soluterminal-Preview-Proxy") != "" {
			t.Error("preview received daemon authority")
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>preview</p>")
	}))
	defer upstream.Close()
	port := webTestPort(t, upstream.URL)
	m := previewTestManager(t)
	now := time.Now().UTC()
	m.targets[daemonPreviewTestID] = &PreviewTarget{ID: daemonPreviewTestID, Mode: previewModeAdopt, TargetPort: port.Port, State: previewStateRunning, CreatedAt: now, LastActivityAt: now, ExpiresAt: now.Add(time.Hour)}
	a := &apiServer{gate: &authGate{token: "test-master"}, ports: &PortRegistry{seen: map[string]*Port{"owned": port}}, previews: m}
	t.Cleanup(func() {
		for _, transport := range a.ports.transports {
			transport.CloseIdleConnections()
		}
	})
	mux := http.NewServeMux()
	a.routes(mux)
	proxyPath := fmt.Sprintf("/api/proxy/%d/page", port.Port)
	previewPath := "/api/preview-serve/" + daemonPreviewTestID + "/page"
	for _, tc := range []struct {
		path, prefix string
		public       bool
	}{
		{proxyPath, "/m/test-machine/b/wsl", false},
		{previewPath, "/m/test-machine", false},
		{previewPath, "/preview/" + daemonPreviewTestID, true},
	} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			for _, source := range []string{"null", "https://preview.example"} {
				r := httptest.NewRequest(method, "http://daemon.preview.invalid"+tc.path, strings.NewReader(`{}`))
				r.Header.Set("Origin", source)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				r.Header.Set("X-Forwarded-Prefix", tc.prefix)
				if method == "OPTIONS" {
					r.Header.Set("Access-Control-Request-Method", "POST")
				}
				w := httptest.NewRecorder()
				markRelayTransport(a.apiHandler(mux)).ServeHTTP(w, r)
				want := http.StatusOK
				if method == "OPTIONS" && tc.path == proxyPath {
					want = http.StatusNoContent
				}
				if w.Code != want {
					t.Fatalf("relay %s %s %s = %d: %s", method, tc.prefix, source, w.Code, w.Body.String())
				}
				if tc.path == proxyPath && w.Header().Get("Access-Control-Allow-Origin") != "null" {
					t.Fatal("opaque CORS contract lost")
				}
				if !tc.public && !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox") && method != "OPTIONS" {
					t.Fatal("preview sandbox lost")
				}
				// Identical headers over the direct listener confer no relay authority.
				w = httptest.NewRecorder()
				a.gate.middleware(a.apiHandler(mux)).ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Fatalf("direct preview accepted relay headers: %d", w.Code)
				}
			}
		}
		// Public share links may navigate here with cross-site metadata.
		if tc.public {
			r := httptest.NewRequest(http.MethodGet, "http://daemon.preview.invalid"+tc.path, nil)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
			r.Header.Set("X-Forwarded-Prefix", tc.prefix)
			w := httptest.NewRecorder()
			markRelayTransport(a.apiHandler(mux)).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("share navigation status=%d", w.Code)
			}
		}
	}
	// The exception does not extend to app APIs on the same relay transport.
	r := httptest.NewRequest(http.MethodPost, "http://daemon.preview.invalid/api/file/line", strings.NewReader(`{}`))
	r.Header.Set("Origin", "null")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("X-Forwarded-Prefix", "/m/test-machine")
	w := httptest.NewRecorder()
	markRelayTransport(a.apiHandler(mux)).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "cross-origin request rejected") {
		t.Fatal("preview exception reached app API")
	}
}

func TestRouterDiagnosticRefusal(t *testing.T) {
	upstreamCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { upstreamCalls++; w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	for _, prefix := range []string{"", "/b/wsl", "/b/win"} {
		h := routerAppGuard(routerGuard(newPrefixProxy(target, prefix, "test-master")))
		for _, path := range []string{"/debug", "/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/trace"} {
			for _, method := range []string{"GET", "POST", "HEAD"} {
				r := httptest.NewRequest(method, "http://127.0.0.1:28787"+prefix+path, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusNotFound {
					t.Fatalf("%s %s status=%d", method, prefix+path, w.Code)
				}
			}
		}
	}
	if upstreamCalls != 0 {
		t.Fatal("router forwarded diagnostics")
	}
}

func TestRouterPreviewNavigation(t *testing.T) {
	gate := &authGate{token: "test-backend"}
	backend := httptest.NewServer(gate.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	for _, prefix := range []string{"/b/wsl", "/b/win"} {
		proxy := newPrefixProxy(target, prefix, "test-backend")
		h := routerGuard(routerPreviewHandler(proxy, proxy))
		for _, dest := range []string{"iframe", "frame", "document", "script"} {
			for _, site := range []string{"same-site", "same-origin", "cross-site"} {
				for _, suffix := range []string{"/api/proxy/41004/page", "/api/file/line"} {
					r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:41001"+prefix+suffix, nil)
					r.Header.Set("Referer", "http://127.0.0.1:28787/")
					r.Header.Set("Sec-Fetch-Mode", "navigate")
					r.Header.Set("Sec-Fetch-Dest", dest)
					r.Header.Set("Sec-Fetch-Site", site)
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					want := http.StatusForbidden
					if suffix == "/api/proxy/41004/page" && dest != "script" && site != "cross-site" {
						want = http.StatusNoContent
					}
					if w.Code != want {
						t.Fatalf("%s %s %s %s status=%d want=%d", prefix, suffix, dest, site, w.Code, want)
					}
				}
			}
		}
	}
}

func TestLoopbackEmbedParents(t *testing.T) {
	for _, raw := range []string{"https://extra.example", "http://192.0.2.1:4700", "http://user@localhost:4700", "http://localhost:*/", "http://localhost:4700/path"} {
		if loopbackEmbedOrigin(raw) != "" {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:4700", "https://localhost:4700", "http://[::1]:4700", "http://window.shinoque.localhost:47800"} {
		if loopbackEmbedOrigin(raw) == "" {
			t.Errorf("refused %s", raw)
		}
	}
}
