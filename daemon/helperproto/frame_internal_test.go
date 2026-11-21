package helperproto

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// A write still waiting for the writer when its context ends is never sent.
func TestQueuedWriteCanceledIsNeverSent(t *testing.T) {
	r, w := io.Pipe()
	lw := newLineWriter(w)
	first := make(chan error, 1)
	go func() { first <- lw.write(Request{ID: 1, Op: "first"}) }() // blocks: nobody reads yet
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := lw.writeCtx(ctx, Request{ID: 2, Op: "canceled"})
	var notSent errNotSent
	if !errors.As(err, &notSent) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued write: %v", err)
	}
	br := bufio.NewReader(r)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, `"first"`) {
		t.Fatalf("first line %q", line)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	rest, _ := io.ReadAll(br)
	if strings.Contains(string(rest), "canceled") {
		t.Fatal("a canceled write reached the wire")
	}
}
