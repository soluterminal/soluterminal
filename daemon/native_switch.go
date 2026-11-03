package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// native_switch.go implements POST /api/panes/{id}/switch-mode (Component 8):
// flip a pane between terminal ("") and native ("native") rendering by
// killing the running agent and respawning a fresh one with the new Kind,
// preserving the pane's identity (ID, Cwd, Cols, Rows, Title) and — for a
// native respawn — its ClaudeSessionID so `claude --resume` continues the
// same conversation.
//
// The hard invariant (R-ROLLBACK): NEVER leave a pane stuck as Kind=="native"
// with a dead agent. A native respawn can fail in two ways — the spawn itself
// errors (e.g. `claude` not on PATH, bad cwd) or the resumed agent exits
// almost immediately because `--resume <sessionId>` references a session this
// machine can't replay. Either way we roll back: re-spawn the ORIGINAL pane
// (same id, original kind + cmd + session) so the user is left exactly where
// they started, then return 500. The pane that the user keeps is always a live
// one whose Kind matches its actual process.

type switchModeReq struct {
	// Kind is the target rendering mode: "" (terminal) or "native".
	Kind string `json:"kind"`
	// When selects WHEN the switch fires:
	//   "" or "now" → the synchronous force-switch (today's behavior): interrupt
	//                  any in-flight turn and respawn immediately. A "now" also
	//                  supersedes (clears) any armed deferred switch first.
	//   "deferred"  → arm a deferred switch: return 200 immediately, then fire
	//                  the switch later (off a watcher goroutine) once the
	//                  agent's current turn has SETTLED — never interrupting it.
	When string `json:"when,omitempty"`
	// Cancel:true clears any armed deferred switch on the pane and returns 200.
	// Kind is ignored when Cancel is set.
	Cancel bool `json:"cancel,omitempty"`
	// Cols/Rows are the client's LIVE measured viewport (wire parity with
	// resume's resizeReq). Optional: a Focus pane has no mounted xterm to
	// measure, so a Focus→Terminal switch usually sends neither and the daemon
	// falls back to persisted dims marked DimsUnverified (see switchSpawnOpts).
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// pendingDeferredSwitch is an armed-but-not-yet-fired deferred mode switch.
// Target is the destination Kind ("" terminal | "native"). RequestedAt is the
// arm time (unix millis), used for the max-wait giveup cap and surfaced (as
// "at") on the DTO. fired is a single-fire CAS guard: claimPendingSwitch flips
// it true exactly once so only one watcher tick can run performSwitchMode.
type pendingDeferredSwitch struct {
	Target      string
	RequestedAt int64
	fired       atomic.Bool
}

// switchModeHealthGrace is how long we wait after a native respawn to confirm
// the agent didn't die on us (the `--resume` failure mode). A healthy headless
// Claude stays running and starts streaming a system/init line; a bad --resume
// exits within a fraction of a second. We keep this short so a real switch
// feels instant but a dead session is caught before we report success.
const switchModeHealthGrace = 600 * time.Millisecond

// Codex app-server readiness is a two-stage handshake (initialize, then
// thread/start|resume), so it cannot share Claude's sub-second health window.
// On a real 0.144.1 resume the initialize response took ~274ms and the local
// thread/resume another ~813ms; the old 600ms deadline killed every healthy
// attempt before EvSessionInit could arrive. This is only a ceiling: the normal
// path still returns as soon as nativeGotInit is observed after the health floor.
const codexSwitchModeHealthGrace = 5 * time.Second

// switchModeHealthFloor is the MINIMUM alive time before a positive health
// signal (native: first system/init line; terminal: first PTY output) may end
// the grace early. A process that prints an error and exits does both within
// this floor, so requiring signal AND floor keeps the quick-exit detection
// intact while letting the common healthy respawn confirm in ~250ms instead of
// always burning the full switchModeHealthGrace — previously a fixed latency
// floor paid on EVERY switch/resume/refresh.
const switchModeHealthFloor = 250 * time.Millisecond

// switchModeRetryDelay is how long the switch waits before each retry, letting
// the just-killed agent's claude child fully release the reused session lock
// (and any stale socket settle) so the retried `claude --resume` wins. An
// interactive claude TUI can take longer to release than a headless one, so we
// allow a couple of attempts.
const switchModeRetryDelay = 700 * time.Millisecond

// switchModeSpawnAttempts is the total spawn tries (1 initial + retries) before
// a switch gives up and rolls back. Each retry waits switchModeRetryDelay.
const switchModeSpawnAttempts = 3

// switchModeSettleTimeout is the HARD cap on how long a graceful switch waits
// for an interrupted in-flight turn to settle before proceeding with the
// kill→respawn anyway. Bounded so a wedged agent (one that ignores the interrupt
// or never emits result) can never block the switch — the interrupt makes the
// transcript clean for `--resume`, but prepareClaudeResume remains the
// backstop, so a forced kill after the timeout is always safe.
const switchModeSettleTimeout = 1500 * time.Millisecond

// switchModeSettlePoll is the poll interval while waiting for the turn to settle.
const switchModeSettlePoll = 25 * time.Millisecond

// switchModeSecondInterruptDelay is when a terminal-source switch sends a SECOND
// ESC — Claude's TUI occasionally needs a second Esc to actually interrupt
// generation. Native panes don't need this (the stdin interrupt control_request
// is acked deterministically).
const switchModeSecondInterruptDelay = 150 * time.Millisecond

// switchModeTerminalGrace is the SHORT fixed wait after a terminal-source ESC
// interrupt before proceeding to kill→respawn. Unlike native, a terminal pane
// has no clean "settled" signal we can poll: Esc makes Claude abort generation
// (near-instant) but it immediately redraws an "interrupted" prompt, which keeps
// LastActivityMs fresh, so IsAgentWorking()==busy stays true. Polling it would
// always time out at switchModeSettleTimeout (a needless ~1.5s stall). The
// interrupt itself lands well within this grace; the forced-kill + repair
// backstop covers the rare case it didn't.
const switchModeTerminalGrace = 350 * time.Millisecond

// switchModeTranscriptFlushWait bounds the pre-kill wait for the session's
// on-disk transcript to appear. A switch fired moments after a session's FIRST
// message can beat claude's initial transcript flush; killing then means the
// respawn's transcript guard (Spawn) sees nothing on disk, silently drops
// `--resume`, and the conversation restarts fresh — the worst "resume is iffy"
// case. Zero cost when the transcript already exists (the overwhelmingly
// common case — one stat); when it doesn't, we give claude a bounded window to
// flush, then proceed regardless (Spawn's fresh-start notice now surfaces it).
// A var (not const) so the test suite shortens it — fixture session ids never
// have transcripts, and every switch test would otherwise idle the full wait.
var switchModeTranscriptFlushWait = 2 * time.Second

// handleSwitchMode flips a pane's rendering Kind. It parses + validates the
// request, then dispatches on the when/cancel fields:
//
//   - cancel:true        → clear any armed deferred switch, return 200 DTO.
//   - when "" | "now"    → supersede (clear) any armed deferred switch, then run
//     the synchronous force-switch (performSwitchMode) immediately.
//   - when "deferred"    → arm a deferred switch (validated here FIRST), return
//     200 DTO immediately; the switch fires later when the turn settles.
//
// The kill→respawn-with-resume + rollback core lives in performSwitchMode.
func (a *apiServer) handleSwitchMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", 404)
		return
	}

	var req switchModeReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}

	// cancel:true clears any armed deferred switch and returns the current DTO.
	// Don't require kind for a cancel. Persisted so the cancel survives a
	// restart too (symmetric with the arm); skipped when nothing was armed.
	if req.Cancel {
		hadPending := p.pendingSwitchSnapshot() != nil
		p.clearPendingSwitch()
		if hadPending {
			a.persistPendingSwitch(p)
		}
		writeJSON(w, 200, makePaneDTO(p, a.aria, a.attention))
		return
	}

	target := strings.TrimSpace(req.Kind)
	if target != "" && target != "native" {
		http.Error(w, "unknown pane kind: "+target, 400)
		return
	}
	if target == "native" && !nativeRendererEnabled() {
		http.Error(w, "native renderer is disabled: Focus mode has been retired — this pane stays in the terminal", 400)
		return
	}
	if target == "native" {
		// Refresh foreground identity at click time (and again in
		// performSwitchMode) so a stale poll cannot resume an older Codex thread.
		snapshotPaneProcesses(p)
		if p.codexIdentityBlocked.Load() {
			http.Error(w, "conversation identity is temporarily ambiguous; wait for one foreground agent and try again", http.StatusConflict)
			return
		}
	}

	when := strings.TrimSpace(req.When)
	if when != "" && when != "now" && when != "deferred" {
		http.Error(w, "unknown switch timing: "+when, 400)
		return
	}

	if when == "deferred" {
		// Arm a deferred switch. Target + env-gate are already validated above.
		// A deferred switch to the CURRENT kind is a no-op; clear any prior arm
		// (this request supersedes it) and return the live DTO without arming.
		if p.Snapshot().Kind == target {
			hadPending := p.pendingSwitchSnapshot() != nil
			p.clearPendingSwitch()
			if hadPending {
				a.persistPendingSwitch(p)
			}
			writeJSON(w, 200, makePaneDTO(p, a.aria, a.attention))
			return
		}
		a.armDeferredSwitch(p, target)
		writeJSON(w, 200, makePaneDTO(p, a.aria, a.attention))
		return
	}

	// when "" | "now": the synchronous force-switch. Acquire the shared respawn
	// single-flight so a switch can't race a concurrent refresh/resume or a
	// firing deferred switch onto the same id (which would bind two agents to one
	// socket). A "now" also supersedes any armed deferred switch — clear it before
	// we (possibly) flip, so a deferred switch armed earlier can't fire onto the
	// post-switch pane.
	if !p.tryAcquireRespawn() {
		http.Error(w, "respawn already in progress", http.StatusConflict)
		return
	}
	defer p.releaseRespawn()
	// Persist the supersede-clear eagerly (only when something WAS armed):
	// performSwitchMode rewrites the entry on success/rollback anyway, but an
	// early failure (kill error) would otherwise leave the CLEARED switch still
	// armed on disk — a restart would resurrect an intent the user superseded.
	hadPending := p.pendingSwitchSnapshot() != nil
	p.clearPendingSwitch()
	if hadPending {
		a.persistPendingSwitch(p)
	}
	newPane, err := a.performSwitchMode(p, target, false, req.Cols, req.Rows)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, makePaneDTO(newPane, a.aria, a.attention))
}

