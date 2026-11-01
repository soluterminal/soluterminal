package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const ownerViewerLeaseDuration = 5 * time.Second

type ownerViewerLease struct {
	token    string
	deadline time.Time
	revision uint64
	viewers  map[int64]*subEntry
	ready    map[int64]bool
	reason   string
	changed  chan struct{}
}

// The gate serializes retirement commit with incoming input and viewer NACKs.
// It never spans provider/filesystem I/O or waiting for acknowledgements.
func (p *Pane) beginOwnerViewerLease(ctx context.Context, token string) (*ownerViewerLease, error) {
	p.ownerRetirementGate.Lock()
	if p.ownerViewerLease != nil || p.ownerRetirementCommitted {
		p.ownerRetirementGate.Unlock()
		return nil, fmt.Errorf("owner retirement already in progress")
	}
	lease := &ownerViewerLease{token: token, deadline: time.Now().Add(ownerViewerLeaseDuration), viewers: map[int64]*subEntry{}, ready: map[int64]bool{}, changed: make(chan struct{}, 1)}
	raw, _ := json.Marshal(map[string]any{"type": "owner-retirement-probe", "token": token, "expiresAtMs": lease.deadline.UnixMilli(), "leaseMs": ownerViewerLeaseDuration.Milliseconds()})
	p.subsMu.Lock()
	lease.revision = p.ownerViewerRevision
	if !p.ownerViewerKnowledgeKnown && p.Role == paneRoleTaskOwner {
		lease.reason = "previous viewer draft state is unknown; inspect the retained owner, which stays open until exact viewer state is known"
	}
	presentDevices := map[string]bool{}
	for id, sub := range p.subs {
		presentDevices[ownerViewerDeviceKey(id, sub)] = true
	}
	for device := range p.ownerViewerPendingDevices {
		if !presentDevices[device] {
			lease.reason = "a disconnected terminal page has not confirmed its draft is empty; this owner remains available for inspection"
		}
	}
	for id, sub := range p.subs {
		lease.viewers[id] = sub
		select {
		case sub.ctrl <- raw:
		default:
			lease.reason = "a terminal viewer cannot acknowledge retirement"
		}
	}
	p.subsMu.Unlock()
	p.ownerViewerLease = lease
	p.ownerRetirementGate.Unlock()
	timer := time.NewTimer(time.Until(lease.deadline))
	defer timer.Stop()
	for {
		p.ownerRetirementGate.Lock()
		err := p.ownerViewersValidLocked(lease, false)
		ready := len(lease.ready) == len(lease.viewers)
		if err != nil {
			p.releaseOwnerViewersLocked(lease)
			p.ownerRetirementGate.Unlock()
			return nil, err
		}
		p.ownerRetirementGate.Unlock()
		if ready {
			return lease, nil
		}
		select {
		case <-ctx.Done():
			p.releaseOwnerViewers(lease)
			return nil, ctx.Err()
		case <-timer.C:
			p.releaseOwnerViewers(lease)
			return nil, fmt.Errorf("terminal viewer did not confirm empty drafts and idle input; keep pane open and reconnect/update that viewer")
		case <-lease.changed:
		}
	}
}

func (p *Pane) ownerViewersValidLocked(lease *ownerViewerLease, requireReady bool) error {
	if p.ownerViewerLease != lease || lease == nil {
		return fmt.Errorf("owner viewer lease changed")
	}
	if lease.reason != "" {
		return fmt.Errorf("%s", lease.reason)
	}
	if !time.Now().Add(150 * time.Millisecond).Before(lease.deadline) {
		return fmt.Errorf("owner viewer acknowledgement expired")
	}
	p.subsMu.RLock()
	unchanged := p.ownerViewerRevision == lease.revision && len(p.subs) == len(lease.viewers)
	if unchanged {
		for id, sub := range lease.viewers {
			if p.subs[id] != sub {
				unchanged = false
				break
			}
		}
	}
	p.subsMu.RUnlock()
	if !unchanged {
		return fmt.Errorf("terminal viewer connections changed during retirement")
	}
	if requireReady && len(lease.ready) != len(lease.viewers) {
		return fmt.Errorf("terminal viewers have not all acknowledged")
	}
	return nil
}

