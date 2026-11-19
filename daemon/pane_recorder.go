package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// paneRecorder writes one pane's output to asciicast v2 files under
// recordings/<pane id>/. The pane's read loop hands it FrameData payloads only
// (never replay, synthetic paint or anything the daemon draws itself) through
// output, which copies into a byte-bounded queue and returns: the read loop
// never waits on the disk. A separate writer goroutine encodes and appends.
// Typed input never reaches it.
type paneRecorder struct {
	hub       *recordingHub
	paneID    string
	dir       string
	startedAt time.Time
	now       func() time.Time
	// onFinish runs once, on the writer goroutine, after the last file closes.
	onFinish func(r *paneRecorder, reason string)
	// verify re-checks the (f) rules when a recording ends for any other
	// reason than an (f) stop, so output since the last passing check is
	// never kept unchecked; it returns the (f) reason, or "".
	verify func() string

	mu          sync.Mutex
	queue       []recItem
	queuedBytes int
	stopping    bool
	stopReason  string
	stopCut     bool // an (f) stop: cut back to the last passing check
	wake        chan struct{}
	done        chan struct{}

	outputBytes   atomic.Int64 // FrameData bytes in the files
	droppedBytes  atomic.Int64 // FrameData bytes dropped on a full queue
	replacedBytes atomic.Int64 // bytes that aren't UTF-8, stored as U+FFFD
	fileBytes     atomic.Int64 // bytes of every file this recording wrote

	// Writer-owned from here on.
	cols, rows uint16
	file       *os.File
	path       string
	fileStart  time.Time
	fileSize   int64
	carry      []byte
	buf        bytes.Buffer
	check      recCheckpoint
	// since lists the files started after the last checkpoint, which an (f)
	// stop deletes whole.
	since  []string
	reason string
}

type recItemKind uint8

const (
	recItemOutput recItemKind = iota
	recItemResize
	recItemGap
	recItemCheckpoint
)

type recItem struct {
	kind       recItemKind
	at         time.Time
	data       []byte
	n          int64
	cols, rows uint16
}

// recCheckpoint is the file and offset at the last passing (f) check.
type recCheckpoint struct {
	path        string
	offset      int64
	fileStart   time.Time
	outputBytes int64
	fileBytes   int64
}

// errRecordingCap stops a recording that can't stay under the total cap.
var errRecordingCap = errors.New(recordingStopDiskCap)

// startPaneRecorder creates the folders and the first file and writes its
// header synchronously, so the person's switch-on gets the refusal; then it
// starts the writer.
func startPaneRecorder(hub *recordingHub, paneID string, cols, rows uint16, verify func() string, onFinish func(*paneRecorder, string)) (*paneRecorder, error) {
	dir, err := hub.prepare(paneID)
	if err != nil {
		return nil, err
	}
	r := &paneRecorder{
		hub:      hub,
		paneID:   paneID,
		dir:      dir,
		now:      hub.now,
		onFinish: onFinish,
		verify:   verify,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		cols:     cols,
		rows:     rows,
	}
	r.startedAt = r.now()
	if err := r.openFile(r.startedAt); err != nil {
		return nil, err
	}
	r.check = r.checkpointHere()
	go r.run()
	return r, nil
}

// output queues one FrameData payload. It is called from the read loop under
// the pane's subsMu, so it only copies and appends.
func (r *paneRecorder) output(b []byte) {
	if len(b) == 0 {
		return
	}
	at := r.now()
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	if r.queuedBytes+len(b) > recordingQueueBytes {
		r.droppedBytes.Add(int64(len(b)))
		if n := len(r.queue); n > 0 && r.queue[n-1].kind == recItemGap {
			r.queue[n-1].n += int64(len(b))
		} else {
			r.queue = append(r.queue, recItem{kind: recItemGap, at: at, n: int64(len(b))})
		}
		r.mu.Unlock()
		r.signal()
		return
	}
	data := make([]byte, len(b))
	copy(data, b)
	r.queue = append(r.queue, recItem{kind: recItemOutput, at: at, data: data})
	r.queuedBytes += len(b)
	r.mu.Unlock()
	r.signal()
}

