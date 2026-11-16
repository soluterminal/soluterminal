package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scheduleChangeFixture is traceTestServer (panes lead, worker, social,
// rogue, kid) with three schedules: one the person made in the app, one the
// lead pane made, and one made by a caller that was no verified pane.
func scheduleChangeFixture(t *testing.T) (a *apiServer, persons, leads, anon *Schedule) {
	t.Helper()
	a = traceTestServer(t)
	a.scheds = newTestScheduleStore(t)
	in := func(name string) ScheduleInput {
		return ScheduleInput{Name: name, Cron: "0 9 * * *", PaneID: "social", Prompt: "hello"}
	}
	var err error
	if persons, err = a.scheds.Create(in("person's")); err != nil {
		t.Fatal(err)
	}
	if leads, err = a.scheds.CreateWithOrigin(in("lead's"), &ScheduleOrigin{Source: "mcp", PaneID: "lead"}); err != nil {
		t.Fatal(err)
	}
	if anon, err = a.scheds.CreateWithOrigin(in("anon's"), &ScheduleOrigin{Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	return a, persons, leads, anon
}

func tracedPaneCaller(id string) *callerIdentity {
	return &callerIdentity{Kind: callerPane, PaneID: id, PID: 4242}
}

// mustStayOn fails unless the schedule still exists, enabled, with no record.
func mustStayOn(t *testing.T, s *ScheduleStore, id, after string) {
	t.Helper()
	got, ok := s.Get(id)
	if !ok {
		t.Fatalf("%s: schedule %s was deleted", after, id)
	}
	if !got.Enabled || len(got.Changes) != 0 {
		t.Fatalf("%s: schedule %s changed: enabled=%t changes=%+v", after, id, got.Enabled, got.Changes)
	}
}

// A pane can't pause, resume or delete a schedule the person made, one
// another pane made, or one with no recorded creator pane.
func TestMCPScheduleChangeRefusedForSchedulesTheCallerDidNotCreate(t *testing.T) {
	a, persons, leads, anon := scheduleChangeFixture(t)
	rogue := tracedPaneCaller("rogue")
	for _, tc := range []struct {
		sched *Schedule
		want  string
	}{
		{persons, "was not created by your pane"},
		{leads, "was created by another pane"},
		{anon, "was not created by your pane"},
	} {
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"set_schedule_enabled", map[string]any{"id": tc.sched.ID, "enabled": false}},
			{"cancel_schedule", map[string]any{"id": tc.sched.ID}},
		} {
			text, failed := mcpAs(t, a, rogue, "rogue", call.tool, call.args)
			if !failed || !strings.Contains(text, tc.want) || !strings.Contains(text, "post_user_ask") {
				t.Fatalf("%s on %q = (failed %t) %q, want a refusal saying it %s", call.tool, tc.sched.Name, failed, text, tc.want)
			}
			mustStayOn(t, a.scheds, tc.sched.ID, call.tool)
		}
	}
	// Naming the creator in the header borrows nothing: the caller is the
	// pane its connection traces to.
	if text, failed := mcpAs(t, a, rogue, "lead", "cancel_schedule", map[string]any{"id": leads.ID}); !failed {
		t.Fatalf("a header naming the creator deleted its schedule: %q", text)
	}
	mustStayOn(t, a.scheds, leads.ID, "header claim")
	if len(a.scheds.Deleted()) != 0 {
		t.Fatalf("a refused delete was recorded: %+v", a.scheds.Deleted())
	}
}

// Resuming is refused on the same terms as pausing: an agent must not switch
// back on what the person switched off.
func TestMCPScheduleResumeRefusedForThePersonsPausedSchedule(t *testing.T) {
	a, persons, _, _ := scheduleChangeFixture(t)
	if _, err := a.scheds.SetEnabledBy(persons.ID, false, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	text, failed := mcpAs(t, a, tracedPaneCaller("lead"), "lead", "set_schedule_enabled", map[string]any{"id": persons.ID, "enabled": true})
	if !failed || !strings.Contains(text, "only the person can turn it back on") {
		t.Fatalf("resume = (failed %t) %q", failed, text)
	}
	if got, _ := a.scheds.Get(persons.ID); got.Enabled || len(got.Changes) != 1 {
		t.Fatalf("the person's paused schedule changed: %+v", got)
	}
}

// Callers that are not a verified pane are refused whatever they name, even
// for a schedule recorded as created by the pane they claim to be.
func TestMCPScheduleChangeRefusedForUnverifiedCallers(t *testing.T) {
	a, _, leads, _ := scheduleChangeFixture(t)
	for _, tc := range []struct {
		name   string
		traced *callerIdentity
		header string
	}{
		{"header claim only", nil, "lead"},
		{"no pane named", nil, ""},
		{"pane named by its environment", &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 4242, FromEnv: true}, "lead"},
		{"program outside the panes", &callerIdentity{Kind: callerUnverified, PID: 4242}, "lead"},
		{"worker process", &callerIdentity{Kind: callerWorker, PID: 4242}, "lead"},
		{"person-kind caller on the agent tools", &callerIdentity{Kind: callerHuman, PID: 4242}, "lead"},
		{"approved remote device, forwarded", &callerIdentity{Kind: callerHuman, Relay: true, Forwarded: true, Device: "dev"}, "lead"},
	} {
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"set_schedule_enabled", map[string]any{"id": leads.ID, "enabled": false}},
			{"cancel_schedule", map[string]any{"id": leads.ID}},
			// An id that does not exist gets the same answer: nothing leaks.
			{"cancel_schedule", map[string]any{"id": "sched_missing"}},
		} {
			text, failed := mcpAs(t, a, tc.traced, tc.header, call.tool, call.args)
			if !failed || text != scheduleUnverifiedCallerNotice {
				t.Fatalf("%s: %s = (failed %t) %q, want the unverified-caller refusal", tc.name, call.tool, failed, text)
			}
			mustStayOn(t, a.scheds, leads.ID, tc.name+" "+call.tool)
		}
	}
}

