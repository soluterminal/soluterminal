package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Screens captured from a real Codex CLI 0.166.0 (`codex --no-alt-screen
// --no-daemon`, 120x40, as SoluTerminal launches it), with only the working
// folder renamed. The probe message is the one each capture typed or queued.
const codexProbeMessage = "[from #3 Claude] queued probe: say CHERRY"

func codex0166Screen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex_0166", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCodex0166ScreensReadTheTurnState(t *testing.T) {
	cases := []struct {
		name                      string
		running, offered, menu    bool
		composerHolds, queueHolds bool
	}{
		// Idle after a turn: input box and footer, no Working row.
		{name: "idle"},
		// Mid-turn with an empty box: the Working row is the only sign.
		{name: "working", running: true},
		// Our paste in the box mid-turn: the footer offers Tab to queue.
		{name: "working_draft", running: true, offered: true, composerHolds: true},
		// After Tab: the box is empty and Codex lists the message as queued.
		{name: "queued", running: true, queueHolds: true},
		// After Esc interrupted that turn: Codex moved the queued message back
		// into the input box, where nothing would send it.
		{name: "restored", composerHolds: true},
		// A command approval during the turn: a menu, not a running turn the
		// message could be queued behind.
		{name: "approval", menu: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			screen := codex0166Screen(t, c.name)
			if got := codexTurnRunningText(screen); got != c.running {
				t.Errorf("turn running = %v, want %v", got, c.running)
			}
			if got := codexQueueOffered(screen); got != c.offered {
				t.Errorf("queue offered = %v, want %v", got, c.offered)
			}
			if got := isGridChoicePromptText(screen, true); got != c.menu {
				t.Errorf("menu = %v, want %v", got, c.menu)
			}
			if got := codexQueueHolds(screen, codexProbeMessage); got != c.queueHolds {
				t.Errorf("queue holds = %v, want %v", got, c.queueHolds)
			}
			if c.menu {
				return // the focused "› 1. Yes" row is not an input box
			}
			holds, known := composerHolds(screen, true, codexProbeMessage)
			if !known || holds != c.composerHolds {
				t.Errorf("composer holds = %v (known %v), want %v", holds, known, c.composerHolds)
			}
		})
	}
	// The raw-tail approval scan computeState runs finds the same menu.
	if len(approvalPromptMatches(codex0166Screen(t, "approval"), true)) == 0 {
		t.Fatal("computeState's approval scan missed the Codex 0.166 command approval")
	}
	// Idle is told apart from working by the Working row, not the box.
	if idle := codex0166Screen(t, "idle"); !looksLikeCodexPrompt(idle) || codexActiveTurnRE.MatchString(idle) {
		t.Fatal("idle capture should show the input box without a live turn")
	}
}

func TestCodexQueueHintAndHeldInputVariants(t *testing.T) {
	box := "› " + codexProbeMessage + "\n"
	for _, footer := range []string{
		"  tab to queue message                                   98% context left",
		"  tab to queue · Plan mode      98% context left", // narrow footer
	} {
		if !codexQueueOffered(box + footer) {
			t.Errorf("queue hint missed in %q", footer)
		}
	}
	// A rebound queue key is not Tab: never press Tab for it.
	if key, ok := codexQueueKey(box + "  ctrl+q to queue message"); !ok || string(key) != "\x11" {
		t.Errorf("rebound ctrl+q read as %q (%v), want ^Q", key, ok)
	}
	// Hint text inside the transcript, above the box, is not the footer.
	if codexQueueOffered("tab to queue message\n› Ask Codex to do anything\n  Context 2% used · GPT-6.1-Sol high · ~/x") {
		t.Error("the hint must sit under the input box")
	}
	pending := "• Working (3s • esc to interrupt)\n" +
		"• Messages to be submitted after next tool call (press esc to interrupt and send immediately)\n" +
		"  ↳ [from #3 Claude] queued probe: say\n    CHERRY\n" +
		"› Ask Codex to do anything\n"
	if !codexQueueHolds(pending, codexProbeMessage) {
		t.Error("a pending steer is held input too, wrapped or not")
	}
	// Two messages from one sender share the "[from …]" label; only the
	// text after it identifies the queued one.
	other := strings.Replace(codex0166Screen(t, "queued"), "↳ "+codexProbeMessage, "↳ [from #3 Claude] a different request entirely", 1)
	if codexQueueHolds(other, codexProbeMessage) {
		t.Error("a different queued message from the same sender matched")
	}
}

// stubQueueWatch makes the queued-message watcher poll fast and reports how
// it ended.
func stubQueueWatch(t *testing.T) <-chan string {
	t.Helper()
	ended := make(chan string, 4)
	prevSleep, prevInterval, prevEnded := queueWatchSleep, queueWatchInterval, queueWatchEnded
	queueWatchSleep = func(time.Duration) { time.Sleep(2 * time.Millisecond) }
	queueWatchInterval = time.Millisecond
	queueWatchEnded = func(_ *Pane, outcome string) { ended <- outcome }
	t.Cleanup(func() { queueWatchSleep, queueWatchInterval, queueWatchEnded = prevSleep, prevInterval, prevEnded })
	return ended
}

