package main

import (
	"strings"
	"testing"
)

// Full-screen fixtures for the terminal auto-approval classifier. Option
// labels and titles come from the installed Claude Code 2.1.283 and codex-cli
// 0.157.1 binaries (strings extracted 2026-09-28); layout follows the live
// captures elsewhere in this package (state_test.go, muse/pi/deepseek tests).

func fixtureScreen(lines ...string) string { return strings.Join(lines, "\n") }

// ---- must stay auto-approvable ----

var claudeBashPermissionScreen = fixtureScreen(
	"● Pushing the branch now.",
	"",
	"────────────────────────────────────────────────────────────────────────",
	" Bash command",
	"",
	"   git push -u origin fix/auto-approve-deny-plan-question-billing",
	"   Push the fix branch",
	"",
	" Do you want to proceed?",
	" ❯ 1. Yes",
	"   2. Yes, and don't ask again for git push commands in",
	"   /home/user/wsl-projects/soluterminal",
	"   3. No, and tell Claude what to do differently (esc)",
	"",
	" Esc to cancel · Tab to amend · ctrl+e to explain",
)

var claudeEditPermissionScreen = fixtureScreen(
	" Edit file",
	" daemon/state.go",
	"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
	"  643 -func isAutoApprovalPromptText(s string, hasCodex bool) bool {",
	"  643 +func isAutoApprovalPromptText(s string, hasCodex bool) bool { // v2",
	"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
	" Do you want to make this edit to state.go?",
	" ❯ 1. Yes",
	"   2. Yes, allow all edits during this session (shift+tab)",
	"   3. No, and tell Claude what to do differently (esc)",
	"",
	" Esc to cancel · Tab to amend",
)

var claudeMcpPermissionScreen = fixtureScreen(
	" Tool use",
	"",
	"   soluterminal - list_panes() (MCP)",
	"   List all terminal panes.",
	"",
	" Do you want to proceed?",
	" ❯ 1. Yes",
	"   2. Yes, and don't ask again for soluterminal - list_panes commands in",
	"   /home/user/wsl-projects/soluterminal",
	"   3. No, and tell Claude what to do differently (esc)",
	"",
	" Esc to cancel · Tab to amend",
)

var claudeWebFetchPermissionScreen = fixtureScreen(
	" Fetch",
	"",
	"   https://pkg.go.dev/regexp/syntax",
	"   Claude wants to fetch content from pkg.go.dev",
	"",
	" Do you want to allow Claude to fetch this content?",
	" ❯ 1. Yes",
	"   2. Yes, and don't ask again for pkg.go.dev",
	"   3. No, and tell Claude what to do differently (esc)",
	"",
	" Esc to cancel",
)

var claudeSettingsWriteScreen = fixtureScreen(
	" Create file",
	" .claude/settings.local.json",
	"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
	"   1 {",
	"   2   \"permissions\": { \"allow\": [\"Bash(go test:*)\"] }",
	"   3 }",
	"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
	" Do you want to create settings.local.json?",
	" ❯ 1. Yes",
	"   2. Yes, and allow Claude to edit its own settings for this session",
	"   3. No, and tell Claude what to do differently (esc)",
	"",
	" Esc to cancel · Tab to amend",
)

var codexCommandApprovalScreen = fixtureScreen(
	"• Running the daemon tests.",
	"",
	"  Would you like to run the following command?",
	"",
	"  Reason: Run the daemon tests outside the sandbox",
	"",
	"  $ go test -C daemon ./...",
	"",
	"› 1. Yes, proceed (y)",
	"  2. Yes, and don't ask again for commands that start with `go test` (p)",
	"  3. No, and tell Codex what to do differently (esc)",
	"",
	"  Press enter to confirm or esc to cancel",
)

var codexPatchApprovalScreen = fixtureScreen(
	"  Would you like to make the following edits?",
	"",
	"  Reason: Tighten the approval classifier",
	"",
	"  daemon/state.go (+3 -1)",
	"    12 -old line",
	"    12 +new line",
	"",
	"› 1. Yes, proceed (y)",
	"  2. Yes, and don't ask again for these files (a)",
	"  3. No, and tell Codex what to do differently (esc)",
	"",
	"  Press enter to confirm or esc to cancel",
)

