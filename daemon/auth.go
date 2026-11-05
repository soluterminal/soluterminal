package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Auth model:
//   - Loopback (127.0.0.1 / ::1) is allowed. Keeps local browser and CLI
//     use zero-friction. On a host shared with other OS users, set
//     SOLU_LOOPBACK_REQUIRE_AUTH=1 so another local account cannot reach
//     the loopback port and impersonate the owner (see strictLoopbackAuth).
//   - Anything else must present the token, via (in order of preference):
//       * `solu_session` cookie (set by the one-time /?token=… link)
//       * `Authorization: Bearer <token>` header (for CLI/API callers)
//       * `?token=<token>` query string — only acceptable because we
//         immediately convert it to a cookie and redirect so it doesn't
//         stick in the URL bar or referer headers.
//
// The token is a 32-byte random value hex-encoded, stored at ~/.solu/token
// with 0600. It's intentionally the same token at every layer so that a
// future cloud relay can pass it through without translation.

const (
	tokenCookie     = "solu_session"
	tokenCookieDays = 365
)

type authGate struct {
	token              string
	routerSkewRequests atomic.Uint64
}

// loadOrCreateToken reads ~/.solu/token, or generates a new one on first run.
func loadOrCreateToken(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "token")
	if b, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(b))
		if t != "" {
			return t, nil
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw[:])
	// Preserve the master token's established persistence path. The bridge's
	// independently scoped token uses the atomic helper below.
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// loadOrCreateNamedToken applies the daemon's one token-generation convention
// to independently scoped credentials. Callers must still enforce scope.
func loadOrCreateNamedToken(dataDir, name string) (string, error) {
	path := filepath.Join(dataDir, name)
	if b, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(b))
		if t != "" {
			return t, nil
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw[:])
	if err := writeFileAtomicSynced(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func (a *authGate) validToken(provided string) bool {
	if provided == "" {
		return false
	}
	// constant-time compare to avoid leaking byte-by-byte via timing.
	return subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) == 1
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func safeLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// strictLoopbackAuth reports whether the operator has required the owner token
// even for loopback requests. It is off by default because the host-native
// desktop shell and a plain local browser deliberately load the UI at
// http://127.0.0.1:<port>/ with no token. On a host shared with other OS
// accounts, a second local user can reach that loopback port; setting
// SOLU_LOOPBACK_REQUIRE_AUTH=1 makes the token (or the `solu_session` cookie
// banked from the one-time /?token=… link) mandatory there too.
func strictLoopbackAuth() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_LOOPBACK_REQUIRE_AUTH"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func tokenlessLoopbackAllowed(r *http.Request) bool {
	return !strictLoopbackAuth() && isLoopback(r) && safeLoopbackHost(r.Host)
}

func (a *authGate) isAuthed(r *http.Request) bool {
	if tokenlessLoopbackAllowed(r) {
		return true
	}
	return a.hasOwnerCredential(r)
}

// hasOwnerCredential deliberately excludes the loopback convenience grant.
func (a *authGate) hasOwnerCredential(r *http.Request) bool {
	if a == nil {
		return false
	}
	if c, err := r.Cookie(tokenCookie); err == nil && a.validToken(c.Value) {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if a.validToken(strings.TrimPrefix(h, "Bearer ")) {
			return true
		}
	}
	return false
}

func sameOriginOrTrusted(r *http.Request) bool {
	if len(r.Header.Values("Origin")) > 1 || len(r.Header.Values("Referer")) > 1 {
		return false
	}
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return true
	}
	u, err := url.Parse(source)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	// Compare the complete authority. Scheme is deliberately omitted because
	// TLS may terminate before this listener; the port remains significant.
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	// The cloud relay bypasses authGate, but WebSocket upgrades still run
	// their own origin check. Cloud-authenticated browser traffic arrives
	// through the yamux reverse proxy with this prefix marker.
	if relayTransport(r) && isCloudForwardedPrefix(r.Header.Get("X-Forwarded-Prefix")) {
		return true
	}
	return false
}

// isTopLevelNavigation reports whether r is a browser navigating to a page,
// as opposed to a script-initiated fetch/XHR or a WebSocket upgrade.
//
// Loading a document is not a CSRF-sensitive act: the response is rendered,
// never handed back to script on the initiating origin. Gating navigations
// is what turned a routine daemon restart into a dead end — the browser's
// own "try again" on a network-error interstitial reloads with
// `Sec-Fetch-Site: cross-site` (the interstitial is its own origin), so the
// daemon answered the page load with a bare 403 and the only way out was
// restarting the whole app. Navigations are allowed; everything that can
// actually mutate state (all /api/, every non-GET, WS upgrades) still runs
// the full origin check below.
func isTopLevelNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	// /api/ is the state-changing surface even on GET — never exempt it.
	if path := appPolicyPath(r.URL.Path); path == "/api" || strings.HasPrefix(path, "/api/") {
		return false
	}
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		// Fetch metadata is authoritative when the browser sends it.
		if mode != "navigate" {
			return false
		}
		// Embedded app navigation has its own narrow allowance, coupled to
		// the response's frame-ancestors policy (embed_policy.go).
		if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
			return false
		}
		return true
	}
	// Older WebView2 / browser builds omit Sec-Fetch-*. Fall back to Accept:
	// navigations ask for HTML, subresource fetches and XHR do not.
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func originProtected(r *http.Request) bool {
	if legacyRouterAccepted(r) {
		return true
	}
	if isTopLevelNavigation(r) || permittedAppFrame(r) {
		return true
	}
	return browserRequestOriginMatches(r)
}

