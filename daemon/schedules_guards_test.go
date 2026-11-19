package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Automations stage 2 (ST-299 PR 6): how often and how many (item 14), and
// runs missed while the computer was asleep (item 15).

func TestCronMinGap(t *testing.T) {
	from := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	for expr, want := range map[string]time.Duration{
		"*/5 * * * *": 5 * time.Minute,
		"0,1 9 * * *": time.Minute,
		"0 9 * * 1-5": 24 * time.Hour,
		"0 * * * *":   time.Hour,
	} {
		got, ok := cronMinGap(expr, from)
		if !ok || got != want {
			t.Errorf("cronMinGap(%q) = %s %v, want %s", expr, got, ok, want)
		}
	}
	if _, ok := cronMinGap("not a cron", from); ok {
		t.Error("a bad expression has a gap")
	}
}

func TestIntervalGuard(t *testing.T) {
	person, agent := scheduleActorPerson, ScheduleActor{By: "@w", PaneID: "p1", Source: scheduleSourceMCP}
	for _, tc := range []struct {
		name    string
		in      ScheduleInput
		actor   ScheduleActor
		editing *Schedule
		refused bool
	}{
		{"person, every minute", ScheduleInput{Cron: "* * * * *"}, person, nil, true},
		{"person, every 5 minutes", ScheduleInput{Cron: "*/5 * * * *"}, person, nil, false},
		{"agent, every 10 minutes", ScheduleInput{Cron: "*/10 * * * *"}, agent, nil, true},
		{"agent, every 15 minutes", ScheduleInput{Cron: "*/15 * * * *"}, agent, nil, false},
		{"one-time", ScheduleInput{RunAt: new(time.Time)}, agent, nil, false},
		{"the daemon's own", ScheduleInput{Cron: "* * * * *"}, scheduleActorDaemon, nil, false},
		{"a built-in agent", ScheduleInput{Kind: "agent", Cron: "* * * * *"}, person, nil, false},
		{"an edit that keeps an older timing", ScheduleInput{Cron: "* * * * *"}, person, &Schedule{Cron: "* * * * *"}, false},
		{"an edit to a timing too often", ScheduleInput{Cron: "*/2 * * * *"}, person, &Schedule{Cron: "0 9 * * *"}, true},
	} {
		err := intervalGuard(tc.in, tc.actor, tc.editing)
		if (err != nil) != tc.refused {
			t.Errorf("%s: err = %v, want refused %v", tc.name, err, tc.refused)
		}
	}
	err := intervalGuard(ScheduleInput{Cron: "*/10 * * * *"}, agent, nil)
	if err == nil || !strings.Contains(err.Error(), "every 15 minutes") || !strings.Contains(err.Error(), "every 10 minutes") {
		t.Errorf("the refusal does not say the limit and the timing: %v", err)
	}
}

