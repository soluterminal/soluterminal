package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOriginProtectedAllowsSameOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/panes", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Origin", "http://127.0.0.1:8787")

	if !originProtected(r) {
		t.Fatal("same-origin request was rejected")
	}
}

func TestOriginProtectedRejectsCrossOriginLoopback(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/panes", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Origin", "https://example.test")

	if originProtected(r) {
		t.Fatal("cross-origin request was allowed")
	}
}

func TestOriginProtectedRejectsCrossSiteReferer(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/panes", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Referer", "https://example.test/form")

	if originProtected(r) {
		t.Fatal("cross-site referer request was allowed")
	}
}

func TestOriginProtectedAllowsSameOriginReferer(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/panes", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Referer", "http://127.0.0.1:8787/")

	if !originProtected(r) {
		t.Fatal("same-origin referer request was rejected")
	}
}

func TestOriginProtectedRejectsCrossSiteFetchMetadata(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/panes", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	r.Header.Set("Sec-Fetch-Site", "cross-site")

	if originProtected(r) {
		t.Fatal("cross-site fetch metadata request was allowed")
	}
}

// The regression that stranded a user on a dead-end 403: the daemon went
// away, the browser showed its network-error interstitial, and clicking
// "try again" reloaded the page with Sec-Fetch-Site: cross-site (the
// interstitial is its own origin). The page load must survive that —
// loading a document is not a CSRF-sensitive act.
func TestOriginProtectedAllowsTopLevelNavigationFromErrorPage(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Accept", "text/html,application/xhtml+xml")

	if !originProtected(r) {
		t.Fatal("top-level navigation was rejected; the app cannot load after a restart")
	}
}

// Browsers that omit Sec-Fetch-* (older WebView2) still have to load.
func TestOriginProtectedAllowsNavigationWithoutFetchMetadata(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Referer", "https://example.test/")
	r.Header.Set("Accept", "text/html,application/xhtml+xml")

	if !originProtected(r) {
		t.Fatal("navigation without fetch metadata was rejected")
	}
}

// The exemption is for the top-level document only. A cross-site page
// embedding the UI in an iframe is clickjacking and must still be refused.
func TestOriginProtectedRejectsCrossSiteSubframeNavigation(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "iframe")
	r.Header.Set("Accept", "text/html")

	if originProtected(r) {
		t.Fatal("cross-site iframe navigation was allowed")
	}
}

// The navigation exemption must never widen to the API surface, even on GET.
func TestOriginProtectedStillRejectsCrossSiteAPIGet(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/panes", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Accept", "text/html")

	if originProtected(r) {
		t.Fatal("cross-site GET of an /api/ path was allowed")
	}
}

// WebSocket upgrades carry the pane I/O stream — they stay gated.
func TestOriginProtectedStillRejectsCrossSiteWebSocketUpgrade(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/panes/x/io", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Origin", "https://example.test")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")

	if originProtected(r) {
		t.Fatal("cross-origin WebSocket upgrade was allowed")
	}
}

// A rejected HTML client gets a page that re-navigates same-origin, so the
// wedge clears itself instead of requiring an app restart.
func TestRejectCrossOriginServesSelfRecoveringPage(t *testing.T) {
	t.Parallel()
	a := &authGate{token: "secret"}
	h := a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/panes", nil)
	r.Host = "127.0.0.1:8787"
	r.RemoteAddr = "127.0.0.1:55555"
	r.Header.Set("Origin", "https://example.test")
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "location.replace") {
		t.Fatal("403 page has no same-origin recovery navigation")
	}
}

func TestAuthAllowsTokenlessLoopbackOnlyForSafeLocalHosts(t *testing.T) {
	t.Parallel()
	a := &authGate{token: "secret"}
	cases := []string{
		"localhost:8787",
		"localhost.",
		"dev.localhost:8787",
		"127.0.0.1:8787",
		"[::1]:8787",
		"::1",
	}
	for _, host := range cases {
		r := httptest.NewRequest("GET", "http://localhost/api/panes", nil)
		r.Host = host
		r.RemoteAddr = "127.0.0.1:55555"
		if !a.isAuthed(r) {
			t.Fatalf("loopback request with host %q required token", host)
		}
	}
}

func TestAuthRequiresTokenForSuspiciousHostOnLoopback(t *testing.T) {
	t.Parallel()
	a := &authGate{token: "secret"}
	r := httptest.NewRequest("GET", "http://attacker.example/api/panes", nil)
	r.Host = "attacker.example"
	r.RemoteAddr = "127.0.0.1:55555"

	if a.isAuthed(r) {
		t.Fatal("tokenless loopback request with suspicious Host was allowed")
	}

	r.Header.Set("Authorization", "Bearer secret")
	if !a.isAuthed(r) {
		t.Fatal("loopback request with suspicious Host and valid token was rejected")
	}
}

