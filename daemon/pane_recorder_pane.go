package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// paneRecording is a pane's recorder state. It lives only in memory: nothing
// is persisted, so a daemon restart, a respawn or a resume (a new Pane) starts
// unrecorded, and only a person switches it on again.
type paneRecording struct {
	active atomic.Pointer[paneRecorder]
	mu     sync.Mutex
	// notice is the last stop the person hasn't dismissed (cap, refused folder,
	// a refused pane, the 8-hour limit, a failed write).
	notice *recordingNotice
}

type recordingNotice struct {
	reason string
	at     time.Time
	stats  recordingStats
}

// paneRecordingDTO is the recording state on the pane list, the status route
// and the "pane-recording" UI event. It never carries content.
type paneRecordingDTO struct {
	On            bool   `json:"on"`
	StoppedReason string `json:"stoppedReason,omitempty"`
	StoppedAt     int64  `json:"stoppedAt,omitempty"`
	// Hours and Overrides are a running recording's terms: its time limit
	// and the rule codes the person overrode at switch-on.
	Hours     int      `json:"hours,omitempty"`
	Overrides []string `json:"overrides,omitempty"`
	// DeleteAfterDays is the deletion age in force while it records.
	DeleteAfterDays int `json:"deleteAfterDays,omitempty"`
	recordingStats
	// Check is how the rules read the pane now, on the status route only.
	Check *recordingCheckDTO `json:"check,omitempty"`
}

// recordingCheckDTO is codes only: never a path, a folder name or a handle.
type recordingCheckDTO struct {
	// Unavailable says switch-on can't work at all: "windows",
	// "not_running" or "no_data_dir".
	Unavailable string `json:"unavailable,omitempty"`
	// Hard is a rule no override touches; Defaults are rules the person may
	// override at switch-on.
	Hard     string   `json:"hard,omitempty"`
	Defaults []string `json:"defaults,omitempty"`
}

// recordingTerms are what the person chose at switch-on. Nothing is
// remembered for the next switch-on.
type recordingTerms struct {
	hours     int
	overrides map[recordingRule]bool
}

// overrideCodes lists the overrides in rule order, for the log and clients.
func (t recordingTerms) overrideCodes() []string {
	var out []string
	for _, r := range recordingRuleOrder {
		if t.overrides[r] {
			out = append(out, string(r))
		}
	}
	return out
}

// recordingStartOptions is a switch-on request: the time limit in hours (0
// means the default) and the rule codes the person chose to override.
type recordingStartOptions struct {
	Hours     int
	Overrides []string
	// Pid, when set, is the pane process the person agreed to record; a pane
	// restarted or resumed since (same id, new process) is refused.
	Pid int
}

// errRecordingBadRequest marks a switch-on request outside its bounds.
type errRecordingBadRequest struct{ msg string }

func (e errRecordingBadRequest) Error() string { return e.msg }

// terms validates the options: hours within bounds, override codes known.
// An override the pane doesn't need is dropped (and never logged).
func (o recordingStartOptions) terms(v recordingVerdict) (recordingTerms, error) {
	t := recordingTerms{hours: o.Hours, overrides: map[recordingRule]bool{}}
	if t.hours == 0 {
		t.hours = recordingHoursDefault
	}
	if t.hours < recordingHoursMin || t.hours > recordingHoursMax {
		return t, errRecordingBadRequest{fmt.Sprintf("the time limit must be %d to %d hours", recordingHoursMin, recordingHoursMax)}
	}
	needed := map[recordingRule]bool{}
	for _, d := range v.Defaults {
		needed[d] = true
	}
	for _, code := range o.Overrides {
		r := recordingRule(code)
		if !recordingOverridable[r] {
			return t, errRecordingBadRequest{fmt.Sprintf("%q can't be overridden", code)}
		}
		if needed[r] {
			t.overrides[r] = true
		}
	}
	return t, nil
}

// Test seams. Production uses the policy constants and the live process tree.
var (
	recordingCheckEvery = recordingPolicyCheckEvery
	recordingHourUnit   = time.Hour
	recordingFactsOf    = liveRecordingFacts
)

