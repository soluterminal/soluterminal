package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every test here records synthetic output from a fake agent connection. No
// real pane is ever recorded.

type recTestPane struct {
	t      *testing.T
	m      *Manager
	p      *Pane
	server net.Conn
	data   string
	facts  atomic.Pointer[recordingPaneFacts]
	checks atomic.Int64
	// hold, when set, parks the next check (after signalling blocked) until
	// it is closed.
	hold    atomic.Pointer[chan struct{}]
	blocked chan struct{}
}

func allowedRecordingFacts() recordingPaneFacts {
	return recordingPaneFacts{Agent: "claude", Cwd: "/work/project", AgentRunning: true}
}

// newRecTestPane runs a real readLoop on a net.Pipe agent connection, with the
// (f) facts and check interval under the test's control.
func newRecTestPane(t *testing.T) *recTestPane {
	t.Helper()
	data := filepath.Join(t.TempDir(), "soludata")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{panes: map[string]*Pane{}, dataDir: data}
	client, server := net.Pipe()
	p := &Pane{ID: "pane_rectest", Status: PaneRunning, agentConn: client, Agent: "claude", Cwd: "/work/project",
		Cols: 80, Rows: 24, buffer: NewRingBuffer(16 << 20), subs: map[int64]*subEntry{}, done: make(chan struct{})}
	m.panes[p.ID] = p
	tp := &recTestPane{t: t, m: m, p: p, server: server, data: data, blocked: make(chan struct{}, 1)}
	f := allowedRecordingFacts()
	tp.facts.Store(&f)
	prevFacts, prevEvery, prevMax := recordingFactsOf, recordingCheckEvery, recordingMaxOnFor
	recordingFactsOf = func(*Pane) recordingPaneFacts {
		tp.checks.Add(1)
		if h := tp.hold.Swap(nil); h != nil {
			tp.blocked <- struct{}{}
			<-*h
		}
		return *tp.facts.Load()
	}
	recordingCheckEvery = 20 * time.Millisecond
	t.Cleanup(func() { recordingFactsOf, recordingCheckEvery, recordingMaxOnFor = prevFacts, prevEvery, prevMax })
	// Drain what the pane writes to its agent (resize frames).
	go func() {
		for {
			if _, _, err := ReadFrame(server); err != nil {
				return
			}
		}
	}()
	go p.readLoop(m)
	t.Cleanup(func() { server.Close(); <-p.done })
	return tp
}

func (tp *recTestPane) send(kind FrameType, payload []byte) {
	tp.t.Helper()
	if err := WriteFrame(tp.server, kind, payload); err != nil {
		tp.t.Fatal(err)
	}
}

// sync returns once every earlier frame has been processed: readLoop reads the
// next header only after finishing the previous payload.
func (tp *recTestPane) sync() {
	tp.t.Helper()
	tp.send(FrameAgentInfo, []byte(`{}`))
	tp.send(FrameAgentInfo, []byte(`{}`))
}

func (tp *recTestPane) start() *paneRecorder {
	tp.t.Helper()
	if _, err := tp.m.StartRecording(tp.p, "local"); err != nil {
		tp.t.Fatalf("start: %v", err)
	}
	rec := tp.p.recording.active.Load()
	if rec == nil {
		tp.t.Fatal("no recorder after start")
	}
	return rec
}

func waitRecorder(t *testing.T, rec *paneRecorder) {
	t.Helper()
	if !rec.wait(5 * time.Second) {
		t.Fatal("recorder did not finish")
	}
}

type castEvent struct {
	t    float64
	code string
	data string
}

func readCast(t *testing.T, path string) (map[string]any, []castEvent) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	if !sc.Scan() {
		t.Fatalf("%s: no header", path)
	}
	var header map[string]any
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		t.Fatalf("header: %v", err)
	}
	var events []castEvent
	for sc.Scan() {
		var raw []any
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil || len(raw) != 3 {
			t.Fatalf("event %q: %v", sc.Text(), err)
		}
		events = append(events, castEvent{raw[0].(float64), raw[1].(string), raw[2].(string)})
	}
	return header, events
}

func castFiles(t *testing.T, data, paneID string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(data, recordingsDirName, paneID, "*.cast"))
	return files
}

func outputOf(events []castEvent) string {
	var b strings.Builder
	for _, e := range events {
		if e.code == "o" {
			b.WriteString(e.data)
		}
	}
	return b.String()
}

