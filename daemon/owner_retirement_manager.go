package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type ownerRetirementHooks struct {
	// Retain durably preserves exact history and recovery recipe, raises the
	// reader floor, records closing intent and removes legacy automatic restore.
	// A timeout must never be interpreted as permission to close.
	Retain func(context.Context, ownerRetirementProof, PaneSnapshot) error
	// CommitGuard rechecks and locks the exact task/link/admission state. Its
	// unlock runs immediately after the commit frame is written, never after I/O.
	CommitGuard func(PaneSnapshot) (unlock func(), err error)
}
type ownerRetirementResult struct {
	Proof           ownerRetirementProof
	CommitSent      bool
	CommitConfirmed bool
	Exited          bool
}

func ownerRetirementConversation(s PaneSnapshot) string {
	if s.Agent == agentCodex {
		return s.CodexThreadID
	}
	return s.ClaudeSessionID
}
func ownerRetirementPaneGuard(s PaneSnapshot, conversation string) error {
	if s.Status != PaneRunning {
		return fmt.Errorf("owner exited before exact provider and child-chain proof was retained; inspect or explicitly reopen this owner, never infer closure from socket EOF")
	}
	if s.Role != paneRoleTaskOwner || s.Kind != "" || s.Pid <= 0 || s.Status != PaneRunning || s.Flagged || ownerRetirementConversation(s) != conversation || conversation == "" {
		return fmt.Errorf("exact unflagged live owner/conversation is no longer eligible")
	}
	return nil
}

