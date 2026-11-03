package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ask_pane and wait_for_pane: the waiting half of agent collaboration. Before
// these, an agent that sent work to another pane had to guess how long to
// sleep (a blind 45 s wait in testing) and then dig the answer out of raw
// scrollback. ask_pane sends, waits for that pane's agent to finish its turn
// and returns only the reply; wait_for_pane does the waiting alone.

const (
	askPaneDefaultTimeout  = 240 * time.Second
	askPaneMaxTimeout      = 900 * time.Second
	waitPaneDefaultTimeout = 120 * time.Second
	collabPollInterval     = 500 * time.Millisecond
	collabSettleScreen     = 2 * time.Second
	collabSettlePrompt     = time.Second
	spawnReadyTimeout      = 45 * time.Second
)

// collabSleep, collabScreenText, collabPaneState and queuedAskTTL are
// swapped by tests.
var (
	collabSleep      = time.Sleep
	collabScreenText = paneVisibleScreen
	collabPaneState  = func(p *Pane) PaneRuntimeState { return p.computeStateCached() }
	// queuedAskTTL bounds how long a queued ask is waited on, as long as the
	// daemon follows the message in Codex's queue (queueWatchLimit).
	queuedAskTTL = queueWatchLimit
)

type collabResult struct {
	Pane          string  `json:"pane"`
	PaneID        string  `json:"pane_id"`
	Status        string  `json:"status"` // replied | needs_approval | still_working | exited | idle | unsent | expired
	State         string  `json:"state"`
	Source        string  `json:"source,omitempty"`
	Reply         string  `json:"reply,omitempty"`
	Screen        string  `json:"screen,omitempty"`
	WaitedSeconds float64 `json:"waited_seconds"`
	Hint          string  `json:"hint,omitempty"`
	// Delivery is "queued" when a working Codex queued ask_pane's message.
	Delivery string `json:"delivery,omitempty"`
}

func collabTimeout(args map[string]any, def time.Duration) time.Duration {
	if v, ok := args["timeout_seconds"].(float64); ok && v > 0 {
		d := time.Duration(v * float64(time.Second))
		if d > askPaneMaxTimeout {
			d = askPaneMaxTimeout
		}
		return d
	}
	return def
}

func (a *apiServer) collabPaneLabel(p *Pane) string {
	snap := p.Snapshot()
	return paneLabel(paneSlotsByID(a.ws)[snap.ID], snap.Title, paneSummaryAgent(p, snap))
}

func collabJSON(r collabResult) any {
	b, err := json.Marshal(r)
	if err != nil {
		return toolErr("marshal: " + err.Error())
	}
	return toolOK(string(b))
}

// waitForPaneTurn polls until the pane's agent has finished the turn started
// at or after since (or, with a zero since, until the pane settles).
// paneTurnBaseline is the pane's last turn as it stood before ask_pane sent
// its message. A reply only counts when its prompt is newer than this, so an
// earlier turn (or another session's transcript) is never taken as the answer.
type paneTurnBaseline struct {
	Since    time.Time // when the message was sent; zero = wait for any turn
	Path     string
	PromptAt time.Time
	// Queued is the message when a working Codex or Claude queued it. Other
	// queued requests can run first, so only a turn whose prompt is this
	// message counts, and none while the agent still lists it as queued.
	Queued string
	// HeldID is the held message when the person was typing in the pane
	// (pane_draft_guard.go): the wait follows that one message, never an
	// earlier one with the same text.
	HeldID string
	// Continued: wait_for_pane picking up a timed-out queued ask_pane; the
	// work it waited on already started, so a settled screen can end it.
	Continued bool
}

func (a *apiServer) paneTurnBaselineFor(p *Pane) paneTurnBaseline {
	r := a.paneLatestReply(p)
	b := paneTurnBaseline{Since: time.Now()}
	if r.Source == "transcript" {
		b.Path, b.PromptAt = r.Path, r.PromptAt
	}
	return b
}