// resize queues an "r" event. Resizes are tiny and never dropped; a run of
// them waiting on the disk folds into the last.
func (r *paneRecorder) resize(cols, rows uint16) {
	at := r.now()
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	if n := len(r.queue); n > 0 && r.queue[n-1].kind == recItemResize {
		r.queue[n-1] = recItem{kind: recItemResize, at: at, cols: cols, rows: rows}
	} else {
		r.queue = append(r.queue, recItem{kind: recItemResize, at: at, cols: cols, rows: rows})
	}
	r.mu.Unlock()
	r.signal()
}

// checkpoint marks that the (f) check passed now: everything queued before it
// survives an (f) stop.
func (r *paneRecorder) checkpoint() {
	r.mu.Lock()
	if !r.stopping {
		r.queue = append(r.queue, recItem{kind: recItemCheckpoint, at: r.now()})
	}
	r.mu.Unlock()
	r.signal()
}

// stop asks the writer to finish. cut is for an (f) stop: what was recorded
// after the last passing check is removed. It does not wait for the disk.
func (r *paneRecorder) stop(reason string, cut bool) {
	r.mu.Lock()
	if !r.stopping {
		r.stopping = true
		r.stopReason = reason
		r.stopCut = cut
	}
	r.mu.Unlock()
	r.signal()
}