func marks(events []castEvent) []string {
	var out []string
	for _, e := range events {
		if e.code == "m" {
			out = append(out, e.data)
		}
	}
	return out
}

func TestRecorderRecordsOnlyLiveOutputWithAMinimalHeader(t *testing.T) {
	tp := newRecTestPane(t)
	tp.send(FrameData, []byte("before recording\r\n"))
	tp.sync() // processed before the switch goes on
	rec := tp.start()
	// Replay, synthetic paint and daemon-drawn bytes must never be recorded.
	tp.send(FrameReplay, []byte("REPLAY-HISTORY"))
	tp.send(FrameSyntheticPaint, []byte("SYNTHETIC-PAINT"))
	want := ""
	for _, chunk := range [][]byte{[]byte("\x1b[1mhello\x1b[0m "), []byte("caf\xc3"), []byte("\xa9 \xf0\x9f"), []byte("\x98\x80 done\r\n")} {
		tp.send(FrameData, chunk)
		want += string(chunk)
	}
	tp.sync()
	tp.m.StopRecording(tp.p, "local human")
	waitRecorder(t, rec)
	files := castFiles(t, tp.data, tp.p.ID)
	if len(files) != 1 {
		t.Fatalf("files: %v", files)
	}
	header, events := readCast(t, files[0])
	keys := []string{}
	for k := range header {
		keys = append(keys, k)
	}
	if len(header) != 4 || header["version"] != float64(2) || header["width"] != float64(80) || header["height"] != float64(24) || header["timestamp"] == nil {
		t.Fatalf("header must hold only version, width, height, timestamp: %v", header)
	}
	for _, forbidden := range []string{"env", "command", "title", "cwd"} {
		if _, ok := header[forbidden]; ok {
			t.Fatalf("header has %s", forbidden)
		}
	}
	got := outputOf(events)
	if got != want {
		t.Fatalf("recorded %q, want %q", got, want)
	}
	for _, e := range events {
		if strings.Contains(e.data, "REPLAY") || strings.Contains(e.data, "SYNTHETIC") || strings.Contains(e.data, "before recording") {
			t.Fatalf("non-live output recorded: %q", e.data)
		}
	}
	st := rec.stats()
	if st.BytesRecorded != int64(len(want)) || st.BytesDropped != 0 || st.BytesReplaced != 0 {
		t.Fatalf("stats %+v, want %d recorded", st, len(want))
	}
	if m := marks(events); len(m) != 1 || m[0] != "stopped: "+recordingStopOff {
		t.Fatalf("marks %v", m)
	}
}

func TestRecorderCountsBytesThatAreNotUTF8(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	tp.send(FrameData, []byte("ok \xff\xfe end"))
	tp.sync()
	tp.m.StopRecording(tp.p, "local human")
	waitRecorder(t, rec)
	_, events := readCast(t, castFiles(t, tp.data, tp.p.ID)[0])
	if got := outputOf(events); got != "ok �� end" {
		t.Fatalf("got %q", got)
	}
	if st := rec.stats(); st.BytesReplaced != 2 || st.BytesRecorded != int64(len("ok \xff\xfe end")) {
		t.Fatalf("stats %+v", st)
	}
}

func TestRecorderResizeWritesAnREvent(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	tp.send(FrameData, []byte("wide"))
	tp.sync()
	if err := tp.p.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	tp.send(FrameData, []byte("narrow"))
	tp.sync()
	tp.m.StopRecording(tp.p, "local human")
	waitRecorder(t, rec)
	_, events := readCast(t, castFiles(t, tp.data, tp.p.ID)[0])
	var order []string
	for _, e := range events {
		order = append(order, e.code+":"+e.data)
	}
	joined := strings.Join(order, "|")
	if !strings.Contains(joined, "o:wide|r:100x30|o:narrow") {
		t.Fatalf("events %s", joined)
	}
}

