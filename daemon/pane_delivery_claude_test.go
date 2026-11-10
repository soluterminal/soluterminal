package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Screens captured from a real Claude Code 2.1.310 pane (120x40, as
// SoluTerminal runs it), with only the working folder renamed. Each probe is
// the message the capture queued.
func claude2110Screen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude_2_1_310", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	claudeProbeCherry = "queued probe: say CHERRY"
	claudeProbeFig    = "[from #3 Claude] multi probe line one\nline two say FIG"
)

func claudeProbeLime() string {
	words := make([]string, 60)
	for i := range words {
		words[i] = fmt.Sprintf("word%d", i)
	}
	return "long probe: " + strings.Join(words, " ") + " say LIME"
}

func TestClaude2110ScreensReadTheQueue(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		queued  []string // messages Claude lists as queued
		absent  []string // messages it must not count as queued
		box     string   // a message the input box holds
	}{
		{name: "idle"},
		// Our message typed while the answer streams: the footer drops "esc
		// to interrupt" while the box holds a draft and no spinner is drawn,
		// so the screen alone cannot tell (delivery reads the pane state).
		{name: "working_draft", box: claudeProbeCherry},
		// Enter mid-turn: Claude lists it above the box with the send-now hint.
		{name: "queued", running: true, queued: []string{claudeProbeCherry}},
		// Queued during thinking, right under the turn's own prompt.
		{name: "queued_thinking", running: true, queued: []string{"second probe: say PLUM"}, absent: []string{claudeProbeCherry}},
		// Three queued: wrapped, multi-line with a sender label, short.
		{name: "queued_multi", running: true, queued: []string{claudeProbeLime(), claudeProbeFig, "short probe say KIWI"}, absent: []string{"short probe say", "multi probe line one"}},
		// Queued while a tool runs.
		{name: "queued_tool", running: true, queued: []string{"third probe: say PEAR"}},
		// Up moved the queue back into the box; the turn still runs.
		{name: "popped", running: true, box: "second probe: say PLUM", absent: []string{"second probe: say PLUM"}},
		// The turn ended with it still there: nothing will send it.
		{name: "left_after_turn", box: "second probe: say PLUM", absent: []string{"second probe: say PLUM"}},
		// Esc interrupted the turn and Claude ran the queued message.
		{name: "esc_sent", running: true, absent: []string{claudeProbeCherry}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			screen := claude2110Screen(t, c.name)
			if got := claudeTurnRunningText(screen); got != c.running {
				t.Errorf("turn running = %v, want %v", got, c.running)
			}
			for _, m := range c.queued {
				if !claudeQueueHolds(screen, m) {
					t.Errorf("queue misses %q (read %q)", m, claudeQueuedMessages(screen))
				}
			}
			for _, m := range c.absent {
				if claudeQueueHolds(screen, m) {
					t.Errorf("queue holds %q", m)
				}
			}
			if c.box != "" {
				if holds, known := composerHolds(screen, false, c.box); !known || !holds {
					t.Errorf("input box holds = %v (known %v), want %q", holds, known, c.box)
				}
			}
		})
	}
	// The sender label is skipped, so ask_pane's unlabelled text matches.
	if !claudeQueueHolds(claude2110Screen(t, "queued_multi"), "multi probe line one\nline two say FIG") {
		t.Error("the unlabelled text missed its labelled queue entry")
	}
}

func stubClaudeBusy(t *testing.T, busy bool) {
	t.Helper()
	prev := deliveryPaneBusy
	deliveryPaneBusy = func(*Pane) bool { return busy }
	t.Cleanup(func() { deliveryPaneBusy = prev })
}

func newClaudeDeliveryPane(t *testing.T, id string) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames := newTypingHoldPane(t, id)
	p.Agent = agentClaude
	return p, frames
}

