package main

// Router subcommand: `solud router --addr X --wsl Y --win Z`.
//
// A narrow HTTP reverse proxy used by the Tauri desktop app in
// Backend::Dual mode to multiplex two daemons behind a single origin.
// Path-prefix routing:
//
//   /b/wsl/*   →  strip prefix, forward to --wsl upstream (WSL daemon
//                  via Tauri's TCP proxy, e.g. 127.0.0.1:18787)
//   /b/win/*   →  strip prefix, forward to --win upstream
//   /          →  serve from the "default" upstream. Defaults to --wsl
//                  so the frontend bundle + Serviceworker register at
//                  this origin, then make `/b/<bk>/api/*` calls.
//
// WebSocket upgrades: Go's httputil.ReverseProxy handles the Upgrade
// header correctly out of the box — hijacks the connection and pipes
// both directions.
//
// Why Go (not axum in Tauri Rust): we already build solud.exe, its
// binary tree includes httputil.ReverseProxy, and keeping routing on
// the daemon side keeps the dependency surface flat. The Tauri app
// just spawns `solud router` as a third child.

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func runRouter() {
	fs := flag.NewFlagSet("router", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:28787", "bind address")
	wslUpstream := fs.String("wsl", "", "WSL-backend upstream (e.g. http://127.0.0.1:18787)")
	winUpstream := fs.String("win", "", "Windows-backend upstream (e.g. http://127.0.0.1:8787)")
	wslToken := fs.String("wsl-token", "", "auth token for the WSL backend")
	winToken := fs.String("win-token", "", "auth token for the Windows backend")
	_ = fs.Parse(os.Args[2:])

	if *wslUpstream == "" || *winUpstream == "" {
		log.Fatalf("router: --wsl and --win are both required")
	}
	if *wslToken == "" || *winToken == "" {
		log.Fatalf("router: --wsl-token and --win-token are both required " +
			"(each daemon has its own ~/.solu/token and they differ)")
	}

	wslURL, err := url.Parse(*wslUpstream)
	if err != nil {
		log.Fatalf("router: invalid --wsl URL %q: %v", *wslUpstream, err)
	}
	winURL, err := url.Parse(*winUpstream)
	if err != nil {
		log.Fatalf("router: invalid --win URL %q: %v", *winUpstream, err)
	}

	wslProxy := newPrefixProxy(wslURL, "/b/wsl", *wslToken)
	winProxy := newPrefixProxy(winURL, "/b/win", *winToken)
	// The default-origin proxy doesn't strip any prefix — it forwards
	// `/foo` as `/foo` — used for the bare frontend bundle load. Uses
	// the WSL token so assets served from `/` (index.html, sw.js,
	// assets/*) come from the WSL daemon authenticated.
	defaultProxy := newPrefixProxy(wslURL, "", *wslToken)
	// The app SHELL (index.html, assets/*, sw.js) is served from the WINDOWS
	// daemon instead. Both daemons embed the same webdist from the same
	// release build, so the bundle is byte-identical either way — but WSL is
	// the side that can vanish. WSL2 reassigns the VM IP on sleep/resume and
	// the distro can wedge "alive but not accepting interop"; with the shell
	// on WSL that took the ENTIRE UI down, leaving a page the user could only
	// escape by restarting the app. Windows is the same host the router runs
	// on, so it is reachable whenever the router is — which downgrades a WSL
	// outage from "the app is dead" to "the WSL panes are reconnecting".
	// Bare /api/* still defaults to WSL below, so pairing fan-out and existing
	// single-backend behavior are unchanged.
	shellProxy := newPrefixProxy(winURL, "", *winToken)
	// Except while WSL runs a Preview build: then its shell is the one the
	// owner is trying out (router_shell.go). Windows stays the fallback.
	shellSrc := newRouterShellSource(wslURL, *wslToken)
	go shellSrc.run()
	shell := routerShellHandler(shellSrc, newPreviewShellProxy(wslURL, *wslToken, shellProxy), shellProxy)

	// Previews get an origin of their own: a second loopback listener that
	// serves only the backends' /api/proxy/ routes. On the app's origin a
	// pane's page would run as the app, and sandboxing it there strips its
	// scripts and data.
	previewOrigin := ""
	if host, _, err := net.SplitHostPort(*addr); err == nil {
		if ln, err := net.Listen("tcp", net.JoinHostPort(host, "0")); err != nil {
			log.Printf("router: no preview listener (%v); previews are refused", err)
		} else {
			previewOrigin = "http://" + ln.Addr().String()
			log.Printf("router previews on %s", previewOrigin)
			go func() {
				srv := &http.Server{Handler: routerGuard(routerPreviewHandler(wslProxy, winProxy))}
				if err := srv.Serve(ln); err != nil {
					log.Printf("router: preview listener: %v", err)
				}
			}()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The router logs no /api traffic, so an installer update request left
		// no trace of which daemon got it (#739). One line each; they are rare.
		if backend := routerShellApplyBackend(r.Method, r.URL.Path); backend != "" {
			log.Printf("router: %s %s -> %s daemon", r.Method, r.URL.Path, backend)
		}
		switch {
		case r.URL.Path == routerPreviewOriginPath:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]string{"origin": previewOrigin})
		case isRouterPreviewPath(r.URL.Path):
			// Never on the app's origin: send it to the preview listener, or
			// refuse it when that could not start.
			if previewOrigin == "" {
				http.Error(w, "Previews are unavailable: the desktop router could not open its preview port. Restart the app.", http.StatusServiceUnavailable)
				return
			}
			http.Redirect(w, r, previewOrigin+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		case strings.HasPrefix(r.URL.Path, "/b/wsl/") || r.URL.Path == "/b/wsl":
			wslProxy.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/b/win/") || r.URL.Path == "/b/win":
			winProxy.ServeHTTP(w, r)
		case isPairingFanoutPath(r.Method, r.URL.Path):
			// Dual-mode pairing fan-out. The desktop UI POSTs the bare,
			// same-origin /api/cloud/pair/start + /cancel (see web cloudPair.ts) —
			// it has no backend prefix, so the default proxy below would route it
			// to the WSL daemon ONLY. In Dual mode that left the Windows-native
			// ("win") daemon's pairing manager dark: it never minted/cancelled a
			// pair, so it stayed unpaired even though the UI showed "connected".
			//
			// We serve the request from the WSL (default) backend and return ITS
			// response verbatim to the UI — preserving the existing single-code UX
			// and the current single-backend behavior exactly — while ALSO firing
			// the SAME start/cancel at the win backend so its pairing manager is
			// engaged in lock-step. This is best-effort: a win-side failure (e.g.
			// the native daemon momentarily down) never blocks or alters the WSL
			// response the user sees.
			fanoutPairing(w, r, defaultProxy, winURL, *winToken)
		case !strings.HasPrefix(r.URL.Path, "/api/"):
			// Static shell — served from Windows so a wedged WSL can't blank
			// the UI, or from a Preview WSL build. See shellProxy above.
			shell.ServeHTTP(w, r)
		default:
			defaultProxy.ServeHTTP(w, r)
		}
	})

	log.Printf("router listening on %s → wsl=%s, win=%s", *addr, wslURL, winURL)
	srv := &http.Server{Addr: *addr, Handler: routerAppGuard(routerGuard(mux))}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("router: %v", err)
	}
}

