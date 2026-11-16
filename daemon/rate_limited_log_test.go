package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func TestRateLimitedLogHoldsRepeatsAndCountsThem(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	l := &rateLimitedLog{every: time.Hour}
	for i := 0; i < 3; i++ {
		l.printf("GET /api/panes", "refused %s", "GET /api/panes")
	}
	l.printf("GET /api/workspaces", "refused %s", "GET /api/workspaces")
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Fatalf("want one line per key, got %d:\n%s", got, buf.String())
	}
	l.seen["GET /api/panes"].last = time.Now().Add(-2 * time.Hour)
	l.printf("GET /api/panes", "refused %s", "GET /api/panes")
	if !strings.Contains(buf.String(), "(2 more like this since the last line)") {
		t.Fatalf("held-back count missing:\n%s", buf.String())
	}
}