// A message to a working Claude: Enter (Claude's own queue key), then it is
// reported as queued and watched until Claude takes it.
func TestSubmitAgentMessageReportsAQueuedClaudeMessage(t *testing.T) {
	stubClaudeBusy(t, true)
	p, frames := newClaudeDeliveryPane(t, "claude-queue")
	var screen atomic.Value
	screen.Store(claude2110Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	done := make(chan deliveryOutcome, 1)
	go func() {
		_, outcome, _ := submitAgentMessage(p, claudeProbeCherry, true, "@lead · #1 Claude")
		done <- outcome
	}()
	expectFrame(t, frames, "body", claudeProbeCherry)
	expectFrame(t, frames, "Enter", "\r")
	screen.Store(claude2110Screen(t, "queued"))
	if got := <-done; got != deliveryQueued {
		t.Fatalf("outcome = %q, want queued", got)
	}
	expectNoFrame(t, frames, "nothing more is typed")
	// Esc: Claude runs it as its own turn.
	screen.Store(claude2110Screen(t, "esc_sent"))
	waitQueueWatch(t, ended, string(deliverySubmitted))
	if p.Snapshot().UnsentMessage != nil {
		t.Fatal("a sent message left an unsent receipt")
	}
}

// Up moves Claude's queue back into the input box, where nothing sends it:
// it becomes the unsent receipt.
func TestQueuedClaudeMessagePutBackInTheBoxBecomesUnsent(t *testing.T) {
	stubClaudeBusy(t, true)
	const plum = "second probe: say PLUM"
	p, frames := newClaudeDeliveryPane(t, "claude-queue-popped")
	var screen atomic.Value
	screen.Store(claude2110Screen(t, "left_after_turn")) // the draft in the box
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	done := make(chan deliveryOutcome, 1)
	go func() {
		_, outcome, _ := submitAgentMessage(p, plum, true, "@lead · #1 Claude")
		done <- outcome
	}()
	expectFrame(t, frames, "body", plum)
	expectFrame(t, frames, "Enter", "\r")
	screen.Store(claude2110Screen(t, "queued_thinking"))
	if got := <-done; got != deliveryQueued {
		t.Fatalf("outcome = %q, want queued", got)
	}
	screen.Store(claude2110Screen(t, "popped"))
	waitQueueWatch(t, ended, string(deliveryLeftInComposer))
	msg := p.Snapshot().UnsentMessage
	if msg == nil || msg.body != plum || !msg.claude || msg.NotTyped {
		t.Fatalf("the message Up put back was not recorded as unsent: %+v", msg)
	}
}

// An idle Claude submits as before: one read of the screen, no queue.
func TestSubmitAgentMessageEntersAnIdleClaude(t *testing.T) {
	stubClaudeBusy(t, false)
	p, frames := newClaudeDeliveryPane(t, "claude-idle")
	var screen atomic.Value
	idle := claude2110Screen(t, "idle")
	screen.Store(strings.Replace(idle, "❯ Try \"refactor api.ts\"", "❯ "+claudeProbeCherry, 1))
	stubDeliveryScreen(t, &screen)
	done := make(chan deliveryOutcome, 1)
	go func() {
		_, outcome, _ := submitAgentMessage(p, claudeProbeCherry, true)
		done <- outcome
	}()
	expectFrame(t, frames, "body", claudeProbeCherry)
	expectFrame(t, frames, "Enter", "\r")
	screen.Store(idle)
	if got := <-done; got != deliverySubmitted {
		t.Fatalf("outcome = %q, want submitted", got)
	}
}

// Transcript records as Claude Code 2.1.310 writes them for a queued message.
func claudeQueuedTurn(at time.Time, prompt, source string) string {
	return claudeLine("user", at, `"promptSource":`+jsonString(source)+`,"message":{"role":"user","content":`+jsonString(prompt)+`}`)
}

func claudeAnswer(at time.Time, text, stop string) string {
	return claudeLine("assistant", at, `"message":{"role":"assistant","stop_reason":`+jsonString(stop)+`,"content":[{"type":"text","text":`+jsonString(text)+`}]}`)
}

func claudeToolRound(at time.Time) []string {
	return []string{
		claudeLine("assistant", at, `"message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash","input":{}}]}`),
		claudeLine("user", at, `"message":{"role":"user","content":[{"type":"tool_result","content":"bin"}]}`),
	}
}

func TestClaudeReplyToFindsTheQueuedMessage(t *testing.T) {
	t0 := time.Now().Add(-time.Minute)
	match := func(want string) func(string) bool {
		return func(prompt string) bool { return queuedPromptMatches(prompt, want) }
	}
	join := func(lines ...[]string) []byte {
		var all []string
		for _, l := range lines {
			all = append(all, l...)
		}
		return []byte(strings.Join(all, "\n") + "\n")
	}

	// The running turn ends; the queued message runs as its own turn, and a
	// later turn follows it.
	own := join(
		[]string{claudeQueuedTurn(t0, "write a story", "typed"), claudeAnswer(t0.Add(time.Second), "Once upon a time.", "end_turn")},
		[]string{claudeQueuedTurn(t0.Add(2*time.Second), "[from #3 Claude] say CHERRY", "queued"), claudeAnswer(t0.Add(3*time.Second), "CHERRY", "end_turn")},
		[]string{claudeQueuedTurn(t0.Add(4*time.Second), "something else", "typed"), claudeAnswer(t0.Add(5*time.Second), "Other.", "")},
	)
	if r := claudeReplyTo(own, match("say CHERRY")); !r.PromptSeen || !r.Complete || r.Reply != "CHERRY" || !r.PromptAt.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("own turn: %+v", r)
	}

	// Mid-turn: Claude takes it with the next tool result and answers inside
	// the running turn; the reply runs to that turn's end.
	mid := join(
		[]string{claudeQueuedTurn(t0, "list six folders", "typed")},
		claudeToolRound(t0.Add(time.Second)),
		[]string{claudeLine("attachment", t0.Add(2*time.Second), `"attachment":{"type":"queued_command","prompt":"say PEAR","commandMode":"prompt"}`)},
		claudeToolRound(t0.Add(3*time.Second)),
		[]string{claudeAnswer(t0.Add(4*time.Second), "PEAR", "tool_use")},
		claudeToolRound(t0.Add(5*time.Second)),
	)
	r := claudeReplyTo(mid, match("say PEAR"))
	if !r.PromptSeen || r.Complete || r.Reply != "PEAR" {
		t.Fatalf("mid-turn, still running: %+v", r)
	}
	mid = append(mid, []byte(claudeAnswer(t0.Add(6*time.Second), "All six done.", "end_turn")+"\n")...)
	if r := claudeReplyTo(mid, match("say PEAR")); !r.Complete || r.Reply != "PEAR\n\nAll six done." {
		t.Fatalf("mid-turn, finished: %+v", r)
	}

	// Messages queued together go in together, back to back, with one reply.
	batch := join(
		[]string{claudeQueuedTurn(t0, "say LIME", "queued"), claudeQueuedTurn(t0, "say FIG", "queued"), claudeQueuedTurn(t0, "say KIWI", "queued")},
		[]string{claudeAnswer(t0.Add(time.Second), "LIME\nFIG\nKIWI", "end_turn")},
	)
	for _, m := range []string{"say LIME", "say FIG", "say KIWI"} {
		if r := claudeReplyTo(batch, match(m)); !r.Complete || r.Reply != "LIME\nFIG\nKIWI" {
			t.Fatalf("batched %q: %+v", m, r)
		}
	}

	// Not in the transcript yet: keep waiting.
	if r := claudeReplyTo(own, match("say PLUM")); r.PromptSeen || r.Complete {
		t.Fatalf("an absent message was found: %+v", r)
	}
}