var codexNetworkApprovalScreen = fixtureScreen(
	"  Do you want to approve network access to \"registry.npmjs.org\"?",
	"",
	"  $ npm install",
	"",
	"› 1. Yes, just this once (y)",
	"  2. Yes, and allow this host for this conversation (a)",
	"  3. Yes, and allow this host in the future (p)",
	"  4. No, and block this host in the future (d)",
	"  5. No, and tell Codex what to do differently (esc)",
	"",
	"  Press enter to confirm or esc to cancel",
)

var codexGrantPermissionsScreen = fixtureScreen(
	"  Would you like to grant these permissions?",
	"",
	"  Reason: Write the build cache outside the workspace",
	"  write /tmp/build-cache",
	"",
	"› 1. Yes, grant these permissions for this turn (y)",
	"  2. Yes, grant for this turn with strict auto review",
	"  3. Yes, grant these permissions for this session (a)",
	"  4. No, continue without running it (n)",
	"  5. No, and tell Codex what to do differently (esc)",
	"",
	"  Press enter to confirm or esc to cancel",
)

var codexSendInputScreen = fixtureScreen(
	"  Would you like to send input to the existing terminal?",
	"",
	"  $ python3 manage.py migrate",
	"  input: yes",
	"",
	"› 1. Yes, proceed (y)",
	"  2. No, and tell Codex what to do differently (esc)",
	"",
	"  Press enter to confirm or esc to cancel",
)

// Codex MCP elicitation info request. Option labels are verbatim from the
// codex 0.157 binary; the header wording around them is approximated.
var codexMcpInfoRequestScreen = fixtureScreen(
	"  soluterminal needs your approval.",
	"",
	"  The MCP server requests: the active workspace path",
	"",
	"› 1. Yes, provide the requested info",
	"  2. No, but continue without it",
	"  3. Cancel this request",
	"",
	"  Press enter to confirm or esc to cancel",
)

// ---- must never be auto-approved ----

var claudePlanExitScreen = fixtureScreen(
	"╭─────────────────────────────────────────────────────────────────╮",
	" Ready to code?",
	"",
	" Here is Claude's plan:",
	"╭─────────────────────────────────────────────────────────────────╮",
	"│ # Fix auto-approve                                              │",
	"│ 1. Add a deny classifier                                        │",
	"│ 2. Require an accept-once focused row                           │",
	"╰─────────────────────────────────────────────────────────────────╯",
	"───────────────────────────────────────────────────────────────────",
	" Claude has written up a plan and is ready to execute. Would you like to proceed?",
	"",
	" ❯ 1. Yes, clear context (38% used) and auto-accept edits",
	"   2. Yes, auto-accept edits",
	"   3. Yes, manually approve edits",
	"   4. No, keep planning",
	"   5. Tell Claude what to change",
	"",
	" shift+tab to approve with this feedback",
)

var claudePlanExitAutoModeScreen = fixtureScreen(
	" Claude has written up a plan and is ready to execute. Would you like to proceed?",
	"",
	" ❯ 1. Yes, and use auto mode",
	"   2. Yes, manually approve edits",
	"   3. No, keep planning",
	"   4. Tell Claude what to change",
)

// Narrow pane: row 1 wraps and the plan header/body scrolled off; only the
// option rows remain.
var claudePlanExitNarrowScreen = fixtureScreen(
	" ❯ 1. Yes, clear context (71% used) and",
	"      use auto mode",
	"   2. Yes, manually approve",
	"      edits",
	"   3. No, keep planning",
)

var claudeExitPlanModeYesNoScreen = fixtureScreen(
	"╭─ Exit plan mode? ─────────────────────────╮",
	" Claude wants to exit plan mode",
	"",
	" ❯ 1. Yes",
	"   2. No",
)

var claudeEnterPlanModeScreen = fixtureScreen(
	" Enter plan mode?",
	" Claude wants to enter plan mode to explore and design an implementation approach.",
	" No code changes will be made until you approve the plan.",
	"",
	" ❯ 1. Yes, enter plan mode",
	"   2. No, start implementing now",
)

var codexImplementPlanScreen = fixtureScreen(
	"• Proposed plan ready.",
	"",
	"  Implement this plan?",
	"",
	"› 1. Yes, implement this plan  Switch to Default and start coding.",
	"  2. No, stay in Plan mode     Continue planning with the model.",
	"",
	"  Press enter to confirm or esc to go back",
)

