package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttentionFocusAckPreservesUnansweredPrompt(t *testing.T) {
	for _, kind := range []string{"choice", "awaiting", "native"} {
		t.Run(kind, func(t *testing.T) {
			makePrompt := func() *Pane {
				p := testPaneWithTail(t, "pane-focus-ack", "worker", "Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
				if kind == "awaiting" {
					p = promptPane(t, 4*time.Second)
				} else if kind == "native" {
					p.Kind = "native"
					p.setNativePending(&NativeApproval{RequestID: "pending-A", ToolName: "AskUserQuestion"})
				}
				return p
			}
			p, baseline := makePrompt(), makePrompt()
			e := newAttentionEngine(nil)
			control := newAttentionEngine(nil)
			e.Refresh(p)
			control.Refresh(baseline)
			state, _, episode := p.attentionEpisodeNow()
			if state != AttentionNeedsUser || episode == "" {
				t.Fatalf("fixture has no needs-user episode: %s/%s", state, episode)
			}
			choice := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
			awaiting := atomic.LoadInt64(&p.LastAckedAwaitingInputPos)
			p.SetAutoDeclined("leave this question for the user")
			baseline.SetAutoDeclined("leave this question for the user")
			push := &Pusher{lastFired: map[rateKey]time.Time{
				{paneID: p.ID, kind: "choice-prompt"}:    time.Now(),
				{paneID: "other", kind: "choice-prompt"}: time.Now(),
			}}
			a := &apiServer{mgr: &Manager{panes: map[string]*Pane{p.ID: p}}, pusher: push}
			r := httptest.NewRequest(http.MethodPost, "/api/panes/"+p.ID+"/ack", nil)
			r.SetPathValue("id", p.ID)
			response := httptest.NewRecorder()
			a.ackPane(response, r)
			a.ackPane(httptest.NewRecorder(), r) // repeated focus is still not input
			if response.Code != http.StatusNoContent {
				t.Fatalf("ack status: %d", response.Code)
			}
			if len(push.lastFired) != 1 || push.lastFired[rateKey{paneID: "other", kind: "choice-prompt"}].IsZero() {
				t.Fatal("focus did not acknowledge only its own notification cooldown")
			}
			if got := atomic.LoadInt64(&p.LastAckedChoicePromptPos); got != choice {
				t.Errorf("focus changed choice identity: %d -> %d", choice, got)
			}
			if got := atomic.LoadInt64(&p.LastAckedAwaitingInputPos); got != awaiting {
				t.Errorf("focus changed awaiting identity: %d -> %d", awaiting, got)
			}
			if state, _, got := p.attentionEpisodeNow(); state != AttentionNeedsUser || got != episode {
				t.Errorf("focus changed unresolved episode: %s -> %s (%s)", episode, got, state)
			}
			if p.autoDeclineFor(atomic.LoadInt64(&p.LastAckedChoicePromptPos)) == "" {
				t.Error("focus invalidated the current prompt's auto-decline")
			}
			// Awaiting-input detection intentionally reports once then settles.
			// Force uncached classification on both paths; focus must behave like
			// no focus, not rely on a one-second cached needs-user observation.
			for _, pane := range []*Pane{p, baseline} {
				pane.stateCacheData.mu.Lock()
				pane.stateCacheData.valid = false
				pane.stateCacheData.mu.Unlock()
			}
			e.Refresh(p)
			control.Refresh(baseline)
			gotState, gotReason := p.AttentionNow()
			wantState, wantReason := baseline.AttentionNow()
			if gotState != wantState || gotReason != wantReason {
				t.Errorf("uncached focus attention %s/%s differs from no-focus %s/%s", gotState, gotReason, wantState, wantReason)
			}
			if gotState == AttentionNeedsUser {
				if _, _, got := p.attentionEpisodeNow(); got != episode {
					t.Error("uncached unresolved prompt rotated on focus")
				}
			}
		})
	}
}

// classifyAttention is a pure function — every branch is a table row.
func TestClassifyAttention(t *testing.T) {
	t.Parallel()
	ap := func(tool, input string) *NativeApproval {
		return &NativeApproval{RequestID: "r1", ToolName: tool, RawInput: json.RawMessage(input)}
	}
	cases := []struct {
		name       string
		f          attentionFacts
		want       PaneAttention
		wantReason string // "" = don't care unless want is needs-user with a specific reason; use "*" for "any"
	}{
		{
			name: "busy pane is none",
			f:    attentionFacts{State: StateBusy, AutoMode: "always"},
			want: AttentionNone,
		},
		{
			name: "idle pane is none",
			f:    attentionFacts{State: StateIdle},
			want: AttentionNone,
		},
		{
			name: "awaiting-input always needs the user (auto mode never answers it)",
			f:    attentionFacts{State: StateAwaitingInput, AutoMode: "always"},
			want: AttentionNeedsUser,
		},
		{
			name: "choice-prompt with no auto policy needs the user",
			f:    attentionFacts{State: StateChoicePrompt},
			want: AttentionNeedsUser,
		},
		{
			name: "choice-prompt in always mode is auto-pending",
			f:    attentionFacts{State: StateChoicePrompt, AutoMode: "always", NativeAutoOn: true},
			want: AttentionAutoPending,
		},
		{
			name: "native kill-switch off needs the user despite auto mode",
			f:    attentionFacts{State: StateChoicePrompt, AutoMode: "always", IsNative: true, NativeAutoOn: false},
			want: AttentionNeedsUser,
		},
		{
			name:       "paused gate needs the user",
			f:          attentionFacts{State: StateChoicePrompt, AutoMode: "always", NativeAutoOn: true, AutoGateReason: "paused"},
			want:       AttentionNeedsUser,
			wantReason: "auto-approval is paused",
		},
		{
			name: "burst cap is transient — stays auto-pending",
			f:    attentionFacts{State: StateChoicePrompt, AutoMode: "always", NativeAutoOn: true, AutoGateReason: "burst-cap"},
			want: AttentionAutoPending,
		},
		{
			name: "AskUserQuestion needs the user instantly even in always mode",
			f: attentionFacts{
				State: StateChoicePrompt, AutoMode: "always", IsNative: true, NativeAutoOn: true,
				Pending: []*NativeApproval{ap("AskUserQuestion", `{}`)},
			},
			want:       AttentionNeedsUser,
			wantReason: nativeNeverAutoReason("AskUserQuestion"),
		},
		{
			name: "ExitPlanMode needs the user instantly even in always mode",
			f: attentionFacts{
				State: StateChoicePrompt, AutoMode: "always", IsNative: true, NativeAutoOn: true,
				Pending: []*NativeApproval{ap("ExitPlanMode", `{}`)},
			},
			want:       AttentionNeedsUser,
			wantReason: nativeNeverAutoReason("ExitPlanMode"),
		},
		{
			name: "cautious deny-needle needs the user instantly with the needle reason",
			f: attentionFacts{
				State: StateChoicePrompt, AutoMode: "cautious", IsNative: true, NativeAutoOn: true,
				Pending: []*NativeApproval{ap("Bash", `{"command":"cat .env"}`)},
			},
			want:       AttentionNeedsUser,
			wantReason: "won't auto-approve — touches environment/secret files",
		},
		{
			name: "safe native approval in always mode is auto-pending",
			f: attentionFacts{
				State: StateChoicePrompt, AutoMode: "always", IsNative: true, NativeAutoOn: true,
				Pending: []*NativeApproval{ap("Read", `{"file_path":"main.go"}`)},
			},
			want: AttentionAutoPending,
		},
		{
			name: "recorded decline needs the user with the decline reason",
			f: attentionFacts{
				State: StateChoicePrompt, AutoMode: "cautious", NativeAutoOn: true,
				DeclineReason: "Deferring — writes to ~/.ssh, rule (d).",
			},
			want:       AttentionNeedsUser,
			wantReason: "Deferring — writes to ~/.ssh, rule (d).",
		},
		{
			name:       "input hold explains pending approval",
			f:          attentionFacts{State: StateChoicePrompt, AutoMode: "always", InputHoldReason: "user typing"},
			want:       AttentionAutoPending,
			wantReason: "user typing",
		},
		{
			name:       "input hold retains its reason after escalation",
			f:          attentionFacts{State: StateChoicePrompt, AutoMode: "always", InputHoldReason: "composer has text (unsent input in the pane; auto-approval waits until 10:09:50)", Dwell: attentionAutoGrace},
			want:       AttentionNeedsUser,
			wantReason: "composer has text (unsent input in the pane; auto-approval waits until 10:09:50)",
		},
		{
			name: "manual mode never surfaces automatic input hold",
			f:    attentionFacts{State: StateChoicePrompt, InputHoldReason: "user typing"},
			want: AttentionNeedsUser,
		},
		{
			name: "dwell past the grace escalates to needs-user",
			f: attentionFacts{
				State: StateChoicePrompt, AutoMode: "always", NativeAutoOn: true,
				Dwell: attentionAutoGrace,
			},
			want:       AttentionNeedsUser,
			wantReason: attentionEscalatedReason,
		},
		{
			name: "dwell within the grace stays auto-pending",
			f: attentionFacts{
				State: StateChoicePrompt, AutoMode: "always", NativeAutoOn: true,
				Dwell: attentionAutoGrace - time.Second,
			},
			want: AttentionAutoPending,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyAttention(tc.f)
			if got != tc.want {
				t.Fatalf("classifyAttention() = %q (reason %q), want %q", got, reason, tc.want)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// The dwell clock restarts when a genuinely new menu appears (pos advances),
// when auto-candidacy flips ON (a just-enabled policy gets its full grace),
// and resets fully when the pane leaves choice-prompt.
func TestAttentionEngineDwell(t *testing.T) {
	t.Parallel()
	e := newAttentionEngine(nil)
	t0 := time.Now()

	if d := e.dwell("p1", 1, true, true, t0); d != 0 {
		t.Fatalf("first sighting dwell = %v, want 0", d)
	}
	if d := e.dwell("p1", 1, true, true, t0.Add(10*time.Second)); d != 10*time.Second {
		t.Fatalf("same menu dwell = %v, want 10s", d)
	}
	// New menu at the same pane → clock restarts.
	if d := e.dwell("p1", 2, true, true, t0.Add(12*time.Second)); d != 0 {
		t.Fatalf("new menu dwell = %v, want 0", d)
	}
	if d := e.dwell("p1", 2, true, true, t0.Add(15*time.Second)); d != 3*time.Second {
		t.Fatalf("new menu +3s dwell = %v, want 3s", d)
	}
	// Leaving choice-prompt resets everything.
	if d := e.dwell("p1", 2, false, true, t0.Add(16*time.Second)); d != 0 {
		t.Fatalf("inactive dwell = %v, want 0", d)
	}
	if d := e.dwell("p1", 2, true, true, t0.Add(20*time.Second)); d != 0 {
		t.Fatalf("re-entry dwell = %v, want 0 (fresh window)", d)
	}
}

// Enabling auto mode on a prompt that already sat needs-user for a long time
// must NOT inherit the old dwell — otherwise the escalation fires instantly
// and the just-enabled policy never gets a chance (found live, 2026-07-10).
func TestAttentionEngineDwellResetsWhenAutoEnabled(t *testing.T) {
	t.Parallel()
	e := newAttentionEngine(nil)
	t0 := time.Now()

	// Menu sits with NO auto policy for a minute.
	if d := e.dwell("p1", 1, true, false, t0); d != 0 {
		t.Fatalf("first sighting dwell = %v, want 0", d)
	}
	if d := e.dwell("p1", 1, true, false, t0.Add(time.Minute)); d != time.Minute {
		t.Fatalf("no-auto dwell = %v, want 1m", d)
	}
	// User enables auto mode → the clock restarts so the policy gets its
	// full attentionAutoGrace before escalation.
	if d := e.dwell("p1", 1, true, true, t0.Add(61*time.Second)); d != 0 {
		t.Fatalf("dwell after enabling auto = %v, want 0 (fresh grace)", d)
	}
	if d := e.dwell("p1", 1, true, true, t0.Add(66*time.Second)); d != 5*time.Second {
		t.Fatalf("dwell 5s after enabling auto = %v, want 5s", d)
	}
	// Turning auto OFF and on again re-grants the grace (each enable is a
	// fresh "let auto try" decision).
	if d := e.dwell("p1", 1, true, false, t0.Add(70*time.Second)); d != 9*time.Second {
		t.Fatalf("dwell after disabling auto = %v, want 9s (clock keeps running)", d)
	}
	if d := e.dwell("p1", 1, true, true, t0.Add(80*time.Second)); d != 0 {
		t.Fatalf("dwell after re-enabling auto = %v, want 0", d)
	}
}

// SetAutoDeclined is scoped to the menu position it was recorded against:
// a new menu invalidates it so a decline on menu A never mutes menu B.
func TestAutoDeclineScopedToMenuPos(t *testing.T) {
	t.Parallel()
	p := &Pane{ID: "p1"}
	p.LastAckedChoicePromptPos = 5
	p.SetAutoDeclined("  too risky  ")
	if got := p.autoDeclineFor(5); got != "too risky" {
		t.Fatalf("autoDeclineFor(5) = %q, want trimmed reason", got)
	}
	if got := p.autoDeclineFor(6); got != "" {
		t.Fatalf("autoDeclineFor(6) = %q, want '' (new menu invalidates)", got)
	}
	// Empty reason falls back to a generic phrase rather than vanishing.
	p.SetAutoDeclined("   ")
	if got := p.autoDeclineFor(5); got == "" || !strings.Contains(got, "auto-approval") {
		t.Fatalf("empty-reason fallback = %q, want generic phrase", got)
	}
}

// setAttention change detection: the very first store of "none" is not a
// change (nothing to announce), but any real value or later flip is.
func TestSetAttentionChangeDetection(t *testing.T) {
	t.Parallel()
	p := &Pane{ID: "p1"}
	if p.setAttention(AttentionNone, "") {
		t.Fatal("first 'none' should not report a change")
	}
	if !p.setAttention(AttentionAutoPending, "") {
		t.Fatal("none → auto-pending should report a change")
	}
	if p.setAttention(AttentionAutoPending, "") {
		t.Fatal("same value should not report a change")
	}
	if !p.setAttention(AttentionNeedsUser, "declined") {
		t.Fatal("auto-pending → needs-user should report a change")
	}
	if !p.setAttention(AttentionNeedsUser, "different reason") {
		t.Fatal("reason change should report a change")
	}
	if att, reason := p.AttentionNow(); att != AttentionNeedsUser || reason != "different reason" {
		t.Fatalf("AttentionNow() = %q/%q", att, reason)
	}
}

func TestNeedsUserEpisodeTracksPromptIdentityWithoutUIChurn(t *testing.T) {
	t.Parallel()
	p := &Pane{ID: "prompt-identity"}
	a := attentionPromptKey{State: StateChoicePrompt, Pos: 101}
	b := attentionPromptKey{State: StateChoicePrompt, Pos: 202}
	if !p.setAttentionForPrompt(AttentionNeedsUser, "", a) {
		t.Fatal("first prompt did not change visible attention")
	}
	_, _, first := p.attentionEpisodeNow()
	if first == "" {
		t.Fatal("first prompt has no episode")
	}
	if p.setAttentionForPrompt(AttentionNeedsUser, "", a) {
		t.Fatal("same prompt churned visible attention")
	}
	if _, _, same := p.attentionEpisodeNow(); same != first {
		t.Fatal("unchanged prompt churned episode")
	}
	// A new menu can carry the same empty manual-mode reason. Its episode
	// must change while the UI-facing attention state remains unchanged.
	if p.setAttentionForPrompt(AttentionNeedsUser, "", b) {
		t.Fatal("new prompt emitted a duplicate visible attention change")
	}
	_, _, second := p.attentionEpisodeNow()
	if second == first || second == "" {
		t.Fatal("same-reason prompt B retained A's episode")
	}
	if !p.setAttentionForPrompt(AttentionNeedsUser, "different reason", b) {
		t.Fatal("reason change was not visible")
	}
	if _, _, same := p.attentionEpisodeNow(); same != second {
		t.Fatal("reason-only change rotated the episode")
	}
	c := attentionPromptKey{State: StateAwaitingInput, Pos: 202}
	if p.setAttentionForPrompt(AttentionNeedsUser, "different reason", c) {
		t.Fatal("runtime-state identity change emitted duplicate UI change")
	}
	if _, _, third := p.attentionEpisodeNow(); third == second {
		t.Fatal("runtime-state change retained old episode")
	}
}

func TestAttentionRefreshUsesChoicePromptIdentity(t *testing.T) {
	t.Parallel()
	p := testPaneWithTail(t, "refresh-prompt-identity", "worker", "Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
	e := newAttentionEngine(nil)
	e.Refresh(p)
	if state, reason := p.AttentionNow(); state != AttentionNeedsUser || reason != "" {
		t.Fatalf("first prompt attention = %q/%q, want needs-user with empty reason", state, reason)
	}
	firstPos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
	if firstPos == 0 {
		t.Fatal("choice prompt identity was not recorded")
	}
	_, _, first := p.attentionEpisodeNow()
	e.Refresh(p)
	if _, _, same := p.attentionEpisodeNow(); same != first {
		t.Fatal("stable prompt changed episode on Refresh")
	}
	// The state detector advances this position when a back-to-back choice
	// prompt replaces the first one. Refresh must carry it into the episode.
	atomic.StoreInt64(&p.LastAckedChoicePromptPos, firstPos+100)
	e.Refresh(p)
	if _, _, second := p.attentionEpisodeNow(); second == first || second == "" {
		t.Fatal("Refresh kept the previous prompt's episode")
	}
}

func TestAttentionRefreshRejectsOlderOutOfOrderPrompt(t *testing.T) {
	t.Parallel()
	p := testPaneWithTail(t, "out-of-order-prompt", "worker", "Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
	e := newAttentionEngine(nil)
	e.Refresh(p)
	aPos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
	if aPos == 0 {
		t.Fatal("choice prompt A has no position")
	}
	observedA := make(chan struct{})
	releaseA := make(chan struct{})
	defer func() {
		select {
		case <-releaseA:
		default:
			close(releaseA)
		}
	}()
	e.beforeAttentionStore = func(_ *Pane, key attentionPromptKey) {
		if key.Pos == aPos {
			close(observedA)
			<-releaseA
		}
	}
	finishedA := make(chan struct{})
	go func() {
		e.Refresh(p)
		close(finishedA)
	}()
	select {
	case <-observedA:
	case <-time.After(2 * time.Second):
		t.Fatal("older Refresh did not capture prompt A")
	}
	bPos := aPos + 100
	atomic.StoreInt64(&p.LastAckedChoicePromptPos, bPos)
	e.Refresh(p) // B publishes while the older A observation is paused.
	_, _, bEpisode := p.attentionEpisodeNow()
	if rec := p.attention.Load(); rec.Prompt.Pos != bPos || bEpisode == "" {
		t.Fatalf("prompt B did not publish: %+v", rec)
	}
	e.Refresh(p) // A newer unchanged observation must also fence old A.
	close(releaseA)
	select {
	case <-finishedA:
	case <-time.After(2 * time.Second):
		t.Fatal("older Refresh did not finish")
	}
	if rec := p.attention.Load(); rec.Prompt.Pos != bPos || rec.Episode != bEpisode {
		t.Fatalf("older A replaced the valid B episode: %+v", rec)
	}
	e.Refresh(p)
	if _, _, stable := p.attentionEpisodeNow(); stable != bEpisode {
		t.Fatal("stable B observation rotated its episode")
	}
}

func TestAttentionRefreshUsesAwaitingInputPromptIdentity(t *testing.T) {
	t.Parallel()
	p := promptPane(t, 4*time.Second)
	e := newAttentionEngine(nil)
	e.Refresh(p)
	if state, reason := p.AttentionNow(); state != AttentionNeedsUser || reason != "" {
		t.Fatalf("first settled prompt attention = %q/%q", state, reason)
	}
	firstPos := atomic.LoadInt64(&p.LastAckedAwaitingInputPos)
	if firstPos == 0 {
		t.Fatal("first settled prompt did not advance its identity")
	}
	_, _, first := p.attentionEpisodeNow()
	e.Refresh(p)
	if _, _, same := p.attentionEpisodeNow(); same != first {
		t.Fatal("stable settled prompt changed episode")
	}
	// A new user turn followed by a new settled reply advances the awaiting
	// counter, while the choice-prompt counter remains unchanged.
	atomic.StoreInt64(&p.LastInputStrippedPos, firstPos+1)
	newReply := "another reply has finished\n❯ \n~/proj [Opus 4.8] | ctx: 42%\n"
	if _, err := p.buffer.Write([]byte(newReply)); err != nil {
		t.Fatal(err)
	}
	atomic.AddInt64(&p.TotalStrippedChars, int64(len(newReply)))
	atomic.StoreInt64(&p.LastActivityMs, time.Now().Add(-4*time.Second).UnixMilli())
	e.Refresh(p)
	if state, reason := p.AttentionNow(); state != AttentionNeedsUser || reason != "" {
		t.Fatalf("second settled prompt attention = %q/%q", state, reason)
	}
	if secondPos := atomic.LoadInt64(&p.LastAckedAwaitingInputPos); secondPos <= firstPos {
		t.Fatalf("second settled prompt position = %d, want > %d", secondPos, firstPos)
	}
	if choicePos := atomic.LoadInt64(&p.LastAckedChoicePromptPos); choicePos != 0 {
		t.Fatalf("choice position changed during awaiting-input test: %d", choicePos)
	}
	_, _, second := p.attentionEpisodeNow()
	if second == first || second == "" {
		t.Fatal("second settled prompt retained the first episode")
	}
	e.Refresh(p)
	if _, _, same := p.attentionEpisodeNow(); same != second {
		t.Fatal("stable second settled prompt changed episode")
	}
}

func TestAttentionRefreshUsesNativePendingRequestIDs(t *testing.T) {
	t.Parallel()
	p := testPaneWithTail(t, "native-prompt-identity", "worker", "")
	p.Kind = "native"
	e := newAttentionEngine(nil)
	p.setNativePending(&NativeApproval{RequestID: "A", ToolName: "AskUserQuestion"})
	e.Refresh(p)
	if state, reason := p.AttentionNow(); state != AttentionNeedsUser || reason != "" {
		t.Fatalf("native A attention = %q/%q", state, reason)
	}
	_, _, onlyA := p.attentionEpisodeNow()
	e.Refresh(p)
	if _, _, same := p.attentionEpisodeNow(); same != onlyA {
		t.Fatal("stable native A changed episode")
	}
	p.setNativePending(&NativeApproval{RequestID: "B", ToolName: "AskUserQuestion"})
	e.Refresh(p)
	_, _, both := p.attentionEpisodeNow()
	if both == onlyA || both == "" {
		t.Fatal("native B arrival retained A's episode")
	}
	if state, reason := p.AttentionNow(); state != AttentionNeedsUser || reason != "" {
		t.Fatalf("native A+B attention = %q/%q", state, reason)
	}
	pos := atomic.LoadInt64(&p.LastAckedChoicePromptPos)
	if p.takeNativePending("A") == nil {
		t.Fatal("native A was not pending")
	}
	e.Refresh(p)
	if got := atomic.LoadInt64(&p.LastAckedChoicePromptPos); got != pos {
		t.Fatalf("removing A advanced choice position: %d to %d", pos, got)
	}
	_, _, onlyB := p.attentionEpisodeNow()
	if onlyB == both || onlyB == "" {
		t.Fatal("native A+B to B retained A+B episode")
	}
	if state, reason := p.AttentionNow(); state != AttentionNeedsUser || reason != "" {
		t.Fatalf("native B attention = %q/%q", state, reason)
	}
	e.Refresh(p)
	if _, _, same := p.attentionEpisodeNow(); same != onlyB {
		t.Fatal("stable native B changed episode")
	}
}

// pushEffectiveState holds the pusher's grace clock while auto-approval is
// expected to act, passes needs-user and unknown attention through, and
// never touches non-attention states.
func TestPushEffectiveState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cur  PaneRuntimeState
		att  PaneAttention
		want PaneRuntimeState
	}{
		{StateChoicePrompt, AttentionAutoPending, StateBusy},
		{StateAwaitingInput, AttentionAutoPending, StateBusy},
		{StateChoicePrompt, AttentionNeedsUser, StateChoicePrompt},
		{StateChoicePrompt, "", StateChoicePrompt}, // unknown → assume human needed
		{StateChoicePrompt, AttentionNone, StateChoicePrompt},
		{StateBusy, AttentionAutoPending, StateBusy},
		{StateIdle, AttentionNeedsUser, StateIdle},
	}
	for _, tc := range cases {
		if got := pushEffectiveState(tc.cur, tc.att); got != tc.want {
			t.Errorf("pushEffectiveState(%q, %q) = %q, want %q", tc.cur, tc.att, got, tc.want)
		}
	}
}

// End-to-end through the pusher's graceTracker: an auto-pending prompt never
// starts the grace clock; the clock starts when attention flips to
// needs-user and fires attentionGrace later.
func TestGraceTrackerWithAttentionFold(t *testing.T) {
	t.Parallel()
	g := newGraceTracker(time.Now().Add(-time.Minute), attentionGrace)
	t0 := time.Now()
	step := func(att PaneAttention, offset time.Duration, prev PaneRuntimeState) (pushVerdict, PaneRuntimeState) {
		cur := pushEffectiveState(StateChoicePrompt, att)
		return g.eval("p1", cur, prev, t0.Add(offset)), cur
	}

	// 30s of auto-pending (well past the grace): never fires.
	prev := StateIdle
	for i := 0; i <= 15; i++ {
		v, cur := step(AttentionAutoPending, time.Duration(i)*2*time.Second, prev)
		if v == pushFire {
			t.Fatalf("fired during auto-pending at +%ds", i*2)
		}
		prev = cur
	}
	// Decline: attention flips to needs-user at +32s → fresh entry, clock starts.
	v, cur := step(AttentionNeedsUser, 32*time.Second, prev)
	if v == pushFire {
		t.Fatal("fired on the needs-user entry edge (grace should start, not fire)")
	}
	prev = cur
	// Still waiting within the grace.
	if v, _ = step(AttentionNeedsUser, 32*time.Second+attentionGrace-time.Second, prev); v == pushFire {
		t.Fatal("fired before the grace elapsed")
	}
	// Grace elapsed → fire.
	if v, _ = step(AttentionNeedsUser, 32*time.Second+attentionGrace, prev); v != pushFire {
		t.Fatal("did not fire after the grace elapsed post-decline")
	}
}

// CurrentFor must reconcile the stored record against the state the caller's
// DTO is about to ship. The 2s ticker leaves a window where a menu appeared
// (or resolved) after the last evaluation: shipping state=choice-prompt with
// the pre-prompt record made every attention consumer flash "needs you" for
// a beat before auto-approval acted (observed live on the footer chip,
// 2026-07-10). Both directions of disagreement force a synchronous refresh;
// agreement returns the record untouched.
func TestCurrentForReconcilesStaleRecord(t *testing.T) {
	t.Parallel()
	e := newAttentionEngine(nil)

	// A menu appeared between ticks: real choice-prompt in the buffer, but
	// the record is still empty (engine never evaluated this pane). The
	// pre-fix DTO shipped "" (absent on the wire → clients assume the human
	// is needed). CurrentFor must classify NOW instead.
	p := &Pane{
		ID:     "pane-currentfor-fresh",
		Cols:   80,
		Rows:   24,
		Status: PaneRunning,
		buffer: NewRingBuffer(1 << 16),
		subs:   map[int64]*subEntry{},
	}
	menu := strings.Join([]string{
		"Bash command",
		"go test ./...",
		"Do you want to proceed?",
		"❯ 1. Yes",
		"  2. No",
		"",
	}, "\n")
	if _, err := p.buffer.Write([]byte(menu)); err != nil {
		t.Fatalf("buffer.Write: %v", err)
	}
	atomic.StoreInt64(&p.TotalStrippedChars, int64(len(menu)))

	state := p.computeStateCached()
	if state != StateChoicePrompt {
		t.Fatalf("fixture state = %v, want choice-prompt", state)
	}
	att, _ := e.CurrentFor(p, state)
	// No ARIA on this engine → no auto policy can act → needs-user. The
	// point is it is CLASSIFIED (non-empty, prompt-consistent), not stale.
	if att != AttentionNeedsUser {
		t.Fatalf("CurrentFor on fresh prompt = %q, want needs-user (classified now, not stale %q)", att, "")
	}

	// The inverse window: the prompt resolved between ticks but the record
	// still says needs-user. The state filter downstream usually hides this,
	// but the DTO should not ship the contradiction either.
	q := &Pane{
		ID:     "pane-currentfor-resolved",
		Cols:   80,
		Rows:   24,
		Status: PaneRunning,
		buffer: NewRingBuffer(1 << 16),
		subs:   map[int64]*subEntry{},
	}
	q.setAttention(AttentionNeedsUser, "stale")
	att, reason := e.CurrentFor(q, q.computeStateCached())
	if att != AttentionNone || reason != "" {
		t.Fatalf("CurrentFor on resolved pane = %q/%q, want none with no reason", att, reason)
	}

	// Agreement: record and state both see the prompt → returned as stored,
	// reason intact (no spurious reclassification churn).
	p.setAttention(AttentionNeedsUser, "kept-reason")
	att, reason = e.CurrentFor(p, StateChoicePrompt)
	if att != AttentionNeedsUser || reason != "kept-reason" {
		t.Fatalf("CurrentFor with agreeing record = %q/%q, want needs-user/kept-reason", att, reason)
	}
}

// Attention is a local signal. Even an enabled operator's pane must not
// request a completion merely because a person needs to answer it.
func TestAttentionRefreshNeverRequestsCompletion(t *testing.T) {
	completionTestVerdict(t, true)
	for _, mode := range []string{"", "cautious", "always"} {
		for _, native := range []bool{false, true} {
			t.Run(mode+"/native="+strconv.FormatBool(native), func(t *testing.T) {
				fixture, calls := completionTestServer(t)
				p := testPaneWithTail(t, "local-attention", "Fixture", "Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
				p.AutoMode = mode
				if native {
					p.Kind = "native"
					p.setNativePending(&NativeApproval{RequestID: "fixture-request", ToolName: "AskUserQuestion", RawInput: json.RawMessage(`{"question":"Review this?"}`)})
				}
				p.computeStateCached()
				p.SetAutoDeclined("Review this request")
				mgr := testManagerWithPane(p)
				base, credential := fixture.cloudCreds()
				a, err := NewAria(mgr, nil, nil, nil, base, credential, t.TempDir(), nil)
				if err != nil {
					t.Fatal("could not construct attention fixture")
				}
				engine := newAttentionEngine(a.mgr)
				engine.SetAria(a)
				for i := 0; i < 3; i++ {
					engine.Refresh(p)
				}
				if state, _ := p.AttentionNow(); state != AttentionNeedsUser {
					t.Fatal("fixture does not need the person")
				}
				time.Sleep(100 * time.Millisecond)
				if calls.Load() != 0 {
					t.Fatal("attention refresh sent pane text for a completion")
				}
			})
		}
	}
}
