package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"soluterminal-shared/machineauth"
)

// attentionGrace is how long a pane must STAY in a needs-user attention
// state (awaiting-input / choice-prompt) before we push the phone. Its job
// is presence-filtering: a user at the keyboard answers most prompts within
// seconds, and only a sustained wait means "you are genuinely needed on the
// phone." Auto-approval suppression no longer rides this constant — the
// attention engine (attention.go) reports auto-pending while auto-mode /
// standing orders are expected to act, and pushEffectiveState holds the
// grace clock at zero for that whole window, however long the eval takes.
// The clock starts only when attention flips to needs-user. Mirrors the 15s
// grace on the in-app pane.awaiting_approval event (daemon/aria_watch.go)
// and the aria desktop watcher, so phone + desktop fire on the same gate.
const attentionGrace = 15 * time.Second

// replyFinishThreshold is the minimum busy duration for a "Claude finished"
// push. Low because reply-complete is already scoped to panes you were pinged
// about (see Start) — the engaged-set, not the duration, is the noise control;
// this just skips trivial sub-5s bounces. Mirrors activity.go's busyFinishThreshold.
const replyFinishThreshold = 5 * time.Second

// Pusher watches every pane for the attention-requiring states
// (awaiting-input / choice-prompt) and, once a pane has been waiting for
// attentionGrace WITHOUT being cleared, posts a notification to the cloud
// so it can fan out web push to the user's phone.
//
// Runs as a single ticker goroutine started from main.go when --relay
// is set. Without --relay there's no cloud to notify; the pusher
// becomes a no-op (Start returns immediately).
type Pusher struct {
	// private reports whether the person allows only approved devices; when
	// it does, notifications carry no session content (minimisePushBody).
	private func() bool
	// alertsHandled reports a live notifications-handler lease: Code alerts
	// (codeAlertKinds) then ask the cloud to skip FCM. Nil = never.
	alertsHandled func() bool

	cloudBase string
	token     string
	mgr       *Manager
	ws        *WorkspaceStore // for mapping paneID → workspace name in notify payloads
	client    *http.Client
	metrics   *Metrics // optional; nil = no telemetry

	// Rate-limit key is (paneID, kind) — NOT just paneID — so an
	// awaiting-input, choice-prompt, and reply-complete push for the same pane
	// have independent budgets and don't suppress each other.
	mu        sync.Mutex
	lastFired map[rateKey]time.Time
	startedAt time.Time
}

// pushCooldown is the per-(pane,kind) re-notify window. Flat 5m to MATCH the
// aria desktop watcher's dedup window (internal/fatigue defaultDedupWindow) so
// the phone and desktop re-surface a repeating prompt on the same cadence. The
// old exponential 1m→30m backoff over-suppressed mobile — desktop kept showing
// prompts the phone had gone quiet on. The 15s attentionGrace already filters
// the flapping that the backoff was guarding against, so a flat window is safe.
const pushCooldown = 5 * time.Minute

type rateKey struct {
	paneID string
	kind   string // "choice-prompt" | "awaiting-input" | "reply-complete"
}

// setToken swaps the machine token used to sign notify requests. Called from
// the token-rotation callback (a different goroutine than the fire loop), so
// it's guarded by the same mu that fire's getToken reads under. An empty token
// (Disconnect) stops every send from this pusher.
func (p *Pusher) setToken(token string) {
	p.mu.Lock()
	p.token = token
	p.mu.Unlock()
}

// getToken reads the current machine token under mu (token rotation can swap it
// concurrently with a fire).
func (p *Pusher) getToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.token
}

func NewPusher(cloudBase, token string, mgr *Manager, ws *WorkspaceStore, metrics *Metrics) *Pusher {
	return &Pusher{
		cloudBase: cloudBase,
		token:     token,
		mgr:       mgr,
		ws:        ws,
		client:    &http.Client{Timeout: 10 * time.Second},
		lastFired: map[rateKey]time.Time{},
		metrics:   metrics,
		startedAt: time.Now(),
	}
}