// A request forwarded for a remote device never acts as a pane, even if it
// reached the handler with one: the first gate refuses it, and the schedule
// check refuses it again by itself.
func TestMCPScheduleChangeRefusedForForwardedPaneCaller(t *testing.T) {
	a, _, leads, _ := scheduleChangeFixture(t)
	forwarded := &callerIdentity{Kind: callerPane, PaneID: "lead", PID: 4242, Forwarded: true}
	if text, failed := mcpAs(t, a, forwarded, "lead", "cancel_schedule", map[string]any{"id": leads.ID}); !failed {
		t.Fatalf("a forwarded caller deleted a schedule: %q", text)
	}
	mustStayOn(t, a.scheds, leads.ID, "forwarded")
	res := a.mcpToolCancelSchedule(map[string]any{"id": leads.ID}, paneCaller{ID: "lead"}, mcpCallMeta{id: *forwarded, transport: "tcp"})
	if text, failed := mcpResultText(t, res); !failed || text != scheduleUnverifiedCallerNotice {
		t.Fatalf("handler with a forwarded caller = (failed %t) %q", failed, text)
	}
	mustStayOn(t, a.scheds, leads.ID, "forwarded, at the handler")
}

// A caller whose pane could not be confirmed on a local socket is told to
// retry, as it is for create_schedule.
func TestMCPScheduleChangeRefusedForUnconfirmedCaller(t *testing.T) {
	a, _, leads, _ := scheduleChangeFixture(t)
	retraceFixture(t, true, os.Getpid(), missedTrace, missedTrace)
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"set_schedule_enabled", map[string]any{"id": leads.ID, "enabled": false}},
		{"cancel_schedule", map[string]any{"id": leads.ID}},
	} {
		text, failed := mcpRaw(t, a, "lead", call.tool, call.args)
		if !failed || text != mcpUnconfirmedCallerNotice {
			t.Fatalf("%s = (failed %t) %q, want the retry notice", call.tool, failed, text)
		}
		mustStayOn(t, a.scheds, leads.ID, call.tool)
	}
}

