package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// attention.go computes the per-pane ATTENTION signal: "who needs to act on
// this pane right now?" It exists because the raw runtime state
// (choice-prompt / awaiting-input) says a prompt EXISTS, not that the HUMAN is
// required — when ARIA auto-mode or a standing order is about to answer a
// prompt, alerting the user is pure noise (the notification lands, then the
// prompt auto-resolves seconds later). Before this signal, every notification
// channel keyed on the raw state independently and picked its own timing:
// the web client fired instantly, the pusher waited a blanket 15s, the
// aria-watcher another 15s. This file is the single source of truth they all
// consume instead.
//
// The signal has three values:
//
//	AttentionNone        — nothing is waiting on anyone.
//	AttentionAutoPending — a prompt is open but auto-approval is expected to
//	                       handle it; surfaces quietly ("⚡ handling…"), never
//	                       as an alert.
//	AttentionNeedsUser   — a human is required. THIS is the only value that
//	                       should ring phones, toast, or speak.
//
// needs-user is decided as EARLY as the daemon can know it: a pane with no
// auto policy is needs-user the moment the prompt appears; AskUserQuestion /
// ExitPlanMode are never auto-answered so they are needs-user instantly even
// in auto mode (with the reason attached); a cautious-mode deny-needle hit is
// needs-user instantly; an LLM decline flips it the moment the eval records
// its outcome (see Aria.noteAttentionOutcome). Only the genuinely-in-flight
// case waits — and even that is bounded by attentionAutoGrace so a hung or
// silently-failed eval can never starve a prompt without the user hearing
// about it.
type PaneAttention string

const (
	AttentionNone        PaneAttention = "none"
	AttentionAutoPending PaneAttention = "auto-pending"
	AttentionNeedsUser   PaneAttention = "needs-user"
)

// attentionAutoGrace bounds how long a pane may sit in auto-pending before
// escalating to needs-user anyway. Sized against the cautious-mode worst
// case (LLM eval 1-5s + the 5s cancel countdown ≈ 10s, plus a sweeper retry
// or two) while staying far under ariaAutoEvalMaxDuration (90s) — if
// auto-approval hasn't cleared the prompt by now, the user should hear about
// it; a late auto-approve after escalation just clears the alert (the pusher
// retracts, the badge drops).
const attentionAutoGrace = 25 * time.Second

// attentionEscalatedReason is the needs-user reason when auto-pending dwelled
// past attentionAutoGrace without resolving.
const attentionEscalatedReason = "auto-approval hasn't resolved this"

// paneAttentionRecord is the last-computed attention for a pane, stored
// atomically on the Pane so DTO builds and the pusher read it lock-free.
type paneAttentionRecord struct {
	State   PaneAttention
	Reason  string
	Episode string
	Prompt  attentionPromptKey
}

// Each runtime state uses its own prompt position. Native approval request IDs
// distinguish a changed pending set even when removing one request leaves the
// choice-prompt position unchanged.
type attentionPromptKey struct {
	State      PaneRuntimeState
	Pos        int64
	PendingIDs string
}

func promptKeyForAttention(p *Pane, f attentionFacts, choicePos int64) attentionPromptKey {
	key := attentionPromptKey{State: f.State, Pos: choicePos}
	if f.State == StateAwaitingInput {
		key.Pos = atomic.LoadInt64(&p.LastAckedAwaitingInputPos)
	}
	if f.IsNative && len(f.Pending) > 0 {
		var ids strings.Builder
		// listNativePending already sorts by RequestID. Length prefixes keep
		// distinct ID sets distinct even if an ID contains a separator.
		for _, pending := range f.Pending {
			if pending == nil {
				continue
			}
			ids.WriteString(strconv.Itoa(len(pending.RequestID)))
			ids.WriteByte(':')
			ids.WriteString(pending.RequestID)
		}
		key.PendingIDs = ids.String()
	}
	return key
}

// AttentionNow returns the pane's last-computed attention signal. Empty
// PaneAttention ("") means the engine hasn't evaluated this pane yet (first
// ~2s after boot/spawn) — consumers treat that as "unknown, assume the human
// is needed" so nothing is ever silently swallowed.
func (p *Pane) AttentionNow() (PaneAttention, string) {
	if r := p.attention.Load(); r != nil {
		return r.State, r.Reason
	}
	return "", ""
}

func (p *Pane) attentionEpisodeNow() (PaneAttention, string, string) {
	if r := p.attention.Load(); r != nil {
		return r.State, r.Reason, r.Episode
	}
	return "", "", ""
}

// setAttention stores the computed attention and reports whether it changed.
func (p *Pane) setAttention(a PaneAttention, reason string) bool {
	return p.setAttentionWithPrompt(a, reason, attentionPromptKey{}, false, p.attentionObservationSeq.Add(1))
}

