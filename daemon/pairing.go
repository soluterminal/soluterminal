package main

import (
	"context"
	"sync"
	"time"
)

// pairingManager drives server-driven (web-initiated) machine pairing.
//
// The embedded web UI POSTs /api/cloud/pair/start to get a code, shows it to
// the user, and polls /api/cloud/status to watch progress. This manager:
//
//   - calls the cloud's POST /api/claim/start to mint a code+secret,
//   - runs a BACKGROUND goroutine that polls the cloud's GET /api/claim/wait
//     with the secret and, on "claimed", writes claim.json via SaveClaim,
//   - exposes the pending code/expiry to GET /api/cloud/status.
//
// CRITICAL decoupling: this manager NEVER dials the relay itself. The relay
// supervisor (relay_supervisor.go) is the SOLE owner of the tunnel — it polls
// claim.json and connects the instant a valid token appears. So pairing's only
// job on success is to persist claim.json (+ optionally nudge the supervisor so
// it reconnects without waiting a full poll interval). This preserves the
// exactly-one-tunnel-per-backendID invariant.
//
// All shared state lives behind `mu`. A single in-flight pair is tracked by
// `pending`; concurrent /pair/start calls are idempotent (same code returned)
// and a /pair/cancel or success/expiry clears it.
type pairingManager struct {
	// baseCtx bounds the lifetime of every poll goroutine. It MUST be the
	// daemon's root context — NOT a per-request context. (The earlier bug:
	// start() derived the poll ctx from the /pair/start request ctx, which
	// net/http cancels the instant the response is written, so the poll
	// goroutine died before its first tick and pairing never completed — the
	// machine showed "offline" forever even after the user accepted the code.)
	baseCtx  context.Context
	relayURL string
	dataDir  string

	// onClaimed is invoked (on the poll goroutine) once claim.json has been
	// written by a successful pair, so the relay supervisor can reconnect
	// immediately rather than waiting for its next claim.json poll. nil-safe.
	// It MUST NOT itself dial the relay — see the decoupling note above.
	onClaimed func()

	// now/poll are injectable for tests. Production uses time.Now and the
	// real cloud poll cadence.
	now          func() time.Time
	pollInterval time.Duration

	mu      sync.Mutex
	pending *pendingPair
}

// pendingPair is the single in-flight pairing's state. Guarded by
// pairingManager.mu.
type pendingPair struct {
	code      string
	secret    string
	expiresAt time.Time
	cancel    context.CancelFunc // stops the poll goroutine
}

// pairingSnapshot is the wire shape served under GET /api/cloud/status's
// `pairing` object. Active is false (and code empty) when no pair is in flight.
type pairingSnapshot struct {
	Active    bool   `json:"active"`
	Code      string `json:"code"`
	ExpiresAt string `json:"expiresAt"`
}

// newPairingManager constructs a manager for the given cloud relay + data dir.
// relayURL=="" yields a manager whose start() always errors (cloud not
// configured) — callers gate on that. onClaimed may be nil.
//
// baseCtx MUST be the daemon's root (lifetime) context — it parents every poll
// goroutine so they outlive the short-lived /pair/start request but still die
// with the daemon. A nil baseCtx falls back to context.Background() (tests).
func newPairingManager(baseCtx context.Context, relayURL, dataDir string, onClaimed func()) *pairingManager {
	requireNormalBootEngine("pairing.newPairingManager")
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &pairingManager{
		baseCtx:      baseCtx,
		relayURL:     relayURL,
		dataDir:      dataDir,
		onClaimed:    onClaimed,
		now:          time.Now,
		pollInterval: 2 * time.Second,
	}
}

// pairStartResult is what start() returns to the HTTP handler.
type pairStartResult struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expiresAt"`
}

// errPairUnconfigured is returned by start() on a local-only daemon (no
// --relay), so the handler can report a clean 400/409 rather than dialing.
var errPairUnconfigured = errPair("cloud is not configured (no --relay); cannot pair")

type errPair string

func (e errPair) Error() string { return string(e) }

