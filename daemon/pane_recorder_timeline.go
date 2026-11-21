package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// The session layout timeline (amendment 2026-11-20 section 5, Addenda A and
// B; format in docs/pane-recorder-timeline.md). While anything records, the
// daemon keeps one JSON-lines file of what the window showed around the
// recorded panes: pane ids and handles, sizes, state marks, REC marks and stop
// codes, message chips as pane ids, counts, and the person's own view records
// (rectangles, focus, scroll, window state). Every field is an id, an enum or
// a bounded number: never a title, a workspace name, a path, a URL, notice
// text or anything typed. Off, it is inert: no file, no goroutine.

const (
	// recordingSessionsDirName holds the timelines, beside the pane folders.
	// A pane id can't start with a dot, so it never collides.
	recordingSessionsDirName = ".sessions"
	recordingTimelineVersion = 1
	// recordingTimelineTick is how often the daemon state is sampled while a
	// session is on.
	recordingTimelineTick = time.Second

	// View records: at most 4 KB each, at most 10 written per second (the
	// latest of a burst wins), with every number bounded.
	recordingViewMaxBytes   = 4 << 10
	recordingViewEvery      = 100 * time.Millisecond
	recordingViewMaxPanes   = 64
	recordingViewMaxGroups  = 64
	recordingViewMaxCoord   = 16384
	recordingViewMaxScroll  = 20000 // the app's largest terminal scrollback
	recordingViewMaxIndexes = 9999
	// recordingTimelineQueue bounds lines waiting for the disk; past it a
	// line is dropped and counted. Nothing that writes a line waits on disk.
	recordingTimelineQueue = 1024
)

var (
	recordingTimelineFilePattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.jsonl$`)
	recordingHandlePattern       = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)
	recordingWorkspaceIndex      = regexp.MustCompile(`^w[1-9][0-9]{0,3}$`)
)

// The timeline's event types. Nothing else is ever written.
const (
	tlHeader = "header"
	tlCast   = "cast"
	tlPane   = "pane"
	tlOpen   = "open"
	tlClose  = "close"
	tlSize   = "size"
	tlState  = "state"
	tlRec    = "rec"
	tlStop   = "stop"
	tlMsg    = "msg"
	tlCounts = "counts"
	tlView   = "view"
	tlAway   = "away"
	tlEnd    = "end"
)

var recordingTimelineEvents = map[string]bool{
	tlHeader: true, tlCast: true, tlPane: true, tlOpen: true, tlClose: true, tlSize: true, tlState: true,
	tlRec: true, tlStop: true, tlMsg: true, tlCounts: true, tlView: true, tlAway: true, tlEnd: true,
}

// Enums. An unknown daemon-side value is written as "other".
var (
	recordingViewStates  = map[string]bool{"shown": true, "minimised": true, "tray": true, "reopened": true}
	recordingViewThemes  = map[string]bool{"dark": true, "light": true}
	recordingViewPanels  = map[string]bool{"none": true, "open": true}
	recordingViewFonts   = map[string]bool{"jetbrains-mono": true, "other": true}
	recordingViewUnicode = map[int]bool{6: true, 11: true, 15: true}
)

// recordingStopCodes maps the stop reasons the person reads to the codes the
// timeline holds (never the text).
func recordingStopCode(reason string) string {
	switch reason {
	case recordingStopOff:
		return "switched_off"
	case recordingStopClosed:
		return "pane_closed"
	case recordingStopReplaced:
		return "pane_replaced"
	case recordingStopDiskCap:
		return "disk_cap"
	case recordingStopFolder:
		return "recordings_folder"
	case recordingStopWriteError:
		return "write_failed"
	case recordingStopDeleted:
		return "deleted"
	}
	for rule, text := range recordingRuleStop {
		if reason == text {
			return string(rule)
		}
	}
	if strings.HasSuffix(reason, "-hour limit") {
		return "time_limit"
	}
	// "disk cap (lowered by … at …)": the code only, never the text.
	if strings.HasPrefix(reason, recordingStopDiskCap) {
		return "disk_cap"
	}
	return "other"
}

// timelineLine is every field a timeline line may carry; each is an id, an
// enum or a bounded number.
type timelineLine struct {
	T  int64  `json:"t"`
	Ev string `json:"ev"`

	// header
	V     int    `json:"v,omitempty"`
	T0    int64  `json:"t0,omitempty"`
	App   string `json:"app,omitempty"`
	Build string `json:"build,omitempty"`
	Mode  string `json:"mode,omitempty"`

	Pane     string `json:"pane,omitempty"`
	From     string `json:"from,omitempty"`
	WS       string `json:"ws,omitempty"`
	Handle   string `json:"handle,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Recorded *bool  `json:"recorded,omitempty"`
	Reason   string `json:"reason,omitempty"`
	File     string `json:"file,omitempty"`
	OffsetMs *int64 `json:"offsetMs,omitempty"`
	Cols     uint16 `json:"cols,omitempty"`
	Rows     uint16 `json:"rows,omitempty"`
	Mark     string `json:"mark,omitempty"`
	On       *bool  `json:"on,omitempty"`

	Counts *timelineCounts `json:"counts,omitempty"`

	// view
	State   string                `json:"state,omitempty"`
	Window  *recordingViewSize    `json:"window,omitempty"`
	Theme   string                `json:"theme,omitempty"`
	Focus   string                `json:"focus,omitempty"`
	Panes   []recordingViewPane   `json:"panes,omitempty"`
	Sidebar *recordingViewSidebar `json:"sidebar,omitempty"`
	Panel   string                `json:"panel,omitempty"`
	Display *recordingViewDisplay `json:"display,omitempty"`
}