// Start begins polling pane state. A notification is sent once a pane has
// stayed in an attention state continuously for attentionGrace and has not
// been cleared (by auto-approval, the user answering, or Claude moving on).
// A prompt that resolves inside the grace window never pushes.
//
// Stops cleanly when ctx is cancelled.
func (p *Pusher) Start(ctx context.Context) {
	last := map[string]PaneRuntimeState{}
	grace := newGraceTracker(p.startedAt, attentionGrace)
	finish := newFinishTracker(replyFinishThreshold)
	// Panes with a live attention notification on some lock screen. When the
	// attention state clears (answered elsewhere, ARIA, Claude moved on) or the
	// pane dies, fire a "clear" so the phone retracts the now-stale
	// notification instead of leaving an actionable Approve pointing at
	// nothing. Marked on every grace-fire, including rate-limited ones (an
	// earlier notification is likely still showing).
	alerted := map[string]bool{}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now := time.Now()
			for _, pane := range p.mgr.List() {
				att, _ := pane.AttentionNow()
				cur := pushEffectiveState(pane.computeStateCached(), att)
				prev := last[pane.ID]
				last[pane.ID] = cur

				// --- "Claude finished" — only for a pane we pinged for
				// attention (a session you're in a loop with), never a
				// background pane "just left there". ---
				if finish.eval(pane.ID, cur, prev, now) {
					snap := pane.Snapshot()
					if snap.ClaudeSessionID != "" || pane.HasClaudeDescendant() {
						if p.recentlyFired(pane.ID, "reply-complete") {
							log.Printf("pusher: SKIP pane %s reply-complete (rate-limited)", snap.ID)
						} else {
							log.Printf("pusher: FIRE pane %s reply-complete title=%q", snap.ID, snap.Title)
							go p.fire(pane, "reply-complete")
						}
					}
				}

				// --- retract a stale attention notification ---
				if alerted[pane.ID] && !isAttentionState(cur) {
					delete(alerted, pane.ID)
					log.Printf("pusher: CLEAR pane %s (attention resolved, state=%s)", pane.ID, cur)
					go p.fireClear(pane.ID)
				}

				// --- attention (15s grace) ---
				if grace.eval(pane.ID, cur, prev, now) != pushFire {
					continue
				}
				// Snapshot for Title is mutex-safe — a direct pane.Title read
				// would race with Rename().
				snap := pane.Snapshot()
				// Pinging you about this pane → await + announce its finish.
				// Mark even when the push is rate-limited below: the pane is
				// part of your active loop either way.
				finish.markEngaged(pane.ID, now)
				alerted[pane.ID] = true
				if p.recentlyFired(pane.ID, string(cur)) {
					log.Printf("pusher: SKIP pane %s state=%s (rate-limited, recent fire for same state)", snap.ID, cur)
					continue
				}
				log.Printf("pusher: FIRE pane %s state=%s title=%q (waited %s)", snap.ID, cur, snap.Title, attentionGrace)
				go p.fire(pane, string(cur))
			}
			// Housekeeping: drop tracking for panes that went away; expire stale
			// engagements (pinged but never finished within the hour).
			for id := range last {
				if _, ok := p.mgr.Get(id); !ok {
					if alerted[id] {
						delete(alerted, id)
						go p.fireClear(id)
					}
					delete(last, id)
					grace.forget(id)
					finish.forget(id)
				}
			}
			finish.expire(now, time.Hour)
		}
	}
}

// finishTracker decides when to push "Claude finished". It is the testable core
// of the reply-complete path: no Manager, no wall clock, no I/O. A finish is
// announced ONLY for a pane that was markEngaged (we pushed it an attention
// alert) and then completed a busy→idle turn of at least `threshold` — so the
// phone hears "done" for the loop you're in, never for a background session.
// Mirrors activity.go's synthetic busy→idle ("recent→idle") edge detection.
type finishTracker struct {
	threshold time.Duration
	busyAt    map[string]time.Time // when the pane entered StateBusy
	engaged   map[string]time.Time // pinged for attention, awaiting its finish
}

func newFinishTracker(threshold time.Duration) *finishTracker {
	return &finishTracker{
		threshold: threshold,
		busyAt:    map[string]time.Time{},
		engaged:   map[string]time.Time{},
	}
}