func browserRequestOriginMatches(r *http.Request) bool {
	if !sameOriginOrTrusted(r) {
		return false
	}
	if sf := r.Header.Get("Sec-Fetch-Site"); sf == "cross-site" || sf == "same-site" {
		return false
	}
	return true
}

func (a *authGate) browserOriginAllowed(r *http.Request) bool {
	// Authenticated relay preview traffic retains its opaque-origin CORS
	// and public-preview navigation contract. The actual transport, not a
	// forwarding header, establishes this exception; target validation and
	// document sandboxing still happen in the two preview handlers.
	path := r.URL.Path
	relayPreview := relayTransport(r) && (path == "/api/proxy" || strings.HasPrefix(path, "/api/proxy/") ||
		path == "/api/preview-serve" || strings.HasPrefix(path, "/api/preview-serve/"))
	// The guarded desktop preview listener carries the backend bearer.
	// The exception cannot reach an app API or a non-navigation request.
	return relayPreview || originProtected(r) || (a.hasOwnerCredential(r) && r.Header.Get("X-Solu-Proxy-Auth") == "router" &&
		strings.HasPrefix(path, "/api/proxy/") && previewNavigationShape(r))
}

func (a *authGate) browserOriginPolicy(next http.Handler) http.Handler {
	return appDocumentHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.browserOriginAllowed(r) {
			if !a.legacyRouterOriginAllowed(r) {
				rejectCrossOrigin(w, r)
				return
			}
			r = a.acceptLegacyRouter(w, r)
		}
		next.ServeHTTP(w, r)
	}))
}

// rejectCrossOrigin writes the 403 for a request that failed originProtected.
// A client that renders HTML gets a page that re-navigates to this origin
// instead of a dead-end string: a scripted same-origin navigation carries
// `Sec-Fetch-Site: same-origin`, so it clears the exact wedge that stranded
// a user on "cross-origin request rejected" until they restarted the app.
func rejectCrossOrigin(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8">
<title>soluterminal</title>
<style>body{font:14px system-ui,sans-serif;background:#0b0f14;color:#e6edf3;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
div{text-align:center}button{font:inherit;margin-top:12px;padding:6px 14px;
border-radius:6px;border:1px solid #30363d;background:#161b22;color:#e6edf3;
cursor:pointer}</style>
<div><p>Reconnecting to soluterminal&hellip;</p>
<button onclick="go()">Reload now</button></div>
<script>function go(){location.replace(location.origin+'/')}
setTimeout(go,1500)</script>`)
}

// middleware wraps a handler with the auth gate. Must run before the mux
// so that /api routes and WS upgrades are both covered.
func (a *authGate) middleware(next http.Handler) http.Handler {
	return a.browserOriginPolicy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Jev decision bridge owns a separate bearer namespace. Let the
		// exact path reach that handler after the browser origin check, before
		// cookie, master-token and tokenless-loopback handling. Near matches
		// stay protected.
		if r.URL.Path == jevBridgePath || strings.HasPrefix(r.URL.Path, modelGatewayPrefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		// One-time login via ?token=…: bank it as a cookie and redirect
		// to the same path without the token. Keeps the token out of
		// the browser history, bookmarks, and referers.
		if t := r.URL.Query().Get("token"); t != "" && a.validToken(t) {
			http.SetCookie(w, &http.Cookie{
				Name:     tokenCookie,
				Value:    t,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Secure:   r.TLS != nil,
				MaxAge:   tokenCookieDays * 24 * 60 * 60,
			})
			q := r.URL.Query()
			q.Del("token")
			r.URL.RawQuery = q.Encode()
			http.Redirect(w, r, r.URL.RequestURI(), http.StatusFound)
			return
		}

		if !a.isAuthed(r) {
			// APIs get a clean 401 so the frontend can react if needed.
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// Everything else (browsers hitting / or static assets) gets
			// a friendly plain-text page.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, "soluterminal: unauthorized.")
			fmt.Fprintln(w)
			fmt.Fprintln(w, "open the login URL printed by the daemon at startup,")
			fmt.Fprintln(w, "or append ?token=<your-token> once to any URL on this host.")
			return
		}

		next.ServeHTTP(w, r)
	}))
}
