package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"soluterminal-shared/typesafe"
)

// The live Codex pane had a clean rendered permission while its cell-diff
// output left no focused menu in the last 30 raw lines. Both the model and
// the final daemon gate must see the permission on screen.
const cautiousRenderedCodexMenu = "Would you like to run the following command?\n\nEnvironment: local\nReason: Read the current directory.\n\n$ pwd\n\n› 1. Yes, proceed (y)\n  2. Yes, and don't ask again for commands that start with `pwd` (p)\n  3. No, and tell Codex what to do differently (esc)\n\nPress enter to confirm or esc to cancel"

func cautiousRenderedFixture(t *testing.T, menu string) (*Aria, *Pane, <-chan directAutoFrameRead, *atomic.Int32) {
	t.Helper()
	a, p, frames, calls := cautiousAcceptanceFixture(t)
	appendDirectAutoMenuRedraw(t, p, menu)
	seedGridMenu(t, p, menu)
	noise := strings.Repeat("cell repaint fragment\n", 40)
	if _, err := p.buffer.Write([]byte(noise)); err != nil {
		t.Fatal(err)
	}
	atomic.AddInt64(&p.TotalStrippedChars, int64(len(noise)))
	if choicePromptRE.MatchString(tailOfPane(p, 30)) {
		t.Fatal("fixture still has a focused menu in the raw tail")
	}
	assertDirectAutoPromptReady(t, p, "rendered cautious permission")
	return a, p, frames, calls
}

func TestCautiousTeamProviderReceivesRenderedPermission(t *testing.T) {
	a, p, frames, _ := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
	completionTestVerdict(t, true)
	a.teamFeatures = func() bool { return true }
	a.settings = &ariaSettingsStore{}
	t.Setenv(ariaJevAutoEnv, "0")
	observed := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/aria/complete" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var body struct {
			Messages []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 || len(body.Messages[0].Content) != 1 {
			t.Error("invalid model request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		observed <- body.Messages[0].Content[0].Text
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	a.cloudBase = server.URL
	a.evaluateObservation(context.Background(), p.ID, p.Title, tailOfPane(p, 30))
	select {
	case text := <-observed:
		if !strings.Contains(text, "$ pwd") || !strings.Contains(text, "› 1. Yes, proceed") || strings.Contains(text, "cell repaint fragment") {
			t.Fatalf("provider did not receive the rendered permission: %s", text)
		}
	default:
		t.Fatal("team provider was not called")
	}
	expectNoFrame(t, frames, "text-only team decision")
}

func TestCautiousTeamToolConfirmsRenderedPermission(t *testing.T) {
	a, p, frames, _ := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
	_, pos, hash, _ := currentChoicePromptIdentity(p, "")
	events := make(chan map[string]any, 8)
	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		done <- a.toolSendInput(ctx, "rendered_team", p.ID, "1\n", pos, hash, true, func(event map[string]any) { events <- event })
	}()
	id := waitNeedsConfirm(t, events, "send_input")
	if !a.signalConfirm(id, false, false) {
		t.Fatal("could not cancel rendered confirmation")
	}
	if result := <-done; !strings.HasPrefix(result, "user rejected") {
		t.Fatalf("rendered confirmation result = %s", result)
	}
	if !a.cautiousPromptCancelled(p) {
		t.Fatal("rendered cancellation was not remembered")
	}
	expectNoFrame(t, frames, "cancelled rendered permission")
}

func TestCautiousTeamRenderedActionKindsReachConfirmation(t *testing.T) {
	for _, test := range []struct{ name, menu string }{
		{"create file", strings.ReplaceAll(claudeSettingsWriteScreen, ".claude/settings.local.json", "notes.json")},
		{"tool use", claudeMcpPermissionScreen},
		{"long edit", "Edit file\nnotes.txt\n" + strings.Repeat("  100 +safe text\n", 20) + "Do you want to make this edit?\n› 1. Yes\n  2. No (esc)"},
		{"grant permissions", codexGrantPermissionsScreen},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, p, frames, _ := cautiousAcceptanceFixture(t)
			p.resizeGridEmu(100, 64)
			seedGridMenu(t, p, test.menu)
			_, pos, hash, _ := currentChoicePromptIdentity(p, "")
			events := make(chan map[string]any, 8)
			done := make(chan string, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go func() {
				done <- a.toolSendInput(ctx, "rendered_action", p.ID, "1\n", pos, hash, true, func(event map[string]any) { events <- event })
			}()
			id := waitNeedsConfirm(t, events, "send_input")
			if !a.signalConfirm(id, false, false) {
				t.Fatal("could not cancel rendered action confirmation")
			}
			if result := <-done; !strings.HasPrefix(result, "user rejected") {
				t.Fatalf("rendered action result = %s", result)
			}
			expectNoFrame(t, frames, "cancelled rendered action")
		})
	}
}

