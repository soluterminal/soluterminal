package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"soluterminal-shared/daemonfeatures"
	"strings"
	"testing"
)

// TestRelayHandshakeIncludesShellInfo verifies the relay dial stamps the shell
// (installer) version + platform onto the handshake query (sv/sp) alongside the
// daemon build version (v) and backend id, so the cloud can report a per-machine
// installer version. We point the dialer at a plain HTTP server that captures
// the request query and refuses the WebSocket upgrade — the dial fails, but the
// request (with its query) still lands, which is all we assert.
func TestRelayHandshakeIncludesShellInfo(t *testing.T) {
	var gotQuery = make(chan map[string][]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery <- r.URL.Query()
		w.WriteHeader(http.StatusBadRequest) // not a 101, so the upgrade fails
	}))
	defer srv.Close()

	// Handshake reads these package-level hooks; set and restore around the test.
	oldVersion := version
	version = "9.9.9"
	liveShellInfo = func() (string, string) { return "0.0.803", "windows-amd64" }
	defer func() { version = oldVersion; liveShellInfo = nil }()

	// ws:// so the gorilla dialer targets the httptest server.
	relayURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_ = serveRelayClassified(context.Background(), relayURL, "tok", "wsl", http.NewServeMux(), nil)

	q := <-gotQuery
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if got := get("sv"); got != "0.0.803" {
		t.Errorf("handshake sv = %q, want 0.0.803", got)
	}
	if got := get("sp"); got != "windows-amd64" {
		t.Errorf("handshake sp = %q, want windows-amd64", got)
	}
	if got := get("v"); got != "9.9.9" {
		t.Errorf("handshake v = %q, want 9.9.9", got)
	}
	if got := get("backend"); got != "wsl" {
		t.Errorf("handshake backend = %q, want wsl", got)
	}
	if got, want := get("f"), daemonfeatures.Encode(daemonFeatures); got != want {
		t.Errorf("handshake f = %q, want registered features %q", got, want)
	}
}

// TestRelayHandshakeOmitsShellInfoWhenUnset confirms the handshake is
// forward-safe: with no liveShellInfo hook (older-style daemon / CLI-only), no
// sv/sp params are sent, and the daemon version still is.
func TestRelayHandshakeOmitsShellInfoWhenUnset(t *testing.T) {
	var gotQuery = make(chan map[string][]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery <- r.URL.Query()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	oldVersion := version
	version = "1.0.0"
	liveShellInfo = nil
	defer func() { version = oldVersion }()

	relayURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_ = serveRelayClassified(context.Background(), relayURL, "tok", "", http.NewServeMux(), nil)

	q := <-gotQuery
	if _, ok := q["sv"]; ok {
		t.Errorf("handshake unexpectedly carried sv=%v with no liveShellInfo", q["sv"])
	}
	if _, ok := q["sp"]; ok {
		t.Errorf("handshake unexpectedly carried sp=%v with no liveShellInfo", q["sp"])
	}
	if v := q["v"]; len(v) == 0 || v[0] != "1.0.0" {
		t.Errorf("handshake v = %v, want [1.0.0]", v)
	}
}
