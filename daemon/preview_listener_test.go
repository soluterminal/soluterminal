package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// previewFixture is a pane's dev server, registered as pane-owned, and the
// daemon API that proxies it.
type previewFixture struct {
	api      *apiServer
	dev      *httptest.Server
	port     int
	received chan http.Header
}

func newPreviewFixture(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *previewFixture {
	t.Helper()
	f := &previewFixture{received: make(chan http.Header, 8)}
	f.dev = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.received <- r.Header.Clone()
		answer(w, r)
	}))
	t.Cleanup(f.dev.Close)
	f.port = f.dev.Listener.Addr().(*net.TCPAddr).Port
	pr := &PortRegistry{seen: map[string]*Port{
		"tcp|127.0.0.1|" + strconv.Itoa(f.port): {
			Port: uint16(f.port), Addr: "127.0.0.1", Proto: "tcp", PaneID: "pane_1",
			Scheme: "http", WebStatus: "ready", FirstAt: time.Unix(10, 0),
		},
	}}
	f.api = &apiServer{ports: pr}
	return f
}

func (f *previewFixture) path(rest string) string {
	return "/b/host/api/proxy/" + strconv.Itoa(f.port) + rest
}

func devPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Add("Set-Cookie", "app_pref=dark; Path=/")
	w.Header().Add("Set-Cookie", "solu_session=planted; Path=/")
	w.Header().Add("Set-Cookie", "__Host-solu_device=planted; Path=/; Secure")
	_, _ = io.WriteString(w, `<html><head><link rel="stylesheet" href="/app.css"></head><body><script src="/main.js"></script><a href="/next">next</a></body></html>`)
}

func serveListener(f *previewFixture, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.api.previewListenerHandler("/b/host").ServeHTTP(w, r)
	return w
}

func TestPreviewListenerServesOnlyItsProxyRoutes(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, devPage)
	for _, path := range []string{
		"/", "/api/health", "/api/preview-origin", "/api/panes", "/mcp", "/ws",
		"/api/proxy/" + strconv.Itoa(f.port) + "/",
		"/b/wsl/api/proxy/" + strconv.Itoa(f.port) + "/",
		"/b/host/api/panes", "/b/host/api/proxyx/1/", "/b/host",
	} {
		w := serveListener(f, httptest.NewRequest("GET", "http://127.0.0.1:41000"+path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, w.Code)
		}
	}
	if len(f.received) != 0 {
		t.Fatalf("the dev server was reached %d times", len(f.received))
	}
	// A listener no pane owns is refused, as on the router.
	w := serveListener(f, httptest.NewRequest("GET", "http://127.0.0.1:41000/b/host/api/proxy/1/", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("unowned port: status %d, want 403", w.Code)
	}
}

func TestPreviewListenerDropsCredentialsAndTheCallersPrefix(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, devPage)
	r := httptest.NewRequest("GET", "http://127.0.0.1:41000"+f.path("/"), nil)
	r.Header.Set("Authorization", "Bearer master-token")
	r.Header.Set("Cookie", "solu_session=master-token; app_pref=dark")
	r.Header.Set("X-Forwarded-Prefix", "/m/someone-else")
	r.Header.Set("X-Solu-Proxy-Auth", "router")
	w := serveListener(f, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got := <-f.received
	for _, h := range []string{"Authorization", "Cookie", "X-Forwarded-Prefix", "X-Solu-Proxy-Auth"} {
		if v := got.Values(h); len(v) != 0 {
			t.Fatalf("the dev server saw %s: %q", h, v)
		}
	}
	body := w.Body.String()
	if !strings.Contains(body, `/b/host/api/proxy/`+strconv.Itoa(f.port)+`/main.js`) || strings.Contains(body, "/m/someone-else") {
		t.Fatalf("page not rewritten under this listener's prefix: %s", body)
	}
}

func TestPreviewListenerResponsesCarryNoDaemonCookieCSPOrCORS(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, devPage)
	r := httptest.NewRequest("GET", "http://127.0.0.1:41000"+f.path("/"), nil)
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	w := serveListener(f, r)
	cookies := w.Result().Header.Values("Set-Cookie")
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0], "app_pref=") {
		t.Fatalf("Set-Cookie = %q, want only the app's own cookie", cookies)
	}
	for _, h := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only", "X-Frame-Options",
		"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Private-Network"} {
		if v := w.Result().Header.Values(h); len(v) != 0 {
			t.Fatalf("preview response carries %s: %q", h, v)
		}
	}
}