// markEngaged records that we pushed an attention alert for this pane.
func (f *finishTracker) markEngaged(paneID string, now time.Time) {
	f.engaged[paneID] = now
}

// eval updates busy tracking and reports whether the pane just finished a
// meaningful turn while engaged. Clears the engagement when it fires.
func (f *finishTracker) eval(paneID string, cur, prev PaneRuntimeState, now time.Time) bool {
	if prev != StateBusy && cur == StateBusy {
		f.busyAt[paneID] = now
	}
	if prev != StateRecent || cur != StateIdle {
		return false
	}
	busyStart, hadBusy := f.busyAt[paneID]
	delete(f.busyAt, paneID)
	if _, eng := f.engaged[paneID]; !hadBusy || !eng {
		return false
	}
	if now.Sub(busyStart) < f.threshold {
		return false
	}
	delete(f.engaged, paneID)
	return true
}

// expire drops engagements older than maxAge (pinged but never finished).
func (f *finishTracker) expire(now time.Time, maxAge time.Duration) {
	for id, t := range f.engaged {
		if now.Sub(t) > maxAge {
			delete(f.engaged, id)
		}
	}
}

// forget drops all state for a pane (it went away).
func (f *finishTracker) forget(paneID string) {
	delete(f.busyAt, paneID)
	delete(f.engaged, paneID)
}

// pushVerdict is the per-pane outcome of one grace evaluation tick.
type pushVerdict int

const (
	pushWait pushVerdict = iota // not attention, or still within the grace window
	pushFire                    // dwelled past the grace, still waiting — push now
)

// graceTracker is the testable core of the pusher's grace window: no Manager,
// no wall clock, no I/O. Feed it each pane's (cur, prev, now) per tick and it
// answers whether to push. State is reset on every change so a flicker
// out-and-back restarts the grace; it fires at most once per continuous dwell.
type graceTracker struct {
	startedAt time.Time
	grace     time.Duration
	enteredAt map[string]time.Time // when the pane entered its CURRENT attention state
	fired     map[string]bool      // already pushed/suppressed for the current dwell
}

func newGraceTracker(startedAt time.Time, grace time.Duration) *graceTracker {
	return &graceTracker{
		startedAt: startedAt,
		grace:     grace,
		enteredAt: map[string]time.Time{},
		fired:     map[string]bool{},
	}
}

func isAttentionState(s PaneRuntimeState) bool {
	return s == StateAwaitingInput || s == StateChoicePrompt
}

// pushEffectiveState is the pusher's view of a pane's state once the
// attention engine's verdict is folded in. While the engine says
// auto-approval is expected to handle the prompt (auto-pending), the pane is
// treated as busy — the grace clock doesn't run and nothing pushes; if the
// eval declines or escalates, attention flips to needs-user, the state
// "re-enters" choice-prompt from the pusher's perspective, and the normal
// 15s presence grace starts from THAT moment. An unknown attention ("",
// engine hasn't evaluated yet) is passed through unchanged so nothing is
// ever silently swallowed.
func pushEffectiveState(cur PaneRuntimeState, att PaneAttention) PaneRuntimeState {
	if isAttentionState(cur) && att == AttentionAutoPending {
		return StateBusy
	}
	return cur
}

