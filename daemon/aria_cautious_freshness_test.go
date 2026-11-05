package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"soluterminal-shared/typesafe"
)

func TestCautiousCancelledPromptSurvivesChangedPrechecks(t *testing.T) {
	for _, change := range []string{"raw focus during repaint", "pilot disabled"} {
		t.Run(change, func(t *testing.T) {
			a, p, frames, calls := cautiousAcceptanceFixture(t)
			_, pos, hash, ok := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
			if !ok || hash == 0 {
				t.Fatal("missing original prompt identity")
			}
			a.rememberJevCautious(p.ID, pos, hash, "cancelled")
			t.Setenv(ariaJevAutoEnv, "1")
			if change == "raw focus during repaint" {
				// Raw output can change a provider precheck before the grid's
				// current permission identity changes during a cell repaint.
				focus := "\n❯ 2. No (esc)\n"
				if _, err := p.buffer.Write([]byte(focus)); err != nil {
					t.Fatal(err)
				}
				atomic.AddInt64(&p.TotalStrippedChars, int64(len(focus)))
			} else {
				t.Setenv(ariaJevAutoEnv, "0")
			}
			_, nextPos, nextHash, ok := currentChoicePromptIdentity(p, "")
			if !ok || nextPos != pos || nextHash != hash {
				t.Fatalf("precheck change replaced identity: ok=%v pos=%d->%d hash=%x->%x", ok, pos, nextPos, hash, nextHash)
			}
			if !a.tryJevCautiousTerminal(context.Background(), p, p.ID, p.Title, "", time.Now()) {
				t.Fatal("cancelled prompt fell through to the team evaluator")
			}
			if calls.Load() != 0 {
				t.Fatal("cancelled prompt called the provider")
			}
			expectNoFrame(t, frames, "cancelled prompt with changed precheck")
		})
	}
}

func TestCautiousCancelledPromptSkipsEarlierDetector(t *testing.T) {
	a, p, frames, calls := cautiousAcceptanceFixture(t)
	_, pos, hash, ok := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
	if !ok {
		t.Fatal("missing permission identity")
	}
	a.rememberJevCautious(p.ID, pos, hash, "cancelled")
	if !a.tryJevAmbiguousTerminal(context.Background(), p, p.ID, p.Title, time.Now()) {
		t.Fatal("cancelled permission escaped the earlier detector path")
	}
	if calls.Load() != 0 {
		t.Fatal("cancelled permission called the detector")
	}
	expectNoFrame(t, frames, "cancelled permission before detector")
}

func TestCautiousCancelledPromptDoesNotBlockDifferentPermission(t *testing.T) {
	a, p, frames, calls := cautiousAcceptanceFixture(t)
	_, pos, hash, _ := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
	a.rememberJevCautious(p.ID, pos, hash, "cancelled")
	next := strings.Replace(cautiousAcceptanceMenu, "git status --short", "git diff --stat", 1)
	appendDirectAutoMenuRedraw(t, p, next)
	seedGridMenu(t, p, next)
	a.jevClientFactory = lowConfidenceJev(calls)
	_, outcome := a.jevCautiousDecide(context.Background(), p, p.ID, p.Title, next, time.Now(), true)
	if outcome != "uncertain_fallback" || calls.Load() != 1 {
		t.Fatalf("new permission outcome=%q calls=%d", outcome, calls.Load())
	}
	expectNoFrame(t, frames, "new uncertain permission")
}

func TestCautiousUncertainVerdictDoesNotPoisonRearmedPolicy(t *testing.T) {
	a, p, frames, _ := cautiousAcceptanceFixture(t)
	_, pos, hash, _ := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
	a.jevClientFactory = func(string) jevSystemOneClient {
		return ariaJevSafetyClient{call: func(context.Context, string, any, map[string]typesafe.Question) (*typesafe.Response, error) {
			p.SetAutoMode("")
			p.SetAutoMode("cautious")
			a.resetAutoModeEvaluation(p)
			response := ariaJevSafeResponse()
			doubt := .1
			response.Answers["safe_action"] = typesafe.Answer{Type: "noul", Noul: &doubt}
			return response, nil
		}}
	}
	a.evaluateObservation(context.Background(), p.ID, p.Title, cautiousAcceptanceMenu)
	if _, seen := a.jevCautiousSeen(p.ID, pos, hash); seen || a.BackoffStreak(p.ID) != 0 {
		t.Fatal("revoked provider response affected the rearmed policy")
	}
	expectNoFrame(t, frames, "revoked uncertain response")
}