func waitQueueWatch(t *testing.T, ended <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ended:
		if got != want {
			t.Fatalf("queued message ended %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("queued message watch did not end (want %q)", want)
	}
}

func TestSubmitAgentMessageQueuesBehindAWorkingCodexTurn(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-queue")
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	done := runDelivery(p, codexProbeMessage)
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	// Enter would steer the running turn; Tab is Codex's queue key.
	expectFrame(t, frames, "queue key", "\t")
	screen.Store(codex0166Screen(t, "queued"))
	if got := <-done; got != deliveryQueued {
		t.Fatalf("outcome = %q, want queued", got)
	}
	expectNoFrame(t, frames, "no Enter after a queued message")
	// The turn ends and Codex sends the queued message itself.
	screen.Store(codex0166Screen(t, "idle"))
	waitQueueWatch(t, ended, string(deliverySubmitted))
	if msg := p.Snapshot().UnsentMessage; msg != nil {
		t.Fatalf("a sent message left an unsent receipt: %+v", msg)
	}
	if !strings.Contains(deliveryNotice(deliveryQueued), "queued") {
		t.Fatal("senders must be told the message is queued")
	}
}

func TestQueuedCodexMessagePutBackInTheBoxBecomesUnsent(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-queue-restored")
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	done := make(chan deliveryOutcome, 1)
	go func() {
		_, outcome, _ := submitAgentMessage(p, codexProbeMessage, false, "@lead · #1 Claude")
		done <- outcome
	}()
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "queue key", "\t")
	screen.Store(codex0166Screen(t, "queued"))
	if got := <-done; got != deliveryQueued {
		t.Fatalf("outcome = %q, want queued", got)
	}
	// An approval menu hides the queue: still waiting, nothing recorded.
	screen.Store(codex0166Screen(t, "approval"))
	time.Sleep(20 * time.Millisecond)
	if p.Snapshot().UnsentMessage != nil {
		t.Fatal("an approval menu covering the queue was read as the message leaving it")
	}
	// Esc interrupts the turn and Codex restores the queue into the box.
	screen.Store(codex0166Screen(t, "restored"))
	waitQueueWatch(t, ended, string(deliveryLeftInComposer))
	msg := p.Snapshot().UnsentMessage
	if msg == nil || msg.body != codexProbeMessage || msg.Sender != "@lead · #1 Claude" || msg.claude {
		t.Fatalf("restored message did not become the unsent receipt: %+v", msg)
	}
	expectNoFrame(t, frames, "the watcher never types")
}

func TestSubmitAgentMessageEntersWhenTheCodexTurnEndedFirst(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-queue-raced")
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	stubQueueWatch(t)
	done := runDelivery(p, codexProbeMessage)
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "queue key", "\t")
	// The turn had just ended: Codex took Tab as a submit, nothing is queued.
	screen.Store(codex0166Screen(t, "idle"))
	if got := <-done; got != deliverySubmitted {
		t.Fatalf("outcome = %q, want submitted", got)
	}
}

func TestSubmitAgentMessageEntersAnIdleCodex(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-idle-codex")
	var screen atomic.Value
	idleDraft := strings.Replace(codex0166Screen(t, "idle"), "› Ask Codex to do anything", "› "+codexProbeMessage, 1)
	screen.Store(idleDraft)
	stubDeliveryScreen(t, &screen)
	done := runDelivery(p, codexProbeMessage)
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "Enter", "\r")
	screen.Store(codex0166Screen(t, "idle"))
	if got := <-done; got != deliverySubmitted {
		t.Fatalf("outcome = %q, want submitted", got)
	}
}

// ask_pane no longer turns a busy Codex away: Codex queues the message and
// the caller is told it is queued behind the running turn.
func TestAskPaneQueuesForABusyCodexAndSaysSo(t *testing.T) {
	p, frames := newTypingHoldPane(t, "pane_codex")
	p.Agent = agentCodex
	atomic.StoreInt64(&p.LastActivityMs, time.Now().UnixMilli())
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	type askResult struct {
		text  string
		isErr bool
	}
	asked := make(chan askResult, 1)
	go func() {
		text, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_codex", "text": codexProbeMessage, "timeout_seconds": 0.5}, "pane_caller"))
		asked <- askResult{text, isErr}
	}()
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "queue key", "\t")
	screen.Store(codex0166Screen(t, "queued"))
	got := <-asked
	if got.isErr {
		t.Fatalf("ask_pane refused a busy Codex: %s", got.text)
	}
	var res collabResult
	if err := json.Unmarshal([]byte(got.text), &res); err != nil {
		t.Fatal(err)
	}
	if res.Delivery != "queued" || res.Status != "still_working" || !strings.Contains(res.Hint, "queue") {
		t.Fatalf("result: %+v", res)
	}
	screen.Store(codex0166Screen(t, "idle"))
	waitQueueWatch(t, ended, string(deliverySubmitted))
}

// wait_for_pane must not read a Codex whose screen shows a live turn as
// finished just because its output went quiet.
func TestWaitForPaneTreatsAQuietCodexTurnAsWorking(t *testing.T) {
	p, _ := newTypingHoldPane(t, "pane_quiet_codex")
	p.Agent = agentCodex
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-time.Minute).UnixMilli())
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working"))
	prev := collabScreenText
	collabScreenText = func(*Pane) string { return screen.Load().(string) }
	t.Cleanup(func() { collabScreenText = prev })
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	res := a.waitForPaneTurn(p, paneTurnBaseline{}, 2500*time.Millisecond)
	if res.Status != "still_working" || res.State != string(StateBusy) {
		t.Fatalf("quiet working Codex: %+v", res)
	}
}