// An upgraded handshake (a dev server's HMR WebSocket) is written on the
// hijacked connection, past WriteHeader: its head is filtered too.
func TestCookieFilterConnDropsDaemonCookiesFromAHijackedHead(t *testing.T) {
	t.Parallel()
	client, server := net.Pipe()
	defer client.Close()
	conn := &cookieFilterConn{Conn: server}
	go func() {
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nSet-Cookie: solu"))
		_, _ = conn.Write([]byte("_session=planted\r\nset-cookie: hmr=1\r\nSet-Cookie: __Secure-solu_x=1\r\n\r\nframe-bytes"))
		_, _ = conn.Write([]byte("-more"))
		_ = server.Close()
	}()
	out, _ := io.ReadAll(client)
	want := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nset-cookie: hmr=1\r\n\r\nframe-bytes-more"
	if string(out) != want {
		t.Fatalf("got %q\nwant %q", out, want)
	}
}

// Byte identity with the router: the same request through the desktop
// router's /b/<id> route and through this listener reaches the dev server
// with the same headers (bar cookies, which the listener drops entirely) and
// comes back with the same status, headers (bar the daemon cookie) and bytes.
func TestPreviewListenerMatchesTheRouterRoute(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "http://"+r.Host+"/next", http.StatusFound)
			return
		}
		if r.URL.Path == "/app.css" {
			w.Header().Set("Content-Type", "text/css")
			_, _ = io.WriteString(w, `body{background:url(/img/bg.png)}`)
			return
		}
		devPage(w, r)
	})
	mainMux := http.NewServeMux()
	mainMux.HandleFunc("/api/proxy/", f.api.proxyPort)
	daemon := httptest.NewServer(mainMux)
	defer daemon.Close()
	target, _ := url.Parse(daemon.URL)
	router := httptest.NewServer(newPrefixProxy(target, "/b/host", "master-token"))
	defer router.Close()
	listener := httptest.NewServer(f.api.previewListenerHandler("/b/host"))
	defer listener.Close()

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(base, rest string) (*http.Response, []byte, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("GET", base+f.path(rest), nil)
		req.Header.Set("Accept", "text/html")
		req.Header.Set("User-Agent", "preview-test")
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body, <-f.received
	}
	for _, rest := range []string{"/", "/app.css", "/old"} {
		viaRouter, routerBody, routerSeen := fetch(router.URL, rest)
		viaListener, listenerBody, listenerSeen := fetch(listener.URL, rest)
		if viaRouter.StatusCode != viaListener.StatusCode || !bytes.Equal(routerBody, listenerBody) {
			t.Fatalf("%s: router %d %q, listener %d %q", rest, viaRouter.StatusCode, routerBody, viaListener.StatusCode, listenerBody)
		}
		for _, h := range []http.Header{viaRouter.Header, viaListener.Header, routerSeen, listenerSeen} {
			h.Del("Date")
			h.Del("Set-Cookie")
			h.Del("Cookie")
			h.Del("X-Forwarded-For") // the hop before the daemon differs by design
		}
		if a, b := mustJSON(viaRouter.Header), mustJSON(viaListener.Header); a != b {
			t.Fatalf("%s: response headers differ\nrouter   %s\nlistener %s", rest, a, b)
		}
		if a, b := mustJSON(routerSeen), mustJSON(listenerSeen); a != b {
			t.Fatalf("%s: dev-server request headers differ\nrouter   %s\nlistener %s", rest, a, b)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The main listener's own routes are unchanged: an unprefixed proxy page keeps
// today's document sandbox, and a /b/ one carries none.
func TestMainProxyRoutesKeepTodaysHeaders(t *testing.T) {
	t.Parallel()
	f := newPreviewFixture(t, devPage)
	plain := httptest.NewRecorder()
	f.api.proxyPort(plain, httptest.NewRequest("GET", "http://127.0.0.1:8787/api/proxy/"+strconv.Itoa(f.port)+"/", nil))
	if plain.Result().Header.Get("Content-Security-Policy") != proxyDocumentSandboxCSP {
		t.Fatalf("unprefixed route CSP = %q", plain.Result().Header.Get("Content-Security-Policy"))
	}
	routed := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/proxy/"+strconv.Itoa(f.port)+"/", nil)
	r.Header.Set("X-Forwarded-Prefix", "/b/host")
	f.api.proxyPort(routed, r)
	if v := routed.Result().Header.Values("Content-Security-Policy"); len(v) != 0 {
		t.Fatalf("/b/ route gained a CSP: %q", v)
	}
}

func TestPreviewListenerGuardAndPort(t *testing.T) {
	t.Parallel()
	ln, err := listenPreviewPort(8787)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) || appPorts[addr.Port] || addr.Port == 8787 {
		t.Fatalf("listened on %s", addr)
	}
	for id, want := range map[string]string{"wsl": "wsl", "win": "win", "host": "host", "": "host", "other": "host"} {
		if got := previewBackendID(id); got != want {
			t.Fatalf("previewBackendID(%q) = %q, want %q", id, got, want)
		}
	}
	// The router's guard: another site's page can't drive it.
	f := newPreviewFixture(t, devPage)
	guarded := previewListenerGuard("/b/host", f.api.previewListenerHandler("/b/host"))
	r := httptest.NewRequest("GET", "http://127.0.0.1:41000"+f.path("/"), nil)
	r.Host = "127.0.0.1:41000"
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	w := httptest.NewRecorder()
	guarded.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(f.received) != 0 {
		t.Fatalf("cross-site request: status %d, dev server reached %d times", w.Code, len(f.received))
	}
	r = httptest.NewRequest("GET", "http://attacker.example"+f.path("/"), nil)
	r.Host = "attacker.example:41000"
	w = httptest.NewRecorder()
	guarded.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(f.received) != 0 {
		t.Fatalf("rebound host: status %d", w.Code)
	}
}

func TestPreviewOriginEndpoint(t *testing.T) {
	t.Parallel()
	a := &apiServer{}
	w := httptest.NewRecorder()
	a.previewOrigin(w, httptest.NewRequest("GET", "/api/preview-origin", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("no listener: status %d", w.Code)
	}
	a.previewListener = &previewListener{origin: "http://127.0.0.1:41000", prefix: "/b/host"}
	w = httptest.NewRecorder()
	a.previewOrigin(w, httptest.NewRequest("GET", "/api/preview-origin", nil))
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got["origin"] != "http://127.0.0.1:41000" || got["prefix"] != "/b/host" {
		t.Fatalf("answer %d %s", w.Code, w.Body)
	}
}

// The app frames a preview from its own port: a same-site iframe navigation.
// The guard lets it through on this listener's own route, whatever the backend
// id (routerGuard alone knows only the router's /b/wsl and /b/win), and still
// refuses a same-site page on another port that fetches it.
func TestPreviewListenerGuardAdmitsTheAppsFrame(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"/b/host", "/b/wsl", "/b/win"} {
		f := newPreviewFixture(t, devPage)
		guarded := previewListenerGuard(prefix, f.api.previewListenerHandler(prefix))
		path := prefix + "/api/proxy/" + strconv.Itoa(f.port) + "/"
		frame := httptest.NewRequest("GET", "http://127.0.0.1:41000"+path, nil)
		frame.Host = "127.0.0.1:41000"
		frame.Header.Set("Sec-Fetch-Site", "same-site")
		frame.Header.Set("Sec-Fetch-Mode", "navigate")
		frame.Header.Set("Sec-Fetch-Dest", "iframe")
		frame.Header.Set("Referer", "http://127.0.0.1:8787/")
		w := httptest.NewRecorder()
		guarded.ServeHTTP(w, frame)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: the app's frame got %d: %s", prefix, w.Code, w.Body)
		}
		<-f.received

		fetch := httptest.NewRequest("GET", "http://127.0.0.1:41000"+path, nil)
		fetch.Host = "127.0.0.1:41000"
		fetch.Header.Set("Origin", "http://127.0.0.1:8787")
		fetch.Header.Set("Sec-Fetch-Site", "same-site")
		fetch.Header.Set("Sec-Fetch-Mode", "cors")
		fetch.Header.Set("Sec-Fetch-Dest", "empty")
		w = httptest.NewRecorder()
		guarded.ServeHTTP(w, fetch)
		if w.Code != http.StatusForbidden || len(f.received) != 0 {
			t.Fatalf("%s: a same-site fetch from another port got %d", prefix, w.Code)
		}

		// Another listener's route is not this guard's exception.
		other := httptest.NewRequest("GET", "http://127.0.0.1:41000/b/other/api/proxy/1/", nil)
		other.Host = "127.0.0.1:41000"
		other.Header = frame.Header.Clone()
		w = httptest.NewRecorder()
		guarded.ServeHTTP(w, other)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: a frame of another route got %d", prefix, w.Code)
		}
	}
}
