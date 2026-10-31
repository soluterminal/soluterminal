package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

var errAutoModePolicyChanged = errors.New("auto-approval policy changed before write")

// An in-memory decision lease, not a persisted setting or an authority grant.
type paneAutoModePolicy struct {
	mode       string
	generation uint64
}

func (p *Pane) autoModePolicy() paneAutoModePolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return paneAutoModePolicy{mode: p.AutoMode, generation: p.autoModeGeneration}
}

func (policy paneAutoModePolicy) matchesLocked(p *Pane) bool {
	return p.AutoMode == policy.mode && p.autoModeGeneration == policy.generation
}

func (policy paneAutoModePolicy) matches(p *Pane) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.NeverAutoAnswer && policy.matchesLocked(p)
}

// Count actual transport bytes so a rejected frame or an expired deadline
// before the first byte does not disconnect an otherwise healthy pane.
type autoModeFrameWriter struct {
	io.Writer
	written int
}

func (w *autoModeFrameWriter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	w.written += n
	return n, err
}

// writeAnswerFrameLocked is a guarded transport used by programmatic input
// and answer writers. Decision-point guards also refuse locked-pane actions. The caller holds agentWriteMu and
// either autoModeMu (ordinary approvals) or p.mu (Jev's screen transaction)
// through the write, so a human lock update cannot pass this check mid-frame.
// stateLocked means the caller already holds p.mu; ordinary writes release
// that snapshot lock during bounded network I/O, retaining autoModeMu.
// Human input and native protocol housekeeping use the ordinary input writer.
func (p *Pane) writeAnswerFrameLocked(conn net.Conn, payload []byte, policy *paneAutoModePolicy, deadline time.Time, stateLocked bool) error {
	if !stateLocked {
		p.mu.Lock()
	}
	locked := p.NeverAutoAnswer
	valid := (policy == nil || policy.matchesLocked(p)) && p.agentConn == conn
	if !stateLocked {
		p.mu.Unlock()
	}
	if locked {
		return fmt.Errorf("%w: pane is locked against automatic answers", errAutoModePolicyChanged)
	}
	if !valid {
		return errAutoModePolicyChanged
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	frame := autoModeFrameWriter{Writer: conn}
	err := WriteFrame(&frame, FrameData, payload)
	if err != nil && frame.written > 0 {
		_ = conn.Close()
		if !stateLocked {
			p.mu.Lock()
		}
		if p.agentConn == conn {
			p.agentConn = nil
		}
		if !stateLocked {
			p.mu.Unlock()
		}
	}
	return err
}

// writeProgrammaticInput is for raw input from a non-human transport. Locked
// panes refuse every raw byte, without relying on menu detection or key shape.
// Verified message delivery has its own composer/choice-prompt checks.
func (p *Pane) writeProgrammaticInput(b []byte) (int, error) {
	return p.writeWithAutoModePolicy(b, true, p.autoModePolicy(), time.Now().Add(ariaJevTimeout))
}
