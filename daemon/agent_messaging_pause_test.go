package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pauseAgentMessagingForTest pauses agent messaging for one test and lifts it
// (and the resume clock) afterwards.
func pauseAgentMessagingForTest(t *testing.T) {
	t.Helper()
	if _, err := setAgentMessagingPaused("", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = setAgentMessagingPaused("", false, time.Now())
		agentMessaging.resumedAt.Store(0)
	})
}

func resumeAgentMessagingForTest(t *testing.T) {
	t.Helper()
	if _, err := setAgentMessagingPaused("", false, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestAgentMessagingLoadFailsOpen(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	for _, tc := range []struct {
		name    string
		content *string
		paused  bool
	}{
		{"missing file", nil, false},
		{"corrupt file", strPtr("{paused: tru"), false},
		{"empty file", strPtr(""), false},
		{"wrong type", strPtr(`{"paused":"yes"}`), false},
		{"saved resume", strPtr(`{"paused":false}`), false},
		{"saved pause", strPtr(`{"paused":true,"since":1700000000000}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.content != nil {
				if err := os.WriteFile(filepath.Join(dir, agentMessagingFile), []byte(*tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			loadAgentMessaging(dir)
			if got := agentMessagingPaused(); got != tc.paused {
				t.Fatalf("paused = %v, want %v", got, tc.paused)
			}
		})
	}
	// A directory where the file should be cannot be read: messaging is on.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, agentMessagingFile), 0700); err != nil {
		t.Fatal(err)
	}
	loadAgentMessaging(dir)
	if agentMessagingPaused() {
		t.Fatal("an unreadable setting must read as messaging on")
	}
}


func TestAgentMessagingPauseIsSavedAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { loadAgentMessaging("") })
	if changed, err := setAgentMessagingPaused(dir, true, time.Now()); err != nil || !changed {
		t.Fatalf("pause = %v, %v", changed, err)
	}
	loadAgentMessaging("")
	if agentMessagingPaused() {
		t.Fatal("control: loading no directory must clear the flag")
	}
	loadAgentMessaging(dir)
	if !agentMessagingPaused() || agentMessagingSnapshot().Since == 0 {
		t.Fatalf("after a restart: %+v, want paused with its start time", agentMessagingSnapshot())
	}
	if changed, err := setAgentMessagingPaused(dir, false, time.Now()); err != nil || !changed {
		t.Fatalf("resume = %v, %v", changed, err)
	}
	loadAgentMessaging(dir)
	if agentMessagingPaused() {
		t.Fatal("a saved resume must load as messaging on")
	}
}

func TestAgentMessagingPauseThatCannotBeSavedStillAppliesAndWarns(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	dir := filepath.Join(t.TempDir(), "missing", "dir")
	if changed, err := setAgentMessagingPaused(dir, true, time.Now()); err == nil || !changed {
		t.Fatalf("pause into a missing directory = %v, %v; want applied with a save error", changed, err)
	}
	if !agentMessagingPaused() {
		t.Fatal("a stop must apply even when its setting cannot be saved")
	}
	if w := agentMessagingWarning(); w != agentMessagingPauseUnsaved {
		t.Fatalf("warning = %q, want the unsaved-pause warning", w)
	}
	// Once the directory exists, asking again saves it and clears the warning.
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := setAgentMessagingPaused(dir, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := agentMessagingWarning(); w != "" {
		t.Fatalf("warning after a good save = %q", w)
	}
	if _, err := os.Stat(filepath.Join(dir, agentMessagingFile)); err != nil {
		t.Fatalf("retried pause not saved: %v", err)
	}
}

func TestAgentMessagingResumeThatCannotBeSavedStillAppliesAndWarns(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	dir := t.TempDir()
	if _, err := setAgentMessagingPaused(dir, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The file's place becomes unwritable: a directory sits where the
	// temporary file would be renamed over.
	if err := os.Remove(filepath.Join(dir, agentMessagingFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, agentMessagingFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentMessagingFile, "x"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := setAgentMessagingPaused(dir, false, time.Now()); err == nil || !changed {
		t.Fatalf("resume = %v, %v; want applied with a save error", changed, err)
	}
	if agentMessagingPaused() {
		t.Fatal("a resume must apply even when its setting cannot be saved")
	}
	if w := agentMessagingWarning(); w != agentMessagingResumeUnsaved {
		t.Fatalf("warning = %q, want the unsaved-resume warning", w)
	}
}

func TestUnreadableSavedSettingIsShownToThePerson(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, agentMessagingFile), []byte("{paused: tru"), 0600); err != nil {
		t.Fatal(err)
	}
	loadAgentMessaging(dir)
	if agentMessagingPaused() || agentMessagingWarning() != agentMessagingUnreadableWarning {
		t.Fatalf("paused=%v warning=%q, want on with the unreadable notice", agentMessagingPaused(), agentMessagingWarning())
	}
	a := &apiServer{dataDir: dir}
	if body := a.agentMessagingBody(); body["warning"] != agentMessagingUnreadableWarning {
		t.Fatalf("GET body = %+v, want the notice", body)
	}
	// A missing file is the default, not something to warn about.
	loadAgentMessaging(t.TempDir())
	if w := agentMessagingWarning(); w != "" {
		t.Fatalf("missing file warned: %q", w)
	}
}

func TestPausedAgentMessageIsHeldAndGoesInOnResume(t *testing.T) {
	p, frames, _ := newDraftGuardPane(t, "paused-held")
	p.noteHumanInput(time.Now().Add(-time.Minute)) // idle, empty box
	pauseAgentMessagingForTest(t)
	if got := <-agentDelivery(p, codexProbeMessage, "@lead"); got != deliveryHeld {
		t.Fatalf("outcome = %q, want held", got)
	}
	if reason := p.heldDeliveryReason(); reason != agentMessagingPausedReason {
		t.Fatalf("reason = %q, want %q", reason, agentMessagingPausedReason)
	}
	held := p.Snapshot().HeldMessages
	if len(held) != 1 || held[0].Sender != "@lead" || held[0].Text != "queued probe: say CHERRY" {
		t.Fatalf("held = %+v, want the sender and text visible", held)
	}
	expectNoFrame(t, frames, "nothing typed while paused")

	resumeAgentMessagingForTest(t)
	expectFrame(t, frames, "held body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "Enter", "\r")
	waitHeldEmpty(t, p)
	if status := heldStatusOf(p, codexProbeMessage); status != "delivered" {
		t.Fatalf("status = %q, want delivered", status)
	}
}

func TestPausedHeldMessageNeverExpiresAndGetsAFreshWait(t *testing.T) {
	prev := heldDeliveryLimit
	heldDeliveryLimit = 40 * time.Millisecond
	t.Cleanup(func() { heldDeliveryLimit = prev })
	p, frames, _ := newDraftGuardPane(t, "paused-expiry")
	p.noteHumanInput(time.Now().Add(-time.Minute))
	pauseAgentMessagingForTest(t)
	<-agentDelivery(p, codexProbeMessage, "@lead")
	time.Sleep(4 * heldDeliveryLimit)
	if held := p.Snapshot().HeldMessages; len(held) != 1 {
		t.Fatalf("held = %+v: a paused message must not expire", held)
	}
	resumeAgentMessagingForTest(t)
	expectFrame(t, frames, "held body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "Enter", "\r")
	waitHeldEmpty(t, p)
	if status := heldStatusOf(p, codexProbeMessage); status != "delivered" {
		t.Fatalf("status = %q, want delivered (not expired) after resume", status)
	}
}

func TestSendNowWhilePausedIsLoggedWithoutText(t *testing.T) {
	logDir := t.TempDir()
	coordLog.mu.Lock()
	prevDir := coordLog.dir
	coordLog.dir = filepath.Join(logDir, "coordination")
	coordLog.mu.Unlock()
	t.Cleanup(func() {
		coordLog.mu.Lock()
		coordLog.dir = prevDir
		coordLog.mu.Unlock()
	})
	p, frames, _ := newDraftGuardPane(t, "paused-send-now")
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	p.noteHumanInput(time.Now().Add(-time.Minute))
	pauseAgentMessagingForTest(t)
	<-agentDelivery(p, codexProbeMessage, "@lead")
	h := p.heldDeliveryByID(p.Snapshot().HeldMessages[0].ID)
	sent := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		a.actOnHeldDelivery(rec, p, h, "send")
		sent <- rec
	}()
	expectFrame(t, frames, "body", "\x1b[200~"+codexProbeMessage+"\x1b[201~")
	expectFrame(t, frames, "Enter", "\r")
	if rec := <-sent; rec.Code != http.StatusOK {
		t.Fatalf("send now while paused = %d %s", rec.Code, rec.Body)
	}
	raw, err := os.ReadFile(filepath.Join(logDir, "coordination", coordinationLogFile))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev coordinationEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if ev.Kind == "held" && ev.Outcome == "sent_while_paused" && ev.By == "@lead" && ev.Pane == p.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no sent_while_paused event in %s", raw)
	}
	if strings.Contains(string(raw), "CHERRY") {
		t.Fatalf("the log must not carry the message text: %s", raw)
	}
}

func TestHeldNoticeNamesThePause(t *testing.T) {
	got := heldNotice("pane_x", agentMessagingPausedReason)
	for _, want := range []string{"paused agent messaging", "pane_x", "does not expire", "Do not resend"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice %q lacks %q", got, want)
		}
	}
}

func callToolForTest(t *testing.T, a *apiServer, caller, name string, args map[string]any) (string, bool) {
	t.Helper()
	b, err := json.Marshal(toolCallParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return mcpResultText(t, a.mcpCallTool(b, nil, caller))
}

func TestPausedAgentMessagingRefusesWhatCannotBeHeld(t *testing.T) {
	a := st10Server(t, true)
	pauseAgentMessagingForTest(t)
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"keys to an agent", "send_input", map[string]any{"pane_id": "B", "text": "\x1b"}},
		{"text to a shell", "send_input", map[string]any{"pane_id": "S", "text": "ls\n"}},
		{"ask a shell", "ask_pane", map[string]any{"pane_id": "S", "text": "hello"}},
		{"answer a prompt", "answer_prompt", map[string]any{"pane_id": "A", "choice": "1"}},
		{"spawn", "spawn_pane", map[string]any{"cmd": "claude", "title": "w"}},
		{"spawn auto", "spawn_pane", map[string]any{"cmd": "auto", "title": "w"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := callToolForTest(t, a, "lead", tc.tool, tc.args)
			if !isErr || !strings.Contains(text, "agent messaging is paused by the person") {
				t.Fatalf("%s = %v %q, want the pause refusal", tc.tool, isErr, text)
			}
		})
	}
	text, isErr := callToolForTest(t, a, "lead", "list_panes", map[string]any{})
	if isErr || !strings.Contains(text, `"agent_messaging_paused":true`) {
		t.Fatalf("list_panes = %v %q, want the paused flag", isErr, text)
	}
	resumeAgentMessagingForTest(t)
	text, _ = callToolForTest(t, a, "lead", "list_panes", map[string]any{})
	if strings.Contains(text, "agent_messaging_paused") {
		t.Fatalf("list_panes after resume still says paused: %q", text)
	}
}

func TestAgentMessagingRouteIsThePersonsOnly(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	dir := t.TempDir()
	a := &apiServer{dataDir: dir}
	mux := http.NewServeMux()
	a.routes(mux)
	prev := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = prev })
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/agent-messaging", strings.NewReader(body)))
		return rec
	}

	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "agent-pane"} }
	if rec := put(`{"paused":true}`); rec.Code < 400 {
		t.Fatalf("an agent paused messaging: %d %s", rec.Code, rec.Body)
	}
	if agentMessagingPaused() {
		t.Fatal("an agent's request must change nothing")
	}

	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	if rec := put(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing paused = %d, want 400", rec.Code)
	}
	rec := put(`{"paused":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"paused":true`) || !agentMessagingPaused() {
		t.Fatalf("the person's pause = %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, agentMessagingFile)); err != nil {
		t.Fatalf("pause not saved: %v", err)
	}

	// The agent cannot lift it either.
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "agent-pane"} }
	if rec := put(`{"paused":false}`); rec.Code < 400 || !agentMessagingPaused() {
		t.Fatalf("an agent resumed messaging: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/agent-messaging", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"paused":true`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}

	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	if rec := put(`{"paused":false}`); rec.Code != http.StatusOK || agentMessagingPaused() {
		t.Fatalf("the person's resume = %d %s", rec.Code, rec.Body)
	}

	// A pause whose save fails is still a pause: 200, paused, with the warning.
	a.dataDir = filepath.Join(dir, "missing")
	rec = put(`{"paused":true}`)
	if rec.Code != http.StatusOK || !agentMessagingPaused() || !strings.Contains(rec.Body.String(), `"paused":true`) || !strings.Contains(rec.Body.String(), "restarting SoluTerminal would lift the pause") {
		t.Fatalf("unsaved pause = %d %s", rec.Code, rec.Body)
	}
}

func TestAgentScheduleIsHeldWhilePausedAndRunsOnResume(t *testing.T) {
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	agentSched, err := s.CreateWithOrigin(ScheduleInput{Name: "agent nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	personSched, err := s.Create(ScheduleInput{Name: "my nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	pauseAgentMessagingForTest(t)

	s.fire(agentSched.ID)
	got, _ := s.Get(agentSched.ID)
	if got.Enabled || got.PausedReason != agentMessagingScheduleReason || len(got.Runs) != 0 {
		t.Fatalf("agent schedule while paused: enabled=%v reason=%q runs=%+v, want held without a run", got.Enabled, got.PausedReason, got.Runs)
	}
	if !s.NextFire(agentSched.ID).IsZero() {
		t.Fatal("a held schedule must not stay armed")
	}
	// The person's own schedule is not agent-to-agent: it runs as usual
	// (here into a closed pane, which errors).
	s.fire(personSched.ID)
	if got, _ := s.Get(personSched.ID); len(got.Runs) != 1 || got.PausedReason != targetPaneGoneReason {
		t.Fatalf("person's schedule = %+v, want it to run", got.Runs)
	}

	resumeAgentMessagingForTest(t)
	s.resumeAgentMessagingHeld(0)
	waitUntil(t, "the held schedule to run on resume", func() bool {
		got, _ := s.Get(agentSched.ID)
		return len(got.Runs) == 1
	})
	got, _ = s.Get(agentSched.ID)
	if got.PausedReason == agentMessagingScheduleReason {
		t.Fatalf("reason still %q after resume", got.PausedReason)
	}
}

func TestAskPaneWhilePausedIsHeldWithThePauseHint(t *testing.T) {
	p, frames, _ := newDraftGuardPane(t, "pane_paused_ask")
	p.noteHumanInput(time.Now().Add(-time.Minute))
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	pauseAgentMessagingForTest(t)
	text, isErr := mcpResultText(t, a.mcpToolAskPane(map[string]any{"pane_id": "pane_paused_ask", "text": codexProbeMessage, "timeout_seconds": 0.3}, "pane_caller"))
	if isErr {
		t.Fatalf("ask_pane refused: %s", text)
	}
	var res collabResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "still_working" || res.Delivery != "held" || !strings.HasPrefix(res.Hint, "held: the person has paused agent messaging") {
		t.Fatalf("ask_pane = %+v", res)
	}
	expectNoFrame(t, frames, "nothing typed while paused")
}

func TestPersonsRunNowReleasesAHeldAgentSchedule(t *testing.T) {
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	sched, err := s.CreateWithOrigin(ScheduleInput{Name: "agent nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	pauseAgentMessagingForTest(t)
	s.fire(sched.ID)
	if got, _ := s.Get(sched.ID); got.PausedReason != agentMessagingScheduleReason {
		t.Fatalf("control: reason = %q, want held", got.PausedReason)
	}
	if _, err := s.FireNow(sched.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(sched.ID)
	// It ran (into a closed pane, which errors and pauses it for that reason).
	if len(got.Runs) != 1 || got.PausedReason == agentMessagingScheduleReason {
		t.Fatalf("after Run now: runs=%+v reason=%q, want one run and the hold released", got.Runs, got.PausedReason)
	}
}

func TestInjectFromAnotherPaneIsRefusedWhilePaused(t *testing.T) {
	a := st10Server(t, true)
	prev := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = prev })
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "lead"} }
	pauseAgentMessagingForTest(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/S/inject", strings.NewReader(`{"text":"ls\r"}`))
	req.SetPathValue("id", "S")
	a.injectPane(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "agent messaging is paused") {
		t.Fatalf("inject by another pane while paused = %d %s", rec.Code, rec.Body)
	}
}

func TestUpdateWaitsWhilePausedMessagesAreHeld(t *testing.T) {
	p, _, _ := newDraftGuardPane(t, "paused-update")
	p.noteHumanInput(time.Now().Add(-time.Minute))
	a := &apiServer{mgr: testManagerWithPane(p), uiBus: newUIBus()}
	pauseAgentMessagingForTest(t)
	<-agentDelivery(p, codexProbeMessage, "@lead")
	busy := a.updateBusy()
	if busy == nil || busy.cause != updateBusyCauseMessagingPaused {
		t.Fatalf("updateBusy = %+v, want the paused-messages cause (no collaboration grace)", busy)
	}
	// The person discards it: nothing is held, so nothing blocks the update.
	rec := httptest.NewRecorder()
	a.actOnHeldDelivery(rec, p, p.heldDeliveryByID(p.Snapshot().HeldMessages[0].ID), "clear")
	if busy := a.updateBusy(); busy != nil && busy.cause == updateBusyCauseMessagingPaused {
		t.Fatalf("updateBusy after discard = %+v", busy)
	}
}

func TestConditionalSubmitFromAnotherPaneIsRefusedWhilePaused(t *testing.T) {
	_, _, conn, req, mux := conditionalFixture(t)
	prev := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = prev })
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerPane, PaneID: "other-pane"} }
	pauseAgentMessagingForTest(t)
	rec, result := conditionalRequest(t, mux, "POST", "/api/panes/conditional-pane/conditional-submit", req)
	if rec.Code != http.StatusConflict || result["reason"] != "agent_messaging_paused" || len(conn.data()) != 0 {
		t.Fatalf("conditional-submit while paused = %d %v", rec.Code, result)
	}
}

func TestDisablingAHeldAgentScheduleKeepsItOffAfterResume(t *testing.T) {
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	sched, err := s.CreateWithOrigin(ScheduleInput{Name: "agent nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	pauseAgentMessagingForTest(t)
	s.fire(sched.ID)
	if _, err := s.SetEnabled(sched.ID, false); err != nil {
		t.Fatal(err)
	}
	resumeAgentMessagingForTest(t)
	s.resumeAgentMessagingHeld(0)
	time.Sleep(50 * time.Millisecond)
	got, _ := s.Get(sched.ID)
	if got.Enabled || len(got.Runs) != 0 || got.PausedReason != "" {
		t.Fatalf("after the person's Disable and a resume: enabled=%v runs=%+v reason=%q, want off and not run", got.Enabled, got.Runs, got.PausedReason)
	}
}

func TestDelayedReleaseKeepsTheHoldUntilItRuns(t *testing.T) {
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	sched, err := s.CreateWithOrigin(ScheduleInput{Name: "agent nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	pauseAgentMessagingForTest(t)
	s.fire(sched.ID)
	resumeAgentMessagingForTest(t)
	s.resumeAgentMessagingHeld(80 * time.Millisecond)
	if got, _ := s.Get(sched.ID); got.PausedReason != agentMessagingScheduleReason {
		t.Fatalf("reason before the delay = %q, want still held (a restart now must not lose the run)", got.PausedReason)
	}
	waitUntil(t, "the delayed release to run it", func() bool {
		got, _ := s.Get(sched.ID)
		return len(got.Runs) == 1
	})
}

func TestUpdateStatusNamesThePausedMessagesWait(t *testing.T) {
	u := &Updater{}
	busy := &updateBusyError{cause: updateBusyCauseMessagingPaused, reason: "agent messaging is paused and messages are held on pane p1; resume, or send or discard them, to update"}
	if _, waiting := u.noteAutoApplyWait(fmt.Errorf("apply: %w", busy)); !waiting {
		t.Fatal("a busy refusal should count as waiting")
	}
	if u.status.IdleWaitCause != updateBusyCauseMessagingPaused || !strings.Contains(u.status.IdleWaitReason, "agent messaging is paused") {
		t.Fatalf("status = %q %q, want the paused cause named", u.status.IdleWaitCause, u.status.IdleWaitReason)
	}
	u.noteAutoApplyWait(nil)
	if u.status.IdleWaitCause != "" || u.status.IdleWaitReason != "" {
		t.Fatalf("status after a clean pass = %q %q, want cleared", u.status.IdleWaitCause, u.status.IdleWaitReason)
	}
}

func TestUnreadableSettingKeepsHeldSchedulesUntilThePersonAnswers(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	dir := t.TempDir()
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	sched, err := s.CreateWithOrigin(ScheduleInput{Name: "agent nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	pauseAgentMessagingForTest(t)
	s.fire(sched.ID) // held by the pause
	// The daemon restarts and finds the saved setting damaged.
	if err := os.WriteFile(filepath.Join(dir, agentMessagingFile), []byte("{paused: tru"), 0600); err != nil {
		t.Fatal(err)
	}
	prevDelay := agentMessagingBootDelay
	agentMessagingBootDelay = 10 * time.Millisecond
	t.Cleanup(func() { agentMessagingBootDelay = prevDelay })
	a := &apiServer{dataDir: dir, scheds: s}
	bootAgentMessagingForTest(a, dir)
	time.Sleep(60 * time.Millisecond)
	if got, _ := s.Get(sched.ID); got.PausedReason != agentMessagingScheduleReason || len(got.Runs) != 0 {
		t.Fatalf("after a boot with an unreadable setting: reason=%q runs=%+v, want still held", got.PausedReason, got.Runs)
	}

	// The person dismisses the notice: the setting is saved as on, the notice
	// clears, and the held schedule is released.
	mux := http.NewServeMux()
	a.routes(mux)
	prev := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = prev })
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/agent-messaging", strings.NewReader(`{"paused":false}`)))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "warning") {
		t.Fatalf("dismiss = %d %s, want 200 without a warning", rec.Code, rec.Body)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, agentMessagingFile)); err != nil || !strings.Contains(string(raw), `"paused":false`) {
		t.Fatalf("setting after dismiss = %q %v, want saved as on", raw, err)
	}
	waitUntil(t, "the held schedule to run after the dismiss", func() bool {
		got, _ := s.Get(sched.ID)
		return len(got.Runs) == 1
	})
}

func TestReadableSettingReleasesHeldSchedulesAtBoot(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	sched, err := s.CreateWithOrigin(ScheduleInput{Name: "agent nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	pauseAgentMessagingForTest(t)
	s.fire(sched.ID)
	prevDelay := agentMessagingBootDelay
	agentMessagingBootDelay = 10 * time.Millisecond
	t.Cleanup(func() { agentMessagingBootDelay = prevDelay })
	bootAgentMessagingForTest(&apiServer{scheds: s}, t.TempDir()) // no file: on, no notice
	waitUntil(t, "the held schedule to run after boot", func() bool {
		got, _ := s.Get(sched.ID)
		return len(got.Runs) == 1
	})
}

// bootAgentMessagingForTest is the bootStores step (agent_messaging_pause.go
// init), which keeps its two call sites in the shape the autonomy registry
// already lists.
func bootAgentMessagingForTest(a *apiServer, dataDir string) {
	loadAgentMessaging(dataDir)
	if agentMessagingReleasesAtBoot() {
		a.resumeAgentMessagingHeld(agentMessagingBootDelay)
	}
}

func TestFailedDismissReleasesNothingAndARetryDoes(t *testing.T) {
	t.Cleanup(func() { loadAgentMessaging("") })
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewScheduleStore(t.TempDir(), NewManager(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	sched, err := s.CreateWithOrigin(ScheduleInput{Name: "agent nudge", PaneID: "pane-closed", Prompt: "keep going", Cron: "0 9 * * *"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	pauseAgentMessagingForTest(t)
	s.fire(sched.ID)
	// Unreadable: a directory stands where the file should be, so saving
	// over it fails too.
	if err := os.Mkdir(filepath.Join(dir, agentMessagingFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentMessagingFile, "x"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{dataDir: dir, scheds: s}
	bootAgentMessagingForTest(a, dir)
	mux := http.NewServeMux()
	a.routes(mux)
	prev := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = prev })
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	dismiss := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/agent-messaging", strings.NewReader(`{"paused":false}`)))
		return rec
	}

	rec := dismiss()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"warningKind":"unreadable"`) {
		t.Fatalf("failed dismiss = %d %s, want the unreadable notice kept (so Dismiss can be retried)", rec.Code, rec.Body)
	}
	time.Sleep(50 * time.Millisecond)
	if got, _ := s.Get(sched.ID); got.PausedReason != agentMessagingScheduleReason || len(got.Runs) != 0 {
		t.Fatalf("after a failed dismiss: reason=%q runs=%+v, want still held", got.PausedReason, got.Runs)
	}

	// The file's place is fixed; the retry saves and releases.
	if err := os.RemoveAll(filepath.Join(dir, agentMessagingFile)); err != nil {
		t.Fatal(err)
	}
	if rec := dismiss(); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "warning") {
		t.Fatalf("retried dismiss = %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "the held schedule to run after the retried dismiss", func() bool {
		got, _ := s.Get(sched.ID)
		return len(got.Runs) == 1
	})
}