func (r *paneRecorder) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// wait blocks until the writer has closed its last file, or d passes.
func (r *paneRecorder) wait(d time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (r *paneRecorder) run() {
	reason := r.loop()
	if r.file != nil {
		_ = r.file.Close()
		r.hub.closed(r.path)
		r.file = nil
	}
	r.reason = reason
	// The notice, log line and event are in place before anyone waiting on
	// done (the person's delete, tests) goes on.
	if r.onFinish != nil {
		r.onFinish(r, reason)
	}
	close(r.done)
}

func (r *paneRecorder) loop() string {
	for range r.wake {
		r.mu.Lock()
		items := r.queue
		r.queue = nil
		r.queuedBytes = 0
		stopping, reason, cut := r.stopping, r.stopReason, r.stopCut
		r.mu.Unlock()
		if stopping && cut {
			// Nothing queued after the last passing check is written.
			return r.cutAndClose(reason)
		}
		if err := r.writeItems(items); err != nil {
			reason := recordingStopWriteError
			if errors.Is(err, errRecordingCap) {
				reason = recordingStopDiskCap
			}
			return r.endUnchecked(reason)
		}
		if stopping {
			if len(r.carry) > 0 {
				if err := r.writeOutput(r.now(), nil, true); err == nil {
					_ = r.flush()
				}
			}
			return r.endUnchecked(reason)
		}
	}
	return recordingStopWriteError
}

// endUnchecked finishes a recording that ends for another reason than an
// (f) stop: an (f) stop requested meanwhile, or an (f) rule that holds now,
// still cuts back to the last passing check.
func (r *paneRecorder) endUnchecked(reason string) string {
	r.mu.Lock()
	cut, cutReason := r.stopping && r.stopCut, r.stopReason
	r.mu.Unlock()
	if !cut {
		switch reason {
		case recordingStopDeleted:
			// Every file is about to go.
		case recordingStopClosed, recordingStopReplaced:
			// The pane's processes are gone, so no final check can pass:
			// keep only what the last passing check covered.
			cut, cutReason = true, reason
		default:
			if r.verify != nil {
				if f := r.verify(); f != "" {
					cut, cutReason = true, f
				}
			}
		}
	}
	if cut {
		return r.cutAndClose(cutReason)
	}
	r.closingMark(reason)
	return reason
}

func (r *paneRecorder) cutAndClose(reason string) string {
	if err := r.cutToCheckpoint(); err != nil {
		return recordingStopWriteError
	}
	r.closingMark(reason)
	return reason
}

// closingMark writes the final "m" event when the cap allows it.
func (r *paneRecorder) closingMark(reason string) {
	if r.writeLine(r.markLine(r.now(), "stopped: "+reason)) == nil {
		_ = r.flush()
	}
}

func (r *paneRecorder) writeItems(items []recItem) error {
	for _, it := range items {
		var err error
		switch it.kind {
		case recItemOutput:
			err = r.writeOutput(it.at, it.data, false)
		case recItemResize:
			r.cols, r.rows = it.cols, it.rows
			err = r.writeLine(r.eventLine(it.at, "r", strconv.Itoa(int(it.cols))+"x"+strconv.Itoa(int(it.rows))))
		case recItemGap:
			// A split character before the gap can't be completed after it.
			if len(r.carry) > 0 {
				err = r.writeOutput(it.at, nil, true)
			}
			if err == nil {
				err = r.writeLine(r.markLine(it.at, "dropped "+strconv.FormatInt(it.n, 10)+" bytes"))
			}
		case recItemCheckpoint:
			if err = r.flush(); err == nil {
				r.check = r.checkpointHere()
				r.since = nil
			}
		}
		if err != nil {
			return err
		}
	}
	return r.flush()
}

// writeOutput encodes one "o" event. A multi-byte character split across
// payloads waits in carry for the rest; final flushes carry as it is.
func (r *paneRecorder) writeOutput(at time.Time, data []byte, final bool) error {
	raw := data
	if len(r.carry) > 0 {
		raw = append(r.carry, data...)
		r.carry = nil
	}
	var tail []byte
	if !final {
		raw, tail = splitUTF8Tail(raw)
	}
	if len(tail) > 0 {
		r.carry = append([]byte(nil), tail...)
	}
	if len(raw) == 0 {
		return nil
	}
	if !utf8.Valid(raw) {
		r.replacedBytes.Add(int64(countInvalidUTF8(raw)))
	}
	if err := r.writeLine(r.eventLine(at, "o", string(raw))); err != nil {
		return err
	}
	r.outputBytes.Add(int64(len(raw)))
	return nil
}

// writeLine appends one line, rolling to a new file at the per-file cap and
// reserving its bytes under the total cap first.
func (r *paneRecorder) writeLine(line []byte) error {
	if r.file == nil {
		return errors.New("no open recording file")
	}
	if r.fileSize+int64(r.buf.Len())+int64(len(line)) > r.hub.fileCap && r.fileSize+int64(r.buf.Len()) > 0 {
		if err := r.roll(); err != nil {
			return err
		}
	}
	if !r.hub.reserve(int64(len(line))) {
		_ = r.flush()
		return errRecordingCap
	}
	r.buf.Write(line)
	if r.buf.Len() >= 64<<10 {
		return r.flush()
	}
	return nil
}

func (r *paneRecorder) flush() error {
	if r.buf.Len() == 0 || r.file == nil {
		return nil
	}
	b := r.buf.Bytes()
	n, err := r.hub.write(r.file, b)
	r.fileSize += int64(n)
	r.fileBytes.Add(int64(n))
	r.hub.written(int64(n))
	if n < len(b) {
		r.hub.unreserve(int64(len(b) - n))
	}
	r.buf.Reset()
	if err == nil && n < len(b) {
		err = errors.New("short write")
	}
	return err
}

// roll closes the current file and starts the next, with a fresh header at
// the current size.
func (r *paneRecorder) roll() error {
	if err := r.flush(); err != nil {
		return err
	}
	_ = r.file.Close()
	r.hub.closed(r.path)
	r.file = nil
	if err := r.openFile(r.now()); err != nil {
		return err
	}
	r.since = append(r.since, r.path)
	return nil
}

// openFile creates a file and writes its header: version, width, height and
// timestamp only. No env, command, title or folder.
func (r *paneRecorder) openFile(start time.Time) error {
	f, path, err := r.hub.createFile(r.dir, start)
	if err != nil {
		return err
	}
	header := []byte(fmt.Sprintf(`{"version":2,"width":%d,"height":%d,"timestamp":%d}`+"\n", r.cols, r.rows, start.Unix()))
	if !r.hub.reserve(int64(len(header))) {
		_ = f.Close()
		_ = os.Remove(path)
		r.hub.closed(path)
		return errRecordingCap
	}
	n, err := r.hub.write(f, header)
	r.hub.written(int64(n))
	if err != nil || n != len(header) {
		// The partial header goes with the file.
		r.hub.unreserve(int64(len(header) - n))
		r.hub.release(int64(n))
		_ = f.Close()
		_ = os.Remove(path)
		r.hub.closed(path)
		if err == nil {
			err = errors.New("short write")
		}
		return err
	}
	r.file, r.path, r.fileStart, r.fileSize = f, path, start, int64(n)
	r.fileBytes.Add(int64(n))
	return nil
}

func (r *paneRecorder) checkpointHere() recCheckpoint {
	return recCheckpoint{path: r.path, offset: r.fileSize, fileStart: r.fileStart, outputBytes: r.outputBytes.Load(), fileBytes: r.fileBytes.Load()}
}

// cutToCheckpoint removes everything written after the last passing check:
// files started since are deleted, and the checkpoint's file is truncated to
// its offset and reopened for the closing mark.
func (r *paneRecorder) cutToCheckpoint() error {
	r.hub.unreserve(int64(r.buf.Len())) // reserved but unwritten
	r.buf.Reset()
	r.carry = nil
	if r.file != nil {
		_ = r.file.Close()
		r.hub.closed(r.path)
		r.file = nil
	}
	for _, path := range r.since {
		r.hub.removeAccounted(path)
	}
	r.since = nil
	cp := r.check
	f, err := r.hub.truncateAccounted(cp.path, cp.offset)
	if err != nil {
		return err
	}
	r.file, r.path, r.fileStart, r.fileSize = f, cp.path, cp.fileStart, cp.offset
	r.outputBytes.Store(cp.outputBytes)
	r.fileBytes.Store(cp.fileBytes)
	return nil
}

func (r *paneRecorder) eventLine(at time.Time, code, data string) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	b.WriteString(r.offsetSeconds(at))
	b.WriteString(`, "`)
	b.WriteString(code)
	b.WriteString(`", `)
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(data) // replaces bytes that aren't UTF-8 with U+FFFD
	b.Truncate(b.Len() - 1)
	b.WriteString("]\n")
	return b.Bytes()
}