func TestMiddlewareRejectsSuspiciousLoopbackHostWithoutToken(t *testing.T) {
	t.Parallel()
	a := &authGate{token: "secret"}
	nextCalled := false
	h := a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	r := httptest.NewRequest("GET", "http://attacker.example/api/panes", nil)
	r.Host = "attacker.example"
	r.RemoteAddr = "127.0.0.1:55555"
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if nextCalled {
		t.Fatal("next handler was called for suspicious tokenless loopback request")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// SOLU_LOOPBACK_REQUIRE_AUTH is the shared-host opt-in. It stays off by default
// because the host-native desktop shell loads the UI with no token, but an
// operator on a machine with other OS accounts can demand the token on loopback.
func TestStrictLoopbackAuthEnvParsing(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"no", false}, {"off", false},
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"on", true}, {" on ", true},
	} {
		t.Setenv("SOLU_LOOPBACK_REQUIRE_AUTH", tc.val)
		if got := strictLoopbackAuth(); got != tc.want {
			t.Fatalf("strictLoopbackAuth(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// In strict mode a loopback caller with no credential is refused, exactly like
// a remote caller; the one-time /?token= link and its cookie (the desktop's
// path) keep working.
func TestAuthStrictModeRequiresTokenOnLoopback(t *testing.T) {
	t.Setenv("SOLU_LOOPBACK_REQUIRE_AUTH", "1")
	a := &authGate{token: "secret"}
	newReq := func() *http.Request {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/files?path=x", nil)
		r.Host = "127.0.0.1:8787"
		r.RemoteAddr = "127.0.0.1:55555"
		return r
	}
	if a.isAuthed(newReq()) {
		t.Fatal("strict mode still trusted a tokenless loopback request")
	}
	r := newReq()
	r.Header.Set("Authorization", "Bearer secret")
	if !a.isAuthed(r) {
		t.Fatal("strict mode rejected a valid bearer token on loopback")
	}
	r = newReq()
	r.AddCookie(&http.Cookie{Name: tokenCookie, Value: "secret"})
	if !a.isAuthed(r) {
		t.Fatal("strict mode rejected a valid session cookie on loopback")
	}
}

// The gate, not just the predicate: a tokenless loopback credential-file read
// never reaches the handler in strict mode.
func TestMiddlewareStrictModeBlocksTokenlessLoopback(t *testing.T) {
	t.Setenv("SOLU_LOOPBACK_REQUIRE_AUTH", "true")
	a := &authGate{token: "secret"}
	nextCalled := false
	h := a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/files?path=/etc/passwd", nil)
	r.Host = "127.0.0.1:8787"
	r.RemoteAddr = "127.0.0.1:55555"
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if nextCalled {
		t.Fatal("strict mode let a tokenless loopback file read reach the handler")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestOriginProtectedIgnoresRetiredPreviewMarker(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:41001/api/health", nil)
	r.Header.Set("Origin", "http://127.0.0.1:41002")
	r.Header.Set("X-Soluterminal-Preview-Proxy", "retired-marker")
	if originProtected(r) {
		t.Fatal("retired marker was trusted")
	}
}

func TestAuthGateJevBridgeExactPathIsolation(t *testing.T) {
	t.Setenv("SOLU_LOOPBACK_REQUIRE_AUTH", "1")
	gate := &authGate{token: "master-token"}

	t.Run("exact path reaches bridge owned auth", func(t *testing.T) {
		called := false
		handler := gate.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodPost, "http://attacker.example"+jevBridgePath, nil)
		req.Host = "attacker.example"
		req.RemoteAddr = "127.0.0.1:55555"
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if !called || rec.Code != http.StatusNoContent {
			t.Fatalf("called=%v status=%d, bridge handler did not own auth", called, rec.Code)
		}
	})

	for _, path := range []string{jevBridgePath + "/", "/api/jev/decide-near", "/api/health"} {
		t.Run(path, func(t *testing.T) {
			called := false
			handler := gate.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787"+path, nil)
			req.Host = "127.0.0.1:8787"
			req.RemoteAddr = "127.0.0.1:55555"
			req.Header.Set("Authorization", "Bearer bridge-token")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if called || rec.Code != http.StatusUnauthorized {
				t.Fatalf("path=%q called=%v status=%d, want ordinary auth rejection", path, called, rec.Code)
			}
		})
	}
}