// setAttentionForPrompt records a prompt observation made at call time.
// Refresh assigns its sequence before classification and calls the shared
// setter directly so a delayed observation cannot supersede a newer one.
func (p *Pane) setAttentionForPrompt(a PaneAttention, reason string, key attentionPromptKey) bool {
	return p.setAttentionWithPrompt(a, reason, key, true, p.attentionObservationSeq.Add(1))
}

func (p *Pane) setAttentionWithPrompt(a PaneAttention, reason string, key attentionPromptKey, hasKey bool, observation uint64) bool {
	p.flagMu.Lock()
	defer p.flagMu.Unlock()
	if observation <= p.attentionCommittedSeq {
		return false
	}
	// Record even an unchanged observation: otherwise a delayed older
	// Refresh could publish after a newer same-state observation returned.
	p.attentionCommittedSeq = observation
	prev := p.attention.Load()
	if !hasKey && prev != nil && a == AttentionNeedsUser && prev.State == a {
		// Direct decline updates have no new prompt key; retain the current
		// identity until Refresh supplies the authoritative one.
		key = prev.Prompt
	}
	visibleChanged := prev == nil && a != AttentionNone || prev != nil && (prev.State != a || prev.Reason != reason)
	newEpisode := a == AttentionNeedsUser && (prev == nil || prev.State != a || prev.Prompt != key)
	if prev != nil && !visibleChanged && !newEpisode {
		return false
	}
	next := &paneAttentionRecord{State: a, Reason: reason, Prompt: key}
	if a == AttentionNeedsUser {
		if newEpisode {
			// Prompt positions are not persisted, so restart begins a new episode.
			next.Episode = newAttentionEpisode()
		} else {
			next.Episode = prev.Episode
		}
	}
	p.attention.Store(next)
	return visibleChanged
}

// SetAutoDeclined records that an auto-approval evaluation looked at the
// pane's CURRENT prompt and left it for the user (LLM decline, never-auto
// tool, safety-needle hit). Scoped to the current menu position: a genuinely
// new prompt (LastAckedChoicePromptPos advances) invalidates the record, so
// a decline on menu A never mutes the alert for menu B.
func (p *Pane) SetAutoDeclined(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "auto-approval left this for you"
	}
	pos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
	p.autoDeclineMu.Lock()
	p.autoDeclineReason = reason
	p.autoDeclinePos = pos
	p.autoDeclineMu.Unlock()
}

// autoDeclineFor returns the recorded decline reason if it applies to the
// given menu position, "" otherwise.
func (p *Pane) autoDeclineFor(pos int64) string {
	p.autoDeclineMu.Lock()
	defer p.autoDeclineMu.Unlock()
	if p.autoDeclineReason == "" || p.autoDeclinePos != pos {
		return ""
	}
	return p.autoDeclineReason
}

// attentionFacts is the pure input to classifyAttention — everything the
// decision needs, gathered up front so the classifier itself is a testable
// function of plain values.
type attentionFacts struct {
	HumanOnlyReason string
	State           PaneRuntimeState
	AutoMode        string
	IsNative        bool
	// NativeAutoOn is the SOLU_ARIA_NATIVE_AUTO kill switch (native panes
	// only): off means ARIA leaves native approvals for the human.
	NativeAutoOn bool
	// AutoGateReason is autoAllowed()'s denial code ("" when allowed).
	// "paused" means auto-approval will NOT act; "burst-cap" is a transient
	// the sweeper retries through, so it stays auto-pending.
	AutoGateReason string
	// Pending is the native pane's open approvals (nil for terminal panes).
	Pending []*NativeApproval
	// DeclineReason is a still-valid SetAutoDeclined record ("" when none).
	DeclineReason string
	// InputHoldReason is computed from current terminal input, not a historic
	// eval outcome, so it disappears when the draft or prompt is resolved.
	InputHoldReason string
	// Dwell is how long the CURRENT menu has been waiting (re-zeroed when a
	// genuinely new menu appears).
	Dwell time.Duration
}

