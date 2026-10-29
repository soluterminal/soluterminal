package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// fakeShellDaemon serves a build channel on /api/health, its name on "/", one
// asset, and the single-page fallback (HTML) for any other asset.
func fakeShellDaemon(t *testing.T, name string, channel *atomic.Value, wantToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok","build":{"channel":"`+channel.Load().(string)+`"}}`)
		case "/assets/" + name + ".js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(w, name+"-js")
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, name+"-index")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func routerShellFixture(t *testing.T) (shell http.Handler, src *routerShellSource, wslChannel *atomic.Value, wsl *httptest.Server) {
	t.Helper()
	wslChannel, winChannel := &atomic.Value{}, &atomic.Value{}
	wslChannel.Store("release")
	winChannel.Store("release")
	wsl = fakeShellDaemon(t, "wsl", wslChannel, "wsl-token")
	win := fakeShellDaemon(t, "win", winChannel, "win-token")
	wslURL, _ := url.Parse(wsl.URL)
	winURL, _ := url.Parse(win.URL)
	winShell := newPrefixProxy(winURL, "", "win-token")
	src = newRouterShellSource(wslURL, "wsl-token")
	return routerShellHandler(src, newPreviewShellProxy(wslURL, "wsl-token", winShell), winShell), src, wslChannel, wsl
}

func getShell(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Body.String()
}

func TestRouterShellFollowsAPreviewWSLBuild(t *testing.T) {
	shell, src, wslChannel, _ := routerShellFixture(t)
	src.probe()
	if got := getShell(t, shell, "/"); got != "win-index" {
		t.Fatalf("released WSL: shell from %q, want Windows", got)
	}
	wslChannel.Store("preview")
	src.probe()
	if got := getShell(t, shell, "/"); got != "wsl-index" {
		t.Fatalf("Preview WSL: shell from %q, want WSL", got)
	}
	if got := getShell(t, shell, "/assets/wsl.js"); got != "wsl-js" {
		t.Fatalf("Preview asset: %q", got)
	}
	// Back to a released build: back to Windows.
	wslChannel.Store("release")
	src.probe()
	if got := getShell(t, shell, "/"); got != "win-index" {
		t.Fatalf("after Preview: shell from %q, want Windows", got)
	}
}

// An asset WSL doesn't have (it answers with its HTML fallback) comes from
// Windows, as does everything when WSL is gone.
func TestRouterPreviewShellFallsBackToWindows(t *testing.T) {
	shell, src, wslChannel, wsl := routerShellFixture(t)
	wslChannel.Store("preview")
	src.probe()
	if got := getShell(t, shell, "/assets/win.js"); got != "win-js" {
		t.Fatalf("missing WSL asset: %q, want the Windows asset", got)
	}
	wsl.Close()
	if got := getShell(t, shell, "/"); got != "win-index" {
		t.Fatalf("WSL down mid-Preview: %q, want Windows", got)
	}
	// And the next probe of an unreachable WSL is "not a Preview".
	src.probe()
	if src.preview.Load() {
		t.Fatal("an unreachable WSL still counts as a Preview")
	}
}

// A WSL that accepts connections but never answers falls back to Windows
// within the timeout instead of hanging the window.
func TestRouterPreviewShellTimesOutToWindows(t *testing.T) {
	prev := routerShellResponseTimeout
	routerShellResponseTimeout = 200 * time.Millisecond
	t.Cleanup(func() { routerShellResponseTimeout = prev })
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(func() { close(release); stuck.Close() })
	winChannel := &atomic.Value{}
	winChannel.Store("release")
	win := fakeShellDaemon(t, "win", winChannel, "win-token")
	stuckURL, _ := url.Parse(stuck.URL)
	winURL, _ := url.Parse(win.URL)
	winShell := newPrefixProxy(winURL, "", "win-token")
	src := newRouterShellSource(stuckURL, "wsl-token")
	src.preview.Store(true) // it was a Preview a moment ago
	shell := routerShellHandler(src, newPreviewShellProxy(stuckURL, "wsl-token", winShell), winShell)
	start := time.Now()
	if got := getShell(t, shell, "/"); got != "win-index" {
		t.Fatalf("stuck WSL: %q, want Windows", got)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("fallback took %v", d)
	}
}

// #739: one router log line per installer update request, naming the daemon.
func TestRouterShellApplyBackend(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{http.MethodPost, "/b/wsl/api/update/apply-shell", "wsl"},
		{http.MethodPost, "/api/update/apply-shell", "wsl"},
		{http.MethodPost, "/b/win/api/update/apply-shell", "win"},
		{http.MethodGet, "/b/wsl/api/update/apply-shell", ""},
		{http.MethodPost, "/b/wsl/api/update/status", ""},
		{http.MethodPost, "/b/wsl/api/update/apply-shell/x", ""},
	} {
		if got := routerShellApplyBackend(tc.method, tc.path); got != tc.want {
			t.Fatalf("%s %s = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
