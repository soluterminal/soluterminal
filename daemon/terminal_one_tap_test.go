package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ST-265: terminal one-tap, pinned on real Claude Code 2.1.322 and Codex
// 0.169 screens (testdata/menu_screens, recorded 14 Nov).

// realScreenPane is the pane a recorded step leaves: its raw bytes up to that
// step, under a unique id so hook records never leak between tests.
func realScreenPane(t *testing.T, session, step, id string) *Pane {
	t.Helper()
	s, raw := loadMenuScreenSession(t, session)
	for _, st := range s.Steps {
		if st.Name == step {
			p := testPaneWithTail(t, id, "screen", string(raw[:st.Bytes]))
			p.Cols, p.Rows = s.Cols, s.Rows
			p.Pid = 100 // fakePaneTree: 100 bash -> 200 claude
			p.Agent = "claude"
			if strings.HasPrefix(session, "codex") {
				p.Agent = "codex"
			}
			return p
		}
	}
	t.Fatalf("%s has no step %s", session, step)
	return nil
}

func realScreenGrid(t *testing.T, session, step string) string {
	t.Helper()
	p := realScreenPane(t, session, step, "grid-"+session+"-"+step)
	p.ensureGrid()
	grid, ok := p.gridVisibleText()
	if !ok {
		t.Fatalf("%s/%s: no grid", session, step)
	}
	return grid
}

func TestExtractClaudeApprovalDialogOnRealScreens(t *testing.T) {
	t.Parallel()
	summaries := map[string]string{
		"bash_permission":           "Bash command\ntouch perm_probe.txt\nDo you want to proceed?",
		"write_permission":          "Create file\nnotes.txt\nDo you want to create notes.txt?",
		"edit_permission":           "Edit file\nREADME.md\nDo you want to make this edit to README.md?",
		"settings_write_permission": "Create file\n.claude/settings.local.json\nDo you want to create settings.local.json?",
	}
	for _, tc := range []struct {
		session, step, kind, command, path string
	}{
		{"claude_2_1_322_perm_100x40", "bash_permission", "bash", "touch perm_probe.txt", ""},
		{"claude_2_1_322_perm_100x40", "sudo_permission", "bash", "sudo -n true", ""},
		{"claude_2_1_322_perm_100x40", "write_permission", "write", "", "notes.txt"},
		{"claude_2_1_322_perm_100x40", "edit_permission", "edit", "", "README.md"},
		{"claude_2_1_322_perm_100x40", "settings_write_permission", "write", "", ".claude/settings.local.json"},
		// A dangerouslyDisableSandbox call renders exactly like a plain one:
		// widening is judged on the hook record, never on this screen.
		{"claude_2_1_322_sandbox_100x40", "sandbox_off_permission", "bash", "touch outside_probe.txt", ""},
		{"claude_2_1_322_sandbox_100x40", "sandboxed_network", "bash", "curl -sI https://example.com", ""},
		// Not answerable by one-tap:
		{"claude_2_1_322_perm_100x40", "trust", "", "", ""},
		{"claude_2_1_322_perm_100x40", "idle", "", "", ""},
		{"claude_2_1_322_perm_100x40", "bash_cancelled", "", "", ""},
		{"claude_2_1_322_perm_100x40", "webfetch_permission", "", "", ""},
		{"codex_0169_ro_100x40", "write_command", "", "", ""},
		{"codex_0169_ro_100x40", "patch_approval", "", "", ""},
		{"codex_0169_ro_100x40", "escalated_command", "", "", ""},
	} {
		t.Run(tc.session+"/"+tc.step, func(t *testing.T) {
			v, why := extractClaudeApprovalDialog(realScreenGrid(t, tc.session, tc.step))
			if tc.kind == "" {
				if why == "" {
					t.Fatalf("extracted %+v from a screen one-tap must not answer", v)
				}
				return
			}
			if why != "" {
				t.Fatalf("refused: %s", why)
			}
			if v.Kind != tc.kind || v.Command != tc.command || v.Path != tc.path {
				t.Fatalf("view = %+v", v)
			}
			// The summary is the dialog's own lines only: no transcript, no
			// model-written description, no options or file content.
			if want, ok := summaries[tc.step]; ok && v.Summary != want {
				t.Fatalf("summary =\n%q\nwant\n%q", v.Summary, want)
			}
			if strings.Contains(v.Summary, "❯") || strings.Contains(v.Summary, "Tip:") || strings.Count(v.Summary, "\n") != 2 || len(v.Summary) > termSummaryMax {
				t.Fatalf("summary is not the compact dialog:\n%s", v.Summary)
			}
		})
	}
}

