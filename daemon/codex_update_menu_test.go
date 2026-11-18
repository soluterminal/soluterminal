package main

import (
	"strings"
	"testing"
	"time"
)

// ST-289: Codex's own start-up update menu, recorded from the official
// 0.168.0 and 0.169.0 builds (testdata/menu_screens/README.md). The menu is
// on screen at "menu" (focus on Update now) and "menu_moved" (focus on Skip);
// Esc skipped it at "skipped", where the input box shows under Codex's
// "Update available!" banner.
var codexUpdateMenuSessions = []struct{ name, from, to string }{
	{"codex_0168_update_100x40", "0.168.0", "0.169.0"},
	{"codex_0168_update_60x40", "0.168.0", "0.169.0"},
	{"codex_0169_update_100x40", "0.169.0", "0.170.0"},
}

func TestCodexUpdateMenuRecorded(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310", "0.169.0")
	for _, s := range codexUpdateMenuSessions {
		session, raw := loadMenuScreenSession(t, s.name)
		for _, step := range session.Steps {
			t.Run(s.name+"/"+step.Name, func(t *testing.T) {
				p := menuScreenPane(t, session, raw[:step.Bytes], agentCodex)
				p.ensureGrid()
				defer p.shutdownGrid()
				screen := paneVisibleScreen(p)
				from, to, menu := codexUpdateMenuVersions(screen)
				open := step.Name != "skipped"
				if menu != open {
					t.Fatalf("update menu = %v, want %v:\n%s", menu, open, screen)
				}
				if !open {
					if !paneAgentReady(p, agentCodex) || p.computeStateCached() == StateChoicePrompt {
						t.Fatalf("the input box after Skip is not ready (state %s):\n%s", p.computeStateCached(), screen)
					}
					return
				}
				if from != s.from || to != s.to {
					t.Fatalf("versions %s → %s, want %s → %s", from, to, s.from, s.to)
				}
				// A menu wherever its focus is, never an input box.
				if !isGridChoicePromptText(screen, true) || p.computeStateCached() != StateChoicePrompt {
					t.Fatalf("not a menu (state %s):\n%s", p.computeStateCached(), screen)
				}
				if paneAgentReady(p, agentCodex) || agentInputBoxVisible(screen, agentCodex) {
					t.Fatal("the update menu read as Codex's input box")
				}
				// The person's to answer: auto-approve declines it.
				if reason := autoApprovalDeclineReason(screen, true); !strings.Contains(reason, "update menu") {
					t.Fatalf("auto-approve reason %q", reason)
				}
			})
		}
	}
}

// Text that only mentions an update, or the banner Codex shows after Skip,
// is not the menu; neither is a menu followed by a live input box.
func TestCodexUpdateMenuNegatives(t *testing.T) {
	menu := "  Update available · 0.168.0 → 0.169.0\n  Release notes: https://github.com/openai/codex/releases/latest\n› 1. Update now (runs `npm install -g @openai/codex`)\n  2. Skip\n  3. Skip until next version\n  enter continue · esc skip\n"
	if len(codexUpdateMenuMatches(menu)) != 1 {
		t.Fatal("the recorded shape did not match")
	}
	// Its span overlaps choicePromptRE's focused "1." row: one menu, and the
	// fingerprint (which slices between spans) still works.
	if n := len(choicePromptMatches(menu)); n != 1 {
		t.Fatalf("%d choice spans for one menu", n)
	}
	moved := strings.Replace(strings.Replace(menu, "› 1.", "  1.", 1), "  2. Skip", "› 2. Skip", 1)
	if approvalPromptFingerprint(menu, true) == 0 || approvalPromptFingerprint(moved, true) == 0 {
		t.Fatal("no fingerprint for the update menu")
	}
	for name, s := range map[string]string{
		"banner":       "│ ✨  Update available! 0.168.0 -> 0.169.0 │\n│ Run npm install -g @openai/codex to update. │\n",
		"prose":        "• Update available · 0.168.0 → 0.169.0 is what Codex said earlier.\n",
		"no footer":    strings.Replace(menu, "  enter continue · esc skip\n", "", 1),
		"no skip":      strings.Replace(menu, "  2. Skip\n", "", 1),
		"box after it": menu + "\n› Ask Codex to do anything\n\n  gpt-5.1 default · ~/work/repo\n",
	} {
		if len(codexUpdateMenuMatches(s)) != 0 {
			t.Fatalf("%s matched", name)
		}
	}
}