func TestCautiousOldCancellationDoesNotPoisonRearmedPolicy(t *testing.T) {
	a, p, frames, _ := cautiousAcceptanceFixture(t)
	_, pos, hash, _ := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
	confirm, done := startCautiousAcceptance(t, a, p)
	p.SetAutoMode("")
	p.SetAutoMode("cautious")
	a.resetAutoModeEvaluation(p)
	if !a.signalConfirm(confirm.ID, false, false) {
		t.Fatal("could not finish the old confirmation")
	}
	finishCautiousAcceptance(t, done)
	if _, seen := a.jevCautiousSeen(p.ID, pos, hash); seen || a.BackoffStreak(p.ID) != 0 {
		t.Fatal("revoked cancellation affected the rearmed policy")
	}
	expectNoFrame(t, frames, "old cancellation after policy change")
}

func TestCautiousWriteDeadlineReleasesPolicyLock(t *testing.T) {
	paneEnd, agentEnd := net.Pipe()
	defer paneEnd.Close()
	defer agentEnd.Close()
	p := &Pane{AutoMode: "cautious", agentConn: paneEnd}
	policy := p.autoModePolicy()
	written := make(chan error, 1)
	go func() {
		_, err := p.writeWithAutoModePolicy([]byte("\r"), false, policy, time.Now().Add(300*time.Millisecond))
		written <- err
	}()
	// Accept just the frame header, leaving the payload blocked. This both
	// exercises a partial frame and proves ordinary pane reads remain usable.
	_ = agentEnd.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(agentEnd, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	snapshot := make(chan struct{})
	go func() { p.Snapshot(); close(snapshot) }()
	select {
	case <-snapshot:
	case <-time.After(100 * time.Millisecond):
		t.Error("blocked approval write held the pane snapshot lock")
	}
	if err := <-written; err == nil {
		t.Fatal("blocked transport did not time out")
	}
	if _, err := p.WriteNoPromptRecord([]byte("next frame")); err == nil {
		t.Fatal("partially written frame left a corrupt stream available for reuse")
	}
	done := make(chan struct{})
	go func() { p.SetAutoMode(""); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failed transport retained the policy lock")
	}
}

func TestCautiousMemoPublicationRejectsOldGeneration(t *testing.T) {
	a, p, _, _ := cautiousAcceptanceFixture(t)
	_, pos, hash, _ := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
	_, old := withAriaAutoModePolicy(context.Background(), p)
	p.SetAutoMode("")
	p.SetAutoMode("cautious")
	a.rememberJevCautious(p.ID, pos, hash, "cancelled")
	// The HTTP setter resets caches after changing the mode. A concurrent
	// cancellation belonging to the new policy must survive that reset.
	a.resetAutoModeEvaluation(p)
	a.rememberJevCautiousForPolicy(old, pos, hash, "uncertain_fallback")
	if outcome, seen := a.jevCautiousSeen(p.ID, pos, hash); !seen || outcome != "cancelled" {
		t.Fatalf("old evaluation overwrote the new decision: %q, seen=%v", outcome, seen)
	}
}

func TestCautiousSystemRefusalDoesNotRememberCancellation(t *testing.T) {
	for _, refusal := range []string{"daemon refusal", "approve while paused"} {
		t.Run(refusal, func(t *testing.T) {
			a, p, frames, _ := cautiousAcceptanceFixture(t)
			_, pos, hash, _ := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
			confirm, done := startCautiousAcceptance(t, a, p)
			var resolved bool
			if refusal == "daemon refusal" {
				resolved = a.signalAutoConfirm(confirm.ID, false)
			} else {
				a.SetPaused(true)
				resolved = a.signalConfirm(confirm.ID, true, false)
			}
			if !resolved {
				t.Fatal("could not resolve confirmation")
			}
			finishCautiousAcceptance(t, done)
			if _, seen := a.jevCautiousSeen(p.ID, pos, hash); seen || a.BackoffStreak(p.ID) != 0 {
				t.Fatal("system refusal was cached as a user cancellation or failure")
			}
			expectNoFrame(t, frames, refusal)
		})
	}
}

func TestCautiousPolicyRevokedDuringProviderCall(t *testing.T) {
	a, p, frames, _ := cautiousAcceptanceFixture(t)
	a.jevClientFactory = func(string) jevSystemOneClient {
		return ariaJevSafetyClient{call: func(context.Context, string, any, map[string]typesafe.Question) (*typesafe.Response, error) {
			p.SetAutoMode("")
			p.SetAutoMode("cautious")
			return ariaJevSafeResponse(), nil
		}}
	}
	events := a.uiBus.subscribe()
	defer a.uiBus.unsubscribe(events)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	a.evaluateObservation(ctx, p.ID, p.Title, cautiousAcceptanceMenu)
	for len(events) > 0 {
		if event := <-events; event.Type == "aria-auto-needs-confirm" {
			t.Fatal("revoked provider verdict opened a new countdown")
		}
	}
	expectNoFrame(t, frames, "revoked provider verdict")
	if a.BackoffStreak(p.ID) != 0 {
		t.Fatal("revoked policy counted as a failed approval")
	}
}

func TestCautiousRevokedTeamTextReplyDoesNotCountAsFailure(t *testing.T) {
	a, p, frames, _ := cautiousAcceptanceFixture(t)
	completionTestVerdict(t, true)
	a.teamFeatures = func() bool { return true }
	a.settings = &ariaSettingsStore{}
	t.Setenv(ariaJevAutoEnv, "0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/aria/complete" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		p.SetAutoMode("")
		p.SetAutoMode("cautious")
		a.resetAutoModeEvaluation(p)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Leave this to the user."}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			`data: {"type":"message_stop"}`,
			"",
		}, "\n\n"))
	}))
	defer server.Close()
	a.cloudBase = server.URL
	a.evaluateObservation(context.Background(), p.ID, p.Title, cautiousAcceptanceMenu)
	if a.BackoffStreak(p.ID) != 0 {
		t.Fatal("revoked team reply counted against the new policy")
	}
	outcomes := a.recentEvalOutcomes(p.ID)
	if len(outcomes) == 0 || outcomes[len(outcomes)-1].Kind != "held" {
		t.Fatalf("revoked team reply outcome: %+v", outcomes)
	}
	expectNoFrame(t, frames, "revoked team reply")
}

