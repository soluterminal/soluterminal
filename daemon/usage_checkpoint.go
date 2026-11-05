package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"
)

const usageCheckpointInterval = 10 * time.Minute
const usageShutdownTimeout = 5 * time.Second

// Protected by the owning cursor store's mutex, including the entire write.
// pending skips are never exposed as committed offsets before durable save.
type usageCheckpoint[T comparable] struct {
	dirty    bool
	lastSave time.Time
	revision uint64
	pending  map[string]T
	write    func(string, []byte, os.FileMode) error
	now      func() time.Time
}

func (c *usageCheckpoint[T]) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *usageCheckpoint[T]) set(m map[string]T, path string, value T) {
	if old, ok := m[path]; !ok || old != value {
		m[path] = value
		c.dirty = true
		c.revision++
	}
}

func (c *usageCheckpoint[T]) skip(m map[string]T, updates map[string]T) {
	for path, value := range updates {
		if old, ok := m[path]; !ok || old != value {
			if c.pending == nil {
				c.pending = make(map[string]T)
			}
			c.pending[path] = value
		}
	}
}

func (c *usageCheckpoint[T]) save(path string, m map[string]T, force bool) error {
	if !c.dirty && len(c.pending) == 0 {
		return nil
	}
	if !force && len(c.pending) == 0 && c.clock().Sub(c.lastSave) < usageCheckpointInterval {
		return nil
	}
	snapshot := m
	if len(c.pending) > 0 {
		snapshot = make(map[string]T, len(m)+len(c.pending))
		for k, v := range m {
			snapshot[k] = v
		}
		for k, v := range c.pending {
			snapshot[k] = v
		}
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	write := c.write
	if write == nil {
		write = writeFileAtomicSynced
	}
	if err := write(path, b, 0o600); err != nil {
		return err
	}
	for k, v := range c.pending {
		c.set(m, k, v)
	}
	c.pending = nil
	c.dirty = false
	c.lastSave = c.clock()
	return nil
}

func (c *usageCheckpoint[T]) nextDelay() time.Duration {
	if !c.dirty && len(c.pending) == 0 {
		return usageCheckpointInterval
	}
	remaining := c.lastSave.Add(usageCheckpointInterval).Sub(c.clock())
	if remaining <= 0 {
		return ccUsageScanInterval
	} // bounded retry after write errors
	return remaining
}

// A separate timer checkpoints even during a long scan; store mutexes serialize
// it with chunk commits. The scanner joins its forced save; main bounds the
// entire scanner-group wait, including uninterruptible filesystem work.
func startUsageCheckpoints(save func(bool) error, nextDelay func() time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTimer(nextDelay())
		defer tick.Stop()
		for {
			select {
			case <-stop:
				if save(true) != nil {
					log.Print("usage: shutdown checkpoint failed")
				}
				return
			case <-tick.C:
				if save(false) != nil {
					log.Print("usage: checkpoint failed; retained for retry")
				}
				tick.Reset(nextDelay())
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// Recheck before each HTTP request, including the reporter's 500-row splits.
// Turning OFF does not wait for a running scan before stopping new requests.
type usageUploadTransport struct{ allowed func() bool }

func (t usageUploadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.allowed() {
		return nil, errors.New("usage upload is disabled")
	}
	return http.DefaultTransport.RoundTrip(r)
}
