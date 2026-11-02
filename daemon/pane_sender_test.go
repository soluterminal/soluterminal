package main

import (
	"strings"
	"sync/atomic"
	"testing"
)

func TestSendInputLabelsOnlyAgentMessagesFromAnotherPane(t *testing.T) {
	for _, tc := range []struct{ name, caller, text, frame string }{
		{"message", "sender", "Please review this\n", "\x1b[200~[from #1 Claude] Please review this\x1b[201~"},
		{"multiline", "sender", "First line\nsecond line\n", "\x1b[200~[from #1 Claude] First line\nsecond line\x1b[201~"},
		{"claimed sender", mcpClaimedCallerPrefix + "sender", "Please review this\n", "\x1b[200~[from #1 Claude (unverified)] Please review this\x1b[201~"},
		{"claimed target", mcpClaimedCallerPrefix + "target", "Please review this\n", "\x1b[200~[from #2 Codex (unverified)] Please review this\x1b[201~"},
		{"unknown claim", mcpClaimedCallerPrefix + "missing", "Please review this\n", "\x1b[200~[from an unverified agent] Please review this\x1b[201~"},
		{"human", "", "Please review this\n", "\x1b[200~Please review this\x1b[201~"},
		{"self", "target", "Please review this\n", "\x1b[200~Please review this\x1b[201~"},
		{"shell sender", "shell", "Please review this\n", "\x1b[200~Please review this\x1b[201~"},
		{"two keys", "sender", "ok\n", "ok"},
		{"one key", "sender", "y\n", "y"},
		// Unterminated text for an agent pane is refused unsent
		// (TestSendInputRefusesUnsubmittedMessageToAgentPane); keys stay raw.
		{"two raw keys", "sender", "ab", "ab"},
		{"arrow", "sender", "\x1b[A", "\x1b[A"},
		{"compact command", "sender", "/compact\n", "\x1b[200~/compact\x1b[201~"},
		{"clear command", "sender", "/clear\n", "\x1b[200~/clear\x1b[201~"},
		{"model command", "sender", "/model opus\n", "\x1b[200~/model opus\x1b[201~"},
		{"shell escape", "sender", "!git status\n", "\x1b[200~!git status\x1b[201~"},
		{"memory shortcut", "sender", "#remember this\n", "\x1b[200~#remember this\x1b[201~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, frames := newTypingHoldPane(t, "target")
			p.Agent = agentCodex
			a := &apiServer{mgr: NewManager("")}
			a.mgr.panes[p.ID] = p
			a.mgr.panes["sender"] = testPaneWithTail(t, "sender", "claude", "")
			a.mgr.panes["sender"].Agent = agentClaude
			a.mgr.panes["shell"] = testPaneWithTail(t, "shell", "bash", "")
			a.ws = &WorkspaceStore{data: map[string]*Workspace{"w": {Name: "w", PaneIds: []string{"sender", "target"}}}}
			var screen atomic.Value
			screen.Store(deliveryCodexScreen(""))
			stubDeliveryScreen(t, &screen)
			done := make(chan any, 1)
			go func() { done <- a.mcpToolSendInput(map[string]any{"pane_id": p.ID, "text": tc.text}, tc.caller) }()
			expectFrame(t, frames, "body", tc.frame)
			if strings.HasSuffix(tc.text, "\n") {
				expectFrame(t, frames, "Enter", "\r")
			}
			text, failed := mcpResultText(t, <-done)
			if failed {
				t.Fatal(text)
			}
			expectNoFrame(t, frames, "no extra input")
		})
	}
}
