package main

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func captureFrames(t *testing.T, p *Pane) <-chan string {
	t.Helper()
	daemonEnd, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = daemonEnd.Close(); _ = agentEnd.Close() })
	p.mu.Lock()
	p.agentConn = daemonEnd
	p.mu.Unlock()
	frames := make(chan string, 8)
	go func() {
		for {
			_, payload, err := ReadFrame(agentEnd)
			if err != nil {
				return
			}
			frames <- string(payload)
		}
	}()
	return frames
}

// ST-267: a schedule or ARIA mention typed into a plain pane never lands in a
// permission menu, even one that opened after the readiness check: the menu is
// read under the writer lock with each write.
func TestUnattendedSubmitNeverAnswersAMenu(t *testing.T) {
	p := choicePromptPane(t, "st267-menu")
	if !promptTakesKeys(p) {
		t.Fatal("fixture pane does not read as a live menu")
	}
	frames := captureFrames(t, p)
	wrote, err := submitLineToPane(p, "1", false)
	if !errors.Is(err, errDeliveryChoicePrompt) || wrote != 0 {
		t.Fatalf("submit at a menu = %d, %v; want nothing written and errDeliveryChoicePrompt", wrote, err)
	}
	select {
	case got := <-frames:
		t.Fatalf("submit at a menu wrote %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestUnattendedSubmitWritesWithoutAMenu(t *testing.T) {
	p := &Pane{ID: "st267-shell", Cols: 84, Rows: 31, Status: PaneRunning, buffer: NewRingBuffer(1 << 20), subs: map[int64]*subEntry{}}
	shell := "user@host:~$ "
	if _, err := p.buffer.Write([]byte(shell)); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&p.TotalStrippedChars, int64(len(shell)))
	if promptTakesKeys(p) {
		t.Fatal("a shell prompt reads as a menu")
	}
	frames := captureFrames(t, p)
	if _, err := submitLineToPane(p, "echo hi", false); err != nil {
		t.Fatalf("submit to a shell: %v", err)
	}
	for _, want := range []string{"\x1b[200~echo hi\x1b[201~", "\r"} {
		select {
		case got := <-frames:
			if got != want {
				t.Fatalf("wrote %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("nothing written, want %q", want)
		}
	}
}

// A menu that opens after the line was typed stops the Enter: the line waits
// in the box (errDeliveryLeftInComposer), so a caller doesn't also answer
// elsewhere.
func TestUnattendedSubmitStopsEnterWhenAMenuOpens(t *testing.T) {
	p := &Pane{ID: "st267-race", Cols: 84, Rows: 31, Status: PaneRunning, buffer: NewRingBuffer(1 << 20), subs: map[int64]*subEntry{}}
	shell := "user@host:~$ "
	if _, err := p.buffer.Write([]byte(shell)); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&p.TotalStrippedChars, int64(len(shell)))
	frames := captureFrames(t, p)
	menu := "\nDo you want to proceed?\n ❯ 1. Yes\n   2. Yes, allow all edits during this session\n   3. No, and tell Claude what to do differently\n"
	opened := make(chan struct{})
	go func() {
		<-frames // the line
		_, _ = p.buffer.Write([]byte(menu))
		atomic.AddInt64(&p.TotalStrippedChars, int64(len(menu)))
		close(opened)
	}()
	_, err := submitLineToPane(p, "echo hi", false)
	<-opened
	if !errors.Is(err, errDeliveryLeftInComposer) {
		t.Fatalf("submit with a menu opening before Enter = %v, want errDeliveryLeftInComposer", err)
	}
	select {
	case got := <-frames:
		t.Fatalf("Enter written into the menu: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}
