package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The relay through a Windows-native solud (ST-157). Shinoque's core runs in
// WSL, where this solud has no /proc, no discovery file and no reliable route.
// The Shinoque desktop app bridges the two:
//
//   - The core pushes its relay key through the desktop's forward bridge, whose
//     head check allows POST /api/shinoque/relay-key alone. The push names the
//     core's port and home, its relay capabilities and the desktop's loopback
//     core proxy port (proxyPort). The desktop never attaches solud's token, so
//     the push is accepted only from the verified desktop process
//     (shinoqueRelayKeyPeer: same SID, Shinoque.exe at its install path).
//   - Relayed requests dial that proxy, which copies bytes to the core over TCP
//     or a wsl.exe pipe. Before a byte is written on a new connection, its
//     server end must pass the check the pusher passed: any Shinoque.exe at
//     the pinned install path running as solud's Windows user, not
//     necessarily the process that pushed (shinoqueRelayBridge.ownerOf). The
//     pid and creation time are recorded, and a change drops every idle
//     connection. Host stays 127.0.0.1:<core port>, which the core checks.
//   - Every forward carries a fresh v2 MAC (shinoque_relay_v2.go), never the raw
//     key or a static device proof, and the relay-token file is never read.
//   - The core proves each reply with the key. A new connection first carries
//     one v2 GET /api/health, and nothing else goes on it until that reply's
//     proof checks out, so a squatter behind the proxy never sees a real
//     request it could replay with a body of its own. Every later reply is
//     checked too (Shinoque @security, 2026-11-10).
//
// Any failure leaves health false; there is no fallback route (@secure C1).
// Only backend "host" (Windows-only mode) takes a push. In Dual mode the WSL
// solud serves the relay and a "win" solud reports false (T16).

// shinoqueRelayBridgeHost is the only backend that relays through the desktop.
const shinoqueRelayBridgeHost = "host"

const (
	// A verified owner is reused for health this long before it is re-probed.
	shinoqueRelayBridgeVerdictTTL = 2 * time.Second
	// The core refuses ts more than 60 s from its clock; stop well before.
	shinoqueRelayBridgeSkewMax = 30 * time.Second
	// After a skewed reply, forwards wait this long before trying again.
	shinoqueRelayBridgeSkewHold = 30 * time.Second
	shinoqueRelayBridgeProbe    = 2 * time.Second
	// The core re-pushes every 10 s while its bridge and remote access are
	// on; three missed pushes and the relay stops, so health goes false.
	shinoqueRelayBridgeKeyTTL = 35 * time.Second
	// The prefix of the route check, which belongs to no phone.
	shinoqueRelayBridgeProbePrefix = "/m/solud-probe/b/host/shinoque"
	shinoqueRelayBridgeCheckBytes  = 4096
)

// shinoqueRelayOwner is the process on the far end of a connection.
type shinoqueRelayOwner struct {
	pid     int
	created int64
	image   string
}

// shinoqueRelayBridge holds what the last accepted push named, and the
// transport that reaches the core through the desktop. A nil bridge means
// this platform relays to a local listener instead (Linux).
type shinoqueRelayBridge struct {
	mu        sync.Mutex
	config    shinoqueRelayConfig // port "" until a push is accepted
	pushedAt  time.Time
	key       string
	proxyPort string
	owner     shinoqueRelayOwner
	verified  time.Time
	failure   string
	skewUntil time.Time

	transport *http.Transport

	// Seams; nil uses the platform's.
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
	ownerOf func(conn net.Conn) (shinoqueRelayOwner, string)
	now     func() time.Time
}

