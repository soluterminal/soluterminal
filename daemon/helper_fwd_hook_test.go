package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Session ids as Claude Code writes them (lowercase UUIDs).
const (
	sidWSL    = "11111111-2222-4333-8444-555555555555"
	sidOther  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	sidShared = "99999999-8888-4777-8666-555555555555"
	sidFresh  = "12345678-1234-4123-8123-123456789abc"
)

func helperHookAPI(t *testing.T) (*apiServer, *Pane, *Pane) {
	t.Helper()
	realCallerClassification(t)
	dir := t.TempDir()
	store, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(dir)
	wsl := transactionTestPane(pid0Pane)
	wsl.Status = PaneRunning
	wsl.Cwd = "/home/u/repo"
	other := transactionTestPane(pid0Other)
	other.Status = PaneRunning
	other.Cwd = "/home/u/repo"
	other.ClaudeSessionID = sidOther
	other.ClaudeTranscriptPath = "/home/u/.claude/projects/-repo/" + sidOther + ".jsonl"
	mgr.panes[wsl.ID] = wsl
	mgr.panes[other.ID] = other
	return &apiServer{mgr: mgr, panes: store, uiBus: newUIBus()}, wsl, other
}

// A forwarded hook acts on its attested pane only: a pane header in the
// request (which the fwd path never forwards anyway) and the cwd fallback
// play no part.
func TestHelperHookPinsTheAttestedPane(t *testing.T) {
	api, wsl, other := helperHookAPI(t)
	body := `{"hook_event_name":"SessionStart","session_id":"` + sidWSL + `","transcript_path":"/home/u/.claude/projects/-repo/` + sidWSL + `.jsonl","cwd":"/home/u/repo"}`
	r := helperRequest("POST", "/api/hooks/tool-call", body)
	r.Header.Set("X-Soluterminal-Pane", other.ID)
	rec := httptest.NewRecorder()
	api.hookToolCall(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := wsl.Snapshot().ClaudeSessionID; got != sidWSL {
		t.Fatalf("attested pane session %q", got)
	}
	if got := other.Snapshot().ClaudeSessionID; got != sidOther {
		t.Fatalf("other pane changed to %q", got)
	}
}

// 8.6a.3: a session or transcript another running pane holds is refused, with
// no effect on either pane.
func TestHelperHookNamingAnotherPaneIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"session id": `{"hook_event_name":"SessionStart","session_id":"` + sidOther + `","transcript_path":"/home/u/.claude/projects/-repo/x.jsonl"}`,
		"transcript": `{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"` + sidFresh + `","transcript_path":"/home/u/.claude/projects/-repo/` + sidOther + `.jsonl"}`,
	} {
		t.Run(name, func(t *testing.T) {
			api, wsl, other := helperHookAPI(t)
			rec := httptest.NewRecorder()
			api.hookToolCall(rec, helperRequest("POST", "/api/hooks/tool-call", body))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d", rec.Code)
			}
			if wsl.Snapshot().ClaudeSessionID != "" || wsl.Snapshot().ClaudeTranscriptPath != "" {
				t.Fatal("the attested pane took another pane's session")
			}
			if other.Snapshot().ClaudeSessionID != sidOther {
				t.Fatal("the other pane changed")
			}
		})
	}
	// An exited pane can still resume its session, so it stays that pane's:
	// a WSL pane naming it is refused and the exited pane keeps its id.
	api, wsl, other := helperHookAPI(t)
	other.Status = PaneExited
	rec := httptest.NewRecorder()
	api.hookToolCall(rec, helperRequest("POST", "/api/hooks/tool-call", `{"hook_event_name":"SessionStart","session_id":"`+sidOther+`"}`))
	if rec.Code != http.StatusForbidden || wsl.Snapshot().ClaudeSessionID != "" || other.Snapshot().ClaudeSessionID != sidOther {
		t.Fatalf("exited pane's session: status %d, wsl %q, other %q", rec.Code, wsl.Snapshot().ClaudeSessionID, other.Snapshot().ClaudeSessionID)
	}
}

