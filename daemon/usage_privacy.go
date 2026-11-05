package main

import (
	"context"
	"errors"
	"log"
	"time"
)

type usagePrivacyParticipant struct {
	flush func() error
	skip  func(context.Context) error
}

// The toggle barrier and scans share a lock. Checkpoint timers use only the
// cursor lock, so they continue during a long network-bound scan.
func (p *privacySettings) registerUsage(flush func() error, skip func(context.Context) error) func() {
	if p == nil {
		return func() {}
	}
	p.usageMu.Lock()
	entry := &usagePrivacyParticipant{flush: flush, skip: skip}
	if p.usageParticipants == nil {
		p.usageParticipants = make(map[*usagePrivacyParticipant]struct{})
	}
	p.usageParticipants[entry] = struct{}{}
	p.usageMu.Unlock()
	return func() { p.usageMu.Lock(); delete(p.usageParticipants, entry); p.usageMu.Unlock() }
}

func (p *privacySettings) runUsage(run func()) {
	if p == nil {
		run()
		return
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	run()
}

// Drain the existing bounded scanner while OFF before permitting the first
// enabled read. Progress is durable per chunk; retries can continue a drain
// that reaches the toggle deadline. No unbounded read buffer is introduced.
func drainUsageSkips(ctx context.Context, revision func() uint64, scan func(), flush func() error, complete ...func() bool) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("usage skip drain failed")
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := revision()
		scan()
		if err := flush(); err != nil {
			return errors.New("usage skip checkpoint failed")
		}
		if revision() == before {
			if len(complete) > 0 && !complete[0]() {
				return errors.New("usage skip boundary not yet durable")
			}
			return nil
		}
	}
}

// Include waiting for an in-flight scan in the toggle's deadline.
func (p *privacySettings) lockUsage(ctx context.Context) bool {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if p.usageMu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

func (p *privacySettings) updateUsage(usage, crash *bool) error {
	p.toggleMu.Lock()
	defer p.toggleMu.Unlock()
	wasAllowed := p.usageUploadAllowed()
	off := usage != nil && !*usage
	if off {
		// Stop requests now, and persist OFF independently of cursor availability.
		p.mu.Lock()
		v := false
		p.record.UsageUpload = &v
		p.mu.Unlock()
		if err := p.persistUpdate(usage, crash); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !p.lockUsage(ctx) {
		if off {
			log.Print("usage: disabled; cursor flush barrier timed out")
			return nil
		}
		return errors.New("usage checkpoint barrier timed out")
	}
	defer p.usageMu.Unlock()
	for entry := range p.usageParticipants {
		if usage != nil && *usage && !wasAllowed {
			if err := entry.skip(ctx); err != nil {
				return err
			}
		}
		if err := entry.flush(); err != nil {
			if off {
				log.Print("usage: disabled; cursor flush retained for retry")
				continue
			}
			return errors.New("usage checkpoint failed")
		}
	}
	if off {
		return nil
	}
	return p.persistUpdate(usage, crash)
}