type timelineCounts struct {
	Panes      int `json:"panes"`
	Recording  int `json:"recording"`
	NeedsYou   int `json:"needsYou"`
	Workspaces int `json:"workspaces"`
}

// timelinePane is what was last written about a pane.
type timelinePane struct {
	ws, handle, kind, agent, reason, mark string
	recorded                              bool
	cols, rows                            uint16
}

// recordingTimeline is one session's file, open while anything records.
type recordingTimeline struct {
	m    *Manager
	hub  *recordingHub
	t0   time.Time
	path string

	mu     sync.Mutex
	f      *os.File
	closed bool
	// queue feeds the timeline's own writer; dropped counts lines a full
	// queue refused.
	queue      chan []byte
	dropped    int
	writerDone chan struct{}
	// ws maps a workspace name to its session index (w1, w2…), in memory only.
	ws     map[string]string
	panes  map[string]*timelinePane
	counts timelineCounts
	// shownWS is the session index the person's window last showed.
	lastView    time.Time
	pendingView *timelineLine
	viewTimer   *time.Timer

	stop chan struct{}
	done chan struct{}
}

func tlBool(b bool) *bool    { return &b }
func tlInt64(v int64) *int64 { return &v }

func recordingTimelineMode() string {
	switch {
	case runtime.GOOS == "windows":
		return "windows"
	case strings.TrimSpace(os.Getenv("WSL_DISTRO_NAME")) != "":
		return "wsl"
	}
	return "other"
}

// recordingSessionsDir is recordings/.sessions, checked like a pane folder.
func (h *recordingHub) sessionsDir() string { return filepath.Join(h.root(), recordingSessionsDirName) }