// Without the fwd context key, a non-loopback request is still refused.
func TestHookNonLoopbackStillRefused(t *testing.T) {
	api, _, _ := helperHookAPI(t)
	r := httptest.NewRequest("POST", "/api/hooks/tool-call", nil)
	r.RemoteAddr = helperFwdRemoteAddr
	rec := httptest.NewRecorder()
	api.hookToolCall(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
}

// Concurrent hooks from two WSL panes naming one new session: the check and
// the pin are one step, so exactly one pane ends up holding it.
func TestHelperHooksRaceForOneSession(t *testing.T) {
	for round := 0; round < 20; round++ {
		api, a, b := helperHookAPI(t)
		b.ClaudeSessionID, b.ClaudeTranscriptPath = "", ""
		body := `{"hook_event_name":"SessionStart","session_id":"` + sidShared + `","transcript_path":"/home/u/.claude/projects/-repo/` + sidShared + `.jsonl"}`
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			pane := a.ID
			if i%2 == 1 {
				pane = b.ID
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := helperRequest("POST", "/api/hooks/tool-call", body)
				r = r.WithContext(context.WithValue(r.Context(), helperFwdCallerKey{}, helperFwdCaller{PaneID: pane, AgentID: "0123456789abcdef0123456789abcdef", Distro: "Ubuntu"}))
				api.hookToolCall(httptest.NewRecorder(), r)
			}()
		}
		wg.Wait()
		holders := 0
		for _, p := range []*Pane{a, b} {
			if p.Snapshot().ClaudeSessionID == sidShared {
				holders++
			}
		}
		if holders != 1 {
			t.Fatalf("round %d: %d panes hold the session", round, holders)
		}
	}
}

// A forwarded hook pins the session, never its transcript path (a file in the
// distro that the host would open on its own filesystem), and records no
// touched files.
func TestHelperHookPinsNoHostPaths(t *testing.T) {
	api, wsl, _ := helperHookAPI(t)
	api.recentFiles = newRecentFilesRing()
	body := `{"hook_event_name":"SessionStart","session_id":"` + sidWSL + `","transcript_path":"C:/Users/someone/.claude/projects/-r/` + sidWSL + `.jsonl"}`
	api.hookToolCall(httptest.NewRecorder(), helperRequest("POST", "/api/hooks/tool-call", body))
	if got := wsl.Snapshot().ClaudeSessionID; got != sidWSL {
		t.Fatalf("session %q", got)
	}
	if got := wsl.Snapshot().ClaudeTranscriptPath; got != "" {
		t.Fatalf("a forwarded hook pinned transcript path %q", got)
	}
	edit := `{"hook_event_name":"PostToolUse","tool_name":"Edit","session_id":"` + sidWSL + `","cwd":"/home/u/repo","tool_input":{"file_path":"C:/Windows/win.ini"}}`
	api.hookToolCall(httptest.NewRecorder(), helperRequest("POST", "/api/hooks/tool-call", edit))
	if n := len(api.recentFiles.recent(100)); n != 0 {
		t.Fatalf("a forwarded hook recorded %d touched files", n)
	}
}

// Only a well-formed (lowercase UUID) session id is accepted from a WSL pane:
// it later names transcript files and --resume arguments on this host.
func TestHelperHookMalformedSessionIDRefused(t *testing.T) {
	for _, sid := range []string{"../x", "../../Windows/win", "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", "11111111-2222-4333-8444-55555555555", "x", "11111111/2222-4333-8444-555555555555"} {
		api, wsl, _ := helperHookAPI(t)
		api.ccLedger = newInAppLedger(t.TempDir())
		body, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": sid})
		rec := httptest.NewRecorder()
		api.hookToolCall(rec, helperRequest("POST", "/api/hooks/tool-call", string(body)))
		if rec.Code != http.StatusBadRequest || wsl.Snapshot().ClaudeSessionID != "" {
			t.Fatalf("%q: status %d, pinned %q", sid, rec.Code, wsl.Snapshot().ClaudeSessionID)
		}
		if _, known := api.ccLedger.Lookup(sid); known {
			t.Fatalf("%q reached the usage ledger", sid)
		}
	}
	for _, sid := range []string{sidWSL, sidFresh} {
		if !claudeSessionIDWellFormed(sid) {
			t.Fatalf("%q refused", sid)
		}
	}
}

// The usage ledger is written for a forwarded hook only once the locked
// recheck has passed, with no host account; a refused hook leaves no trace.
func TestHelperHookLedgerAfterRecheck(t *testing.T) {
	api, _, _ := helperHookAPI(t)
	api.ccLedger = newInAppLedger(t.TempDir())
	rec := httptest.NewRecorder()
	api.hookToolCall(rec, helperRequest("POST", "/api/hooks/tool-call", `{"hook_event_name":"SessionStart","session_id":"`+sidOther+`"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	if _, known := api.ccLedger.Lookup(sidOther); known {
		t.Fatal("a refused forwarded hook wrote the usage ledger")
	}
	rec = httptest.NewRecorder()
	api.hookToolCall(rec, helperRequest("POST", "/api/hooks/tool-call", `{"hook_event_name":"SessionStart","session_id":"`+sidWSL+`"}`))
	if inApp, known := api.ccLedger.Lookup(sidWSL); rec.Code != 200 || !known || !inApp {
		t.Fatalf("accepted hook: status %d, ledger known=%t inApp=%t", rec.Code, known, inApp)
	}
	if acct := api.ccLedger.LookupAccount(sidWSL); acct != "" {
		t.Fatalf("a WSL session was attributed to host account %q", acct)
	}
}