// recordingHub returns the manager's recordings folder owner, or nil without a
// data dir.
func (m *Manager) recordingHub() *recordingHub {
	if m == nil {
		return nil
	}
	m.recordingsMu.Lock()
	defer m.recordingsMu.Unlock()
	if m.recordings == nil && m.dataDir != "" {
		m.recordings = newRecordingHub(m.dataDir)
		m.recordings.onFileOpen = m.timelineCast
	}
	return m.recordings
}

// recordOutput is the read loop's hook for FrameData payloads. With recording
// off it is one atomic load.
func (p *Pane) recordOutput(b []byte) {
	if r := p.recording.active.Load(); r != nil {
		r.output(b)
	}
}

// recordResize writes an "r" event when the pane is recording.
func (p *Pane) recordResize(cols, rows uint16) {
	if r := p.recording.active.Load(); r != nil {
		r.resize(cols, rows)
	}
}

// Recording reports the pane's recording state for clients, or nil when it is
// off with nothing to show.
func (p *Pane) Recording() *paneRecordingDTO {
	if r := p.recording.active.Load(); r != nil {
		return r.onDTO()
	}
	p.recording.mu.Lock()
	defer p.recording.mu.Unlock()
	if n := p.recording.notice; n != nil {
		return &paneRecordingDTO{StoppedReason: n.reason, StoppedAt: n.at.UnixMilli(), recordingStats: n.stats}
	}
	return nil
}

func (p *Pane) recordingEventDTO() *paneRecordingDTO {
	if d := p.Recording(); d != nil {
		return d
	}
	return &paneRecordingDTO{}
}

// stopRecording asks the pane's recorder to finish; cut removes what was
// recorded after the last passing (f) check. It doesn't wait for the disk.
func (p *Pane) stopRecording(reason string, cut bool) bool {
	r := p.recording.active.Load()
	if r == nil || !p.recording.active.CompareAndSwap(r, nil) {
		return false
	}
	r.stop(reason, cut)
	return true
}

// recheckRecordingHandle stops a recording at once when the pane's handle
// becomes one the handle default refuses, unless the person overrode it.
func (p *Pane) recheckRecordingHandle(handle string) {
	r := p.recording.active.Load()
	if r == nil || r.terms.overrides[recordingRuleHandle] || !recordingHandleRefused(handle) {
		return
	}
	if p.stopRecording(recordingStopHandle, true) {
		log.Printf("recording: pane %s switch-off by the daemon: %s", p.ID, recordingStopHandle)
	}
}

// liveRecordingFacts reads what the rules need from the pane and its process
// tree: whether a claude or codex process is live anywhere in it (C1), and
// the folders of the pane's own shell (the helper's direct child) and every
// such agent process, and no other descendant (C2). The helper's own folder
// is never read.
func liveRecordingFacts(p *Pane) recordingPaneFacts {
	p.mu.Lock()
	f := recordingPaneFacts{Handle: identityHandle(p.Identity), Agent: recordingPaneAgent(p.Agent, p.Cmd)}
	pid := p.Pid
	p.mu.Unlock()
	if pid <= 0 {
		f.CwdUnknown = true
		return f
	}
	// p.Pid is the pane's `solud agent` helper, whose folder is where it was
	// started; the pane's own shell (or command) is its direct child.
	tree := snapshotProcessTreeMaxAge(time.Second)
	own := append([]int(nil), tree.children[pid]...)
	if len(own) == 0 {
		f.CwdUnknown = true
	}
	for _, proc := range tree.descendantPids(pid) {
		if proc != pid && recordingIsAgentProcess(proc) {
			f.AgentRunning = true
			if !slices.Contains(own, proc) {
				own = append(own, proc)
			}
		}
	}
	if len(own) > 0 {
		f.Cwds, f.CwdUnknown = recordingLiveCwds(own)
	}
	return f
}

// recordingPaneAgent is the agent a pane was started as, read the way the
// app's effectivePaneAgent reads it. A Claude pane is stored with Agent ""
// (the legacy zero value, healPaneIdentity), so a "" pane is Claude only when
// its own launch command runs claude: bare, or as the 'claude' token of the
// login-shell wrapper (cmdRunsClaude, as the app's agentFromCommand). It is
// what the pane was started as; whether the pane counts as an agent pane is
// C1's reading of the live process tree (liveRecordingFacts).
func recordingPaneAgent(agent string, cmd []string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case agentClaude:
		return agentClaude
	case agentCodex:
		return agentCodex
	case agentMuse:
		return agentMuse
	}
	if cmdRunsClaude(cmd) {
		return agentClaude
	}
	if cmdRunsCodex(cmd) {
		return agentCodex
	}
	return ""
}

