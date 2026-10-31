package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"
)

// relaySupervisor is the SOLE owner of the cloud relay's lifecycle.
//
// Why this exists: the daemon used to decide cloud on/off exactly once at
// boot (main.go), so a machine that was claimed AFTER boot stayed dark for
// the whole process lifetime — the production "claimed 7 weeks ago, never
// connected" bug. And token rotation used to bounce the relay itself
// (cancelRelay + a fresh startRelay), which — combined with a naive
// claim.json watcher — would dial TWICE per backendID on every rotation,
// risking duplicate tunnels on the 5 healthy machines.
//
// The supervisor fixes both: it re-reads claim.json on a poll, (re)dials
// whenever a NEW token appears, and is the only place a tunnel is ever
// started. Rotation just writes claim.json + signals; the supervisor
// observes the change and redials EXACTLY ONCE. The token-value debounce
// (current == c.Token) guarantees an unchanged file — including the
// supervisor's view of rotation's own write — never causes a redundant dial.
//
// Correctness property (gated by relay_supervisor_test.go): at most ONE
// live tunnel per backendID at any instant, including across a rotation.
type relaySupervisor struct {
	ctx       context.Context
	dataDir   string
	relayURL  string
	backendID string
	handler   http.Handler

	// dial runs one tunnel to completion (blocks until the tunnel is torn
	// down). Production uses serveRelayClassified; the exactly-one-tunnel
	// test injects a fake. It must return an error classified by
	// isRelayAuthError when the cloud rejected the bearer (401/403).
	// onConnected is invoked once the tunnel is live so the supervisor can
	// flip RelayState to "online"; it carries the cloud instance named on the
	// handshake ("" for clouds that don't name one).
	dial func(ctx context.Context, relayURL, token, backendID string, handler http.Handler, onConnected func(instance string)) error

	// pollInterval is how often claim.json is re-read. 5s in production;
	// tests set it tiny. The poll is the reliable contract (fsnotify on
	// WSL DrvFs/9p is unreliable across the boundary; we don't depend on it).
	pollInterval time.Duration

	// redialDelay is the per-supervisor BASE backoff used by
	// nextRelayBackoff. Production uses relayRedialDelay; tests override this
	// instance field so one test cannot race a still-shutting-down supervisor
	// from another test by mutating package-global state.
	redialDelay time.Duration

	mu    sync.RWMutex
	state cloudStatus

	// signal nudges the poll loop to re-read claim.json immediately
	// (used by rotation's signalTokenChanged). Buffered + coalesced.
	signal chan struct{}

	// onCreds fires (on the supervisor goroutine) whenever a NEW usable token
	// is first observed — at boot if claim.json already exists, or the instant
	// a post-boot pairing writes it. main.go uses it to wire the HTTPS-side
	// cloud integrations (push/usage/rotation poller). Idempotent per token is
	// the callee's responsibility. nil in tests that don't need it.
	onCreds func(token string)

	// redialAttempt is the consecutive-failure counter driving the exponential
	// backoff in nextRelayBackoff. Reset to 0 only after a tunnel stays UP for
	// relayRedialResetDwell (see onlineSince) so a fleet that all dropped at
	// once (cloud restart) spreads its redials over a jittered window instead
	// of reconnecting in lockstep. Touched only on the supervisor goroutine.
	redialAttempt int

	// onlineSince is when the current tunnel came up (set in markOnline under
	// mu). The redial branch consults it to decide whether the just-dropped
	// tunnel dwelled long enough to reset redialAttempt.
	onlineSince time.Time

	// onCredsLost fires (on the supervisor goroutine) ONCE when a machine that
	// HAD usable creds transitions to having none — i.e. claim.json was deleted
	// (disconnect) or became corrupt. main.go uses it to tear the HTTPS-side
	// integrations down (cancel the pusher / usage scanner / rotation poller and
	// clear the in-memory cloud creds) so the daemon stops reaching a cloud this
	// machine no longer belongs to. Fires only on the having→none EDGE (guarded
	// by current != ""), never on a never-paired boot. nil in tests.
	onCredsLost func()

	// Re-homing after cloud deploys (relay_rehome.go). healthURL is the
	// cloud's GET /api/health ("" = no checks); healthInterval overrides
	// relayHealthInterval in tests; healthClient overrides the default client.
	// rehomeCh carries a move request (the instance being left) from the
	// health watcher to the supervisor loop, which alone touches the tunnel.
	rehome         relayRehome
	healthURL      string
	healthInterval time.Duration
	healthClient   *http.Client
	rehomeCh       chan string
	// rehomeBackoff, when set, replaces nextRelayBackoff for a move that
	// follows one which landed back on the old instance (tests count it).
	rehomeBackoff func() time.Duration
}