// The creating pane pauses, resumes and deletes its own schedule, and each
// change is recorded with its name, pane and source.
func TestMCPScheduleChangeByCreatorIsAllowedAndRecorded(t *testing.T) {
	a, _, leads, _ := scheduleChangeFixture(t)
	lead := tracedPaneCaller("lead")
	before := time.Now().Add(-time.Second)
	if text, failed := mcpAs(t, a, lead, "lead", "set_schedule_enabled", map[string]any{"id": leads.ID, "enabled": false}); failed {
		t.Fatalf("pause own: %q", text)
	}
	// Pausing what is already paused changes nothing and records nothing.
	if text, failed := mcpAs(t, a, lead, "lead", "set_schedule_enabled", map[string]any{"id": leads.ID, "enabled": false}); failed {
		t.Fatalf("pause own again: %q", text)
	}
	if text, failed := mcpAs(t, a, lead, "lead", "set_schedule_enabled", map[string]any{"id": leads.ID, "enabled": true}); failed {
		t.Fatalf("resume own: %q", text)
	}
	got, _ := a.scheds.Get(leads.ID)
	if !got.Enabled || len(got.Changes) != 2 {
		t.Fatalf("after pause and resume: enabled=%t changes=%+v", got.Enabled, got.Changes)
	}
	wantBy := a.paneCallerLabel(a.mgr.panes["lead"])
	for i, action := range []string{scheduleChangeEnabled, scheduleChangeDisabled} { // newest first
		c := got.Changes[i]
		if c.Action != action || c.By != wantBy || c.PaneID != "lead" || c.Source != scheduleSourceMCP || c.At.Before(before) {
			t.Fatalf("change %d = %+v, want %s by %q (pane lead, mcp)", i, c, action, wantBy)
		}
	}
	// list_schedules carries the newest change; the full record comes with
	// the run history.
	brief, _ := mcpAs(t, a, lead, "lead", "list_schedules", map[string]any{"nameContains": "lead's"})
	full, _ := mcpAs(t, a, lead, "lead", "list_schedules", map[string]any{"nameContains": "lead's", "includeRuns": true})
	if n := strings.Count(brief, `"action"`); n != 1 {
		t.Fatalf("list_schedules shows %d changes, want the newest only: %s", n, brief)
	}
	if n := strings.Count(full, `"action"`); n != 2 {
		t.Fatalf("list_schedules with run history shows %d changes, want 2: %s", n, full)
	}
	if text, failed := mcpAs(t, a, lead, "lead", "cancel_schedule", map[string]any{"id": leads.ID}); failed {
		t.Fatalf("delete own: %q", text)
	}
	if _, ok := a.scheds.Get(leads.ID); ok {
		t.Fatal("own schedule still there after cancel_schedule")
	}
	deleted := a.scheds.Deleted()
	if len(deleted) != 1 {
		t.Fatalf("deleted record = %+v", deleted)
	}
	d := deleted[0]
	if d.ID != leads.ID || d.Name != "lead's" || d.CreatedBy == nil || d.CreatedBy.PaneID != "lead" ||
		d.Change.Action != scheduleChangeDeleted || d.Change.By != wantBy || d.Change.PaneID != "lead" || d.Change.Source != scheduleSourceMCP {
		t.Fatalf("deleted record = %+v", d)
	}
}

