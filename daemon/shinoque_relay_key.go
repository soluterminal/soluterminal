package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shinoque's core mints a fresh relay signing key at each start, keeps it in
// memory and pushes it here, so the secret that vouches for relayed phone and
// window-mode requests lives only in the two processes' memory.
// solud holds the key in memory only: never in an environment variable, a
// file, a log line or a response. Logs carry a fingerprint (the first 8 hex of
// its sha256) instead.
//
// Signing (signingKey) follows one rule:
//   - a key pushed for this home and port signs the forward;
//   - otherwise, once any key has been pushed in this process, or when the
//     home is marked memory-key (relay-key-mode, written by the core), the
//     forward fails closed. The relay-token file is never read again;
//   - otherwise, the relay-token file signs, as before. This keeps Shinoque
//     cores that predate the push working; a later change removes it.
//
// After a solud restart there is no key in memory. A pushing core has marked
// its home, so forwards fail closed until its next push (every 10 s).
//
// Residual: any holder of ~/.solu/token can push a wrong key or exhaust the
// rate limit. Either breaks the phone visibly; neither lets it forge a proof,
// because the core checks proofs with its own key.

const (
	shinoqueRelayKeyPath    = "/api/shinoque/relay-key"
	shinoqueRelayKeyMarker  = "relay-key-mode"
	shinoqueRelayKeyBodyMax = 1024
	// A burst of 6, then one push per 5 s: the core's 10 s cadence plus its
	// start and retry pushes, with twice the headroom.
	shinoqueRelayKeyBurst   = 6
	shinoqueRelayKeyRefill  = 5 * time.Second
	shinoqueRelayKeySummary = time.Hour
	shinoqueRelayKeyQuiet   = time.Minute
)

var shinoqueRelayKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// shinoqueRelayKeyPeer finds the process on the other end of a push: a
// same-uid pid and its executable. A variable for tests.
var shinoqueRelayKeyPeer = shinoqueRelayKeyPeerPlatform

// shinoqueRelayKeyState is the pushed key and the push route's bookkeeping.
// The zero value holds no key.
type shinoqueRelayKeyState struct {
	mu   sync.Mutex
	home string
	port string
	key  string
	// pushed is sticky for the life of the process: once any key has been
	// accepted, the relay-token file is never read again.
	pushed bool

	tokens   float64
	refilled time.Time

	lastFP, lastExe string
	lastPID         int
	repeats         int
	summarized      time.Time
	refusals        map[string]time.Time

	now func() time.Time // nil uses time.Now
}

