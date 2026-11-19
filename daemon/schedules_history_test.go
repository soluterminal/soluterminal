package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Automations stage 2 (ST-299), items 9 and 10: every run has an outcome and
// lands in an append-only history kept by day.

func historyOf(t *testing.T, s *ScheduleStore, id string, q historyQuery) historyPage {
	t.Helper()
	page, err := s.History(id, q)
	if err != nil {
		t.Fatalf("History(%s): %v", id, err)
	}
	return page
}

func historyRuns(page historyPage) []ScheduleRun {
	var out []ScheduleRun
	for _, e := range page.Entries {
		if e.Type == historyEntryRun {
			out = append(out, *e.Run)
		}
	}
	return out
}

func historyChanges(page historyPage) []ScheduleChange {
	var out []ScheduleChange
	for _, e := range page.Entries {
		if e.Type == historyEntryChange {
			out = append(out, *e.Change)
		}
	}
	return out
}

func offPrompt(t *testing.T, s *ScheduleStore, name string) *Schedule {
	t.Helper()
	off := false
	sched, err := s.Create(ScheduleInput{Name: name, PaneID: "p1", Prompt: "hi", Cron: "0 9 * * *", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	return sched
}

func TestRecordRunFillsTheRecordAndAppendsHistory(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "standup")
	base := time.Now().Add(-time.Minute)
	s.recordRun(sched.ID, ScheduleRun{At: base, Status: "fired", Detail: "delivered"})
	s.recordRun(sched.ID, ScheduleRun{At: base.Add(time.Second), Status: "skipped", Cause: runCausePaneBusy, Detail: "busy"})
	s.recordRun(sched.ID, ScheduleRun{At: base.Add(2 * time.Second), Status: "error", Trigger: runTriggerRunNow, Detail: "boom"})

	got, _ := s.Get(sched.ID)
	want := []struct{ outcome, trigger string }{
		{runOutcomeFailed, runTriggerRunNow},
		{runOutcomeSkipped, runTriggerScheduled},
		{runOutcomeWorked, runTriggerScheduled},
	}
	for i, w := range want {
		r := got.Runs[i]
		if r.ID == "" || r.Outcome != w.outcome || r.Trigger != w.trigger {
			t.Errorf("Runs[%d] = id %q outcome %q trigger %q, want an id, %q, %q", i, r.ID, r.Outcome, r.Trigger, w.outcome, w.trigger)
		}
	}
	runs := historyRuns(historyOf(t, s, sched.ID, historyQuery{}))
	if len(runs) != 3 {
		t.Fatalf("history has %d runs, want 3", len(runs))
	}
	for i := range runs {
		if runs[i].ID != got.Runs[i].ID || runs[i].Outcome != got.Runs[i].Outcome {
			t.Errorf("history run %d = %+v, want the same record as Runs[%d] %+v", i, runs[i], i, got.Runs[i])
		}
	}
	if runs[1].Cause != runCausePaneBusy {
		t.Errorf("cause = %q, want %q", runs[1].Cause, runCausePaneBusy)
	}
}

func TestHistoryKeepsMoreThanTheRowAndPagesWithAFailedFilter(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "often")
	base := time.Now().Add(-48 * time.Hour)
	const n = 3*maxRunHistory + 5
	for i := 0; i < n; i++ {
		status := "fired"
		if i%5 == 0 {
			status = "error"
		}
		// Spread over three UTC days.
		s.recordRun(sched.ID, ScheduleRun{At: base.Add(time.Duration(i) * 40 * time.Minute), Status: status, Detail: "run"})
	}
	got, _ := s.Get(sched.ID)
	if len(got.Runs) != maxRunHistory {
		t.Fatalf("Runs keeps %d, want the newest %d", len(got.Runs), maxRunHistory)
	}

	var all []ScheduleRun
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging does not end")
		}
		page := historyOf(t, s, sched.ID, historyQuery{Before: cursor, Limit: 7})
		if len(page.Entries) > 7 {
			t.Fatalf("page of %d entries, limit 7", len(page.Entries))
		}
		all = append(all, historyRuns(page)...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(all) != n {
		t.Fatalf("paged %d runs, want all %d", len(all), n)
	}
	seen := map[string]bool{}
	for i, r := range all {
		if seen[r.ID] {
			t.Fatalf("run %s returned twice", r.ID)
		}
		seen[r.ID] = true
		if i > 0 && r.At.After(all[i-1].At) {
			t.Fatalf("history not newest first at %d", i)
		}
	}

	failed := historyOf(t, s, sched.ID, historyQuery{FailedOnly: true, Limit: historyPageMax})
	if len(historyChanges(failed)) != 0 {
		t.Error("the failed-only filter returned changes")
	}
	wantFailed := (n + 4) / 5
	if runs := historyRuns(failed); len(runs) != wantFailed {
		t.Fatalf("failed-only returned %d runs, want %d", len(runs), wantFailed)
	} else {
		for _, r := range runs {
			if r.Outcome != runOutcomeFailed {
				t.Fatalf("failed-only returned a %q run", r.Outcome)
			}
		}
	}
	if _, err := s.History(sched.ID, historyQuery{Before: "not-a-cursor"}); err != errHistoryCursor {
		t.Errorf("a bad cursor: err = %v, want errHistoryCursor", err)
	}
}

