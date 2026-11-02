package main

import (
	"encoding/json"

	"strings"
	"testing"
	"time"
)

func scopedCallForTest(t *testing.T, a *apiServer, scope *mcpScope, name string, args map[string]any) any {
	t.Helper()
	b, err := json.Marshal(toolCallParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return a.mcpCallTool(b, scope, "")
}

func TestMcpScopeStoreMintLookupExpiryRevoke(t *testing.T) {
	t.Parallel()
	s := newMcpScopeStore()

	tok := s.Mint("reviewer", []string{"list_panes", "set_pane_flag"}, time.Minute)
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(tok))
	}
	sc, ok := s.Lookup(tok)
	if !ok {
		t.Fatal("freshly minted token did not resolve")
	}
	if sc.Identity != "reviewer" || !sc.allows("list_panes") || sc.allows("kill_pane") {
		t.Fatalf("scope = %+v, want reviewer with exactly the minted tools", sc)
	}

	if _, ok := s.Lookup("not-a-token"); ok {
		t.Fatal("unknown token resolved")
	}
	if _, ok := s.Lookup(""); ok {
		t.Fatal("empty token resolved")
	}

	expired := s.Mint("reviewer", []string{"list_panes"}, -time.Second)
	if _, ok := s.Lookup(expired); ok {
		t.Fatal("expired token resolved")
	}

	s.Revoke(tok)
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("revoked token resolved")
	}

	// Nil-receiver safety — partial wiring must degrade, not panic.
	var nilStore *mcpScopeStore
	if _, ok := nilStore.Lookup(tok); ok {
		t.Fatal("nil store resolved a token")
	}
	nilStore.Revoke(tok) // must not panic
}

func TestMCPScopedCallEnforcesToolBudget(t *testing.T) {
	t.Parallel()
	a := &apiServer{mgr: NewManager(""), uiBus: newUIBus()}
	scope := &mcpScope{
		Identity: "reviewer",
		Tools:    map[string]bool{"list_panes": true},
		Expires:  time.Now().Add(time.Minute),
	}

	// Out-of-scope tool → refused with the scope error, regardless of the
	// mutation policy (kill_pane would otherwise fail later on pane lookup).
	text, isErr := mcpResultText(t, scopedCallForTest(t, a, scope, "kill_pane", map[string]any{"pane_id": "x"}))
	if !isErr || !strings.Contains(text, "not in this session's scope") {
		t.Fatalf("out-of-scope call = (%q, err=%v), want scope refusal", text, isErr)
	}

	// In-scope tool works.
	text, isErr = mcpResultText(t, scopedCallForTest(t, a, scope, "list_panes", map[string]any{}))
	if isErr {
		t.Fatalf("in-scope list_panes errored: %q", text)
	}
}

func TestMCPToolsListFilteredByScope(t *testing.T) {
	t.Parallel()
	scope := &mcpScope{
		Identity: "reviewer",
		Tools:    map[string]bool{"list_panes": true, "read_pane_tail": true},
		Expires:  time.Now().Add(time.Minute),
	}
	got := mcpToolsForScope(scope)
	if len(got) != 2 {
		names := make([]string, 0, len(got))
		for _, tl := range got {
			names = append(names, tl.Name)
		}
		t.Fatalf("filtered tools = %v, want exactly the 2 scoped ones", names)
	}
	for _, tl := range got {
		if !scope.allows(tl.Name) {
			t.Fatalf("tools/list leaked out-of-scope tool %s", tl.Name)
		}
	}
	if n := len(mcpToolsForScope(nil)); n != len(mcpTools()) {
		t.Fatalf("nil scope filtered the list: %d != %d", n, len(mcpTools()))
	}
}