// ask_pane no longer turns a busy Claude away: Claude queues the message and
// ask_pane returns the reply to that message, not the running turn's.
func TestAskPaneQueuesForABusyClaudeAndGetsItsOwnReply(t *testing.T) {
	stubClaudeBusy(t, true)
	t0 := time.Now()
	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	running := claudeQueuedTurn(t0.Add(-time.Minute), "write a story", "typed") + "\n"
	if err := os.WriteFile(transcript, []byte(running), 0o600); err != nil {
		t.Fatal(err)
	}
	p, frames := newClaudeDeliveryPane(t, "pane_claude_busy")
	p.ClaudeTranscriptPath = transcript
	atomic.StoreInt64(&p.LastActivityMs, time.Now().UnixMilli())
	var screen atomic.Value
	screen.Store(claude2110Screen(t, "working_draft"))
	stubDeliveryScreen(t, &screen)
	ended := stubQueueWatch(t)
	prevState := collabPaneState
	var state atomic.Value
	state.Store(StateBusy)
	collabPaneState = func(*Pane) PaneRuntimeState { return state.Load().(PaneRuntimeState) }
	t.Cleanup(func() { collabPaneState = prevState })
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	type askResult struct {
		text  string
		isErr bool
	}
	asked := make(chan askResult, 1)
	go func() {
		text, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_claude_busy", "text": claudeProbeCherry, "timeout_seconds": 10.0}, "pane_caller"))
		asked <- askResult{text, isErr}
	}()
	select {
	case f := <-frames:
		if f.err != nil || !strings.HasSuffix(string(f.payload), claudeProbeCherry) {
			t.Fatalf("body: %q %v", f.payload, f.err)
		}
	case got := <-asked:
		t.Fatalf("ask_pane returned before typing: %+v", got)
	case <-time.After(2 * time.Second):
		t.Fatal("ask_pane typed nothing")
	}
	expectFrame(t, frames, "Enter", "\r")
	screen.Store(claude2110Screen(t, "queued"))
	// The running turn finishes first; its story is not the answer.
	fh, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(fh, claudeAnswer(time.Now(), "Once upon a time.", "end_turn"))
	time.Sleep(300 * time.Millisecond)
	screen.Store(claude2110Screen(t, "esc_sent"))
	now := time.Now()
	fmt.Fprintln(fh, claudeQueuedTurn(now, "[from pane_caller] "+claudeProbeCherry, "queued"))
	fmt.Fprintln(fh, claudeAnswer(now, "CHERRY", "end_turn"))
	// Another sender's message queued behind ours has already run too.
	fmt.Fprintln(fh, claudeQueuedTurn(now.Add(time.Second), "[from #4 Codex] say PLUM", "queued"))
	fmt.Fprintln(fh, claudeAnswer(now.Add(time.Second), "PLUM", "end_turn"))
	fh.Close()
	state.Store(StateIdle)
	got := <-asked
	if got.isErr {
		t.Fatalf("ask_pane refused a busy Claude: %s", got.text)
	}
	var res collabResult
	if err := json.Unmarshal([]byte(got.text), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "replied" || res.Reply != "CHERRY" || res.Delivery != "queued" {
		t.Fatalf("result: %+v", res)
	}
	waitQueueWatch(t, ended, string(deliverySubmitted))
}

// A busy shell (or an agent with no queue SoluTerminal knows) is not typed
// into: the message is refused and kept as a not-typed receipt, which the
// person can send. Keys without a line still go through.
func TestSendInputRefusesABusyPaneWithoutAQueue(t *testing.T) {
	prev := sendInputPaneBusy
	var busy atomic.Bool
	busy.Store(true)
	sendInputPaneBusy = func(*Pane) bool { return busy.Load() }
	t.Cleanup(func() { sendInputPaneBusy = prev })
	p, frames := newTypingHoldPane(t, "pane_shell")
	p.Agent = ""
	p.forceCodexForTest = false
	p.Title = "bash"
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	if agent, _ := paneAgentProvider(p); agent {
		t.Fatal("fixture: want a plain shell pane")
	}
	text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": "pane_shell", "text": "make test\n"}, "pane_caller"))
	if !isErr || !strings.Contains(text, "busy") || !strings.Contains(text, "not sent") {
		t.Fatalf("send_input typed into a busy pane or did not say why: %q", text)
	}
	expectNoFrame(t, frames, "nothing typed into a busy pane")
	msg := p.Snapshot().UnsentMessage
	if msg == nil || !msg.NotTyped || msg.body != "make test" || msg.Sender == "" {
		t.Fatalf("no not-typed receipt: %+v", msg)
	}
	// Typing in the pane does not invalidate a receipt for text never typed.
	p.inputRevision.Add(1)
	if p.Snapshot().UnsentMessage == nil {
		t.Fatal("input in the pane dropped the not-typed receipt")
	}
	// Ctrl+C still reaches the busy pane.
	if _, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": "pane_shell", "text": "\x03"}, "pane_caller")); isErr {
		t.Fatal("Ctrl+C to a busy pane was refused")
	}
	expectFrame(t, frames, "Ctrl+C", "\x03")
	// Once the pane settles the agent sends it again, which clears the notice.
	busy.Store(false)
	if text, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": "pane_shell", "text": "make test\n"}, "pane_caller")); isErr {
		t.Fatal(text)
	}
	expectFrame(t, frames, "line", "make test")
	expectFrame(t, frames, "Enter", "\r")
	if p.Snapshot().UnsentMessage != nil {
		t.Fatal("the message was sent but its not-typed receipt stayed")
	}
}