func TestGuardsOnCreateAndEdit(t *testing.T) {
	s := newTestScheduleStore(t)
	off := false
	if _, err := s.CreateAs(ScheduleInput{PaneID: "p1", Prompt: "x", Cron: "* * * * *", Enabled: &off}, nil, scheduleActorPerson); err == nil {
		t.Fatal("the person created an automation that runs every minute")
	}
	sched, err := s.CreateAs(ScheduleInput{PaneID: "p1", Prompt: "x", Cron: "0 9 * * *", Enabled: &off}, nil, scheduleActorPerson)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBy(sched.ID, ScheduleInput{PaneID: "p1", Prompt: "x", Cron: "*/2 * * * *", Enabled: &off}, scheduleActorPerson); err == nil {
		t.Fatal("an edit to every 2 minutes was saved")
	}
	// An agent over MCP: every 15 minutes at most.
	origin := &ScheduleOrigin{Source: scheduleSourceMCP, PaneID: "p9", PaneTitle: "@w"}
	if _, err := s.CreateWithOrigin(ScheduleInput{PaneID: "p1", Prompt: "x", Cron: "*/5 * * * *", Enabled: &off}, origin); err == nil {
		t.Fatal("an agent created an automation that runs every 5 minutes")
	}
	// An automation from before the guards keeps running, marked.
	old, err := s.Create(ScheduleInput{PaneID: "p1", Prompt: "x", Cron: "* * * * *", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	a := &apiServer{scheds: s}
	got, _ := s.Get(old.ID)
	if dto := a.makeScheduleDTO(got); !dto.TooFrequent {
		t.Error("an older automation that runs every minute is not marked")
	}
	if _, err := s.UpdateBy(old.ID, ScheduleInput{Name: "renamed", PaneID: "p1", Prompt: "x", Cron: "* * * * *", Enabled: &off}, scheduleActorPerson); err != nil {
		t.Errorf("an edit that keeps its older timing was refused: %v", err)
	}
}

func TestCountCaps(t *testing.T) {
	s := newTestScheduleStore(t)
	off := false
	in := func(i int) ScheduleInput {
		return ScheduleInput{Name: fmt.Sprintf("n%d", i), PaneID: "p1", Prompt: "x", Cron: "0 9 * * *", Enabled: &off}
	}
	mine := &ScheduleOrigin{Source: scheduleSourceMCP, PaneID: "p9"}
	for i := 0; i < maxAutomationsPerAgentPane; i++ {
		if _, err := s.CreateWithOrigin(in(i), mine); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := s.CreateWithOrigin(in(99), mine); err == nil || !strings.Contains(err.Error(), "this pane has already created") {
		t.Fatalf("pane cap: err = %v", err)
	}
	if _, err := s.CreateWithOrigin(in(98), &ScheduleOrigin{Source: scheduleSourceMCP, PaneID: "p8"}); err != nil {
		t.Fatalf("another pane is refused: %v", err)
	}
	for i := 0; ; i++ {
		_, err := s.CreateAs(in(1000+i), nil, scheduleActorPerson)
		if err != nil {
			if !strings.Contains(err.Error(), "the most this computer keeps") {
				t.Fatalf("total cap: err = %v", err)
			}
			break
		}
		if i > maxAutomations {
			t.Fatal("no total cap")
		}
	}
	if n := len(s.List()); n != maxAutomations {
		t.Errorf("%d automations, want the cap of %d", n, maxAutomations)
	}
	// The daemon's own (built-in agents' bootstrap) is not capped.
	if _, err := s.Create(in(5000)); err != nil {
		t.Errorf("the daemon's own create was capped: %v", err)
	}
}

// pastFires sets up a recurring automation as if its timer had stopped
// (the computer slept) since lastFired.
func pastFires(t *testing.T, s *ScheduleStore, cronExpr string, lastFired time.Time, in ScheduleInput) *Schedule {
	t.Helper()
	off := false
	in.Cron, in.Enabled = cronExpr, &off
	if in.PaneID == "" && in.Kind != "task" {
		in.PaneID = "pane_gone"
	}
	if in.Prompt == "" {
		in.Prompt = "x"
	}
	sched, err := s.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	it := s.items[sched.ID]
	it.CreatedAt = lastFired.Add(-time.Minute)
	it.LastFiredAt = &lastFired
	it.Enabled = true
	s.mu.Unlock()
	return sched
}

func TestARunFarPastItsSlotIsRecordedAsMissedWhileAsleep(t *testing.T) {
	s := newTestScheduleStore(t)
	// Yearly on 1 Jan: slept through two of them; this fire is months late.
	sched := pastFires(t, s, "0 0 1 1 *", time.Now().AddDate(-2, 0, 0), ScheduleInput{})
	s.fire(sched.ID)
	got, _ := s.Get(sched.ID)
	if len(got.Runs) != 1 {
		t.Fatalf("runs = %+v, want one record and no run", got.Runs)
	}
	r := got.Runs[0]
	if r.Cause != runCauseMissedAsleep || r.Status != "skipped" || !regexp.MustCompile(`^\d+ runs missed while the computer was asleep$`).MatchString(r.Detail) {
		t.Fatalf("run = %+v", r)
	}
	if got.SkipCount < 2 {
		t.Errorf("skipCount = %d, want each missed run counted", got.SkipCount)
	}
}

func TestARunJustLateRunsAndTheSlotsBeforeItAreMissed(t *testing.T) {
	s := newTestScheduleStore(t)
	// Every minute (the daemon's own, so no guard): the timer stopped for
	// five minutes; this fire is under a minute late, so it runs.
	sched := pastFires(t, s, "* * * * *", time.Now().Add(-5*time.Minute-10*time.Second), ScheduleInput{})
	s.fire(sched.ID)
	got, _ := s.Get(sched.ID)
	if len(got.Runs) != 2 {
		t.Fatalf("runs = %+v, want the missed record and the run", got.Runs)
	}
	if got.Runs[1].Cause != runCauseMissedAsleep {
		t.Errorf("older record = %+v, want the missed slots", got.Runs[1])
	}
	// The run itself was tried (the test store has no panes, so it fails
	// there): not skipped as missed.
	if r := got.Runs[0]; r.Cause == runCauseMissedAsleep || r.Status == "skipped" {
		t.Errorf("newest = %+v, want the run itself, tried", r)
	}
}

func TestATaskWithCatchUpRunsOnceAfterSleep(t *testing.T) {
	fakeClaude(t, "echo 'Nothing new needs you.'\n")
	s := newTestScheduleStore(t)
	sched := pastFires(t, s, "0 0 1 1 *", time.Now().AddDate(-1, 0, -1), ScheduleInput{Kind: "task", Cwd: t.TempDir(), Permission: "plan", CatchUp: true})
	s.fire(sched.ID)
	got := waitTaskIdle(t, s, sched.ID, 2)
	if got.Runs[0].Trigger != runTriggerCatchUp || got.Runs[0].Status != "fired" {
		t.Errorf("newest = %+v, want the one catch-up run", got.Runs[0])
	}
	if got.Runs[1].Cause != runCauseMissedAsleep || !strings.Contains(got.Runs[1].Detail, "catching up with one run") {
		t.Errorf("older = %+v, want the missed record saying it catches up", got.Runs[1])
	}
}

func TestOneNoticeForAWake(t *testing.T) {
	old := asleepNoticeDelay
	asleepNoticeDelay = 700 * time.Millisecond // both fires land well inside it, even on a loaded machine
	t.Cleanup(func() { asleepNoticeDelay = old })
	bus := newUIBus()
	events := bus.subscribe()
	s, err := NewScheduleStore(t.TempDir(), nil, bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	a := pastFires(t, s, "0 0 1 1 *", time.Now().AddDate(-2, 0, 0), ScheduleInput{Name: "Yearly A"})
	b := pastFires(t, s, "0 0 1 1 *", time.Now().AddDate(-2, 0, 0), ScheduleInput{Name: "Yearly B"})
	s.fire(a.ID)
	s.fire(b.ID)
	var toasts []string
	deadline := time.After(3 * time.Second)
	for done := false; !done; {
		select {
		case e := <-events:
			if e.Type == "toast" {
				toasts = append(toasts, e.Text)
			}
		case <-deadline:
			done = true
		}
	}
	if len(toasts) != 1 || !strings.Contains(toasts[0], "and 1 more missed runs while the computer was asleep") {
		t.Fatalf("toasts = %q, want one for both", toasts)
	}
}

// The gap is worked out from the minute and hour fields, so a short gap that
// a sample of runs would only meet years later is still found.
func TestCronMinGapFindsAGapASampleMisses(t *testing.T) {
	expr := "0,59 0,2,4,6,8,10,12,14,16,18,20,23 29 2 MON"
	gap, ok := cronMinGap(expr, time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC))
	if !ok || gap != time.Minute {
		t.Fatalf("cronMinGap(%q) = %s %v, want 1m (23:59 to 00:00)", expr, gap, ok)
	}
	if err := intervalGuard(ScheduleInput{Cron: expr}, scheduleActorPerson, nil); err == nil {
		t.Error("a timing with a one-minute gap was allowed")
	}
}

// A run still going from its last slot is not counted as missed: the slot is
// recorded as "previous run still going" by the run gate.
func TestARunInFlightIsNotCountedAsMissedWhileAsleep(t *testing.T) {
	fakeClaude(t, "exit 0\n") // so the run gets as far as its slot (CI has no claude)
	s := newTestScheduleStore(t)
	sched := pastFires(t, s, "0 0 1 1 *", time.Now().AddDate(-2, 0, 0), ScheduleInput{Kind: "task", Cwd: t.TempDir(), Permission: "plan"})
	s.mu.Lock()
	s.taskRunning[sched.ID] = true
	s.mu.Unlock()
	s.fire(sched.ID)
	got, _ := s.Get(sched.ID)
	for _, r := range got.Runs {
		if r.Cause == runCauseMissedAsleep {
			t.Fatalf("counted as missed while a run was in flight: %+v", r)
		}
	}
	if len(got.Runs) != 1 || got.Runs[0].Cause != runCauseAlreadyRunning {
		t.Fatalf("runs = %+v, want one 'previous run still going'", got.Runs)
	}
}

// Past the cap of slots walked, the newest slot is still the real one, so a
// run on time is not taken for a late one.
func TestMissedSlotsNewestIsRealPastTheCap(t *testing.T) {
	now := time.Now()
	last := now.Add(-48 * time.Hour) // every minute: far more than the cap
	sched := &Schedule{Cron: "* * * * *", CreatedAt: last.Add(-time.Minute), LastFiredAt: &last}
	n, newest := missedSlots(sched, now, time.Time{})
	if n != missedFireCountCap {
		t.Fatalf("n = %d, want the cap %d", n, missedFireCountCap)
	}
	if now.Sub(newest) > time.Minute {
		t.Fatalf("newest slot %v is %s before now, want within a minute", newest, now.Sub(newest))
	}
}

// Across midnight counts only when two days in a row can both run it.
func TestCronMinGapAcrossMidnightOnlyOnAdjacentDays(t *testing.T) {
	from := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if gap, ok := cronMinGap("0,59 0,23 * * MON", from); !ok || gap != 59*time.Minute {
		t.Errorf("Mondays only: gap = %s %v, want 59m", gap, ok)
	}
	if err := intervalGuard(ScheduleInput{Cron: "0,59 0,23 * * MON"}, scheduleActorPerson, nil); err != nil {
		t.Errorf("Mondays only was refused: %v", err)
	}
	if gap, _ := cronMinGap("0,59 0,23 * * *", from); gap != time.Minute {
		t.Errorf("every day: gap = %s, want 1m (23:59 to 00:00)", gap)
	}
}

// A new timing starts its timetable at the edit: its earlier slots are not
// counted as missed.
func TestAnEditOfTheTimingStartsItsTimetableAgain(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := pastFires(t, s, "0 9 * * *", time.Now().Add(-26*time.Hour), ScheduleInput{})
	on := true
	if _, err := s.UpdateBy(sched.ID, ScheduleInput{PaneID: "pane_gone", Prompt: "x", Cron: "*/5 * * * *", Enabled: &on}, scheduleActorPerson); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(sched.ID)
	if n, _ := missedSlots(&got, time.Now().Add(6*time.Minute), time.Time{}); n > 2 {
		t.Errorf("%d slots counted after the edit, want only the new timing's (at most 2)", n)
	}
}

// A run in flight across a suspend: the slots after it started are still
// counted as missed.
func TestSlotsAfterAnInFlightRunStartedAreCounted(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := pastFires(t, s, "0 * * * *", time.Now().Add(-5*time.Hour), ScheduleInput{Kind: "task", Cwd: t.TempDir(), Permission: "plan"})
	s.mu.Lock()
	s.taskRunning[sched.ID] = true
	s.runStarted = map[string]time.Time{sched.ID: time.Now().Add(-3*time.Hour - 30*time.Minute)}
	s.mu.Unlock()
	s.fire(sched.ID)
	got, _ := s.Get(sched.ID)
	found := false
	for _, r := range got.Runs {
		if r.Cause == runCauseMissedAsleep && strings.Contains(r.Detail, "a run was still going") {
			found = true
		}
	}
	if !found {
		t.Fatalf("runs = %+v, want the slots after the run started counted as missed", got.Runs)
	}
}

// A prompt waiting for its busy pane counts the slots that come due
// meanwhile itself: they are not also "missed while asleep".
func TestSlotsDuringAPaneWaitAreNotMissedWhileAsleep(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := pastFires(t, s, "*/5 * * * *", time.Now().Add(-21*time.Minute), ScheduleInput{})
	since := time.Now().Add(-20 * time.Minute)
	s.mu.Lock()
	s.paneWaits = map[string]*paneWait{sched.ID: {since: since, until: since.Add(time.Hour), why: "the pane is busy"}}
	s.items[sched.ID].WaitingSince = &since
	s.mu.Unlock()
	s.fire(sched.ID)
	got, _ := s.Get(sched.ID)
	for _, r := range got.Runs {
		if r.Cause == runCauseMissedAsleep {
			t.Fatalf("a slot during the wait was recorded as missed while asleep: %+v", r)
		}
	}
}

// A run that overran its next slot: that slot got "previous run still
// going"; the next on-time fire, after the run ended, counts nothing missed.
func TestAnOverrunIsNotMissedWhileAsleep(t *testing.T) {
	fakeClaude(t, "exit 0\n")
	s := newTestScheduleStore(t)
	sched := pastFires(t, s, "*/5 * * * *", time.Now().Add(-12*time.Minute), ScheduleInput{Kind: "task", Cwd: t.TempDir(), Permission: "plan"})
	s.mu.Lock()
	s.taskRunning[sched.ID] = true // the long run, started 12 min ago
	s.mu.Unlock()
	s.fire(sched.ID) // its next slot: "previous run still going"
	s.mu.Lock()
	delete(s.taskRunning, sched.ID)
	s.mu.Unlock()
	// The long run ends and is recorded with its start time.
	started := time.Now().Add(-12 * time.Minute)
	s.recordRun(sched.ID, ScheduleRun{At: started, Status: "fired", Detail: "long run"})
	s.mu.Lock()
	s.items[sched.ID].LastFiredAt = &started
	s.mu.Unlock()
	if n, _ := missedSlots(func() *Schedule { g, _ := s.Get(sched.ID); return &g }(), time.Now().Add(time.Second), time.Time{}); n > 1 {
		t.Fatalf("%d slots counted after the overrun, want at most the current one", n)
	}
}

// An edit that turns a built-in agent's every-minute schedule into a prompt
// is checked against the limit.
func TestAnEditFromAnExemptKindIsChecked(t *testing.T) {
	err := intervalGuard(ScheduleInput{Kind: "prompt", Cron: "* * * * *"}, scheduleActorPerson, &Schedule{Kind: "agent", Cron: "* * * * *"})
	if err == nil {
		t.Fatal("a built-in agent's every-minute timing became a prompt's")
	}
}

func TestMissedAsleepDetailSaysPlusPastTheCap(t *testing.T) {
	if got := missedAsleepDetail(missedFireCountCap-1, missedFireCountCap); !strings.HasPrefix(got, "500+ runs") {
		t.Errorf("detail = %q, want 500+", got)
	}
}

// A run in flight and a fire far past its slot: the slots are recorded
// once; the next fire does not count them again.
func TestMissedSlotsAreRecordedOnceWhileARunIsGoing(t *testing.T) {
	s := newTestScheduleStore(t)
	sched := pastFires(t, s, "0 * * * *", time.Now().Add(-5*time.Hour), ScheduleInput{Kind: "task", Cwd: t.TempDir(), Permission: "plan"})
	started := time.Now().Add(-4*time.Hour - 30*time.Minute)
	s.mu.Lock()
	s.taskRunning[sched.ID] = true
	s.runStarted = map[string]time.Time{sched.ID: started}
	s.mu.Unlock()
	s.fire(sched.ID)
	// The long run ends and is recorded with its start time.
	s.mu.Lock()
	delete(s.taskRunning, sched.ID)
	delete(s.runStarted, sched.ID)
	s.mu.Unlock()
	s.recordRun(sched.ID, ScheduleRun{At: started, Status: "fired", Detail: "long run"})
	got, _ := s.Get(sched.ID)
	if n, _ := missedSlots(&got, time.Now().Add(time.Second), time.Time{}); n > 1 {
		t.Fatalf("%d slots counted again after they were recorded", n)
	}
	missed := 0
	for _, r := range got.Runs {
		if r.Cause == runCauseMissedAsleep {
			missed++
		}
	}
	if missed != 1 {
		t.Fatalf("missed records = %d, want one", missed)
	}
}