// routerPreviewOriginPath tells the web app where previews are served
// (empty when the preview listener could not start).
const routerPreviewOriginPath = "/router/preview-origin"

// isRouterPreviewPath reports whether path is a backend's preview proxy route.
// routerShellApplyBackend names the daemon a POST to the installer update
// (/api/update/apply-shell) goes to, or "" for any other request.
func routerShellApplyBackend(method, path string) string {
	if method != http.MethodPost {
		return ""
	}
	switch path {
	case "/b/wsl/api/update/apply-shell", "/api/update/apply-shell":
		return "wsl"
	case "/b/win/api/update/apply-shell":
		return "win"
	}
	return ""
}

func isRouterPreviewPath(path string) bool {
	return strings.HasPrefix(path, "/b/wsl/api/proxy/") || strings.HasPrefix(path, "/b/win/api/proxy/")
}

// routerPreviewHandler serves the preview listener: the backends' preview
// proxy routes and nothing else, so a page there reaches no app API.
func routerPreviewHandler(wslProxy, winProxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/b/wsl/api/proxy/"):
			wslProxy.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/b/win/api/proxy/"):
			winProxy.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// newPrefixProxy returns a ReverseProxy that strips `prefix` from the
// incoming request path before forwarding to `target` and attaches
// the per-backend auth token as an Authorization: Bearer header.
//
// Why Bearer not cookie: cookies are set per response based on origin;
// the frontend's origin is the router, not the upstream daemon, so a
// cookie granted by one daemon's auth gate isn't automatically sent to
// the other. A Bearer header works for every request unambiguously.
// The daemon's auth gate already accepts Authorization: Bearer (see
// daemon/auth.go isAuthed()) so no daemon-side changes needed.
func newPrefixProxy(target *url.URL, prefix string, token string) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(target)
	// FlushInterval = -1 means "flush after every write to the client".
	// The default (0) means "flush only when the 8KB write buffer fills
	// or when the response body ends", which kills SSE: our /api/ui/events
	// messages are tiny (~50 bytes) and never fill the buffer, so they
	// pile up inside ReverseProxy until the upstream connection closes —
	// which happens exactly when the daemon dies during a self-update.
	// The browser then sees ERR_INCOMPLETE_CHUNKED_ENCODING and our
	// "update-applying" heads-up event is lost. -1 fixes this for SSE
	// without hurting regular HTTP (the write already flushes when the
	// response handler returns).
	p.FlushInterval = -1
	baseDirector := p.Director
	p.Director = func(req *http.Request) {
		baseDirector(req)
		// Strip prefix. Match exactly (no trailing slash) OR with a
		// slash — the handler routes both cases to us.
		if prefix != "" {
			switch {
			case req.URL.Path == prefix:
				req.URL.Path = "/"
			case strings.HasPrefix(req.URL.Path, prefix+"/"):
				req.URL.Path = req.URL.Path[len(prefix):]
			}
			if req.URL.RawPath != "" && strings.HasPrefix(req.URL.RawPath, prefix+"/") {
				req.URL.RawPath = req.URL.RawPath[len(prefix):]
			}
		}
		// Service HTML, CSS, redirects and runtime requests must retain the
		// selected backend. Never trust a caller-supplied forwarding prefix.
		req.Header.Del("X-Forwarded-Prefix")
		req.Header.Del("X-Solu-Proxy-Auth")
		if strings.HasPrefix(req.URL.Path, "/api/proxy/") {
			req.Header.Set("X-Forwarded-Prefix", prefix)
			req.Header.Set("X-Solu-Proxy-Auth", "router")
		}
		// Inject per-backend auth. Overwrite any pre-existing
		// Authorization to avoid a smuggled header leaking across
		// backends.
		req.Header.Set("Authorization", "Bearer "+token)
		// NewSingleHostReverseProxy leaves req.Host as the caller's
		// host (e.g. 127.0.0.1:28787). Be explicit so the upstream
		// sees a well-formed request.
		req.Host = target.Host
	}
	return p
}