// The default Claude box (no status line) reads as known only with Claude's
// own rules around it and its own chrome below.
func TestClaudePlainComposerNeedsClaudesOwnChrome(t *testing.T) {
	idle := claude2110Screen(t, "idle")
	if draft, known := composerDraft(idle, false); !known || draft != conditionalDraftText(`Try "refactor api.ts"`) {
		t.Fatalf("default box: %q %v", draft, known)
	}
	for name, screen := range map[string]string{
		"foreign line below": strings.Replace(idle, "  ⏵⏵ auto mode on", "$ make test", 1),
		"too much below":     idle + strings.Repeat("  more\n", claudeFooterRows),
		"no top rule":        strings.Replace(idle, "────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n❯", "❯", 1),
	} {
		if _, known := claudePlainComposer(screen); known {
			t.Errorf("%s: read as Claude's input box", name)
		}
	}
}

// A narrow pane cuts the status line to "[Haiku…", so the model-footer reader
// cannot place the box; the default-box reader still can, from its rules and
// Claude's padded footer. Captured at 46 columns, the width of many panes.
func TestClaudeNarrowPaneBoxIsReadable(t *testing.T) {
	draft := claude2110Screen(t, "narrow_draft")
	if _, _, known := conditionalClaudeComposer(draft); known {
		t.Fatal("fixture: the cut status line should defeat the model-footer reader")
	}
	if got, known := composerDraft(draft, false); !known || got != conditionalDraftText(claudeProbeCherry) {
		t.Fatalf("narrow box: %q %v", got, known)
	}
	if !composerHoldsExactly(draft, false, claudeProbeCherry) || !composerShowsExactLine(draft, false, claudeProbeCherry) {
		t.Fatal("narrow box does not show exactly the message")
	}
	queued := claude2110Screen(t, "narrow_queued")
	const probe = "queued probe: when done, say CHERRY"
	if !claudeTurnRunningText(queued) || !claudeQueueHolds(queued, "[from @lead · #1 Claude] "+probe) || !claudeQueueHolds(queued, probe) {
		t.Fatalf("narrow queue: running %v, read %q", claudeTurnRunningText(queued), claudeQueuedMessages(queued))
	}
	if got, known := composerDraft(queued, false); !known || got != conditionalDraftText("Press up to edit queued messages") {
		t.Fatalf("narrow box under a queue: %q %v", got, known)
	}
}