func TestRecorderBlockedWriterNeverStallsReadLoop(t *testing.T) {
	tp := newRecTestPane(t)
	hub := tp.m.recordingHub()
	release := make(chan struct{})
	var calls atomic.Int64
	hub.write = func(f *os.File, b []byte) (int, error) {
		if calls.Add(1) > 1 { // the header goes through; then the disk "hangs"
			<-release
		}
		return f.Write(b)
	}
	rec := tp.start()
	before := tp.p.buffer.TotalWritten()
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	const chunks = 160 // 10 MiB, well past the 4 MiB queue
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < chunks; i++ {
		tp.send(FrameData, chunk)
		if time.Now().After(deadline) {
			t.Fatal("readLoop stalled behind a blocked disk")
		}
	}
	tp.sync()
	total := int64(chunks * len(chunk))
	if got := tp.p.buffer.TotalWritten() - before; got != total {
		t.Fatalf("pane output changed: %d of %d", got, total)
	}
	if rec.stats().BytesDropped == 0 {
		t.Fatal("no drops counted with a blocked writer")
	}
	close(release)
	tp.m.StopRecording(tp.p, "local human")
	waitRecorder(t, rec)
	st := rec.stats()
	if st.BytesRecorded+st.BytesDropped != total {
		t.Fatalf("recorded %d + dropped %d != %d", st.BytesRecorded, st.BytesDropped, total)
	}
	_, events := readCast(t, castFiles(t, tp.data, tp.p.ID)[0])
	var dropped int64
	for _, m := range marks(events) {
		var n int64
		if _, err := fmt.Sscanf(m, "dropped %d bytes", &n); err == nil {
			dropped += n
		}
	}
	if dropped != st.BytesDropped {
		t.Fatalf("gap marks say %d, stats %d", dropped, st.BytesDropped)
	}
	if int64(len(outputOf(events))) != st.BytesRecorded {
		t.Fatalf("file holds %d, stats %d", len(outputOf(events)), st.BytesRecorded)
	}
}

func TestRecordingOffCreatesNothing(t *testing.T) {
	tp := newRecTestPane(t)
	tp.send(FrameData, []byte("output while off"))
	if err := tp.p.Resize(90, 20); err != nil {
		t.Fatal(err)
	}
	tp.sync()
	tp.m.recordingHub().sweep()
	tp.m.StopRecording(tp.p, "agent")
	if _, err := os.Lstat(filepath.Join(tp.data, recordingsDirName)); !os.IsNotExist(err) {
		t.Fatalf("recordings folder exists with recording off: %v", err)
	}
	if tp.p.Recording() != nil || tp.p.recording.active.Load() != nil {
		t.Fatal("recording state with recording off")
	}
}

func TestRecordingOffWhenThePaneCloses(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	tp.send(FrameData, []byte("x"))
	tp.sync()
	tp.server.Close()
	<-tp.p.done
	waitRecorder(t, rec)
	if rec.reason != recordingStopClosed || tp.p.recording.active.Load() != nil {
		t.Fatalf("reason %q", rec.reason)
	}
}

func TestRecordingOffWhenThePaneIsReplaced(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	// A respawn or resume puts a new Pane under the same id.
	tp.m.mu.Lock()
	tp.m.panes[tp.p.ID] = &Pane{ID: tp.p.ID, Status: PaneRunning}
	tp.m.mu.Unlock()
	waitRecorder(t, rec)
	if rec.reason != recordingStopReplaced {
		t.Fatalf("reason %q", rec.reason)
	}
	if (&Pane{}).Recording() != nil {
		t.Fatal("a new pane starts recorded")
	}
}

func TestRecordingOffAfterEightHours(t *testing.T) {
	tp := newRecTestPane(t)
	recordingMaxOnFor = 60 * time.Millisecond
	rec := tp.start()
	waitRecorder(t, rec)
	d := tp.p.Recording()
	if rec.reason != recordingStopMaxOn || d == nil || d.On || d.StoppedReason != recordingStopMaxOn {
		t.Fatalf("reason %q dto %+v", rec.reason, d)
	}
}

func TestRecordingIsNeverPersisted(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	defer func() { tp.m.StopRecording(tp.p, "test"); waitRecorder(t, rec) }()
	raw, err := json.Marshal(tp.p.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("record")) {
		t.Fatalf("pane snapshot (panes.json) carries recording state: %s", raw)
	}
}

