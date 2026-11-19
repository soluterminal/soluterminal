package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Automation history (Automations stage 2, items 9 and 10).
//
// Every run of an automation, and every change to it (created, edited, turned
// on or off), is appended as one JSON line to a file for its day:
//
//	<dataDir>/schedule-history/<scheduleID>/YYYY-MM-DD.jsonl   (UTC day)
//
// Files are 0600 in a 0700 folder and are only ever appended to. They are
// pruned by whole days: older than historyRetention, or the oldest days
// first once one automation's history passes historyMaxBytes. The history
// holds no prompt text: a run line carries its outcome, times, cause,
// account, model and the short Detail/Summary the row already showed; the
// prompt stays in schedules.json and in the run log (0600) only.
//
// Schedule.Runs stays as it was, the newest maxRunHistory runs, for the list,
// the phone app and the reports reader. The history is the full record
// behind it, read a page at a time (GET /api/schedules/{id}/history).

const (
	// Run outcomes. "worked": the run did its job (a prompt was typed into
	// its pane, a task or agent finished). "nothing": a task reported there
	// was nothing to do. "failed": the run was tried and did not finish.
	// "skipped": nothing was tried (busy pane, snoozed, paused, missed).
	runOutcomeWorked  = "worked"
	runOutcomeNothing = "nothing"
	runOutcomeFailed  = "failed"
	runOutcomeSkipped = "skipped"

	// Run triggers: what started a run.
	runTriggerScheduled = "scheduled"
	runTriggerRunNow    = "run-now"
	runTriggerRetry     = "retry"
	runTriggerCatchUp   = "catch-up"
)

// Run causes: why a run failed or was skipped, as a stable code the app turns
// into plain words and one button (stage 2, item 11). Detail keeps the full
// text; Cause is what a client may switch on.
const (
	runCausePaneGone       = "pane-gone"
	runCausePaneBusy       = "pane-busy"
	runCausePaneMenu       = "pane-menu"
	runCausePaneTyping     = "pane-typing"
	runCausePaneNotReady   = "pane-not-ready"
	runCauseClaudeMissing  = "claude-missing"
	runCauseFolderGone     = "folder-gone"
	runCauseBadSetting     = "bad-setting"
	runCauseUsageLimit     = "usage-limit"
	runCauseTimeout        = "timeout"
	runCauseTemporary      = "temporary-error"
	runCauseRunFailed      = "run-failed"
	runCauseReportedFailed = "reported-failed"
	runCauseStartFailed    = "start-failed"
	runCauseAlreadyRunning = "already-running"
	runCauseTooManyRuns    = "too-many-runs"
	runCauseAgentMissing   = "agent-missing"
	runCauseKillSwitch     = "kill-switch"
	runCauseSnoozed        = "snoozed"
	runCauseEnded          = "ended"
	runCauseMissedAppOff   = "missed-app-off"
	runCauseMissedAsleep   = "missed-asleep"
	runCauseStopped        = "stopped"
	runCauseInternal       = "internal"
	// runCauseInterrupted: SoluTerminal stopped while the run was going, and
	// the run left no result.
	runCauseInterrupted = "interrupted"
)

const (
	historyDirName = "schedule-history"
	// historyRetention is how long history is kept (90 days, decided by
	// @release-lead 18 Nov for ST-299).
	historyRetention = 90 * 24 * time.Hour
	// historyPageDefault and historyPageMax bound one history page.
	historyPageDefault = 50
	historyPageMax     = 200
)

// historyMaxBytes caps one automation's history on disk; the oldest days go
// first. A var so tests can shrink it. 8 MiB holds about 90 days of a run
// every five minutes.
var historyMaxBytes int64 = 8 << 20

// Entry types in a history file.
const (
	historyEntryRun    = "run"
	historyEntryChange = "change"
	historyEntryNote   = "note"
)

// Change actions recorded in the history only (the capped Schedule.Changes
// keeps on/off/deleted, which the row reads for "Turned off by …").
const (
	scheduleChangeCreated = "created"
	scheduleChangeEdited  = "edited"
)

// scheduleHistoryEntry is one line of a history file. A "note" adds text to
// an earlier run's Detail (a chat delivery outcome that lands after the run).
type scheduleHistoryEntry struct {
	Type   string          `json:"type"`
	At     time.Time       `json:"at"`
	Run    *ScheduleRun    `json:"run,omitempty"`
	Change *ScheduleChange `json:"change,omitempty"`
	RunID  string          `json:"runId,omitempty"`
	Note   string          `json:"note,omitempty"`
}