func TestCautiousTeamToolCancellationIsRemembered(t *testing.T) {
	a, p, frames, _ := cautiousAcceptanceFixture(t)
	_, pos, hash, ok := currentChoicePromptIdentity(p, cautiousAcceptanceMenu)
	if !ok {
		t.Fatal("missing permission identity")
	}
	events := make(chan map[string]any, 8)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		done <- a.toolSendInput(ctx, "team_aria", p.ID, "1\n", pos, hash, true, func(event map[string]any) { events <- event })
	}()
	confirmID := waitNeedsConfirm(t, events, "send_input")
	if !a.signalConfirm(confirmID, false, false) {
		t.Fatal("could not cancel team confirmation")
	}
	if result := <-done; !strings.HasPrefix(result, "user rejected") {
		t.Fatalf("unexpected cancellation result: %s", result)
	}
	t.Setenv(ariaJevAutoEnv, "0")
	if !a.tryJevCautiousTerminal(ctx, p, p.ID, p.Title, "", time.Now()) {
		t.Fatal("team cancellation was not remembered across evaluator fallback")
	}
	expectNoFrame(t, frames, "cancelled team permission")
}

// A pacing timer belongs to the input state that scheduled it. This models a
// queued five-second hold after the human has answered, with the old menu still
// present on a lagging rendered screen. Times are aged to pass the typing hold.
func TestAutoFirePacedRetryStopsAfterInterveningInput(t *testing.T) {
	a, p, frames, _ := newDirectAutoRepeatHarness(t, "pane-old-retry")
	p.SetAutoMode("always")
	atomic.StoreInt64(&p.LastInputMs, time.Now().Add(-20*time.Second).UnixMilli())
	a.autoFireHold(p.ID, autoFireSettleKey(p), time.Now())
	a.retryAutoFireAfter(p.ID, p.Title, 20*time.Millisecond)
	atomic.StoreInt64(&p.LastInputMs, time.Now().Add(-10*time.Second).UnixMilli())
	time.Sleep(100 * time.Millisecond)
	expectNoFrame(t, frames, "old pacing timer after another input")
}

