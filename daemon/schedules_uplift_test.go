package main

// Tests for the schedules uplift (2026-07-09): missed-run visibility,
// per-run agent logs + the runlog endpoint, and schedule push notifications.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCountMissedFires(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.Local)
	hoursAgo := func(h float64) *time.Time {
		x := now.Add(-time.Duration(h * float64(time.Hour)))
		return &x
	}
	tests := []struct {
		name  string
		sched Schedule
		want  int
	}{
		{
			name:  "hourly, last fired 3.5h ago",
			sched: Schedule{Cron: "0 * * * *", CreatedAt: now.Add(-48 * time.Hour), LastFiredAt: hoursAgo(3.5)},
			want:  3,
		},
		{
			name:  "no gap",
			sched: Schedule{Cron: "0 * * * *", CreatedAt: now.Add(-48 * time.Hour), LastFiredAt: hoursAgo(0.5)},
			want:  0,
		},
		{
			name: "re-enabled after a long pause: LastArmedAt wins over LastFiredAt",
			sched: Schedule{
				Cron: "0 * * * *", CreatedAt: now.Add(-48 * time.Hour),
				LastFiredAt: hoursAgo(30), LastArmedAt: hoursAgo(0.5),
			},
			want: 0,
		},
		{
			name: "synthetic run from a prior sweep anchors the next one",
			sched: Schedule{
				Cron: "0 * * * *", CreatedAt: now.Add(-48 * time.Hour),
				LastFiredAt: hoursAgo(30),
				Runs:        []ScheduleRun{{At: *hoursAgo(0.5), Status: "skipped"}},
			},
			want: 0,
		},
		{
			name:  "cap: every-minute cron off for two days",
			sched: Schedule{Cron: "* * * * *", CreatedAt: now.Add(-72 * time.Hour), LastFiredAt: hoursAgo(48)},
			want:  missedFireCountCap,
		},
		{
			name:  "never fired: anchored on creation",
			sched: Schedule{Cron: "0 * * * *", CreatedAt: now.Add(-2*time.Hour - 30*time.Minute)},
			want:  2,
		},
		{
			name:  "bad cron is someone else's problem",
			sched: Schedule{Cron: "not a cron", CreatedAt: now.Add(-48 * time.Hour)},
			want:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countMissedFires(&tc.sched, now); got != tc.want {
				t.Errorf("countMissedFires = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLoadRecordsMissedRecurringRuns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sched, err := s.Create(ScheduleInput{Cron: "0 * * * *", PaneID: "p1", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a fire 3.5 hours ago, then daemon downtime until now.
	s.mu.Lock()
	past := time.Now().Add(-3*time.Hour - 30*time.Minute)
	s.items[sched.ID].LastFiredAt = &past
	s.items[sched.ID].CreatedAt = past
	_ = s.saveUnlocked()
	s.mu.Unlock()
	s.Stop()

	s2, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	got, ok := s2.Get(sched.ID)
	if !ok {
		t.Fatal("schedule vanished across reload")
	}
	// The exact count depends on where hour boundaries fall inside the
	// 3.5h gap (3 or 4) — assert the invariants, not wall-clock alignment.
	if len(got.Runs) == 0 || got.Runs[0].Status != "skipped" ||
		!strings.Contains(got.Runs[0].Detail, "runs missed while the app was off") {
		t.Fatalf("expected synthetic missed-runs record, got runs=%+v", got.Runs)
	}
	if got.SkipCount < 3 || got.SkipCount > 4 {
		t.Errorf("SkipCount = %d, want 3 or 4 for a 3.5h hourly gap", got.SkipCount)
	}
	firstCount := got.SkipCount

	// A third load must NOT double-count: the synthetic run is the anchor.
	s2.Stop()
	s3, err := NewScheduleStore(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Stop()
	got3, _ := s3.Get(sched.ID)
	if got3.SkipCount != firstCount {
		t.Errorf("SkipCount after re-load = %d, want %d (double-counted)", got3.SkipCount, firstCount)
	}
}

func TestWriteRunLogPrunes(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "runs")
	now := time.Now()
	write := func(at time.Time) string {
		name := writeRunLog(dir, at, []byte("log"))
		if name == "" {
			t.Fatal("writeRunLog returned empty name")
		}
		return name
	}
	count := func() int {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	// Five old logs, then runLogKeepNewest+3 recent ones: every recent log
	// stays (before stage 2 only the newest 10 did), every old one goes.
	var old []string
	for i := 0; i < 5; i++ {
		old = append(old, write(now.Add(-runLogRetention-time.Duration(i+1)*time.Hour)))
	}
	if got := count(); got != 5 {
		t.Fatalf("old logs kept while under the newest %d: got %d files, want 5", runLogKeepNewest, got)
	}
	var last string
	for i := 0; i < runLogKeepNewest+3; i++ {
		last = write(now.Add(-time.Hour + time.Duration(i)*time.Minute))
	}
	if got, want := count(), runLogKeepNewest+3; got != want {
		t.Errorf("runs dir has %d files, want %d (recent logs only)", got, want)
	}
	for _, name := range old {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("log %s older than %s kept", name, runLogRetention)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, last)); err != nil {
		t.Errorf("newest log pruned: %v", err)
	}
}

func TestPruneRunLogsKeepsNewestUnderTheSizeCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < runLogKeepNewest+5; i++ {
		name := filepath.Join(dir, "run-"+strconv.FormatInt(now.Add(time.Duration(i)*time.Minute).UnixMilli(), 10)+".log")
		if err := os.WriteFile(name, bytes.Repeat([]byte("x"), 1000), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := runLogMaxBytes
	runLogMaxBytes = 3000
	t.Cleanup(func() { runLogMaxBytes = old })
	pruneRunLogs(dir, now)
	entries, _ := os.ReadDir(dir)
	if len(entries) != runLogKeepNewest {
		t.Errorf("after the size cap: %d files, want the newest %d", len(entries), runLogKeepNewest)
	}
}

// FireSchedule posts to the cloud notify relay with sched:-prefixed key and
// respects the per-(schedule,kind) cooldown.
func TestPusherFireSchedule(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sent":1}`))
	}))
	defer srv.Close()

	p := NewPusher(srv.URL, "tok", nil, nil, nil)
	p.FireSchedule("sched_1", "nightly build", "schedule-error", "exit 1 in 4s")
	p.FireSchedule("sched_1", "nightly build", "schedule-error", "exit 1 in 4s") // within cooldown → dropped
	p.FireSchedule("sched_1", "nightly build", "schedule-fired", "done")         // different kind → own bucket

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("deliveries = %d, want 2 (cooldown must drop the repeat)", len(bodies))
	}
	first := bodies[0]
	if first["paneId"] != "sched:sched_1" || first["state"] != "schedule-error" ||
		first["title"] != "nightly build" || first["summary"] != "exit 1 in 4s" {
		t.Errorf("unexpected payload: %+v", first)
	}
}

func TestScheduleMetricsCounters(t *testing.T) {
	t.Parallel()
	s := newTestScheduleStore(t)
	m := NewMetrics()
	s.SetMetrics(m)
	sched, err := s.Create(ScheduleInput{Cron: "0 9 * * *", PaneID: "p1", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	s.recordRun(sched.ID, ScheduleRun{At: time.Now(), Status: "error", Detail: "x"})
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.labelled[`solud_schedule_creates_total{kind="prompt"}`] != 1 {
		t.Errorf("creates counter = %v, want 1 (map: %v)", m.labelled[`solud_schedule_creates_total{kind="prompt"}`], m.labelled)
	}
	if m.labelled[`solud_schedule_runs_total{kind="prompt",status="error"}`] != 1 {
		t.Errorf("runs counter missing (map: %v)", m.labelled)
	}
}