// A schedule of a kind an agent cannot create (a built-in agent's) is the
// person's to change even if its record names the caller as creator.
func TestMCPScheduleChangeRefusedForBuiltinAgentKind(t *testing.T) {
	a, _, _, _ := scheduleChangeFixture(t)
	builtin := &Schedule{
		ID: "sched_builtin", Name: "PR babysitter", Kind: "agent", AgentID: "pr-agent", Cron: "*/10 * * * *",
		Enabled: true, CreatedBy: &ScheduleOrigin{Source: "mcp", PaneID: "lead"}, CreatedAt: time.Now(),
	}
	a.scheds.mu.Lock()
	a.scheds.items[builtin.ID] = builtin
	a.scheds.order = append(a.scheds.order, builtin.ID)
	a.scheds.mu.Unlock()
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"set_schedule_enabled", map[string]any{"id": builtin.ID, "enabled": false}},
		{"cancel_schedule", map[string]any{"id": builtin.ID}},
	} {
		text, failed := mcpAs(t, a, tracedPaneCaller("lead"), "lead", call.tool, call.args)
		if !failed || !strings.Contains(text, "runs a built-in agent") {
			t.Fatalf("%s = (failed %t) %q, want the built-in agent refusal", call.tool, failed, text)
		}
		mustStayOn(t, a.scheds, builtin.ID, call.tool)
	}
}

func scheduleRouteAs(t *testing.T, a *apiServer, who callerIdentity, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return who }
	defer func() { identifyCallerHook = prev }()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/schedules/{id}", a.updateSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", a.deleteSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/enable", a.enableSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/disable", a.disableSchedule)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// The person still pauses, resumes, edits and deletes any schedule from the
// app, whoever made it, and each change is recorded as the person's.
func TestPersonChangesAnyScheduleAndItIsRecorded(t *testing.T) {
	a, persons, leads, _ := scheduleChangeFixture(t)
	person := callerIdentity{Kind: callerHuman}
	for _, id := range []string{persons.ID, leads.ID} {
		if rec := scheduleRouteAs(t, a, person, "POST", "/api/schedules/"+id+"/disable", ""); rec.Code != 200 {
			t.Fatalf("disable %s: %d %s", id, rec.Code, rec.Body.String())
		}
		if rec := scheduleRouteAs(t, a, person, "POST", "/api/schedules/"+id+"/enable", ""); rec.Code != 200 {
			t.Fatalf("enable %s: %d %s", id, rec.Code, rec.Body.String())
		}
		got, _ := a.scheds.Get(id)
		if !got.Enabled || len(got.Changes) != 2 {
			t.Fatalf("%s after disable and enable: %+v", id, got)
		}
		for i, action := range []string{scheduleChangeEnabled, scheduleChangeDisabled} {
			if c := got.Changes[i]; c.Action != action || c.By != schedulePersonName || c.Source != scheduleSourceUI || c.PaneID != "" {
				t.Fatalf("%s change %d = %+v, want %s by the person from the app", id, i, c, action)
			}
		}
	}
	// An edit that also switches it off is recorded too.
	edit := `{"name":"person's","cron":"0 9 * * *","paneId":"social","prompt":"hello","enabled":false}`
	if rec := scheduleRouteAs(t, a, person, "PUT", "/api/schedules/"+persons.ID, edit); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := a.scheds.Get(persons.ID); got.Enabled || len(got.Changes) != 3 || got.Changes[0].Action != scheduleChangeDisabled || got.Changes[0].By != schedulePersonName {
		t.Fatalf("after the edit: %+v", got)
	}
	if rec := scheduleRouteAs(t, a, person, "DELETE", "/api/schedules/"+leads.ID, ""); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	deleted := a.scheds.Deleted()
	if len(deleted) != 1 || deleted[0].ID != leads.ID || deleted[0].Change.By != schedulePersonName || deleted[0].Change.Source != scheduleSourceUI {
		t.Fatalf("deleted record = %+v", deleted)
	}
}