func TestCautiousRenderedMenuStillRefusesUnsafePermission(t *testing.T) {
	for _, test := range []struct{ name, menu string }{
		{"remote mutation", strings.ReplaceAll(cautiousRenderedCodexMenu, "pwd", "git push origin main")},
		{"question", "Choose an output format\n❯ 1. Short\n  2. Detailed"},
		{"wrapped remote mutation", strings.ReplaceAll(cautiousRenderedCodexMenu, "$ pwd", "$ git pu\nsh origin main")},
		{"wrapped sensitive file", strings.ReplaceAll(cautiousRenderedCodexMenu, "$ pwd", "$ cat .en\nv")},
		{"clipped command", "Do you want to proceed?\n› 1. Yes\n  2. No (esc)"},
		{"stale transcript command", "$ pwd\n\nDo you want to proceed?\n› 1. Yes\n  2. No (esc)"},
		{"clipped file path", "  100 +safe-looking text\nDo you want to make this edit?\n› 1. Yes\n  2. No (esc)"},
		{"wrapped persistent grant", strings.ReplaceAll(cautiousRenderedCodexMenu, "› 1. Yes, proceed (y)", "› 1. Yes, and don't a\nsk again")},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, p, frames, _ := cautiousAcceptanceFixture(t)
			seedGridMenu(t, p, test.menu) // raw scrollback still contains the old safe permission
			_, pos, hash, _ := currentChoicePromptIdentity(p, "")
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			var events []map[string]any
			result := a.toolSendInput(ctx, "unsafe_rendered", p.ID, "1\n", pos, hash, true, func(event map[string]any) { events = append(events, event) })
			if !strings.Contains(result, "cautious auto-approval denied") || len(events) != 0 {
				t.Fatalf("unsafe current menu reached confirmation: result=%s events=%v", result, events)
			}
			expectNoFrame(t, frames, "unsafe rendered permission")
		})
	}
}

func TestCautiousJevRenderedPermissionSubmitsAfterCountdown(t *testing.T) {
	a, p, frames, calls := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
	_, done := startCautiousAcceptance(t, a, p)
	expectNoFrame(t, frames, "rendered permission cancel window")
	finishCautiousAcceptance(t, done)
	expectFrame(t, frames, "safe rendered permission", "\r")
	expectNoFrame(t, frames, "duplicate rendered permission")
	if calls.Load() != 1 {
		t.Fatalf("rendered permission provider calls = %d", calls.Load())
	}
}

func TestCautiousJevRenderedRiskNeverReachesProvider(t *testing.T) {
	for _, test := range []struct{ name, menu string }{
		{"remote mutation", strings.ReplaceAll(cautiousRenderedCodexMenu, "pwd", "git push origin main")},
		{"sensitive file", strings.ReplaceAll(cautiousRenderedCodexMenu, "pwd", "cat .env")},
		{"visible credential", "AKIAABCDEFGHIJKLMNOP\n" + cautiousRenderedCodexMenu},
		{"no action context", "Do you want to proceed?\n› 1. Yes\n  2. No (esc)"},
		{"wrapped remote mutation", strings.ReplaceAll(cautiousRenderedCodexMenu, "$ pwd", "$ git pu\nsh origin main")},
		{"wrapped sensitive file", strings.ReplaceAll(cautiousRenderedCodexMenu, "$ pwd", "$ cat .en\nv")},
		{"several command wraps", strings.ReplaceAll(cautiousRenderedCodexMenu, "$ pwd", "$ git\npu\nsh origin main")},
		{"wrapped credential", "AKIAABCD\nEFGHIJKL\nMNOP\n" + cautiousRenderedCodexMenu},
		{"wrapped credential next to words", "echo\nAKIAABCD\nEFGHIJKL\nMNOP\nmore output\n" + cautiousRenderedCodexMenu},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, p, frames, calls := cautiousRenderedFixture(t, test.menu)
			_, outcome := a.jevCautiousDecide(context.Background(), p, p.ID, p.Title, "", time.Now(), true)
			if calls.Load() != 0 || outcome == "" {
				t.Fatalf("unsafe rendered view reached provider: calls=%d outcome=%q", calls.Load(), outcome)
			}
			expectNoFrame(t, frames, "unsafe rendered view")
		})
	}
}

func TestCautiousRenderedSafeNamesDoNotMatchCommandDenyRules(t *testing.T) {
	for _, command := range []string{
		"cat dashboard/src/app.ts", "cat model/foo.txt", "rg 'srv.Shutdown(' daemon", "cat pseudo.txt",
	} {
		t.Run(command, func(t *testing.T) {
			menu := strings.ReplaceAll(cautiousRenderedCodexMenu, "$ pwd", "$ "+command)
			if reason := cautiousAutoDenyReason(menu, "1\n"); reason != "" {
				t.Fatalf("safe command denied: %s", reason)
			}
		})
	}
	separate := strings.Replace(cautiousRenderedCodexMenu, "Environment: local\nReason: Read the current directory.", "Reason: Read the current directory.\n\nEnvironment: local", 1)
	if reason := cautiousAutoDenyReason(separate, "1\n"); reason != "" {
		t.Fatalf("separate UI regions were joined into a deny phrase: %s", reason)
	}
}