// Two requests queued behind one Codex turn: the first one's answer is not
// the second one's reply, and nothing counts while ours is still queued.
func TestQueuedAskPaneWaitsForItsOwnTurn(t *testing.T) {
	sent := time.Now()
	base := paneTurnBaseline{Since: sent, Path: "/t/r.jsonl", PromptAt: sent.Add(-time.Minute), Queued: "second request: list the failing tests"}
	earlier := paneReply{Path: "/t/r.jsonl", Prompt: "[from #2 Claude] first request: summarise the diff", PromptAt: sent.Add(10 * time.Second), Complete: true}
	if base.freshTurn(earlier) {
		t.Fatal("the request queued ahead of ours was taken as our reply")
	}
	ours := paneReply{Path: "/t/r.jsonl", Prompt: "[from #1 Claude] second request: list the failing tests", PromptAt: sent.Add(20 * time.Second), Complete: true}
	if !base.freshTurn(ours) {
		t.Fatal("our own queued turn did not count")
	}

	p, _ := newTypingHoldPane(t, "pane_queued_codex")
	p.Agent = agentCodex
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-time.Minute).UnixMilli())
	// Between turns: idle screen, but ours is still listed as queued.
	held := strings.Replace(codex0166Screen(t, "idle"), "› Ask Codex to do anything",
		"• Queued follow-up inputs\n  ↳ [from #1 Claude] second request: list the failing tests\n    shift+← edit last queued message\n› Ask Codex to do anything", 1)
	prev := collabScreenText
	collabScreenText = func(*Pane) string { return held }
	t.Cleanup(func() { collabScreenText = prev })
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	res := a.waitForPaneTurn(p, paneTurnBaseline{Since: time.Now(), Queued: "second request: list the failing tests"}, 2500*time.Millisecond)
	if res.Status != "still_working" || res.State != string(StateBusy) {
		t.Fatalf("settled while our message was still queued: %+v", res)
	}
}

// wait_for_pane after a queued ask_pane timed out keeps that request's
// identity: an earlier queued request's answer is still not the reply.
func TestWaitForPaneContinuesAQueuedAsk(t *testing.T) {
	p, _ := newTypingHoldPane(t, "pane_codex_cont")
	p.Agent = agentCodex
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-time.Minute).UnixMilli())
	held := strings.Replace(codex0166Screen(t, "idle"), "› Ask Codex to do anything",
		"• Queued follow-up inputs\n  ↳ [from #1 Claude] second request: list the failing tests\n    shift+← edit last queued message\n› Ask Codex to do anything", 1)
	var screen atomic.Value
	screen.Store(held)
	prev := collabScreenText
	collabScreenText = func(*Pane) string { return screen.Load().(string) }
	t.Cleanup(func() { collabScreenText = prev })
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	a.queuedAsks.put("pane_codex_cont", "pane_caller", paneTurnBaseline{Since: time.Now().Add(-time.Minute), Queued: "second request: list the failing tests"})

	text, isErr := mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_cont", "timeout_seconds": 1.5}, "pane_caller"))
	var res collabResult
	if isErr || json.Unmarshal([]byte(text), &res) != nil {
		t.Fatalf("wait_for_pane: %s", text)
	}
	if res.Status != "still_working" || res.Delivery != "queued" {
		t.Fatalf("returned while our message was still queued: %+v", res)
	}
	if _, kept := a.queuedAsks.get("pane_codex_cont", "pane_caller"); !kept {
		t.Fatal("the queued request was forgotten before its reply")
	}
	// Codex ran our message and settled: the continuation ends and forgets it.
	screen.Store(codex0166Screen(t, "idle"))
	text, _ = mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_cont", "timeout_seconds": 5.0}, "pane_caller"))
	if json.Unmarshal([]byte(text), &res) != nil || res.Status != "idle" {
		t.Fatalf("continuation after the turn: %s", text)
	}
	if _, kept := a.queuedAsks.get("pane_codex_cont", "pane_caller"); kept {
		t.Fatal("the queued request was kept after its reply")
	}
	// Another caller's wait is unaffected.
	if _, other := a.queuedAsks.get("pane_codex_cont", "someone_else"); other {
		t.Fatal("baselines must be per caller")
	}
}

// A command approval that opens while the paste lands must not get the
// submit key: Enter would approve the command for the user.
func TestSubmitAgentMessageStopsWhenAnApprovalOpensBeforeTheKey(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-menu-midway")
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working"))
	stubDeliveryScreen(t, &screen)
	done := make(chan error, 1)
	go func() {
		_, _, err := submitAgentMessage(p, codexProbeMessage, false)
		done <- err
	}()
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	screen.Store(codex0166Screen(t, "approval"))
	if err := <-done; err != errDeliveryChoicePrompt {
		t.Fatalf("err = %v, want the choice-menu refusal", err)
	}
	expectNoFrame(t, frames, "no Enter or Tab into the approval")
}

// A queued ask whose message an interrupt put back in the box says so
// instead of waiting forever with "do not resend".
func TestQueuedAskReportsAMessagePutBackInTheBox(t *testing.T) {
	p, _ := newTypingHoldPane(t, "pane_codex_putback")
	p.Agent = agentCodex
	p.recordUnsentMessage("@lead · #1 Claude", "[from @lead · #1 Claude] second request: list the failing tests", false, p.inputRevision.Load())
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	res := a.waitForPaneTurn(p, paneTurnBaseline{Since: time.Now(), Queued: "second request: list the failing tests"}, 2*time.Second)
	if res.Status != "unsent" || !strings.Contains(res.Hint, "input box") {
		t.Fatalf("put-back queued message: %+v", res)
	}
	// Queued asks match the whole message, not a shared opening.
	base := paneTurnBaseline{Since: time.Now().Add(-time.Minute), Queued: "Please check the build and then the docs"}
	if base.freshTurn(paneReply{Prompt: "[from #2 Claude] Please check the build and then the tests", PromptAt: time.Now(), Complete: true}) {
		t.Fatal("a request with the same opening was taken as ours")
	}
}