func (m *Manager) retireOwnerPane(ctx context.Context, paneID, conversation string, hooks ownerRetirementHooks) (result ownerRetirementResult, err error) {
	if hooks.Retain == nil || hooks.CommitGuard == nil {
		return result, fmt.Errorf("retirement requires durable history and final task guard")
	}
	p, ok := m.Get(paneID)
	if !ok {
		return result, fmt.Errorf("exact owner pane is missing")
	}
	if !p.tryAcquireRespawn() {
		return result, fmt.Errorf("owner process is restarting or changing configuration")
	}
	defer p.respawning.Store(false)
	snapshot := p.Snapshot()
	if err := ownerRetirementPaneGuard(snapshot, conversation); err != nil {
		return result, err
	}
	p.mu.Lock()
	supported := p.ownerRetirementSupported
	conn := p.agentConn
	p.mu.Unlock()
	if !supported || conn == nil {
		return result, fmt.Errorf("owner agent lacks conditional retirement support; keep this pane and update/reopen it explicitly")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, err
	}
	token := hex.EncodeToString(nonce[:])
	lease, err := p.beginOwnerViewerLease(ctx, token)
	if err != nil {
		return result, err
	}
	defer p.releaseOwnerViewers(lease)
	if !p.agentWriteMu.TryLock() {
		return result, fmt.Errorf("owner terminal input or geometry is being delivered")
	}
	writeLocked := true
	defer func() {
		if writeLocked {
			p.agentWriteMu.Unlock()
		}
	}()
	if pending, _ := p.promptRec.draft(); pending != 0 {
		return result, fmt.Errorf("owner retains unsent terminal input")
	}
	if p.codexLiveMigrating.Load() {
		return result, fmt.Errorf("owner connection is being upgraded")
	}
	p.mu.Lock()
	if p.agentConn != conn || p.Pid != snapshot.Pid || p.ownerRetirementReplies != nil {
		p.mu.Unlock()
		return result, fmt.Errorf("owner process connection changed")
	}
	replies := make(chan ownerRetirementReply, 4)
	p.ownerRetirementReplies = replies
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.ownerRetirementReplies == replies {
			p.ownerRetirementReplies = nil
		}
		p.mu.Unlock()
	}()
	request := ownerRetirementRequest{Token: token, PaneID: paneID, AgentPID: snapshot.Pid, ConversationID: conversation, ClaudeTranscriptPath: snapshot.ClaudeTranscriptPath}
	deadline := time.Now().Add(ownerRetirementLeaseDuration)
	if limit := lease.deadline.Add(-200 * time.Millisecond); deadline.After(limit) {
		deadline = limit
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := writeOwnerRetirementFrame(conn, FrameOwnerRetirePrepare, request, deadline); err != nil {
		return result, err
	}
	p.agentWriteMu.Unlock()
	writeLocked = false
	// No daemon input lock spans provider proof or durable history I/O. New
	// input cancels the agent lease immediately; a stalled disk never strands
	// a user behind this housekeeping transaction.
	defer func() {
		if !result.CommitConfirmed {
			acquired := false
			if !writeLocked {
				if !p.agentWriteMu.TryLock() {
					return
				}
				acquired = true
			}
			_ = writeOwnerRetirementFrame(conn, FrameOwnerRetireAbort, request, time.Now().Add(200*time.Millisecond))
			if acquired {
				p.agentWriteMu.Unlock()
			}
		}
	}()
	reply, err := waitOwnerRetirementReply(bounded, p, replies, token)
	if err != nil {
		return result, err
	}
	if !reply.Ready || reply.Proof == nil {
		return result, fmt.Errorf("owner idle proof refused: %s", reply.Error)
	}
	proof := *reply.Proof
	if proof.AgentPID != snapshot.Pid || proof.ConversationID != conversation || proof.ProviderPID <= 0 {
		return result, fmt.Errorf("owner proof identity mismatch")
	}
	result.Proof = proof
	exited, closeFence, err := retainOwnerExitFence(proof)
	if err != nil {
		return result, err
	}
	defer closeFence()
	if err := hooks.Retain(bounded, proof, snapshot); err != nil {
		return result, err
	}
	if err := bounded.Err(); err != nil {
		return result, err
	}
	current, ok := m.Get(paneID)
	if !ok || current != p {
		return result, fmt.Errorf("owner pane object changed")
	}
	currentSnapshot := p.Snapshot()
	if currentSnapshot.Pid != snapshot.Pid {
		return result, fmt.Errorf("owner process changed")
	}
	if err := ownerRetirementPaneGuard(currentSnapshot, conversation); err != nil {
		return result, err
	}
	if !p.agentWriteMu.TryLock() {
		return result, fmt.Errorf("terminal input or geometry changed before commit")
	}
	writeLocked = true
	if pending, _ := p.promptRec.draft(); pending != 0 {
		return result, fmt.Errorf("owner draft appeared before commit")
	}
	unlock, err := hooks.CommitGuard(currentSnapshot)
	if err != nil {
		return result, err
	}
	if unlock == nil {
		return result, fmt.Errorf("owner task guard did not establish its commit fence")
	}
	p.ownerRetirementGate.Lock()
	err = p.ownerViewersValidLocked(lease, true)
	if err == nil && bounded.Err() != nil {
		err = bounded.Err()
	}
	if err == nil {
		// Hold subscription membership and mutable pane identity through the
		// frame write. Try-locks avoid extending a stopped lease for another
		// operation's lock; a retry is cheaper than uncertain authority.
		if !p.subsMu.TryRLock() {
			err = fmt.Errorf("terminal viewers are changing")
		} else {
			if p.ownerViewerRevision != lease.revision || len(p.subs) != len(lease.viewers) {
				err = fmt.Errorf("terminal viewers changed before commit")
			} else if !p.mu.TryLock() {
				err = fmt.Errorf("owner state is changing")
			} else {
				liveConversation := p.ClaudeSessionID
				if p.Agent == agentCodex {
					liveConversation = p.CodexThreadID
				}
				if p.Flagged || p.Pid != snapshot.Pid || p.agentConn != conn || p.Role != paneRoleTaskOwner || p.Kind != "" || p.Status != PaneRunning || liveConversation != conversation {
					err = fmt.Errorf("exact unflagged owner changed before commit")
				} else {
					// Earlier input/NACK cancelled the lease. Later input
					// receives an explicit retired-pane error, no new target.
					p.ownerRetirementCommitted = true
					p.userClosing = true
					err = writeOwnerRetirementFrame(conn, FrameOwnerRetireCommit, request, deadline)
					result.CommitSent = err == nil
				}
				p.mu.Unlock()
			}
			p.subsMu.RUnlock()
		}
	}
	p.ownerRetirementGate.Unlock()
	unlock()
	p.agentWriteMu.Unlock()
	writeLocked = false
	if err != nil {
		p.resetOwnerRetirementCommit()
		return result, err
	}
	reply, err = waitOwnerRetirementReply(bounded, p, replies, token)
	result.CommitConfirmed = err == nil && reply.Committed
	// A socket EOF is not exit evidence. Generation-bound process handles must
	// establish the whole proved child chain has actually gone before removal.
	exitDeadline := time.Now().Add(2 * time.Second)
	for !exited() && time.Now().Before(exitDeadline) {
		select {
		case <-ctx.Done():
			p.resetOwnerRetirementCommit()
			return result, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !exited() {
		p.resetOwnerRetirementCommit()
		return result, fmt.Errorf("retirement close was not confirmed; preserve history and reconcile exact process")
	}
	result.Exited = true
	m.mu.Lock()
	if m.panes[paneID] == p {
		delete(m.panes, paneID)
		forgetTerminalOneTapPane(paneID)
	}
	m.mu.Unlock()
	p.detachSubscribers()
	return result, nil
}
func (p *Pane) resetOwnerRetirementCommit() {
	p.ownerRetirementGate.Lock()
	p.ownerRetirementCommitted = false
	p.ownerRetirementGate.Unlock()
	p.mu.Lock()
	p.userClosing = false
	p.mu.Unlock()
}
func writeOwnerRetirementFrame(conn net.Conn, kind FrameType, request ownerRetirementRequest, deadline time.Time) error {
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	return WriteFrame(conn, kind, raw)
}
func waitOwnerRetirementReply(ctx context.Context, p *Pane, replies <-chan ownerRetirementReply, token string) (ownerRetirementReply, error) {
	for {
		select {
		case <-ctx.Done():
			return ownerRetirementReply{}, ctx.Err()
		case <-p.done:
			// readLoop enqueues an accepted commit before observing FrameExit.
			// A ready done channel must not hide that ordered acknowledgement.
			for {
				select {
				case reply := <-replies:
					if reply.Token == token {
						return reply, nil
					}
				default:
					return ownerRetirementReply{}, fmt.Errorf("owner agent stream closed before acknowledgement")
				}
			}
		case reply := <-replies:
			if reply.Token == token {
				return reply, nil
			}
		}
	}
}
