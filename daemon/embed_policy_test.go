package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAppFrameAndDocumentPolicy(t *testing.T) {
	t.Setenv("SOLU_EMBED_PARENTS", "https://localhost:41004, http://127.0.0.1:41004 https://*.invalid https://extra.example https://bad.invalid/with-path")
	for _, parent := range append(embedParents(), "http://127.0.0.1:41002") {
		for _, path := range []string{"/", "/?embed=shinoque&theme=dark", "/assets/missing.js", "/api/panes", "/b/wsl/api/panes", "/debug/pprof/"} {
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:41001"+path, nil)
			r.RemoteAddr = "127.0.0.1:41003"
			r.Header.Set("Referer", parent+"/")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "iframe")
			r.Header.Set("Sec-Fetch-Site", "same-site")
			reached := false
			w := httptest.NewRecorder()
			(&authGate{}).middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if strings.Contains(path, "embed=") && r.URL.Query().Get("embed") != "shinoque" {
					t.Error("frame query was lost")
				}
			})).ServeHTTP(w, r)
			want := parent != "http://127.0.0.1:41002" && !strings.Contains(path, "/api/") && !strings.HasPrefix(path, "/debug/")
			if reached != want {
				t.Errorf("%s %s reached=%v want=%v", parent, path, reached, want)
			}
			csp := w.Header().Get("Content-Security-Policy")
			if !strings.HasPrefix(csp, "frame-ancestors 'self' ") || strings.Contains(csp, "*") || strings.Contains(csp, "bad.invalid") || strings.Contains(csp, "extra.example") || !strings.Contains(csp, "https://localhost:41004") {
				t.Fatalf("invalid document policy: %s", csp)
			}
		}
	}
}

func TestFrameAllowanceDoesNotAdmitAPIOrScripts(t *testing.T) {
	for _, change := range []string{"cross-site", "no metadata", "post", "api", "debug", "script", "upgrade"} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:41001/?embed=shinoque", nil)
		r.Header.Set("Referer", "http://127.0.0.1:14700/")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "iframe")
		r.Header.Set("Sec-Fetch-Site", "same-site")
		switch change {
		case "cross-site":
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		case "no metadata":
			r.Header.Del("Sec-Fetch-Mode")
		case "post":
			r.Method = http.MethodPost
		case "api":
			r.URL.Path = "/api/panes"
		case "debug":
			r.URL.Path = "/debug/pprof/"
		case "script":
			r.Header.Set("Sec-Fetch-Dest", "script")
		case "upgrade":
			r.Header.Set("Upgrade", "websocket")
		}
		if originProtected(r) {
			t.Errorf("allowed %s", change)
		}
	}
}

func TestTunnelFrameWithoutBrowserSource(t *testing.T) {
	// The private channel calls the already authenticated relay inner handler
	// (main.go), not the local authGate. Host may be the tunnel's logical host.
	for _, metadata := range []bool{false, true} {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		a := &apiServer{}
		r := httptest.NewRequest(http.MethodGet, "http://daemon.tunnel.invalid/?embed=shinoque", nil)
		if metadata {
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "iframe")
			r.Header.Set("Sec-Fetch-Site", "same-site")
		}
		w := httptest.NewRecorder()
		markRelayTransport(a.apiHandler(mux)).ServeHTTP(w, r)
		if w.Code != http.StatusNoContent || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("tunnel metadata=%v status=%d", metadata, w.Code)
		}
	}
}

func TestGuardedRouterBackendHandoff(t *testing.T) {
	gate := &authGate{token: "test-backend"}
	backend := httptest.NewServer(gate.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Solu-Proxy-Auth") != "router" || r.Header.Get("Authorization") != "Bearer test-backend" {
			t.Error("guarded handoff did not replace external credentials/marker")
		}
		if r.URL.Path != "/api/health" {
			t.Errorf("backend path=%s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	for _, prefix := range []string{"", "/b/wsl", "/b/win"} {
		frontend := httptest.NewServer(routerAppGuard(routerGuard(newPrefixProxy(target, prefix, "test-backend"))))
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, own := range []bool{true, false} {
				r, _ := http.NewRequest(method, frontend.URL+prefix+"/api/health", nil)
				origin := frontend.URL
				if !own {
					origin = "http://127.0.0.1:1"
				}
				r.Header.Set("Origin", origin)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set("X-Solu-Proxy-Auth", "external-marker")
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				want := http.StatusForbidden
				if own {
					want = http.StatusNoContent
				}
				if resp.StatusCode != want {
					t.Errorf("%s %s own=%v status=%d", prefix, method, own, resp.StatusCode)
				}
			}
		}
		// A frontend-looking source and marker on a direct backend request
		// cannot stand in for the guarded router handoff.
		r, _ := http.NewRequest(http.MethodPost, backend.URL+"/api/health", nil)
		r.Header.Set("Origin", frontend.URL)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Solu-Proxy-Auth", "router")
		r.Header.Set("Authorization", "Bearer test-backend")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("direct backend accepted frontend origin: %d", resp.StatusCode)
		}
		frontend.Close()
	}
	if gate.routerSkewRequests.Load() != 0 {
		t.Fatal("current router entered the compatibility path")
	}
}