// The live failure: Enter lands too soon after the paste and Claude keeps the
// text in its box. Delivery presses Enter once more, only while the box holds
// exactly the message and nothing else was typed; otherwise, or if it is
// still there, the message is left for the person. It is never typed twice.
func TestClaudeSwallowedEnterIsPressedOnceMore(t *testing.T) {
	draft := claude2110Screen(t, "narrow_draft")
	empty := strings.Replace(draft, claudeProbeCherry, "", 1)
	cases := []struct {
		name    string
		screen  string // the box after the first Enter
		typed   bool   // someone else typed after the first Enter
		retry   bool   // a second Enter is pressed
		clears  bool   // the second Enter sends it
		want    deliveryOutcome
		receipt bool // Send/Clear offered
	}{
		{name: "retried", screen: draft, retry: true, clears: true, want: deliverySubmitted},
		{name: "still there", screen: draft, retry: true, want: deliveryLeftInComposer, receipt: true},
		{name: "someone typed", screen: draft, typed: true, want: deliveryLeftInComposer},
		{name: "box holds more", screen: strings.Replace(draft, claudeProbeCherry, claudeProbeCherry+" and more", 1), want: deliveryLeftInComposer, receipt: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubClaudeBusy(t, false)
			p, frames := newClaudeDeliveryPane(t, "claude-swallowed")
			var screen atomic.Value
			screen.Store(draft)
			stubDeliveryScreen(t, &screen)
			done := make(chan deliveryOutcome, 1)
			go func() {
				_, outcome, _ := submitAgentMessage(p, claudeProbeCherry, true, "@lead · #1 Claude")
				done <- outcome
			}()
			expectFrame(t, frames, "body", claudeProbeCherry)
			screen.Store(c.screen)
			expectFrame(t, frames, "Enter", "\r")
			if c.typed {
				time.Sleep(50 * time.Millisecond)
				p.inputRevision.Add(1)
			}
			if c.retry {
				expectFrame(t, frames, "second Enter", "\r")
				if c.clears {
					screen.Store(empty)
				}
			}
			if got := <-done; got != c.want {
				t.Fatalf("outcome = %q, want %q", got, c.want)
			}
			expectNoFrame(t, frames, "never typed twice, no third Enter")
			msg := p.Snapshot().UnsentMessage
			if c.receipt != (msg != nil) || (msg != nil && (msg.body != claudeProbeCherry || !msg.claude || msg.NotTyped)) {
				t.Fatalf("receipt = %+v, want offered %v", msg, c.receipt)
			}
		})
	}
}