func (s *shinoqueRelayKeyState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// held returns the key pushed for this home and port, or "", and whether any
// key has been pushed in this process.
func (s *shinoqueRelayKeyState) held(home, port string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != "" && home != "" && s.home == home && s.port == port {
		return s.key, true
	}
	return "", s.pushed
}

// admit takes one push from the bucket, or reports how long until one is
// free. Every request to the route counts, refused ones included.
func (s *shinoqueRelayKeyState) admit() (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if s.refilled.IsZero() {
		s.tokens, s.refilled = shinoqueRelayKeyBurst, now
	}
	if elapsed := now.Sub(s.refilled); elapsed > 0 {
		s.tokens = math.Min(shinoqueRelayKeyBurst, s.tokens+float64(elapsed)/float64(shinoqueRelayKeyRefill))
		s.refilled = now
	}
	if s.tokens >= 1 {
		s.tokens--
		return true, 0
	}
	return false, time.Duration((1 - s.tokens) * float64(shinoqueRelayKeyRefill))
}

// store replaces the held key and logs the push: a full line whenever the
// fingerprint, pid or exe changes, so both a stray push and the core's next
// one show; identical re-pushes are counted into one line an hour.
func (s *shinoqueRelayKeyState) store(home, port, key string, pid int, exe string) {
	sum := sha256.Sum256([]byte(key))
	fp := hex.EncodeToString(sum[:])[:8]
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	s.home, s.port, s.key, s.pushed = home, port, key, true
	if fp == s.lastFP && pid == s.lastPID && exe == s.lastExe {
		s.repeats++
		if now.Sub(s.summarized) >= shinoqueRelayKeySummary {
			s.flushRepeats()
			s.summarized = now
		}
		return
	}
	s.flushRepeats()
	log.Printf("shinoque relay key: push pid=%d exe=%q fp=%s", pid, exe, fp)
	s.lastFP, s.lastPID, s.lastExe, s.summarized = fp, pid, exe, now
}

func (s *shinoqueRelayKeyState) flushRepeats() {
	if s.repeats > 0 {
		log.Printf("shinoque relay key: %d unchanged re-pushes pid=%d exe=%q fp=%s", s.repeats, s.lastPID, s.lastExe, s.lastFP)
		s.repeats = 0
	}
}

// refused logs a refusal at most once per reason a minute.
func (s *shinoqueRelayKeyState) refused(reason string, pid int, exe string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if last, ok := s.refusals[reason]; ok && now.Sub(last) < shinoqueRelayKeyQuiet {
		return
	}
	if s.refusals == nil {
		s.refusals = make(map[string]time.Time)
	}
	s.refusals[reason] = now
	log.Printf("shinoque relay key: refused reason=%s pid=%d exe=%q", reason, pid, exe)
}

// shinoqueRelayKeyMarked reports whether the core marked home memory-key.
// Any entry at the marker path counts; only a confirmed absence does not.
func shinoqueRelayKeyMarked(home string) bool {
	if home == "" {
		return true
	}
	_, err := os.Lstat(filepath.Join(home, shinoqueRelayKeyMarker))
	return !errors.Is(err, os.ErrNotExist)
}

// signingKey picks the secret that signs a forward for config; see the rule
// at the top of this file. "" refuses the forward.
func (c *shinoqueTokenCache) signingKey(config shinoqueRelayConfig) string {
	if config.relayV2 {
		// Bridged: the pushed key or nothing, never the file.
		key, _ := c.pushedKey.held(config.home, config.port)
		return key
	}
	if key, pushed := c.pushedKey.held(config.home, config.port); key != "" || pushed {
		return key
	}
	if config.relayKey == 1 || shinoqueRelayKeyMarked(config.home) {
		return ""
	}
	// Compatibility with cores that predate the push; removed later.
	return c.readHome(config.home)
}

// relayCapability resolves discovery once for health: whether a forward can
// be signed now, and whether a pushed key would sign it.
func (c *shinoqueTokenCache) relayCapability() (usable, pushed bool) {
	config, ok := c.discovery.resolve()
	if !ok {
		return false, false
	}
	if key, _ := c.pushedKey.held(config.home, config.port); key != "" {
		if c.discovery.bridge != nil && (!config.supportsRelayAuth() || !c.discovery.bridge.usable()) {
			// Through the desktop, health is true only while a forward could go.
			return false, true
		}
		return true, true
	}
	if c.discovery.bridge != nil {
		return false, false
	}
	return c.signingKey(config) != "", false
}

// acceptPushedKey holds key for home and port, and drops any token read from
// the file so it can't linger in memory beside the pushed one.
func (c *shinoqueTokenCache) acceptPushedKey(home, port, key string, pid int, exe string) {
	c.pushedKey.store(home, port, key, pid, exe)
	c.mu.Lock()
	hadFile := c.path != ""
	c.path, c.token, c.info = "", "", nil
	c.mu.Unlock()
	if hadFile {
		c.discovery.invalidate()
	}
}

// shinoqueRelayKeyPushLocal reports whether r is a non-browser request made
// directly to the local listener: not through the relay tunnel or a private
// channel, not forwarded by a router, and not signed as a device.
func shinoqueRelayKeyPushLocal(r *http.Request) bool {
	if requestViaRelay(r) || privateRelayDevice(r) != "" || requestHasForwardingMetadata(r) ||
		localVerifiedDevice(r) != "" || localScopedDevice(r) != "" || !isLoopback(r) {
		return false
	}
	if len(r.Header.Values("Origin")) != 0 {
		return false
	}
	site := r.Header.Values("Sec-Fetch-Site")
	return len(site) == 0 || (len(site) == 1 && site[0] == "none")
}

// POST /api/shinoque/relay-key {"key","home","port"}. Never answers 404: the
// core reads a 404 as a solud that predates the push.
func (a *apiServer) shinoqueRelayKeyPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	tokens := a.shinoqueTokens
	if tokens == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "discovery_mismatch"})
		return
	}
	state := &tokens.pushedKey
	pid, exe := 0, ""
	refuse := func(status int, code, reason string) {
		state.refused(reason, pid, exe)
		writeJSON(w, status, map[string]any{"error": code})
	}
	if ok, wait := state.admit(); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		refuse(http.StatusTooManyRequests, "rate_limited", "rate_limited")
		return
	}
	bridge := tokens.discovery.bridge
	local := shinoqueRelayKeyPushLocal
	if bridge != nil {
		local = shinoqueRelayBridgePushLocal
	}
	if !local(r) {
		refuse(http.StatusForbidden, "local_only", "not_local")
		return
	}
	traced := false
	if pid, exe, traced = shinoqueRelayKeyPeer(r); !traced {
		pid, exe = 0, ""
		refuse(http.StatusForbidden, "local_only", "untraced_caller")
		return
	}
	if bridge != nil {
		// The verified desktop carries the push; it never holds solud's token.
		a.acceptBridgedRelayKey(w, r, tokens, pid, exe, refuse)
		return
	}
	// The owner token as a bearer, which the core's other solud calls
	// already send; tokenless loopback and the browser cookie don't count.
	header := r.Header.Values("Authorization")
	bearer, isBearer := "", false
	if len(header) == 1 {
		bearer, isBearer = strings.CutPrefix(header[0], "Bearer ")
	}
	if !isBearer || a.gate == nil || !a.gate.validToken(bearer) {
		refuse(http.StatusUnauthorized, "owner_token_required", "owner_token_required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, shinoqueRelayKeyBodyMax))
	if err != nil {
		refuse(http.StatusBadRequest, "bad_body", "bad_body")
		return
	}
	push, err := parseShinoqueRelayKeyPush(body)
	if err != nil {
		refuse(http.StatusBadRequest, "bad_body", "bad_body")
		return
	}
	config, ok := tokens.discovery.resolve()
	port := strconv.Itoa(push.Port)
	if !ok || config.home == "" || config.home != push.Home || config.port != port {
		refuse(http.StatusConflict, "discovery_mismatch", "discovery_mismatch")
		return
	}
	tokens.acceptPushedKey(push.Home, port, push.Key, pid, exe)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

type shinoqueRelayKeyBody struct {
	Key, Home string
	Port      int
}

// parseShinoqueRelayKeyPush accepts exactly {"key","home","port"}, each once.
func parseShinoqueRelayKeyPush(content []byte) (shinoqueRelayKeyBody, error) {
	var push shinoqueRelayKeyBody
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
	if len(seen) != 3 || !shinoqueRelayKeyPattern.MatchString(push.Key) || push.Home == "" ||
		!filepath.IsAbs(push.Home) || push.Port < 1 || push.Port > 65535 {
		return push, invalid
	}
	return push, nil
}