// @secure: focus must be on option 1; a command must be one untruncated line.
func TestExtractClaudeApprovalDialogRefusals(t *testing.T) {
	t.Parallel()
	grid := realScreenGrid(t, "claude_2_1_322_perm_100x40", "bash_permission")
	if _, why := extractClaudeApprovalDialog(grid); why != "" {
		t.Fatalf("baseline refused: %s", why)
	}
	moved := strings.Replace(strings.Replace(grid, " ❯ 1. Yes", "   1. Yes", 1), "   2. Yes, and always", " ❯ 2. Yes, and always", 1)
	if _, why := extractClaudeApprovalDialog(moved); !strings.Contains(why, "option 1") {
		t.Fatalf("focus on option 2: %q", why)
	}
	wrapped := replaceGridLine(grid, "touch perm_probe.txt", " touch perm_probe.txt &&\n rm -rf ~/x")
	if _, why := extractClaudeApprovalDialog(wrapped); !strings.Contains(why, "single line") {
		t.Fatalf("two-line command: %q", why)
	}
	cut := replaceGridLine(grid, "touch perm_probe.txt", " touch perm_probe.t…")
	if _, why := extractClaudeApprovalDialog(cut); !strings.Contains(why, "truncated") {
		t.Fatalf("truncated command: %q", why)
	}
	if wrapped == grid || cut == grid {
		t.Fatal("test edit did not reach the dialog's command line")
	}
	long := replaceGridLine(grid, "touch perm_probe.txt", " touch "+strings.Repeat("a", 220))
	if _, why := extractClaudeApprovalDialog(long); !strings.Contains(why, "too long") {
		t.Fatalf("over-long command: %q", why)
	}
	bidi := replaceGridLine(grid, "touch perm_probe.txt", " touch perm_\u202eprobe.txt")
	if _, why := extractClaudeApprovalDialog(bidi); !strings.Contains(why, "invisible") {
		t.Fatalf("bidi override: %q", why)
	}
	two := grid + "\n" + grid
	if _, why := extractClaudeApprovalDialog(two); why == "" {
		t.Fatal("two dialogs on one screen accepted")
	}
}

// @secure change 1: only the pane's own top-level claude may post a record.
func TestIsPaneClaudeMainProcess(t *testing.T) {
	comm := map[int]string{100: "bash", 200: "claude", 300: "bash", 400: "claude", 500: "curl", 900: "claude"}
	parent := map[int]int{200: 100, 300: 200, 400: 300, 500: 300, 900: 1}
	prevC, prevP := termProcComm, termProcParent
	termProcComm = func(pid int) string { return comm[pid] }
	termProcParent = func(pid int) int { return parent[pid] }
	t.Cleanup(func() { termProcComm, termProcParent = prevC, prevP })

	for _, tc := range []struct {
		pane, pid int
		want      bool
	}{
		{100, 200, true},  // the pane's claude
		{100, 400, false}, // a nested `claude -p` run by a tool
		{100, 500, false}, // the agent's own curl
		{100, 300, false}, // the agent's shell
		{100, 900, false}, // a claude outside the pane
		{200, 200, true},  // the pane runs claude directly
		{200, 400, false}, // a claude under it
		{0, 200, false},
	} {
		if got := isPaneClaudeMainProcess(tc.pane, tc.pid); got != tc.want {
			t.Errorf("pane %d pid %d = %v, want %v", tc.pane, tc.pid, got, tc.want)
		}
	}
}