// postUnsentAction acts as a current client would: Send carries the hash of
// the receipt's text it showed.
func postUnsentAction(t *testing.T, p *Pane, id, action string, who ...callerKind) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]string{"id": id, "action": action}
	if msg := p.Snapshot().UnsentMessage; msg != nil && msg.ID == id && action == "send" {
		body["textSha256"] = unsentTextSha256(msg.Text)
	}
	b, _ := json.Marshal(body)
	return postUnsentBody(t, p, string(b), who...)
}

func postUnsentBody(t *testing.T, p *Pane, body string, who ...callerKind) *httptest.ResponseRecorder {
	t.Helper()
	kind := callerHuman
	if len(who) > 0 {
		kind = who[0]
	}
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: kind} }
	defer func() { identifyCallerHook = prev }()
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.SetPathValue("id", p.ID)
	rec := httptest.NewRecorder()
	(&apiServer{mgr: testManagerWithPane(p)}).actOnUnsentMessage(rec, req)
	return rec
}

// A not-typed receipt: Send types the line and Enter, Discard types nothing.
func TestNotTypedReceiptSendTypesItAndDiscardDropsIt(t *testing.T) {
	for _, action := range []string{"send", "clear"} {
		t.Run(action, func(t *testing.T) {
			stubSendInputBusy(t, false)
			p, frames := newTypingHoldPane(t, "pane_shell_"+action)
			p.Agent, p.forceCodexForTest = "", false
			p.recordUntypedMessage("#1 Claude", "make test")
			msg := p.Snapshot().UnsentMessage
			if msg == nil || !msg.NotTyped {
				t.Fatalf("receipt: %+v", msg)
			}
			rec := postUnsentAction(t, p, msg.ID, action)
			if action == "send" {
				expectFrame(t, frames, "line", "make test")
				expectFrame(t, frames, "Enter", "\r")
			}
			expectNoFrame(t, frames, "nothing else typed")
			want := map[string]string{"send": "submitted", "clear": "cleared"}[action]
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) || p.Snapshot().UnsentMessage != nil {
				t.Fatalf("%s: %d %s %+v", action, rec.Code, rec.Body.String(), p.Snapshot().UnsentMessage)
			}
		})
	}
}

// ask_pane to a busy shell types nothing, even a two-letter message that
// send_input would pass through as keys.
func TestAskPaneRefusesABusyPaneWithoutAQueue(t *testing.T) {
	prev := sendInputPaneBusy
	sendInputPaneBusy = func(*Pane) bool { return true }
	t.Cleanup(func() { sendInputPaneBusy = prev })
	for _, text := range []string{"OK", "run the tests"} {
		t.Run(text, func(t *testing.T) {
			p, frames := newTypingHoldPane(t, "pane_shell_ask")
			p.Agent, p.forceCodexForTest = "", false
			a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
			got, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_shell_ask", "text": text, "timeout_seconds": 0.5}, "pane_caller"))
			if !isErr || !strings.Contains(got, "not sent") {
				t.Fatalf("ask_pane typed into a busy pane or did not say why: %q", got)
			}
			expectNoFrame(t, frames, "nothing typed into a busy pane")
			if msg := p.Snapshot().UnsentMessage; msg == nil || !msg.NotTyped || msg.body != text {
				t.Fatalf("no not-typed receipt: %+v", msg)
			}
		})
	}
}