// performSwitchMode is the core kill→respawn-with-resume switch: it snapshots
// the pane, gracefully interrupts any in-flight turn, kills the agent, respawns
// it under `target` (resuming the same claude session), confirms the respawn is
// healthy with a retry loop, rolls back to the original pane on any failure,
// stamps the switch notice, and persists the kind change. It returns the NEW
// live pane (a brand-new *Pane struct from Spawn) on success.
//
// It is called BOTH synchronously from the HTTP handler (when:"now") and from
// the deferred-switch watcher goroutine (when:"deferred", once the turn has
// settled). It must NEVER be called from the parser/emitNativeEvent goroutine
// or the readLoop: it kills the pane and would self-deadlock on `<-p.done`.
//
// `deferred` is true ONLY when called from the deferred-switch watcher, which
// fires exactly when the turn has ALREADY settled. In that case the switch is
// lossless BY DEFINITION, so we MUST NOT interrupt and MUST NOT stamp the
// "interrupted" notice. We assert this EXPLICITLY rather than relying on
// gracefullyInterruptForSwitch's own check: IsAgentWorking is now prompt-aware
// (a visible Claude prompt box reads "not working"), but the deferred watcher
// can also settle on a terminal choice menu — where no prompt box is visible —
// and we must still not mark that lossless fire as interrupted. Keeping the skip
// explicit makes "deferred ⇒ never an interrupted notice" a guarantee, not an
// inference.
func (a *apiServer) performSwitchMode(p *Pane, target string, deferred bool, liveCols, liveRows uint16) (*Pane, error) {
	id := p.ID
	finishPresence := a.trackPaneRespawn(id)
	defer finishPresence()
	if target == "native" && p.Snapshot().Kind != "native" {
		// A terminal muse pane has no session-identity discovery yet — a
		// Focus flip would mint a FRESH session and silently drop the
		// conversation. Only a pane that already knows its id (spawned via
		// `muse resume <uuid>` or a previous Focus life) may flip.
		if snap := p.Snapshot(); (effectiveAgentKind(snap.Agent) == agentMuse || p.HasMuseDescendant()) &&
			strings.TrimSpace(snap.MuseSessionID) == "" {
			return nil, errors.New("switch-mode: this Muse conversation's identity isn't recoverable from a terminal pane — open a new Focus muse pane instead")
		}
		snapshotPaneProcesses(p)
		if p.codexIdentityBlocked.Load() {
			return nil, errors.New("switch-mode: Codex thread identity is not safely recoverable yet")
		}
	}

	// Capture the pane's full config BEFORE we kill it. This snapshot is the
	// source of truth for both the new spawn and the rollback spawn.
	snap := p.Snapshot()
	if snap.Kind == target {
		// No-op: already in the requested mode. Idempotent success — return the
		// existing live pane unchanged.
		return p, nil
	}

	// GRACEFUL INTERRUPT (before the kill): a blind kill→respawn silently aborts
	// an in-flight turn and leaves the transcript with a dangling tool_use that
	// `--resume` then has to repair. If the agent is mid-turn, interrupt it and
	// wait (bounded) for the turn to settle so the transcript is clean before the
	// respawn. `wasMidTurn` drives the client-facing "previous response
	// interrupted — Continue" notice set after a successful switch.
	//
	// SKIPPED for a deferred fire: the turn is already settled (that's the firing
	// condition), so there's nothing to interrupt and the switch is lossless —
	// no notice. This is explicit (not inferred) so the terminal-recency window
	// can never mislabel it.
	wasMidTurn := false
	if !deferred {
		wasMidTurn = a.gracefullyInterruptForSwitch(p, snap, target)
	}

	// Transcript-flush guard + virgin-session detection, BEFORE the spawn opts
	// are built. Claude writes a session's transcript on the FIRST user turn:
	//   - a session WITH recorded prompts but no file yet is racing its first
	//     flush — wait (bounded) so the respawn's --resume finds it (killing
	//     early silently restarted the conversation, the worst "resume is
	//     iffy" case);
	//   - a VIRGIN session (no prompts ever) will never flush — there is
	//     nothing to resume, so drop the session id for the spawn: no wasted
	//     wait, no doomed --resume, and no misleading "couldn't resume the
	//     previous conversation" banner on a pane that had no conversation.
	// The rollback path keeps the ORIGINAL snap (session id included) either way.
	snapForSpawn := snap
	if sess := strings.TrimSpace(snap.ClaudeSessionID); sess != "" {
		// Deliberately the CHEAP stat rather than claudeTranscriptResumable:
		// this is polled every 50ms for the length of the flush wait, and it is
		// only ever used to notice a transcript APPEARING. A wrong "no" costs
		// nothing here — the virgin drop below needs LastPrompts to be empty
		// too, and Spawn re-checks properly and is the one that decides.
		onDisk := func() bool {
			return claudeTranscriptOnDisk(snap.Cwd, sess, snap.ClaudeTranscriptPath)
		}
		if !onDisk() {
			if len(snap.LastPrompts) == 0 {
				snapForSpawn.ClaudeSessionID = ""
				snapForSpawn.ClaudeTranscriptPath = ""
			} else if !waitFor(switchModeTranscriptFlushWait, 50*time.Millisecond, onDisk) {
				log.Printf("switch-mode: transcript for session %s never appeared within %s (pane=%s) — proceeding; the respawn will start fresh and say so",
					shortSessionID(sess), switchModeTranscriptFlushWait, id)
			}
		}
	}

	// Build the spawn opts that recreate this pane under the target kind (see
	// switchSpawnOpts — pure + unit-tested, mirroring resumeSpawnOpts).
	newOpts := switchSpawnOpts(snapForSpawn, target, liveCols, liveRows)

	// Pre-seed Claude's per-project folder-trust flag when switching TO terminal:
	// the interactive `claude --resume` TUI shows the "Do you trust the files in
	// this folder?" onboarding gate when projects[cwd].hasTrustDialogAccepted is
	// absent/false (the headless native agent never shows it), which would block
	// the resumed conversation behind the dialog. Best-effort — the dialog
	// appearing is a soft UX failure, not a switch failure, so we log and proceed.
	if target == "" && effectiveAgentKind(snap.Agent) != agentCodex {
		if err := ensureFolderTrusted(snap.Cwd); err != nil {
			log.Printf("switch-mode: pre-trust folder failed (pane=%s cwd=%s): %v — proceeding (trust dialog may appear)", id, snap.Cwd, err)
		}
	}

	// Tear down the current agent. The id is freed from the manager so the
	// respawn can reuse it.
	if err := a.killPaneForRollback(id); err != nil {
		return nil, errors.New("switch-mode: kill failed: " + err.Error())
	}

	// Spawn the target, confirming it actually comes up — with ONE retry on a
	// quick exit. Two reasons the first attempt can fail and a retry succeed:
	//   • Stale socket: the respawn reuses the pane id (and <id>.sock path); a
	//     lingering socket from the just-killed agent makes dialAgent link the
	//     dead socket, so the daemon never connects to the fresh agent and the
	//     pane reads "exited" while the new process runs orphaned.
	//   • Session handoff race: both modes reuse the same claude session, and
	//     the just-killed agent's claude child may not have released the session
	//     lock yet, so the fresh `claude --resume` loses the race and exits.
	// Each attempt first removes the socket; the retry also waits so the old
	// child fully releases. (background-resume in main.go removes the socket for
	// the same reason — the switch path must too.)
	var newPane *Pane
	var err error
	for attempt := 0; attempt < switchModeSpawnAttempts; attempt++ {
		if attempt > 0 {
			if newPane != nil {
				_ = a.killPaneForRollback(newPane.ID)
				newPane = nil
			}
			time.Sleep(switchModeRetryDelay)
		}
		if snap.SocketPath != "" {
			_ = os.Remove(snap.SocketPath)
		}
		newPane, err = a.spawnPaneForRequest(newOpts)
		if err == nil {
			// Retries (attempt > 0) wait out the FULL health grace: they follow a
			// session-lock/socket race, exactly where a process can emit output
			// and still die late — the early-exit signal isn't trustworthy there.
			err = a.confirmSwitchHealthy(newPane, target, attempt > 0)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		// The new pane failed to come up (spawn error, or a native --resume that
		// died immediately). Roll back to the ORIGINAL pane so we never leave the
		// user with a dead native session — or a half-applied switch.
		if newPane != nil {
			_ = a.killPaneForRollback(newPane.ID)
		}
		rolled := a.rollbackSwitchMode(snap)
		if rolled != nil && a.panes != nil {
			_ = a.panes.Put(rolled)
		}
		return nil, errors.New("switch-mode failed (rolled back): " + err.Error())
	}

	// New pane is live and healthy. If we interrupted an in-flight turn to get
	// here, stamp a transient notice on the NEW pane so the client can show a
	// "previous response interrupted — Continue" marker. fromKind is the SOURCE
	// kind (the mode the interrupted turn was running in = snap.Kind). Cleared on
	// the next user turn (paneNativeInput / a committed terminal prompt).
	if wasMidTurn {
		newPane.setSwitchNotice(snap.Kind)
	}

	// Restore the non-spawn state SpawnOpts doesn't carry — AutoMode (the ARIA
	// auto-approval policy), flag + memo, and prompt history. The creation time
	// is already installed by Spawn before publication.
	// Without this a Terminal↔Focus switch silently DROPS them and persists the
	// wiped pane below (permanent loss) — notably disabling auto-approval on a
	// pane the user had opted in. Mirrors resume's restoreResumedState.
	restoreResumedState(newPane, snap)

	// Persist the kind change so it survives a daemon restart. A persist failure
	// is non-fatal to the live switch (the pane is already running in the new
	// mode); surface it but keep the pane.
	if a.panes != nil {
		if perr := a.panes.Put(newPane); perr != nil {
			return nil, errors.New("switch-mode: pane is live but persist failed: " + perr.Error())
		}
	}
	return newPane, nil
}

// gracefullyInterruptForSwitch checks whether the pane's agent is mid-turn and,
// if so, interrupts it and waits (bounded) for the turn to settle BEFORE the
// caller proceeds with the kill→respawn. It returns whether a turn was actually
// in flight (wasMidTurn) so the caller can stamp the client-facing switch notice.
//
//   - Native source: write the adapter's stdin interrupt control_request
//     (EncodeInterrupt) and wait for the in-turn flag to clear (the agent acks
//     with control_cancel_request + a result, which flips nativeInTurn false).
//   - Terminal source: write ESC to the PTY — Claude's TUI interrupts generation
//     on Esc — with a second ESC after a short delay if it's still busy, and wait
//     for computeState to leave the busy/recent band.
//
// In both cases the wait is hard-bounded by switchModeSettleTimeout so a stuck
// agent never blocks the switch; on timeout we proceed anyway (the kill is
// forced and prepareClaudeResume cleans the transcript for `--resume`).
func (a *apiServer) gracefullyInterruptForSwitch(p *Pane, snap PaneSnapshot, target string) bool {
	if !p.IsAgentWorking() {
		return false
	}

	source := snap.Kind
	if source == "" {
		source = "terminal"
	}
	log.Printf("switch-mode: interrupted in-flight %s turn before %s respawn (pane=%s)",
		source, switchTargetLabel(target), p.ID)

	if snap.Kind == "native" {
		// Native: stdin interrupt control_request. Mirrors interruptNative — deny
		// any open approval first so the tool doesn't run, then interrupt.
		if adapter, ok := p.NativeAdapterRef(); ok {
			for _, ap := range p.drainNativePending() {
				if frame := adapter.EncodeApproval(ap.RequestID, ap.ToolUseID, false, ap.RawInput, "interrupted"); len(frame) > 0 {
					_, _ = p.WriteNoPromptRecord(frame)
				} else {
					p.setNativePending(ap)
				}
			}
			if frame := adapter.EncodeInterrupt(); len(frame) > 0 {
				_, _ = p.WriteNoPromptRecord(frame)
			}
		}
		waitFor(switchModeSettleTimeout, switchModeSettlePoll, func() bool {
			return !p.IsAgentWorking()
		})
		return true
	}

	// Terminal: ESC interrupts Claude's TUI generation. Recency-based state
	// can't confirm the abort (the post-interrupt redraw keeps the pane busy),
	// but the PROMPT BOX can: after Esc lands, Claude redraws its "interrupted"
	// prompt, and claudePromptVisible is exactly the steady-state signal the
	// deferred-switch watcher already trusts (turnSettledForSwitch). So poll it
	// instead of sleeping fixed delays: send ESC, wait briefly for the prompt —
	// only if it hasn't appeared send the second ESC (stubborn TUI states) —
	// then wait out the remaining grace or the prompt, whichever comes first.
	// Same worst-case wall time as the old fixed sleeps (~500ms), typical case
	// ~75-150ms. The forced-kill + repair backstop covers the rare case the
	// interrupt didn't land. p.Write is the same choke point user keystrokes use.
	_, _ = p.Write([]byte{0x1b})
	if !waitFor(switchModeSecondInterruptDelay, switchModeSettlePoll, p.claudePromptVisible) {
		_, _ = p.Write([]byte{0x1b})
	}
	waitFor(switchModeTerminalGrace, switchModeSettlePoll, p.claudePromptVisible)
	return true
}

// waitFor polls cond every `poll` until it returns true or `timeout` elapses,
// returning whether cond was satisfied within the deadline. A cheap bounded
// settle-wait used by the graceful-switch interrupt.
func waitFor(timeout, poll time.Duration, cond func() bool) bool {
	if cond() {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		if cond() {
			return true
		}
	}
	return cond()
}

// switchTargetLabel renders the target Kind for log lines ("" => "terminal").
func switchTargetLabel(target string) string {
	if target == "" {
		return "terminal"
	}
	return target
}

// switchSpawnOpts builds the SpawnOpts that recreate a pane under `target`
// during a mode switch. Extracted (and pure) so the dims decision and the
// resume-parity fields are unit-testable, mirroring resumeSpawnOpts. When
// switching TO native, Spawn rewrites Cmd to the headless claude argv (with
// --resume <ClaudeSessionID>), so Cmd stays nil for native; when switching AWAY
// from native (back to terminal) the right continuation is the INTERACTIVE
// `claude --resume` TUI (a native pane's snap.Cmd is the headless stream-json
// argv, which is broken in a real terminal).
//
// ClaudeSessionID + ClaudeTranscriptPath are carried in BOTH directions so the
// respawn resumes the same conversation (and Spawn's transcript-on-disk resume
// guard sees the real path instead of re-deriving it). TitleOverride rides
// along so a Terminal↔Focus flip doesn't silently drop a custom pane title.
//
// Dims: a terminal target prefers the client's LIVE measured viewport
// (liveCols/liveRows) so the fresh PTY comes up at the CURRENT window width.
// Without a live measurement (the usual case for a Focus→Terminal switch — a
// native pane has no mounted xterm to measure), fall back to the persisted
// dims but mark them DimsUnverified so the first client attach re-measures and
// nudges a redraw — never trust a possibly-stale snapshot width for the TUI's
// first paint (that's the mis-wrapped-until-manual-refresh bug).
func switchSpawnOpts(snap PaneSnapshot, target string, liveCols, liveRows uint16) SpawnOpts {
	snap, _ = healPaneSnapshot(snap)
	agent := snap.Agent
	opts := SpawnOpts{
		AutoMode:             snap.AutoMode,
		NeverAutoAnswer:      snap.NeverAutoAnswer,
		ID:                   snap.ID,
		CreatedAt:            snap.CreatedAt,
		Cwd:                  snap.Cwd,
		Title:                snap.Title,
		Cols:                 snap.Cols,
		Rows:                 snap.Rows,
		ClaudeSessionID:      strings.TrimSpace(snap.ClaudeSessionID),
		ClaudeTranscriptPath: strings.TrimSpace(snap.ClaudeTranscriptPath),
		TitleOverride:        snap.TitleOverride,
		Origin:               snap.Origin.clone(),
		Review:               snap.Review.clone(),
		Identity:             snap.Identity.clone(),
		Kind:                 target,
		Agent:                agent,
		CodexThreadID:        snap.CodexThreadID,
		MuseSessionID:        snap.MuseSessionID,
		// Provider is preserved across a Terminal↔Native flip so a GLM pane
		// keeps routing to Z.ai after the switch.
		Provider: snap.Provider,
		// Effort is preserved across the flip too, so a Term→Focus switch keeps
		// the pane's reasoning level instead of dropping to claude's default.
		Effort: snap.Effort,
		Model:  snap.Model,
		// Role is preserved across the flip so an orchestrator pane keeps its
		// brief in either renderer (Spawn re-derives it into the new argv).
		Role:        snap.Role,
		ContextPack: snap.ContextPack,
		// Carry the "always allow" set across the flip: switch-mode respawns
		// the SAME conversation (claude --resume <session>), so the user's
		// opt-ins should survive a Terminal↔Native toggle. It's only applied
		// for a native target (Spawn ignores it otherwise).
		NativeAllowlist: snap.NativeAllowlist,
	}
	if target != "native" {
		if snap.Kind == "native" {
			switch effectiveAgentKind(agent) {
			case agentCodex:
				if threadID := strings.TrimSpace(opts.CodexThreadID); threadID != "" {
					opts.Cmd = []string{"codex", "resume", threadID}
				} else {
					opts.Cmd = []string{"codex"}
				}
			case agentMuse:
				if sess := strings.TrimSpace(opts.MuseSessionID); sess != "" {
					opts.Cmd = []string{"muse", "resume", sess}
				} else {
					opts.Cmd = []string{"muse"}
				}
			default:
				if sess := opts.ClaudeSessionID; sess != "" {
					opts.Cmd = []string{"claude", "--resume", sess}
				} else {
					opts.Cmd = []string{"claude"}
				}
			}
		} else {
			opts.Cmd = cloneStringSlice(snap.Cmd)
		}
		if liveCols > 0 && liveRows > 0 {
			opts.Cols, opts.Rows = liveCols, liveRows
		} else {
			opts.DimsUnverified = true
		}
	}
	return opts
}

// confirmSwitchHealthy verifies a freshly-respawned pane actually came up. For a
// native pane this catches the `--resume` failure mode: a bad session id makes
// the headless agent exit almost immediately, so we give it a short grace window
// and check it's still running. A terminal respawn is trusted to be healthy if
// Spawn returned (its readLoop is up and the shell doesn't self-exit).
//
// The grace is a CAP, not a fixed cost: once the pane has been alive for
// switchModeHealthFloor AND shows a positive health signal (native: the first
// system/init streamed; terminal: first PTY output), we confirm early — the
// common healthy switch settles in ~switchModeHealthFloor instead of always
// paying the full grace. fullGrace=true (retry attempts) disables the early
// exit: retries follow a session-lock/socket race, exactly where a process can
// emit output and STILL die late, so they wait out the whole window.
func (a *apiServer) confirmSwitchHealthy(p *Pane, target string, fullGrace bool) error {
	if p == nil {
		return errPaneRespawnNil
	}
	// Check BOTH directions for a quick exit. A native `--resume` dies on a bad
	// session id; a TERMINAL `claude --resume` can ALSO die immediately when the
	// just-killed native agent's claude child hasn't released the session lock
	// yet (the handoff race) — so a fresh interactive claude that exits inside
	// the grace window means the switch lost the race and the caller must retry.
	// (Switch-mode targets are always a long-lived claude/agent, never a fast
	// one-shot command, so a sub-grace exit is unambiguously a failure.)
	start := time.Now()
	healthGrace := switchModeHealthGrace
	if target == "native" && effectiveAgentKind(p.Snapshot().Agent) == agentCodex {
		healthGrace = codexSwitchModeHealthGrace
	}
	deadline := start.Add(healthGrace)
	floor := start.Add(switchModeHealthFloor)
	for time.Now().Before(deadline) {
		if p.Snapshot().Status == PaneExited {
			return errNativeSessionDied
		}
		if !fullGrace && !time.Now().Before(floor) && switchHealthSignal(p, target) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.Snapshot().Status == PaneExited {
		return errNativeSessionDied
	}
	if target == "native" && effectiveAgentKind(p.Snapshot().Agent) == agentCodex && !p.nativeGotInit.Load() {
		return errors.New("Codex Focus app-server did not establish a ready thread")
	}
	return nil
}

// switchHealthSignal reports the earliest positive "the respawn is genuinely
// up" evidence for the target kind: a native agent's first stream-json
// system/init (a bad --resume exits without one), or a terminal PTY's first
// output bytes (a healthy claude TUI paints well inside the grace window).
func switchHealthSignal(p *Pane, target string) bool {
	if target == "native" {
		return p.nativeGotInit.Load()
	}
	return p.buffer.TotalWritten() > 0
}

// rollbackSwitchMode re-spawns the pane under its ORIGINAL kind + command so a
// failed switch leaves the user exactly where they started. Best-effort: returns
// the restored pane (or nil if even the rollback spawn failed — at which point
// the pane is genuinely gone and the user must re-create it, but we never leave
// a dead native pane parked in state).
func (a *apiServer) rollbackSwitchMode(snap PaneSnapshot) *Pane {
	opts := SpawnOpts{
		AutoMode:             snap.AutoMode,
		NeverAutoAnswer:      snap.NeverAutoAnswer,
		ID:                   snap.ID,
		CreatedAt:            snap.CreatedAt,
		Cwd:                  snap.Cwd,
		Title:                snap.Title,
		Cols:                 snap.Cols,
		Rows:                 snap.Rows,
		ClaudeSessionID:      snap.ClaudeSessionID,
		ClaudeTranscriptPath: strings.TrimSpace(snap.ClaudeTranscriptPath),
		TitleOverride:        snap.TitleOverride,
		Origin:               snap.Origin.clone(),
		Review:               snap.Review.clone(),
		Identity:             snap.Identity.clone(),
		Kind:                 snap.Kind,
		Agent:                snap.Agent,
		CodexThreadID:        snap.CodexThreadID,
		// Preserve Provider on rollback too: without this a GLM pane whose
		// mode-flip fails health-check would silently downgrade to Anthropic.
		Provider: snap.Provider,
		// Effort likewise restored on rollback so a failed flip lands the pane
		// exactly where it started, reasoning level included.
		Effort: snap.Effort,
		Model:  snap.Model,
		// Role is preserved across the rollback too, so a failed flip lands the
		// pane exactly where it started, orchestrator brief included.
		Role:        snap.Role,
		ContextPack: snap.ContextPack,
		// Restore the user's "always allow" set on rollback so a failed flip
		// leaves the pane exactly where it started, allowlist included.
		NativeAllowlist: snap.NativeAllowlist,
	}
	if snap.Kind != "native" {
		opts.Cmd = cloneStringSlice(snap.Cmd)
		// The rollback PTY has no live client measurement either — mark the
		// persisted dims unverified so the first attach re-measures (same
		// mis-wrap guard as switchSpawnOpts / resumeSpawnOpts).
		opts.DimsUnverified = true
	}
	p, err := a.spawnPaneForRequest(opts)
	if err != nil {
		return nil
	}
	return p
}

// ---------------------------------------------------------------------------
// Deferred mode switch (when:"deferred")
//
// A deferred switch waits for the agent's CURRENT turn to finish, THEN flips
// Terminal↔Native — instead of force-interrupting (which would abort the turn).
// The HTTP request returns 200 immediately after arming; a single standalone
// watcher goroutine per pane polls turnSettledForSwitch and runs
// performSwitchMode once the turn has settled.
//
// Concurrency guards:
//   - pendingSwitchMu is a LEAF lock (the only thing it guards is the pointer);
//     never taken while holding nativeMu/subsMu/p.mu.
//   - pendingSwitchWatching (atomic CAS false→true) gates exactly one watcher
//     goroutine per pane.
//   - pendingDeferredSwitch.fired (atomic CAS, via claimPendingSwitch) gates a
//     single performSwitchMode fire even if two ticks race.
//   - performSwitchMode runs ON the watcher goroutine — NOT the parser/readLoop
//     — so killing the pane (which closes p.done) can't self-deadlock.
// ---------------------------------------------------------------------------

// deferredSwitchMaxWait caps how long the watcher waits for the turn to settle
// before giving up (logging + clearing the pending switch). The cap is the
// backstop for a pane that never reaches a positive settle signal (e.g. a
// terminal pane that never redraws an AwaitingInput prompt). We NEVER
// auto-interrupt on the cap — that would defeat the user's intent (don't
// disturb the running turn); we simply abandon the deferred switch.
const deferredSwitchMaxWait = 10 * time.Minute

// deferredSwitchPoll is the watcher's tick interval. 200ms is responsive enough
// (a fired switch feels prompt) while keeping the per-tick cost trivial — the
// settle check uses the cached computeState variant, so a tick that finds the
// turn still running does no procfs walk.
const deferredSwitchPoll = 200 * time.Millisecond

// setPendingSwitch arms (or re-targets) a deferred switch on the pane. Leaf
// lock; replaces any prior pending entry (a later arm supersedes an earlier).
func (p *Pane) setPendingSwitch(target string) {
	p.setPendingSwitchAt(target, time.Now().UnixMilli())
}

// setPendingSwitchAt is setPendingSwitch with an explicit arm time — the boot
// restore path passes the PERSISTED RequestedAt so the watcher's max-wait cap
// spans the daemon restart instead of restarting from boot.
func (p *Pane) setPendingSwitchAt(target string, requestedAt int64) {
	if requestedAt <= 0 {
		requestedAt = time.Now().UnixMilli()
	}
	p.pendingSwitchMu.Lock()
	p.pendingSwitch = &pendingDeferredSwitch{
		Target:      target,
		RequestedAt: requestedAt,
	}
	p.pendingSwitchMu.Unlock()
}

// clearPendingSwitch drops any armed deferred switch (cancel / "now"-supersede /
// max-wait giveup). The watcher observes nil on its next tick and exits.
func (p *Pane) clearPendingSwitch() {
	p.pendingSwitchMu.Lock()
	p.pendingSwitch = nil
	p.pendingSwitchMu.Unlock()
}

// pendingSwitchSnapshot returns a copy of the armed deferred switch (or nil) for
// the DTO / the watcher's cancellation check. Copied under the lock so the read
// never races a concurrent set/clear. The copy is a shallow value copy: the
// atomic `fired` flag is NOT carried (the snapshot is read-only).
func (p *Pane) pendingSwitchSnapshot() *pendingDeferredSwitch {
	p.pendingSwitchMu.Lock()
	defer p.pendingSwitchMu.Unlock()
	if p.pendingSwitch == nil {
		return nil
	}
	return &pendingDeferredSwitch{
		Target:      p.pendingSwitch.Target,
		RequestedAt: p.pendingSwitch.RequestedAt,
	}
}

// claimPendingSwitch single-fire CAS-claims the armed deferred switch: it returns
// the target and ok=true to EXACTLY ONE caller (the one that wins the `fired`
// CAS on the live entry), and ok=false to everyone else (no entry, or already
// claimed). The winner is the only goroutine allowed to run performSwitchMode.
func (p *Pane) claimPendingSwitch() (target string, ok bool) {
	p.pendingSwitchMu.Lock()
	pend := p.pendingSwitch
	p.pendingSwitchMu.Unlock()
	if pend == nil {
		return "", false
	}
	if !pend.fired.CompareAndSwap(false, true) {
		return "", false
	}
	return pend.Target, true
}

// armDeferredSwitch records a deferred switch and ensures a watcher goroutine is
// running. If a watcher is already active (the CAS loses) we've only updated the
// target via setPendingSwitch — that running watcher will pick up the new target
// on its next tick. The armed entry is persisted so it survives a daemon restart
// (the boot path restores it and calls ensureSwitchWatcher).
func (a *apiServer) armDeferredSwitch(p *Pane, target string) {
	p.setPendingSwitch(target)
	a.persistPendingSwitch(p)
	a.ensureSwitchWatcher(p)
}

// ensureSwitchWatcher starts the (single) deferred-switch watcher goroutine for
// a pane whose pendingSwitch is armed — the CAS half of armDeferredSwitch,
// without re-stamping the entry. Boot restore calls this for panes that came
// back from panes.json with a persisted pending switch.
func (a *apiServer) ensureSwitchWatcher(p *Pane) {
	if p.pendingSwitchWatching.CompareAndSwap(false, true) {
		go a.deferredSwitchWatcher(p)
	}
}

// persistPendingSwitch re-persists the pane so its (just-armed or just-cleared)
// deferred switch reaches panes.json. Best-effort: a persist failure never
// blocks the live arm/cancel — worst case a restart drops the intent, exactly
// the pre-persistence behavior.
func (a *apiServer) persistPendingSwitch(p *Pane) {
	if a.panes == nil {
		return
	}
	if err := a.panes.Put(p); err != nil {
		log.Printf("switch-mode: persist pending switch failed (pane=%s): %v", p.ID, err)
	}
}

// deferredSwitchWatcher polls the pane until its current turn settles, then fires
// the armed deferred switch via performSwitchMode (ON this goroutine — safe to
// kill the pane here). It exits — releasing the single-watcher gate — when:
//   - the pane dies (<-p.done): abandon.
//   - the pending switch is cleared/superseded (snapshot==nil): nothing to do.
//   - the turn settles and the switch fires (or another claimer won).
//   - the max-wait cap elapses: log, clear pending, give up (never auto-interrupt).
func (a *apiServer) deferredSwitchWatcher(p *Pane) {
	defer p.pendingSwitchWatching.Store(false)

	ticker := time.NewTicker(deferredSwitchPoll)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			// Pane died — the watcher (and any pending switch) is moot.
			return
		case <-ticker.C:
			pend := p.pendingSwitchSnapshot()
			if pend == nil {
				// Cancelled or superseded by a "now" switch.
				return
			}
			// The max-wait cap is anchored on the entry's OWN arm time (not
			// watcher start): a re-targeted arm extends it naturally, and a
			// watcher re-created after a daemon restart honors the time the
			// user has ALREADY waited instead of restarting the clock.
			if time.Now().After(time.UnixMilli(pend.RequestedAt).Add(deferredSwitchMaxWait)) {
				log.Printf("switch-mode: deferred switch to %s gave up after %s (pane=%s never settled) — abandoning (no auto-interrupt)",
					switchTargetLabel(pend.Target), deferredSwitchMaxWait, p.ID)
				p.clearPendingSwitch()
				a.persistPendingSwitch(p)
				return
			}
			if !p.turnSettledForSwitch() {
				continue
			}
			// Acquire the shared respawn single-flight before firing; if a refresh,
			// resume, or "now" switch is mid-respawn, leave this deferred switch
			// armed and retry on the next tick rather than racing a second kill.
			if !p.tryAcquireRespawn() {
				continue
			}
			target, ok := p.claimPendingSwitch()
			if !ok {
				p.releaseRespawn()
				// Cleared between the snapshot and the claim, or already fired.
				return
			}
			log.Printf("switch-mode: deferred switch to %s firing now (pane=%s turn settled)",
				switchTargetLabel(target), p.ID)
			if _, err := a.performSwitchMode(p, target, true, 0, 0); err != nil {
				log.Printf("switch-mode: deferred switch to %s failed (pane=%s): %v",
					switchTargetLabel(target), p.ID, err)
			}
			p.releaseRespawn()
			// Whether the switch succeeded (pane replaced by a fresh struct) or
			// rolled back, this pane's deferred switch is done. The old struct's
			// pending is discarded with it; nothing more to do here.
			return
		}
	}
}