// freshTurn reports whether a transcript reply belongs to the asked turn.
func (b paneTurnBaseline) freshTurn(r paneReply) bool {
	if b.Since.IsZero() {
		return true
	}
	if b.Queued != "" && !queuedPromptMatches(r.Prompt, b.Queued) {
		return false
	}
	if r.PromptAt.IsZero() || r.PromptAt.Before(b.Since.Add(-3*time.Second)) {
		return false
	}
	return b.Path == "" || r.Path != b.Path || r.PromptAt.After(b.PromptAt)
}

// queuedPromptMatches reports whether a turn's prompt is the queued message:
// the whole prompt equals the whole message once the sender labels are off
// and whitespace is ignored. Not a prefix (queued requests can start alike)
// and not a substring (a short "yes" is inside many other prompts). Codex
// sends one queued input per turn, so the prompt is exactly the message.
func queuedPromptMatches(prompt, queued string) bool {
	want := conditionalDraftText(stripSenderLabel(queued))
	return want != "" && conditionalDraftText(stripSenderLabel(prompt)) == want
}

// queuedAskOpen reports a wait result after which the queued ask is still
// waiting for its own reply: still working, or held by an approval menu
// (often in the turn ahead of it) that wait_for_pane continues after.
func queuedAskOpen(status string) bool {
	return status == "still_working" || status == "needs_approval"
}

// queuedAskBaselines remembers, per target pane and caller, the baseline of
// an ask_pane whose message a working Codex queued and that timed out before
// the reply. The caller's wait_for_pane continues from it, so a request queued
// ahead of theirs is not returned as their reply. An entry ends with a final
// status, after queuedAskTTL, or when either pane closes.
type queuedAskBaselines struct {
	mu sync.Mutex
	m  map[queuedAskKey]paneTurnBaseline
}

type queuedAskKey struct{ pane, caller string }

func (q *queuedAskBaselines) put(pane, caller string, b paneTurnBaseline) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.m == nil {
		q.m = map[queuedAskKey]paneTurnBaseline{}
	}
	// A caller that never came back leaves its entry; clear long-dead ones.
	for k, old := range q.m {
		if time.Since(old.Since) >= 2*queuedAskTTL {
			delete(q.m, k)
		}
	}
	q.m[queuedAskKey{pane, caller}] = b
}

func (q *queuedAskBaselines) get(pane, caller string) (paneTurnBaseline, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	b, ok := q.m[queuedAskKey{pane, caller}]
	return b, ok
}

func (q *queuedAskBaselines) drop(pane, caller string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.m, queuedAskKey{pane, caller})
}

// dropPane forgets the queued asks to or from a closed pane, so a reused id
// never inherits them.
func (q *queuedAskBaselines) dropPane(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for k := range q.m {
		if k.pane == id || k.caller == id {
			delete(q.m, k)
		}
	}
}

// paneRemoved runs when a pane closes: agents' open requests about it and
// queued asks to or from it end.
func (a *apiServer) paneRemoved(id string) {
	a.dropPaneConfirmsFor(id)
	a.queuedAsks.dropPane(id)
}

// queuedAskPutBack reports that the pane's unsent receipt is this queued
// message: an interrupted turn moved it back into the input box.
func queuedAskPutBack(p *Pane, queued string) bool {
	msg := p.Snapshot().UnsentMessage
	return msg != nil && queuedPromptMatches(msg.body, queued)
}