func stubSendInputBusy(t *testing.T, busy bool) *atomic.Bool {
	t.Helper()
	var b atomic.Bool
	b.Store(busy)
	prev := sendInputPaneBusy
	sendInputPaneBusy = func(*Pane) bool { return b.Load() }
	t.Cleanup(func() { sendInputPaneBusy = prev })
	return &b
}

// Send on a not-typed receipt types an agent's text, so it is for the person
// only, after the whole text was shown, and only into a pane that is ready
// for a line: not locked against automatic answers, not busy again, not
// asking a question. A refusal types nothing and keeps the receipt.
func TestNotTypedReceiptSendNeedsThePersonAndAReadyPane(t *testing.T) {
	busy := stubSendInputBusy(t, false)
	p, frames := newTypingHoldPane(t, "pane_shell_ready")
	p.Agent, p.forceCodexForTest = "", false
	var screen atomic.Value
	screen.Store("~/demo$ ")
	stubDeliveryScreen(t, &screen)
	if !p.recordUntypedMessage("#1 Claude", "make test") {
		t.Fatal("receipt not kept")
	}
	msg := p.Snapshot().UnsentMessage
	if b, _ := json.Marshal(msg); !strings.Contains(string(b), `"text":"make test"`) {
		t.Fatalf("the receipt does not carry its text for the chip: %s", b)
	}
	refused := func(step string, who callerKind, code int) {
		t.Helper()
		if rec := postUnsentAction(t, p, msg.ID, "send", who); rec.Code != code {
			t.Fatalf("%s: %d %s", step, rec.Code, rec.Body.String())
		}
		expectNoFrame(t, frames, step+": nothing typed")
		if p.Snapshot().UnsentMessage != msg {
			t.Fatalf("%s: the receipt was dropped", step)
		}
	}
	refused("an agent", callerUnverified, http.StatusForbidden)
	refused("a pane", callerWorker, http.StatusForbidden)
	busy.Store(true)
	refused("busy again", callerHuman, http.StatusConflict)
	busy.Store(false)
	screen.Store("~/demo$ sudo make install\n[sudo] password for dev: ")
	refused("password prompt", callerHuman, http.StatusConflict)
	screen.Store("~/demo$ ")
	p.mu.Lock()
	p.NeverAutoAnswer = true
	p.mu.Unlock()
	refused("locked", callerHuman, http.StatusConflict)
	p.mu.Lock()
	p.NeverAutoAnswer = false
	p.mu.Unlock()
	if rec := postUnsentAction(t, p, msg.ID, "send"); rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	expectFrame(t, frames, "line", "make test")
	expectFrame(t, frames, "Enter", "\r")
}