// The app's schedule routes take only the person: a pane or an unverified
// program is refused and changes nothing.
func TestScheduleRoutesRefuseAgentCallers(t *testing.T) {
	a, persons, _, _ := scheduleChangeFixture(t)
	for _, who := range []callerIdentity{
		{Kind: callerPane, PaneID: "lead", PID: 4242},
		{Kind: callerWorker, PID: 4242},
		{Kind: callerUnverified, PID: 4242},
	} {
		for _, call := range []struct{ method, path, body string }{
			{"POST", "/api/schedules/" + persons.ID + "/disable", ""},
			{"DELETE", "/api/schedules/" + persons.ID, ""},
			{"PUT", "/api/schedules/" + persons.ID, `{"cron":"0 9 * * *","paneId":"social","prompt":"x","enabled":false}`},
		} {
			if rec := scheduleRouteAs(t, a, who, call.method, call.path, call.body); rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s as %s = %d, want 403", call.method, call.path, who.Kind, rec.Code)
			}
			mustStayOn(t, a.scheds, persons.ID, string(who.Kind)+" "+call.method)
		}
	}
}

// The record survives a restart, stays bounded, and cannot be authored
// through an edit.
func TestScheduleChangeRecordPersistsAndIsBounded(t *testing.T) {
	dir := t.TempDir()
	s, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sched, err := s.Create(ScheduleInput{Cron: "0 9 * * *", PaneID: "p1", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	actor := ScheduleActor{By: "@cmo\nforged line", PaneID: "pane-9", Source: scheduleSourceMCP}
	for i := 0; i < maxScheduleChanges+5; i++ {
		if _, err := s.SetEnabledBy(sched.ID, i%2 == 1, actor); err != nil {
			t.Fatal(err)
		}
	}
	// An edit that keeps the state leaves the record alone.
	if _, err := s.UpdateBy(sched.ID, ScheduleInput{Cron: "0 10 * * *", PaneID: "p1", Prompt: "x"}, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	s2, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	got, ok := s2.Get(sched.ID)
	if !ok || len(got.Changes) != maxScheduleChanges {
		t.Fatalf("after reload: ok=%t changes=%d, want %d", ok, len(got.Changes), maxScheduleChanges)
	}
	if c := got.Changes[0]; c.Action != scheduleChangeDisabled || c.By != "@cmo forged line" || c.PaneID != "pane-9" || c.Source != scheduleSourceMCP {
		t.Fatalf("newest change = %+v", c)
	}
	if c, ok := got.pausedChange(); !ok || c.By != "@cmo forged line" {
		t.Fatalf("pausedChange = %+v ok=%t", c, ok)
	}
	if err := s2.DeleteBy(sched.ID, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, deletedSchedulesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("deleted-schedules file mode = %v, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, deletedSchedulesFile))
	if strings.Contains(string(raw), `"prompt"`) {
		t.Fatalf("the deleted-schedules file keeps the prompt: %s", raw)
	}
}

// The daemon's own pause is recorded with its reason, and a finished
// schedule is not taken for a paused one.
func TestScheduleDaemonPauseRecordedAndFinishedIsNotPaused(t *testing.T) {
	s := newTestScheduleStore(t)
	sched, err := s.Create(ScheduleInput{Cron: "0 9 * * *", PaneID: "p1", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	s.pauseForReason(sched.ID, targetPaneGoneReason)
	got, _ := s.Get(sched.ID)
	c, ok := got.pausedChange()
	if !ok || c.By != scheduleActorDaemon.By || c.Source != scheduleSourceDaemon || c.Reason != targetPaneGoneReason {
		t.Fatalf("daemon pause record = %+v ok=%t", c, ok)
	}
	now := time.Now()
	past := now.Add(-time.Hour)
	for name, sc := range map[string]Schedule{
		"paused":            {Cron: "0 9 * * *"},
		"past its end date": {Cron: "0 9 * * *", Until: &past},
		"ran out of runs":   {Cron: "0 9 * * *", MaxFires: 2, FireCount: 2},
		"one-time, spent":   {RunAt: &past},
	} {
		if got, want := sc.finished(now), name != "paused"; got != want {
			t.Errorf("%s: finished = %t, want %t", name, got, want)
		}
	}
	on := Schedule{Cron: "0 9 * * *", Enabled: true, Until: &past}
	if on.finished(now) {
		t.Error("an enabled schedule reported as finished")
	}
}