func (r *paneRecorder) markLine(at time.Time, label string) []byte {
	return r.eventLine(at, "m", label)
}

func (r *paneRecorder) offsetSeconds(at time.Time) string {
	d := at.Sub(r.fileStart)
	if d < 0 {
		d = 0
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 6, 64)
}

// splitUTF8Tail holds back a multi-byte character cut off at the end of b.
func splitUTF8Tail(b []byte) (complete, tail []byte) {
	for i := 1; i <= 3 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c < 0x80 {
			break
		}
		if c >= 0xC0 {
			need := 0
			switch {
			case c < 0xE0:
				need = 2
			case c < 0xF0:
				need = 3
			case c < 0xF8:
				need = 4
			}
			if need > i {
				return b[:len(b)-i], b[len(b)-i:]
			}
			break
		}
	}
	return b, nil
}

func countInvalidUTF8(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			n++
		}
		b = b[size:]
	}
	return n
}

// recordingStats is the status a recording reports; it never holds content.
type recordingStats struct {
	StartedAt     int64 `json:"startedAt,omitempty"`
	BytesRecorded int64 `json:"bytesRecorded"`
	BytesWritten  int64 `json:"bytesWritten"`
	BytesDropped  int64 `json:"bytesDropped"`
	BytesReplaced int64 `json:"bytesReplaced"`
}

func (r *paneRecorder) stats() recordingStats {
	return recordingStats{
		StartedAt:     r.startedAt.UnixMilli(),
		BytesRecorded: r.outputBytes.Load(),
		BytesWritten:  r.fileBytes.Load(),
		BytesDropped:  r.droppedBytes.Load(),
		BytesReplaced: r.replacedBytes.Load(),
	}
}