// turnSettledForSwitch is the deferred-switch settle predicate: it reports
// whether the agent has handed the turn back to the user, so a switch can fire
// LOSSLESSLY (without interrupting). It is STRICTER than !IsAgentWorking().
//
//   - A parked approval (hasNativePending) means the agent is still mid-turn,
//     waiting on the user to answer — NOT settled.
//   - Native: settle when the explicit in-turn flag clears (turn_end/result/
//     error flipped nativeInTurn false).
//   - Terminal: settle on a POSITIVE "turn handed back" signal — Claude's visible
//     prompt box (claudePromptVisible) or a live choice menu (StateChoicePrompt).
//     claudePromptVisible is the un-ack-gated form of StateAwaitingInput: it stays
//     true for as long as the prompt is shown, so — unlike the ONE-SHOT
//     StateAwaitingInput, which the 200ms watcher tick could miss between the
//     prompt's appearance and the next cursor-blink redraw, then never see again
//     until the next user input (a stall to the max-wait cap) — an armed switch
//     fires promptly, including when it was armed while Claude was already idle.
//     CRITICAL: we still do NOT settle on bare StateIdle/StateRecent/StateBusy.
//     Claude's TUI can pause >3s mid-turn and read StateIdle, but it shows the
//     spinner, not the prompt box, so claudePromptVisible stays false and the
//     in-flight turn is never aborted — the exact loss this feature prevents.
//
// A NEW user turn re-lights nativeInTurn / hides the prompt box, so the predicate
// naturally returns false again and the watcher keeps waiting — correct: the user
// is still working, so the switch should stay deferred.
func (p *Pane) turnSettledForSwitch() bool {
	if p.hasNativePending() {
		return false
	}
	if p.Kind == "native" {
		return !p.nativeInTurnNow()
	}
	// A live terminal choice menu settles (the agent is parked on the user); the
	// cached state read keeps this cheap. Otherwise the prompt box is the signal.
	if p.computeStateCached() == StateChoicePrompt {
		return true
	}
	return p.claudePromptVisible()
}