// isPairingFanoutPath reports whether a request is one of the bare,
// same-origin pairing side-effect endpoints that must reach BOTH backends in
// Dual mode (not just the default/WSL upstream). Only the mutating
// start/cancel/disconnect POSTs need fan-out — each backend owns its own
// claim.json, so disconnect must clear BOTH or the sibling keeps its stale
// connection. GET /api/cloud/status is per-backend display state the UI
// reads via the explicit /b/<bk>/ prefixes, and /api/claim/* is the cloud's own
// route (the browser hits the cloud directly, never this router). Matched on the
// exact method+path so nothing else is accidentally duplicated to the sibling.
func isPairingFanoutPath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	return path == "/api/cloud/pair/start" ||
		path == "/api/cloud/pair/cancel" ||
		path == "/api/cloud/disconnect" ||
		// One agent-integration choice covers both backends: the Windows
		// daemon writes %USERPROFILE%'s Claude/Codex config, WSL its own.
		path == "/api/settings/agent-integration" ||
		path == "/api/settings/agent-integration/notice-ack"
}

// fanoutPairing serves a pairing request from the default (WSL) backend and
// returns its response unchanged to the client, then fires the IDENTICAL
// request at the win backend as a fire-and-forget side effect so the
// Windows-native daemon's pairing manager starts/cancels in lock-step.
//
// Why mirror rather than merge: each daemon owns its own claim.json and runs an
// independent pairing manager. start() is idempotent and cancel() is safe when
// nothing is pending (see daemon/pairing.go), so duplicating the side effect to
// win is harmless if win was already (un)pairing. We deliberately do NOT surface
// win's result to the UI — the user reads/acts on the WSL code, and a transient
// win-side error must not break the primary flow.
//
// NOTE: this engages win's pairing manager but does not, by itself, make win
// share WSL's machine identity — the cloud derives a distinct machineId/token
// per accepted code (cloud/claim.go), and the router cannot observe the token
// the WSL daemon's background poll fetches. Closing that gap (one user code →
// both backends paired under one machineId, using the cloud's existing
// multi-tunnel-per-machine model) needs a daemon-side claim-import endpoint so
// the WSL claim can be replicated to win; that lives outside this file. See the
// task report.
func fanoutPairing(w http.ResponseWriter, r *http.Request, defaultProxy *httputil.ReverseProxy, winURL *url.URL, winToken string) {
	// Buffer the body once so both the proxied (primary) request and the
	// mirrored win request get an identical copy. Pairing bodies are tiny
	// (start/cancel send {} or nothing), so this is cheap.
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
	}

	// Fire-and-forget the mirror to win BEFORE serving the primary, so both
	// managers start their outbound /api/claim/* call at roughly the same time.
	// Errors are logged, never propagated.
	go mirrorPairingToWin(r.Method, r.URL.Path, body, winURL, winToken)

	// Primary: serve from the default (WSL) backend exactly as before.
	defaultProxy.ServeHTTP(w, r)
}