// classifyAttention decides the attention signal for one pane. Pure function
// of facts — no locks, no clocks, no I/O.
func classifyAttention(f attentionFacts) (PaneAttention, string) {
	if f.State != StateChoicePrompt && f.State != StateAwaitingInput {
		return AttentionNone, ""
	}
	// awaiting-input (Claude finished a reply and wants direction) is never
	// auto-answered — the auto-eval gate in aria_watch fires on choice-prompt
	// only — so it always needs the human.
	if f.State == StateAwaitingInput {
		return AttentionNeedsUser, ""
	}
	// choice-prompt from here down.
	if f.HumanOnlyReason != "" {
		return AttentionNeedsUser, f.HumanOnlyReason
	}
	if f.AutoMode == "" {
		return AttentionNeedsUser, ""
	}
	if f.IsNative && !f.NativeAutoOn {
		return AttentionNeedsUser, ""
	}
	switch f.AutoGateReason {
	case "":
		// allowed
	case "burst-cap":
		// Transient (5/sec limiter); the sweeper retries within seconds.
		// Escalation still bounds the wait.
	case "paused":
		return AttentionNeedsUser, "auto-approval is paused"
	default: // any future hard denial
		return AttentionNeedsUser, "auto-approval " + autoSuppressReason(f.AutoGateReason)
	}
	// Native panes carry structured approvals, so "will auto-mode even try?"
	// is answerable synchronously — no reason to wait for an eval to decline.
	for _, ap := range f.Pending {
		if ap == nil {
			continue
		}
		if isNonAutoApprovableNativeTool(ap.ToolName) {
			return AttentionNeedsUser, nativeNeverAutoReason(ap.ToolName)
		}
		// Deterministic cautious-mode safety needles.
		if f.AutoMode == "cautious" {
			if r := nativeCautiousAutoDenyReason(ap); r != "" {
				return AttentionNeedsUser, "won't auto-approve — " + r
			}
		}
	}
	if f.DeclineReason != "" {
		return AttentionNeedsUser, f.DeclineReason
	}
	if f.InputHoldReason != "" {
		if f.Dwell >= attentionAutoGrace {
			return AttentionNeedsUser, f.InputHoldReason
		}
		return AttentionAutoPending, f.InputHoldReason
	}
	if f.Dwell >= attentionAutoGrace {
		return AttentionNeedsUser, attentionEscalatedReason
	}
	return AttentionAutoPending, ""
}

// attentionEngine owns the dwell clocks and publishes attention changes.
// One instance per daemon, created in main.go before panes attach; Aria is
// late-bound via SetAria (it is constructed later in startup). All entry
// points funnel through Refresh so the ticker, the native turn-edge hook,
// and the eval-outcome hook can never disagree.
type attentionEngine struct {
	mgr  *Manager
	aria atomic.Pointer[Aria]
	// Test seam for a deterministic delayed observation before publication.
	beforeAttentionStore func(*Pane, attentionPromptKey)

	mu      sync.Mutex
	entered map[string]time.Time // when the auto-approval clock for the current menu started
	pos     map[string]int64     // LastAckedChoicePromptPos of that menu
	wasAuto map[string]bool      // whether an auto policy could act at the last eval
}

func newAttentionEngine(mgr *Manager) *attentionEngine {
	return &attentionEngine{
		mgr:     mgr,
		entered: map[string]time.Time{},
		pos:     map[string]int64{},
		wasAuto: map[string]bool{},
	}
}

// SetAria late-binds the Aria instance (pause/cap gates).
// Nil-safe: before it's called (or when ARIA is disabled) panes classify as
// if no auto policy can act — i.e. prompts are needs-user, which is correct.
func (e *attentionEngine) SetAria(a *Aria) { e.aria.Store(a) }

// dwell advances the per-pane auto-approval clock and returns how long
// auto-approval has had a chance to act on the current menu. It restarts on
// a genuinely new menu (pos advances) AND when auto-candidacy flips on
// (autoCandidate false→true): the escalation grace measures how long AUTO
// has been failing to resolve the prompt, so enabling auto mode on a prompt
// that already sat needs-user for minutes must not instantly escalate — the
// just-enabled policy gets its full grace. active=false (pane left
// choice-prompt) resets everything.
func (e *attentionEngine) dwell(paneID string, pos int64, active, autoCandidate bool, now time.Time) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !active {
		delete(e.entered, paneID)
		delete(e.pos, paneID)
		delete(e.wasAuto, paneID)
		return 0
	}
	prevPos, hadPos := e.pos[paneID]
	autoJustEnabled := autoCandidate && !e.wasAuto[paneID]
	e.wasAuto[paneID] = autoCandidate
	if !hadPos || prevPos != pos || autoJustEnabled {
		e.pos[paneID] = pos
		e.entered[paneID] = now
		return 0
	}
	return now.Sub(e.entered[paneID])
}