var codexClearContextImplementScreen = fixtureScreen(
	"  Implement this plan?",
	"",
	"› 1. Yes, clear context and implement",
	"  2. Yes, implement this plan",
	"  3. No, stay in Plan mode",
)

var codexModelQuestionScreen = fixtureScreen(
	"  Question 1/1",
	"  Should the migration also backfill old rows?",
	"",
	"› 1. Yes, backfill everything  Slower deploy but consistent data.",
	"  2. No, only new rows          Faster; old rows stay null.",
	"  3. None of the above          Optionally, add details in notes (tab).",
	"",
	"  tab to add notes | enter to submit answer | esc to interrupt",
)

// Same question with the footer and "None of the above" cropped away: only
// the focused-row check stands between Enter and a model-authored answer.
var codexModelQuestionCroppedScreen = fixtureScreen(
	"  Should the migration also backfill old rows?",
	"",
	"› 1. Yes, backfill everything",
	"  2. No, only new rows",
)

var claudeUseResetScreen = fixtureScreen(
	" Use your reset?",
	" Refills your session and weekly limits now · your weekly reset day stays Monday",
	" 2 left · use by Oct 31",
	"",
	" ❯ 1. Yes, use my reset",
	"   2. No, keep it",
)

var claudeReenableCreditsScreen = fixtureScreen(
	" You've reached your Fable limit",
	" You've used your included Fable usage for this week. Continuing on Fable uses usage credits — you have $0.00 in credits.",
	" Usage credits are turned off. Re-enable to use Fable.",
	"",
	" ❯ 1. Yes, re-enable and continue",
	"   2. Upgrade to Max",
	"   3. No, wait for my limits to reset",
)

var claudeBuyCreditsScreen = fixtureScreen(
	" Fable runs on usage credits, purchased separately from your plan.",
	" You don't have usage credits yet.",
	"",
	" ❯ 1. Yes, buy usage credits",
	"   2. No, cancel",
)

var claudeSwitchModelScreen = fixtureScreen(
	" You've reached your Fable limit",
	"",
	" ❯ 1. Switch to Opus 4.8 and continue",
	"   2. No, keep my current model",
)

var codexUseResetScreen = fixtureScreen(
	"  Use this reset?",
	"",
	"› 1. Yes, use this reset",
	"  2. No, go back",
)

// AskUserQuestion in a narrow pane: the "Enter to select" footer wraps
// across lines, and the model authored "Yes, …" labels.
var claudeAskUserQuestionWrappedScreen = fixtureScreen(
	" ☐ Rollout",
	"",
	" Ship the fix behind a flag?",
	"",
	" ❯ 1. Yes, behind a flag",
	"      Safer; default off.",
	"   2. No, ship directly",
	"      Faster.",
	"   3. Type something.",
	" ──────────",
	"   4. Chat about this",
	"",
	" Enter to",
	" select · ↑/↓ to",
	" navigate · Esc to",
	" cancel",
)

// Worst case: model-authored "Yes, proceed" (an accept-once label) with the
// free-text rows cropped; only the wrapped footer identifies the question.
var claudeAskUserQuestionFooterOnlyScreen = fixtureScreen(
	" Do you want to proceed with the migration?",
	"",
	" ❯ 1. Yes, proceed",
	"   2. No, wait",
	"",
	// Every wrapped footer line is short enough for trimTrailingStatusJunk
	// to drop, so the deny pass must read the untrimmed screen.
	" Enter to",
	" select ·",
	" Esc to",
	" cancel",
)

func TestAutoApprovalClassifierFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		hasCodex bool
		want     bool
	}{
		{"claude bash permission", claudeBashPermissionScreen, false, true},
		{"claude edit permission", claudeEditPermissionScreen, false, true},
		{"claude mcp permission", claudeMcpPermissionScreen, false, true},
		{"claude webfetch permission", claudeWebFetchPermissionScreen, false, true},
		{"claude settings-write permission", claudeSettingsWriteScreen, false, true},
		{"claude bash permission on codex-descendant pane", claudeBashPermissionScreen, true, true},
		{"codex command approval", codexCommandApprovalScreen, true, true},
		{"codex patch approval", codexPatchApprovalScreen, true, true},
		{"codex network approval", codexNetworkApprovalScreen, true, true},
		{"codex grant-permissions approval", codexGrantPermissionsScreen, true, true},
		{"codex send-input approval", codexSendInputScreen, true, true},
		{"codex mcp permission modal", codexMcpPermissionObservedPrompt, true, true},
		{"codex mcp info request", codexMcpInfoRequestScreen, true, true},
		{"muse network approval", museNetworkMenuFixture, true, true},
		{"pi approval", piApprovalMenuFixture, false, true},
		{"deepseek approval", deepseekApprovalMenuFixtureWithComposer, false, true},

		{"claude plan exit (clear context)", claudePlanExitScreen, false, false},
		{"claude plan exit (auto mode)", claudePlanExitAutoModeScreen, false, false},
		{"claude plan exit (narrow, header scrolled off)", claudePlanExitNarrowScreen, false, false},
		{"claude exit plan mode yes/no", claudeExitPlanModeYesNoScreen, false, false},
		{"claude enter plan mode", claudeEnterPlanModeScreen, false, false},
		{"codex implement this plan", codexImplementPlanScreen, true, false},
		{"codex clear context and implement", codexClearContextImplementScreen, true, false},
		{"codex model question", codexModelQuestionScreen, true, false},
		{"codex model question (footer cropped)", codexModelQuestionCroppedScreen, true, false},
		{"claude use your reset", claudeUseResetScreen, false, false},
		{"claude re-enable usage credits", claudeReenableCreditsScreen, false, false},
		{"claude buy usage credits", claudeBuyCreditsScreen, false, false},
		{"claude switch model and continue", claudeSwitchModelScreen, false, false},
		{"codex use this reset", codexUseResetScreen, true, false},
		{"claude ask-user-question (wrapped footer)", claudeAskUserQuestionWrappedScreen, false, false},
		{"claude ask-user-question (footer only, yes-proceed label)", claudeAskUserQuestionFooterOnlyScreen, false, false},
	}
	for _, c := range cases {
		if got := isAutoApprovalPromptText(c.in, c.hasCodex); got != c.want {
			t.Errorf("%s: isAutoApprovalPromptText = %v, want %v (reason %q)",
				c.name, got, c.want, autoApprovalDeclineReason(c.in, c.hasCodex))
		}
	}
}

// The escape check must be option-row based: "know"/"not"/"cancelled" in the
// prompt body used to satisfy the bare "no"/"cancel" substring check.
func TestAutoApprovalEscapeOptionIsWordAnchored(t *testing.T) {
	t.Parallel()
	noEscape := fixtureScreen(
		" Bash command",
		"",
		"   echo 'I know this is not reversible; the last run was cancelled'",
		"",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, and don't ask again for echo commands",
	)
	if isAutoApprovalPromptText(noEscape, false) {
		t.Fatal("menu without a No/Cancel option was approved via body text")
	}
	withNo := noEscape + "\n   3. No"
	if !isAutoApprovalPromptText(withNo, false) {
		t.Fatalf("menu with a No option declined: %q", autoApprovalDeclineReason(withNo, false))
	}
	withEscFooter := noEscape + "\n\n Esc to cancel · Tab to amend"
	if !isAutoApprovalPromptText(withEscFooter, false) {
		t.Fatalf("menu with an Esc-to-cancel footer declined: %q", autoApprovalDeclineReason(withEscFooter, false))
	}
}

// A resolved Codex command modal left in scrollback must not let the header
// fast path approve a LATER focused menu.
func TestAutoApprovalStaleCodexHeaderDoesNotApproveLaterMenu(t *testing.T) {
	t.Parallel()
	stale := "Would you like to run the following command?\n\nReason: Run tests.\n\n$ go test ./daemon\n\nPress enter to confirm or esc to cancel\n\n• Ran go test ./daemon\n\n"
	if !isAutoApprovalPromptText(stale, true) {
		t.Fatal("precondition: header-only Codex modal should be approvable on its own")
	}
	for name, later := range map[string]string{
		"plan menu":    codexImplementPlanScreen,
		"question":     codexModelQuestionScreen,
		"generic menu": "› 1. Open settings\n  2. Back to chat\n",
	} {
		if isAutoApprovalPromptText(stale+later, true) {
			t.Errorf("%s: stale Codex command header approved a later non-approval menu", name)
		}
	}
	// A newer command modal below an older plan menu is still approvable.
	if !isAutoApprovalPromptText(codexImplementPlanScreen+"\n\n"+stale, true) {
		t.Fatal("newest header-only Codex command modal was declined")
	}
}