func codexRolloutTurn(at time.Time, prompt string, rest ...string) []string {
	ts := `"timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `"`
	lines := []string{`{` + ts + `,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(prompt) + `}]}}`}
	for _, r := range rest {
		switch {
		case strings.HasPrefix(r, "done:"):
			lines = append(lines, `{`+ts+`,"type":"event_msg","payload":{"type":"task_complete","last_agent_message":`+jsonString(strings.TrimPrefix(r, "done:"))+`}}`)
		case strings.HasPrefix(r, "steer:"):
			lines = append(lines, `{`+ts+`,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":`+jsonString(strings.TrimPrefix(r, "steer:"))+`}]}}`)
		default:
			lines = append(lines, `{`+ts+`,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+jsonString(r)+`}]}}`)
		}
	}
	return lines
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The reply to a queued message is found in its own turn, even after the
// turns queued behind it have run; the newest turn is someone else's.
func TestCodexReplyToFindsTheQueuedTurnBehindLaterTurns(t *testing.T) {
	const queued = "second request: list the failing tests"
	match := func(prompt string) bool { return queuedPromptMatches(prompt, queued) }
	t0 := time.Now()
	var lines []string
	lines = append(lines, codexRolloutTurn(t0, "[from #2 Claude] first request: summarise the diff", "done:A summary.")...)
	lines = append(lines, codexRolloutTurn(t0.Add(time.Second), "[from #1 Claude] "+queued, "Looking.", "steer:also check lint", "done:Two tests fail.")...)
	lines = append(lines, codexRolloutTurn(t0.Add(2*time.Second), "[from #4 Codex] third request: bump the version", "Bumping.")...)
	buf := []byte(strings.Join(lines, "\n"))

	if latest := codexLatestReply(buf); latest.Prompt == "" || queuedPromptMatches(latest.Prompt, queued) {
		t.Fatalf("fixture: the newest turn should be another request's: %+v", latest)
	}
	r := codexReplyTo(buf, match)
	if !r.PromptSeen || !r.Complete || r.Reply != "Two tests fail." || !r.PromptAt.Equal(t0.Add(time.Second).UTC()) {
		t.Fatalf("our turn behind a later one: %+v", r)
	}
	// Still running (a steer arrived, no task_complete yet): not complete.
	running := []byte(strings.Join(append(lines[:len(lines)-2:len(lines)-2], codexRolloutTurn(t0.Add(time.Second), "[from #1 Claude] "+queued, "Looking.", "steer:also check lint")...), "\n"))
	if r := codexReplyTo(running, match); !r.PromptSeen || r.Complete || r.Reply != "Looking." {
		t.Fatalf("our turn still running: %+v", r)
	}
	// Not run yet: no turn of ours at all.
	if r := codexReplyTo([]byte(strings.Join(lines[:2], "\n")), match); r.PromptSeen || r.Complete {
		t.Fatalf("a turn was found before ours ran: %+v", r)
	}
}

// wait_for_pane continuing a queued ask returns that ask's reply once its
// turn finished, even while a turn queued behind it is still running.
func TestWaitForPaneFindsAQueuedReplyBehindALaterTurn(t *testing.T) {
	const thread = "019f5443-20e7-7e62-a771-5e2c0560171c"
	const queued = "second request: list the failing tests"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	day := filepath.Join(home, "sessions", "2026", "11", "01")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	sent := time.Now().Add(-time.Minute)
	var lines []string
	lines = append(lines, codexRolloutTurn(sent.Add(-time.Minute), "an earlier turn", "done:Earlier.")...)
	lines = append(lines, codexRolloutTurn(sent.Add(10*time.Second), "[from #2 Claude] first request: summarise the diff", "done:A summary.")...)
	lines = append(lines, codexRolloutTurn(sent.Add(20*time.Second), "[from #1 Claude] "+queued, "done:Two tests fail.")...)
	lines = append(lines, codexRolloutTurn(sent.Add(30*time.Second), "[from #4 Codex] third request: bump the version", "Bumping.")...)
	rollout := filepath.Join(day, "rollout-2026-11-01T10-00-00-"+thread+".jsonl")
	if err := os.WriteFile(rollout, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { codexRolloutPathCache.Delete(thread) })

	p, _ := newTypingHoldPane(t, "pane_codex_later")
	p.Agent = agentCodex
	p.CodexThreadID = thread
	prev := collabScreenText
	collabScreenText = func(*Pane) string { return codex0166Screen(t, "working") }
	t.Cleanup(func() { collabScreenText = prev })
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	a.queuedAsks.put("pane_codex_later", "pane_caller", paneTurnBaseline{Since: sent, Path: rollout, PromptAt: sent.Add(-time.Minute), Queued: queued})

	text, isErr := mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_later", "timeout_seconds": 3.0}, "pane_caller"))
	var res collabResult
	if isErr || json.Unmarshal([]byte(text), &res) != nil {
		t.Fatalf("wait_for_pane: %s", text)
	}
	if res.Status != "idle" || res.Reply != "Two tests fail." || res.Delivery != "queued" {
		t.Fatalf("queued reply behind a running turn: %+v", res)
	}
	if _, kept := a.queuedAsks.get("pane_codex_later", "pane_caller"); kept {
		t.Fatal("the queued request was kept after its reply")
	}
}

// A queued ask is not waited on forever: past its TTL wait_for_pane gives a
// final "expired", and closing either pane forgets it.
func TestQueuedAskExpiresAndPaneCloseForgetsIt(t *testing.T) {
	prevTTL := queuedAskTTL
	queuedAskTTL = time.Minute
	t.Cleanup(func() { queuedAskTTL = prevTTL })
	p, _ := newTypingHoldPane(t, "pane_codex_ttl")
	p.Agent = agentCodex
	prev := collabScreenText
	collabScreenText = func(*Pane) string { return codex0166Screen(t, "working") }
	t.Cleanup(func() { collabScreenText = prev })
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}

	a.queuedAsks.put("pane_codex_ttl", "pane_caller", paneTurnBaseline{Since: time.Now().Add(-30 * time.Second), Queued: "second request: list the failing tests"})
	text, _ := mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_ttl", "timeout_seconds": 1.0}, "pane_caller"))
	var res collabResult
	if json.Unmarshal([]byte(text), &res) != nil || res.Status != "still_working" {
		t.Fatalf("inside the TTL: %s", text)
	}
	a.queuedAsks.put("pane_codex_ttl", "pane_caller", paneTurnBaseline{Since: time.Now().Add(-2 * time.Minute), Queued: "second request: list the failing tests"})
	text, _ = mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_ttl", "timeout_seconds": 5.0}, "pane_caller"))
	if json.Unmarshal([]byte(text), &res) != nil || res.Status != "expired" || res.Delivery != "queued" || strings.Contains(res.Hint, "do not resend") {
		t.Fatalf("past the TTL: %s", text)
	}
	if _, kept := a.queuedAsks.get("pane_codex_ttl", "pane_caller"); kept {
		t.Fatal("the expired request was kept")
	}

	fresh := paneTurnBaseline{Since: time.Now(), Queued: "x"}
	a.queuedAsks.put("pane_a", "pane_b", fresh)
	a.queuedAsks.put("pane_c", "pane_a", fresh)
	a.queuedAsks.put("pane_c", "pane_d", fresh)
	a.queuedAsks.put("pane_old", "pane_d", paneTurnBaseline{Since: time.Now().Add(-3 * time.Minute), Queued: "x"})
	a.queuedAsks.put("pane_e", "pane_d", fresh)
	if _, ok := a.queuedAsks.get("pane_old", "pane_d"); ok {
		t.Fatal("an entry long past its TTL was not cleared")
	}
	a.paneRemoved("pane_a")
	if _, ok := a.queuedAsks.get("pane_a", "pane_b"); ok {
		t.Fatal("a queued ask to a closed pane was kept")
	}
	if _, ok := a.queuedAsks.get("pane_c", "pane_a"); ok {
		t.Fatal("a queued ask from a closed pane was kept")
	}
	if _, ok := a.queuedAsks.get("pane_c", "pane_d"); !ok {
		t.Fatal("an unrelated queued ask was dropped")
	}
}

// A queued ask keeps its identity through an approval menu: ask_pane and
// wait_for_pane both save the baseline on needs_approval, so once the menu
// is answered the next wait still ignores the other requests' answers.
func TestQueuedAskKeepsItsBaselineThroughAnApproval(t *testing.T) {
	const thread = "019f5443-20e7-7e62-a771-5e2c0560171d"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	day := filepath.Join(home, "sessions", "2026", "11", "01")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	// The newest finished turn is another request's; ours has not run.
	rollout := filepath.Join(day, "rollout-2026-11-01T11-00-00-"+thread+".jsonl")
	other := codexRolloutTurn(time.Now(), "[from #2 Claude] first request: summarise the diff", "done:A summary.")
	if err := os.WriteFile(rollout, []byte(strings.Join(other, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { codexRolloutPathCache.Delete(thread) })

	p, frames := newTypingHoldPane(t, "pane_codex_menu")
	p.Agent = agentCodex
	p.CodexThreadID = thread
	atomic.StoreInt64(&p.LastActivityMs, time.Now().UnixMilli())
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	prevScreen, prevState := collabScreenText, collabPaneState
	collabScreenText = func(*Pane) string { return screen.Load().(string) }
	var state atomic.Value
	state.Store(StateChoicePrompt)
	collabPaneState = func(*Pane) PaneRuntimeState { return state.Load().(PaneRuntimeState) }
	t.Cleanup(func() { collabScreenText, collabPaneState = prevScreen, prevState })
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}

	asked := make(chan string, 1)
	go func() {
		text, _ := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_codex_menu", "text": codexProbeMessage, "timeout_seconds": 3.0}, "pane_caller"))
		asked <- text
	}()
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "queue key", "\t")
	screen.Store(codex0166Screen(t, "queued"))
	var res collabResult
	if text := <-asked; json.Unmarshal([]byte(text), &res) != nil || res.Status != "needs_approval" || res.Delivery != "queued" || !strings.Contains(res.Hint, "do not resend") {
		t.Fatalf("ask_pane held by an approval: %s", text)
	}
	if base, kept := a.queuedAsks.get("pane_codex_menu", "pane_caller"); !kept || base.Queued == "" {
		t.Fatal("ask_pane dropped the queued baseline on needs_approval")
	}

	// The approval is still up: wait_for_pane says so and keeps the baseline.
	text, _ := mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_menu", "timeout_seconds": 3.0}, "pane_caller"))
	if json.Unmarshal([]byte(text), &res) != nil || res.Status != "needs_approval" || !strings.Contains(res.Hint, "do not resend") {
		t.Fatalf("wait_for_pane held by an approval: %s", text)
	}
	if _, kept := a.queuedAsks.get("pane_codex_menu", "pane_caller"); !kept {
		t.Fatal("wait_for_pane dropped the queued baseline on needs_approval")
	}

	// Answered; the pane settles with another request's answer newest. That
	// answer is not ours.
	state.Store(StateIdle)
	screen.Store(codex0166Screen(t, "idle"))
	text, _ = mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_menu", "timeout_seconds": 2.5}, "pane_caller"))
	if json.Unmarshal([]byte(text), &res) != nil || res.Status != "still_working" || res.Reply == "A summary." {
		t.Fatalf("after the approval, another request's answer was returned: %s", text)
	}
	// Our turn runs and finishes: that is the reply.
	ours := codexRolloutTurn(time.Now(), codexProbeMessage, "done:CHERRY")
	if err := os.WriteFile(rollout, []byte(strings.Join(append(other, ours...), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, _ = mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": "pane_codex_menu", "timeout_seconds": 3.0}, "pane_caller"))
	if json.Unmarshal([]byte(text), &res) != nil || res.Status != "idle" || res.Reply != "CHERRY" {
		t.Fatalf("our own reply: %s", text)
	}
	waitQueueWatch(t, ended, string(deliverySubmitted))
}

// A queued ask matches a turn whose whole prompt is its message: a short
// ask is not found inside a longer prompt from someone else.
func TestQueuedPromptMatchesTheWholePrompt(t *testing.T) {
	cases := []struct {
		prompt, queued string
		want           bool
	}{
		{"[from #1 Claude] yes", "yes", true},
		{"[from @lead · #1 Claude] yes\n", "  yes ", true},
		{"[from #2 Claude] yes, and then run the tests", "yes", false},
		{"[from #2 Claude] say yes", "yes", false},
		{"[from #2 Claude] status?", "[from #1 Claude] status?", true},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := queuedPromptMatches(c.prompt, c.queued); got != c.want {
			t.Errorf("queuedPromptMatches(%q, %q) = %v, want %v", c.prompt, c.queued, got, c.want)
		}
	}
	base := paneTurnBaseline{Since: time.Now().Add(-time.Minute), Queued: "yes"}
	if base.freshTurn(paneReply{Prompt: "[from #2 Claude] yes, and then run the tests", PromptAt: time.Now(), Complete: true}) {
		t.Fatal("a longer prompt containing a short queued ask was taken as its turn")
	}
}

// An approval that opens after the paste reached the box: delivery stops,
// says the text is still in the box (so a retry would type it twice), and
// records it as the unsent message.
func TestApprovalAfterThePasteLeavesAnHonestUnsentMessage(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-menu-after-paste")
	var screen atomic.Value
	screen.Store(codex0166Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	// The pre-paste check and the box check see the draft; then an approval
	// covers it before the key.
	var reads atomic.Int32
	draft, approval := codex0166Screen(t, "working_draft"), codex0166Screen(t, "approval")
	deliveryScreenText = func(*Pane) string {
		if reads.Add(1) <= 2 {
			return draft
		}
		return approval
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := submitAgentMessage(p, codexProbeMessage, false, "@lead · #1 Claude")
		done <- err
	}()
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	err := <-done
	if !errors.Is(err, errDeliveryChoicePrompt) || !strings.Contains(err.Error(), "still in the input box") || !strings.Contains(err.Error(), "typed twice") {
		t.Fatalf("err = %v, want the honest menu stop", err)
	}
	expectNoFrame(t, frames, "no key into the approval")
	if msg := p.Snapshot().UnsentMessage; msg == nil || msg.body != codexProbeMessage || msg.Sender != "@lead · #1 Claude" {
		t.Fatalf("the pasted text was not recorded as unsent: %+v", msg)
	}
}

func stubNoSteer(t *testing.T, turnOpen bool) {
	t.Helper()
	prevWait, prevOpen := deliveryQueueHintWait, deliveryCodexTurnOpen
	deliveryQueueHintWait = 50 * time.Millisecond
	deliveryCodexTurnOpen = func(*Pane) bool { return turnOpen }
	t.Cleanup(func() { deliveryQueueHintWait, deliveryCodexTurnOpen = prevWait, prevOpen })
}

// A running Codex turn whose footer offers no queue key (rebound key, older
// Codex, footer in another mode): ask_pane's no-steer delivery never presses
// Enter. A line the screen shows exactly is removed from the box; anything
// else is recorded as unsent. send_input keeps its Enter.
func TestNoSteerDeliveryNeverEntersARunningCodexTurn(t *testing.T) {
	working := codex0166Screen(t, "working")
	noHint := strings.Replace(working, "› Ask Codex to do anything", "› "+codexProbeMessage, 1)
	if codexQueueOffered(noHint) || !composerShowsExactLine(noHint, true, codexProbeMessage) {
		t.Fatal("fixture: want a running turn, the draft shown exactly and no queue hint")
	}

	t.Run("exact line is removed", func(t *testing.T) {
		stubNoSteer(t, false)
		p, frames := newTypingHoldPane(t, "nosteer-clear")
		var screen atomic.Value
		screen.Store(noHint)
		stubDeliveryScreen(t, &screen)
		done := make(chan error, 1)
		go func() {
			_, _, err := deliverAgentMessage(p, codexProbeMessage, false, deliveryNoSteer, "@lead · #1 Claude")
			done <- err
		}()
		expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
		expectFrame(t, frames, "clear the line", "\x01\x0b")
		screen.Store(working)
		err := <-done
		if !errors.Is(err, errDeliveryWouldSteer) || !strings.Contains(err.Error(), "removed from the input box") {
			t.Fatalf("err = %v", err)
		}
		expectNoFrame(t, frames, "no Enter into the running turn")
		if p.Snapshot().UnsentMessage != nil {
			t.Fatal("a removed message left an unsent receipt")
		}
	})

	t.Run("streaming turn seen in the rollout, unverifiable box", func(t *testing.T) {
		stubNoSteer(t, true)
		p, frames := newTypingHoldPane(t, "nosteer-receipt")
		// No Working row (the answer is streaming), no queue hint, and a
		// footer the exact-line check cannot read.
		streaming := strings.Replace(codex0166Screen(t, "working_draft"), "tab to queue message", "                    ", 1)
		streaming = strings.Replace(streaming, codexActiveTurnRE.FindString(streaming), "", 1)
		if codexTurnRunningText(streaming) {
			t.Fatal("fixture: the screen alone must not show the turn")
		}
		var screen atomic.Value
		screen.Store(streaming)
		stubDeliveryScreen(t, &screen)
		_, _, err := deliverAgentMessage(p, codexProbeMessage, false, deliveryNoSteer, "@lead · #1 Claude")
		if !errors.Is(err, errDeliveryWouldSteer) || !strings.Contains(err.Error(), "still in the input box") {
			t.Fatalf("err = %v", err)
		}
		expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
		expectNoFrame(t, frames, "no Enter into the running turn")
		if msg := p.Snapshot().UnsentMessage; msg == nil || msg.body != codexProbeMessage {
			t.Fatalf("the message left in the box was not recorded as unsent: %+v", msg)
		}
	})

	t.Run("the hint draws late and gets Tab", func(t *testing.T) {
		stubNoSteer(t, false)
		deliveryQueueHintWait = 2 * time.Second
		p, frames := newTypingHoldPane(t, "nosteer-late-hint")
		var screen atomic.Value
		screen.Store(noHint)
		stubDeliveryScreen(t, &screen)
		ended := stubQueueWatch(t)
		done := make(chan deliveryOutcome, 1)
		go func() {
			_, outcome, _ := deliverAgentMessage(p, codexProbeMessage, false, deliveryNoSteer)
			done <- outcome
		}()
		expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
		time.Sleep(30 * time.Millisecond)
		screen.Store(codex0166Screen(t, "working_draft"))
		expectFrame(t, frames, "queue key", "\t")
		screen.Store(codex0166Screen(t, "queued"))
		if got := <-done; got != deliveryQueued {
			t.Fatalf("outcome = %q, want queued", got)
		}
		screen.Store(codex0166Screen(t, "idle"))
		waitQueueWatch(t, ended, string(deliverySubmitted))
	})

	t.Run("the turn ended and gets Enter", func(t *testing.T) {
		stubNoSteer(t, false)
		p, frames := newTypingHoldPane(t, "nosteer-idle")
		var screen atomic.Value
		screen.Store(strings.Replace(codex0166Screen(t, "idle"), "› Ask Codex to do anything", "› "+codexProbeMessage, 1))
		stubDeliveryScreen(t, &screen)
		done := make(chan deliveryOutcome, 1)
		go func() {
			_, outcome, _ := deliverAgentMessage(p, codexProbeMessage, false, deliveryNoSteer)
			done <- outcome
		}()
		expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
		expectFrame(t, frames, "Enter", "\r")
		screen.Store(codex0166Screen(t, "idle"))
		if got := <-done; got != deliverySubmitted {
			t.Fatalf("outcome = %q, want submitted", got)
		}
	})

	t.Run("the person's message box keeps Enter", func(t *testing.T) {
		stubNoSteer(t, true)
		p, frames := newTypingHoldPane(t, "plain-enter")
		var screen atomic.Value
		screen.Store(noHint)
		stubDeliveryScreen(t, &screen)
		done := runDelivery(p, codexProbeMessage)
		expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
		expectFrame(t, frames, "Enter", "\r")
		screen.Store(working)
		if got := <-done; got != deliverySubmitted {
			t.Fatalf("outcome = %q, want submitted", got)
		}
	})
}

// ask_pane uses the no-steer delivery: a working Codex without a queue key
// is refused with a clear error, not steered.
func TestAskPaneDoesNotSteerACodexWithoutAQueueKey(t *testing.T) {
	stubNoSteer(t, false)
	p, frames := newTypingHoldPane(t, "pane_codex_nosteer")
	p.Agent = agentCodex
	atomic.StoreInt64(&p.LastActivityMs, time.Now().UnixMilli())
	working := codex0166Screen(t, "working")
	var screen atomic.Value
	screen.Store(strings.Replace(working, "› Ask Codex to do anything", "› "+codexProbeMessage, 1))
	stubDeliveryScreen(t, &screen)
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	type askResult struct {
		text  string
		isErr bool
	}
	asked := make(chan askResult, 1)
	go func() {
		text, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_codex_nosteer", "text": codexProbeMessage, "timeout_seconds": 1.0}, "pane_caller"))
		asked <- askResult{text, isErr}
	}()
	if f := <-frames; f.err != nil || !strings.HasPrefix(string(f.payload), "\x1b[200~") {
		t.Fatalf("bracketed body: %q %v", f.payload, f.err)
	}
	expectFrame(t, frames, "clear the line", "\x01\x0b")
	screen.Store(working)
	got := <-asked
	if !got.isErr || !strings.Contains(got.text, "would steer") || !strings.Contains(got.text, "wait_for_pane") {
		t.Fatalf("ask_pane steered or did not say why: %+v", got)
	}
	expectNoFrame(t, frames, "no Enter into the running turn")
}

// The footer names the queue key as Codex labels it, rebound or not. Only
// one-byte keys that cannot read as Enter, Esc or a stop are pressed.
func TestCodexQueueKeyReadsTheFooterBinding(t *testing.T) {
	box := "› " + codexProbeMessage + "\n\n"
	for _, c := range []struct {
		hint string
		key  string // "" = refused
	}{
		{"tab to queue message", "\t"},
		{"tab to queue · Plan mode", "\t"},
		{"^q to queue message", "\x11"},     // Linux
		{"ctrl+q to queue message", "\x11"}, // Windows
		{"⌃q to queue message", "\x11"},     // macOS
		{"^g to queue", "\x07"},
		{"^m to queue message", ""}, // the byte is Enter: it would steer
		{"^i to queue message", ""}, // the byte is Tab
		{"^j to queue message", ""}, // LF: ConPTY can read it as Enter
		{"^c to queue message", ""}, // stops Codex
		{"^d to queue message", ""},
		{"^z to queue message", ""},
		{"alt+q to queue message", ""}, // starts with Esc, which interrupts
		{"shift+tab to queue message", ""},
		{"^x q to queue message", ""}, // a chord
		{"f2 to queue message", ""},
	} {
		key, ok := codexQueueKey(box + "  " + c.hint + "                 98% context left")
		if ok != (c.key != "") || string(key) != c.key {
			t.Errorf("%q: key %q (%v), want %q", c.hint, key, ok, c.key)
		}
	}
	// The whole capture with its footer rebound, as `-c
	// 'tui.keymap.composer.queue="ctrl-q"'` draws it on Linux.
	rebound := strings.Replace(codex0166Screen(t, "working_draft"), "tab to queue message", "^q to queue message", 1)
	if key, ok := codexQueueKey(rebound); !ok || string(key) != "\x11" || !codexTurnRunningText(rebound) {
		t.Fatalf("rebound capture: key %q (%v)", key, ok)
	}
}

