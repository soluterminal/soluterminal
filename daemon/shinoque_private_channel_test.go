package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestShinoquePrivateChannelDesktopProxy(t *testing.T) {
	const token = "private-relay-fixture"
	home := shinoqueTestToken(t, token)
	seen := make(chan *http.Request, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Security-Policy", "sandbox")
		_, _ = io.WriteString(w, "<html><head></head><body>pane app</body></html>")
	}))
	defer service.Close()
	owned := webTestPort(t, service.URL)
	port := strconv.Itoa(int(owned.Port))
	shinoqueTestDiscovery(t, home, port)
	unownedHits := make(chan struct{}, 1)
	unownedService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unownedHits <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer unownedService.Close()
	unowned := webTestPort(t, unownedService.URL)
	unowned.PaneID = ""

	a := trustedDevicesTestServer(t)
	d := newTestDevice(t)
	approveTestDevice(t, a, d)
	a.ports = &PortRegistry{seen: map[string]*Port{"owned": owned, "unowned": unowned}}
	defer func() {
		for _, transport := range a.ports.transports {
			transport.CloseIdleConnections()
		}
	}()
	routes := http.NewServeMux()
	routes.HandleFunc("/api/proxy/", a.proxyPort)
	a.privateRelayInner = markRelayTransport(newShinoqueRelayHandler(routes, &shinoqueTokenCache{}, nil))
	privateServer := httptest.NewServer(markRelayTransport(http.HandlerFunc(a.handlePrivateRelay)))
	defer privateServer.Close()
	mux, _, client := openPrivateClient(t, a, d, privateServer)
	defer mux.shutdown()
	defer client.CloseIdleConnections()

	t.Run("desktop proxy is unsandboxed and drops daemon credentials", func(t *testing.T) {
		const prefix = "/b/wsl"
		if proxyRouteSandboxed(prefix) {
			t.Fatal("desktop backend route must remain unsandboxed")
		}
		req, err := http.NewRequest(http.MethodGet, "http://private/api/proxy/"+port+"/home?access_token=cloud-secret&keep=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-Prefix", prefix)
		req.Header.Set("Authorization", "Bearer daemon-secret")
		req.AddCookie(&http.Cookie{Name: "solu_session", Value: "daemon-secret"})
		req.AddCookie(&http.Cookie{Name: "pref", Value: "1"})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "pane app") {
			t.Fatalf("proxy response = %d, %q, %v", resp.StatusCode, body, err)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
			t.Fatalf("desktop proxy unexpectedly sandboxed: %q", csp)
		}
		got := <-seen
		if got.Header.Get("Authorization") != "" {
			t.Fatal("daemon bearer reached the pane server")
		}
		if _, err := got.Cookie("solu_session"); err != http.ErrNoCookie {
			t.Fatal("daemon session cookie reached the pane server")
		}
		if cookie, err := got.Cookie("pref"); err != nil || cookie.Value != "1" {
			t.Fatal("ordinary pane-server cookie was not preserved")
		}
		if got.URL.Path != "/home" || got.URL.RawQuery != "keep=1" {
			t.Fatalf("upstream URL = %s", got.URL)
		}
	})

	t.Run("desktop proxy refuses an unowned listening port", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://private/api/proxy/"+strconv.Itoa(int(unowned.Port))+"/home", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-Prefix", "/b/wsl")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unowned port response = %d, want 403", resp.StatusCode)
		}
		select {
		case <-unownedHits:
			t.Fatal("unowned listener received the request")
		default:
		}
	})

	// Signing belongs to the dedicated Shinoque route, not arbitrary pane
	// servers. Use that route over the same authenticated private channel.
	t.Run("Shinoque Rewrite proves the authenticated channel device", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://private/shinoque/api/files/roots", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
		req.Header.Set("X-Shinoque-Private-Device", deviceID("forged-device"))
		req.Header.Set("X-Shinoque-Relay", "forged-proof")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Shinoque response = %d", resp.StatusCode)
		}
		got := <-seen
		id := deviceID(d.spki)
		if got.Header.Get("X-Shinoque-Private-Device") != id {
			t.Fatal("Rewrite did not use the authenticated channel's device")
		}
		mac := hmac.New(sha256.New, []byte(token))
		_, _ = mac.Write([]byte("shinoque-private-device-v1|" + id))
		if proof := got.Header.Get("X-Shinoque-Relay"); proof != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("channel proof = %q", proof)
		}
	})
}