// The hook path keeps a record from the pane's claude and drops one posted by
// a child process (an agent's curl forging PreToolUse).
func TestHookRecordsOnlyFromPaneClaude(t *testing.T) {
	comm := map[int]string{100: "bash", 200: "claude", 300: "bash", 500: "curl"}
	parent := map[int]int{200: 100, 300: 200, 500: 300}
	prevC, prevP := termProcComm, termProcParent
	termProcComm = func(pid int) string { return comm[pid] }
	termProcParent = func(pid int) int { return parent[pid] }
	t.Cleanup(func() { termProcComm, termProcParent = prevC, prevP })
	p := testPaneWithTail(t, "term-hook-pid", "hook", "")
	p.Pid = 100
	a := &apiServer{mgr: testManagerWithPane(p)}
	post := func(pid int, cmd string) {
		t.Helper()
		setPeerPID(t, pid, true)
		body := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"tu-` + cmd + `","tool_input":{"command":"` + cmd + `"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/hooks/tool-call", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("X-Soluterminal-Pane", p.ID)
		var payload hookPayload
		_ = json.Unmarshal([]byte(body), &payload)
		a.recordTermPreToolFromHook(req, p.ID, payload)
	}
	post(200, "older")
	post(500, "forged")
	if recs := termPreToolSnapshot(p.ID); len(recs) != 0 {
		t.Fatalf("an unattributed PreToolUse did not clear the pane's records: %+v", recs)
	}
	post(200, "real")
	recs := termPreToolSnapshot(p.ID)
	if len(recs) != 1 || recs[0].ToolUseID != "tu-real" {
		t.Fatalf("records = %+v", recs)
	}
	// A PostToolUse from a child can't delete the live record.
	forget := func(pid int) {
		t.Helper()
		setPeerPID(t, pid, true)
		req := httptest.NewRequest(http.MethodPost, "/api/hooks/tool-call", nil)
		a.forgetTermPreToolFromHook(req, p.ID, hookPayload{HookEventName: "PostToolUse", ToolUseID: "tu-real"})
	}
	forget(500)
	if recs := termPreToolSnapshot(p.ID); len(recs) != 1 {
		t.Fatalf("a child's PostToolUse deleted the record: %+v", recs)
	}
	forget(200)
	if recs := termPreToolSnapshot(p.ID); len(recs) != 0 {
		t.Fatalf("PostToolUse did not clear: %+v", recs)
	}
}

func setPeerPID(t *testing.T, pid int, ok bool) {
	t.Helper()
	prev := lookupPeerPID
	lookupPeerPID = func(*http.Request) (int, bool) { return pid, ok }
	t.Cleanup(func() { lookupPeerPID = prev })
}

func claudeOnly(t *testing.T, yes bool) {
	t.Helper()
	prev := termPaneIsClaudeOnly
	termPaneIsClaudeOnly = func(*Pane) bool { return yes }
	t.Cleanup(func() { termPaneIsClaudeOnly = prev })
}

func addRecord(paneID, tool, input string) {
	recordTermPreTool(paneID, termPreTool{ToolUseID: paneID + input, Tool: tool, Input: json.RawMessage(input), At: time.Now(), PID: 200})
}

// fakePaneTree makes pid 200 the top-level claude of a pane whose process is
// 100; 300 is a shell under it.
func fakePaneTree(t *testing.T) {
	t.Helper()
	comm := map[int]string{100: "bash", 200: "claude", 300: "bash", 600: "claude"}
	parent := map[int]int{200: 100, 300: 200, 600: 100}
	prevC, prevP := termProcComm, termProcParent
	termProcComm = func(pid int) string { return comm[pid] }
	termProcParent = func(pid int) int { return parent[pid] }
	t.Cleanup(func() { termProcComm, termProcParent = prevC, prevP })
}

// Not parallel: swaps the process check.
func TestEvaluateTerminalOneTap(t *testing.T) {
	claudeOnly(t, true)
	fakePaneTree(t)

	p := realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-missing")
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "no matching") {
		t.Fatalf("no hook record: %q", why)
	}

	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-ok")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt","description":"x"}`)
	got, why := evaluateTerminalOneTap(p)
	if why != "" || got.Widens || got.Tool != "Bash" || !strings.HasPrefix(got.PromptID, "term:") || !strings.HasPrefix(strings.TrimSpace(got.Summary), "Bash command") {
		t.Fatalf("plain bash: %+v %q", got, why)
	}

	// Same screen, real input asks for the sandbox off: widens.
	p = realScreenPane(t, "claude_2_1_322_sandbox_100x40", "sandbox_off_permission", "term-eval-sandbox")
	addRecord(p.ID, "Bash", `{"command":"touch outside_probe.txt","dangerouslyDisableSandbox":true}`)
	if got, why := evaluateTerminalOneTap(p); why != "" || !got.Widens {
		t.Fatalf("sandbox off: %+v %q", got, why)
	}

	// Two records for the same command (one may be the sandbox-off one):
	// ambiguous, Review.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-two")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt"}`)
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt","dangerouslyDisableSandbox":true}`)
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "more than one") {
		t.Fatalf("two records: %q", why)
	}

	// A stale matching record behind a newer call: the dialog is for the
	// newest call, so Review.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-stale")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt"}`)
	addRecord(p.ID, "Read", `{"file_path":"/repo/README.md"}`)
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "newest") {
		t.Fatalf("stale record: %q", why)
	}

	// A retry of the identical call replaces the older record, so it isn't
	// ambiguous.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-retry")
	recordTermPreTool(p.ID, termPreTool{ToolUseID: "a", Tool: "Bash", Input: json.RawMessage(`{"command":"touch perm_probe.txt"}`), At: time.Now(), PID: 200})
	recordTermPreTool(p.ID, termPreTool{ToolUseID: "b", Tool: "Bash", Input: json.RawMessage(`{"command":"touch perm_probe.txt"}`), At: time.Now(), PID: 200})
	if got, why := evaluateTerminalOneTap(p); why != "" || got.Widens {
		t.Fatalf("identical retry: %+v %q", got, why)
	}

	// A record from a claude that is no longer the pane's agent (it exited
	// and another started, perhaps without our hook): Review.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-oldclaude")
	recordTermPreTool(p.ID, termPreTool{ToolUseID: "o", Tool: "Bash", Input: json.RawMessage(`{"command":"touch perm_probe.txt"}`), At: time.Now(), PID: 999})
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "no longer the pane's agent") {
		t.Fatalf("predecessor claude's record: %q", why)
	}

	// The bot's case: a plain record for a call the person already answered
	// (denied at the desktop) must not stand in for a later retry whose own
	// hook never arrived. Any input after the record makes it stale.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-answered")
	recordTermPreTool(p.ID, termPreTool{ToolUseID: "d", Tool: "Bash", Input: json.RawMessage(`{"command":"touch perm_probe.txt"}`), At: time.Now().Add(-time.Minute), PID: 200})
	atomic.StoreInt64(&p.LastInputMs, time.Now().UnixMilli())
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "predates the last input") {
		t.Fatalf("record from before an answer: %q", why)
	}

	// A record whose command has a hidden second line: Review.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-newline")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt\n"}`)
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "more than one line") {
		t.Fatalf("newline record: %q", why)
	}

	// sudo and permission-settings writes widen.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "sudo_permission", "term-eval-sudo")
	addRecord(p.ID, "Bash", `{"command":"sudo -n true"}`)
	if got, why := evaluateTerminalOneTap(p); why != "" || !got.Widens {
		t.Fatalf("sudo: %+v %q", got, why)
	}
	// File create/edit dialogs show the path, not the content: Review,
	// whatever the path (one-tap answers Bash only, like the phone's rule).
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "settings_write_permission", "term-eval-settings")
	addRecord(p.ID, "Write", `{"file_path":"/repo/.claude/settings.local.json","content":"{}"}`)
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "Bash commands only") {
		t.Fatalf("settings write: %q", why)
	}
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "edit_permission", "term-eval-edit")
	addRecord(p.ID, "Edit", `{"file_path":"/repo/README.md","old_string":"hi","new_string":"hello"}`)
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "Bash commands only") {
		t.Fatalf("plain edit: %q", why)
	}
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "write_permission", "term-eval-write")
	addRecord(p.ID, "Write", `{"file_path":"/repo/notes.txt","content":"hi"}`)
	if _, why := evaluateTerminalOneTap(p); !strings.Contains(why, "Bash commands only") {
		t.Fatalf("plain write: %q", why)
	}

	// Records exist but none matches the dialog: a distinct (logged) reason.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-eval-nomatch")
	addRecord(p.ID, "Bash", `{"command":"ls"}`)
	if _, why := evaluateTerminalOneTap(p); why == termReasonNoRecords || !strings.Contains(why, "matches the dialog") {
		t.Fatalf("non-matching record: %q", why)
	}

	// An idle pane whose raw tail still holds the answered menu: no.
	p = realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_cancelled", "term-eval-cancelled")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt"}`)
	if _, why := evaluateTerminalOneTap(p); why == "" {
		t.Fatal("answered menu offered for one-tap")
	}

	// Codex panes: always Review.
	claudeOnly(t, false)
	p = realScreenPane(t, "codex_0169_ro_100x40", "write_command", "term-eval-codex")
	if _, why := evaluateTerminalOneTap(p); why == "" {
		t.Fatal("Codex approval offered for one-tap")
	}
}

// captureWrites routes the pane's input to a channel.
func captureWrites(t *testing.T, p *Pane) <-chan []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	out := make(chan []byte, 8)
	go func() {
		for {
			_, payload, err := ReadFrame(c2)
			if err != nil {
				return
			}
			out <- payload
		}
	}()
	p.mu.Lock()
	p.agentConn = c1
	p.mu.Unlock()
	return out
}

// Not parallel: installs the signer, caller hook and process check.
func TestTerminalOneTapPushAndResolve(t *testing.T) {
	id := withTestApprovalSigner(t, "mach_term")
	claudeOnly(t, true)
	fakePaneTree(t)
	p := realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-push-resolve")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt"}`)

	// The push carries a signed promptId over the dialog text.
	bodies := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/notify" {
			b, _ := io.ReadAll(r.Body)
			bodies <- b
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	pusher := &Pusher{cloudBase: server.URL, token: "synthetic-credential", client: server.Client()}
	pusher.fire(p, string(StateChoicePrompt))
	var fields map[string]any
	select {
	case b := <-bodies:
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("push not delivered")
	}
	promptID, _ := fields["promptId"].(string)
	summary, _ := fields["summary"].(string)
	sig, _ := fields["approvalSig"].(string)
	issued, _ := fields["approvalIssuedAt"].(string)
	if promptID == "" || fields["requestId"] != nil || fields["toolName"] != "Bash" || fields["widensPermissions"] != false || sig == "" {
		t.Fatalf("push = %+v", fields)
	}
	if !verifyApprovalSig(t, id.spki, "mach_term", p.ID, promptID, "Bash", false, issued, summary, sig) {
		t.Fatal("terminal push signature does not verify")
	}

	a := &apiServer{mgr: testManagerWithPane(p)}
	writes := captureWrites(t, p)
	resolve := func(caller callerIdentity, device, body string) *httptest.ResponseRecorder {
		t.Helper()
		callAsIdentity(t, caller)
		req := httptest.NewRequest(http.MethodPost, "/api/panes/"+p.ID+"/choice-prompt/resolve", strings.NewReader(body))
		if device != "" {
			req = withRelayDevice(req, device)
		}
		req.SetPathValue("id", p.ID)
		rec := httptest.NewRecorder()
		a.handlePaneChoicePromptResolve(rec, req)
		return rec
	}
	person := callerIdentity{Kind: callerHuman, Relay: true}
	digest := liveTerminalDigest(p.ID, promptID)
	ok := `{"promptId":"` + promptID + `","approvalDigest":"` + digest + `","allow":true}`

	if rec := resolve(person, "dev_phone", `{"promptId":"`+promptID+`","allow":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no digest: %d %s", rec.Code, rec.Body.String())
	}
	if rec := resolve(callerIdentity{Kind: callerHuman}, "", ok); rec.Code != http.StatusForbidden {
		t.Fatalf("no device proof: %d %s", rec.Code, rec.Body.String())
	}
	if rec := resolve(person, "dev_phone", `{"promptId":"term:0:0","approvalDigest":"`+digest+`","allow":true}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "approval_changed") {
		t.Fatalf("stale prompt: %d %s", rec.Code, rec.Body.String())
	}
	if rec := resolve(person, "dev_phone", `{"promptId":"`+promptID+`","approvalDigest":"bogus","allow":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("wrong digest: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case b := <-writes:
		t.Fatalf("a refused one-tap wrote %q", b)
	case <-time.After(50 * time.Millisecond):
	}
	if rec := resolve(person, "dev_phone", ok); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resolved":true`) {
		t.Fatalf("matching one-tap: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case b := <-writes:
		if string(b) != "\r" {
			t.Fatalf("allow wrote %q, want Enter on option 1", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allow wrote nothing")
	}
	// A second tap on the same prompt (lock screen and app) writes nothing.
	if rec := resolve(person, "dev_phone", ok); rec.Code != http.StatusConflict {
		t.Fatalf("second tap: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case b := <-writes:
		t.Fatalf("second tap wrote %q", b)
	case <-time.After(50 * time.Millisecond):
	}
}

// A widening request gets no promptId, and the resolve refuses an allow.
func TestTerminalOneTapWideningIsReviewOnly(t *testing.T) {
	withTestApprovalSigner(t, "mach_term")
	claudeOnly(t, true)
	fakePaneTree(t)
	p := realScreenPane(t, "claude_2_1_322_sandbox_100x40", "sandbox_off_permission", "term-widen")
	addRecord(p.ID, "Bash", `{"command":"touch outside_probe.txt","dangerouslyDisableSandbox":true}`)
	bodies := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	(&Pusher{cloudBase: server.URL, token: "synthetic-credential", client: server.Client()}).fire(p, string(StateChoicePrompt))
	select {
	case b := <-bodies:
		if strings.Contains(string(b), "promptId") || strings.Contains(string(b), "approvalSig") || !strings.Contains(string(b), `"widensPermissions":true`) {
			t.Fatalf("widening push = %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("push not delivered")
	}
	// Even with a digest for this prompt, the daemon refuses an allow.
	t1, _ := evaluateTerminalOneTap(p)
	s := signTerminalOneTap(p.ID, t1)
	a := &apiServer{mgr: testManagerWithPane(p)}
	callAsIdentity(t, callerIdentity{Kind: callerHuman, Relay: true})
	req := withRelayDevice(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"promptId":"`+t1.PromptID+`","approvalDigest":"`+s.digest+`","allow":true}`)), "dev_phone")
	req.SetPathValue("id", p.ID)
	rec := httptest.NewRecorder()
	a.handlePaneChoicePromptResolve(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "review_required") {
		t.Fatalf("allow of a widening request: %d %s", rec.Code, rec.Body.String())
	}
}

// withRelayDevice marks req as arriving over the cloud relay with device's
// signature verified, as relayDeviceGate does.
func withRelayDevice(req *http.Request, device string) *http.Request {
	ctx := context.WithValue(req.Context(), relayTransportKey{}, true)
	return req.WithContext(context.WithValue(ctx, relayVerifiedDeviceKey{}, device))
}

// replaceGridLine replaces the grid line whose trimmed text is exactly old.
func replaceGridLine(grid, old, repl string) string {
	lines := strings.Split(grid, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == old {
			lines[i] = repl
			return strings.Join(lines, "\n")
		}
	}
	return grid
}

// Deny is Esc under the same binding.
func TestTerminalOneTapDenyWritesEsc(t *testing.T) {
	withTestApprovalSigner(t, "mach_term")
	claudeOnly(t, true)
	fakePaneTree(t)
	p := realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-deny")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt"}`)
	got, why := evaluateTerminalOneTap(p)
	if why != "" {
		t.Fatal(why)
	}
	s := signTerminalOneTap(p.ID, got)
	writes := captureWrites(t, p)
	a := &apiServer{mgr: testManagerWithPane(p)}
	callAsIdentity(t, callerIdentity{Kind: callerHuman, Relay: true})
	req := withRelayDevice(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"promptId":"`+got.PromptID+`","approvalDigest":"`+s.digest+`","allow":false}`)), "dev_phone")
	req.SetPathValue("id", p.ID)
	rec := httptest.NewRecorder()
	a.handlePaneChoicePromptResolve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case b := <-writes:
		if string(b) != "\x1b" {
			t.Fatalf("deny wrote %q, want Esc", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deny wrote nothing")
	}
}

// Input landing between the checks and the write (a desktop key the grid
// hasn't redrawn yet) cancels the one-tap: nothing is written.
func TestTerminalOneTapInputBeforeWriteCancels(t *testing.T) {
	withTestApprovalSigner(t, "mach_term")
	claudeOnly(t, true)
	fakePaneTree(t)
	p := realScreenPane(t, "claude_2_1_322_perm_100x40", "bash_permission", "term-race")
	addRecord(p.ID, "Bash", `{"command":"touch perm_probe.txt"}`)
	got, why := evaluateTerminalOneTap(p)
	if why != "" {
		t.Fatal(why)
	}
	s := signTerminalOneTap(p.ID, got)
	writes := captureWrites(t, p)
	prev := termResolveBeforeWrite
	// Another writer's key lands in the window: it bumps the input revision
	// under the pane's write lock, as every real input does.
	termResolveBeforeWrite = func(p *Pane) { p.inputRevision.Add(1) }
	t.Cleanup(func() { termResolveBeforeWrite = prev })
	a := &apiServer{mgr: testManagerWithPane(p)}
	callAsIdentity(t, callerIdentity{Kind: callerHuman, Relay: true})
	req := withRelayDevice(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"promptId":"`+got.PromptID+`","approvalDigest":"`+s.digest+`","allow":true}`)), "dev_phone")
	req.SetPathValue("id", p.ID)
	rec := httptest.NewRecorder()
	a.handlePaneChoicePromptResolve(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("input before write: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case b := <-writes:
		t.Fatalf("wrote %q after input landed", b)
	case <-time.After(50 * time.Millisecond):
	}
}

// A pane with no hook records (every Codex or plain-shell pane at a menu) is
// declined silently; other declines are logged. Not parallel: swaps log output.
func TestTermOneTapDeclineLogIsQuietForPanesWithoutRecords(t *testing.T) {
	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	logTermOneTapDecline("p-codex", termReasonNoRecords)
	if buf.Len() != 0 {
		t.Fatalf("logged a no-records decline: %q", buf.String())
	}
	logTermOneTapDecline("p-claude", "focus is not on option 1 (Yes)")
	if !strings.Contains(buf.String(), "p-claude") {
		t.Fatalf("a real decline was not logged: %q", buf.String())
	}
}