func (a *apiServer) waitForPaneTurn(p *Pane, base paneTurnBaseline, timeout time.Duration, callers ...string) collabResult {
	start := time.Now()
	res := collabResult{Pane: a.collabPaneLabel(p), PaneID: p.ID}
	var settledSince, promptSince time.Time
	sawWork := base.Since.IsZero() || base.Continued
	agent, claude := paneAgentProvider(p)
	codex := agent && !claude
	for {
		state := collabPaneState(p)
		// Output age can read a quiet Codex turn, or its closing redraws, as
		// settled; Codex's own Working row and queue hint do not. A message
		// still in Codex's or Claude's queue has not gone in yet.
		if agent && (state == StateIdle || state == StateRecent || state == StateAwaitingInput) {
			screen := collabScreenText(p)
			if (codex && codexTurnRunningText(screen)) || (base.Queued != "" && agentQueueHolds(screen, codex, base.Queued)) {
				state = StateBusy
			}
		}
		res.State = string(state)
		res.WaitedSeconds = time.Since(start).Round(100 * time.Millisecond).Seconds()
		if p.Snapshot().Status == PaneExited || state == StateExited {
			res.Status = "exited"
			res.Screen = paneScreenText(p)
			return res
		}
		// A message held while the person types has not gone in yet; one
		// they discarded, or that could not be typed, never will.
		if base.HeldID != "" {
			switch status, err := p.heldDeliveryStatus(base.HeldID); status {
			case string(deliveryHeld):
				state = StateBusy
				res.State = string(state)
			case "discarded", "gone", "expired", "failed":
				res.Status = "unsent"
				res.Screen = paneScreenText(p)
				res.Hint = heldUnsentHint(status, err)
				return res
			}
		}
		if base.Queued != "" && queuedAskPutBack(p, base.Queued) {
			res.Status = "unsent"
			res.Screen = paneScreenText(p)
			res.Hint = "Your queued message was put back in the agent's input box, unsent (an interrupted Codex turn, or Up in Claude). The pane shows it as an unsent message; tell the user rather than resending."
			return res
		}
		if base.Queued != "" && time.Since(base.Since) >= queuedAskTTL {
			res.Status = "expired"
			res.Screen = paneScreenText(p)
			res.Hint = fmt.Sprintf("Your queued message got no reply within %s of being queued, so this wait has ended. Check the pane with read_pane_tail, or tell the user, before resending.", queuedAskTTL)
			return res
		}
		reply := a.paneReplyTo(p, base)
		res.Source = reply.Source
		// The turn that ran a queued message is finished at its task_complete,
		// whatever the turns queued behind it are doing now.
		if base.Queued != "" && reply.Source == "transcript" && reply.Complete && base.freshTurn(reply) {
			res.Status = "replied"
			res.Reply = reply.Reply
			return res
		}
		switch state {
		case StateBusy, StateRecent:
			sawWork = true
			settledSince, promptSince = time.Time{}, time.Time{}
		case StateChoicePrompt:
			settledSince = time.Time{}
			if promptSince.IsZero() {
				promptSince = time.Now()
			}
			if time.Since(promptSince) >= collabSettlePrompt {
				res.Status = "needs_approval"
				res.Screen = paneScreenText(p)
				res.Hint = "The pane is waiting on an approval menu. Answer it with answer_prompt (or tell the user), then call wait_for_pane."
				return res
			}
		default:
			promptSince = time.Time{}
			if settledSince.IsZero() {
				settledSince = time.Now()
			}
		}
		if reply.Source == "transcript" {
			if base.freshTurn(reply) && reply.Complete && state != StateBusy && state != StateChoicePrompt {
				res.Status = "replied"
				res.Reply = reply.Reply
				if len(callers) > 0 {
					a.noteWorkerReplyRead(callers[0], p, reply)
				}
				return res
			}
		} else if sawWork && !settledSince.IsZero() && time.Since(settledSince) >= collabSettleScreen {
			res.Status = "replied"
			res.Reply = reply.Reply
			if base.Queued != "" {
				res.Hint = "Your message was queued behind other work and the pane's transcript could not be found, so this is the settled screen, not a verified reply to your message: look for your answer in it (it may also hold the earlier work's output)."
			}
			return res
		}
		if time.Since(start) >= timeout {
			res.Status = "still_working"
			res.Reply = reply.Reply
			res.Hint = "Not finished yet. Call wait_for_pane to keep waiting; the reply so far is included."
			return res
		}
		collabSleep(collabPollInterval)
	}
}