// recordingIsAgentProcess reports whether a process is claude or codex: by
// its name, or for node by the base name of argv[1] (Codex installed through
// npm). The command line is only compared, never kept or logged.
func recordingIsAgentProcess(pid int) bool {
	comm := strings.ToLower(readComm(pid))
	if recordingAgentNames[comm] {
		return true
	}
	if comm != "node" || runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	args := strings.Split(string(raw), "\x00")
	return len(args) > 1 && filepath.Base(args[1]) == "codex"
}

// recordingLiveCwds reads the current folders of the pane's own process and
// its agent processes. A process that exited meanwhile is skipped; one whose
// folder can't be read, or no /proc at all (macOS for now), makes the folder
// unknown, which refuses the pane like a hard folder.
func recordingLiveCwds(pids []int) (cwds []string, unknown bool) {
	if runtime.GOOS != "linux" {
		return nil, true
	}
	for _, pid := range pids {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue // exited meanwhile
		}
		if err != nil {
			return cwds, true
		}
		cwds = append(cwds, target)
	}
	return cwds, len(cwds) == 0
}

// recordingRuleOfStop maps a rule's stop reason back to its rule.
func recordingRuleOfStop(reason string) recordingRule {
	for rule, text := range recordingRuleStop {
		if text == reason {
			return rule
		}
	}
	return recordingRuleCwdUnknown
}

// recordingRefusedError is a rule's refusal of a switch-on.
type recordingRefusedError struct{ rule recordingRule }

func (e recordingRefusedError) Error() string {
	return "this pane can't be recorded: " + recordingStopText(e.rule)
}

var (
	errRecordingPlatform    = errors.New("recording is not available on Windows yet")
	errRecordingNotRunning  = errors.New("the pane is not running")
	errRecordingPaneChanged = errors.New("the pane restarted after you confirmed; recording was not started")
	errRecordingNoDataDir   = errors.New("no data folder for recordings")
)

// recordingAvailability says whether switch-on can work at all for the pane:
// "" or "windows", "no_data_dir", "not_running".
func (m *Manager) recordingAvailability(p *Pane) string {
	switch {
	case !recordingPlatformSupported():
		return "windows"
	case m.recordingHub() == nil:
		return "no_data_dir"
	case p.Snapshot().Status != PaneRunning:
		return "not_running"
	}
	return ""
}

// RecordingCheck is how the rules read the pane now, as codes only.
func (m *Manager) RecordingCheck(p *Pane) *recordingCheckDTO {
	c := &recordingCheckDTO{Unavailable: m.recordingAvailability(p)}
	if c.Unavailable != "" {
		return c
	}
	home, _ := os.UserHomeDir()
	v := recordingCheck(recordingFactsOf(p), home, m.recordingHub().dataDir)
	c.Hard = string(v.Hard)
	for _, d := range v.Defaults {
		c.Defaults = append(c.Defaults, string(d))
	}
	return c
}