func (h *recordingHub) prepareSessions() (string, error) {
	if err := ensureRecordingDir(h.root()); err != nil {
		return "", err
	}
	dir := h.sessionsDir()
	if err := ensureRecordingDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// timelineEnsure starts the session's timeline when the first pane switches
// on. A timeline that can't be created is logged; the recording goes on.
// timelineMu serializes only starting and ending a session; the hooks read
// the current timeline without it (a recorder's file hook runs under its
// pane's lock, which the ending session's sampler may be waiting for).
func (m *Manager) timelineEnsure(hub *recordingHub) {
	m.timelineMu.Lock()
	defer m.timelineMu.Unlock()
	if m.timeline.Load() != nil {
		return
	}
	t, err := startRecordingTimeline(m, hub)
	if err != nil {
		log.Printf("recording: session timeline not started: %v", err)
		return
	}
	m.timeline.Store(t)
}

// timelineEndIfIdle closes the timeline once nothing records and no
// switch-on is in progress; the file is complete once this returns.
func (m *Manager) timelineEndIfIdle() {
	m.timelineMu.Lock()
	defer m.timelineMu.Unlock()
	t := m.timeline.Load()
	if t == nil || m.recordersLive.Load() > 0 {
		return
	}
	t.end()
	m.timeline.Store(nil)
}

func (m *Manager) currentTimeline() *recordingTimeline {
	if m == nil {
		return nil
	}
	return m.timeline.Load()
}

func startRecordingTimeline(m *Manager, hub *recordingHub) (*recordingTimeline, error) {
	dir, err := hub.prepareSessions()
	if err != nil {
		return nil, err
	}
	t0 := hub.now()
	f, path, err := hub.createFileExt(dir, t0, ".jsonl")
	if err != nil {
		return nil, err
	}
	t := &recordingTimeline{
		m: m, hub: hub, t0: t0, path: path, f: f,
		ws: map[string]string{}, panes: map[string]*timelinePane{},
		stop: make(chan struct{}), done: make(chan struct{}),
		queue: make(chan []byte, recordingTimelineQueue), writerDone: make(chan struct{}),
	}
	go t.writer()
	build := buildSourceSha
	if len(build) > 12 {
		build = build[:12]
	}
	t.mu.Lock()
	t.writeLocked(timelineLine{Ev: tlHeader, V: recordingTimelineVersion, T0: t0.UnixMilli(), App: version, Build: build, Mode: recordingTimelineMode()})
	t.mu.Unlock()
	t.sample()
	go t.run()
	return t, nil
}

func (t *recordingTimeline) run() {
	defer close(t.done)
	tick := time.NewTicker(recordingTimelineTick)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.sample()
		}
	}
}

func (t *recordingTimeline) end() {
	close(t.stop)
	<-t.done
	t.mu.Lock()
	if t.viewTimer != nil {
		t.viewTimer.Stop()
	}
	t.flushViewLocked()
	t.writeLocked(timelineLine{Ev: tlEnd})
	t.closed = true
	close(t.queue)
	dropped := t.dropped
	t.mu.Unlock()
	<-t.writerDone
	_ = t.f.Close()
	t.hub.closed(t.path)
	if dropped > 0 {
		log.Printf("recording: session timeline dropped %d lines while the disk was behind", dropped)
	}
}

// writeLocked queues one line for the writer. A full queue drops it.
func (t *recordingTimeline) writeLocked(l timelineLine) {
	if t.closed || !recordingTimelineEvents[l.Ev] {
		return
	}
	if l.Ev != tlHeader {
		l.T = t.hub.now().Sub(t.t0).Milliseconds()
	}
	b, err := json.Marshal(l)
	if err != nil {
		return
	}
	select {
	case t.queue <- append(b, '\n'):
	default:
		t.dropped++
	}
}

// writer appends queued lines within the shared cap. A refused reservation or
// a failed write ends the timeline's writing (the recordings go on).
func (t *recordingTimeline) writer() {
	defer close(t.writerDone)
	failed := false
	for b := range t.queue {
		if failed {
			continue
		}
		if !t.hub.reserve(int64(len(b))) {
			failed = true
			log.Printf("recording: session timeline stopped: %s", recordingStopDiskCap)
			continue
		}
		n, err := t.f.Write(b)
		t.hub.written(int64(n))
		if n < len(b) {
			t.hub.unreserve(int64(len(b) - n))
		}
		if err != nil || n != len(b) {
			failed = true
			log.Printf("recording: session timeline stopped: %s", recordingStopWriteError)
		}
	}
}

func (t *recordingTimeline) write(l timelineLine) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writeLocked(l)
}

// wsIndexLocked returns the session index for a workspace name.
func (t *recordingTimeline) wsIndexLocked(name string) string {
	if idx, ok := t.ws[name]; ok {
		return idx
	}
	idx := fmt.Sprintf("w%d", len(t.ws)+1)
	t.ws[name] = idx
	return idx
}