func TestRecordingPolicyStopCutsBackToTheLastPassingCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts recordingPaneFacts
		want  string
	}{
		{"refused folder", recordingPaneFacts{Agent: "claude", Cwd: "/work/project", LiveCwds: []string{"/work/project", "/etc/ssl"}, AgentRunning: true}, recordingStopCwd},
		{"agent exited to a shell", recordingPaneFacts{Agent: "claude", Cwd: "/work/project"}, recordingStopShell},
		{"refused handle", recordingPaneFacts{Handle: "gate-main", Agent: "claude", Cwd: "/work/project", AgentRunning: true}, recordingStopHandle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newRecTestPane(t)
			// A tiny per-file cap makes the unchecked output roll into new
			// files, which the cut must delete whole.
			tp.m.recordingHub().fileCap = 400
			rec := tp.start()
			tp.send(FrameData, []byte("CHECKED-OUTPUT"))
			tp.sync()
			// Two passing checks after it guarantee a checkpoint behind it.
			n := tp.checks.Load()
			for tp.checks.Load() < n+2 {
				time.Sleep(5 * time.Millisecond)
			}
			// Park the checker, so every passing check is behind us, while the
			// unchecked output arrives; then the next check fails.
			hold := make(chan struct{})
			tp.hold.Store(&hold)
			<-tp.blocked
			for i := 0; i < 20; i++ {
				tp.send(FrameData, []byte(strings.Repeat("U", 50)+"UNCHECKED"))
			}
			tp.sync()
			f := tc.facts
			tp.facts.Store(&f)
			close(hold)
			waitRecorder(t, rec)
			if rec.reason != tc.want {
				t.Fatalf("reason %q, want %q", rec.reason, tc.want)
			}
			files := castFiles(t, tp.data, tp.p.ID)
			var all []castEvent
			var onDisk int64
			for _, f := range files {
				_, ev := readCast(t, f)
				all = append(all, ev...)
				if info, err := os.Stat(f); err == nil {
					onDisk += info.Size()
				}
			}
			out := outputOf(all)
			if !strings.Contains(out, "CHECKED-OUTPUT") || strings.Contains(out, "UNCHECKED") {
				t.Fatalf("after the cut the files hold %q", out)
			}
			if m := marks(all); len(m) == 0 || m[len(m)-1] != "stopped: "+tc.want {
				t.Fatalf("marks %v", m)
			}
			hub := tp.m.recordingHub()
			hub.mu.Lock()
			total := hub.total
			hub.mu.Unlock()
			if total != onDisk {
				t.Fatalf("cap account %d, files on disk %d", total, onDisk)
			}
			if d := tp.p.Recording(); d == nil || d.StoppedReason != tc.want {
				t.Fatalf("notice %+v", d)
			}
		})
	}
}

func TestRecordingStopsAtOnceOnARefusedHandle(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	h := "secure"
	tp.p.SetIdentity(&PaneIdentity{Handle: h})
	waitRecorder(t, rec)
	if rec.reason != recordingStopHandle {
		t.Fatalf("reason %q", rec.reason)
	}
}

func TestRecordingSwitchOnRefusedPanes(t *testing.T) {
	home, _ := os.UserHomeDir()
	tp := newRecTestPane(t)
	cases := map[string]recordingPaneFacts{}
	for _, h := range append(append([]string{}, recordingRefusedHandles...), "release-x", "publish-y", "gate-z", "Secure") {
		cases["handle "+h] = recordingPaneFacts{Handle: h, Agent: "claude", Cwd: "/work", AgentRunning: true}
	}
	for _, dir := range recordingRefusedDirs(home, tp.data) {
		cases["cwd "+dir] = recordingPaneFacts{Agent: "codex", Cwd: filepath.Join(dir, "sub"), AgentRunning: true}
		// Another process in the pane in a refused folder, beside an allowed one.
		cases["live cwd "+dir] = recordingPaneFacts{Agent: "codex", Cwd: "/work", LiveCwds: []string{"/work/sub", dir}, AgentRunning: true}
	}
	cases["folder can't be read"] = recordingPaneFacts{Agent: "claude", Cwd: "/work", CwdUnknown: true, AgentRunning: true}
	cases["plain shell"] = recordingPaneFacts{Agent: "", Cwd: "/work", AgentRunning: false}
	cases["other agent"] = recordingPaneFacts{Agent: "muse", Cwd: "/work", AgentRunning: true}
	cases["agent exited"] = recordingPaneFacts{Agent: "claude", Cwd: "/work", AgentRunning: false}
	for name, f := range cases {
		f := f
		tp.facts.Store(&f)
		_, err := tp.m.StartRecording(tp.p, "local")
		var refused recordingRefusedError
		if err == nil || !errors.As(err, &refused) {
			t.Errorf("%s: switch-on allowed (%v)", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(tp.data, recordingsDirName)); !os.IsNotExist(err) {
		t.Fatal("a refused switch-on created the folder")
	}
	ok := allowedRecordingFacts()
	if recordingRefusal(ok, home, tp.data) != "" {
		t.Fatal("an ordinary agent pane is refused")
	}
}
