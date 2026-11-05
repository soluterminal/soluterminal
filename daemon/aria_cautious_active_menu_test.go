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

const cautiousActivePWDMenu = "Would you like to run the following command?\n\nEnvironment: local\nReason: Read the current directory.\n\n$ pwd\n\n› 1. Yes, proceed (y)\n  2. Yes, and don't ask again for commands that start with `pwd` (p)\n  3. No, and tell Codex what to do differently (esc)\n\nPress enter to confirm or esc to cancel"

// Codex renders its pending execution summary outside the two-column picker.
// Raw repaint fixtures deliberately keep their original modal-only input.
func cautiousVisibleDialog(menu string, codex bool) string {
	if codex {
		return "• Running pwd\n\n" + menu
	}
	return menu
}

func TestCautiousCurrentPermissionBaselineAutoFires(t *testing.T) {
	for _, codex := range []bool{true, false} {
		menu, name := cautiousActivePWDMenu, "codex"
		if !codex {
			menu, name = cautiousActiveClaudePWDMenu, "claude"
		}
		t.Run(name, func(t *testing.T) {
			a, p, frames, calls := cautiousAcceptanceFixture(t)
			p.forceCodexForTest = codex
			if !codex {
				p.Agent, p.ClaudeSessionID = "claude", "cautious-active-claude-fixture"
			}
			appendDirectAutoMenuRedraw(t, p, menu)
			p.resizeGridEmu(100, 64)
			seedGridMenu(t, p, "• Earlier curl http://127.0.0.1:8787/api/health\n\n"+cautiousVisibleDialog(menu, codex))
			_, done := startCautiousAcceptance(t, a, p)
			expectNoFrame(t, frames, "baseline current-dialog cancel window")
			finishCautiousAcceptance(t, done)
			expectFrame(t, frames, "baseline current-dialog Enter", "\r")
			expectNoFrame(t, frames, "duplicate baseline Enter")
			if calls.Load() != 1 {
				t.Fatalf("baseline provider calls = %d", calls.Load())
			}
		})
	}
}

const cautiousActiveClaudePWDMenu = "────────────────────────────────────────\nBash command\n\n  pwd\n\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again for pwd commands\n  3. No, and tell Claude what to do differently (esc)\n\nEsc to cancel · Tab to amend"

// Kept compatible with v1220 so the identical screen/raw fixture can compare
// its raw-tail gate against the rendered-screen gate introduced in #616.
func TestCautiousCurrentPermissionIgnoresEarlierOutput(t *testing.T) {
	assertCautiousCurrentPermissionIgnoresEarlierOutput(t, cautiousActivePWDMenu, true)
}

func TestCautiousClaudeCurrentPermissionIgnoresEarlierOutput(t *testing.T) {
	t.Run("with separator", func(t *testing.T) {
		assertCautiousCurrentPermissionIgnoresEarlierOutput(t, cautiousActiveClaudePWDMenu, false)
	})
	t.Run("without separator", func(t *testing.T) {
		assertCautiousCurrentPermissionIgnoresEarlierOutput(t, strings.SplitN(cautiousActiveClaudePWDMenu, "\n", 2)[1], false)
	})
}

func assertCautiousCurrentPermissionIgnoresEarlierOutput(t *testing.T, menu string, codex bool) {
	t.Helper()
	for _, history := range []string{
		"• Ran curl http://127.0.0.1:8787/api/health\n  Previous health response was OK.",
		"• Earlier discussion mentioned .env files.",
		"• Earlier discussion mentioned git push origin main.",
	} {
		t.Run(history, func(t *testing.T) {
			a, p, frames, _ := cautiousAcceptanceFixture(t)
			p.forceCodexForTest = codex
			if !codex {
				p.Agent = "claude"
				p.ClaudeSessionID = "cautious-active-claude-fixture"
			}
			appendDirectAutoMenuRedraw(t, p, menu)
			p.resizeGridEmu(100, 64)
			seedGridMenu(t, p, history+"\n\n"+cautiousVisibleDialog(menu, codex))
			_, pos, hash, ok := currentChoicePromptIdentity(p, "")
			if !ok || hash == 0 {
				t.Fatal("fixture lacks a current permission identity")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			done := make(chan string, 1)
			finished := make(chan struct{})
			confirm := make(chan string, 1)
			go func() {
				defer close(finished)
				done <- a.toolSendInput(ctx, "active_menu", p.ID, "1\n", pos, hash, true, func(event map[string]any) {
					if event["type"] == "needs_confirm" {
						confirm <- event["id"].(string)
					}
				})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("active-menu confirmation did not stop")
				}
			})
			select {
			case id := <-confirm:
				if !a.signalConfirm(id, false, false) {
					t.Fatal("could not cancel the safe confirmation")
				}
			case result := <-done:
				t.Fatalf("unrelated history blocked the current pwd permission: %s", result)
			case <-ctx.Done():
				t.Fatal("safe current permission did not reach confirmation")
			}
			if result := <-done; !strings.HasPrefix(result, "user rejected") {
				t.Fatalf("confirmation result = %s", result)
			}
			expectNoFrame(t, frames, "cancelled current permission")
		})
	}
}

