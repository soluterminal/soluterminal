package main

import (
	"net"
	"net/http"
	"strconv"
	"testing"
)

// shinoqueEnvPort configures the relay through SHINOQUE_HOME and
// SHINOQUE_RELAY_PORT, with no discovery file.
func shinoqueEnvPort(t *testing.T, port int) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shinoqueTestToken(t, "env-secret")
	t.Setenv("SHINOQUE_RELAY_PORT", strconv.Itoa(port))
}

// An env-configured relay advertises no relay capabilities, so it never
// forwards; but resolve and the health probe must still refuse a port solud
// holds, as they do for discovery, including after a takeover within the TTL.
func TestShinoqueEnvPortRejectsSoludListener(t *testing.T) {
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	port := listener.Addr().(*net.TCPAddr).Port
	shinoqueEnvPort(t, port)
	tokens := &shinoqueTokenCache{}
	scans := shinoqueCountedScan(tokens)
	shinoqueFrozenClock(tokens)
	if tokens.read() != "env-secret" || tokens.discovery.source != "env" || scans.Load() != 1 {
		t.Fatalf("env relay unavailable: source=%q rejection=%q scans=%d", tokens.discovery.source, tokens.discovery.rejection, scans.Load())
	}
	_ = listener.Close()
	shinoqueSoludHolder(t, shinoqueListen(t, "tcp4", "127.0.0.1:"+strconv.Itoa(port), false))
	if tokens.read() != "" || tokens.discovery.rejection != "discovery port belongs to solud" {
		t.Fatalf("env solud listener accepted: %q", tokens.discovery.rejection)
	}
	if w := shinoqueRelayGet(newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("env solud response = %d", w.Code)
	}
}

// SHINOQUE_RELAY_PORT pointing straight at a solud is never advertised.
func TestShinoqueEnvPortAtSoludRefused(t *testing.T) {
	solud := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	port := solud.Addr().(*net.TCPAddr).Port
	shinoqueSoludHolder(t, solud)
	shinoqueEnvPort(t, port)
	tokens := &shinoqueTokenCache{}
	a := &apiServer{shinoqueTokens: tokens}
	if a.shinoqueTokens.read() != "" || tokens.discovery.rejection != "discovery port belongs to solud" {
		t.Fatalf("health advertises a relay to solud: %q", tokens.discovery.rejection)
	}
}

// With no discovery file and no env, the default port is checked once per
// TTL: a missing relay.json must not invalidate the verdict on every resolve.
func TestShinoqueDefaultPortScansOncePerTTL(t *testing.T) {
	shinoqueDiscoveryFixture(t)
	port := shinoquePort()
	// Any listener on the default port will do, ours or one already there.
	if listener, err := net.Listen("tcp4", "127.0.0.1:"+port); err == nil {
		defer listener.Close()
	}
	if len(shinoqueListenSockets(port, nil)) == 0 {
		t.Skipf("nothing listens on default port %s", port)
	}
	tokens := &shinoqueTokenCache{}
	scans := shinoqueCountedScan(tokens)
	advance := shinoqueFrozenClock(tokens)
	for range 5 {
		if _, ok := tokens.discovery.resolve(); !ok || tokens.discovery.source != "default" {
			t.Fatalf("default source rejected: source=%q rejection=%q", tokens.discovery.source, tokens.discovery.rejection)
		}
	}
	if scans.Load() != 1 {
		t.Fatalf("scans = %d, want 1 within the TTL", scans.Load())
	}
	advance(shinoqueSoludVerdictTTL)
	if _, ok := tokens.discovery.resolve(); !ok || scans.Load() != 2 {
		t.Fatalf("ok=%t scans=%d after the TTL", ok, scans.Load())
	}
}