// eval advances the tracker for one pane and reports whether to push this tick.
func (g *graceTracker) eval(paneID string, cur, prev PaneRuntimeState, now time.Time) pushVerdict {
	// Left (or never in) the attention state — cleared by auto-approval, the
	// user, or Claude resuming work. Reset so the next wait is a fresh window.
	if !isAttentionState(cur) {
		g.forget(paneID)
		return pushWait
	}

	// Startup grace: a pane already attention-bound within the first few
	// seconds of process start is pre-existing, not a fresh wait — suppress it
	// (handles auto-update restart where a standing prompt would otherwise push
	// the instant the new daemon boots). Marking fired keeps it silent until it
	// leaves and genuinely re-enters.
	if now.Sub(g.startedAt) < 5*time.Second {
		g.fired[paneID] = true
		return pushWait
	}

	// Fresh entry into an attention state: start the grace timer.
	if cur != prev {
		g.enteredAt[paneID] = now
		g.fired[paneID] = false
		return pushWait
	}

	// Still in the same attention state. Already handled this dwell?
	if g.fired[paneID] {
		return pushWait
	}
	entered, ok := g.enteredAt[paneID]
	if !ok {
		// Defensive: missed the entry edge — start timing now.
		g.enteredAt[paneID] = now
		return pushWait
	}
	if now.Sub(entered) < g.grace {
		return pushWait // still within grace — auto-approval may yet clear it
	}

	// Dwelled past the grace and still waiting → genuinely needs the user.
	// Fire once; suppress further evaluation for this continuous dwell.
	g.fired[paneID] = true
	return pushFire
}

// forget drops dwell state for a pane (left the attention state, or gone).
func (g *graceTracker) forget(paneID string) {
	delete(g.enteredAt, paneID)
	delete(g.fired, paneID)
}

// recentlyFired reports whether (pane, kind) fired within pushCooldown; if not,
// it records this fire and returns false. Flat window matched to the desktop
// dedup so phone and desktop re-surface on the same cadence. AckPane clears a
// pane's entries so a handled-then-recurring prompt re-notifies immediately.
func (p *Pusher) recentlyFired(paneID, kind string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := rateKey{paneID: paneID, kind: kind}
	if last, ok := p.lastFired[key]; ok && time.Since(last) < pushCooldown {
		return true
	}
	p.lastFired[key] = time.Now()
	return false
}

// AckPane is called when the user has acknowledged any pending attention
// state for the pane. Removes all rate-limit entries so the next genuine
// state transition fires immediately.
func (p *Pusher) AckPane(paneID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.lastFired {
		if k.paneID == paneID {
			delete(p.lastFired, k)
		}
	}
}

// fire POSTs a push for one pane. kind is the payload "state" the service
// worker renders against: "choice-prompt", "awaiting-input", or "reply-complete".
func (p *Pusher) fire(pane *Pane, kind string) {
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	snap := pane.Snapshot()
	// Enrich the payload with workspace name + cwd so the phone
	// notification can disambiguate between many panes sharing a
	// generic title like "shell". Workspace lookup may return "" when
	// the pane is in _current / Background / not in any named
	// workspace; the SW handles the empty case.
	var workspaceName string
	var ordinal int
	if p.ws != nil {
		workspaceName = p.ws.NameForPane(snap.ID)
		ordinal = p.ws.OrdinalForPane(snap.ID)
	}
	// Notification heading: prefer the Activity/generated title (what the pane
	// list shows) — user override, then Claude's cached ai-title, then the
	// rename title, then the workspace name. Prefix the pane's "#x" number so
	// the alert matches the pane the user sees in the list. Left empty → the
	// client falls back to the machine name (unchanged behavior).
	heading := strings.TrimSpace(snap.TitleOverride)
	if heading == "" {
		heading = strings.TrimSpace(pane.AITitleCache())
	}
	if heading == "" {
		heading = strings.TrimSpace(snap.Title)
	}
	if heading == "" {
		heading = workspaceName
	}
	if ordinal > 0 {
		if heading != "" {
			heading = fmt.Sprintf("#%d %s", ordinal, heading)
		} else {
			heading = fmt.Sprintf("#%d", ordinal)
		}
	}
	// Native lock-screen one-tap: carry the single open approval's requestId (+ a
	// tool/summary for the body) so the notification can resolve it directly.
	// Empty for terminal panes and parallel-approval panes (no safe single reqID).
	var reqID, toolName, summary string
	if pending := pane.listNativePending(); len(pending) == 1 {
		reqID = pending[0].RequestID
		toolName, summary = nativeApprovalSummary(pending[0])
	}
	// Attention reason (WHY the human is needed — a cautious decline, a
	// never-auto tool, escalation) rides along so the phone notification can
	// explain itself instead of a generic state line. Empty for plain
	// no-auto-policy prompts; older clouds ignore the extra field.
	_, attReason := pane.AttentionNow()
	// LLM-graded attention item, when the async summarizer already ran for
	// this prompt: the phone notification reads "wants to run a prod DB
	// migration — risky" instead of a generic state line. Empty when the
	// summary hasn't landed (push often fires first); older clients ignore
	// the extra fields.
	var aiSummary, aiRisk string
	if rec := pane.attentionSummaryNow(); rec != nil {
		aiSummary, aiRisk = rec.Summary, rec.Risk
	}
	body, _ := json.Marshal(map[string]string{
		"paneId":        snap.ID,
		"state":         kind,
		"title":         heading,
		"workspaceName": workspaceName,
		"cwd":           snap.Cwd,
		"requestId":     reqID,
		"toolName":      toolName,
		"summary":       summary,
		"reason":        attReason,
		"aiSummary":     aiSummary,
		"aiRisk":        aiRisk,
	})
	p.deliver(body, snap.ID, kind)
}