func newShinoqueRelayBridge() *shinoqueRelayBridge {
	b := &shinoqueRelayBridge{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = shinoqueLimitDials(b.dialContext, 8)
	b.transport = transport
	return b
}

func (b *shinoqueRelayBridge) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// current returns the pushed relay facts, or a refusal reason.
func (b *shinoqueRelayBridge) current() (shinoqueRelayConfig, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.config.port == "" {
		return shinoqueRelayConfig{}, "no relay key from Shinoque yet"
	}
	if b.clock().Sub(b.pushedAt) > shinoqueRelayBridgeKeyTTL {
		return shinoqueRelayConfig{}, "Shinoque stopped pushing its relay key"
	}
	return b.config, ""
}

// accept records a push. A changed core or proxy port forgets the verified
// owner and drops idle connections, which lead to the old destination.
func (b *shinoqueRelayBridge) accept(push shinoqueRelayBridgePush) shinoqueRelayConfig {
	config := shinoqueRelayConfig{
		port:               strconv.Itoa(push.Port),
		home:               push.Home,
		relayAuth:          push.RelayAuth,
		privateDeviceProof: push.PrivateDeviceProof,
		relayKey:           1,
		relayV2:            true,
	}
	proxyPort := strconv.Itoa(push.ProxyPort)
	b.mu.Lock()
	moved := b.config.port != config.port || b.proxyPort != proxyPort
	b.config, b.proxyPort, b.pushedAt, b.key = config, proxyPort, b.clock(), push.Key
	if moved {
		b.owner, b.verified = shinoqueRelayOwner{}, time.Time{}
	}
	b.mu.Unlock()
	if moved {
		b.transport.CloseIdleConnections()
	}
	return config
}

// forget drops the verified owner, so the next dial and health re-check it,
// and closes idle connections. The proxy logs the error that caused it.
func (b *shinoqueRelayBridge) forget() {
	b.mu.Lock()
	b.owner, b.verified = shinoqueRelayOwner{}, time.Time{}
	b.mu.Unlock()
	b.transport.CloseIdleConnections()
}

// fail is forget with a reason, logged once while it repeats.
func (b *shinoqueRelayBridge) fail(reason string) {
	b.mu.Lock()
	changed := reason != b.failure
	b.failure = reason
	b.mu.Unlock()
	b.forget()
	if changed {
		log.Printf("shinoque relay: desktop route refused: %s", reason)
	}
}

// dialContext reaches the core through the desktop's proxy, whatever address
// the transport asks for: that address only names the core's Host.
func (b *shinoqueRelayBridge) dialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	b.mu.Lock()
	port, corePort, key := b.proxyPort, b.config.port, b.key
	b.mu.Unlock()
	if port == "" {
		return nil, errors.New("shinoque relay: no desktop route")
	}
	dial := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	if b.dial != nil {
		dial = b.dial
	}
	conn, err := dial(ctx, "tcp4", "127.0.0.1:"+port)
	if err != nil {
		b.fail("cannot reach the Shinoque app")
		return nil, err
	}
	ownerOf := shinoqueRelayDesktopOwner
	if b.ownerOf != nil {
		ownerOf = b.ownerOf
	}
	owner, reason := ownerOf(conn)
	if reason == "" {
		reason = b.checkRoute(conn, key, corePort)
	}
	if reason != "" {
		conn.Close()
		b.fail(reason)
		return nil, errors.New("shinoque relay: " + reason)
	}
	b.mu.Lock()
	replaced := b.owner != (shinoqueRelayOwner{}) && b.owner != owner
	if replaced || b.failure != "" {
		log.Printf("shinoque relay: desktop route pid=%d image=%q", owner.pid, owner.image)
	}
	b.owner, b.verified, b.failure = owner, b.clock(), ""
	b.mu.Unlock()
	if replaced {
		// Idle connections lead to the previous owner (@secure C4).
		b.transport.CloseIdleConnections()
	}
	return conn, nil
}

// checkRoute sends the route check on a new connection and reads its whole
// reply, so the connection is left clean for the transport.
func (b *shinoqueRelayBridge) checkRoute(conn net.Conn, key, corePort string) string {
	if key == "" || corePort == "" {
		return "no relay key from Shinoque"
	}
	_ = conn.SetDeadline(time.Now().Add(shinoqueRelayBridgeProbe))
	defer conn.SetDeadline(time.Time{})
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+corePort+"/api/health", nil)
	if err != nil {
		return "cannot build the route check"
	}
	v2 := shinoqueRelayV2Sign(key, req.Method, req.URL.RequestURI(), shinoqueRelayBridgeProbePrefix, "")
	req.Header.Set("X-Forwarded-Prefix", shinoqueRelayBridgeProbePrefix)
	req.Header.Set(shinoqueRelayV2Header, v2)
	if err := req.Write(conn); err != nil {
		return "cannot send the route check"
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return "the route check got no reply"
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, shinoqueRelayBridgeCheckBytes+1))
	resp.Body.Close()
	b.observe(resp)
	switch {
	case resp.StatusCode != http.StatusOK || !shinoqueRelayProofValid(key, v2, resp.StatusCode, resp.Header):
		return "the route did not prove it reaches Shinoque's core"
	case n > shinoqueRelayBridgeCheckBytes || reader.Buffered() != 0 || resp.Close:
		return "the route check reply was not reusable"
	}
	return ""
}

// checkReply admits a reply proved with the key that signed its request.
// Anything else is never passed on, and its connection is not reused.
func (b *shinoqueRelayBridge) checkReply(resp *http.Response, key string) error {
	b.observe(resp)
	if resp.Request == nil || !shinoqueRelayProofValid(key, resp.Request.Header.Get(shinoqueRelayV2Header), resp.StatusCode, resp.Header) {
		b.fail("a reply came without a valid proof")
		return errors.New("shinoque relay: reply without a valid proof")
	}
	resp.Header.Del(shinoqueRelayProofHeader)
	return nil
}

// skewed reports whether forwards are held after a reply from a core whose
// clock is too far from ours for v2 (@secure C3).
func (b *shinoqueRelayBridge) skewed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clock().Before(b.skewUntil)
}