func TestCautiousAmbiguousRenderedProofSubmitsAfterCountdown(t *testing.T) {
	a, p, frames := newJevDetectHarness(t, "cautious")
	noise := strings.Repeat("cell repaint fragment\n", 40)
	if _, err := p.buffer.Write([]byte(noise)); err != nil {
		t.Fatal(err)
	}
	atomic.AddInt64(&p.TotalStrippedChars, int64(len(noise)))
	if choicePromptRE.MatchString(tailOfPane(p, 30)) {
		t.Fatal("fixture still has a raw permission")
	}
	var calls atomic.Int32
	a.jevClientFactory = func(string) jevSystemOneClient {
		return ariaJevSafetyClient{call: func(context.Context, string, any, map[string]typesafe.Question) (*typesafe.Response, error) {
			calls.Add(1)
			return jevDetectYesResponse(true), nil
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	done := make(chan bool, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- a.tryJevAmbiguousTerminal(ctx, p, p.ID, p.Title, time.Now())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("rendered pilot evaluator did not stop")
		}
	})
	deadline := time.After(2 * time.Second)
	for {
		a.mu.Lock()
		pending := len(a.confirms) > 0
		a.mu.Unlock()
		if pending {
			break
		}
		select {
		case <-done:
			t.Fatal("rendered pilot did not open a confirmation")
		case <-deadline:
			t.Fatal("rendered pilot confirmation timed out")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	expectNoFrame(t, frames, "rendered pilot cancel window")
	select {
	case handled := <-done:
		if !handled {
			t.Fatal("rendered pilot fell through")
		}
	case <-ctx.Done():
		t.Fatal("rendered pilot did not finish after countdown")
	}
	expectFrame(t, frames, "rendered pilot locked Enter", "\r")
	expectNoFrame(t, frames, "duplicate rendered pilot Enter")
	if calls.Load() != 1 {
		t.Fatalf("rendered pilot provider calls = %d", calls.Load())
	}
}

func TestCautiousRenderedMCPAndNetworkContext(t *testing.T) {
	for _, menu := range []string{codexTwoOptionMCP, jevNarrowMCPPermission, codexNetworkApprovalScreen} {
		if !cautiousRenderedHasActionContext(menu) {
			t.Fatalf("visible MCP/network action treated as clipped: %s", menu)
		}
	}
}

func TestCautiousAmbiguousProofReadsTheRenderedPermission(t *testing.T) {
	_, p, _, _ := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
	text := ariaJevPermissionTail(p, false)
	if !strings.Contains(text, "$ pwd") || strings.Contains(text, "cell repaint fragment") {
		t.Fatal("cautious proof is not reading the rendered permission")
	}
}

func TestCautiousJevSafeWrappedCommandStillReachesProvider(t *testing.T) {
	menu := strings.ReplaceAll(cautiousRenderedCodexMenu, "$ pwd", "$ git diff --stat\n-- daemon/aria_auto.go")
	a, p, frames, calls := cautiousRenderedFixture(t, menu)
	a.jevClientFactory = func(string) jevSystemOneClient {
		return ariaJevSafetyClient{call: func(context.Context, string, any, map[string]typesafe.Question) (*typesafe.Response, error) {
			calls.Add(1)
			return nil, context.DeadlineExceeded // eligibility only, no confirmation
		}}
	}
	a.jevCautiousDecide(context.Background(), p, p.ID, p.Title, "", time.Now(), true)
	if calls.Load() != 1 {
		t.Fatalf("safe wrapped command provider calls = %d", calls.Load())
	}
	expectNoFrame(t, frames, "provider failure on safe wrapped command")
}

func TestCautiousJevRenderedChangeDuringProviderDoesNotWrite(t *testing.T) {
	a, p, frames, calls := cautiousRenderedFixture(t, cautiousRenderedCodexMenu)
	a.jevClientFactory = func(string) jevSystemOneClient {
		return ariaJevSafetyClient{call: func(context.Context, string, any, map[string]typesafe.Question) (*typesafe.Response, error) {
			calls.Add(1)
			seedGridMenu(t, p, strings.ReplaceAll(cautiousRenderedCodexMenu, "pwd", "git push origin main"))
			return ariaJevSafeResponse(), nil
		}}
	}
	handled, _ := a.jevCautiousDecide(context.Background(), p, p.ID, p.Title, "", time.Now(), true)
	if !handled || calls.Load() != 1 {
		t.Fatalf("changed rendered permission: handled=%v calls=%d", handled, calls.Load())
	}
	expectNoFrame(t, frames, "rendered action changed while provider was answering")
}
