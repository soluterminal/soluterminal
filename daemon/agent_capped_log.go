package main

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// agentLogCap bounds an agent's own log output under --capped-log, the
// helper's agents/<agentId>.log (helper-protocol-v1, Agent output).
const agentLogCap = 64 << 10

// cappedLogNoteRoom is kept back from the cap for the final dropped count.
const cappedLogNoteRoom = 128

// cappedLogWriter passes whole writes through until the cap, then drops and
// counts them. A write is one log record, so records are never cut.
type cappedLogWriter struct {
	mu      sync.Mutex
	w       io.Writer
	left    int
	dropped int64
	closed  bool
}

func newCappedLogWriter(w io.Writer, capBytes int) *cappedLogWriter {
	return &cappedLogWriter{w: w, left: capBytes - cappedLogNoteRoom}
}

func (c *cappedLogWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(p) > c.left {
		c.dropped += int64(len(p))
		c.left = 0 // later, shorter records are dropped too, so order holds
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.left -= n
	return len(p), err
}

// Close writes the dropped count, if any, into the room kept for it.
func (c *cappedLogWriter) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.dropped == 0 {
		return nil
	}
	note := fmt.Sprintf("log capped at %d bytes: %d bytes dropped\n", agentLogCap, c.dropped)
	if len(note) > cappedLogNoteRoom {
		note = note[:cappedLogNoteRoom]
	}
	_, err := io.WriteString(c.w, note)
	return err
}

// catchHangup keeps a WSL helper's agent alive through a SIGHUP (its
// wsl.exe session ending, Check 1) by catching it rather than ignoring it: a
// caught signal goes back to the default action across exec, so the pane's
// own processes still hang up normally when their terminal closes.
func catchHangup() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for range ch {
			slog.Info("agent: ignored SIGHUP")
		}
	}()
}

// installCappedAgentLog routes the agent's slog and log output through the
// cap. fd 2 itself stays where it is, for the Go runtime's crash report.
func installCappedAgentLog(stderr io.Writer) *cappedLogWriter {
	c := newCappedLogWriter(stderr, agentLogCap)
	slog.SetDefault(slog.New(buildLogHandler(c)))
	log.SetFlags(0)
	log.SetOutput(slogStdlibBridge{})
	return c
}