func TestCautiousProvidersReceiveCurrentDialogWithoutEarlierOutput(t *testing.T) {
	for _, test := range []struct {
		name, menu  string
		team, codex bool
	}{
		{"jev codex", cautiousActivePWDMenu, false, true},
		{"team codex", cautiousActivePWDMenu, true, true},
		{"jev claude", cautiousActiveClaudePWDMenu, false, false},
		{"team claude", cautiousActiveClaudePWDMenu, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			history := "• Ran curl http://127.0.0.1:8787/api/health\nprevious-result-marker\n\n"
			a, p, frames, _ := cautiousRenderedFixture(t, history+cautiousVisibleDialog(test.menu, test.codex))
			if !test.codex {
				p.forceCodexForTest = false
				p.Agent, p.ClaudeSessionID = "claude", "cautious-active-claude-fixture"
			}
			observed := make(chan string, 1)
			if test.team {
				completionTestVerdict(t, true)
				a.teamFeatures = func() bool { return true }
				a.settings = &ariaSettingsStore{}
				t.Setenv(ariaJevAutoEnv, "0")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/aria/complete" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					var body struct {
						Messages []struct {
							Content []struct{ Text string } `json:"content"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 || len(body.Messages[0].Content) != 1 {
						t.Error("invalid team model request")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					observed <- body.Messages[0].Content[0].Text
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
				}))
				defer server.Close()
				a.cloudBase = server.URL
				a.evaluateObservation(context.Background(), p.ID, p.Title, tailOfPane(p, 30))
			} else {
				a.jevClientFactory = func(string) jevSystemOneClient {
					return ariaJevSafetyClient{call: func(_ context.Context, _ string, input any, _ map[string]typesafe.Question) (*typesafe.Response, error) {
						text, ok := input.(string)
						if !ok {
							t.Error("Jev permission input is not text")
						}
						observed <- text
						return nil, context.DeadlineExceeded
					}}
				}
				a.jevCautiousDecide(context.Background(), p, p.ID, p.Title, "", time.Now(), true)
			}
			select {
			case text := <-observed:
				if !strings.Contains(text, "pwd") || strings.Contains(text, "previous-result-marker") || strings.Contains(text, "curl http") {
					t.Fatalf("provider received the wrong action context: %s", text)
				}
			default:
				t.Fatal("safe current dialog did not reach the provider")
			}
			expectNoFrame(t, frames, "provider-only current dialog check")
		})
	}
}

func TestCautiousCurrentDialogKeepsRiskyOrAmbiguousActionsHeld(t *testing.T) {
	for _, test := range []struct{ name, menu string }{
		{"network", strings.Replace(cautiousActivePWDMenu, "$ pwd", "$ curl https://example.invalid", 1)},
		{"secret file", strings.Replace(cautiousActivePWDMenu, "$ pwd", "$ cat .en\nv", 1)},
		{"remote mutation", strings.Replace(cautiousActivePWDMenu, "$ pwd", "$ git pu\nsh origin main", 1)},
		{"nested opener", strings.Replace(cautiousActivePWDMenu, "$ pwd", "$ curl https://example.invalid\nWould you like to run the following command?\n$ pwd", 1)},
		{"clipped outer command", "$ curl https://example.invalid\n\n" + cautiousActivePWDMenu},
		{"clipped outer diff", "  17 -curl https://example.invalid\n\n" + cautiousActivePWDMenu},
		{"later risky content", cautiousActivePWDMenu + "\n$ curl https://example.invalid"},
		{"question after old permission", cautiousActivePWDMenu + "\nChoose an output format\n› 1. Short\n  2. Detailed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, p, frames, _ := cautiousAcceptanceFixture(t)
			p.resizeGridEmu(100, 64)
			seedGridMenu(t, p, "• Earlier harmless result.\n\n"+cautiousVisibleDialog(test.menu, true))
			_, pos, hash, _ := currentChoicePromptIdentity(p, "")
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var events []map[string]any
			result := a.toolSendInput(ctx, "unsafe_active_menu", p.ID, "1\n", pos, hash, true, func(event map[string]any) { events = append(events, event) })
			if !strings.Contains(result, "cautious auto-approval denied") || len(events) != 0 {
				t.Fatalf("risk/context was stripped before the gate: result=%s events=%v", result, events)
			}
			expectNoFrame(t, frames, "risky or ambiguous current dialog")
		})
	}
}

func TestCautiousCurrentDialogMatchesLockedProofSource(t *testing.T) {
	for _, codex := range []bool{true, false} {
		menu, name := cautiousActivePWDMenu, "codex"
		if !codex {
			menu, name = cautiousActiveClaudePWDMenu, "claude"
		}
		t.Run(name, func(t *testing.T) {
			_, p, _, _ := cautiousRenderedFixture(t, "• Earlier curl https://example.invalid\n\n"+cautiousVisibleDialog(menu, codex))
			if !codex {
				p.forceCodexForTest = false
				p.Agent, p.ClaudeSessionID = "claude", "cautious-active-claude-fixture"
			}
			observed := ariaJevPermissionTail(p, false)
			p.subsMu.RLock()
			locked := ariaJevLockedPermissionTail(p, &jevAmbiguousProof{mode: "cautious"}, codex)
			p.subsMu.RUnlock()
			if observed != locked || strings.Contains(locked, "Earlier curl") || !strings.Contains(locked, "pwd") {
				t.Fatal("cautious observation and locked proof use different dialog boundaries")
			}
		})
	}
}

func TestCautiousQuotedCommandOpenerDoesNotReplaceAnotherDialog(t *testing.T) {
	quoted := "Would you like to run the following command?\n\n$ pwd\n"
	for _, text := range []string{
		"Would you like to grant these permissions?\nwrite /etc/config\nReason:\n" + quoted + "› 1. Yes, grant these permissions for this turn (y)\n  2. No (esc)",
		"Would you like to grant these permissions?\nwrite /etc/config\nReason:\n" + quoted + "› 1. Yes, proceed (y)\n  2. No (esc)",
		"Allow the example MCP server to run tool \"write_file\"?\npath: /etc/config\ncontent:\n" + quoted + "› 1. Allow\n  2. Cancel\nenter to submit | esc to cancel",
		"Tool: write_file\n/etc/config\narguments:\n" + quoted + "› 1. Yes, proceed (y)\n  2. No (esc)",
	} {
		if got := cautiousCurrentDialogText(text, true); got != text {
			t.Fatalf("a quoted opener hid the real dialog context: %s", got)
		}
		if reason := cautiousAutoDenyReason(cautiousCurrentDialogText(text, true), "1\n"); reason == "" {
			t.Fatal("foreign dialog with a quoted safe command bypassed the cautious gate")
		}
	}
}

func TestCautiousQuotedClaudeHeadingDoesNotReplaceAnotherDialog(t *testing.T) {
	quoted := "Bash command\n  pwd\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No (esc)"
	for _, prefix := range []string{
		"Would you like to grant these permissions?\nwrite /etc/config\nReason:\n",
		"Allow the example MCP server to run tool \"write_file\"?\npath: /etc/config\ncontent:\n",
		"Tool: write_file\n/etc/config\narguments:\n",
	} {
		text := prefix + quoted
		if got := cautiousCurrentDialogText(text, false); got != text {
			t.Fatalf("quoted Claude heading hid another dialog: %s", got)
		}
		if reason := cautiousAutoDenyReason(cautiousCurrentDialogText(text, false), "1\n"); reason == "" {
			t.Fatal("foreign dialog with a quoted Claude heading bypassed the gate")
		}
	}
}

func TestCautiousJevCurrentDialogSubmitsAfterCountdown(t *testing.T) {
	for _, codex := range []bool{true, false} {
		menu, name := cautiousActivePWDMenu, "codex"
		if !codex {
			menu, name = cautiousActiveClaudePWDMenu, "claude"
		}
		t.Run(name, func(t *testing.T) {
			a, p, frames, calls := cautiousRenderedFixture(t, "• Earlier curl https://example.invalid\n\n"+cautiousVisibleDialog(menu, codex))
			if !codex {
				p.forceCodexForTest = false
				p.Agent, p.ClaudeSessionID = "claude", "cautious-active-claude-fixture"
			}
			_, done := startCautiousAcceptance(t, a, p)
			expectNoFrame(t, frames, "current-dialog cancel window")
			finishCautiousAcceptance(t, done)
			expectFrame(t, frames, "safe current-dialog Enter", "\r")
			expectNoFrame(t, frames, "duplicate current-dialog Enter")
			if calls.Load() != 1 {
				t.Fatalf("current-dialog provider calls = %d", calls.Load())
			}
		})
	}
}

func TestCautiousClaudePermissionKindsKeepCurrentContext(t *testing.T) {
	const choices = "\n❯ 1. Yes\n  2. Yes, and don't ask again for this action\n  3. No (esc)\n\nEsc to cancel"
	for _, menu := range []string{
		"Read file\n  notes.txt\nDo you want to proceed?" + choices,
		"Edit file\n  notes.txt\n  1 -old text\n  1 +new text\nDo you want to make this edit?" + choices,
		"Write file\n  notes.txt\n  new text\nDo you want to proceed?" + choices,
		"Create file\n  notes.txt\n  1 +new text\nDo you want to create notes.txt?" + choices,
		"Tool use\n  soluterminal - list_panes() (MCP)\nDo you want to proceed?" + choices,
	} {
		t.Run(strings.SplitN(menu, "\n", 2)[0], func(t *testing.T) {
			assertCautiousCurrentPermissionIgnoresEarlierOutput(t, menu, false)
		})
	}
}

func TestCautiousClaudeCurrentDialogKeepsRiskyAndQuotedPayloadsHeld(t *testing.T) {
	for _, test := range []struct{ name, menu string }{
		{"network", strings.Replace(cautiousActiveClaudePWDMenu, "  pwd", "  curl https://example.invalid", 1)},
		{"remote mutation", strings.Replace(cautiousActiveClaudePWDMenu, "  pwd", "  git pu\n  sh origin main", 1)},
		{"secret file", strings.Replace(cautiousActiveClaudePWDMenu, "  pwd", "  cat .en\n  v", 1)},
		{"nested Claude heading", strings.Replace(cautiousActiveClaudePWDMenu, "  pwd", "  curl https://example.invalid\n   Bash command\n     pwd", 1)},
		{"nested Codex opener", strings.Replace(cautiousActiveClaudePWDMenu, "  pwd", "  curl https://example.invalid\nWould you like to run the following command?\n$ pwd", 1)},
		{"clipped outer heading", "  curl https://example.invalid\n   Bash command\n     pwd\nDo you want to proceed?\n❯ 1. Yes\n  2. No (esc)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, p, frames, _ := cautiousAcceptanceFixture(t)
			p.forceCodexForTest = false
			p.Agent, p.ClaudeSessionID = "claude", "cautious-active-claude-fixture"
			p.resizeGridEmu(100, 64)
			seedGridMenu(t, p, "● Earlier harmless result.\n\n"+test.menu)
			_, pos, hash, _ := currentChoicePromptIdentity(p, "")
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var events []map[string]any
			result := a.toolSendInput(ctx, "unsafe_claude_dialog", p.ID, "1\n", pos, hash, true, func(event map[string]any) { events = append(events, event) })
			if !strings.Contains(result, "cautious auto-approval denied") || len(events) != 0 {
				t.Fatalf("Claude payload was truncated or reclassified: result=%s events=%v", result, events)
			}
			expectNoFrame(t, frames, "risky Claude current action")
		})
	}
}

func TestCautiousCurrentDialogDoesNotSpendHistoryByteBudget(t *testing.T) {
	a, p, frames, _ := cautiousAcceptanceFixture(t)
	p.resizeGridEmu(100, 128)
	history := strings.Repeat("Earlier benign output remains above this permission; it is not the current action.\n", 100)
	seedGridMenu(t, p, history+"\n"+cautiousVisibleDialog(cautiousActivePWDMenu, true))
	var calls atomic.Int32
	a.jevClientFactory = func(string) jevSystemOneClient {
		return ariaJevSafetyClient{call: func(_ context.Context, _ string, input any, _ map[string]typesafe.Question) (*typesafe.Response, error) {
			calls.Add(1)
			if text := input.(string); len(text) > ariaJevMaxTailBytes || strings.Contains(text, "Earlier benign output") {
				t.Error("irrelevant history consumes the current-dialog budget")
			}
			return nil, context.DeadlineExceeded
		}}
	}
	a.jevCautiousDecide(context.Background(), p, p.ID, p.Title, "", time.Now(), true)
	if calls.Load() != 1 {
		t.Fatal("a complete safe dialog was refused because of earlier history length")
	}
	expectNoFrame(t, frames, "provider failure after bounded dialog")
}

func TestCautiousClippedCodexContinuationCannotReplacePendingAction(t *testing.T) {
	for _, prefix := range []string{
		"  curl https://example.invalid\n",
		"  curl https://example.invalid\n  • Running pwd\n\n",
		"• Running curl https://example.invalid\n\n",
		"• Running printf hello\n  │ curl https://example.invalid\n\n",
	} {
		t.Run(prefix, func(t *testing.T) {
			text := prefix + cautiousActivePWDMenu
			got := cautiousCurrentDialogText(text, true)
			if !strings.Contains(got, "curl https://example.invalid") {
				t.Fatal("quoted opener hid the pending command continuation")
			}
			a, p, frames, calls := cautiousRenderedFixture(t, text)
			a.evaluateObservation(context.Background(), p.ID, p.Title, tailOfPane(p, 30))
			if calls.Load() != 0 {
				t.Fatal("unsafe pending action reached the evaluator")
			}
			expectNoFrame(t, frames, "clipped command with quoted opener")
		})
	}
	text := "• Running pwd\n\n" + strings.Replace(cautiousActivePWDMenu, "Would you like", "    Would you like", 1)
	if cautiousCurrentDialogText(text, true) != text {
		t.Fatal("payload-indented opener was treated as a new dialog")
	}
}

func TestCautiousElidedCodexActionStaysWithHuman(t *testing.T) {
	menu := strings.Replace(cautiousActivePWDMenu, "$ pwd", "$ pwd\n[… 90 lines] ctrl+shift+a view all", 1)
	a, p, frames, calls := cautiousRenderedFixture(t, menu)
	if cautiousRenderedHasActionContext(menu) {
		t.Fatal("elided command was accepted as complete context")
	}
	if acted, outcome := a.jevCautiousDecide(context.Background(), p, p.ID, p.Title, "", time.Now(), true); acted || outcome != "precheck_fallback" {
		t.Fatalf("elided command reached Jev: acted=%v outcome=%s", acted, outcome)
	}
	if calls.Load() != 0 {
		t.Fatal("incomplete command reached Jev")
	}
	_, pos, hash, _ := currentChoicePromptIdentity(p, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := a.toolSendInput(ctx, "elided_dialog", p.ID, "1\n", pos, hash, true, nil)
	if !strings.Contains(result, "cautious auto-approval denied") {
		t.Fatalf("elided action reached input confirmation: %s", result)
	}
	expectNoFrame(t, frames, "elided command")
}

func TestCautiousCodexPickerMarginKeepsPendingSummary(t *testing.T) {
	menu := "  " + strings.ReplaceAll(cautiousActivePWDMenu, "\n", "\n  ")
	current := "• Running pwd\n\n" + menu
	text := "Earlier curl https://example.invalid\n\n" + current
	if got := cautiousCurrentDialogText(text, true); got != current {
		t.Fatalf("picker margin did not preserve the pending action: %q", got)
	}
}
