package main

import (
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// ST-262 follow-up: recurring automations ran late on a machine whose wall
// clock runs ahead of the clock Go timers count. The cron library sleeps on
// one such timer until the next entry is due, and reads the wall clock again
// only when it wakes: for a run, or when an entry is added or removed. So the
// lateness grows with the gap between two wake-ups (5% of it on one WSL PC:
// up to 39 s with many automations, minutes with a single daily one). The
// 30-second pass now wakes the loop, which bounds the gap to 30 seconds.

// cronLoopLog records what the cron loop reports about itself.
type cronLoopLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *cronLoopLog) Info(msg string, _ ...interface{}) {
	l.mu.Lock()
	l.lines = append(l.lines, msg)
	l.mu.Unlock()
}

func (l *cronLoopLog) Error(error, string, ...interface{}) {}

func (l *cronLoopLog) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if line == msg {
			n++
		}
	}
	return n
}

func TestThe30SecondPassWakesTheCronLoop(t *testing.T) {
	store, err := NewScheduleStore(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	// The same cron as NewScheduleStore builds, plus a log of its loop.
	loop := &cronLoopLog{}
	<-store.cron.Stop().Done()
	store.mu.Lock()
	store.cron = cron.New(cron.WithLocation(time.Local), cron.WithParser(cronParser), cron.WithLogger(loop))
	store.mu.Unlock()
	store.cron.Start()

	sched, err := store.Create(ScheduleInput{Name: "daily", Cron: "0 9 * * *", PaneID: "p1", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	next := store.NextFire(sched.ID)
	if next.IsZero() {
		t.Fatal("control: the schedule should be armed")
	}

	// The loop reports "removed" when it has stopped its timer and read the
	// wall clock again for a removal; removing an entry that does not exist
	// is how the pass wakes it without changing anything.
	// (The loop writes that line just after it takes the hand-off, so the
	// test waits for it.)
	store.tick()
	waitUntil(t, "the cron loop to read the clock again after one pass", func() bool { return loop.count("removed") == 1 })
	store.tick()
	waitUntil(t, "the cron loop to read the clock again after two passes", func() bool { return loop.count("removed") == 2 })
	// Nothing about the schedule changed, and nothing ran.
	if got := store.NextFire(sched.ID); !got.Equal(next) {
		t.Fatalf("next run moved from %v to %v", next, got)
	}
	if runs := stage1Runs(store, sched.ID); len(runs) != 0 {
		t.Fatalf("runs = %+v, want none", runs)
	}

	// After the store stops, a late pass is harmless.
	store.Stop()
	store.tick()
}