// spawn_pane with a prompt on a Codex pane held by its update menu returns
// at once, names the menu, and holds the prompt on the pane instead of
// typing it into the menu (where it would choose an option).
func TestSpawnHoldsPromptBehindCodexUpdateMenu(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310", "0.168.0")
	session, raw := loadMenuScreenSession(t, "codex_0168_update_100x40")
	for _, step := range []string{"menu", "menu_moved"} {
		t.Run(step, func(t *testing.T) {
			at := map[string]int{}
			for _, s := range session.Steps {
				at[s.Name] = s.Bytes
			}
			p, frames := newTypingHoldPane(t, "pane_update_"+step)
			p.Agent = agentCodex
			p.Cols, p.Rows = session.Cols, session.Rows
			codexUpdateFeed(p, raw[:at[step]], true)
			a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}

			start := time.Now()
			out := a.spawnFollowUp(p, []string{"codex"}, map[string]any{"prompt": "review the diff"})
			if time.Since(start) > 10*time.Second {
				t.Fatalf("spawn waited %s behind a menu only the person answers", time.Since(start))
			}
			hint, _ := out["hint"].(string)
			if out["ready"] != false || out["status"] != "needs_user" || out["menu"] != codexUpdateMenuName || out["prompt_sent"] != false || out["delivery"] != "held" || !strings.Contains(hint, "0.168.0 → 0.169.0") {
				t.Fatalf("spawn result: %+v", out)
			}
			p.mu.Lock()
			held := append([]*heldDelivery(nil), p.heldDeliveries...)
			reason := p.heldReason
			p.mu.Unlock()
			if len(held) != 1 || held[0].body != "review the diff" || reason != heldMenuReason {
				t.Fatalf("held: %d %q", len(held), reason)
			}
			// Auto mode is "always" on this pane: nothing answers the menu,
			// and the held prompt is not typed while the menu is up.
			if !menuBeforeKey(p, true) {
				t.Fatal("the held queue does not see the menu")
			}
			expectNoFrame(t, frames, "nothing typed into the update menu")
			p.resolveHeld(held[0], "discarded", nil)
		})
	}
	// After Skip the box is free: the held queue types the prompt there.
	p, _ := newTypingHoldPane(t, "pane_update_skipped")
	p.Agent = agentCodex
	p.Cols, p.Rows = session.Cols, session.Rows
	codexUpdateFeed(p, raw, true)
	if menuBeforeKey(p, true) {
		t.Fatal("the skipped menu still holds the pane")
	}
	if reason := p.draftGuardReason(true, true, time.Now(), p.humanComposerPendingSnapshot); reason != "" {
		t.Fatalf("the input box after Skip holds messages: %q", reason)
	}
}

func codexUpdateFeed(p *Pane, raw []byte, fresh bool) {
	p.ensureGrid()
	p.subsMu.Lock()
	if fresh {
		raw = append([]byte("\x1b[H\x1b[2J"), raw...)
	}
	p.feedGridPayloadLocked(raw, true)
	_ = p.gridEmu.DrainReplies()
	p.subsMu.Unlock()
}

// The update menu is the person's alone: a traced agent with the grant, which
// may answer other panes' prompts, is refused by answer_prompt, by send_input
// keys and by the REST/socket key check, and nothing is written.
func TestAgentsNeverAnswerTheCodexUpdateMenu(t *testing.T) {
	setDetectedAgentVersions(t, "2.1.310", "0.168.0")
	session, raw := loadMenuScreenSession(t, "codex_0168_update_100x40")
	for _, step := range []string{"menu", "menu_moved"} {
		t.Run(step, func(t *testing.T) {
			at := map[string]int{}
			for _, s := range session.Steps {
				at[s.Name] = s.Bytes
			}
			a := st10Server(t, true)
			p, frames := newTypingHoldPane(t, "U")
			p.AutoMode = ""
			p.Agent = agentCodex
			p.Cols, p.Rows = session.Cols, session.Rows
			codexUpdateFeed(p, raw[:at[step]], true)
			a.mgr.panes["U"] = p
			if !promptTakesKeys(p) {
				t.Fatal("the update menu does not take keys")
			}
			traced := callerIdentity{Kind: callerPane, PaneID: "B", PID: 4242}
			if v := a.decidePromptAnswer(mcpCallMeta{id: traced, transport: "socket"}, false, p); v.allowed || v.refusal != promptAnswerCodexUpdate {
				t.Fatalf("decision: %+v", v)
			}
			if text := answerViaSocket(t, a, traced, "U"); !strings.Contains(text, promptAnswerCodexUpdate) {
				t.Fatalf("answer_prompt: %q", text)
			}
			msg := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_input","arguments":{"pane_id":"U","text":"1"}}}`
			resp, _ := a.mcpSocketDispatch([]byte(msg), traced)
			if text, isErr := mcpResultText(t, resp.Result); !isErr {
				t.Fatalf("send_input keys: %q", text)
			}
			// Admitted to answer prompts (heldAtPrompt false, as when the
			// menu appeared after admission): the writer lock's check holds.
			admitted, _ := a.mcpToolSendInputOutcomeAs(map[string]any{"pane_id": "U", "text": "1"}, deliveryNoSteer, paneCaller{ID: "B"})
			if text, isErr := mcpResultText(t, admitted); !isErr || !strings.Contains(text, errPromptHeld.Error()) {
				t.Fatalf("admitted keys: %q", text)
			}
			if err := a.promptWriteCheck(traced, p, []byte("1"), "pane-socket"); err == nil || err() == nil {
				t.Fatal("REST/socket keys pass the update menu")
			}
			expectNoFrame(t, frames, "nothing written to the update menu")
			// Another pane's ordinary menu is unaffected.
			if v := a.decidePromptAnswer(mcpCallMeta{id: traced, transport: "socket"}, false, a.mgr.panes["A"]); !v.allowed {
				t.Fatalf("ordinary prompt: %+v", v)
			}
		})
	}
}
