package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ST-262, Automations stage 1 (daemon): one-time runs follow the wall clock,
// a prompt run waits for a busy pane instead of being dropped, and Run now
// says what happened.

// stage1Pane is a plain terminal pane (a shell) whose PTY writes the test
// reads as frames. It starts idle.
func stage1Pane(t *testing.T, id string) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames := newTypingHoldPane(t, id)
	p.forceCodexForTest = false
	p.AutoMode = ""
	setStage1PaneBusy(p, false)
	return p, frames
}

// setStage1PaneBusy makes computeState read busy (output just arrived, and
// keeps arriving) or idle (quiet for a minute).
func setStage1PaneBusy(p *Pane, busy bool) {
	at := time.Now().Add(-time.Minute)
	if busy {
		at = time.Now().Add(time.Hour)
	}
	atomic.StoreInt64(&p.LastActivityMs, at.UnixMilli())
	// Real panes turn busy by writing output, which the cached state read
	// notices. This one only moves its clock, so the cached read is dropped.
	p.stateCacheData.mu.Lock()
	p.stateCacheData.valid = false
	p.stateCacheData.mu.Unlock()
}

// stage1QuietSince makes the last scheduled prompt typed into the pane old
// enough (and the pane quiet enough) for the next one to go in.
func stage1QuietSince(store *ScheduleStore, p *Pane) {
	store.mu.Lock()
	if _, ok := store.paneLastRelease[p.ID]; ok {
		store.paneLastRelease[p.ID] = time.Now().Add(-time.Hour)
	}
	store.mu.Unlock()
	setStage1PaneBusy(p, false)
}

func stage1WaitReason(store *ScheduleStore, id string) string {
	why, _ := store.paneWaitReason(id)
	return why
}

// stage1Settle makes the test wait, before its folders are removed, for
// every attempt to type a prompt to be over: an attempt saves the schedules
// file as its last step, a moment after its run is recorded.
func stage1Settle(t *testing.T, store *ScheduleStore) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			store.mu.Lock()
			busy := len(store.promptRunning) + len(store.paneTyping)
			store.mu.Unlock()
			if busy == 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
}