func TestHistoryRecordsCreateEditAndOnOffWithWho(t *testing.T) {
	s := newTestScheduleStore(t)
	off := false
	sched, err := s.CreateAs(ScheduleInput{Name: "digest", PaneID: "p1", Prompt: "hi", Cron: "0 9 * * *", Enabled: &off}, nil, scheduleActorPerson)
	if err != nil {
		t.Fatal(err)
	}
	agent := ScheduleActor{By: "@orchestrator", PaneID: "p9", Source: scheduleSourceMCP}
	if _, err := s.UpdateBy(sched.ID, ScheduleInput{Name: "digest", PaneID: "p1", Prompt: "hi there", Cron: "0 10 * * *", Enabled: &off}, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetEnabledBy(sched.ID, true, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetEnabledBy(sched.ID, false, agent); err != nil {
		t.Fatal(err)
	}
	changes := historyChanges(historyOf(t, s, sched.ID, historyQuery{}))
	want := []struct{ action, by, source string }{
		{scheduleChangeDisabled, "@orchestrator", scheduleSourceMCP},
		{scheduleChangeEnabled, schedulePersonName, scheduleSourceUI},
		{scheduleChangeEdited, schedulePersonName, scheduleSourceUI},
		{scheduleChangeCreated, schedulePersonName, scheduleSourceUI},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v, want %d", changes, len(want))
	}
	for i, w := range want {
		if c := changes[i]; c.Action != w.action || c.By != w.by || c.Source != w.source {
			t.Errorf("change %d = %+v, want %s by %s from %s", i, c, w.action, w.by, w.source)
		}
	}
	// The row keeps only on/off for "Turned off by …".
	got, _ := s.Get(sched.ID)
	if pc, ok := got.pausedChange(); !ok || pc.By != "@orchestrator" {
		t.Errorf("pausedChange = %+v %v, want the agent's pause", pc, ok)
	}
	for _, c := range got.Changes {
		if c.Action == scheduleChangeCreated || c.Action == scheduleChangeEdited {
			t.Errorf("row Changes has %q; created and edited live in the history only", c.Action)
		}
	}

	// An agent's create over MCP names its pane.
	byAgent, err := s.CreateWithOrigin(ScheduleInput{Name: "nightly", PaneID: "p1", Prompt: "x", Cron: "0 2 * * *", Enabled: &off},
		&ScheduleOrigin{Source: scheduleSourceMCP, PaneID: "p9", PaneTitle: "@orchestrator"})
	if err != nil {
		t.Fatal(err)
	}
	created := historyChanges(historyOf(t, s, byAgent.ID, historyQuery{}))
	if len(created) != 1 || created[0].Action != scheduleChangeCreated || created[0].PaneID != "p9" || created[0].Source != scheduleSourceMCP {
		t.Errorf("agent create recorded as %+v", created)
	}
}

func TestHistoryHoldsNoPromptText(t *testing.T) {
	s := newTestScheduleStore(t)
	off := false
	const secret = "PROMPT-TEXT-MUST-NOT-BE-IN-HISTORY"
	sched, err := s.CreateAs(ScheduleInput{Name: "n", PaneID: "p1", Prompt: secret, Cron: "0 9 * * *", Enabled: &off}, nil, scheduleActorPerson)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBy(sched.ID, ScheduleInput{Name: "n", PaneID: "p1", Prompt: secret + " 2", Cron: "0 9 * * *", Enabled: &off}, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	s.recordRun(sched.ID, ScheduleRun{Status: "fired", Detail: "delivered"})
	dir, _ := s.history.dir(sched.ID)
	files, _ := os.ReadDir(dir)
	if len(files) == 0 {
		t.Fatal("no history written")
	}
	for _, f := range files {
		data, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		if strings.Contains(string(data), secret) {
			t.Fatalf("history file %s holds the prompt", f.Name())
		}
		fi, _ := os.Stat(filepath.Join(dir, f.Name()))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("history file mode %v, want 0600", fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("history folder mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestHistoryImportsOlderRunsOnceAndDeleteRemovesIt(t *testing.T) {
	dir := t.TempDir()
	created := time.Now().Add(-72 * time.Hour).UTC().Round(time.Second)
	file := map[string]any{
		"schedules": []map[string]any{{
			"id": "sched_legacy", "name": "legacy", "cron": "0 9 * * *", "paneId": "p1", "prompt": "hi",
			"enabled": false, "createdAt": created,
			"runs": []map[string]any{
				{"at": created.Add(48 * time.Hour), "status": "error", "detail": "newest"},
				{"at": created.Add(24 * time.Hour), "status": "fired", "detail": "older"},
			},
			"changes": []map[string]any{{"at": created.Add(36 * time.Hour), "action": "disabled", "by": "person", "source": "ui"}},
		}},
		"order": []string{"sched_legacy"},
	}
	data, _ := json.Marshal(file)
	if err := os.WriteFile(filepath.Join(dir, "schedules.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	load := func() *ScheduleStore {
		s, err := NewScheduleStore(dir, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Stop)
		return s
	}
	s := load()
	// Import happens with the first new record (a run or a change).
	s.recordRun("sched_legacy", ScheduleRun{Status: "skipped", Detail: "new"})
	s.Stop()
	s2 := load()
	s2.recordRun("sched_legacy", ScheduleRun{Status: "skipped", Detail: "newer"})

	page := historyOf(t, s2, "sched_legacy", historyQuery{})
	var order []string
	for _, e := range page.Entries {
		if e.Type == historyEntryRun {
			order = append(order, e.Run.Detail)
		} else {
			order = append(order, e.Change.Action)
		}
	}
	want := []string{"newer", "new", "newest", "disabled", "older", "created"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("history = %v, want %v (imported once, oldest first)", order, want)
	}
	if runs := historyRuns(page); runs[2].Outcome != runOutcomeFailed || runs[3].Outcome != runOutcomeWorked {
		t.Errorf("imported runs got outcomes %q, %q", runs[2].Outcome, runs[3].Outcome)
	}

	if err := s2.Delete("sched_legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, historyDirName, "sched_legacy")); !os.IsNotExist(err) {
		t.Errorf("history kept after delete: %v", err)
	}
}

func TestHistoryPrunesByAgeAndSize(t *testing.T) {
	root := t.TempDir()
	h := newScheduleHistory(root)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	write := func(at time.Time) {
		h.mu.Lock()
		delete(h.pruned, "sched_a")
		h.mu.Unlock()
		h.append("sched_a", historyRun(ScheduleRun{ID: newRunID(at), At: at, Status: "fired", Outcome: runOutcomeWorked, Detail: strings.Repeat("d", 200)}, at))
	}
	write(now.Add(-historyRetention - 48*time.Hour))
	write(now.Add(-10 * 24 * time.Hour))
	write(now)
	days := daysIn(filepath.Join(root, historyDirName, "sched_a"))
	if len(days) != 2 || days[1].day != now.Add(-10*24*time.Hour).Format(time.DateOnly) {
		t.Fatalf("days after the age prune = %+v, want today and 10 days ago", days)
	}
	old := historyMaxBytes
	historyMaxBytes = 300
	t.Cleanup(func() { historyMaxBytes = old })
	write(now)
	days = daysIn(filepath.Join(root, historyDirName, "sched_a"))
	if len(days) != 1 || days[0].day != now.Format(time.DateOnly) {
		t.Fatalf("days after the size prune = %+v, want only today (today always stays)", days)
	}
}

func TestHistoryRefusesUnsafeIDs(t *testing.T) {
	h := newScheduleHistory(t.TempDir())
	for _, id := range []string{"", "../x", "a/b", ".", strings.Repeat("a", 81)} {
		if _, ok := h.dir(id); ok {
			t.Errorf("dir(%q) accepted", id)
		}
	}
	if _, err := h.page("../x", historyQuery{}); err == nil {
		t.Error("page accepted an unsafe id")
	}
}

func TestHistoryNotesLandOnTheirRun(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "chat")
	at := time.Now().Add(-time.Minute).Round(time.Millisecond)
	s.recordRun(sched.ID, ScheduleRun{At: at, Status: "fired", Detail: "report ready"})
	s.stampRunDetail(sched.ID, at, "chat ✓ delivered")
	runs := historyRuns(historyOf(t, s, sched.ID, historyQuery{}))
	if len(runs) != 1 || runs[0].Detail != "report ready · chat ✓ delivered" {
		t.Fatalf("history runs = %+v, want the note on the run", runs)
	}
}

func TestScheduleHistoryRoute(t *testing.T) {
	s := newTestScheduleStore(t)
	a := &apiServer{scheds: s}
	sched := offPrompt(t, s, "route")
	s.recordRun(sched.ID, ScheduleRun{Status: "error", Detail: "x"})
	s.recordRun(sched.ID, ScheduleRun{Status: "fired", Detail: "y"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/schedules/{id}/history", a.scheduleHistory)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	rec := get("/api/schedules/" + sched.ID + "/history?outcome=failed")
	var page historyPage
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
		t.Fatalf("GET history = %d %s", rec.Code, rec.Body.String())
	}
	if runs := historyRuns(page); len(runs) != 1 || runs[0].Outcome != runOutcomeFailed {
		t.Fatalf("failed-only page = %+v", page)
	}
	for path, code := range map[string]int{
		"/api/schedules/sched_nope/history":                     404,
		"/api/schedules/" + sched.ID + "/history?outcome=other": 400,
		"/api/schedules/" + sched.ID + "/history?limit=0":       400,
		"/api/schedules/" + sched.ID + "/history?before=bad":    400,
	} {
		if rec := get(path); rec.Code != code {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, code)
		}
	}
}

func TestTaskReportVerdict(t *testing.T) {
	for out, want := range map[string]string{
		"Nothing to do.\n":                                          runOutcomeNothing,
		"**Nothing to do.**":                                        runOutcomeNothing,
		"Nothing to do.\n\n## Done\n- fixed a typo":                 "",
		"Could not finish: the repo is not checked out\n\nDetails…": runOutcomeFailed,
		"## Could not finish: no network":                           runOutcomeFailed,
		"Nothing new needs you.\n\n## Digest\n- …":                  "",
		"## Needs you\n- merge PR #7":                               "",
	} {
		if got := taskReportVerdict([]byte(out)); got != want {
			t.Errorf("taskReportVerdict(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestTaskRunRecordsOutcomeTriggerAccountAndTimes(t *testing.T) {
	fakeClaude(t, "if [ \"$n\" -eq 1 ]; then echo 'Could not finish: the build folder is missing (service unavailable)'; else echo 'Nothing to do.'; fi\n")
	s := newTestScheduleStore(t)
	s.SetAccountResolver(func(provider string) string {
		if provider != "claude" {
			t.Errorf("account asked for %q", provider)
		}
		return "work"
	})
	work := t.TempDir()
	off := false
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 9 * * *", Prompt: "build", Cwd: work, Permission: "plan", Model: "sonnet", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RunNow(sched.ID); err != nil {
		t.Fatal(err)
	}
	got := waitTaskIdle(t, s, sched.ID, 1)
	r := got.Runs[0]
	// Status stays "fired" (it ran to its end: it counts toward "stop
	// after N runs" and its report still goes to its chat); the outcome
	// says it failed.
	if r.Outcome != runOutcomeFailed || r.Cause != runCauseReportedFailed || r.Status != "fired" {
		t.Errorf("run = outcome %q cause %q status %q, want a fired run with outcome failed", r.Outcome, r.Cause, r.Status)
	}
	if r.Trigger != runTriggerRunNow || r.Account != "work" || r.Model != "sonnet" {
		t.Errorf("run = trigger %q account %q model %q", r.Trigger, r.Account, r.Model)
	}
	if r.EndedAt == nil || r.EndedAt.Before(r.At) {
		t.Errorf("run end = %v, start %v", r.EndedAt, r.At)
	}
	if !strings.Contains(r.Summary, "the build folder is missing") {
		t.Errorf("summary = %q, want the report's reason", r.Summary)
	}

	s.fireTask(sched.ID, work, "build", "plan", "", 0)
	got = waitTaskIdle(t, s, sched.ID, 2)
	if r := got.Runs[0]; r.Outcome != runOutcomeNothing || r.Status != "fired" || r.Trigger != runTriggerScheduled {
		t.Errorf("nothing-to-do run = outcome %q status %q trigger %q", r.Outcome, r.Status, r.Trigger)
	}
}

func TestTaskFailureCauses(t *testing.T) {
	s := newTestScheduleStore(t)
	work := t.TempDir()
	off := false
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 9 * * *", Prompt: "p", Cwd: work, Permission: "plan", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, filepath.Join(work, "gone"), "p", "plan", "", 0)
	if r := waitForRun(t, s, sched.ID); r.Cause != runCauseFolderGone || r.Outcome != runOutcomeFailed {
		t.Errorf("missing folder: cause %q outcome %q", r.Cause, r.Outcome)
	}
}

func TestReportedFailureCountsTowardMaxFires(t *testing.T) {
	fakeClaude(t, "echo 'Could not finish: no network'\n")
	s := newTestScheduleStore(t)
	work := t.TempDir()
	sched, err := s.Create(ScheduleInput{Kind: "task", Cron: "0 9 * * *", MaxFires: 1, Prompt: "p", Cwd: work, Permission: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	s.fireTask(sched.ID, work, "p", "plan", "", 0)
	got := waitTaskIdle(t, s, sched.ID, 1)
	if got.Enabled || got.FireCount != 1 || got.Runs[0].Outcome != runOutcomeFailed {
		t.Fatalf("after one reported failure with stop after 1: enabled %v, fireCount %d, outcome %q; want stopped, 1, failed",
			got.Enabled, got.FireCount, got.Runs[0].Outcome)
	}
}

// A run that started before an edit and ended after it is written after the
// edit, and the history is newest first by each line's time.
func TestHistoryOrderIsWhenEachLineWasWritten(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "order")
	started := time.Now().Add(-time.Minute)
	time.Sleep(2 * time.Millisecond)
	off := false
	if _, err := s.UpdateBy(sched.ID, ScheduleInput{Name: "order", PaneID: "p1", Prompt: "hi", Cron: "0 8 * * *", Enabled: &off}, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	s.recordRun(sched.ID, ScheduleRun{At: started, Status: "fired", Detail: "late"})
	page := historyOf(t, s, sched.ID, historyQuery{})
	if len(page.Entries) != 3 || page.Entries[0].Type != historyEntryRun || page.Entries[1].Type != historyEntryChange {
		t.Fatalf("entries = %+v, want the run, the edit, the create", page.Entries)
	}
	for i := 1; i < len(page.Entries); i++ {
		if page.Entries[i].At.After(page.Entries[i-1].At) {
			t.Fatalf("entry %d is newer than entry %d", i, i-1)
		}
	}
	if !page.Entries[0].Run.At.Equal(started) {
		t.Errorf("run.at = %v, want its start %v", page.Entries[0].Run.At, started)
	}
}

// An automation from before the history shows its runs the first time its
// history is read, with nothing else having happened.
func TestHistoryReadImportsOlderRuns(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "old")
	s.mu.Lock()
	s.history.remove(sched.ID) // as before stage 2: no history folder
	s.items[sched.ID].Runs = []ScheduleRun{{At: time.Now().Add(-time.Hour), Status: "fired", Detail: "older run"}}
	s.mu.Unlock()
	runs := historyRuns(historyOf(t, s, sched.ID, historyQuery{}))
	if len(runs) != 1 || runs[0].Detail != "older run" || runs[0].ID == "" {
		t.Fatalf("history on first read = %+v, want the older run with an id", runs)
	}
	got, _ := s.Get(sched.ID)
	if got.Runs[0].ID != runs[0].ID {
		t.Errorf("row run id %q, history %q: want the same", got.Runs[0].ID, runs[0].ID)
	}
	// A second read adds nothing.
	if again := historyRuns(historyOf(t, s, sched.ID, historyQuery{})); len(again) != 1 {
		t.Errorf("second read: %d runs, want 1", len(again))
	}
}

// An import cut short (here: a day file left behind, no marker) is written
// again whole, never doubled.
func TestHistoryImportCutShortIsRedoneNotDoubled(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "cut")
	at := time.Now().Add(-time.Hour).UTC()
	s.mu.Lock()
	s.history.remove(sched.ID)
	s.items[sched.ID].Runs = []ScheduleRun{{At: at, Status: "fired", Detail: "older run"}}
	s.mu.Unlock()
	dir, _ := s.history.dir(sched.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	half := `{"type":"run","at":"` + at.Format(time.RFC3339Nano) + `","run":{"at":"` + at.Format(time.RFC3339Nano) + `","status":"fired","detail":"older run"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, at.Format(time.DateOnly)+".jsonl"), []byte(half), 0o600); err != nil {
		t.Fatal(err)
	}
	s.recordRun(sched.ID, ScheduleRun{Status: "fired", Detail: "new run"})
	runs := historyRuns(historyOf(t, s, sched.ID, historyQuery{}))
	var details []string
	for _, r := range runs {
		details = append(details, r.Detail)
	}
	if strings.Join(details, ",") != "new run,older run" {
		t.Fatalf("history = %v, want the new run and the older run once each", details)
	}
}

// The size cap is checked on every write, not only on a day's first.
func TestHistorySizeCapIsCheckedOnEveryWrite(t *testing.T) {
	root := t.TempDir()
	h := newScheduleHistory(root)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	h.markStarted("sched_b")
	old := historyMaxBytes
	historyMaxBytes = 2000
	t.Cleanup(func() { historyMaxBytes = old })
	yesterday := now.Add(-24 * time.Hour)
	for i := 0; i < 5; i++ {
		h.append("sched_b", historyRun(ScheduleRun{At: yesterday, Status: "fired", Detail: strings.Repeat("y", 300)}, yesterday))
	}
	for i := 0; i < 5; i++ {
		h.append("sched_b", historyRun(ScheduleRun{At: now, Status: "fired", Detail: strings.Repeat("t", 300)}, now))
	}
	days := daysIn(filepath.Join(root, historyDirName, "sched_b"))
	if len(days) != 1 || days[0].day != now.Format(time.DateOnly) {
		t.Fatalf("days = %+v, want yesterday pruned once over the cap, the same day", days)
	}
	// One line's text is clipped.
	h.append("sched_b", historyRun(ScheduleRun{At: now, Status: "fired", Summary: strings.Repeat("s", 10000)}, now))
	runs := historyRuns(func() historyPage { p, _ := h.page("sched_b", historyQuery{Limit: 1}); return p }())
	if n := len([]rune(runs[0].Summary)); n > historyLineMaxRunes+1 {
		t.Errorf("summary in history has %d runes, want at most %d", n, historyLineMaxRunes+1)
	}
}

// The first-read import fills a copy of Runs: a Schedule another reader got
// before it is not changed under that reader.
func TestHistoryImportLeavesEarlierReadersAlone(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "copy")
	s.mu.Lock()
	s.history.remove(sched.ID)
	s.items[sched.ID].Runs = []ScheduleRun{{At: time.Now().Add(-time.Hour), Status: "fired"}}
	s.mu.Unlock()
	before, _ := s.Get(sched.ID)
	historyOf(t, s, sched.ID, historyQuery{})
	if before.Runs[0].ID != "" || before.Runs[0].Outcome != "" {
		t.Fatalf("an earlier reader's run changed under it: %+v", before.Runs[0])
	}
	if after, _ := s.Get(sched.ID); after.Runs[0].ID == "" {
		t.Fatal("the row's run got no id")
	}
}

// The retired-model migration wrote an undated failure; it is imported, in
// its place in the list, and the failed-only filter finds it.
func TestHistoryImportKeepsAnUndatedFailure(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := offPrompt(t, s, "undated")
	older := time.Now().Add(-2 * time.Hour)
	s.mu.Lock()
	s.history.remove(sched.ID)
	s.items[sched.ID].Runs = []ScheduleRun{
		{Status: "error", Detail: "disabled: retired DeepSeek model"},
		{At: older, Status: "fired", Detail: "older"},
	}
	s.mu.Unlock()
	failed := historyRuns(historyOf(t, s, sched.ID, historyQuery{FailedOnly: true}))
	if len(failed) != 1 || failed[0].Detail != "disabled: retired DeepSeek model" || !failed[0].At.After(older) {
		t.Fatalf("failed-only = %+v, want the undated failure, placed after the older run", failed)
	}
}

// A busy day is split into segments, so the size cap drops a busy day's
// older segments too and the newest segment stays small.
func TestHistorySegmentsABusyDayUnderTheCap(t *testing.T) {
	root := t.TempDir()
	h := newScheduleHistory(root)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	h.markStarted("sched_c")
	oldSeg, oldMax := historySegmentBytes, historyMaxBytes
	historySegmentBytes, historyMaxBytes = 1000, 3000
	t.Cleanup(func() { historySegmentBytes, historyMaxBytes = oldSeg, oldMax })
	const n = 60
	for i := 0; i < n; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		h.append("sched_c", historyRun(ScheduleRun{ID: newRunID(at), At: at, Status: "fired", Detail: strings.Repeat("x", 200)}, at))
	}
	segs := daysIn(filepath.Join(root, historyDirName, "sched_c"))
	var total int64
	for _, d := range segs {
		total += d.size
		if d.size > historySegmentBytes+400 {
			t.Errorf("segment %s is %d bytes, want about %d", d.name(), d.size, historySegmentBytes)
		}
	}
	if len(segs) < 2 || total > historyMaxBytes+historySegmentBytes {
		t.Fatalf("segments %+v total %d: want several, under the cap", segs, total)
	}
	// Paging across segments returns what is left, newest first, once each.
	var all []ScheduleRun
	cursor := ""
	for {
		page, err := h.page("sched_c", historyQuery{Before: cursor, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, historyRuns(page)...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(all) == 0 || len(all) >= n {
		t.Fatalf("paged %d runs, want the ones left after pruning (fewer than %d)", len(all), n)
	}
	for i := 1; i < len(all); i++ {
		if !all[i].At.Before(all[i-1].At) {
			t.Fatalf("run %d not older than run %d", i, i-1)
		}
	}
	if !all[0].At.Equal(now.Add((n - 1) * time.Second)) {
		t.Errorf("newest run %v, want the last written", all[0].At)
	}
}

// A line cut short by an earlier write does not swallow the next record.
func TestHistoryAppendEndsATornLine(t *testing.T) {
	root := t.TempDir()
	h := newScheduleHistory(root)
	now := time.Now().UTC()
	h.markStarted("sched_d")
	day := filepath.Join(root, historyDirName, "sched_d", now.Format(time.DateOnly)+".jsonl")
	if err := os.WriteFile(day, []byte(`{"type":"run","at":"2026`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.append("sched_d", historyRun(ScheduleRun{ID: "run_x", At: now, Status: "fired", Detail: "after the tear"}, now))
	page, _ := h.page("sched_d", historyQuery{})
	runs := historyRuns(page)
	if len(runs) != 1 || runs[0].Detail != "after the tear" {
		t.Fatalf("runs after a torn line = %+v, want the new record", runs)
	}
}

// A run recorded again after a long stop still finds the line its first try
// wrote days earlier, so it gets no second history line.
func TestHasRunSearchesBackToTheRunsStart(t *testing.T) {
	h := newScheduleHistory(t.TempDir())
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	h.markStarted("sched_e")
	started := now.Add(-4 * 24 * time.Hour)
	h.append("sched_e", historyRun(ScheduleRun{ID: "run_old", At: started, Status: "fired"}, started))
	for d := 3; d >= 0; d-- {
		at := now.Add(-time.Duration(d) * 24 * time.Hour)
		h.append("sched_e", historyRun(ScheduleRun{ID: newRunID(at), At: at, Status: "skipped"}, at))
	}
	if !h.hasRun("sched_e", "run_old", started) {
		t.Error("the first try's line four days back was not found")
	}
	if h.hasRun("sched_e", "run_other", started) {
		t.Error("found a run that is not there")
	}
}