// Refresh recomputes one pane's attention and publishes a "pane-attention"
// event when it changed. Safe from any goroutine: pane reads are atomic or
// snapshot-based, and dwell state has its own lock. Called by the 2s ticker,
// by the native turn-edge hook (instant classification when an approval
// parks/resolves), and by Aria's eval-outcome hook (instant needs-user on a
// decline).
func (e *attentionEngine) Refresh(p *Pane) {
	if p == nil {
		return
	}
	observation := p.attentionObservationSeq.Add(1)
	now := time.Now()
	state := p.computeStateCached()
	pos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)

	f := attentionFacts{
		State:         state,
		IsNative:      false,
		NativeAutoOn:  true,
		DeclineReason: p.autoDeclineFor(pos),
	}
	snap := p.Snapshot()
	if state == StateChoicePrompt && paneWaitingForFolderTrust(p) {
		f.HumanOnlyReason = folderTrustHint
	}
	f.AutoMode = snap.AutoMode
	if snap.NeverAutoAnswer {
		f.AutoMode = ""
		f.HumanOnlyReason = "automatic answers locked by user"
	}
	f.IsNative = snap.Kind == "native"
	if f.IsNative {
		f.NativeAutoOn = nativeAriaAutoEnabled()
		f.Pending = p.listNativePending()
	}
	if a := e.aria.Load(); a != nil {
		if _, reason := a.autoAllowed(); reason != "" {
			// Only consult the gate when an auto policy could act — a pane
			// with no policy is already needs-user without a reason.
			f.AutoGateReason = reason
		}
	} else {
		// No ARIA (disabled / not yet constructed): nothing will auto-act,
		// so any auto policy on the pane is inert. Classify as needs-user
		// by clearing the policy inputs.
		f.AutoMode = ""
	}

	// The escalation clock runs only while an auto policy could act (see
	// dwell) — computed after the facts so a just-enabled policy resets it.
	autoCandidate := f.AutoMode != "" && (!f.IsNative || f.NativeAutoOn)
	if autoCandidate && !f.IsNative && state == StateChoicePrompt {
		_, f.InputHoldReason = autoFireUserHold(p, now)
	}
	if !f.IsNative && state != StateChoicePrompt {
		// While no menu covers the input box, check the recorder's draft
		// against it, so a phantom cannot hold the next approval.
		p.reconcileComposerDraft(now)
	}
	f.Dwell = e.dwell(p.ID, pos, state == StateChoicePrompt, autoCandidate, now)

	att, reason := classifyAttention(f)
	key := promptKeyForAttention(p, f, pos)
	// LLM-graded attention items: kick (or clear) the one-per-prompt summary.
	// Runs on every tick, not just transitions, so a NEW prompt on an
	// already-needs-user pane still gets summarized; maybeSummarizeAttention's
	// fast path is a map lookup, so the 2s ticker cost is negligible.
	if a := e.aria.Load(); a != nil {
		if att == AttentionNeedsUser {
			a.maybeSummarizeAttention(p, reason)
		} else {
			a.noteAttentionLeft(p)
		}
	}
	if e.beforeAttentionStore != nil {
		e.beforeAttentionStore(p, key)
	}
	if !p.setAttentionWithPrompt(att, reason, key, true, observation) {
		return
	}
	if e.mgr == nil || e.mgr.bus == nil {
		return
	}
	e.mgr.bus.publish(uiEvent{
		Type:            "pane-attention",
		PaneID:          snap.ID,
		PaneTitle:       snap.Title,
		Ts:              now.UnixMilli(),
		Attention:       string(att),
		AttentionReason: reason,
	})
}

// CurrentFor returns the pane's attention consistent with the runtime state
// the caller just computed for its DTO. The stored record is at most ~2s
// stale (ticker cadence) — fresh enough everywhere EXCEPT the moment a
// prompt appears or resolves between ticks. A DTO built inside that window
// pairs state=choice-prompt with the pre-prompt record ("none", or empty
// right after spawn), and every attention consumer briefly counts the pane
// as needing the human until the tick reclassifies it auto-pending — the
// exact flash-then-auto-resolves noise this engine exists to kill (observed
// live on the footer's "1 needs you" chip, 2026-07-10). When the record and
// the state disagree about a prompt being open, reclassify synchronously
// (atomic reads plus two small locks) and re-read.
func (e *attentionEngine) CurrentFor(p *Pane, state PaneRuntimeState) (PaneAttention, string) {
	att, reason := p.AttentionNow()
	promptOpen := state == StateChoicePrompt || state == StateAwaitingInput
	recordSeesPrompt := att == AttentionAutoPending || att == AttentionNeedsUser
	if promptOpen != recordSeesPrompt {
		e.Refresh(p)
		att, reason = p.AttentionNow()
	}
	return att, reason
}

// Start runs the 2s evaluation ticker until ctx is done. The ticker is the
// backstop that notices dwell escalation and terminal-pane state changes;
// native approval park/resolve and eval declines refresh eagerly through the
// hooks, so their latency doesn't ride this cadence.
func (e *attentionEngine) Start(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			live := map[string]struct{}{}
			for _, p := range e.mgr.List() {
				live[p.ID] = struct{}{}
				e.Refresh(p)
			}
			e.mu.Lock()
			for id := range e.entered {
				if _, ok := live[id]; !ok {
					delete(e.entered, id)
					delete(e.pos, id)
					delete(e.wasAuto, id)
				}
			}
			e.mu.Unlock()
		}
	}
}

func newAttentionEpisode() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("attention episode: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}