// cloudStatus is the live snapshot served by GET /api/cloud/status.
type cloudStatus struct {
	// Configured is true when --relay was supplied (cloud is possible at all).
	Configured bool `json:"configured"`
	// HasCreds is true when a parseable claim.json with a non-empty token exists.
	HasCreds bool `json:"hasCreds"`
	// CredsInvalid is true when the cloud rejected the token (401/403). The
	// supervisor stops retrying until the token changes.
	CredsInvalid bool `json:"credsInvalid"`
	// ClaimCorrupt is true when claim.json exists but cannot be parsed.
	ClaimCorrupt bool `json:"claimCorrupt"`
	// RelayState is the coarse connection state for the UI:
	// "unconfigured" | "offline" | "connecting" | "online".
	RelayState string `json:"relayState"`
	// OwnerEmail is the paired account's email if claim.json records it,
	// else empty. (P0: claim.json has no owner field yet, so usually "".)
	OwnerEmail string `json:"ownerEmail"`
}

const (
	relayStateUnconfigured = "unconfigured"
	relayStateOffline      = "offline"
	relayStateConnecting   = "connecting"
	relayStateOnline       = "online"
)

// newRelaySupervisor constructs the supervisor. relayURL=="" yields a
// supervisor that immediately reports "unconfigured" and does nothing.
func newRelaySupervisor(ctx context.Context, dataDir, relayURL, backendID string, handler http.Handler) *relaySupervisor {
	requireNormalBootEngine("relay_supervisor.newRelaySupervisor")
	s := &relaySupervisor{
		ctx:          ctx,
		dataDir:      dataDir,
		relayURL:     relayURL,
		backendID:    backendID,
		handler:      handler,
		dial:         serveRelayClassified,
		pollInterval: 5 * time.Second,
		redialDelay:  relayRedialDelay,
		signal:       make(chan struct{}, 1),
		rehomeCh:     make(chan string, 1),
	}
	if base := httpsBaseFromRelayURL(relayURL); base != "" {
		s.healthURL = base + "/api/health"
	}
	if relayURL == "" {
		s.state = cloudStatus{Configured: false, RelayState: relayStateUnconfigured}
	} else {
		s.state = cloudStatus{Configured: true, RelayState: relayStateOffline}
	}
	return s
}

// status returns a copy of the live state for GET /api/cloud/status.
func (s *relaySupervisor) status() cloudStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// connected reports whether a relay tunnel is currently online. Voice / push
// / other cloud features consult this LIVE state rather than the boot-time
// relayURL flag, so cloud comes up once pairing happens — without a restart.
func (s *relaySupervisor) connected() bool {
	return s.status().RelayState == relayStateOnline
}

func (s *relaySupervisor) setState(mut func(*cloudStatus)) {
	s.mu.Lock()
	mut(&s.state)
	s.mu.Unlock()
}

// signalTokenChanged is how token rotation hands the relay off to the
// supervisor: it does NOT bounce the relay itself anymore. The supervisor's
// in-flight tunnel returns on its own when the cloud drops the old bearer (or
// it's nudged here), the loop re-reads claim.json, sees the new token, and
// redials exactly once.
func (s *relaySupervisor) signalTokenChanged() {
	select {
	case s.signal <- struct{}{}:
	default: // already pending — coalesce
	}
}