func (a *apiServer) mcpToolAskPane(args map[string]any, callerPane string) any {
	return a.mcpToolAskPaneAs(args, splitMCPCaller(callerPane))
}

func (a *apiServer) mcpToolAskPaneAs(args map[string]any, c paneCaller) any {
	callerPane := c.ID
	ref, _ := args["pane_id"].(string)
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return toolErr("text is required: the message to send to the other pane's agent")
	}
	paneID := a.resolvePaneRef(ref, callerPane)
	if paneID == "" {
		return toolErr("no pane matches " + ref + " (pass a pane id or #number from list_panes)")
	}
	if paneID == callerPane {
		return toolErr("that is your own pane; ask a different pane")
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID)
	}
	state := p.computeStateCached()
	switch state {
	case StateExited:
		return toolErr("pane " + paneID + " has exited")
	case StateChoicePrompt:
		return toolErr("pane " + paneID + " is waiting on an approval menu; answer it with answer_prompt first, or tell the user")
	}
	agent, claude := paneAgentProvider(p)
	// A busy pane with no queue SoluTerminal knows gets nothing typed, however
	// short the message (send_input's guard skips one- and two-letter keys).
	if !agent && p.Snapshot().Kind != "native" && sendInputPaneBusy(p) {
		return a.refuseBusyPaneAs(p, strings.TrimRight(unescapeAriaInput(text), "\r\n"), c)
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	base := a.paneTurnBaselineFor(p)
	// Never steer: a working Codex that offers no queue key gets no Enter.
	sent, outcome := a.mcpToolSendInputOutcomeAs(map[string]any{"pane_id": paneID, "text": text}, deliveryNoSteer, c)
	if m, ok := sent.(map[string]any); ok && m["isError"] == true && outcome != deliveryHeld {
		return sent
	}
	// A working Codex or Claude queues the message itself (pane_delivery.go);
	// other panes refuse a busy pane in send_input. A message to a busy
	// Claude is matched by its text even when its queue was not seen: Claude
	// may take it with the running turn's next tool result.
	if outcome == deliveryQueued || outcome == deliveryHeld || (agent && claude && (state == StateBusy || state == StateRecent)) {
		// send_input turns a typed "\n" into a line break and drops invisible
		// format characters; match what was sent.
		base.Queued = withoutInvisibleFormat(strings.TrimRight(unescapeAriaInput(text), "\r\n"))
	}
	if outcome == deliveryHeld {
		// Found right after the send; empty when it was typed meanwhile, and
		// then the wait goes by Queued alone.
		sender := a.paneMessageSenderAs(c, paneID)
		if sender == "" {
			sender = "SoluTerminal"
		}
		// The pane holds the message without control characters; its turn's
		// prompt will be that text.
		base.Queued = heldText(base.Queued)
		base.HeldID = p.pendingHeldID(sender, base.Queued, c.messageKey())
	}
	res := a.waitForPaneTurn(p, base, collabTimeout(args, askPaneDefaultTimeout), callerPane)
	a.queuedAsks.drop(paneID, c.bucket())
	if base.Queued != "" && queuedAskOpen(res.Status) {
		a.queuedAsks.put(paneID, c.bucket(), base)
	}
	if outcome == deliveryQueued {
		res.Delivery = string(deliveryQueued)
		switch res.Status {
		case "still_working":
			res.Hint = "That agent was mid-turn, so your message is in its queue and goes in when the agent takes it. Call wait_for_pane to keep waiting for the reply; do not resend."
		case "needs_approval":
			res.Hint += " Your message is still in its queue; do not resend."
		}
	}
	noteHeldAsk(p, base.HeldID, &res)
	return collabJSON(res)
}