// mirrorPairingToWin replays a pairing POST against the win backend with its own
// Bearer token. Best-effort: a non-2xx or transport error is logged and dropped
// — it never affects the client-facing (WSL) response.
func mirrorPairingToWin(method, path string, body []byte, winURL *url.URL, winToken string) {
	target := *winURL
	target.Path = path
	req, err := http.NewRequest(method, target.String(), strings.NewReader(string(body)))
	if err != nil {
		log.Printf("router: pairing fan-out to win build %s failed: %v", path, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+winToken)
	req.Header.Set("Content-Type", "application/json")
	req.Host = winURL.Host
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("router: pairing fan-out to win %s failed: %v", path, err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
	if resp.StatusCode >= 400 {
		log.Printf("router: pairing fan-out to win %s returned HTTP %d", path, resp.StatusCode)
	}
}

// routerGuard refuses what each daemon's own auth gate would have refused
// before the router attached that daemon's token: a non-loopback Host (a web
// page reaching the router through DNS rebinding) and cross-origin script
// requests. The daemons only skip their Host check for tokenless callers, and
// every proxied request carries a token, so the router has to check here.
func routerGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if !originProtected(r) {
			rejectCrossOrigin(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// routerAppGuard guards the app listener: on top of routerGuard it requires
// the exact origin (routerExactOrigin), so a page on the preview listener,
// which shares the hostname, cannot use the app. The preview listener itself
// is framed by the app (a same-site navigation), so it keeps routerGuard only.
func routerAppGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !routerExactOrigin(r) {
			rejectCrossOrigin(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// routerExactOrigin requires the request's own origin, port included. The
// router's two listeners (the app, and previews) share a hostname, and
// originProtected compares hostnames only; another port is another origin.
// Top-level navigations are exempt, as in originProtected.
func routerExactOrigin(r *http.Request) bool {
	if isTopLevelNavigation(r) {
		return true
	}
	if r.Header.Get("Sec-Fetch-Site") == "same-site" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}
