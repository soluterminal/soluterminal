package main

import (
	"sync"
	"time"
)

// Tracks every cloud generation's scanners. Sealing registration before Wait
// prevents a reconnect from adding work after shutdown observes an empty group.
type usageScannerGroup struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func (g *usageScannerGroup) start(run func()) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return false
	}
	g.wg.Add(1)
	g.mu.Unlock()
	go func() { defer g.wg.Done(); run() }()
	return true
}

// main owns the single best-effort shutdown budget, including forced cursor
// saves. Filesystem calls cannot be cancelled; on timeout main logs and exits.
func (g *usageScannerGroup) join(timeout time.Duration) bool {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