func TestAutoFirePacedRetryDoesNotSendLateEnterAfterAnswer(t *testing.T) {
	a, p, frames, _ := newDirectAutoRepeatHarness(t, "pane-late-enter")
	p.SetAutoMode("always")
	atomic.StoreInt64(&p.LastInputMs, time.Now().Add(-20*time.Second).UnixMilli())
	a.autoFireHold(p.ID, autoFireSettleKey(p), time.Now())
	wait := productionAutoFirePacing.unrendered
	if wait < 5*time.Second {
		t.Fatal("test requires the production five-second unrendered hold")
	}
	a.retryAutoFireAfter(p.ID, p.Title, wait)
	if _, err := p.WriteNoPromptRecord([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "human answered the permission", "\r")
	time.Sleep(wait + 100*time.Millisecond)
	expectNoFrame(t, frames, "late Enter after answered permission")
}

func TestAutoFirePacedRetryStopsAfterPolicyCycle(t *testing.T) {
	a, p, frames, _ := newDirectAutoRepeatHarness(t, "pane-retry-policy")
	p.SetAutoMode("always")
	a.autoFireHold(p.ID, autoFireSettleKey(p), time.Now())
	a.retryAutoFireAfter(p.ID, p.Title, 20*time.Millisecond)
	p.SetAutoMode("")
	p.SetAutoMode("always")
	time.Sleep(100 * time.Millisecond)
	expectNoFrame(t, frames, "old pacing timer after policy cycle")
}

func TestAutoFireVerificationStopsAfterPolicyCycle(t *testing.T) {
	a, p, frames := newClaudeVerifyHarness(t, "pane-verify-policy")
	fireFirstClaudeDialog(t, a, p, frames)
	p.SetAutoMode("")
	p.SetAutoMode("always")
	time.Sleep(200 * time.Millisecond)
	expectNoFrame(t, frames, "old dropped-Enter verification after policy cycle")
}

func TestCautiousWriteRejectsRevokedPolicyAfterWriterWait(t *testing.T) {
	_, p, frames, _ := cautiousAcceptanceFixture(t)
	policy := p.autoModePolicy()
	p.agentWriteMu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := p.writeWithAutoModePolicy([]byte("\r"), false, policy, time.Now().Add(time.Second))
		result <- err
	}()
	p.SetAutoMode("")
	p.SetAutoMode("cautious")
	p.agentWriteMu.Unlock()
	select {
	case err := <-result:
		if !errors.Is(err, errAutoModePolicyChanged) {
			t.Fatalf("queued approval result = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued approval did not stop")
	}
	expectNoFrame(t, frames, "policy revoked while waiting for the input lock")
}

func TestCautiousWriteAcceptsUnchangedPolicy(t *testing.T) {
	_, p, frames, _ := cautiousAcceptanceFixture(t)
	policy := p.autoModePolicy()
	p.SetAutoMode("cautious") // a repeated setting is not a transition
	if _, err := p.writeWithAutoModePolicy([]byte("\r"), false, policy, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, frames, "current policy", "\r")
}

func TestCautiousWriteBeforeFirstByteKeepsPaneConnected(t *testing.T) {
	for _, cause := range []string{"deadline expired while queued", "oversized frame"} {
		t.Run(cause, func(t *testing.T) {
			_, p, frames, _ := cautiousAcceptanceFixture(t)
			policy := p.autoModePolicy()
			var err error
			if cause == "deadline expired while queued" {
				p.agentWriteMu.Lock()
				done := make(chan error, 1)
				deadline := time.Now().Add(20 * time.Millisecond)
				go func() {
					_, writeErr := p.writeWithAutoModePolicy([]byte("\r"), false, policy, deadline)
					done <- writeErr
				}()
				time.Sleep(40 * time.Millisecond)
				p.agentWriteMu.Unlock()
				err = <-done
			} else {
				_, err = p.writeWithAutoModePolicy(make([]byte, MaxFramePayload+1), false, policy, time.Now().Add(time.Second))
				if !errors.Is(err, ErrFrameTooLarge) {
					t.Fatalf("oversized frame result = %v", err)
				}
			}
			if err == nil {
				t.Fatal("guarded write unexpectedly succeeded")
			}
			expectNoFrame(t, frames, cause)
			if _, err := p.WriteNoPromptRecord([]byte("still connected")); err != nil {
				t.Fatalf("zero-byte failure disconnected the pane: %v", err)
			}
			expectFrame(t, frames, "manual input after zero-byte failure", "still connected")
		})
	}
}