// run is the supervisor loop. It owns the relay tunnel for the whole daemon
// lifetime. It blocks (call it as `go sup.run()`), returning only when ctx
// is cancelled.
func (s *relaySupervisor) run() {
	requireNormalBootEngine("relay_supervisor.run")
	if s.relayURL == "" {
		return // unconfigured: cloud-DEAD by user choice, nothing to supervise
	}

	var current string // token of the tunnel we are (or were) serving
	var credsInvalidFor string

	// tunnelDone is closed by the dialer goroutine when the current tunnel
	// exits; nil when no tunnel is running. dialCancel tears the current
	// tunnel down. This pair is the single-tunnel invariant: we never start
	// a new dialer while one is live — we cancel + wait for it first.
	var dialCancel context.CancelFunc
	var tunnelDone chan error

	// reconcile drives the loop one step from the on-disk claim state.
	reconcile := func() {
		c, perr := loadClaimSafe(s.dataDir)

		switch {
		case perr != nil:
			// Corrupt claim.json: treat as ABSENT (never crash the daemon),
			// surface claimCorrupt, tear any tunnel down, and park.
			s.teardown(&dialCancel, &tunnelDone)
			// having→none EDGE (see the c==nil branch): if we had a live token,
			// tear the HTTPS-side integrations down too.
			if current != "" && s.onCredsLost != nil {
				s.onCredsLost()
			}
			current = ""
			credsInvalidFor = ""
			s.setState(func(st *cloudStatus) {
				st.HasCreds = false
				st.ClaimCorrupt = true
				st.CredsInvalid = false
				st.OwnerEmail = ""
				st.RelayState = relayStateOffline
			})
			return

		case c == nil || c.Token == "":
			// No creds yet (cloud-DORMANT, not cloud-DEAD): keep the
			// supervisor alive so a later pairing connects with no restart.
			s.teardown(&dialCancel, &tunnelDone)
			// having→none EDGE: a machine that WAS connected just lost its
			// claim.json (disconnect). Tear the HTTPS-side integrations down
			// once. Guarded by current != "" so a never-paired boot — where
			// this branch fires every poll tick — never calls it.
			if current != "" && s.onCredsLost != nil {
				s.onCredsLost()
			}
			current = ""
			credsInvalidFor = ""
			s.setState(func(st *cloudStatus) {
				st.HasCreds = false
				st.ClaimCorrupt = false
				st.CredsInvalid = false
				st.OwnerEmail = ""
				st.RelayState = relayStateOffline
			})
			return
		}

		// Valid creds present.
		owner := rawClaimOwnerEmail(s.dataDir)

		// Token rejected by the cloud and unchanged since: stay parked
		// (no tight 401 loop). Resume only when the token VALUE changes.
		if credsInvalidFor == c.Token {
			s.setState(func(st *cloudStatus) {
				st.HasCreds = true
				st.ClaimCorrupt = false
				st.CredsInvalid = true
				st.OwnerEmail = owner
				st.RelayState = relayStateOffline
			})
			return
		}

		// Debounce on token value: an unchanged token with a tunnel already
		// running (or being run) is a no-op — this is what makes rotation's
		// own claim.json write never trigger a second dial.
		if c.Token == current && tunnelDone != nil {
			return
		}

		// New token (rotation) or first dial: tear the old tunnel down FIRST,
		// then start exactly one new dialer. teardown blocks until the old
		// dialer goroutine has fully exited, so there is never an overlap
		// window with two live tunnels for this backendID.
		s.teardown(&dialCancel, &tunnelDone)
		current = c.Token
		credsInvalidFor = ""
		s.setState(func(st *cloudStatus) {
			st.HasCreds = true
			st.ClaimCorrupt = false
			st.CredsInvalid = false
			st.OwnerEmail = owner
			st.RelayState = relayStateConnecting
		})

		// Wire the HTTPS-side cloud integrations for this token (push, usage,
		// rotation poller). Fires before the dial so push/voice/ARIA creds are
		// ready as soon as the tunnel is up. onCreds is idempotent per token.
		if s.onCreds != nil {
			s.onCreds(current)
		}

		dctx, cancel := context.WithCancel(s.ctx)
		dialCancel = cancel
		done := make(chan error, 1)
		tunnelDone = done
		tok := current
		go func() {
			done <- s.dial(dctx, s.relayURL, tok, s.backendID, s.handler, s.markOnline)
		}()
	}

	reconcile()

	go s.watchCloudInstance()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			s.teardown(&dialCancel, &tunnelDone)
			s.setState(func(st *cloudStatus) { st.RelayState = relayStateOffline })
			return

		case err := <-tunnelChan(tunnelDone):
			// The current tunnel exited on its own (network drop, cloud
			// restart, rotation invalidated the bearer, or auth reject).
			dialCancel = nil
			tunnelDone = nil
			s.rehome.tunnelDown()
			if isRelayAuthError(err) {
				// Auth reject: stop retrying THIS token. Park until it changes.
				credsInvalidFor = current
				s.setState(func(st *cloudStatus) {
					st.CredsInvalid = true
					st.RelayState = relayStateOffline
				})
				continue
			}
			// Network error / clean disconnect: drop to offline and let the
			// next reconcile (poll or signal) redial after a backoff.
			//
			// If the tunnel dwelled at least relayRedialResetDwell it was a
			// genuinely-stable connection, so reset the attempt counter for a
			// fast first retry. A flap (never dwelled) leaves it climbing so we
			// don't hammer a sick cloud. Full-jitter exponential backoff then
			// spreads a fleet that all dropped at once (cloud restart) instead
			// of reconnecting in synchronized lockstep.
			var dwelled bool
			s.setState(func(st *cloudStatus) {
				if st.RelayState == relayStateOnline || st.RelayState == relayStateConnecting {
					st.RelayState = relayStateOffline
				}
				if !s.onlineSince.IsZero() && time.Since(s.onlineSince) >= relayRedialResetDwell {
					dwelled = true
				}
				s.onlineSince = time.Time{}
			})
			if dwelled {
				s.redialAttempt = 0
			}
			select {
			case <-s.ctx.Done():
				continue
			case <-time.After(s.nextRelayBackoff()):
			}
			// Force an immediate re-evaluation so we redial without waiting a
			// full poll interval.
			current = "" // ensure reconcile treats this as a fresh dial
			reconcile()

		case from := <-s.rehomeCh:
			// New cloud traffic now lands on a different instance than the
			// one holding our tunnel (a deploy). Move the tunnel now rather
			// than wait minutes for the old instance to drain it.
			if tunnelDone == nil || s.rehome.current() != from {
				s.rehome.abandon() // that tunnel is already gone
				continue
			}
			log.Printf("relay: cloud traffic moved off instance %s; reconnecting the tunnel", from)
			s.teardown(&dialCancel, &tunnelDone)
			s.setState(func(st *cloudStatus) {
				if st.RelayState == relayStateOnline || st.RelayState == relayStateConnecting {
					st.RelayState = relayStateOffline
				}
				s.onlineSince = time.Time{}
			})
			if s.rehome.shouldBackOff() {
				// The last move landed back on the old instance: don't hammer
				// it, use the normal growing backoff.
				backoff := s.nextRelayBackoff
				if s.rehomeBackoff != nil {
					backoff = s.rehomeBackoff
				}
				select {
				case <-s.ctx.Done():
					continue
				case <-time.After(backoff()):
				}
			} else {
				// A clean move, not a failure: fresh attempt counter, no wait.
				s.redialAttempt = 0
			}
			current = ""
			reconcile()

		case <-s.signal:
			reconcile()

		case <-ticker.C:
			reconcile()
		}
	}
}

