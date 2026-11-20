package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
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
	recordingStats
}

// Test seams. Production uses the policy constants and the live process tree.
var (
	recordingCheckEvery = recordingPolicyCheckEvery
	recordingMaxOnFor   = recordingMaxOn
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
		return &paneRecordingDTO{On: true, recordingStats: r.stats()}
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
// becomes one (f)1 refuses.
func (p *Pane) recheckRecordingHandle(handle string) {
	if p.recording.active.Load() == nil {
		return
	}
	if recordingRefusal(recordingPaneFacts{Handle: handle, Agent: "claude", AgentRunning: true}, "", "") == recordingStopHandle {
		if p.stopRecording(recordingStopHandle, true) {
			log.Printf("recording: pane %s switch-off by the daemon: %s", p.ID, recordingStopHandle)
		}
	}
}

// liveRecordingFacts reads what the (f) rules need from the pane and its
// process tree.
func liveRecordingFacts(p *Pane) recordingPaneFacts {
	p.mu.Lock()
	f := recordingPaneFacts{Handle: identityHandle(p.Identity), Agent: recordingPaneAgent(p.Agent, p.Cmd), Cwd: p.Cwd}
	pid := p.Pid
	p.mu.Unlock()
	if pid <= 0 {
		return f
	}
	tree := snapshotProcessTreeMaxAge(time.Second)
	pids := tree.descendantPids(pid)
	f.LiveCwds, f.CwdUnknown = recordingLiveCwds(append([]int{pid}, pids...))
	for _, child := range pids {
		if f.Agent != "" && child != pid && readComm(child) == f.Agent {
			f.AgentRunning = true
			break
		}
	}
	return f
}

// recordingPaneAgent is the agent a pane was started as, read the way the
// app's effectivePaneAgent reads it. A Claude pane is stored with Agent ""
// (the legacy zero value, healPaneIdentity), so a "" pane is Claude only when
// its own launch command runs claude: bare, or as the 'claude' token of the
// login-shell wrapper (cmdRunsClaude, as the app's agentFromCommand). A plain
// shell where someone typed claude stays "", and (f)3 still refuses it.
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

// recordingLiveCwds reads the current folder of every process in the pane.
// Where that can't be read at all (no /proc, as on macOS), the folder is
// unknown and (f)2 refuses the pane.
func recordingLiveCwds(pids []int) (cwds []string, unknown bool) {
	if runtime.GOOS != "linux" {
		return nil, true
	}
	read := 0
	for _, pid := range pids {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue // exited meanwhile
		}
		if err != nil {
			// A live process whose folder can't be read (another user's, as
			// under sudo) may be anywhere.
			return cwds, true
		}
		read++
		cwds = append(cwds, target)
	}
	return cwds, read == 0
}

// recordingRefusedError is an (f) refusal of a switch-on.
type recordingRefusedError struct{ reason string }

func (e recordingRefusedError) Error() string { return "this pane can't be recorded: " + e.reason }

var (
	errRecordingPlatform   = errors.New("recording is not available on Windows yet")
	errRecordingNotRunning = errors.New("the pane is not running")
	errRecordingNoDataDir  = errors.New("no data folder for recordings")
)

// StartRecording switches recording on for a running pane. The caller has
// already admitted the person (local, human-only).
func (m *Manager) StartRecording(p *Pane, via string) (*paneRecordingDTO, error) {
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
	home, _ := os.UserHomeDir()
	// The watcher keeps these for the recording's life.
	w := recordingWatch{every: recordingCheckEvery, maxOn: recordingMaxOnFor, facts: recordingFactsOf, home: home}
	if reason := recordingRefusal(w.facts(p), home, hub.dataDir); reason != "" {
		return nil, recordingRefusedError{reason}
	}
	p.recording.mu.Lock()
	defer p.recording.mu.Unlock()
	if p.recording.active.Load() != nil {
		return p.recordingDTOLocked(), nil
	}
	hub.sweepIfDue()
	p.mu.Lock()
	cols, rows := p.Cols, p.Rows
	p.mu.Unlock()
	verify := func() string { return recordingRefusal(w.facts(p), w.home, hub.dataDir) }
	rec, err := startPaneRecorder(hub, p.ID, cols, rows, verify, func(r *paneRecorder, reason string) { m.recordingFinished(p, r, reason) })
	if err == nil {
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
		// SetIdentity and markExited change the pane, then look for a
		// recorder; this publishes the recorder, then looks at the pane, so
		// a change made while the file was being created is never missed.
		if p.Snapshot().Status != PaneRunning {
			p.stopRecording(recordingStopClosed, true)
		} else if reason := recordingRefusal(w.facts(p), w.home, hub.dataDir); reason != "" {
			p.stopRecording(reason, true)
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
	log.Printf("recording: pane %s on by the person (%s)", p.ID, via)
	go m.watchRecording(p, rec, w)
	go m.publishRecording(p)
	return p.recordingDTOLocked(), nil
}

func (p *Pane) recordingDTOLocked() *paneRecordingDTO {
	if r := p.recording.active.Load(); r != nil {
		return &paneRecordingDTO{On: true, recordingStats: r.stats()}
	}
	return &paneRecordingDTO{}
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
// recordingPolicyCheckEvery it re-checks the (f) rules and that the pane is
// still this running pane, and it ends the recording at the 8-hour limit.
type recordingWatch struct {
	every, maxOn time.Duration
	facts        func(*Pane) recordingPaneFacts
	home         string
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
			if p.recording.active.Load() == rec && p.stopRecording(recordingStopMaxOn, false) {
				log.Printf("recording: pane %s switch-off by the daemon: %s", p.ID, recordingStopMaxOn)
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
			if reason := recordingRefusal(w.facts(p), w.home, rec.hub.dataDir); reason != "" {
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
}

// publishRecording pushes the pane's recording state to every client.
func (m *Manager) publishRecording(p *Pane) {
	if m == nil || m.bus == nil {
		return
	}
	m.bus.publish(uiEvent{Type: "pane-recording", PaneID: p.ID, Recording: p.recordingEventDTO(), Ts: time.Now().UnixMilli()})
}