// A not-typed receipt is never kept for a locked pane, never replaces an
// earlier sender's, and never holds text Send could not type as one plain line.
func TestNotTypedReceiptIsKeptOnlyWhenSafe(t *testing.T) {
	stubSendInputBusy(t, true)
	p, frames := newTypingHoldPane(t, "pane_shell_keep")
	p.Agent, p.forceCodexForTest = "", false
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	send := func(text string) string {
		t.Helper()
		got, isErr := mcpResultText(t, a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": text}, "pane_caller"))
		if !isErr {
			t.Fatalf("%q was typed into a busy pane", text)
		}
		expectNoFrame(t, frames, "nothing typed")
		return got
	}
	p.mu.Lock()
	p.NeverAutoAnswer = true
	p.mu.Unlock()
	if got := send("make test\n"); !strings.Contains(got, "locked against automatic answers") || p.Snapshot().UnsentMessage != nil {
		t.Fatalf("locked pane: %q, receipt %+v", got, p.Snapshot().UnsentMessage)
	}
	p.mu.Lock()
	p.NeverAutoAnswer = false
	p.mu.Unlock()
	send("make test\n")
	first := p.Snapshot().UnsentMessage
	if first == nil || first.body != "make test" {
		t.Fatalf("first receipt: %+v", first)
	}
	if got := send("make clean\n"); !strings.Contains(got, "not kept") || p.Snapshot().UnsentMessage != first {
		t.Fatalf("a second sender replaced the first receipt: %q", got)
	}
	if got := send("make test\n"); strings.Contains(got, "not kept") || p.Snapshot().UnsentMessage != first {
		t.Fatalf("the same message again: %q", got)
	}
	p.dropUnsentMessage(first)
	// The Send… dialog shows the text: nothing that displays differently from
	// the bytes typed is kept (C1 controls, bidi overrides and isolates,
	// zero-width characters, a tab a shell would complete on).
	if got := send("rm -rf \u202e~/cod\n"); !strings.Contains(got, "not kept") || p.Snapshot().UnsentMessage != nil {
		t.Fatalf("bidi override through send_input: %q, receipt %+v", got, p.Snapshot().UnsentMessage)
	}
	for _, text := range []string{"run\x1b[31m tests", "line one\nline two", strings.Repeat("x", untypedReceiptMaxBytes+1),
		"echo \u202egnp.exe", "echo \u2066a\u2069 b", "echo a\u200bb", "rm\u2060 -rf x", "echo \ufeffhi", "echo \u0085x", "echo \u009bx", "make\ttest"} {
		got, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": p.ID, "text": text, "timeout_seconds": 0.5}, "pane_caller"))
		if !isErr || !strings.Contains(got, "not kept") || p.Snapshot().UnsentMessage != nil {
			t.Fatalf("%.20q: %q, receipt %+v", text, got, p.Snapshot().UnsentMessage)
		}
		expectNoFrame(t, frames, "nothing typed")
	}
	// Ordinary non-ASCII text is kept.
	if got := send("echo café ✓ 日本\n"); strings.Contains(got, "not kept") || p.Snapshot().UnsentMessage == nil {
		t.Fatalf("plain Unicode was not kept: %q", got)
	}
}

// Send's person-only check is made again under the delivery lock, against
// the receipt Send would act on: a not-typed receipt that appears after the
// first check is still refused to an agent.
func TestNotTypedSendIsCheckedAgainUnderTheLock(t *testing.T) {
	stubSendInputBusy(t, false)
	p, frames := newTypingHoldPane(t, "pane_shell_relock")
	p.Agent, p.forceCodexForTest = "", false
	var screen atomic.Value
	screen.Store("~/demo$ ")
	stubDeliveryScreen(t, &screen)
	if !p.recordUntypedMessage("#1 Claude", "make test") {
		t.Fatal("receipt not kept")
	}
	msg := p.Snapshot().UnsentMessage
	p.dropUnsentMessage(msg)
	p.deliveryMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postUnsentAction(t, p, msg.ID, "send", callerUnverified) }()
	// The request passed its first check (there was no receipt) and waits
	// for the lock; the receipt appears now.
	deadline := time.Now().Add(5 * time.Second)
	for !goroutineWaitsForLockIn("actOnUnsentMessage") {
		if time.Now().After(deadline) {
			p.deliveryMu.Unlock()
			t.Fatal("the request never reached the delivery lock")
		}
		time.Sleep(time.Millisecond)
	}
	p.mu.Lock()
	p.unsentMessage = msg
	p.mu.Unlock()
	p.deliveryMu.Unlock()
	if rec := <-done; rec.Code != http.StatusForbidden {
		t.Fatalf("an agent sent a not-typed receipt: %d %s", rec.Code, rec.Body.String())
	}
	expectNoFrame(t, frames, "nothing typed")
	if p.Snapshot().UnsentMessage != msg {
		t.Fatal("the receipt was dropped")
	}
	if rec := postUnsentAction(t, p, msg.ID, "send"); rec.Code != 200 {
		t.Fatalf("the person's Send: %d %s", rec.Code, rec.Body.String())
	}
	expectFrame(t, frames, "line", "make test")
}

// goroutineWaitsForLockIn: some goroutine is blocked taking a mutex inside fn.
func goroutineWaitsForLockIn(fn string) bool {
	buf := make([]byte, 1<<20)
	for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, "."+fn+"(") {
			return true
		}
	}
	return false
}