// A Codex whose queue key was rebound gets that key, not Tab or Enter.
func TestSubmitAgentMessageQueuesWithAReboundKey(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-queue-rebound")
	var screen atomic.Value
	screen.Store(strings.Replace(codex0166Screen(t, "working_draft"), "tab to queue message", "^q to queue message", 1))
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	done := runDelivery(p, codexProbeMessage)
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "rebound queue key", "\x11")
	screen.Store(codex0166Screen(t, "queued"))
	if got := <-done; got != deliveryQueued {
		t.Fatalf("outcome = %q, want queued", got)
	}
	expectNoFrame(t, frames, "no Enter after a queued message")
	screen.Store(codex0166Screen(t, "idle"))
	waitQueueWatch(t, ended, string(deliverySubmitted))
}

// An agent's send_input to a running Codex turn with no queue key it can
// press is refused like ask_pane, not pressed in as a steer: a line the
// screen shows exactly is removed, anything else becomes the unsent receipt.
func TestSendInputDoesNotSteerABusyCodex(t *testing.T) {
	stubNoSteer(t, true)
	working := codex0166Screen(t, "working")
	noHint := strings.Replace(working, "› Ask Codex to do anything", "› "+codexProbeMessage, 1)
	// A queue key bound to Alt: Codex draws the hint in place of the
	// model footer, as it does for Tab.
	altHint := strings.Replace(codex0166Screen(t, "working_draft"), "tab to queue message", "alt+q to queue message", 1)
	for _, c := range []struct {
		name, screen, want string
		cleared            bool
	}{
		{"no queue hint", noHint, "removed from the input box", true},
		{"alt key hint", altHint, "still in the input box", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, frames := newTypingHoldPane(t, "pane_codex_send")
			p.Agent = agentCodex
			var screen atomic.Value
			screen.Store(c.screen)
			stubDeliveryScreen(t, &screen)
			a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
			sent := make(chan any, 1)
			go func() {
				sent <- a.mcpToolSendInput(map[string]any{"pane_id": "pane_codex_send", "text": codexProbeMessage + "\n"}, "pane_caller")
			}()
			if f := <-frames; f.err != nil || !strings.HasPrefix(string(f.payload), "\x1b[200~") {
				t.Fatalf("bracketed body: %q %v", f.payload, f.err)
			}
			if c.cleared {
				expectFrame(t, frames, "clear the line", "\x01\x0b")
				screen.Store(working)
			}
			text, isErr := mcpResultText(t, <-sent)
			if !isErr || !strings.Contains(text, "would steer") || !strings.Contains(text, c.want) {
				t.Fatalf("send_input steered or did not say why: %q", text)
			}
			expectNoFrame(t, frames, "no Enter into the running turn")
			if msg := p.Snapshot().UnsentMessage; (msg != nil) == c.cleared {
				t.Fatalf("unsent receipt = %+v, want one only for text left in the box", msg)
			}
		})
	}
}