// timelineAgent is the pane's agent as an enum.
func timelineAgent(s PaneSnapshot) string {
	switch recordingPaneAgent(s.Agent, s.Cmd) {
	case agentClaude:
		return "claude"
	case agentCodex:
		return "codex"
	case "":
		return "shell"
	}
	return "other"
}

func timelineKind(s PaneSnapshot) string {
	if s.Kind == "" {
		return "terminal"
	}
	return "other"
}

// timelineMark is the pane's state mark from daemon state.
func timelineMark(p *Pane, s PaneSnapshot) string {
	if s.Status != PaneRunning {
		return "exited"
	}
	state := p.computeStateCached()
	att, _ := p.AttentionNow()
	switch {
	case state == StateChoicePrompt:
		return "choice_prompt"
	case att == AttentionNeedsUser:
		return "needs_you"
	case state == StateBusy:
		return "working"
	case state == StateRecent:
		return "done_recent"
	case state == StateIdle || state == StateAwaitingInput:
		return "idle"
	case state == StateExited:
		return "exited"
	}
	return "other"
}

func timelineHandle(id *PaneIdentity) string {
	h := identityHandle(id)
	if recordingHandlePattern.MatchString(h) {
		return h
	}
	return ""
}

// sample reads daemon state once: the workspaces holding a recording pane,
// their panes and tiles, sizes, marks, REC state and the counts, and writes
// what changed.
func (t *recordingTimeline) sample() {
	all := t.m.List()
	recording := map[string]bool{}
	notice := map[string]string{}
	needsYou := 0
	for _, p := range all {
		if p.recording.active.Load() != nil {
			recording[p.ID] = true
		} else if d := p.Recording(); d != nil && d.StoppedReason != "" {
			notice[p.ID] = recordingStopCode(d.StoppedReason)
		}
		if att, _ := p.AttentionNow(); att == AttentionNeedsUser {
			needsYou++
		}
	}
	var workspaces []*Workspace
	if t.m.recordingWorkspaces != nil {
		workspaces = t.m.recordingWorkspaces()
	}
	type member struct {
		ws      string
		browser bool
	}
	members := map[string]member{}
	for _, w := range workspaces {
		hasRecording := false
		for _, id := range w.PaneIds {
			if recording[id] {
				hasRecording = true
				break
			}
		}
		if !hasRecording {
			continue
		}
		for _, id := range w.PaneIds {
			members[id] = member{ws: w.Name}
		}
		for _, b := range w.Browsers {
			if recordingPaneIDPattern.MatchString(b.ID) {
				members[b.ID] = member{ws: w.Name, browser: true}
			}
		}
	}
	// Without workspace data (tests, a daemon with no store) the recording
	// panes stand alone.
	for id := range recording {
		if _, ok := members[id]; !ok {
			members[id] = member{}
		}
	}
	byID := map[string]*Pane{}
	for _, p := range all {
		byID[p.ID] = p
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	initial := len(t.panes) == 0
	ids := make([]string, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		mem := members[id]
		next := &timelinePane{reason: "not_recorded"}
		if mem.ws != "" {
			next.ws = t.wsIndexLocked(mem.ws)
		}
		p := byID[id]
		switch {
		case mem.browser:
			next.kind, next.agent, next.reason = "browser", "other", "browser_pane"
		case p == nil:
			continue // a file tile or a pane that is gone
		default:
			s := p.Snapshot()
			next.kind, next.agent = timelineKind(s), timelineAgent(s)
			next.handle = timelineHandle(s.Identity)
			next.cols, next.rows = s.Cols, s.Rows
			next.mark = timelineMark(p, s)
			next.recorded = recording[id]
			if next.recorded {
				next.reason = ""
			} else if code := notice[id]; code != "" {
				next.reason = code
			}
		}
		prev := t.panes[id]
		if prev == nil && !initial {
			t.writeLocked(timelineLine{Ev: tlOpen, Pane: id})
		}
		if prev == nil || prev.ws != next.ws || prev.handle != next.handle || prev.kind != next.kind ||
			prev.agent != next.agent || prev.recorded != next.recorded || prev.reason != next.reason {
			t.writeLocked(timelineLine{Ev: tlPane, Pane: id, WS: next.ws, Handle: next.handle, Kind: next.kind,
				Agent: next.agent, Recorded: tlBool(next.recorded), Reason: next.reason})
		}
		if next.kind != "browser" && (prev == nil || prev.cols != next.cols || prev.rows != next.rows) && next.cols > 0 {
			t.writeLocked(timelineLine{Ev: tlSize, Pane: id, Cols: next.cols, Rows: next.rows})
		}
		if next.mark != "" && (prev == nil || prev.mark != next.mark) {
			t.writeLocked(timelineLine{Ev: tlState, Pane: id, Mark: next.mark})
		}
		t.panes[id] = next
	}
	for id := range t.panes {
		if _, ok := members[id]; !ok || (byID[id] == nil && !members[id].browser) {
			t.writeLocked(timelineLine{Ev: tlClose, Pane: id})
			delete(t.panes, id)
		}
	}
	counts := timelineCounts{Panes: len(all), Recording: len(recording), NeedsYou: needsYou, Workspaces: len(workspaces)}
	if counts != t.counts || initial {
		t.counts = counts
		c := counts
		t.writeLocked(timelineLine{Ev: tlCounts, Counts: &c})
	}
}

// timelineCast records a .cast file's start on the session clock (the
// shared clock): its base name only, never a path.
func (m *Manager) timelineCast(paneID, path string, start time.Time) {
	if t := m.currentTimeline(); t != nil {
		t.write(timelineLine{Ev: tlCast, Pane: paneID, File: filepath.Base(path), OffsetMs: tlInt64(start.Sub(t.t0).Milliseconds())})
	}
}

// timelineSample samples daemon state now, so a pane and its workspace are
// in the timeline at once. The caller holds no pane recording lock.
func (m *Manager) timelineSample() {
	if t := m.currentTimeline(); t != nil {
		t.sample()
	}
}

// timelineRec records a REC mark going on or off, and a stop's code. It
// takes only the timeline's own lock, so a switch-on writes it under the
// pane's lock in order with any stop that follows.
func (m *Manager) timelineRec(paneID string, on bool, reason string) {
	t := m.currentTimeline()
	if t == nil {
		return
	}
	t.write(timelineLine{Ev: tlRec, Pane: paneID, On: tlBool(on)})
	if !on && reason != "" {
		t.write(timelineLine{Ev: tlStop, Pane: paneID, Reason: recordingStopCode(reason)})
	}
}

// recordingTimelineMessage records a message chip: pane to received a
// message from pane from. Pane ids only, and only between panes the session
// already holds.
func (m *Manager) recordingTimelineMessage(to, from string) {
	t := m.currentTimeline()
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.panes[to] == nil {
		return
	}
	line := timelineLine{Ev: tlMsg, Pane: to}
	if t.panes[from] != nil {
		line.From = from
	}
	t.writeLocked(line)
}

// --- the person's view records ------------------------------------------------

type recordingViewSize struct {
	W int `json:"w"`
	H int `json:"h"`
}

type recordingViewPane struct {
	ID     string `json:"id"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Max    bool   `json:"max,omitempty"`
	Scroll int    `json:"scroll,omitempty"`
}

type recordingViewSidebar struct {
	Collapsed    bool     `json:"collapsed"`
	ClosedGroups []string `json:"closedGroups,omitempty"`
}

// recordingViewDisplay: devicePixelRatio and zoom ×100, line height ×100.
type recordingViewDisplay struct {
	DPR           int    `json:"dpr"`
	Zoom          int    `json:"zoom"`
	Font          string `json:"font"`
	Size          int    `json:"size"`
	LineHeight    int    `json:"lineHeight"`
	LetterSpacing int    `json:"letterSpacing"`
	Xterm         []int  `json:"xterm"`
	Unicode       int    `json:"unicode"`
}

type recordingViewReq struct {
	State   string                `json:"state"`
	Away    bool                  `json:"away,omitempty"`
	Window  *recordingViewSize    `json:"window,omitempty"`
	Theme   string                `json:"theme,omitempty"`
	Focus   string                `json:"focus,omitempty"`
	Panes   []recordingViewPane   `json:"panes,omitempty"`
	Sidebar *recordingViewSidebar `json:"sidebar,omitempty"`
	Panel   string                `json:"panel,omitempty"`
	Display *recordingViewDisplay `json:"display,omitempty"`
}

var errRecordingViewNotRecording = errors.New("nothing in this view is being recorded")

func inRange(v, lo, hi int) bool { return v >= lo && v <= hi }

// validate checks the record's enums and bounds; it never trims or clamps.
func (v *recordingViewReq) validate() error {
	if !recordingViewStates[v.State] {
		return errors.New(`"state" must be shown, minimised, tray or reopened`)
	}
	if v.Away {
		if v.State != "shown" || v.Window != nil || v.Theme != "" || v.Focus != "" || v.Panes != nil || v.Sidebar != nil || v.Panel != "" || v.Display != nil {
			return errors.New(`an away record is exactly {"state":"shown","away":true}`)
		}
		return nil
	}
	if v.Window == nil || !inRange(v.Window.W, 1, recordingViewMaxCoord) || !inRange(v.Window.H, 1, recordingViewMaxCoord) {
		return errors.New(`"window" needs w and h from 1 to 16384`)
	}
	if v.Theme != "" && !recordingViewThemes[v.Theme] {
		return errors.New(`"theme" must be dark or light`)
	}
	if v.Panel != "" && !recordingViewPanels[v.Panel] {
		return errors.New(`"panel" must be none or open`)
	}
	if v.Focus != "" && !recordingPaneIDPattern.MatchString(v.Focus) {
		return errors.New(`"focus" is not a pane id`)
	}
	if len(v.Panes) > recordingViewMaxPanes {
		return errors.New("too many panes")
	}
	for _, p := range v.Panes {
		if !recordingPaneIDPattern.MatchString(p.ID) {
			return errors.New("a pane id is not an id")
		}
		if !inRange(p.X, 0, recordingViewMaxCoord) || !inRange(p.Y, 0, recordingViewMaxCoord) ||
			!inRange(p.W, 0, recordingViewMaxCoord) || !inRange(p.H, 0, recordingViewMaxCoord) {
			return errors.New("a pane rectangle is out of bounds")
		}
		if !inRange(p.Scroll, 0, recordingViewMaxScroll) {
			return errors.New(`"scroll" must be 0 to 20000 lines`)
		}
	}
	if s := v.Sidebar; s != nil {
		if len(s.ClosedGroups) > recordingViewMaxGroups {
			return errors.New("too many sidebar groups")
		}
		for _, g := range s.ClosedGroups {
			if !recordingWorkspaceIndex.MatchString(g) {
				return errors.New("a sidebar group is not a session index")
			}
		}
	}
	if d := v.Display; d != nil {
		if !inRange(d.DPR, 25, 1000) || !inRange(d.Zoom, 25, 500) || !recordingViewFonts[d.Font] || !inRange(d.Size, 6, 72) ||
			!inRange(d.LineHeight, 80, 300) || !inRange(d.LetterSpacing, -10, 20) || !recordingViewUnicode[d.Unicode] {
			return errors.New(`"display" is out of bounds`)
		}
		if len(d.Xterm) != 3 {
			return errors.New(`"display.xterm" is three numbers`)
		}
		for _, n := range d.Xterm {
			if !inRange(n, 0, 999) {
				return errors.New(`"display.xterm" is out of bounds`)
			}
		}
	}
	return nil
}

// RecordingView takes one view record from the person's own window. It is
// written only while a recording is on: a record of a workspace without a
// recording pane is refused, except the bare "away" record. Unknown pane ids
// are dropped; the response maps the session's workspace names to their
// indexes, for the window's sidebar groups (it is never written).
func (m *Manager) RecordingView(v recordingViewReq) (map[string]string, error) {
	t := m.currentTimeline()
	if t == nil {
		return nil, errRecordingViewNotRecording
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errRecordingViewNotRecording
	}
	line := &timelineLine{Ev: tlView, State: v.State}
	if v.Away {
		line.Ev = tlAway
	} else {
		votes := map[string]int{}
		for _, p := range v.Panes {
			if tp := t.panes[p.ID]; tp != nil {
				line.Panes = append(line.Panes, p)
				if tp.ws != "" {
					votes[tp.ws]++
				}
			}
		}
		if len(line.Panes) == 0 {
			return nil, errRecordingViewNotRecording
		}
		best := ""
		for ws, n := range votes {
			if best == "" || n > votes[best] || (n == votes[best] && ws < best) {
				best = ws
			}
		}
		line.WS = best
		line.Window, line.Theme, line.Panel, line.Display = v.Window, v.Theme, v.Panel, v.Display
		if v.Focus != "" && t.panes[v.Focus] != nil {
			line.Focus = v.Focus
		}
		if s := v.Sidebar; s != nil {
			known := map[string]bool{}
			for _, idx := range t.ws {
				known[idx] = true
			}
			kept := &recordingViewSidebar{Collapsed: s.Collapsed}
			for _, g := range s.ClosedGroups {
				if known[g] {
					kept.ClosedGroups = append(kept.ClosedGroups, g)
				}
			}
			line.Sidebar = kept
		}
	}
	t.pendingView = line
	if wait := recordingViewEvery - t.hub.now().Sub(t.lastView); wait <= 0 {
		t.flushViewLocked()
	} else if t.viewTimer == nil {
		t.viewTimer = time.AfterFunc(wait, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.viewTimer = nil
			t.flushViewLocked()
		})
	}
	indexes := make(map[string]string, len(t.ws))
	for name, idx := range t.ws {
		indexes[name] = idx
	}
	return indexes, nil
}

func (t *recordingTimeline) flushViewLocked() {
	if t.pendingView == nil {
		return
	}
	t.writeLocked(*t.pendingView)
	t.pendingView = nil
	t.lastView = t.hub.now()
}

// --- deletion -----------------------------------------------------------------

// timelineCastRefsLocked lists the .cast files a finished timeline refers to,
// as recordings/<pane>/<file> paths. It reads only "cast" lines and builds
// paths from checked ids and names.
func (h *recordingHub) timelineCastRefsLocked(path string) ([]string, error) {
	f, err := readRecordingTimeline(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var refs []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), recordingViewMaxBytes*4)
	for sc.Scan() {
		var l struct {
			Ev   string `json:"ev"`
			Pane string `json:"pane"`
			File string `json:"file"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Ev != tlCast {
			continue
		}
		if recordingPaneIDPattern.MatchString(l.Pane) && recordingFileNamePattern.MatchString(l.File) {
			refs = append(refs, filepath.Join(h.root(), l.Pane, l.File))
		}
	}
	return refs, sc.Err()
}