// scheduleHistory writes and reads the history files. Its own lock: callers
// may hold ScheduleStore.mu (lock order: store, then history).
type scheduleHistory struct {
	mu   sync.Mutex
	root string
	now  func() time.Time
	// pruned is the UTC day each automation was last pruned on, so a busy
	// automation prunes once a day, not on every run.
	pruned map[string]string
	// known marks automations whose history is started: their older runs
	// were imported (historyImportedMarker) or they are new.
	known map[string]bool
	// sizes is each automation's history size on disk, once read.
	sizes map[string]int64
}

// historyImportedMarker is written in an automation's history folder once
// its older runs are in it (importLegacyHistoryLocked), or when it is new.
const historyImportedMarker = ".started"

// historyLineMaxRunes bounds the Detail and Summary a history line carries,
// so one line stays a few KiB. With cron's one-minute floor a day's file then
// stays under historyMaxBytes, which is why today's file is never pruned.
const historyLineMaxRunes = 500

func newScheduleHistory(dataDir string) *scheduleHistory {
	return &scheduleHistory{root: filepath.Join(dataDir, historyDirName), now: time.Now, pruned: map[string]string{}, known: map[string]bool{}, sizes: map[string]int64{}}
}

// historyIDRe is what a schedule id must look like before it names a folder:
// ids come from schedules.json and from request paths.
var historyIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// A day's history is one or more segment files: YYYY-MM-DD.jsonl, then
// YYYY-MM-DD.1.jsonl, .2 … once a segment reaches historySegmentBytes, so
// the size cap can drop whole segments of a busy day too, and a page decodes
// one segment at a time.
var historyDayRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:\.(\d{1,4}))?\.jsonl$`)

// historySegmentBytes is when a day's segment is full. A var for tests.
var historySegmentBytes int64 = 1 << 20

func (h *scheduleHistory) dir(schedID string) (string, bool) {
	if h == nil || !historyIDRe.MatchString(schedID) {
		return "", false
	}
	return filepath.Join(h.root, schedID), true
}

// started reports whether an automation's history is started: the one-time
// import of its older runs has been written, or it was created with one.
func (h *scheduleHistory) started(schedID string) bool {
	dir, ok := h.dir(schedID)
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.known[schedID] {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, historyImportedMarker))
	h.known[schedID] = err == nil
	return err == nil
}

// append writes entries, in order, to their days' files. Errors are logged,
// never returned: the run itself is already recorded in schedules.json.
func (h *scheduleHistory) append(schedID string, entries ...scheduleHistoryEntry) {
	dir, ok := h.dir(schedID)
	if !ok || len(entries) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("scheduler: history for %s not written: %v", schedID, err)
		return
	}
	days, byDay := historyLines(entries, h.now)
	segs := daysIn(dir)
	var wrote int64
	for _, day := range days {
		seg := historyDayFile{day: day}
		for _, d := range segs {
			if d.day == day && d.part >= seg.part {
				seg = d
			}
		}
		if seg.size >= historySegmentBytes {
			seg = historyDayFile{day: day, part: seg.part + 1}
		}
		f, err := os.OpenFile(filepath.Join(dir, seg.name()), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			log.Printf("scheduler: history for %s not written: %v", schedID, err)
			continue
		}
		data := byDay[day]
		// A write cut short earlier left a line without its newline: end
		// it, so it reads as one bad line (skipped) and not as the start of
		// this one.
		if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
			last := make([]byte, 1)
			if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
				data = append([]byte{'\n'}, data...)
			}
		}
		n, err := f.Write(data)
		wrote += int64(n)
		if err != nil {
			log.Printf("scheduler: history for %s not written: %v", schedID, err)
		}
		_ = f.Close()
	}
	h.afterWriteLocked(schedID, dir, wrote)
}

// historyLines groups entries' JSON lines by UTC day, in order.
func historyLines(entries []scheduleHistoryEntry, now func() time.Time) ([]string, map[string][]byte) {
	byDay := map[string][]byte{}
	var days []string
	for _, e := range entries {
		if e.At.IsZero() {
			e.At = now()
		}
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		day := e.At.UTC().Format(time.DateOnly)
		if _, seen := byDay[day]; !seen {
			days = append(days, day)
		}
		byDay[day] = append(append(byDay[day], line...), '\n')
	}
	return days, byDay
}

// afterWriteLocked keeps the size count and prunes: on the first write of a
// day (the age limit), and whenever the history passes historyMaxBytes.
func (h *scheduleHistory) afterWriteLocked(schedID, dir string, wrote int64) {
	size, known := h.sizes[schedID]
	if !known {
		size = 0
		for _, d := range daysIn(dir) {
			size += d.size
		}
	} else {
		size += wrote
	}
	today := h.now().UTC().Format(time.DateOnly)
	if h.pruned[schedID] != today || size > historyMaxBytes {
		h.pruned[schedID] = today
		size = h.pruneLocked(dir)
	}
	h.sizes[schedID] = size
}

// historyDayFile is one segment of a day's history.
type historyDayFile struct {
	day  string
	part int
	size int64
}

func (d historyDayFile) key() string {
	if d.part == 0 {
		return d.day
	}
	return d.day + "." + strconv.Itoa(d.part)
}

func (d historyDayFile) name() string { return d.key() + ".jsonl" }

// newer reports whether d comes after o: a later day, or a later segment of
// the same day.
func (d historyDayFile) newer(o historyDayFile) bool {
	if d.day != o.day {
		return d.day > o.day
	}
	return d.part > o.part
}

// daysIn lists an automation's day files, newest first.
func daysIn(dir string) []historyDayFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []historyDayFile
	for _, e := range ents {
		m := historyDayRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		var size int64
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		part := 0
		if m[2] != "" {
			part, _ = strconv.Atoi(m[2])
		}
		out = append(out, historyDayFile{day: m[1], part: part, size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].newer(out[j]) })
	return out
}

// pruneLocked drops whole days older than historyRetention, then the oldest
// segments while the folder is over historyMaxBytes, and returns the size
// left. The newest segment always stays; it is at most about
// historySegmentBytes.
func (h *scheduleHistory) pruneLocked(dir string) int64 {
	days := daysIn(dir)
	cutoff := h.now().Add(-historyRetention).UTC().Format(time.DateOnly)
	var total, kept int64
	for i, d := range days {
		total += d.size
		if i > 0 && (d.day < cutoff || total > historyMaxBytes) {
			if os.Remove(filepath.Join(dir, d.name())) == nil {
				continue
			}
		}
		kept += d.size
	}
	return kept
}

// remove deletes an automation's history (it was deleted).
func (h *scheduleHistory) remove(schedID string) {
	dir, ok := h.dir(schedID)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pruned, schedID)
	delete(h.known, schedID)
	delete(h.sizes, schedID)
	_ = os.RemoveAll(dir)
}

// historyQuery selects one page.
type historyQuery struct {
	// Before returns entries strictly older than this cursor (empty: from
	// the newest).
	Before string
	Limit  int
	// FailedOnly returns failed runs only (no skips, no changes).
	FailedOnly bool
}

// historyPage is one page, newest first. Next is the cursor for the page
// after it, empty at the end.
type historyPage struct {
	Entries []scheduleHistoryEntry `json:"entries"`
	Next    string                 `json:"next,omitempty"`
}

// historyCursor is a place in the history: a segment and a line in it.
// Lines are appended in time order, so the line number orders entries that
// share a timestamp (an import's in particular).
type historyCursor struct {
	seg  historyDayFile
	line int
}

func (c historyCursor) String() string { return c.seg.key() + ":" + strconv.Itoa(c.line) }

func parseHistoryCursor(s string) (historyCursor, error) {
	key, line, ok := strings.Cut(s, ":")
	n, err := strconv.Atoi(line)
	m := historyDayRe.FindStringSubmatch(key + ".jsonl")
	if !ok || err != nil || n < 0 || m == nil {
		return historyCursor{}, errors.New("invalid history cursor")
	}
	c := historyCursor{seg: historyDayFile{day: m[1]}, line: n}
	if m[2] != "" {
		c.seg.part, _ = strconv.Atoi(m[2])
	}
	return c, nil
}

// errHistoryCursor is a cursor the reader cannot use.
var errHistoryCursor = errors.New("invalid history cursor")

// page reads one page of an automation's history, newest first. Entries are
// in file order within a day (appends are in time order), so the cursor is
// the day and the line number.
func (h *scheduleHistory) page(schedID string, q historyQuery) (historyPage, error) {
	dir, ok := h.dir(schedID)
	if !ok {
		return historyPage{}, errors.New("invalid automation id")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = historyPageDefault
	}
	if limit > historyPageMax {
		limit = historyPageMax
	}
	var before *historyCursor
	if q.Before != "" {
		c, err := parseHistoryCursor(q.Before)
		if err != nil {
			return historyPage{}, errHistoryCursor
		}
		before = &c
	}
	h.mu.Lock()
	days := daysIn(dir)
	h.mu.Unlock()

	page := historyPage{Entries: []scheduleHistoryEntry{}}
	// Notes name a run by id and land after it, in the same day's file or a
	// later one. Days are read newest first, so a run's notes are collected
	// before the run itself is reached. A note later than one day after its
	// cursor day is not seen (chat delivery stamps within minutes).
	notes := map[string][]string{}
	for _, d := range days {
		if before != nil && d.newer(before.seg) {
			if d.day <= nextDay(before.seg.day) {
				collectNotes(filepath.Join(dir, d.name()), notes)
			}
			continue
		}
		lines := readHistoryDay(filepath.Join(dir, d.name()))
		same := before != nil && d.day == before.seg.day && d.part == before.seg.part
		for i := len(lines) - 1; i >= 0; i-- {
			e := lines[i]
			if same && i >= before.line {
				if e.Type == historyEntryNote {
					notes[e.RunID] = append(notes[e.RunID], e.Note)
				}
				continue
			}
			switch e.Type {
			case historyEntryNote:
				notes[e.RunID] = append(notes[e.RunID], e.Note)
				continue
			case historyEntryRun:
				if e.Run == nil {
					continue
				}
				if q.FailedOnly && e.Run.Outcome != runOutcomeFailed {
					continue
				}
				// Notes were collected newest first; apply them oldest first.
				if ns := notes[e.Run.ID]; len(ns) > 0 && e.Run.ID != "" {
					run := *e.Run
					for j := len(ns) - 1; j >= 0; j-- {
						if run.Detail != "" {
							run.Detail += " · "
						}
						run.Detail += ns[j]
					}
					e.Run = &run
				}
			case historyEntryChange:
				if q.FailedOnly || e.Change == nil {
					continue
				}
			default:
				continue
			}
			if len(page.Entries) == limit {
				page.Next = historyCursor{seg: d, line: i + 1}.String()
				return page, nil
			}
			page.Entries = append(page.Entries, e)
		}
	}
	return page, nil
}

func nextDay(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, 1).Format(time.DateOnly)
}

// readHistoryDay parses one day's file; a torn or foreign line is skipped
// (and keeps its place, so cursors stay valid).
func readHistoryDay(path string) []scheduleHistoryEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []scheduleHistoryEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e scheduleHistoryEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			e = scheduleHistoryEntry{}
		}
		out = append(out, e)
	}
	return out
}

func collectNotes(path string, notes map[string][]string) {
	lines := readHistoryDay(path)
	for i := len(lines) - 1; i >= 0; i-- {
		if e := lines[i]; e.Type == historyEntryNote && e.RunID != "" {
			notes[e.RunID] = append(notes[e.RunID], e.Note)
		}
	}
}

// newRunID names one run: when it began, plus a random suffix.
func newRunID(at time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("run_%s_%s", strconv.FormatInt(at.UnixMilli(), 36), hex.EncodeToString(b[:]))
}

// finishRun fills what every run record carries: an id, an outcome, and a
// trigger. Callers set Outcome only to say more than Status does ("nothing").
func finishRun(run *ScheduleRun) {
	if run.At.IsZero() {
		run.At = time.Now()
	}
	if run.ID == "" {
		run.ID = newRunID(run.At)
	}
	if run.Trigger == "" {
		run.Trigger = runTriggerScheduled
	}
	switch run.Status {
	case "error":
		run.Outcome = runOutcomeFailed
	case "skipped":
		run.Outcome = runOutcomeSkipped
	case "fired":
		// A run that ran to its end: "nothing" and "failed" (its report
		// said it could not finish) are set by the task path.
		if run.Outcome != runOutcomeNothing && run.Outcome != runOutcomeFailed {
			run.Outcome = runOutcomeWorked
		}
	}
}

// endRun stamps when a run ended and how long it took.
func endRun(run *ScheduleRun, end time.Time) {
	end = end.Round(time.Millisecond)
	run.EndedAt = &end
	if d := end.Sub(run.At); d > 0 {
		run.DurationMs = d.Milliseconds()
	}
}

// historyRun is a run's history line, recorded at `at`: the line's time is
// when it was written (a run that started before a later edit and ended
// after it comes after the edit), so file order is time order. run.At stays
// when the run started. Detail and Summary are clipped
// (historyLineMaxRunes).
func historyRun(run ScheduleRun, at time.Time) scheduleHistoryEntry {
	r := run
	r.Detail = clipRunesEllipsis(r.Detail, historyLineMaxRunes)
	r.Summary = clipRunesEllipsis(r.Summary, historyLineMaxRunes)
	return scheduleHistoryEntry{Type: historyEntryRun, At: at, Run: &r}
}

// historyChange is a change's history line.
func historyChange(c ScheduleChange) scheduleHistoryEntry {
	cc := c
	return scheduleHistoryEntry{Type: historyEntryChange, At: cc.At, Change: &cc}
}

// importLegacyHistoryLocked writes an automation's existing runs and changes
// (from before the history existed) into its history, oldest first, once,
// and reports whether its history is started. Each day's file is written
// whole, then the marker: an import cut short (a full disk, a stop) is
// simply written again next time, never doubled. Caller holds s.mu.
func (s *ScheduleStore) importLegacyHistoryLocked(sched *Schedule) bool {
	if s.history == nil {
		return false
	}
	if s.history.started(sched.ID) {
		return true
	}
	var entries []scheduleHistoryEntry
	if !sched.CreatedAt.IsZero() {
		c := ScheduleChange{At: sched.CreatedAt.UTC(), Action: scheduleChangeCreated, Source: scheduleSourceDaemon}
		if o := sched.CreatedBy; o != nil && o.PaneID != "" {
			c.By, c.PaneID, c.Source = o.PaneTitle, o.PaneID, scheduleSourceMCP
		}
		if c.By == "" {
			c.By = "unknown (before history was kept)"
		}
		entries = append(entries, historyChange(c))
	}
	type stamped struct {
		at time.Time
		e  scheduleHistoryEntry
	}
	// Fill the records on a copy: readers may still hold the old slice
	// (Get and List copy the Schedule, not its Runs).
	runs := append([]ScheduleRun(nil), sched.Runs...)
	// An undated run (the retired-model migration wrote one) is placed just
	// after the older run below it in the list, or the automation's creation.
	prev := sched.CreatedAt
	if prev.IsZero() {
		prev = time.Now()
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].At.IsZero() {
			runs[i].At = prev.Add(time.Millisecond)
		}
		prev = runs[i].At
	}
	var rest []stamped
	for i := range runs {
		r := &runs[i]
		if r.At.IsZero() {
			continue
		}
		finishRun(r)
		rest = append(rest, stamped{r.At, historyRun(*r, r.At)})
	}
	sched.Runs = runs
	for _, c := range sched.Changes {
		rest = append(rest, stamped{c.At, historyChange(c)})
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].at.Before(rest[j].at) })
	for _, r := range rest {
		entries = append(entries, r.e)
	}
	return s.history.writeImport(sched.ID, entries)
}

// writeImport writes an import's day files whole (replacing any left by an
// import cut short), then the marker. Only an automation whose history is
// not started gets here, so those files hold nothing else.
func (h *scheduleHistory) writeImport(schedID string, entries []scheduleHistoryEntry) bool {
	dir, ok := h.dir(schedID)
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("scheduler: history for %s not started: %v", schedID, err)
		return false
	}
	days, byDay := historyLines(entries, h.now)
	for _, day := range days {
		if err := writeFileAtomicSynced(filepath.Join(dir, historyDayFile{day: day}.name()), byDay[day], 0o600); err != nil {
			log.Printf("scheduler: history for %s not started: %v", schedID, err)
			return false
		}
	}
	if err := writeFileAtomicSynced(filepath.Join(dir, historyImportedMarker), nil, 0o600); err != nil {
		log.Printf("scheduler: history for %s not started: %v", schedID, err)
		return false
	}
	h.known[schedID] = true
	delete(h.sizes, schedID)
	h.afterWriteLocked(schedID, dir, 0)
	return true
}

// markStarted starts a new automation's history: there is nothing older to
// import.
func (h *scheduleHistory) markStarted(schedID string) {
	h.writeImport(schedID, nil)
}

// prependRunLocked records run as the automation's newest: in Runs (capped)
// and in its history. Every run record goes through here. Caller holds s.mu.
//
// If the history could not be started (the import failed), the run is not
// appended: it is in Runs, so the import's next try takes it in.
func (s *ScheduleStore) prependRunLocked(sched *Schedule, run ScheduleRun) ScheduleRun {
	started := s.importLegacyHistoryLocked(sched)
	finishRun(&run)
	sched.Runs = append([]ScheduleRun{run}, sched.Runs...)
	if len(sched.Runs) > maxRunHistory {
		sched.Runs = sched.Runs[:maxRunHistory]
	}
	if started && !(run.replayed && s.history.hasRun(sched.ID, run.ID, run.At)) {
		s.history.append(sched.ID, historyRun(run, time.Now()))
	}
	return run
}

// hasRun reports whether an automation's history, from the day `since`
// on, holds run id: a run recorded again after a restart (its record had not
// reached disk) was written there by the first try, on or after the day the
// run started. However long the app was off, that day is still searched.
func (h *scheduleHistory) hasRun(schedID, runID string, since time.Time) bool {
	dir, ok := h.dir(schedID)
	if !ok || runID == "" {
		return false
	}
	h.mu.Lock()
	segs := daysIn(dir)
	h.mu.Unlock()
	first := since.UTC().Format(time.DateOnly)
	for _, d := range segs {
		if d.day < first {
			break
		}
		for _, e := range readHistoryDay(filepath.Join(dir, d.name())) {
			if e.Type == historyEntryRun && e.Run != nil && e.Run.ID == runID {
				return true
			}
		}
	}
	return false
}

// noteChangeLocked records an on/off change on the automation (its capped
// Changes, which the row reads) and in its history. Caller holds s.mu.
//
// As for runs, a change is not appended while the history cannot be started;
// it is in Changes, so the import's next try takes it in.
func (s *ScheduleStore) noteChangeLocked(sched *Schedule, action string, actor ScheduleActor, reason string, at time.Time) {
	started := s.importLegacyHistoryLocked(sched)
	sched.noteChange(action, actor, reason, at)
	if started {
		s.history.append(sched.ID, historyChange(sched.Changes[0]))
	}
}

// noteHistoryChangeLocked records a change in the history only: created and
// edited, which the row does not show. Caller holds s.mu.
func (s *ScheduleStore) noteHistoryChangeLocked(sched *Schedule, action string, actor ScheduleActor, at time.Time) {
	if !s.importLegacyHistoryLocked(sched) {
		return
	}
	var tmp Schedule
	tmp.noteChange(action, actor, "", at)
	s.history.append(sched.ID, historyChange(tmp.Changes[0]))
}

// History returns one page of an automation's history.
//
// An automation from before the history (no run or change since) has its
// older runs imported first, so its history is never empty while its row
// shows runs.
func (s *ScheduleStore) History(id string, q historyQuery) (historyPage, error) {
	s.mu.Lock()
	sched, ok := s.items[id]
	available := ok && scheduleAvailable(sched)
	if available && s.history != nil && !s.history.started(id) && s.importLegacyHistoryLocked(sched) {
		_ = s.saveUnlocked() // the imported runs now carry their ids
	}
	s.mu.Unlock()
	if !available {
		return historyPage{}, errors.New("schedule not found")
	}
	return s.history.page(id, q)
}

// SetAccountResolver names the account a provider's runs use now ("" when
// unknown), for the run record.
func (s *ScheduleStore) SetAccountResolver(fn func(provider string) string) {
	s.mu.Lock()
	s.accountOf = fn
	s.mu.Unlock()
}

// runAccount is the account a run of provider uses now. Not under s.mu: the
// resolver may read credential files.
func (s *ScheduleStore) runAccount(provider string) string {
	s.mu.Lock()
	fn := s.accountOf
	s.mu.Unlock()
	if fn == nil {
		return ""
	}
	return clipRunes(oneLine(fn(provider)), 80)
}

func init() {
	registerBootStep(bootServer, func(b *bootEnv) {
		if b.scheds != nil && b.api != nil {
			b.scheds.SetAccountResolver(b.api.credentialAccount)
		}
	})
}