// observe compares a reply's Date with our clock. Skew holds forwards and
// health false for a while; it never widens the window or retries.
func (b *shinoqueRelayBridge) observe(resp *http.Response) {
	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return
	}
	now := b.clock()
	drift := now.Sub(date)
	if drift < 0 {
		drift = -drift
	}
	if drift <= shinoqueRelayBridgeSkewMax {
		return
	}
	b.mu.Lock()
	first := !now.Before(b.skewUntil)
	b.skewUntil = now.Add(shinoqueRelayBridgeSkewHold)
	b.mu.Unlock()
	if first {
		log.Printf("shinoque relay: clock skew %s between Windows and Shinoque's core; holding the relay", drift.Truncate(time.Second))
	}
}

// usable reports whether a forward could go now: a push is held, the clocks
// agree, and the desktop's proxy is verified within the TTL or by a probe.
func (b *shinoqueRelayBridge) usable() bool {
	b.mu.Lock()
	ready := b.config.port != "" && !b.clock().Before(b.skewUntil)
	fresh := !b.verified.IsZero() && b.clock().Sub(b.verified) < shinoqueRelayBridgeVerdictTTL
	b.mu.Unlock()
	if !ready {
		return false
	}
	if fresh {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), shinoqueRelayBridgeProbe)
	defer cancel()
	conn, err := b.dialContext(ctx, "tcp", "")
	if err != nil {
		return false
	}
	conn.Close()
	// The probe's route check can itself reveal skew.
	return !b.skewed()
}

// shinoqueRelayBridgePush is the body of a push through the desktop.
type shinoqueRelayBridgePush struct {
	Key, Home                                     string
	Port, ProxyPort, RelayAuth, PrivateDeviceProof int
}

// parseShinoqueRelayBridgePush accepts exactly {"key","home","port",
// "proxyPort","relayAuth","privateDeviceProof"}, each once. home is the
// core's WSL path, so it is a POSIX absolute path.
func parseShinoqueRelayBridgePush(content []byte) (shinoqueRelayBridgePush, error) {
	var push shinoqueRelayBridgePush
	invalid := errors.New("invalid relay key push")
	decoder := json.NewDecoder(bytes.NewReader(content))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return push, invalid
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return push, invalid
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return push, invalid
		}
		seen[name] = true
		var value any
		switch name {
		case "key":
			value = &push.Key
		case "home":
			value = &push.Home
		case "port":
			value = &push.Port
		case "proxyPort":
			value = &push.ProxyPort
		case "relayAuth":
			value = &push.RelayAuth
		case "privateDeviceProof":
			value = &push.PrivateDeviceProof
		default:
			return push, invalid
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return push, invalid
		}
		if err := json.Unmarshal(raw, value); err != nil {
			return push, invalid
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return push, invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return push, invalid
	}
	if len(seen) != 6 || !shinoqueRelayKeyPattern.MatchString(push.Key) || !path.IsAbs(push.Home) ||
		path.Clean(push.Home) != push.Home || push.Port < 1 || push.Port > 65535 ||
		push.ProxyPort < 1 || push.ProxyPort > 65535 ||
		(push.PrivateDeviceProof != 0 && push.PrivateDeviceProof != 1) {
		return push, invalid
	}
	return push, nil
}

// shinoqueRelayBridgePushLocal adds the bridge's browser rules to the local
// check: no Authorization, Referer, Cookie or fetch metadata at all, and Host
// naming this listener exactly (@secure T3).
func shinoqueRelayBridgePushLocal(r *http.Request) bool {
	if !shinoqueRelayKeyPushLocal(r) || len(r.Header.Values("Authorization")) != 0 ||
		len(r.Header.Values("Referer")) != 0 || len(r.Header.Values("Cookie")) != 0 {
		return false
	}
	for name := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Sec-Fetch-") {
			return false
		}
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return ok && r.Host == local.String()
}

// acceptBridgedRelayKey answers a push on a bridged solud. The caller has
// already admitted it and traced the peer to the verified desktop.
func (a *apiServer) acceptBridgedRelayKey(w http.ResponseWriter, r *http.Request, tokens *shinoqueTokenCache, pid int, exe string, refuse func(int, string, string)) {
	if a.backendID != shinoqueRelayBridgeHost {
		refuse(http.StatusConflict, "not_host_backend", "not_host_backend")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, shinoqueRelayKeyBodyMax))
	if err != nil {
		refuse(http.StatusBadRequest, "bad_body", "bad_body")
		return
	}
	push, err := parseShinoqueRelayBridgePush(body)
	if err != nil {
		refuse(http.StatusBadRequest, "bad_body", "bad_body")
		return
	}
	config := tokens.discovery.bridge.accept(push)
	tokens.acceptPushedKey(config.home, config.port, push.Key, pid, exe)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}