func TestMCPSetPaneFlagProvenanceGuard(t *testing.T) {
	t.Parallel()
	p := testPaneWithTail(t, "pane_flag1", "worker", "$ ")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	reviewer := &mcpScope{Identity: "reviewer", Tools: map[string]bool{"set_pane_flag": true}, Expires: time.Now().Add(time.Minute)}

	// User flags the pane; a scoped caller may not overwrite or clear it.
	p.SetFlag(true, "mine — checking later")
	text, isErr := mcpResultText(t, scopedCallForTest(t, a, reviewer, "set_pane_flag",
		map[string]any{"pane_id": p.ID, "flagged": true, "memo": "reviewer brief"}))
	if !isErr || !strings.Contains(text, "already flagged by 'user'") {
		t.Fatalf("scoped overwrite of user flag = (%q, err=%v), want provenance refusal", text, isErr)
	}
	text, isErr = mcpResultText(t, scopedCallForTest(t, a, reviewer, "set_pane_flag",
		map[string]any{"pane_id": p.ID, "flagged": false}))
	if !isErr {
		t.Fatalf("scoped clear of user flag succeeded: %q", text)
	}
	if snap := p.Snapshot(); !snap.Flagged || snap.FlagOwner != "user" || snap.FlagMemo != "mine — checking later" {
		t.Fatalf("user flag was disturbed: %+v", snap)
	}

	// Full-access callers must preserve the person's bookmark too.
	for _, flagged := range []bool{true, false} {
		text, isErr = mcpResultText(t, scopedCallForTest(t, a, nil, "set_pane_flag",
			map[string]any{"pane_id": p.ID, "flagged": flagged, "memo": "agent note"}))
		if !isErr || !strings.Contains(text, "only the person") {
			t.Fatalf("full-access changed user flag: %q", text)
		}
	}

	// The reviewer may claim an unowned/cleared pane and clear its OWN flag.
	p.SetFlagAs(false, "", "")
	if text, isErr = mcpResultText(t, scopedCallForTest(t, a, reviewer, "set_pane_flag",
		map[string]any{"pane_id": p.ID, "flagged": true, "memo": "needs a decision: A or B — recommend A"})); isErr {
		t.Fatalf("reviewer flag on unflagged pane refused: %q", text)
	}
	if snap := p.Snapshot(); snap.FlagOwner != "reviewer" {
		t.Fatalf("owner = %q, want reviewer", snap.FlagOwner)
	}
	if text, isErr = mcpResultText(t, scopedCallForTest(t, a, reviewer, "set_pane_flag",
		map[string]any{"pane_id": p.ID, "flagged": false})); isErr {
		t.Fatalf("reviewer clearing its own flag refused: %q", text)
	}
	if snap := p.Snapshot(); snap.Flagged || snap.FlagOwner != "" {
		t.Fatalf("clear left residue: %+v", snap)
	}
}

func TestSetFlagMemoRuneSafeCap(t *testing.T) {
	t.Parallel()
	p := testPaneWithTail(t, "pane_memo", "t", "$ ")
	// 600 four-byte runes: the old byte-slice cap would cut mid-rune.
	long := strings.Repeat("🦉", 600)
	p.SetFlag(true, long)
	snap := p.Snapshot()
	if r := []rune(snap.FlagMemo); len(r) != flagMemoMaxRunes {
		t.Fatalf("memo runes = %d, want %d", len(r), flagMemoMaxRunes)
	}
	if !strings.HasSuffix(snap.FlagMemo, "🦉") {
		t.Fatal("memo does not end on a whole rune — cap split a UTF-8 sequence")
	}
}

func TestRedactTerminalTail(t *testing.T) {
	t.Parallel()
	in := strings.Join([]string{
		"$ gh auth token",
		"gho_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"$ echo $OPENAI_KEY",
		"sk-proj-abcdefghijklmnop1234",
		"PASSWORD=hunter2",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"b3BlbnNzaC1rZXktdjEAAAAA",
		"-----END OPENSSH PRIVATE KEY-----",
		"commit 9779aa29d84430b2aaaaaaaabbbbbbbbcccccccc (HEAD -> main)",
		"/home/user/wsl-projects/soluterminal/daemon/mcp.go:42",
	}, "\n")
	out := redactTerminalTail(in)

	for _, leaked := range []string{"gho_", "sk-proj", "hunter2", "eyJhbGci", "OPENSSH PRIVATE KEY-----\nb3Blbn"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("tail leaked %q:\n%s", leaked, out)
		}
	}
	// Legitimate tail content must SURVIVE — this scrub is keyed rules only.
	for _, kept := range []string{
		"commit 9779aa29d84430b2aaaaaaaabbbbbbbbcccccccc",
		"/home/user/wsl-projects/soluterminal/daemon/mcp.go:42",
	} {
		if !strings.Contains(out, kept) {
			t.Fatalf("tail scrub destroyed legitimate content %q:\n%s", kept, out)
		}
	}
	if !strings.Contains(out, "PASSWORD=[REDACTED]") {
		t.Fatalf("kv secret not scrubbed with keyword kept:\n%s", out)
	}
}