func (p *Pane) acknowledgeOwnerRetirement(subID int64, token string, ready bool, reason string) {
	p.ownerRetirementGate.Lock()
	defer p.ownerRetirementGate.Unlock()
	lease := p.ownerViewerLease
	if lease == nil || token != lease.token || lease.viewers[subID] == nil {
		return
	}
	if ready && lease.reason == "" {
		lease.ready[subID] = true
		p.subsMu.Lock()
		if sub := p.subs[subID]; sub != nil {
			device := ownerViewerDeviceKey(subID, sub)
			allReady := true
			for id, other := range p.subs {
				if ownerViewerDeviceKey(id, other) == device && !lease.ready[id] {
					allReady = false
				}
			}
			if allReady {
				delete(p.ownerViewerPendingDevices, device)
			}
		}
		p.subsMu.Unlock()
	} else {
		// Never persist arbitrary browser text as a workflow recovery instruction.
		lease.reason = "terminal viewer has pending input, a draft, or an unfinished interaction"
		p.releaseOwnerViewersLocked(lease)
	}
	select {
	case lease.changed <- struct{}{}:
	default:
	}
}
func (p *Pane) releaseOwnerViewers(lease *ownerViewerLease) {
	p.ownerRetirementGate.Lock()
	defer p.ownerRetirementGate.Unlock()
	p.releaseOwnerViewersLocked(lease)
}
func (p *Pane) releaseOwnerViewersLocked(lease *ownerViewerLease) {
	if p.ownerViewerLease != lease || lease == nil {
		return
	}
	raw, _ := json.Marshal(map[string]any{"type": "owner-retirement-release", "token": lease.token})
	p.subsMu.Lock()
	for id, sub := range lease.viewers {
		if p.subs[id] == sub {
			p.noteOwnerViewerPendingLocked(id, sub)
			select {
			case sub.ctrl <- raw:
			default:
			}
		}
	}
	p.subsMu.Unlock()
	p.ownerViewerLease = nil
	select {
	case lease.changed <- struct{}{}:
	default:
	}
}
func (p *Pane) ownerRetirementInputBarrier() error {
	if p.workerArchiving.Load() {
		return fmt.Errorf("worker archived; reopen it before sending input")
	}
	p.ownerRetirementGate.Lock()
	defer p.ownerRetirementGate.Unlock()
	if p.ownerRetirementCommitted {
		return fmt.Errorf("owner pane was retired; retained conversation is read-only")
	}
	if lease := p.ownerViewerLease; lease != nil {
		lease.reason = "terminal input arrived during retirement"
		p.releaseOwnerViewersLocked(lease)
	}
	return nil
}

func ownerViewerDeviceKey(id int64, sub *subEntry) string {
	if sub.retirementViewerID != "" {
		return "viewer:" + sub.retirementViewerID
	}
	return fmt.Sprintf("unknown-connection:%d:%d", ringEpochMs, id)
}

func ownerViewerClientID(value string) string {
	if len(value) < 16 || len(value) > 96 {
		return ""
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
	}
	return value
}

// A clean close is advisory until the same connection actually disconnects.
// Any later client message revokes it; sibling tabs cannot attest for it.
func (p *Pane) ownerViewerClosing(subID int64, clean bool) {
	if clean {
		if !p.agentWriteMu.TryLock() {
			return
		}
		pending, _ := p.promptRec.draft()
		clean = pending == 0
		p.agentWriteMu.Unlock()
	}
	p.subsMu.Lock()
	defer p.subsMu.Unlock()
	if sub := p.subs[subID]; sub != nil {
		sub.ownerViewerCleanClosing = clean
	}
}
func (p *Pane) noteOwnerViewerPendingLocked(id int64, sub *subEntry) {
	if p.Role != paneRoleTaskOwner {
		return
	}
	if p.ownerViewerPendingDevices == nil {
		p.ownerViewerPendingDevices = map[string]bool{}
	}
	key := ownerViewerDeviceKey(id, sub)
	if len(p.ownerViewerPendingDevices) >= 64 && !p.ownerViewerPendingDevices[key] {
		p.ownerViewerKnowledgeKnown = false
		return
	}
	p.ownerViewerPendingDevices[key] = true
}
