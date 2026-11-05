package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// Temporary update compatibility for a still-running desktop router. Remove
// no earlier than 0.0.1239, and only after real-machine counters are quiet.
// Desktop's shipped ROUTER_ADDR is fixed; custom old routers must restart.
const legacyRouterOrigin = "http://127.0.0.1:28787"

type legacyRouterAcceptedKey struct{}

func legacyRouterAccepted(r *http.Request) bool {
	v, _ := r.Context().Value(legacyRouterAcceptedKey{}).(bool)
	return v
}

func routerCarriedRequest(r *http.Request) bool {
	return legacyRouterAccepted(r) || r.Header.Get("X-Solu-Proxy-Auth") == "router"
}

func (a *authGate) legacyRouterOriginAllowed(r *http.Request) bool {
	if a == nil || requestViaRelay(r) || r.Header.Get("X-Solu-Proxy-Auth") != "" ||
		len(r.Header.Values("Authorization")) != 1 || len(r.Header.Values("Origin")) > 1 || len(r.Header.Values("Referer")) > 1 {
		return false
	}
	bearer := r.Header.Get("Authorization")
	if !strings.HasPrefix(bearer, "Bearer ") || !a.validToken(strings.TrimPrefix(bearer, "Bearer ")) {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	path := appPolicyPath(r.URL.Path)
	if path == "/debug" || strings.HasPrefix(path, "/debug/") {
		return false
	}
	source := r.Header.Get("Origin")
	if source == "" {
		if len(r.Header.Values("Origin")) != 0 {
			return false
		}
		// Browser writes and socket upgrades always have Origin. Only reads
		// may use the Referer supplied by older same-origin fetch clients.
		if (r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions) || r.Header.Get("Upgrade") != "" {
			return false
		}
		source = r.Header.Get("Referer")
	}
	u, err := url.Parse(source)
	if err != nil || u.User != nil || u.Opaque != "" || u.Scheme+"://"+u.Host != legacyRouterOrigin {
		return false
	}
	if r.Header.Get("Origin") != "" && (u.Path != "" || u.RawQuery != "" || u.Fragment != "") {
		return false
	}
	return true
}

func (a *authGate) acceptLegacyRouter(w http.ResponseWriter, r *http.Request) *http.Request {
	count := a.routerSkewRequests.Add(1)
	// Fixed labels and a bounded log rate; never record source URLs or tokens.
	if count&(count-1) == 0 {
		log.Printf("router compatibility: client=desktop-router requests=%d restart_required=true", count)
	}
	w.Header().Set("X-Solu-Router-Restart", "required")
	// The WebSocket upgrader and inner middleware consume the authenticated
	// decision. A browser cannot supply this process-local context value.
	return r.WithContext(context.WithValue(r.Context(), legacyRouterAcceptedKey{}, true))
}