// relayRedialDelay is the BASE backoff (attempt-0 cap) used between redials
// after an in-flight tunnel drops. nextRelayBackoff grows the cap exponentially
// from this base and draws a FULL-jitter value in [0, cap]. Kept a var for
// compatibility with focused zero-value-supervisor tests; constructed
// supervisors snapshot it into their instance-local redialDelay before run.
var relayRedialDelay = time.Second

// relayRedialCeiling caps the backoff window so a daemon that's been dark
// still reconnects promptly (≤ ~30s) once the cloud returns.
const relayRedialCeiling = 30 * time.Second

// relayRedialResetDwell is the minimum time a tunnel must stay UP before a
// subsequent drop resets the attempt counter. Below this, a connection that
// immediately re-drops is treated as a continuing failure and the backoff
// keeps escalating — it does NOT reset to the fast first retry.
const relayRedialResetDwell = 30 * time.Second

// nextRelayBackoff returns a FULL-jitter delay in [0, cap] for the current
// redialAttempt, where cap = min(relayRedialDelay << redialAttempt,
// relayRedialCeiling), then advances the attempt counter. Full jitter
// de-correlates a fleet that all dropped at once: each daemon waits an
// independent random slice of its window, so a thundering-herd reconnect
// after a cloud restart spreads out instead of synchronizing.
// math/rand/v2 is auto-seeded per process (an unseeded math/rand would make
// every daemon draw the SAME delay and re-sync the herd). Supervisor-goroutine
// only, so the counter needs no lock.
func (s *relaySupervisor) nextRelayBackoff() time.Duration {
	base := s.redialDelay
	if base <= 0 {
		// Preserve the production default for zero-value supervisors used by
		// focused unit tests and any future direct construction.
		base = relayRedialDelay
	}
	cap := base
	if s.redialAttempt > 0 {
		// Guard the shift: a large attempt count would overflow and go negative
		// or wildly exceed the ceiling. Clamp in any of those cases.
		shifted := base << s.redialAttempt
		if shifted <= 0 || shifted > relayRedialCeiling {
			cap = relayRedialCeiling
		} else {
			cap = shifted
		}
	}
	if cap > relayRedialCeiling {
		cap = relayRedialCeiling
	}
	s.redialAttempt++
	// +1 so cap itself is reachable; cap is always >= relayRedialDelay > 0.
	return time.Duration(rand.Int64N(int64(cap) + 1))
}

