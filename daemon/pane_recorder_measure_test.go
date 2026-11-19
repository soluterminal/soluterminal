package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// The recorder's three measurements (decision brief A3 and A4), on synthetic
// output only. Run them alone with:
//
//	go test -run 'TestRecorderMeasure' -v .
//
// Each logs its numbers with t.Logf and fails past the brief's thresholds.

// synthAgentChunk is one chunk of agent-TUI-shaped output: synchronized-output
// redraws with SGR colour, cursor moves, streamed text with multi-byte
// characters, and occasional tool-output bursts.
func synthAgentChunk(rng *rand.Rand, i int) []byte {
	switch {
	case i%40 == 39:
		return synthToolBurst(rng)
	case i%3 == 0:
		return synthText(i)
	default:
		return synthRedraw(i)
	}
}

func synthToolBurst(rng *rand.Rand) []byte {
	var b bytes.Buffer
	for j := 0; j < 120; j++ {
		fmt.Fprintf(&b, "\x1b[2m%4d\x1b[0m  src/module_%02d.go: func handler%d() error { return nil } // ok ✓\r\n", j, j%17, rng.Intn(1000))
	}
	return b.Bytes()
}

func synthText(i int) []byte {
	return []byte(fmt.Sprintf("The change keeps the read loop off the disk — résumé of step %d: queue, writer, cap. ", i))
}