// noteHeldAsk marks a wait result whose message is still held while the
// person types in that pane.
func noteHeldAsk(p *Pane, heldID string, res *collabResult) {
	if heldID == "" || !queuedAskOpen(res.Status) {
		return
	}
	if status, _ := p.heldDeliveryStatus(heldID); status != string(deliveryHeld) {
		return
	}
	res.Delivery = string(deliveryHeld)
	reason := p.heldDeliveryReason()
	if strings.HasPrefix(reason, unsentInBoxPrefix) {
		res.Hint = "held: " + reason + ", so nothing has been typed yet. Your message waits on the pane, queued behind it, and goes in once the person sends or clears that message. Call wait_for_pane to keep waiting; do not resend."
		return
	}
	if reason == heldMenuReason {
		res.Hint = "queued: a menu is open in that pane, so nothing has been typed yet. Your message is held on the pane with your name and goes in by itself once the menu is answered. Call wait_for_pane to keep waiting; do not resend."
		return
	}
	if reason == "" {
		res.Hint = "queued: other held messages go into that pane first, so nothing has been typed yet. Your message is held on the pane with your name and goes in by itself after them. Call wait_for_pane to keep waiting; do not resend."
		return
	}
	res.Hint = "queued: the person is typing in that pane, so nothing has been typed yet. Your message is held on the pane with your name and goes in by itself once their input box is empty and they have stopped typing. Call wait_for_pane to keep waiting; do not resend."
}

// heldUnsentHint explains a held message that will not be typed.
func heldUnsentHint(status string, err error) string {
	switch status {
	case "discarded":
		return "The person discarded your message: it was held while they typed in that pane, and nothing was typed. Do not resend it unless they ask."
	case "expired":
		return fmt.Sprintf("Your message was held for %s while the person typed in that pane and was dropped, untyped. Tell the user rather than resending.", heldDeliveryLimit)
	case "failed":
		return fmt.Sprintf("Your message was held while the person typed in that pane, and typing it afterwards failed: %v. Check the pane with read_pane_tail before resending.", err)
	}
	return "Your message was held while the person typed in that pane, and the pane's agent exited or restarted before it was typed. Nothing was typed."
}

func (a *apiServer) mcpToolWaitForPane(args map[string]any, callerPane string) any {
	return a.mcpToolWaitForPaneAs(args, splitMCPCaller(callerPane))
}

func (a *apiServer) mcpToolWaitForPaneAs(args map[string]any, c paneCaller) any {
	callerPane := c.ID
	ref, _ := args["pane_id"].(string)
	paneID := a.resolvePaneRef(ref, callerPane)
	if paneID == "" {
		return toolErr("no pane matches " + ref + " (pass a pane id or #number from list_panes)")
	}
	if paneID == callerPane {
		return toolErr("that is your own pane")
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return toolErr("no pane with id " + paneID)
	}
	// Continue a queued ask_pane that timed out: only its own turn counts,
	// and the wait ends for good once it is answered, put back, or expired.
	base, queued := a.queuedAsks.get(paneID, c.bucket())
	base.Continued = queued
	res := a.waitForPaneTurn(p, base, collabTimeout(args, waitPaneDefaultTimeout), callerPane)
	if queued {
		res.Delivery = string(deliveryQueued)
		switch {
		case res.Status == "still_working":
			res.Hint = "Your queued message has not been answered yet. Call wait_for_pane again to keep waiting; do not resend."
		case res.Status == "needs_approval":
			res.Hint += " Your queued message has not been answered yet; do not resend."
		}
		if !queuedAskOpen(res.Status) {
			a.queuedAsks.drop(paneID, c.bucket())
		}
		noteHeldAsk(p, base.HeldID, &res)
	}
	if res.Status == "replied" {
		res.Status = "idle"
	}
	return collabJSON(res)
}