// teardown cancels the current tunnel (if any) and BLOCKS until its dialer
// goroutine has fully exited, then clears the handles. Blocking is the
// single-tunnel guarantee: a new dialer is only ever started after the old
// one is gone.
func (s *relaySupervisor) teardown(cancel *context.CancelFunc, done *chan error) {
	if *cancel != nil {
		(*cancel)()
	}
	if *done != nil {
		// Drain the dialer's exit. ctx cancel makes serveRelayClassified
		// return promptly; bounded so a wedged dialer can't hang shutdown.
		select {
		case <-*done:
		case <-time.After(15 * time.Second):
		}
	}
	*cancel = nil
	*done = nil
	s.rehome.tunnelDown()
}

// tunnelChan returns ch if non-nil, else a nil channel (which blocks
// forever in a select — the desired behavior when no tunnel is running).
func tunnelChan(ch chan error) <-chan error {
	if ch == nil {
		return nil
	}
	return ch
}

// markOnline lets the production dialer report the online transition into the
// supervisor's live state. serveRelayClassified calls it once the yamux
// session is up (passed as its onConnected callback), with the cloud instance
// the handshake named.
func (s *relaySupervisor) markOnline(instance string) {
	s.rehome.tunnelUp(instance)
	s.setState(func(st *cloudStatus) {
		if !st.CredsInvalid && !st.ClaimCorrupt {
			st.RelayState = relayStateOnline
			// Stamp under mu so the redial branch's dwell check is race-free.
			s.onlineSince = time.Now()
		}
	})
}

// --- claim.json safe loading -------------------------------------------------

// loadClaimSafe is LoadClaim with a hard guarantee it NEVER crashes the
// daemon. A malformed/truncated claim.json returns (nil, parseErr) so the
// supervisor surfaces claimCorrupt; absence returns (nil, nil). This replaces
// the old log.Fatalf path so a truncated rename can't brick local terminal use.
func loadClaimSafe(dataDir string) (*Claim, error) {
	c, err := LoadClaim(dataDir)
	if err != nil {
		// LoadClaim already distinguishes absence (nil,nil) from parse error.
		// Any error here is a parse/IO problem → treat as corrupt-but-present.
		return nil, err
	}
	return c, nil
}

// rawClaimOwnerEmail best-effort reads an "ownerEmail" field straight from
// claim.json without coupling to the typed Claim struct. Unused fields are
// ignored. Returns "" on any problem.
func rawClaimOwnerEmail(dataDir string) string {
	b, err := os.ReadFile(claimFilePath(dataDir))
	if err != nil {
		return ""
	}
	var m struct {
		OwnerEmail string `json:"ownerEmail"`
	}
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.OwnerEmail
}

// errRelayAuth marks a dial that was rejected by the cloud for auth reasons
// (401/403), so the supervisor stops retrying instead of looping forever.
var errRelayAuth = errors.New("relay: auth rejected")

type relayAuthError struct{ status int }

func (e *relayAuthError) Error() string {
	return "relay: auth rejected by cloud (status " + http.StatusText(e.status) + ")"
}
func (e *relayAuthError) Unwrap() error { return errRelayAuth }

// isRelayAuthError reports whether err is (or wraps) a relay auth rejection.
func isRelayAuthError(err error) bool {
	return errors.Is(err, errRelayAuth)
}