// The cautious ARIA path must share the classifier, not keep its own.
func TestCautiousAutoDenySharesDenyClassifier(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]string{
		"plan exit": claudePlanExitScreen,
		"use reset": claudeUseResetScreen,
		"question":  claudeAskUserQuestionWrappedScreen,
	} {
		if reason := cautiousAutoDenyReason(s, "1\n"); reason == "" {
			t.Errorf("%s: cautiousAutoDenyReason allowed option 1", name)
		}
	}
}

func TestNormalizePromptTextJoinsWrappedFooter(t *testing.T) {
	t.Parallel()
	got := normalizePromptText(" Enter to\n select · ↑/↓ to \n navigate  · Esc  to\ncancel")
	if want := "enter to select · ↑/↓ to navigate · esc to cancel"; got != want {
		t.Fatalf("normalizePromptText = %q, want %q", got, want)
	}
}

// Synthetic menus share an accept-once row and generic footer. Clipping the
// header and siblings leaves the actual short-pane Claude permission shape;
// that lone row cannot establish which menu it belongs to.
func TestAutoApprovalMenuVisibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, header, siblings, fullReason string
	}{
		{"plan-exit rows", "", "   2. Yes, manually approve edits\n   3. No, keep planning", "plan-exit menu"},
		{"plan-exit header", "Ready to code?", "   2. No", "plan-exit menu"},
		{"exit-plan yes/no", "Exit plan mode?", "   2. No", "plan-exit menu"},
		{"reset header", "Use your reset?", "   2. No", "billing/reset confirmation"},
		{"usage credits row", "", "   2. Yes, buy usage credits\n   3. No", "billing/credits menu"},
		{"upgrade row", "", "   2. Upgrade to Max\n   3. No", "billing/upgrade menu"},
		{"question with generic footer", "", "   2. Type something.\n   3. No", "question menu"},
		{"plain permission", "", "   2. No", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, provider := range []struct {
				name, pointer string
				hasCodex      bool
			}{
				{"claude", "❯", false},
				{"codex", "›", true},
			} {
				t.Run(provider.name, func(t *testing.T) {
					clippedRows := fixtureScreen(" "+provider.pointer+" 1. Yes", "", " Esc to cancel · Tab to amend")
					clipped := fixtureScreen("Do you want to proceed?", clippedRows)
					header := tc.header
					if header == "" {
						header = "Do you want to proceed?"
					}
					full := fixtureScreen(header, " "+provider.pointer+" 1. Yes", tc.siblings, "", " Esc to cancel · Tab to amend")
					if got := autoApprovalDeclineReason(full, provider.hasCodex); got != tc.fullReason {
						t.Fatalf("full menu: reason = %q, want %q", got, tc.fullReason)
					}
					for _, screen := range []string{clipped, clippedRows} {
						if got := autoApprovalDeclineReason(screen, provider.hasCodex); got == "" {
							t.Fatalf("clipped menu approved: %q", screen)
						}
					}
					if got := autoApprovalDeclineReason(clipped, provider.hasCodex); got != "menu has no visible sibling option" {
						t.Fatalf("clipped permission shape: reason = %q", got)
					}
					// Deny evidence that remains visible must still win over
					// the new structural check.
					if tc.header != "" {
						if got := autoApprovalDeclineReason(tc.header+"\n"+clipped, provider.hasCodex); got != tc.fullReason {
							t.Fatalf("visible header with clipped rows: reason = %q, want %q", got, tc.fullReason)
						}
					}
				})
			}
		})
	}
}

func TestAutoApprovalMenuVisibilityRequiresAcceptOnceFocus(t *testing.T) {
	t.Parallel()
	for _, label := range []string{
		"Always allow",
		"Yes, and switch to auto mode",
		"Yes, and use auto mode",
	} {
		t.Run(label, func(t *testing.T) {
			for _, siblings := range []string{"", "   2. No", " ↓ 2. Yes"} {
				s := fixtureScreen("Do you want to proceed?", " ❯ 1. "+label, siblings, "", " Esc to cancel · Tab to amend")
				if reason := autoApprovalDeclineReason(s, false); reason == "" {
					t.Fatalf("focused %q approved with siblings %q", label, siblings)
				}
			}
		})
	}
}
