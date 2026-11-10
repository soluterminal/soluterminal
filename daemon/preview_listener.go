package main

import (
	"bufio"
	"bytes"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The preview listener gives a pane's dev server an origin of its own when no
// desktop router is in front of this daemon (Windows-only, WSL-only, macOS and
// Linux desktops): the preview tile frames it there instead of on the app's
// origin. It mirrors the router's preview listener (router.go): 127.0.0.1 on a
// random port, the router's guard (previewListenerGuard), and only the
// /b/<id>/api/proxy/ routes, rewritten
// exactly as the router's newPrefixProxy hands them to proxyPort. It is
// additional: the main listener's routes are unchanged.
//
// Unlike the router it hands requests over in-process, so it carries no
// bearer: trust comes from the loopback bind, the guard and proxyPort's
// pane-owned listener allowlist. Every Cookie and Authorization header is
// dropped on the way in (cookies ignore ports, so the browser sends the app's
// session cookie here too), and a preview may not set a daemon cookie.

// appPorts are the local ports the desktop app's windows load
// (desktop/src-tauri daemon::APP_ADDRS). A preview origin must never be one.
var appPorts = map[int]bool{8787: true, 18787: true, 28787: true, 38787: true}

type previewListener struct {
	origin string // http://127.0.0.1:<port>
	prefix string // /b/<id>
}

// previewBackendID is the /b/ segment for this daemon's previews: its
// backend id when it is one the app knows, "host" otherwise.
func previewBackendID(backendID string) string {
	switch backendID {
	case "wsl", "win", "host":
		return backendID
	}
	return "host"
}

// startPreviewListener opens the listener and serves it until the process
// exits. SOLU_PREVIEW_LISTENER=0 turns it off (previews then open in the
// person's browser on desktops without a router).
func (a *apiServer) startPreviewListener(backendID string, ownPort int) (*previewListener, error) {
	if os.Getenv("SOLU_PREVIEW_LISTENER") == "0" {
		return nil, errors.New("disabled by SOLU_PREVIEW_LISTENER=0")
	}
	ln, err := listenPreviewPort(ownPort)
	if err != nil {
		return nil, err
	}
	pl := &previewListener{
		origin: "http://" + ln.Addr().String(),
		prefix: "/b/" + previewBackendID(backendID),
	}
	srv := &http.Server{Handler: previewListenerGuard(pl.prefix, a.previewListenerHandler(pl.prefix)), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("preview listener: %v", err)
		}
	}()
	return pl, nil
}

func listenPreviewPort(ownPort int) (net.Listener, error) {
	for range 8 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if !appPorts[port] && port != ownPort {
			return ln, nil
		}
		_ = ln.Close()
	}
	return nil, errors.New("no free preview port outside the app's ports")
}

// previewListenerGuard is routerGuard (router.go) line for line, with its
// preview exception keyed to this listener's own route: routerGuard's
// isRouterPreviewPath knows only the router's /b/wsl and /b/win, so the app's
// same-site frame navigation to /b/host would be refused there.
func previewListenerGuard(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path := appPolicyPath(r.URL.Path); path == "/debug" || strings.HasPrefix(path, "/debug/") {
			http.NotFound(w, r)
			return
		}
		if !safeLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if !originProtected(r) && !(strings.HasPrefix(r.URL.Path, prefix+"/api/proxy/") && previewNavigationShape(r)) {
			rejectCrossOrigin(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// previewListenerHandler serves prefix+"/api/proxy/…" and nothing else.
func (a *apiServer) previewListenerHandler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix+"/api/proxy/") {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		// The same rewrite as newPrefixProxy's Director (router.go).
		r2.URL.Path = r.URL.Path[len(prefix):]
		if r2.URL.RawPath != "" && strings.HasPrefix(r2.URL.RawPath, prefix+"/") {
			r2.URL.RawPath = r2.URL.RawPath[len(prefix):]
		}
		r2.RequestURI = r2.URL.RequestURI()
		r2.Header.Del("X-Forwarded-Prefix")
		r2.Header.Del("X-Solu-Proxy-Auth")
		r2.Header.Set("X-Forwarded-Prefix", prefix)
		r2.Header.Del("Authorization")
		r2.Header.Del("Cookie")
		a.proxyPort(&previewResponseWriter{ResponseWriter: w}, r2)
	})
}

// isDaemonCookie names a cookie only the daemon may set on its host.
func isDaemonCookie(name string) bool {
	name = strings.TrimPrefix(strings.TrimPrefix(name, "__Host-"), "__Secure-")
	return strings.HasPrefix(strings.ToLower(name), "solu_") || name == tokenCookie
}

func dropDaemonSetCookies(h http.Header) {
	var kept []string
	for _, line := range h.Values("Set-Cookie") {
		name, _, _ := strings.Cut(line, "=")
		if !isDaemonCookie(strings.TrimSpace(name)) {
			kept = append(kept, line)
		}
	}
	h.Del("Set-Cookie")
	for _, line := range kept {
		h.Add("Set-Cookie", line)
	}
}

// previewResponseWriter removes daemon cookies from what a preview answers,
// including an upgraded (WebSocket) handshake, which bypasses WriteHeader.
type previewResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *previewResponseWriter) WriteHeader(code int) {
	// Every call: an informational (1xx) head is written before the final one.
	dropDaemonSetCookies(w.Header())
	if code >= 200 {
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *previewResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *previewResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *previewResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *previewResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, brw, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	filtered := &cookieFilterConn{Conn: conn}
	return filtered, bufio.NewReadWriter(brw.Reader, bufio.NewWriter(filtered)), nil
}

// cookieFilterConn drops daemon Set-Cookie lines from the response head
// written on a hijacked connection, then passes everything through.
type cookieFilterConn struct {
	net.Conn
	head []byte
	done bool
}

const maxPreviewHead = 64 << 10

func (c *cookieFilterConn) Write(b []byte) (int, error) {
	if c.done {
		return c.Conn.Write(b)
	}
	c.head = append(c.head, b...)
	end := bytes.Index(c.head, []byte("\r\n\r\n"))
	if end < 0 {
		if len(c.head) > maxPreviewHead {
			return 0, errors.New("preview response head too large")
		}
		return len(b), nil
	}
	c.done = true
	head, rest := c.head[:end+2], c.head[end+2:]
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(head, []byte("\r\n")) {
		if name, value, ok := bytes.Cut(line, []byte(":")); ok && strings.EqualFold(string(bytes.TrimSpace(name)), "Set-Cookie") {
			cookie, _, _ := strings.Cut(strings.TrimSpace(string(value)), "=")
			if isDaemonCookie(strings.TrimSpace(cookie)) {
				continue
			}
		}
		out.Write(line)
	}
	out.Write(rest)
	c.head = nil
	if _, err := c.Conn.Write(out.Bytes()); err != nil {
		return 0, err
	}
	return len(b), nil
}

// previewOrigin answers where this daemon serves previews:
// {"origin":"http://127.0.0.1:<port>","prefix":"/b/<id>"}, or 404 when it
// doesn't (the listener is off or could not start).
func (a *apiServer) previewOrigin(w http.ResponseWriter, r *http.Request) {
	if a.previewListener == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"origin": a.previewListener.origin, "prefix": a.previewListener.prefix})
}

func portFromAddr(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