// FireAsks pushes new asks to the user ("user-ask"): title names who is
// waiting, summary is the ask (or how many). paneId is the newest ask's pane,
// so a tap opens it. The caller coalesces and spaces these (asks_push.go).
func (p *Pusher) FireAsks(paneID, title, summary string) bool {
	if p == nil {
		return false
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	workspaceName := ""
	if p.ws != nil {
		workspaceName = p.ws.NameForPane(paneID)
	}
	body, _ := json.Marshal(map[string]string{
		"paneId":        paneID,
		"state":         "user-ask",
		"title":         title,
		"summary":       summary,
		"workspaceName": workspaceName,
	})
	return p.deliver(body, paneID, "user-ask")
}

// FireSchedule pushes a schedule lifecycle notification. kind is
// "schedule-error" (always sent — a job failing while nobody watches is
// exactly what push exists for) or "schedule-fired" (per-schedule opt-in,
// Schedule.NotifyOnRun). The service worker / phone app renders the
// schedule name as the subject and detail as the body. Reuses the
// per-(key,kind) cooldown map under a "sched:" key prefix so a flapping
// 1-minute schedule can't buzz the phone more than once per window.
func (p *Pusher) FireSchedule(schedID, schedName, kind, detail string) {
	key := "sched:" + schedID
	if p.recentlyFired(key, kind) {
		return
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	body, _ := json.Marshal(map[string]string{
		"paneId":  key, // rides the paneId slot for client-side tag coalescing
		"state":   kind,
		"title":   schedName,
		"summary": detail,
	})
	p.deliver(body, key, kind)
}

// FireAgent pushes a managed-agent notification
// (docs/DESIGN-agents-surface.md §6.3). kind is "agent-error" — ALWAYS sent,
// whatever the agent's notification mode and whatever its quiet hours say, on
// the same reasoning FireSchedule's error kind carries — or "agent-report", a
// routine run report the CALLER has already policy-gated
// (ManagedAgentStore.publishRunNotification). This function deliberately knows nothing
// about modes: a policy decided in two places is a policy that disagrees with
// itself.
//
// Rides the same per-(key,kind) cooldown map as FireSchedule under an "agent:"
// key prefix, so a flapping every-minute agent cannot buzz the phone more than
// once per window, and its error and report kinds hold independent budgets.
func (p *Pusher) FireAgent(agentID, agentName, kind, detail string) {
	_ = p.FireAgentResult(agentID, agentName, kind, detail)
}

// FireAgentResult is the synchronous form used by the durable needs-you
// ledger. Its delivery timestamp advances only after the relay accepts the
// notification, preserving at-least-once behavior across network failures.
func (p *Pusher) FireAgentResult(agentID, agentName, kind, detail string) bool {
	if p == nil {
		return false
	}
	key := "agent:" + agentID
	if p.recentlyFired(key, kind) {
		return false
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	body, _ := json.Marshal(map[string]string{
		"paneId":  key, // rides the paneId slot for client-side tag coalescing
		"state":   kind,
		"title":   agentName,
		"summary": detail,
	})
	delivered := p.deliver(body, key, kind)
	if !delivered {
		p.mu.Lock()
		delete(p.lastFired, rateKey{paneID: key, kind: kind})
		p.mu.Unlock()
	}
	return delivered
}

// FireWorkflow pushes an actionable workflow lifecycle notification. The
// caller supplies the user-facing heading and latest stage detail. Reuses the
// standard per-(key,kind) cooldown so repeated durable transitions cannot
// buzz more than once per window.
func (p *Pusher) FireWorkflow(taskID, kind, title, detail string) {
	_ = p.FireWorkflowResult(taskID, kind, title, detail)
}

// FireWorkflowResult is the synchronous form used when a caller's durable
// dedup state must advance only after the relay accepted the notification.
func (p *Pusher) FireWorkflowResult(taskID, kind, title, detail string) bool {
	key := "workflow:" + taskID
	if p.recentlyFired(key, kind) {
		return false
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	body, _ := json.Marshal(map[string]string{
		"paneId":  key, // rides the paneId slot for client-side tag coalescing
		"state":   kind,
		"title":   title,
		"summary": detail,
	})
	delivered := p.deliver(body, key, kind)
	if !delivered {
		p.mu.Lock()
		delete(p.lastFired, rateKey{paneID: key, kind: kind})
		p.mu.Unlock()
	}
	return delivered
}

// FireProviderBilling emits one provider-scoped top-up notice for an active
// billing incident. generation changes only after confirmed recovery, so
// failures from different tasks and execution surfaces coalesce under the same
// key without preventing a later, distinct exhaustion episode from notifying.
func (p *Pusher) FireProviderBilling(provider string, generation uint64, title, detail string) bool {
	if p == nil || provider == "" || generation == 0 {
		return false
	}
	key := fmt.Sprintf("provider-billing:%s:%d", provider, generation)
	kind := "provider-billing"
	if p.recentlyFired(key, kind) {
		return false
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	body, _ := json.Marshal(map[string]string{
		"paneId":  key,
		"state":   kind,
		"title":   title,
		"summary": detail,
	})
	delivered := p.deliver(body, key, kind)
	if !delivered {
		p.mu.Lock()
		delete(p.lastFired, rateKey{paneID: key, kind: kind})
		p.mu.Unlock()
	}
	return delivered
}

// FireWorkflowPreauth is the one-shot notice after a checked-in repository
// policy's visible grace has elapsed and its approval decision is durable.
// Landing sends no repeat; the hint points the human back to the inspection
// and recovery surface without creating a remote approval action.
func (p *Pusher) FireWorkflowPreauth(taskID, taskTitle, class, branch string) {
	key := "workflow:" + taskID
	kind := "workflow-preauthorized"
	if p.recentlyFired(key, kind) {
		return
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	if branch == "" {
		branch = "solu/" + taskID
	}
	summary := fmt.Sprintf("Repo policy approved %s after its grace window. Inspect in Tasks; to undo after landing, revert branch %s and use Tasks to inspect or Re-land.", class, branch)
	body, _ := json.Marshal(map[string]string{
		"paneId": key, "state": kind,
		"title":   "Repo policy approved: " + taskTitle,
		"summary": summary,
	})
	p.deliver(body, key, kind)
}

// FireWorkflowAutopilot is the window-START notice for an approval-autopilot
// countdown (docs/DESIGN-approval-autopilot.md §4.2). It is deliberately a
// separate kind from FireWorkflowPreauth above: that one is the receipt for a
// decision already made, this one is the last chance to stop one. Conflating
// them under a single string would make "about to approve, you can still stop
// it" and "already approved" indistinguishable in logs, metrics, and the
// phone's notification history.
//
// The key is per-task, so the (paneID, kind) cooldown that rate-limits repeats
// of the SAME task's window can never swallow a second task's window opening
// in the same minute.
func (p *Pusher) FireWorkflowAutopilot(taskID, taskTitle, class string, decideAt time.Time) {
	key := "workflow:" + taskID
	kind := "workflow-autopilot"
	if p.recentlyFired(key, kind) {
		return
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	// §3.8's rule: copy always names the way out. Both ways out are named,
	// because the per-task veto and the global stop answer different fears.
	deadline := decideAt.UTC()
	summary := fmt.Sprintf("Autopilot approves this %s task at %s (%s) unless you veto. Veto in Tasks, or use Return everything to manual in Settings > Tasks.",
		class, deadline.Format(time.Kitchen)+" UTC", deadline.Format(time.RFC3339))
	body, _ := json.Marshal(map[string]string{
		"paneId": key, "state": kind,
		"title":   "Autopilot will approve: " + taskTitle,
		"summary": summary,
	})
	p.deliver(body, key, kind)
}

// FireJanitorCancelProposal is the lane-3 window-start notice. It never
// carries a remote cancel action; the deep-linked Tasks surface owns the veto.
func (p *Pusher) FireJanitorCancelProposal(taskID, taskTitle, reason string, decideAt time.Time) {
	key := "workflow:" + taskID
	kind := "workflow-janitor-cancel"
	if p.recentlyFired(key, kind) {
		return
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	summary := fmt.Sprintf("Board janitor proposes cancelling this moot task: %s. Veto in Tasks before %s.", reason, decideAt.Local().Format(time.RFC822))
	body, _ := json.Marshal(map[string]string{
		"paneId": key, "state": kind,
		"title":   "Proposed cancel: " + taskTitle,
		"summary": summary,
	})
	p.deliver(body, key, kind)
}

// FireEngineClosure announces the beginning of a proof-positive closure veto
// window. Execution itself never emits this push kind.
func (p *Pusher) FireEngineClosure(taskID, taskTitle, reason string, decideAt time.Time) {
	key := "workflow:" + taskID
	kind := "workflow-engine-closure"
	if p.recentlyFired(key, kind) {
		return
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	summary := fmt.Sprintf("The engine proved this record already landed and will close it at %s unless you veto in Tasks. %s", decideAt.Local().Format(time.RFC822), reason)
	body, _ := json.Marshal(map[string]string{
		"paneId": key, "state": kind,
		"title":   "Engine closure countdown: " + taskTitle,
		"summary": summary,
	})
	p.deliver(body, key, kind)
}

// FireProposalAutopilot announces a policy proposal's auto-accept window.
func (p *Pusher) FireProposalAutopilot(taskID, taskTitle, kind string, decideAt time.Time) {
	key := "workflow:" + taskID
	pushKind := "workflow-proposal-autopilot"
	if p.recentlyFired(key, pushKind) {
		return
	}
	if p.metrics != nil {
		p.metrics.PushFireTotal.Add(1)
	}
	summary := fmt.Sprintf("Policy will accept %s at %s unless you veto in Tasks.", strings.ReplaceAll(kind, "_", " "), decideAt.Local().Format(time.RFC822))
	body, _ := json.Marshal(map[string]string{
		"paneId": key, "state": pushKind,
		"title":   "Proposal auto-accept countdown: " + taskTitle,
		"summary": summary,
	})
	p.deliver(body, key, pushKind)
}

// deliver POSTs one already-marshalled notify payload to the cloud relay.
// logKey identifies the subject (pane ID or "sched:<id>") in logs/metrics.
// pushContentFields are the parts of a notification that describe what is on
// screen. With only approved devices allowed (trusted_devices.go) the person
// has chosen that the cloud should not see their sessions, so these are
// replaced with neutral text before the notification leaves the machine: the
// phone still learns which pane needs it (paneId, state, requestId) and shows
// the detail once it opens the pane through its own connection.
var pushContentFields = map[string]string{
	"title":         "SoluTerminal",
	"summary":       "Something needs your attention. Open the app to see it.",
	"workspaceName": "",
	"cwd":           "",
	"toolName":      "",
	"reason":        "",
	"aiSummary":     "",
	"aiRisk":        "",
}

func minimisePushBody(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	for k, neutral := range pushContentFields {
		if _, ok := m[k]; ok {
			m[k] = neutral
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func (p *Pusher) deliver(body []byte, logKey, kind string) bool {
	token := p.getToken()
	if token == "" {
		return false // disconnected: the token was cleared, send nothing
	}
	if p.private != nil && p.private() {
		body = minimisePushBody(body)
	}
	if codeAlertKinds[kind] && p.alertsHandled != nil && p.alertsHandled() {
		body = withSkipFCM(body)
	}
	url := p.cloudBase + "/api/internal/notify"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("pusher: build request: %v", err)
		return false
	}
	// Tamper-evidence + replay protection on the daemon→cloud notify
	// channel. Bearer alone authenticates the request, but a captured
	// payload could be replayed by anything that briefly sees the
	// header (proxy logs, browser dev tools on a misrouted request,
	// future debug surfaces). HMAC-SHA256 over "<timestamp>.<body>"
	// using the machine token as the key lets the cloud detect both
	// stale replays (timestamp skew check) and any body tampering.
	// Cloud accepts requests without these headers for backward-
	// compat during rolling deploys; it logs a warning when they're
	// missing so we can flip to strict once every daemon has rolled.
	machineauth.SignRequest(req, token, body, time.Now())

	resp, err := p.client.Do(req)
	if err != nil {
		if p.metrics != nil {
			p.metrics.PushErrorsTotal.Add(1)
		}
		slog.Error("push notify request failed", "url", url, "err", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		if p.metrics != nil {
			p.metrics.PushErrorsTotal.Add(1)
		}
		slog.Error("push notify rejected", "pane", logKey, "state", kind, "http", resp.StatusCode)
		return false
	}
	// Parse {sent: N} so the log line shows how many subscriptions
	// actually got fanned out to. Before the cloud returned this count,
	// "DELIVERED 204" was ambiguous — zero-sub wipes (Render cold start,
	// user never subscribed from this browser, user-ID mismatch) looked
	// identical to a normal fan-out. The cloud responds 200 with a JSON
	// body since the fix; older clouds return 204 with no body (during
	// the deploy window when daemon is ahead of cloud). Skip parsing
	// and the zero-subs warning in that legacy case so rolling deploys
	// don't spam false-positive warnings.
	if resp.StatusCode == http.StatusNoContent {
		log.Printf("pusher: DELIVERED pane %s state=%s http=%d (legacy cloud, sub-count unknown)", logKey, kind, resp.StatusCode)
		return true
	}
	var r struct {
		Sent int `json:"sent"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if p.metrics != nil {
		p.metrics.PushDeliveriesSum.Add(int64(r.Sent))
	}
	slog.Info("push delivered", "pane", logKey, "state", kind, "http", resp.StatusCode, "sent", r.Sent)
	if r.Sent == 0 {
		slog.Warn("push had zero subscriptions", "hint", "user needs to toggle bell off+on in the web UI to re-register")
	}
	return true
}

// fireClear tells the cloud to retract a previously-delivered attention
// notification for this pane (the prompt was answered elsewhere, ARIA handled
// it, or the pane died). Minimal payload — no Snapshot, so it's safe to call
// for an already-removed pane. The cloud routes state="clear" to FCM only (a
// data message the phone uses to cancel the local notification); web-push has
// no retraction, so it's skipped there. Best-effort: a lost clear just leaves
// the old notification, which is today's behavior.
func (p *Pusher) fireClear(paneID string) {
	token := p.getToken()
	if token == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{
		"paneId": paneID,
		"state":  "clear",
	})
	req, err := http.NewRequest("POST", p.cloudBase+"/api/internal/notify", bytes.NewReader(body))
	if err != nil {
		return
	}
	machineauth.SignRequest(req, token, body, time.Now())
	resp, err := p.client.Do(req)
	if err != nil {
		slog.Error("push clear request failed", "pane", paneID, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Error("push clear rejected", "pane", paneID, "http", resp.StatusCode)
	}
}

// httpsBaseFromRelayURL derives the HTTPS base URL for the cloud's
// HTTP endpoints from the wss:// (or ws://) relay URL the daemon is
// already configured with. Avoids a second --flag.
//
//	wss://cloud.example.com/relay
//	→ https://cloud.example.com
func httpsBaseFromRelayURL(relayURL string) string {
	u, err := url.Parse(relayURL)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "https"
	if u.Scheme == "ws" || u.Scheme == "http" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}