// pruneTimelinesLocked deletes every finished timeline that no longer refers
// to a remaining recording (Addendum B 6). Run at the person's delete and at
// every sweep.
func (h *recordingHub) pruneTimelinesLocked() (deleted int) {
	// Only inside a checked recordings/ and .sessions: never through a
	// symlinked, foreign or open folder.
	dir := h.sessionsDir()
	if _, err := os.Lstat(dir); err != nil || checkRecordingDir(h.root()) != nil || checkRecordingDir(dir) != nil {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if !e.Type().IsRegular() || !recordingTimelineFilePattern.MatchString(e.Name()) || h.open[path] {
			continue
		}
		refs, err := h.timelineCastRefsLocked(path)
		if err != nil {
			continue
		}
		remaining := false
		for _, ref := range refs {
			if info, err := os.Lstat(ref); err == nil && info.Mode().IsRegular() {
				remaining = true
				break
			}
		}
		if !remaining && os.Remove(path) == nil {
			deleted++
		}
	}
	return deleted
}

// isTimelineFile reports whether a recordings/ subfolder entry is one of the
// timelines (sweep and cap count them with the .cast files).
func isTimelineEntry(dir, name string) bool {
	return filepath.Base(dir) == recordingSessionsDirName && recordingTimelineFilePattern.MatchString(name)
}