// StartRecording switches recording on for a running pane with the person's
// time limit and overrides. The caller has already admitted the person
// (local, human-only).
func (m *Manager) StartRecording(p *Pane, via string, opts recordingStartOptions) (*paneRecordingDTO, error) {
	if !recordingPlatformSupported() {
		return nil, errRecordingPlatform
	}
	hub := m.recordingHub()
	if hub == nil {
		return nil, errRecordingNoDataDir
	}
	if p.Snapshot().Status != PaneRunning {
		return nil, errRecordingNotRunning
	}
	if opts.Pid != 0 && p.Snapshot().Pid != opts.Pid {
		return nil, errRecordingPaneChanged
	}
	home, _ := os.UserHomeDir()
	// The watcher keeps these for the recording's life.
	w := recordingWatch{every: recordingCheckEvery, facts: recordingFactsOf, home: home}
	verdict := recordingCheck(w.facts(p), home, hub.dataDir)
	terms, err := opts.terms(verdict)
	if err != nil {
		return nil, err
	}
	if rule := verdict.refusal(terms.overrides); rule != "" {
		return nil, recordingRefusedError{rule}
	}
	w.maxOn = time.Duration(terms.hours) * recordingHourUnit
	// The session timeline starts before the first file, so every file's
	// start sits on its clock.
	// Sweep first: the sweep recounts the folder, so it never overlaps the
	// timeline's first write.
	hub.sweepIfDue()
	// A start in progress keeps the session open: it counts as a live
	// recorder from here, and the recorder takes the count over once it
	// exists (its finish gives it back). Otherwise it is given back here.
	m.recordersLive.Add(1)
	owned := false
	defer func() {
		if !owned {
			m.recordersLive.Add(-1)
			go m.timelineEndIfIdle()
		}
	}()
	m.timelineEnsure(hub)
	// The timeline samples pane recording state, so it samples after the
	// pane's lock is released; the REC line itself is written under it.
	started := false
	defer func() {
		if started {
			m.timelineSample()
		}
	}()
	p.recording.mu.Lock()
	defer p.recording.mu.Unlock()
	if p.recording.active.Load() != nil {
		return p.recordingDTOLocked(), nil
	}
	p.mu.Lock()
	cols, rows := p.Cols, p.Rows
	p.mu.Unlock()
	verify := func() string { return w.stopReason(p, hub.dataDir, terms) }
	rec, err := startPaneRecorder(hub, p.ID, cols, rows, verify, func(r *paneRecorder, reason string) { m.recordingFinished(p, r, reason) })
	if err == nil {
		owned = true
		rec.terms = terms
		// Under subsMu (which Resize and the read loop hold) the first output
		// follows the header, and a resize since the header is not lost.
		p.subsMu.Lock()
		p.mu.Lock()
		nowCols, nowRows := p.Cols, p.Rows
		p.mu.Unlock()
		if nowCols != cols || nowRows != rows {
			rec.resize(nowCols, nowRows)
		}
		p.recording.active.Store(rec)
		p.subsMu.Unlock()
		// REC on is written now, before any stop can finish and write off.
		m.timelineRec(p.ID, true, "")
		// SetIdentity and markExited change the pane, then look for a
		// recorder; this publishes the recorder, then looks at the pane, so
		// a change made while the file was being created is never missed.
		if p.Snapshot().Status != PaneRunning {
			p.stopRecording(recordingStopClosed, true)
			err = errRecordingNotRunning
		} else if opts.Pid != 0 && p.Snapshot().Pid != opts.Pid {
			p.stopRecording(recordingStopReplaced, true)
			err = errRecordingPaneChanged
		} else if reason := verify(); reason != "" {
			p.stopRecording(reason, true)
			err = recordingRefusedError{recordingRuleOfStop(reason)}
		}
	}
	if err != nil {
		if errors.Is(err, errRecordingFolder) {
			p.recording.notice = &recordingNotice{reason: recordingStopFolder, at: time.Now()}
			go m.publishRecording(p)
		}
		log.Printf("recording: pane %s switch-on refused (%s): %v", p.ID, via, err)
		return nil, err
	}
	p.recording.notice = nil
	overridden := "none"
	if codes := terms.overrideCodes(); len(codes) > 0 {
		overridden = strings.Join(codes, ",")
	}
	log.Printf("recording: pane %s on by the person (%s) hours=%d overrides=%s", p.ID, via, terms.hours, overridden)
	started = true
	go m.watchRecording(p, rec, w)
	go m.publishRecording(p)
	return p.recordingDTOLocked(), nil
}

func (p *Pane) recordingDTOLocked() *paneRecordingDTO {
	if r := p.recording.active.Load(); r != nil {
		return r.onDTO()
	}
	return &paneRecordingDTO{}
}

func (r *paneRecorder) onDTO() *paneRecordingDTO {
	return &paneRecordingDTO{On: true, Hours: r.terms.hours, Overrides: r.terms.overrideCodes(),
		DeleteAfterDays: r.hub.recordingAgeFor(r.startedAt), recordingStats: r.stats()}
}

// StopRecording switches recording off. Anyone may: off is the safe direction.
func (m *Manager) StopRecording(p *Pane, via string) bool {
	if p.stopRecording(recordingStopOff, false) {
		log.Printf("recording: pane %s switch-off by %s", p.ID, via)
		return true
	}
	return false
}