func stage1Store(t *testing.T, p *Pane) *ScheduleStore {
	t.Helper()
	store, err := NewScheduleStore(t.TempDir(), testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	stage1Settle(t, store)
	return store
}

func stage1Runs(store *ScheduleStore, id string) []ScheduleRun {
	sched, _ := store.Get(id)
	return sched.Runs
}

func expectStage1Delivery(t *testing.T, frames <-chan directAutoFrameRead, body string) {
	t.Helper()
	expectFrame(t, frames, "body", "\x1b[200~"+body+"\x1b[201~")
	expectFrame(t, frames, "Enter", "\r")
}

// On one WSL PC every one-time prompt reminder ran late, by 1.5 to 55
// minutes (schedules.json, 7-12 Nov). Its wall clock gains about 5% on the
// clock Go timers count (measured 14 Nov: 47.2 s against 45.0 s), so a timer
// set for "runAt minus now" ends that much after runAt. The 30-second pass
// must run a one-time schedule once the wall clock has reached its time,
// however long its timer still has to go.
func TestOneTimeRunsWhenTheWallClockReachesItsTime(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-late-reminder")
	store := stage1Store(t, p)
	runAt := time.Now().Add(time.Hour)
	sched, err := store.Create(ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up time"})
	if err != nil {
		t.Fatal(err)
	}

	// The timer still has an hour to count; the wall clock says it is time.
	store.mu.Lock()
	store.wallClock = func() time.Time { return runAt.Add(time.Second) }
	store.mu.Unlock()
	store.tick()

	expectStage1Delivery(t, frames, "stand-up time")
	waitUntil(t, "the reminder's run", func() bool { return len(stage1Runs(store, sched.ID)) == 1 })
	if run := stage1Runs(store, sched.ID)[0]; run.Status != "fired" {
		t.Fatalf("run = %+v, want fired", run)
	}
	// Once only: later passes do not run it again.
	store.tick()
	store.tick()
	expectNoFrame(t, frames, "a second delivery")
	if runs := stage1Runs(store, sched.ID); len(runs) != 1 {
		t.Fatalf("runs = %+v, want the one", runs)
	}
}

// A weekly review was skipped on 9 Nov because its pane was busy at that
// second, and was not due again for a week: nothing retried and nobody was
// told. A scheduled prompt now waits for the pane and goes in when it is free.
func TestScheduledPromptWaitsForABusyPane(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-busy")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	off := false
	sched, err := store.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}

	store.fire(sched.ID)
	expectNoFrame(t, frames, "nothing typed into a busy pane")
	if runs := stage1Runs(store, sched.ID); len(runs) != 0 {
		t.Fatalf("runs = %+v: a busy pane must not end the run yet", runs)
	}

	// Still busy on the next pass: still waiting, still nothing typed.
	store.tick()
	expectNoFrame(t, frames, "still busy")

	setStage1PaneBusy(p, false)
	store.tick()
	expectStage1Delivery(t, frames, "review the week")
	waitUntil(t, "the delivered run", func() bool { return len(stage1Runs(store, sched.ID)) == 1 })
	if run := stage1Runs(store, sched.ID)[0]; run.Status != "fired" {
		t.Fatalf("run = %+v, want fired", run)
	}
	store.tick()
	expectNoFrame(t, frames, "delivered once")
}

func fireNowResponse(t *testing.T, store *ScheduleStore, id string) (outcome, detail string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/schedules/"+id+"/fire", nil)
	req.SetPathValue("id", id)
	(&apiServer{scheds: store}).fireSchedule(rec, req)
	if rec.Code != 200 {
		t.Fatalf("fire = %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		ID     string `json:"id"`
		RunNow struct {
			Outcome string `json:"outcome"`
			Detail  string `json:"detail"`
		} `json:"runNow"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != id {
		t.Fatalf("the response is still the schedule: %s", rec.Body)
	}
	return resp.RunNow.Outcome, resp.RunNow.Detail
}

// Run now answered 200 with the schedule whether the run started or was
// skipped, so the screen could say nothing either way.
func TestRunNowSaysStartedOrSkippedAndWhy(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-run-now")
	store := stage1Store(t, p)
	off := false
	sched, err := store.Create(ScheduleInput{Name: "check-in", Cron: "0 9 * * *", PaneID: p.ID, Prompt: "status?", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}

	// A busy pane: the person is there, so they are told at once and
	// nothing waits behind their back.
	setStage1PaneBusy(p, true)
	outcome, detail := fireNowResponse(t, store, sched.ID)
	if outcome != "skipped" || !strings.Contains(detail, "busy") {
		t.Fatalf("busy pane: outcome=%q detail=%q, want skipped and why", outcome, detail)
	}
	expectNoFrame(t, frames, "nothing typed into a busy pane")
	setStage1PaneBusy(p, false)
	store.tick()
	expectNoFrame(t, frames, "a skipped Run now is not retried later")

	done := make(chan [2]string, 1)
	go func() {
		outcome, detail := fireNowResponse(t, store, sched.ID)
		done <- [2]string{outcome, detail}
	}()
	expectStage1Delivery(t, frames, "status?")
	if got := <-done; got[0] != "started" || got[1] == "" {
		t.Fatalf("idle pane: outcome=%q detail=%q, want started and where", got[0], got[1])
	}
}

// stage1Clock is a wall clock the test moves. It starts at the real time.
type stage1Clock struct{ ahead atomic.Int64 }

func (c *stage1Clock) now() time.Time          { return time.Now().Add(time.Duration(c.ahead.Load())) }
func (c *stage1Clock) advance(d time.Duration) { c.ahead.Add(int64(d)) }

func useStage1Clock(store *ScheduleStore) *stage1Clock {
	c := &stage1Clock{}
	store.mu.Lock()
	store.wallClock = c.now
	store.mu.Unlock()
	return c
}

// stage1Pushes counts the schedule pushes the relay received. (Its server
// also sees unrelated bodiless requests to "/" now and then, ST-272; those
// are not pushes.)
func stage1Pushes(relay *pushRelay) []map[string]string {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	var out []map[string]string
	for _, p := range relay.pushes {
		if p["state"] != "" {
			out = append(out, p)
		}
	}
	return out
}

func stage1Toasts(bus *uiBus) []string {
	var out []string
	for _, e := range bus.Recent() {
		if e.Type == "toast" {
			out = append(out, e.Text)
		}
	}
	return out
}

// A timer that ends before the wall clock has reached the run's time (the
// wall clock was set back, or runs slower than the timer's) must not run the
// reminder early: it waits out the rest.
func TestOneTimeDoesNotRunBeforeItsWallClockTime(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-early-timer")
	store := stage1Store(t, p)
	runAt := time.Now().Add(time.Hour)
	sched, err := store.Create(ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up time"})
	if err != nil {
		t.Fatal(err)
	}
	var wall atomic.Int64
	wall.Store(runAt.Add(-30 * time.Millisecond).UnixNano())
	store.mu.Lock()
	store.wallClock = func() time.Time { return time.Unix(0, wall.Load()) }
	store.mu.Unlock()

	// The timer is set for 30 ms; then the wall clock goes back ten minutes.
	store.tick()
	wall.Store(runAt.Add(-10 * time.Minute).UnixNano())
	expectNoFrame(t, frames, "the reminder before its time")
	if runs := stage1Runs(store, sched.ID); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none before its time", runs)
	}
	if store.NextFire(sched.ID).IsZero() {
		t.Fatal("the reminder must still be waiting for its time")
	}

	wall.Store(runAt.Add(time.Second).UnixNano())
	store.tick()
	expectStage1Delivery(t, frames, "stand-up time")
	// The run is saved before the test's folder is removed.
	waitUntil(t, "the reminder's run", func() bool { return len(stage1Runs(store, sched.ID)) == 1 })
}

// A one-time reminder that comes due while its pane is busy used to spend
// its only chance on a skip. It waits like any scheduled prompt, and turns
// itself off only once it is delivered.
func TestOneTimeReminderWaitsForABusyPane(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-reminder-busy")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	runAt := time.Now().Add(time.Hour)
	sched, err := store.Create(ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up time"})
	if err != nil {
		t.Fatal(err)
	}
	clock := useStage1Clock(store)
	clock.advance(time.Hour + time.Second)
	store.tick()
	waitUntil(t, "the reminder to wait for its pane", func() bool { return store.waitingForPane(sched.ID) })
	if got, _ := store.Get(sched.ID); !got.Enabled || len(got.Runs) != 0 {
		t.Fatalf("while waiting: enabled=%v runs=%+v, want still on with no run", got.Enabled, got.Runs)
	}
	expectNoFrame(t, frames, "nothing typed into a busy pane")

	setStage1PaneBusy(p, false)
	store.tick()
	expectStage1Delivery(t, frames, "stand-up time")
	waitUntil(t, "the reminder to finish", func() bool { got, _ := store.Get(sched.ID); return !got.Enabled && got.FireCount == 1 })
}

// A pane that stays busy for the whole window: the run is given up, recorded
// as skipped with the reason, and the person is told once (toast and phone
// push). Runs that came due meanwhile are counted into that one record. A
// second window of the same busy stretch is recorded but not announced again.
func TestWaitForABusyPaneIsGivenUpAndThePersonIsTold(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-give-up")
	setStage1PaneBusy(p, true)
	relay := newPushRelay(t)
	bus := newUIBus()
	store, err := NewScheduleStore(t.TempDir(), testManagerWithPane(p), bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	stage1Settle(t, store)
	store.SetPusher(NewPusher(relay.srv.URL, "tok", nil, nil, nil))
	clock := useStage1Clock(store)
	off := false
	sched, err := store.Create(ScheduleInput{Name: "check-in", Cron: "*/20 * * * *", PaneID: p.ID, Prompt: "status?", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}

	store.fire(sched.ID) // 9:00 finds the pane busy
	store.fire(sched.ID) // so do 9:20
	store.fire(sched.ID) // and 9:40
	clock.advance(paneWaitWindow - time.Minute)
	store.fireAs(sched.ID, fireRetry)
	if runs := stage1Runs(store, sched.ID); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none inside the window", runs)
	}
	clock.advance(2 * time.Minute)
	store.fireAs(sched.ID, fireRetry)

	got, _ := store.Get(sched.ID)
	if len(got.Runs) != 1 || got.Runs[0].Status != "skipped" || got.Runs[0].Detail != "not sent: the pane stayed busy for 61 min (3 runs)" {
		t.Fatalf("runs = %+v, want one skip saying the pane stayed busy for the window, for 3 runs", got.Runs)
	}
	if got.SkipCount != 3 {
		t.Fatalf("skipCount = %d, want 3", got.SkipCount)
	}
	if store.waitingForPane(sched.ID) {
		t.Fatal("a given-up run must not keep waiting")
	}
	waitUntil(t, "the push for the given-up run", func() bool { return len(stage1Pushes(relay)) == 1 })
	if push := stage1Pushes(relay)[0]; push["state"] != "schedule-error" || !strings.Contains(push["summary"], "the pane stayed busy") {
		t.Errorf("push = %+v, want a schedule-error saying the pane stayed busy", push)
	}
	if toasts := stage1Toasts(bus); len(toasts) != 1 || !strings.Contains(toasts[0], "check-in not sent: the pane stayed busy") {
		t.Fatalf("toasts = %q, want the one notice", toasts)
	}

	// The same busy stretch, a second window: recorded, not announced again.
	store.fire(sched.ID)
	clock.advance(paneWaitWindow + time.Minute)
	store.fireAs(sched.ID, fireRetry)
	if runs := stage1Runs(store, sched.ID); len(runs) != 2 || runs[0].Status != "skipped" {
		t.Fatalf("runs = %+v, want a second skip", runs)
	}
	time.Sleep(100 * time.Millisecond) // a second push would have left by now
	if pushes := stage1Pushes(relay); len(pushes) != 1 {
		t.Fatalf("pushes = %+v, want still 1 for the same busy stretch", pushes)
	}
	if toasts := stage1Toasts(bus); len(toasts) != 1 {
		t.Fatalf("toasts = %q, want still 1", toasts)
	}

	// Busy again hours later is a new stretch: announced again.
	clock.advance(3 * time.Hour)
	store.fire(sched.ID)
	clock.advance(paneWaitWindow + time.Minute)
	store.fireAs(sched.ID, fireRetry)
	if toasts := stage1Toasts(bus); len(toasts) != 2 {
		t.Fatalf("toasts = %q, want a second notice for the later stretch", toasts)
	}
	expectNoFrame(t, frames, "nothing was ever typed")
}

// A retry was admitted when it first came due, so it does not spend a snooze
// the person has since put on the next run.
func TestRetryForABusyPaneDoesNotSpendASnooze(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-snooze")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	sched, err := store.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}
	store.fire(sched.ID)
	if _, err := store.SnoozeNext(sched.ID); err != nil {
		t.Fatal(err)
	}
	setStage1PaneBusy(p, false)
	go store.fireAs(sched.ID, fireRetry)
	expectStage1Delivery(t, frames, "review the week")
	waitUntil(t, "the delivered run", func() bool { return len(stage1Runs(store, sched.ID)) == 1 })
	if got, _ := store.Get(sched.ID); got.Runs[0].Status != "fired" || got.SnoozeUntil == nil {
		t.Fatalf("run=%+v snoozeUntil=%v, want delivered with the snooze kept for the next run", got.Runs[0], got.SnoozeUntil)
	}
}

// Pausing, editing or deleting an automation ends its wait: nothing is typed
// later on its behalf. A retry already on its way does nothing either, even
// if the automation has become a task meanwhile.
func TestPausingOrEditingEndsAWaitForThePane(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-pause")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	sched, err := store.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}

	store.fire(sched.ID)
	if !store.waitingForPane(sched.ID) {
		t.Fatal("control: the run should be waiting for its pane")
	}
	if _, err := store.SetEnabled(sched.ID, false); err != nil {
		t.Fatal(err)
	}
	if store.waitingForPane(sched.ID) {
		t.Fatal("pausing must end the wait")
	}

	if _, err := store.SetEnabled(sched.ID, true); err != nil {
		t.Fatal(err)
	}
	store.fire(sched.ID)
	if _, err := store.Update(sched.ID, ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "a new prompt"}); err != nil {
		t.Fatal(err)
	}
	if store.waitingForPane(sched.ID) {
		t.Fatal("an edit must end the wait")
	}

	setStage1PaneBusy(p, false)
	store.fireAs(sched.ID, fireRetry) // one that was already on its way
	store.tick()
	expectNoFrame(t, frames, "nothing typed for an ended wait")
	if runs := stage1Runs(store, sched.ID); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none", runs)
	}
}

// A pause or edit can land while a run that has just come due is still
// reading its pane. That run must not start a wait afterwards, or the paused
// automation would type into the pane up to an hour later. A retry whose wait
// predates the pause does nothing either.
func TestAPauseDuringARunStartsNoWait(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-pause-race")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	sched, err := store.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}
	// The run came due and read the pane as busy; the person then paused.
	on, dueSeq, _ := store.editStamp(sched.ID)
	if _, err := store.SetEnabled(sched.ID, false); err != nil {
		t.Fatal(err)
	}
	store.waitForPane(sched.ID, p.ID, ScheduleRun{At: time.Now(), PaneID: p.ID}, waitBusy, fireScheduled, dueStamp{seq: dueSeq, on: on}, paneWaitIdentity(p))
	if store.waitingForPane(sched.ID) {
		t.Fatal("a run that came due before the pause must not start a wait after it")
	}

	// A wait left from before the pause (however it got there) is dropped by
	// the retry, not typed.
	store.mu.Lock()
	store.paneWaits = map[string]*paneWait{sched.ID: {since: time.Now(), until: time.Now().Add(time.Hour), due: dueStamp{seq: dueSeq, on: on}, paneID: p.ID}}
	store.mu.Unlock()
	setStage1PaneBusy(p, false)
	store.fireAs(sched.ID, fireRetry)
	expectNoFrame(t, frames, "nothing typed for a paused automation")
	if store.waitingForPane(sched.ID) || len(stage1Runs(store, sched.ID)) != 0 {
		t.Fatalf("waiting=%v runs=%+v, want the stale wait dropped and no run", store.waitingForPane(sched.ID), stage1Runs(store, sched.ID))
	}
}

// A pane restarted under the same id as a different agent while a prompt
// waited for it: the prompt was meant for what ran there before, so it is
// not typed into the new one. The run is recorded and the person is told.
func TestAWaitEndsWhenThePaneBecomesADifferentAgent(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-respawn")
	setStage1PaneBusy(p, true)
	bus := newUIBus()
	mgr := testManagerWithPane(p)
	store, err := NewScheduleStore(t.TempDir(), mgr, bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	stage1Settle(t, store)
	sched, err := store.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}
	store.fire(sched.ID)
	if !store.waitingForPane(sched.ID) {
		t.Fatal("control: the run should be waiting for its pane")
	}

	// Same id, another agent, idle and ready to take a prompt.
	next, nextFrames := stage1Pane(t, "stage1-respawn")
	next.Agent = "codex"
	next.Cmd = []string{"codex"}
	mgr.mu.Lock()
	mgr.panes[p.ID] = next
	mgr.mu.Unlock()

	store.fireAs(sched.ID, fireRetry)
	expectNoFrame(t, nextFrames, "nothing typed into the new agent")
	expectNoFrame(t, frames, "nothing typed into the old pane")
	runs := stage1Runs(store, sched.ID)
	if len(runs) != 1 || runs[0].Status != "skipped" || !strings.Contains(runs[0].Detail, "restarted as a different agent") {
		t.Fatalf("runs = %+v, want one skip saying the pane was restarted as a different agent", runs)
	}
	if store.waitingForPane(sched.ID) {
		t.Fatal("the wait must be over")
	}
	if toasts := stage1Toasts(bus); len(toasts) != 1 || !strings.Contains(toasts[0], "weekly review not sent") {
		t.Fatalf("toasts = %q, want the one notice", toasts)
	}
}

// The person stops agent messaging while an agent's scheduled prompt is
// waiting for its busy pane. When the pane frees up the prompt is not typed:
// it is held under the stop's rules, and runs once when the person resumes.
// The person's own automation is not agent-to-agent and goes in as usual.
func TestAWaitingAgentPromptIsHeldWhenAgentMessagingIsPaused(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-msg-pause")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	agentSched, err := store.CreateWithOrigin(ScheduleInput{Name: "agent nudge", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "keep going"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	personSched, err := store.Create(ScheduleInput{Name: "my nudge", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "my check-in"})
	if err != nil {
		t.Fatal(err)
	}
	store.fire(agentSched.ID)
	store.fire(personSched.ID)
	if !store.waitingForPane(agentSched.ID) || !store.waitingForPane(personSched.ID) {
		t.Fatal("control: both runs should be waiting for the pane")
	}

	pauseAgentMessagingForTest(t)
	setStage1PaneBusy(p, false)
	store.fireAs(agentSched.ID, fireRetry)
	expectNoFrame(t, frames, "nothing typed for the agent's schedule while agent messaging is paused")
	got, _ := store.Get(agentSched.ID)
	if got.Enabled || got.PausedReason != agentMessagingScheduleReason || len(got.Runs) != 0 {
		t.Fatalf("agent schedule: enabled=%v reason=%q runs=%+v, want held without a run", got.Enabled, got.PausedReason, got.Runs)
	}
	if store.waitingForPane(agentSched.ID) {
		t.Fatal("a held schedule must not keep waiting for the pane")
	}

	go store.fireAs(personSched.ID, fireRetry)
	expectStage1Delivery(t, frames, "my check-in")
	waitUntil(t, "the person's run", func() bool { return len(stage1Runs(store, personSched.ID)) == 1 })

	// The person's prompt went into the pane a moment ago, so the held one
	// first waits for the pane's next quiet period, like any other.
	resumeAgentMessagingForTest(t)
	store.resumeAgentMessagingHeld(0)
	waitUntil(t, "the released prompt to wait for a quiet pane", func() bool { return store.waitingForPane(agentSched.ID) })
	expectNoFrame(t, frames, "nothing typed straight after another prompt")
	stage1QuietSince(store, p)
	store.tick()
	expectStage1Delivery(t, frames, "keep going")
	waitUntil(t, "the held run on resume", func() bool { return len(stage1Runs(store, agentSched.ID)) == 1 })
	if got, _ := store.Get(agentSched.ID); got.Runs[0].Status != "fired" || !got.Enabled || got.PausedReason != "" {
		t.Fatalf("after resume: run=%+v enabled=%v reason=%q, want delivered once and on again", got.Runs[0], got.Enabled, got.PausedReason)
	}
}

// While one attempt is typing a schedule's prompt, a second attempt for the
// same schedule (the retry and the timetable landing together) types nothing
// and is recorded as skipped with the reason.
func TestOnlyOneAttemptTypesAtATime(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-one-attempt")
	store := stage1Store(t, p)
	off := false
	sched, err := store.Create(ScheduleInput{Name: "check-in", Cron: "*/5 * * * *", PaneID: p.ID, Prompt: "status?", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.promptRunning = map[string]bool{sched.ID: true}
	store.mu.Unlock()
	store.fire(sched.ID)
	expectNoFrame(t, frames, "a second attempt")
	if runs := stage1Runs(store, sched.ID); len(runs) != 1 || runs[0].Status != "skipped" || !strings.Contains(runs[0].Detail, "another run of this automation is being sent") {
		t.Fatalf("runs = %+v, want the second attempt skipped with the reason", runs)
	}
	if outcome, detail := fireNowResponse(t, store, sched.ID); outcome != "skipped" || !strings.Contains(detail, "another run") {
		t.Fatalf("Run now during an attempt: outcome=%q detail=%q, want skipped and why", outcome, detail)
	}
	store.endPromptRun(sched.ID, p.ID)
	go store.fire(sched.ID)
	expectStage1Delivery(t, frames, "status?")
	waitUntil(t, "the attempt to end", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return !store.promptRunning[sched.ID] && len(store.items[sched.ID].Runs) == 3
	})
}

// Run now on a task: "started" while the run is going (its record lands when
// it ends), and "failed" with the reason when it cannot start.
func TestRunNowOnATaskSaysStartedOrFailed(t *testing.T) {
	fakeClaude(t, "sleep 0.3\necho done\n")
	work := t.TempDir()
	store, err := NewScheduleStore(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	stage1Settle(t, store)
	off := false
	sched, err := store.Create(ScheduleInput{Kind: "task", Name: "digest", Cron: "0 9 * * *", Prompt: "digest", Cwd: work, Permission: "plan", Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, detail := fireNowResponse(t, store, sched.ID); outcome != "started" {
		t.Fatalf("outcome=%q detail=%q, want started", outcome, detail)
	}
	waitTaskIdle(t, store, sched.ID, 1)

	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	if outcome, detail := fireNowResponse(t, store, sched.ID); outcome != "failed" || !strings.Contains(detail, "working directory is gone") {
		t.Fatalf("outcome=%q detail=%q, want failed: working directory is gone", outcome, detail)
	}
}

// Several automations waiting for ONE pane used to go in on the same poll,
// back to back: each read the pane as free before any of them typed. A pane
// now takes one prompt per quiet period, oldest first; the others see it as
// taken, then busy, and wait for the next quiet period.
func TestOnePromptPerPanePerQuietPeriod(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-two-on-one")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	first, err := store.Create(ScheduleInput{Name: "first", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "first prompt"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ScheduleInput{Name: "second", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "second prompt"})
	if err != nil {
		t.Fatal(err)
	}
	store.fire(first.ID)
	time.Sleep(2 * time.Millisecond) // the second began waiting later
	store.fire(second.ID)
	if !store.waitingForPane(first.ID) || !store.waitingForPane(second.ID) {
		t.Fatal("control: both runs should be waiting for the pane")
	}

	// The pane goes quiet. One poll: the older prompt goes in, alone.
	setStage1PaneBusy(p, false)
	store.tick()
	expectStage1Delivery(t, frames, "first prompt")
	waitUntil(t, "the first run", func() bool { return len(stage1Runs(store, first.ID)) == 1 })
	expectNoFrame(t, frames, "the second prompt on the same poll")
	if why := stage1WaitReason(store, second.ID); why != waitQueue.row {
		t.Fatalf("the second waits because %q, want %q", why, waitQueue.row)
	}
	// More polls right after: the pane was just given a prompt.
	store.tick()
	store.tick()
	expectNoFrame(t, frames, "the second prompt right after the first")

	// The pane works on the first prompt. However long ago that prompt went
	// in, the second waits while the pane is busy.
	stage1QuietSince(store, p)
	setStage1PaneBusy(p, true)
	store.tick()
	expectNoFrame(t, frames, "the second prompt into a busy pane")
	waitUntil(t, "the second to see the pane busy", func() bool { return stage1WaitReason(store, second.ID) == waitBusy.row })

	// Quiet again: now the second goes in.
	setStage1PaneBusy(p, false)
	store.tick()
	expectStage1Delivery(t, frames, "second prompt")
	waitUntil(t, "the second run", func() bool { return len(stage1Runs(store, second.ID)) == 1 })
	// (The wait ends just after the run is recorded.)
	waitUntil(t, "both waits to end", func() bool { return !store.waitingForPane(first.ID) && !store.waitingForPane(second.ID) })
}

// Two automations due at the same minute into an idle pane: the first types,
// the second waits for the pane's next quiet period instead of typing behind
// it. An automation that comes due while others wait joins the back.
func TestAutomationsDueTogetherGoInOneAtATime(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-due-together")
	store := stage1Store(t, p)
	mk := func(name, prompt string) *Schedule {
		sched, err := store.Create(ScheduleInput{Name: name, Cron: "0 9 * * *", PaneID: p.ID, Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		return sched
	}
	first, second, third := mk("first", "first prompt"), mk("second", "second prompt"), mk("third", "third prompt")

	go store.fire(first.ID)
	expectStage1Delivery(t, frames, "first prompt")
	waitUntil(t, "the first run", func() bool { return len(stage1Runs(store, first.ID)) == 1 })
	store.fire(second.ID)
	time.Sleep(2 * time.Millisecond)
	store.fire(third.ID)
	expectNoFrame(t, frames, "a prompt typed behind the first")
	if !store.waitingForPane(second.ID) || !store.waitingForPane(third.ID) {
		t.Fatal("the second and third should wait for the pane's next quiet period")
	}

	stage1QuietSince(store, p)
	store.tick()
	expectStage1Delivery(t, frames, "second prompt")
	waitUntil(t, "the second run", func() bool { return len(stage1Runs(store, second.ID)) == 1 })
	expectNoFrame(t, frames, "the third on the same poll")
	stage1QuietSince(store, p)
	store.tick()
	expectStage1Delivery(t, frames, "third prompt")
	waitUntil(t, "the third run", func() bool { return len(stage1Runs(store, third.ID)) == 1 })
}

// A released prompt could land in the person's half-typed text and submit it
// (tracker #35): "not busy" says nothing about the person. A scheduled prompt
// is not typed while they type in the pane; it waits in the same wait as for
// a busy pane, and goes in when they stop. Run now says why at once and
// leaves the waiting run alone.
func TestScheduledPromptWaitsWhileThePersonTypes(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-typing")
	store := stage1Store(t, p)
	sched, err := store.Create(ScheduleInput{Name: "check-in", Cron: "0 9 * * *", PaneID: p.ID, Prompt: "status?"})
	if err != nil {
		t.Fatal(err)
	}

	p.noteHumanInput(time.Now())
	store.fire(sched.ID)
	expectNoFrame(t, frames, "nothing typed while the person types")
	if runs := stage1Runs(store, sched.ID); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none while it waits", runs)
	}
	if why := stage1WaitReason(store, sched.ID); why != waitTyped.row {
		t.Fatalf("waiting because %q, want %q", why, waitTyped.row)
	}
	got, _ := store.Get(sched.ID)
	if dto := (&apiServer{scheds: store}).makeScheduleDTO(got); !dto.WaitingForPane || dto.WaitingReason != "you are typing in the pane" {
		t.Fatalf("row: waiting=%v reason=%q, want it to say why", dto.WaitingForPane, dto.WaitingReason)
	}
	store.mu.Lock()
	since := store.paneWaits[sched.ID].since
	store.mu.Unlock()

	// They keep typing through three more polls: the same wait, not a new
	// hour each time, and still nothing typed.
	for i := 0; i < 3; i++ {
		p.noteHumanInput(time.Now())
		store.fireAs(sched.ID, fireRetry)
	}
	expectNoFrame(t, frames, "nothing typed on the retries")
	store.mu.Lock()
	w := store.paneWaits[sched.ID]
	store.mu.Unlock()
	if w == nil || !w.since.Equal(since) {
		t.Fatalf("wait = %+v, want the one that began at %v", w, since)
	}

	// Run now while they type: told at once, nothing typed, and the
	// scheduled run goes on waiting.
	if outcome, detail := fireNowResponse(t, store, sched.ID); outcome != "skipped" || detail != "you are typing in the pane" {
		t.Fatalf("Run now: outcome=%q detail=%q, want skipped and why", outcome, detail)
	}
	expectNoFrame(t, frames, "nothing typed by Run now")
	if !store.waitingForPane(sched.ID) {
		t.Fatal("a skipped Run now must not end the scheduled run's wait")
	}

	// They stop typing: it goes in.
	p.noteHumanInput(time.Now().Add(-time.Minute))
	store.tick()
	expectStage1Delivery(t, frames, "status?")
	waitUntil(t, "the delivered run", func() bool { return len(stage1Runs(store, sched.ID)) == 2 })
	if run := stage1Runs(store, sched.ID)[0]; run.Status != "fired" {
		t.Fatalf("run = %+v, want fired", run)
	}
	// (The wait ends just after the run is recorded.)
	waitUntil(t, "the delivered run's wait to end", func() bool { return !store.waitingForPane(sched.ID) })
}

// The person types for the whole window: given up once, with the reason.
func TestTypingForTheWholeWindowIsGivenUp(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-typing-give-up")
	bus := newUIBus()
	store, err := NewScheduleStore(t.TempDir(), testManagerWithPane(p), bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	stage1Settle(t, store)
	clock := useStage1Clock(store)
	// Never named: its name is the start of its prompt, which a notice must
	// not show.
	sched, err := store.Create(ScheduleInput{Cron: "0 9 * * *", PaneID: p.ID, Prompt: "summarise the private notes"})
	if err != nil {
		t.Fatal(err)
	}
	p.noteHumanInput(time.Now())
	store.fire(sched.ID)
	clock.advance(paneWaitWindow + time.Minute)
	p.noteHumanInput(time.Now())
	store.fireAs(sched.ID, fireRetry)

	runs := stage1Runs(store, sched.ID)
	if len(runs) != 1 || runs[0].Status != "skipped" || runs[0].Detail != "not sent: you were typing in the pane for 61 min" {
		t.Fatalf("runs = %+v, want one skip saying they were typing for the window", runs)
	}
	toasts := stage1Toasts(bus)
	if len(toasts) != 1 || !strings.Contains(toasts[0], "Automation not sent: you were typing") || strings.Contains(toasts[0], "private notes") {
		t.Fatalf("toasts = %q, want one notice that names it \"Automation\", not by its prompt", toasts)
	}
	expectNoFrame(t, frames, "nothing was ever typed")
}

// An agent pane whose input box holds the person's draft: the same hold, read
// off the screen under the writer lock.
func TestScheduledPromptWaitsWhileTheInputBoxHasText(t *testing.T) {
	p, frames, screen := newDraftGuardPane(t, "stage1-draft")
	p.noteHumanInput(time.Now().Add(-time.Minute))
	screen.Store(deliveryCodexScreen("I want the release updates and inf"))
	store := stage1Store(t, p)
	sched, err := store.Create(ScheduleInput{Name: "check-in", Cron: "0 9 * * *", PaneID: p.ID, Prompt: "status?"})
	if err != nil {
		t.Fatal(err)
	}
	store.fire(sched.ID)
	expectNoFrame(t, frames, "nothing typed onto the draft")
	if why := stage1WaitReason(store, sched.ID); why != waitDraft.row {
		t.Fatalf("waiting because %q, want %q", why, waitDraft.row)
	}
	if runs := stage1Runs(store, sched.ID); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none while it waits", runs)
	}
}

// A run that waited for its pane and then cannot go in for a reason that will
// not pass (here the pane has exited) is told to the person: nothing was said
// while it waited, so a quiet skip would lose the run without a word.
func TestASkipAfterAWaitIsAnnounced(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-skip-after-wait")
	setStage1PaneBusy(p, true)
	bus := newUIBus()
	store, err := NewScheduleStore(t.TempDir(), testManagerWithPane(p), bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	stage1Settle(t, store)
	sched, err := store.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}
	store.fire(sched.ID)
	if !store.waitingForPane(sched.ID) {
		t.Fatal("control: the run should be waiting")
	}
	p.mu.Lock()
	p.Status = PaneExited
	p.mu.Unlock()
	store.fireAs(sched.ID, fireRetry)

	runs := stage1Runs(store, sched.ID)
	if len(runs) != 1 || runs[0].Status != "skipped" || !strings.Contains(runs[0].Detail, "exited") {
		t.Fatalf("runs = %+v, want one skip naming the exited pane", runs)
	}
	if toasts := stage1Toasts(bus); len(toasts) != 1 || !strings.Contains(toasts[0], "weekly review") {
		t.Fatalf("toasts = %q, want the person told once", toasts)
	}
	if store.waitingForPane(sched.ID) {
		t.Fatal("the wait must be over")
	}
	expectNoFrame(t, frames, "nothing typed into an exited pane")

	// The same skip without a wait stays quiet, as before.
	store.fire(sched.ID)
	if toasts := stage1Toasts(bus); len(toasts) != 1 {
		t.Fatalf("toasts = %q, want no notice for a skip that never waited", toasts)
	}
}

// A one-time automation waiting for its pane is past its time, so editing it
// was refused ("runAt must be in the future") unless its time was moved. The
// edit is now taken, and the edited prompt is the one that goes in.
func TestAWaitingOneTimeCanBeEdited(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-edit-waiting")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	runAt := time.Now().Add(300 * time.Millisecond)
	sched, err := store.Create(ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up time"})
	if err != nil {
		t.Fatal(err)
	}
	// Not waiting yet: an edit to a past time is still refused.
	past := time.Now().Add(-time.Minute)
	if _, err := store.Update(sched.ID, ScheduleInput{Name: "stand-up", RunAt: &past, PaneID: p.ID, Prompt: "x"}); err == nil {
		t.Fatal("a past time must still be refused")
	}

	// Its time comes (for real: the edit's check reads the real clock), and
	// it waits for the busy pane.
	waitUntil(t, "the reminder to wait for its pane", func() bool { return store.waitingForPane(sched.ID) })
	if !runAt.Before(time.Now()) {
		t.Fatal("control: the reminder's time should be past")
	}

	if _, err := store.Update(sched.ID, ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up, room 2"}); err != nil {
		t.Fatalf("editing a waiting one-time: %v", err)
	}
	// The edited run is due at once and meets the same busy pane.
	waitUntil(t, "the edited reminder to wait for its pane", func() bool { return store.waitingForPane(sched.ID) })
	expectNoFrame(t, frames, "nothing typed into a busy pane")
	setStage1PaneBusy(p, false)
	store.tick()
	expectStage1Delivery(t, frames, "stand-up, room 2")
	waitUntil(t, "the reminder to finish", func() bool { got, _ := store.Get(sched.ID); return !got.Enabled && got.FireCount == 1 })
}

// ST-274. A wait lived in memory only, so a daemon restart, an update's
// included, lost the run without a word: a one-time reminder was recorded as
// "missed while daemon was down" with no notice. The wait is saved with the
// automation and taken up again after the restart.
func shortResumeDelay(t *testing.T, d time.Duration) {
	t.Helper()
	prev := paneWaitResumeDelay
	paneWaitResumeDelay = d
	t.Cleanup(func() { paneWaitResumeDelay = prev })
}

func TestAWaitForThePaneSurvivesARestart(t *testing.T) {
	shortResumeDelay(t, 10*time.Millisecond)
	p, frames := stage1Pane(t, "stage1-restart")
	setStage1PaneBusy(p, true)
	dir := t.TempDir()
	first, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Stop)
	stage1Settle(t, first)
	runAt := time.Now().Add(300 * time.Millisecond)
	sched, err := first.Create(ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up time"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the reminder to wait for its pane", func() bool { return first.waitingForPane(sched.ID) })
	if got, _ := first.Get(sched.ID); got.WaitingSince == nil {
		t.Fatal("the wait must be saved with the automation")
	}
	first.Stop()

	// The daemon comes back; the pane is still busy.
	second, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Stop)
	stage1Settle(t, second)
	got, _ := second.Get(sched.ID)
	if !got.Enabled || len(got.Runs) != 0 {
		t.Fatalf("after the restart: enabled=%v runs=%+v, want still on and not recorded as missed", got.Enabled, got.Runs)
	}
	waitUntil(t, "the wait to be taken up again", func() bool { return second.waitingForPane(sched.ID) })
	expectNoFrame(t, frames, "nothing typed into a busy pane")

	setStage1PaneBusy(p, false)
	second.tick()
	expectStage1Delivery(t, frames, "stand-up time")
	waitUntil(t, "the reminder to finish", func() bool {
		got, _ := second.Get(sched.ID)
		return !got.Enabled && got.FireCount == 1 && got.WaitingSince == nil && !second.waitingForPane(sched.ID)
	})
}

// A wait already past its hour when the daemon comes back is given up at
// once, and the person is told by toast and phone push.
func TestAWaitPastItsHourAtRestartIsGivenUpWithANotice(t *testing.T) {
	shortResumeDelay(t, 150*time.Millisecond)
	p, frames := stage1Pane(t, "stage1-restart-late")
	setStage1PaneBusy(p, true)
	dir := t.TempDir()
	first, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Stop)
	stage1Settle(t, first)
	sched, err := first.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}
	first.fire(sched.ID)
	// It began waiting two hours ago.
	first.mu.Lock()
	old := time.Now().Add(-2 * time.Hour)
	first.items[sched.ID].WaitingSince = &old
	_ = first.saveUnlocked()
	first.mu.Unlock()
	first.Stop()

	relay := newPushRelay(t)
	bus := newUIBus()
	second, err := NewScheduleStore(dir, testManagerWithPane(p), bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Stop)
	stage1Settle(t, second)
	second.SetPusher(NewPusher(relay.srv.URL, "tok", nil, nil, nil))

	waitUntil(t, "the given-up run", func() bool { return len(stage1Runs(second, sched.ID)) == 1 })
	got, _ := second.Get(sched.ID)
	if run := got.Runs[0]; run.Status != "skipped" || run.Detail != "not sent: SoluTerminal restarted while the prompt waited for its pane" {
		t.Fatalf("run = %+v, want a skip saying the restart ended the wait", run)
	}
	if got.WaitingSince != nil || second.waitingForPane(sched.ID) || !got.Enabled {
		t.Fatalf("waitingSince=%v waiting=%v enabled=%v, want the wait over and the weekly automation still on", got.WaitingSince, second.waitingForPane(sched.ID), got.Enabled)
	}
	waitUntil(t, "the push", func() bool { return len(stage1Pushes(relay)) == 1 })
	if toasts := stage1Toasts(bus); len(toasts) != 1 || !strings.Contains(toasts[0], "weekly review not sent: SoluTerminal restarted") {
		t.Fatalf("toasts = %q, want the one notice", toasts)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// Pausing an automation clears its saved wait too, so a restart cannot bring
// the wait back.
func TestPausingClearsTheSavedWait(t *testing.T) {
	shortResumeDelay(t, 10*time.Millisecond)
	p, frames := stage1Pane(t, "stage1-restart-paused")
	setStage1PaneBusy(p, true)
	dir := t.TempDir()
	first, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Stop)
	stage1Settle(t, first)
	sched, err := first.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}
	first.fire(sched.ID)
	if _, err := first.SetEnabled(sched.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := first.Get(sched.ID); got.WaitingSince != nil {
		t.Fatal("pausing must clear the saved wait")
	}
	if _, err := first.SetEnabled(sched.ID, true); err != nil {
		t.Fatal(err)
	}
	first.Stop()

	setStage1PaneBusy(p, false)
	second, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Stop)
	stage1Settle(t, second)
	time.Sleep(60 * time.Millisecond) // the resume delay has passed
	second.tick()
	expectNoFrame(t, frames, "nothing typed for a wait that was ended")
}

// An attempt in flight when a one-time reminder gets its run from another
// attempt (and is turned off by it) must not start a wait afterwards: the
// retry would type the reminder a second time.
func TestAReminderThatHasRunStartsNoWait(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-spent")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	runAt := time.Now().Add(time.Hour)
	sched, err := store.Create(ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up time"})
	if err != nil {
		t.Fatal(err)
	}
	// One attempt came due while the reminder was on...
	on, seq, _ := store.editStamp(sched.ID)
	if !on {
		t.Fatal("control: the reminder should be on")
	}
	// ...another delivered it, which turns a one-time off...
	store.recordRun(sched.ID, ScheduleRun{At: time.Now(), Status: "fired", Detail: "delivered"})
	if got, _ := store.Get(sched.ID); got.Enabled {
		t.Fatal("control: a one-time is off after its run")
	}
	// ...and the first now finds the pane busy.
	store.waitForPane(sched.ID, p.ID, ScheduleRun{At: time.Now(), PaneID: p.ID}, waitBusy, fireScheduled, dueStamp{seq: seq, on: on}, paneWaitIdentity(p))
	if store.waitingForPane(sched.ID) {
		t.Fatal("a reminder that has had its run must not start a wait")
	}
	setStage1PaneBusy(p, false)
	store.tick()
	expectNoFrame(t, frames, "the reminder a second time")
}

// A repeating automation that was waiting when the daemon stopped: the
// waiting run is not a missed one, but the runs that came due after it,
// while the app was off, are still counted and recorded.
func TestRunsMissedAfterAWaitingRunAreStillCounted(t *testing.T) {
	shortResumeDelay(t, 10*time.Millisecond)
	p, frames := stage1Pane(t, "stage1-restart-missed")
	setStage1PaneBusy(p, true)
	dir := t.TempDir()
	first, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Stop)
	stage1Settle(t, first)
	sched, err := first.Create(ScheduleInput{Name: "check-in", Cron: "*/5 * * * *", PaneID: p.ID, Prompt: "status?"})
	if err != nil {
		t.Fatal(err)
	}
	first.fire(sched.ID)
	// Created three hours ago; began waiting twenty minutes ago.
	first.mu.Lock()
	it := first.items[sched.ID]
	it.CreatedAt = time.Now().Add(-3 * time.Hour)
	it.LastArmedAt = nil
	since := time.Now().Add(-20 * time.Minute)
	it.WaitingSince = &since
	_ = first.saveUnlocked()
	first.mu.Unlock()
	first.Stop()

	second, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Stop)
	stage1Settle(t, second)
	runs := stage1Runs(second, sched.ID)
	if len(runs) != 1 || runs[0].Status != "skipped" || !strings.Contains(runs[0].Detail, "missed while the app was off") {
		t.Fatalf("runs = %+v, want the runs missed after the waiting one recorded", runs)
	}
	// Twenty minutes of a five-minute timetable, not three hours of it.
	if d := runs[0].Detail; !strings.HasPrefix(d, "3 runs ") && !strings.HasPrefix(d, "4 runs ") {
		t.Fatalf("detail = %q, want 3 or 4 runs counted from when the wait began", d)
	}
	waitUntil(t, "the wait to be taken up again", func() bool { return second.waitingForPane(sched.ID) })
	second.mu.Lock()
	w := second.paneWaits[sched.ID]
	second.mu.Unlock()
	if w == nil || !w.since.Equal(since) {
		t.Fatalf("wait = %+v, want it to go on from %v", w, since)
	}
	expectNoFrame(t, frames, "nothing typed into a busy pane")
}

// A run that comes due after a restart, before the saved wait is taken up
// again, is that waiting run: it is not typed into a pane that came back as
// a different agent.
func TestARunDueBeforeTheSavedWaitResumesKeepsItsPaneCheck(t *testing.T) {
	shortResumeDelay(t, time.Hour) // never taken up in this test
	p, frames := stage1Pane(t, "stage1-restart-identity")
	setStage1PaneBusy(p, true)
	dir := t.TempDir()
	first, err := NewScheduleStore(dir, testManagerWithPane(p), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Stop)
	stage1Settle(t, first)
	sched, err := first.Create(ScheduleInput{Name: "weekly review", Cron: "0 9 * * 1", PaneID: p.ID, Prompt: "review the week"})
	if err != nil {
		t.Fatal(err)
	}
	first.fire(sched.ID)
	if !first.waitingForPane(sched.ID) {
		t.Fatal("control: the run should be waiting")
	}
	first.Stop()

	// The pane comes back under the same id as another agent, idle.
	next, nextFrames := stage1Pane(t, "stage1-restart-identity")
	next.Agent = "codex"
	next.Cmd = []string{"codex"}
	second, err := NewScheduleStore(dir, testManagerWithPane(next), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Stop)
	stage1Settle(t, second)
	second.fire(sched.ID)
	expectNoFrame(t, nextFrames, "nothing typed into the new agent")
	expectNoFrame(t, frames, "nothing typed into the old pane")
	got, _ := second.Get(sched.ID)
	if len(got.Runs) != 1 || got.Runs[0].Status != "skipped" || !strings.Contains(got.Runs[0].Detail, "restarted as a different agent") {
		t.Fatalf("runs = %+v, want one skip saying the pane was restarted as a different agent", got.Runs)
	}
	if got.WaitingSince != nil {
		t.Fatal("the saved wait must be over")
	}
}

// Run now on a one-time reminder that types nothing (its pane is busy) used
// to spend the reminder: any attempt turned a one-time off, so "Skipped: the
// pane is busy" also meant the reminder would never come. A skipped Run now
// leaves it as it was: still waiting for its pane, or still set for its time.
func TestASkippedRunNowDoesNotSpendAReminder(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-run-now-reminder")
	setStage1PaneBusy(p, true)
	relay := newPushRelay(t)
	store := stage1Store(t, p)
	store.SetPusher(NewPusher(relay.srv.URL, "tok", nil, nil, nil))

	// One that is waiting for its busy pane.
	runAt := time.Now().Add(300 * time.Millisecond)
	waiting, err := store.Create(ScheduleInput{Name: "stand-up", RunAt: &runAt, PaneID: p.ID, Prompt: "stand-up time"})
	if err != nil {
		t.Fatal(err)
	}
	// One whose time has not come.
	later := time.Now().Add(time.Hour)
	armed, err := store.Create(ScheduleInput{Name: "lunch", RunAt: &later, PaneID: p.ID, Prompt: "lunch time"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the reminder to wait for its pane", func() bool { return store.waitingForPane(waiting.ID) })

	for _, sched := range []*Schedule{waiting, armed} {
		if outcome, detail := fireNowResponse(t, store, sched.ID); outcome != "skipped" || detail != "the pane is busy" {
			t.Fatalf("%s: Run now outcome=%q detail=%q, want skipped: the pane is busy", sched.Name, outcome, detail)
		}
	}
	expectNoFrame(t, frames, "nothing typed into a busy pane")
	got, _ := store.Get(waiting.ID)
	if !got.Enabled || !store.waitingForPane(waiting.ID) || got.WaitingSince == nil {
		t.Fatalf("waiting reminder after a skipped Run now: enabled=%v waiting=%v saved=%v, want still on and still waiting", got.Enabled, store.waitingForPane(waiting.ID), got.WaitingSince != nil)
	}
	if got, _ := store.Get(armed.ID); !got.Enabled || !store.NextFire(armed.ID).Equal(later) {
		t.Fatalf("armed reminder after a skipped Run now: enabled=%v next=%v, want still set for %v", got.Enabled, store.NextFire(armed.ID), later)
	}
	// The person pressed Run now and was told on screen: no phone push.
	time.Sleep(100 * time.Millisecond)
	if pushes := stage1Pushes(relay); len(pushes) != 0 {
		t.Fatalf("pushes = %+v, want none for a skipped Run now", pushes)
	}

	// The waiting one still goes in when the pane is free, and is then spent.
	setStage1PaneBusy(p, false)
	store.tick()
	expectStage1Delivery(t, frames, "stand-up time")
	waitUntil(t, "the reminder to finish", func() bool { got, _ := store.Get(waiting.ID); return !got.Enabled && got.FireCount == 1 })

	// A Run now that delivers still spends a reminder.
	stage1QuietSince(store, p)
	done := make(chan string, 1)
	go func() { outcome, _ := fireNowResponse(t, store, armed.ID); done <- outcome }()
	expectStage1Delivery(t, frames, "lunch time")
	if outcome := <-done; outcome != "started" {
		t.Fatalf("Run now on an idle pane: %q, want started", outcome)
	}
	if got, _ := store.Get(armed.ID); got.Enabled {
		t.Fatal("a delivered Run now spends the reminder")
	}
}

// A reminder an agent filed, held by the stop on agent messaging when its
// time came. Run now releases the hold; if that Run now then types nothing,
// the reminder is on with its time past and nothing to run it. It now waits
// for its pane, like any run that came due.
func TestASkippedRunNowOnAHeldReminderLeavesItWaiting(t *testing.T) {
	p, frames := stage1Pane(t, "stage1-run-now-held")
	setStage1PaneBusy(p, true)
	store := stage1Store(t, p)
	pauseAgentMessagingForTest(t)
	runAt := time.Now().Add(300 * time.Millisecond)
	sched, err := store.CreateWithOrigin(ScheduleInput{Name: "agent reminder", RunAt: &runAt, PaneID: p.ID, Prompt: "keep going"}, &ScheduleOrigin{Source: "mcp", PaneID: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the reminder to be held", func() bool {
		got, _ := store.Get(sched.ID)
		return got.PausedReason == agentMessagingScheduleReason
	})

	if outcome, detail := fireNowResponse(t, store, sched.ID); outcome != "skipped" || detail != "the pane is busy" {
		t.Fatalf("Run now outcome=%q detail=%q, want skipped: the pane is busy", outcome, detail)
	}
	got, _ := store.Get(sched.ID)
	if !got.Enabled || got.PausedReason != "" || !store.waitingForPane(sched.ID) {
		t.Fatalf("after the skipped Run now: enabled=%v reason=%q waiting=%v, want on and waiting for its pane", got.Enabled, got.PausedReason, store.waitingForPane(sched.ID))
	}
	expectNoFrame(t, frames, "nothing typed into a busy pane")

	resumeAgentMessagingForTest(t)
	setStage1PaneBusy(p, false)
	store.tick()
	expectStage1Delivery(t, frames, "keep going")
	waitUntil(t, "the reminder to finish", func() bool { got, _ := store.Get(sched.ID); return !got.Enabled && got.FireCount == 1 })
}