// Codex can draw its queue list a frame after the box empties. A message
// the list shows on a later read is still queued and watched, not reported
// as sent.
func TestQueuedMessageShownOnALaterReadIsStillQueued(t *testing.T) {
	p, frames := newTypingHoldPane(t, "delivery-queue-late-list")
	draft, empty, queued := codex0166Screen(t, "working_draft"), codex0166Screen(t, "working"), codex0166Screen(t, "queued")
	var phase atomic.Int32
	var reads atomic.Int32
	var screen atomic.Value
	screen.Store(draft)
	stubDeliveryScreen(t, &screen)
	idle := codex0166Screen(t, "idle")
	deliveryScreenText = func(*Pane) string {
		switch phase.Load() {
		case 0:
			return draft
		case 2:
			return idle
		}
		// The box is empty on the first read after Tab; the list lands on
		// the third.
		if reads.Add(1) < 3 {
			return empty
		}
		return queued
	}
	ended := stubQueueWatch(t)
	done := runDelivery(p, codexProbeMessage)
	expectFrame(t, frames, "bracketed body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "queue key", "\t")
	phase.Store(1)
	if got := <-done; got != deliveryQueued {
		t.Fatalf("outcome = %q, want queued", got)
	}
	phase.Store(2) // the turn ends and Codex sends it
	waitQueueWatch(t, ended, string(deliverySubmitted))
}

// The queue match is the whole message, not its first 24 characters: two
// requests from one sender that start alike are told apart. A preview Codex
// cut short ("…") matches on everything it shows.
func TestCodexQueueHoldsMatchesTheWholeMessage(t *testing.T) {
	const ours = "[from #3 Claude] please review the change in pane_delivery.go and reply"
	const theirs = "[from #3 Claude] please review the change in mcp.go and reply"
	list := func(rows ...string) string {
		return "• Working (3s • esc to interrupt)\n\n• Queued follow-up inputs\n" + strings.Join(rows, "\n") +
			"\n    shift+← edit last queued message\n\n› Ask Codex to do anything\n\n  Context 2% used · GPT-6.1-Sol high · ~/x"
	}
	if codexQueueHolds(list("  ↳ "+theirs), ours) {
		t.Error("another queued request with the same start matched")
	}
	if !codexQueueHolds(list("  ↳ "+theirs, "  ↳ [from #3 Claude] please review the change in", "    pane_delivery.go and reply"), ours) {
		t.Error("our wrapped message, second in the list, was missed")
	}
	// ask_pane follows the unlabelled text it was given.
	if !codexQueueHolds(list("  ↳ "+ours), "please review the change in pane_delivery.go and reply") {
		t.Error("the unlabelled text did not match its labelled queue row")
	}
	if codexQueueHolds(list("  ↳ "+ours+" today"), ours) {
		t.Error("a longer message that starts with ours matched")
	}
	long := ours + "\nline two\nline three\nline four\nline five"
	cut := list("  ↳ "+ours, "    line two", "    line three", "    …")
	if !codexQueueHolds(cut, long) {
		t.Error("a preview cut short missed the message it shows")
	}
	if codexQueueHolds(cut, theirs+"\nline two\nline three\nline four") {
		t.Error("a cut preview matched a message it does not show")
	}
}
