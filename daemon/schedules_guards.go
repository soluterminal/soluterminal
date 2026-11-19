package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Automation guards (Automations stage 2, ST-299 item 14; numbers decided by
// release-lead 18 Nov): how often an automation may run, and how many there
// may be. Checked by the daemon on create and on edit, whoever asks. A spend
// budget comes later.
//
// Automations from before the guards keep running as they are: one that runs
// more often than allowed is marked (tooFrequent on the row) and only a
// change to its timing is checked. Built-in agents' schedules and the
// daemon's own are not counted or checked.
const (
	// The shortest gap between two runs: for the person, and for an agent
	// creating through MCP.
	minIntervalPerson = 5 * time.Minute
	minIntervalAgent  = 15 * time.Minute
	// At most this many automations, and this many created by one agent pane
	// (agents whose pane could not be traced share one allowance).
	maxAutomations             = 100
	maxAutomationsPerAgentPane = 20
)

// cronMinGap is the shortest time a cron expression can leave between two
// runs, worked out from its fields, not from a sample of its runs (a sample
// can miss a short gap that comes years later). Within a day it is the
// closest two of its times; across midnight it is from the day's last time
// to the next day's first, when two days in a row can both run it
// (adjacentDaysMatch). ok is false for an expression that does
// not parse. from is unused (kept for the callers' readability).
func cronMinGap(expr string, from time.Time) (time.Duration, bool) {
	_ = from
	parsed, err := cronParser.Parse(strings.TrimSpace(expr))
	if err != nil {
		return 0, false
	}
	spec, ok := parsed.(*cron.SpecSchedule)
	if !ok {
		return 0, false
	}
	var times []int // minutes of the day it runs at, in order
	for h := 0; h < 24; h++ {
		if spec.Hour&(1<<uint(h)) == 0 {
			continue
		}
		for m := 0; m < 60; m++ {
			if spec.Minute&(1<<uint(m)) != 0 {
				times = append(times, h*60+m)
			}
		}
	}
	if len(times) == 0 {
		return 0, false
	}
	intra := 24 * 60
	for i := 1; i < len(times); i++ {
		if gap := times[i] - times[i-1]; gap < intra {
			intra = gap
		}
	}
	// Across midnight: only when two days in a row can both run it; else
	// at least a day goes by. Asked only when it could be the shorter gap.
	min := intra
	if cross := 24*60 - times[len(times)-1] + times[0]; cross < intra {
		if adjacentDaysMatchCached(strings.TrimSpace(expr), spec) {
			min = cross
		}
	}
	return time.Duration(min) * time.Minute, true
}

// guarded says whether an automation is checked and counted: the person's and
// the agents', not built-in agents' schedules or the daemon's hidden ones.
func guarded(kind string) bool {
	k := strings.TrimSpace(kind)
	return k != "agent" && k != agentKindSchedule
}

// fmtGap is a gap in words: "1 minute", "10 minutes", "2 hours".
func fmtGap(d time.Duration) string {
	if d < time.Hour {
		m := int(d.Round(time.Minute) / time.Minute)
		if m <= 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", m)
	}
	h := int(d.Round(time.Hour) / time.Hour)
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}

// intervalGuard refuses a timing that runs more often than actor may set.
// editing is the automation as it is (nil on create): an edit that keeps its
// timing is not checked, so an older automation stays as it was.
func intervalGuard(in ScheduleInput, actor ScheduleActor, editing *Schedule) error {
	if actor.Source == scheduleSourceDaemon || !guarded(in.Kind) || strings.TrimSpace(in.Cron) == "" {
		return nil
	}
	// An edit that keeps the timing (and stays a kind that is checked) is not
	// checked: an older automation stays as it was. One that turns a
	// built-in agent's schedule into a prompt or task is.
	if editing != nil && strings.TrimSpace(editing.Cron) == strings.TrimSpace(in.Cron) && guarded(editing.Kind) {
		return nil
	}
	min, who := minIntervalPerson, "Automations"
	if actor.Source == scheduleSourceMCP {
		min, who = minIntervalAgent, "Automations an agent creates"
	}
	gap, ok := cronMinGap(in.Cron, time.Now())
	if !ok || gap >= min {
		return nil
	}
	return fmt.Errorf("%s can run at most every %s; this timing runs every %s. Pick a longer interval", who, fmtGap(min), fmtGap(gap))
}

// countGuardLocked refuses a new automation past the caps. Caller holds s.mu.
func (s *ScheduleStore) countGuardLocked(in ScheduleInput, actor ScheduleActor, createdBy *ScheduleOrigin) error {
	if actor.Source == scheduleSourceDaemon || !guarded(in.Kind) {
		return nil
	}
	total, byPane := 0, 0
	pane := ""
	if createdBy != nil {
		pane = createdBy.PaneID
	}
	for _, sc := range s.items {
		if !guarded(sc.Kind) || !scheduleListed(*sc) {
			continue
		}
		total++
		if actor.Source == scheduleSourceMCP && sc.CreatedBy != nil && sc.CreatedBy.Source == scheduleSourceMCP && sc.CreatedBy.PaneID == pane {
			byPane++
		}
	}
	if total >= maxAutomations {
		return fmt.Errorf("there are already %d automations, the most this computer keeps. Delete one you no longer need first", total)
	}
	if actor.Source == scheduleSourceMCP && byPane >= maxAutomationsPerAgentPane {
		return fmt.Errorf("this pane has already created %d automations, the most one pane may. Ask the person to delete some first, or set this one up in the app", byPane)
	}
	return nil
}

// tooFrequent reports an automation from before the guards that runs more
// often than the person may set one now.
func tooFrequent(s Schedule) bool {
	if !guarded(s.Kind) || strings.TrimSpace(s.Cron) == "" {
		return false
	}
	gap, ok := cronMinGap(s.Cron, time.Now())
	return ok && gap < minIntervalPerson
}

// lateGrace is how late a scheduled run may start and still run: past it,
// the run was missed (the computer was asleep). A var for tests.
var lateGrace = 10 * time.Minute

// asleepNoticeDelay gathers the automations that missed runs on one wake into
// one notice. A var for tests.
var asleepNoticeDelay = 3 * time.Second

// missedAsleepDetail words a missed-while-asleep record: "N runs missed",
// "500+" once the slots walked reached the cap (n is all of them).
func missedAsleepDetail(earlier, n int) string {
	if n >= missedFireCountCap {
		return fmt.Sprintf("%d+ runs missed while the computer was asleep", missedFireCountCap)
	}
	return fmt.Sprintf("%d run%s missed while the computer was asleep", earlier, plural(earlier))
}

// recordMissedAsleep records runs missed while the computer was asleep (one
// skipped run naming how many), and adds the automation to the wake's notice.
func (s *ScheduleStore) recordMissedAsleep(id, name, detail string, missed int, at time.Time) {
	s.recordRun(id, ScheduleRun{At: at, Status: "skipped", Cause: runCauseMissedAsleep, Detail: detail})
	// These slots are handled: stamped here, so a fire that stops after this
	// record (too late, a run still going) never counts them again.
	s.mu.Lock()
	if sched, ok := s.items[id]; ok {
		if missed > 1 {
			sched.SkipCount += missed - 1 // recordRun counted this record itself
		}
		stamp := at
		sched.LastSlotAt = &stamp
		_ = s.saveUnlocked()
	}
	s.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asleepNames = append(s.asleepNames, name)
	if s.asleepTimer == nil {
		s.asleepTimer = time.AfterFunc(asleepNoticeDelay, s.sendAsleepNotice)
	}
}

// sendAsleepNotice tells the person, once per wake, in the app, which
// automations missed runs while the computer was asleep.
func (s *ScheduleStore) sendAsleepNotice() {
	s.mu.Lock()
	names := s.asleepNames
	s.asleepNames, s.asleepTimer = nil, nil
	bus := s.uiBus
	s.mu.Unlock()
	if len(names) == 0 {
		return
	}
	text := names[0]
	if len(names) > 1 {
		text = fmt.Sprintf("%s and %d more", names[0], len(names)-1)
	}
	body := text + " missed runs while the computer was asleep. Their history says which."
	// In the app, not a push: the person has just woken the computer and is
	// at it. Each automation's history keeps the record.
	if bus != nil {
		bus.publish(uiEvent{Type: "toast", Text: "⏰ " + body})
	}
}

// adjacentDaysCache remembers adjacentDaysMatch by expression: the row's
// tooFrequent mark asks on every list.
var adjacentDaysCache sync.Map

func adjacentDaysMatchCached(expr string, spec *cron.SpecSchedule) bool {
	if v, ok := adjacentDaysCache.Load(expr); ok {
		return v.(bool)
	}
	m := adjacentDaysMatch(spec)
	adjacentDaysCache.Store(expr, m)
	return m
}

// cronStarBit is robfig/cron's mark on a day field written as "*".
const cronStarBit = 1 << 63

// adjacentDaysMatch reports whether two calendar days in a row can both run
// spec: walked over 29 years of days (the calendar's weekday and leap-year
// pattern repeats within 28), so a day field like "29 2 MON" is judged by
// the dates it really matches.
func adjacentDaysMatch(spec *cron.SpecSchedule) bool {
	day := time.Date(2001, 1, 1, 12, 0, 0, 0, time.UTC)
	end := day.AddDate(29, 0, 0)
	prev := false
	for ; day.Before(end); day = day.AddDate(0, 0, 1) {
		m := specDayMatches(spec, day)
		if m && prev {
			return true
		}
		prev = m
	}
	return false
}

// specDayMatches is robfig/cron's day rule: the month must match, and the
// day of the month and the weekday both, or either one when both are
// restricted (neither is "*").
func specDayMatches(spec *cron.SpecSchedule, t time.Time) bool {
	if spec.Month&(1<<uint(t.Month())) == 0 {
		return false
	}
	dom := spec.Dom&(1<<uint(t.Day())) != 0
	dow := spec.Dow&(1<<uint(t.Weekday())) != 0
	if spec.Dom&cronStarBit != 0 || spec.Dow&cronStarBit != 0 {
		return dom && dow
	}
	return dom || dow
}

// noteRunStarted and noteRunEnded keep runStarted.
func (s *ScheduleStore) noteRunStarted(id string, at time.Time) {
	s.mu.Lock()
	if s.runStarted == nil {
		s.runStarted = map[string]time.Time{}
	}
	s.runStarted[id] = at
	s.mu.Unlock()
}

func (s *ScheduleStore) noteRunEnded(id string) {
	s.mu.Lock()
	delete(s.runStarted, id)
	s.mu.Unlock()
}