// DismissRecordingNotice clears a stop notice (the person's, human-only).
func (m *Manager) DismissRecordingNotice(p *Pane) {
	p.recording.mu.Lock()
	had := p.recording.notice != nil
	p.recording.notice = nil
	p.recording.mu.Unlock()
	if had {
		m.publishRecording(p)
	}
}

// DeletePaneRecordings stops the pane's recording and deletes every file in
// its folder (the person's, human-only).
func (m *Manager) DeletePaneRecordings(p *Pane, via string) (int, error) {
	hub := m.recordingHub()
	if hub == nil {
		return 0, errRecordingNoDataDir
	}
	if r := p.recording.active.Load(); r != nil {
		p.stopRecording(recordingStopDeleted, false)
		if !r.wait(10 * time.Second) {
			return 0, fmt.Errorf("the recording is still closing; try again")
		}
	}
	n, err := hub.deletePane(p.ID)
	p.recording.mu.Lock()
	p.recording.notice = nil
	p.recording.mu.Unlock()
	log.Printf("recording: pane %s recordings deleted by the person (%s): %d files, err=%v", p.ID, via, n, err)
	m.publishRecording(p)
	return n, err
}

// watchRecording runs only while a recording is on: every
// recordingPolicyCheckEvery it re-checks the rules against the switch-on's
// overrides and that the pane is still this running pane, and it ends the
// recording at its time limit.
type recordingWatch struct {
	every, maxOn time.Duration
	facts        func(*Pane) recordingPaneFacts
	home         string
}

// stopReason is the rule stop for the pane under the recording's terms, or "".
func (w recordingWatch) stopReason(p *Pane, dataDir string, t recordingTerms) string {
	if rule := recordingCheck(w.facts(p), w.home, dataDir).refusal(t.overrides); rule != "" {
		return recordingStopText(rule)
	}
	return ""
}

func (m *Manager) watchRecording(p *Pane, rec *paneRecorder, w recordingWatch) {
	tick := time.NewTicker(w.every)
	defer tick.Stop()
	limit := time.NewTimer(w.maxOn)
	defer limit.Stop()
	for {
		select {
		case <-rec.done:
			return
		case <-limit.C:
			reason := recordingStopTimeLimit(rec.terms.hours)
			if p.recording.active.Load() == rec && p.stopRecording(reason, false) {
				log.Printf("recording: pane %s switch-off by the daemon: %s", p.ID, reason)
			}
			return
		case <-tick.C:
			if p.recording.active.Load() != rec {
				return
			}
			if cur, ok := m.Get(p.ID); !ok || cur != p || p.Snapshot().Status != PaneRunning {
				if p.stopRecording(recordingStopReplaced, false) {
					log.Printf("recording: pane %s switch-off by the daemon: %s", p.ID, recordingStopReplaced)
				}
				return
			}
			if reason := w.stopReason(p, rec.hub.dataDir, rec.terms); reason != "" {
				if p.stopRecording(reason, true) {
					log.Printf("recording: pane %s switch-off by the daemon: %s", p.ID, reason)
				}
				return
			}
			rec.checkpoint()
		}
	}
}

// recordingFinished runs on the writer goroutine once the files are closed.
func (m *Manager) recordingFinished(p *Pane, rec *paneRecorder, reason string) {
	p.recording.active.CompareAndSwap(rec, nil)
	stats := rec.stats()
	switch reason {
	case recordingStopOff, recordingStopClosed, recordingStopReplaced, recordingStopDeleted:
	default:
		p.recording.mu.Lock()
		p.recording.notice = &recordingNotice{reason: reason, at: time.Now(), stats: stats}
		p.recording.mu.Unlock()
	}
	log.Printf("recording: pane %s off: %s (recorded %d bytes, dropped %d, replaced %d)", p.ID, reason, stats.BytesRecorded, stats.BytesDropped, stats.BytesReplaced)
	m.publishRecording(p)
	m.timelineSample()
	m.timelineRec(p.ID, false, reason)
	m.recordersLive.Add(-1)
	m.timelineEndIfIdle()
}

// publishRecording pushes the pane's recording state to every client.
func (m *Manager) publishRecording(p *Pane) {
	if m == nil || m.bus == nil {
		return
	}
	m.bus.publish(uiEvent{Type: "pane-recording", PaneID: p.ID, Recording: p.recordingEventDTO(), Ts: time.Now().UnixMilli()})
}