func synthRedraw(i int) []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[?2026h\x1b[2K\r\x1b[38;5;174m✻\x1b[39m \x1b[1mWorking…\x1b[22m \x1b[2m(")
	fmt.Fprintf(&b, "%ds · ↓ %d tokens · esc to interrupt", i/10, i*7)
	b.WriteString(")\x1b[22m\x1b[1B\r\x1b[2K\x1b[38;5;246m╭────────────────────────────────────────────╮\x1b[39m\x1b[1A\x1b[?2026l")
	return b.Bytes()
}

// A3, first half: the recorded byte count equals the pane's output byte count
// on a busy pane, through the real read loop and a real file.
func TestRecorderMeasureRecordedBytesEqualPaneOutput(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	rng := rand.New(rand.NewSource(1))
	var sent bytes.Buffer
	before := tp.p.buffer.TotalWritten()
	start := time.Now()
	for i := 0; sent.Len() < 8<<20; i++ {
		chunk := synthAgentChunk(rng, i)
		// Split some chunks mid-character, as a PTY read can.
		if i%7 == 0 && len(chunk) > 8 {
			cut := bytes.IndexRune(chunk, '…')
			if cut > 0 {
				tp.send(FrameData, chunk[:cut+1])
				tp.send(FrameData, chunk[cut+1:])
				sent.Write(chunk)
				continue
			}
		}
		tp.send(FrameData, chunk)
		sent.Write(chunk)
		// A busy agent pane emits a few MB/s at most; pace to 8 MB/s.
		if want := time.Duration(sent.Len()) * time.Second / (8 << 20); time.Since(start) < want {
			time.Sleep(want - time.Since(start))
		}
	}
	tp.sync()
	tp.m.StopRecording(tp.p, "measure")
	waitRecorder(t, rec)
	st := rec.stats()
	paneOutput := tp.p.buffer.TotalWritten() - before
	t.Logf("A3 bytes: pane output %d, recorded %d, dropped %d, replaced %d", paneOutput, st.BytesRecorded, st.BytesDropped, st.BytesReplaced)
	if st.BytesRecorded != paneOutput || st.BytesDropped != 0 || st.BytesReplaced != 0 {
		t.Fatalf("recorded %d of %d (dropped %d, replaced %d)", st.BytesRecorded, paneOutput, st.BytesDropped, st.BytesReplaced)
	}
	var got string
	for _, f := range castFiles(t, tp.data, tp.p.ID) {
		_, events := readCast(t, f)
		got += outputOf(events)
	}
	if got != sent.String() {
		t.Fatalf("decoded recording differs from the pane output (%d vs %d bytes)", len(got), sent.Len())
	}
}

func p95(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)*95/100]
}

// A3, second half: the delay recording adds per chunk is under 5 ms at the
// 95th percentile on a busy pane, with the writer on a real file and with the
// disk blocked.
func TestRecorderMeasureAddedDelayPerChunk(t *testing.T) {
	const chunks = 3000
	run := func(mode string) time.Duration {
		tp := newRecTestPane(t)
		var rec *paneRecorder
		var release chan struct{}
		if mode == "blocked disk" {
			release = make(chan struct{})
			var calls atomic.Int64
			tp.m.recordingHub().write = func(f *os.File, b []byte) (int, error) {
				if calls.Add(1) > 1 {
					<-release
				}
				return f.Write(b)
			}
		}
		if mode != "off" {
			rec = tp.start()
		}
		rng := rand.New(rand.NewSource(2))
		lat := make([]time.Duration, 0, chunks)
		for i := 0; i < chunks; i++ {
			chunk := synthAgentChunk(rng, i)
			t0 := time.Now()
			tp.send(FrameData, chunk)
			// The next header is read only after this chunk is processed.
			tp.send(FrameAgentInfo, []byte(`{}`))
			lat = append(lat, time.Since(t0))
		}
		if release != nil {
			close(release)
		}
		if rec != nil {
			tp.m.StopRecording(tp.p, "measure")
			waitRecorder(t, rec)
		}
		return p95(lat)
	}
	off := run("off")
	on := run("file")
	blocked := run("blocked disk")
	t.Logf("A3 delay per chunk, p95: off %v, recording %v (+%v), blocked disk %v (+%v)", off, on, on-off, blocked, blocked-off)
	for name, d := range map[string]time.Duration{"recording": on - off, "blocked disk": blocked - off} {
		if d >= 5*time.Millisecond {
			t.Errorf("%s adds %v per chunk at p95 (limit 5 ms)", name, d)
		}
	}
}

// A4: under 50 MB per pane-hour on an agent-session shape: 70% of the hour
// working (12 status redraws a second, streamed text, a tool-output burst
// every 20 s), 30% idle (a redraw every 5 s). Time is simulated, so the hour
// takes a second or two.
func TestRecorderMeasureFileSizePerPaneHour(t *testing.T) {
	hub := newRecordingHub(t.TempDir())
	clock := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(clock.UnixNano())
	hub.now = func() time.Time { return time.Unix(0, now.Load()) }
	rec, err := startPaneRecorder(hub, "pane_measure", 120, 40, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(3))
	var raw int64
	emit := func(b []byte) {
		raw += int64(len(b))
		for {
			rec.mu.Lock()
			room := rec.queuedBytes+len(b) <= recordingQueueBytes
			rec.mu.Unlock()
			if room {
				break
			}
			time.Sleep(time.Millisecond) // simulated time: never drop
		}
		rec.output(b)
	}
	const tick = 80 * time.Millisecond // 12.5 redraws a second
	i := 0
	for elapsed := time.Duration(0); elapsed < time.Hour; elapsed += tick {
		now.Add(int64(tick))
		working := (elapsed/time.Minute)%10 < 7
		switch {
		case working:
			emit(synthRedraw(i))
			if i%4 == 0 {
				emit(synthText(i))
			}
			if elapsed%(20*time.Second) < tick {
				emit(synthToolBurst(rng))
			}
		case elapsed%(5*time.Second) < tick:
			emit(synthRedraw(i))
		}
		i++
	}
	rec.stop(recordingStopOff, false)
	waitRecorder(t, rec)
	st := rec.stats()
	mb := float64(st.BytesWritten) / 1e6
	t.Logf("A4 size: %.1f MB per pane-hour on disk (%.1f MB of raw output, %d dropped)", mb, float64(raw)/1e6, st.BytesDropped)
	if st.BytesDropped != 0 || st.BytesRecorded != raw {
		t.Fatalf("recorded %d of %d", st.BytesRecorded, raw)
	}
	if mb >= 50 {
		t.Fatalf("%.1f MB per pane-hour (limit 50)", mb)
	}
}