// start begins (or resumes) a pairing.
//
// Idempotent: if a pair is already pending and unexpired, the SAME code is
// returned and no second claim is started — this is what keeps /status's
// pairing.code stable while the user reads it off the screen. On expiry or a
// prior success the stale pending is cleared first so a fresh code is minted.
//
// On a fresh start it calls the cloud's /api/claim/start (which does the
// outbound HTTP — so start blocks briefly), records the pending state, and
// spawns the background poll goroutine.
//
// ctx scopes ONLY the outbound /api/claim/start call (so an aborted request
// cancels the mint). The poll goroutine is parented by m.baseCtx (the daemon
// context) instead, because it must OUTLIVE this request — net/http cancels the
// request ctx the moment /pair/start's response is written.
func (m *pairingManager) start(ctx context.Context) (pairStartResult, error) {
	requireNormalBootEngine("pairing.start")
	if m == nil || m.relayURL == "" {
		return pairStartResult{}, errPairUnconfigured
	}

	// Fast path: an unexpired pending pair → return the same code.
	m.mu.Lock()
	if p := m.pending; p != nil && m.now().Before(p.expiresAt) {
		res := pairStartResult{Code: p.code, ExpiresAt: p.expiresAt.UTC().Format(time.RFC3339)}
		m.mu.Unlock()
		return res, nil
	}
	// A stale (expired) pending lingers — tear it down before starting fresh.
	m.clearPendingLocked()
	m.mu.Unlock()

	// Outbound: ask the cloud to mint a code+secret. Done OUTSIDE the lock so a
	// slow/stalled cloud can't block concurrent /status reads.
	sc, err := startClaim(ctx, m.relayURL)
	if err != nil {
		return pairStartResult{}, err
	}
	expiresAt := m.now().Add(time.Duration(sc.TTLSec) * time.Second)

	// Background poll goroutine ctx — parented by the DAEMON ctx (m.baseCtx),
	// NOT the request ctx (which net/http cancels when /pair/start returns).
	// pending.cancel still cancels this independently on cancel()/replace.
	pollCtx, cancel := context.WithCancel(m.baseCtx)

	m.mu.Lock()
	// Re-check: a concurrent start() may have won the race and installed a
	// pending pair while we were doing the outbound call. If so, honor theirs
	// and drop ours (cancel the unused ctx, no goroutine was spawned for it).
	if p := m.pending; p != nil && m.now().Before(p.expiresAt) {
		res := pairStartResult{Code: p.code, ExpiresAt: p.expiresAt.UTC().Format(time.RFC3339)}
		m.mu.Unlock()
		cancel()
		return res, nil
	}
	m.pending = &pendingPair{
		code:      sc.Code,
		secret:    sc.Secret,
		expiresAt: expiresAt,
		cancel:    cancel,
	}
	m.mu.Unlock()

	go m.poll(pollCtx, sc.Secret)

	return pairStartResult{Code: sc.Code, ExpiresAt: expiresAt.UTC().Format(time.RFC3339)}, nil
}

// cancel stops any pending pair (kills the poll goroutine, clears state). Safe
// to call when nothing is pending.
func (m *pairingManager) cancel() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.clearPendingLocked()
	m.mu.Unlock()
}

// clearPendingLocked cancels the pending poll goroutine and drops the pending
// record. Caller holds mu.
func (m *pairingManager) clearPendingLocked() {
	if m.pending != nil {
		if m.pending.cancel != nil {
			m.pending.cancel()
		}
		m.pending = nil
	}
}

// snapshot reports the current pairing progress for GET /api/cloud/status.
func (m *pairingManager) snapshot() pairingSnapshot {
	if m == nil {
		return pairingSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pending
	if p == nil || !m.now().Before(p.expiresAt) {
		// No pair, or it lapsed — report inactive. (A lapsed record is cleared
		// lazily by the poll goroutine's expiry branch / the next start().)
		return pairingSnapshot{}
	}
	return pairingSnapshot{
		Active:    true,
		Code:      p.code,
		ExpiresAt: p.expiresAt.UTC().Format(time.RFC3339),
	}
}

// poll runs the background /api/claim/wait loop for one pairing. It writes
// claim.json on success (and fires onClaimed so the supervisor reconnects),
// clears pending state on success/expiry, and exits when the pending record is
// replaced/cancelled (ctx done). It NEVER dials the relay.
func (m *pairingManager) poll(ctx context.Context, secret string) {
	t := time.NewTicker(m.pollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		w, err := pollClaimWait(ctx, m.relayURL, secret)
		if err != nil {
			// Network/parse hiccup — keep polling until TTL runs out (the cloud
			// reports "expired" once the record lapses) or ctx is cancelled.
			continue
		}
		switch w.Status {
		case "pending":
			continue
		case "expired":
			// The code lapsed before acceptance. Clear pending (only if it's
			// still ours — a newer start() may have replaced it) so /status
			// flips active=false.
			m.finishIfOurs(secret)
			return
		case "claimed":
			c := claimFromWait(m.relayURL, w)
			// Persist the token. The supervisor watches claim.json and dials —
			// we deliberately do NOT touch the relay here.
			if err := SaveClaim(m.dataDir, c); err != nil {
				// Save failed: leave pending in place so /status still shows the
				// code and a retry/cancel is possible; keep polling (the cloud
				// keeps returning "claimed" briefly) in case the next write wins.
				continue
			}
			m.finishIfOurs(secret)
			if m.onClaimed != nil {
				m.onClaimed() // nudge the supervisor to reconnect immediately
			}
			return
		}
	}
}

// finishIfOurs clears the pending record IFF it still belongs to this poll's
// secret. Guards against a race where a newer start() replaced the pending pair
// (and cancelled this goroutine's ctx) between our last poll and here.
func (m *pairingManager) finishIfOurs(secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != nil && m.pending.secret == secret {
		// Don't call cancel() on ourselves via clearPendingLocked — our ctx is
		// already winding down; just drop the record so /status reflects it.
		m.pending = nil
	}
}