// paneAgentReady reports whether a freshly spawned agent shows its input box.
func paneAgentReady(p *Pane, agent string) bool {
	state := p.computeStateCached()
	if state == StateAwaitingInput {
		return true
	}
	if state == StateBusy || state == StateRecent || state == StateChoicePrompt || state == StateExited {
		return false
	}
	screen := paneVisibleScreen(p)
	low := strings.ToLower(screen)
	if strings.Contains(low, "loading") || strings.Contains(low, "starting") || strings.Contains(low, "booting") {
		return false
	}
	return agentInputBoxVisible(screen, agent)
}

// agentInputBoxVisible requires the agent's real input box, not just its
// glyph: a first-run "trust this folder?" menu also draws ❯/› on its focused
// option, and a prompt sent there lands in the menu instead of the composer.
func agentInputBoxVisible(screen, agent string) bool {
	switch agent {
	case agentCodex:
		return looksLikeCodexPrompt(screen)
	case agentClaude:
		if _, known := composerDraft(screen, false); known {
			return true
		}
		// Unrecognised status line: fall back to the input glyph, but never
		// while a menu (trust prompt, picker) is on screen.
		low := strings.ToLower(screen)
		menu := strings.Contains(low, "enter to confirm") || isGridChoicePromptText(screen, false)
		return !menu && (strings.Contains(screen, "❯") || strings.Contains(screen, "│ >"))
	}
	return strings.TrimSpace(screen) != ""
}

// waitAgentReady waits for a spawned agent to accept input. Returns how long
// it took and whether it got there.
func waitAgentReady(p *Pane, agent string, timeout time.Duration) (time.Duration, bool) {
	start := time.Now()
	var readySince time.Time
	for time.Since(start) < timeout {
		if paneWaitingForFolderTrust(p) {
			return time.Since(start), false
		}
		if p.Snapshot().Status == PaneExited {
			return time.Since(start), false
		}
		if paneAgentReady(p, agent) {
			if readySince.IsZero() {
				readySince = time.Now()
			}
			if time.Since(readySince) >= time.Second {
				return time.Since(start), true
			}
		} else {
			readySince = time.Time{}
		}
		collabSleep(collabPollInterval)
	}
	return time.Since(start), false
}

// spawnFollowUp runs after spawn_pane created an agent pane: it waits for the
// agent to be ready and, when asked, sends the first prompt.
func (a *apiServer) spawnFollowUp(p *Pane, cmd []string, args map[string]any) map[string]any {
	agent := agentFromCmd(cmd)
	prompt, _ := args["prompt"].(string)
	wait := agent != ""
	if v, ok := args["wait_ready"].(bool); ok {
		wait = v
	}
	out := map[string]any{}
	if !wait && strings.TrimSpace(prompt) == "" {
		return out
	}
	took, ready := waitAgentReady(p, agent, spawnReadyTimeout)
	out["ready"] = ready
	out["ready_after_seconds"] = took.Round(100 * time.Millisecond).Seconds()
	if !ready && paneWaitingForFolderTrust(p) {
		out["status"] = "needs_user"
		out["state"] = string(StateChoicePrompt)
		out["hint"] = folderTrustHint
		if strings.TrimSpace(prompt) != "" {
			out["prompt_sent"] = false
			out["hint"] = folderTrustHint + " The initial prompt was not sent. After trusting the folder, use ask_pane to send the original prompt."
		}
		return out
	}
	if strings.TrimSpace(prompt) == "" {
		return out
	}
	if !ready {
		out["prompt_sent"] = false
		out["hint"] = "The agent did not show its input box in time, so the prompt was not sent. Check it with get_pane_state, then use ask_pane."
		return out
	}
	if !strings.HasSuffix(prompt, "\n") {
		prompt += "\n"
	}
	sent := a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": prompt})
	if m, ok := sent.(map[string]any); ok && m["isError"] == true {
		out["prompt_sent"] = false
		out["hint"] = fmt.Sprint(m["content"])
		return out
	}
	out["prompt_sent"] = true
	out["hint"] = "The prompt was sent. Call wait_for_pane to get the reply."
	return out
}
