package main

import (
	"errors"
	"fmt"
	"net"
	"time"

	hp "soluterminal/helperproto"
)

// Helper-side attestation for M3 `fwd` streams (ST-253, helper-protocol-v1
// "Proposed for M3 fwd", @secure ruling 8.6a).
//
// A process inside a WSL pane connects to one of the helper's unix sockets
// (mcp.sock, hooks.sock). The helper names the pane it belongs to from kernel
// facts only:
//   - SO_PEERCRED gives the peer's pid and uid; the uid must be the helper's;
//   - the peer's /proc start time is pinned, so a reused pid is noticed;
//   - its parents are walked up to an agent the helper spawned, matched by
//     that agent record's pid AND startTicks (the helper's own identity check,
//     hp.AgentRegistry). Each step requires the parent to have started no
//     later than its child: a reused pid in the chain started after the child
//     it claims, so the walk stops there instead of crossing into another
//     pane;
//   - every start time read on the way is read again once the agent is
//     found, so a chain that changed during the walk is refused.
// Nothing the peer sends (headers, env, body) takes part. No match means no
// attestation: the connection is closed and nothing is forwarded.
//
// This file is inert until the helper serves those sockets (W3 PR 2) and the
// host accepts `fwd` streams (W3 PR 3, off by default until enabled per 8.6a).

// helperAttestMaxDepth bounds the parent walk, as mcpPeerUnderDaemon does.
const helperAttestMaxDepth = 64

// The agent registry is helperproto.AgentRegistry (W2): AgentByPid returns
// the live agent the helper spawned with that pid, from its records. The walk
// below also requires the record's StartTicks to equal the live process's,
// so a record whose pid was reused never matches.

// helperPeer is the process that connected, as the kernel names it: uid and
// pid from SO_PEERCRED, and alive, which reports whether that pid still
// belongs to the connector (a pidfd from SO_PEERPIDFD on Linux). A /proc read
// followed by a true alive() described the connector, not a process that
// reused its pid.
type helperPeer struct {
	UID, PID int
	alive    func() bool
	close    func()
}

// helperAttestProc is the kernel side of the walk, replaceable in tests.
type helperAttestProc struct {
	peerOf    func(net.Conn) (helperPeer, error)
	startTime func(pid int) (uint64, bool)
	ppid      func(pid int) int
	euid      func() int
}

func defaultHelperAttestProc() helperAttestProc {
	return helperAttestProc{
		peerOf:    helperPeerOfPlatform,
		startTime: mcpProcessStartTime,
		ppid:      readPpid,
		euid:      mcpSocketEUID,
	}
}

// helperAttestation is what the helper vouches for on one connection. It
// holds the peer's pidfd: Close it when the connection ends.
type helperAttestation struct {
	PaneID string
	Agent  hp.AgentInfo
	PID    int    // the peer, from SO_PEERCRED
	UID    int    // the peer, from SO_PEERCRED
	Start  uint64 // the peer's /proc start ticks
	peer   helperPeer
}

func (a helperAttestation) wire() hp.FwdAttest {
	return hp.FwdAttest{Agent: a.Agent.AgentID, PID: a.PID, Start: a.Start, UID: a.UID}
}

// Close releases the peer's pidfd. After it, stillValid is false.
func (a helperAttestation) Close() {
	if a.peer.close != nil {
		a.peer.close()
	}
}

var (
	errHelperAttestNoPeer     = errors.New("no peer pid")
	errHelperAttestOtherUser  = errors.New("peer runs as another user")
	errHelperAttestPeerGone   = errors.New("peer exited (its pid may name another process now)")
	errHelperAttestNoStart    = errors.New("process start time unreadable")
	errHelperAttestNoAgent    = errors.New("no spawned agent among the peer's parents")
	errHelperAttestReused     = errors.New("a parent started after its child (pid reused)")
	errHelperAttestChanged    = errors.New("the process chain changed during the walk")
	errHelperAttestBadPaneRef = errors.New("agent record has an invalid id")
)

type helperAttestor struct {
	index        hp.AgentRegistry
	proc         helperAttestProc
	registerWait time.Duration
}

func newHelperAttestor(index hp.AgentRegistry) *helperAttestor {
	return &helperAttestor{index: index, proc: defaultHelperAttestProc(), registerWait: helperAttestRegisterWait}
}

// helperAttestRegisterWait bounds how long attestConn waits for a just-
// started agent's record: the CLI can connect before the helper has written
// it. Only "no spawned agent among the parents" is retried; every other
// refusal is immediate. It fits inside hook-forward's own timeout together
// with the host's registration wait (helperFwdAdmitWait).
const helperAttestRegisterWait = 1500 * time.Millisecond

// attestConn identifies the process at the other end of c. On success the
// caller owns the attestation and must Close it.
func (h *helperAttestor) attestConn(c net.Conn) (helperAttestation, error) {
	peer, err := h.proc.peerOf(c)
	if err != nil {
		return helperAttestation{}, fmt.Errorf("peer credentials: %w", err)
	}
	deadline := time.Now().Add(h.registerWait)
	for {
		a, err := h.attestPeer(peer)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, errHelperAttestNoAgent) || !time.Now().Before(deadline) {
			if peer.close != nil {
				peer.close()
			}
			return a, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type helperAttestStep struct {
	pid   int
	start uint64
}

// attestPeer is attestConn after the kernel named the peer.
func (h *helperAttestor) attestPeer(peer helperPeer) (helperAttestation, error) {
	pid, uid := peer.PID, peer.UID
	if pid <= 0 {
		return helperAttestation{}, errHelperAttestNoPeer
	}
	if uid != h.proc.euid() {
		return helperAttestation{}, errHelperAttestOtherUser
	}
	if peer.alive == nil {
		return helperAttestation{}, errHelperAttestPeerGone
	}
	start, ok := h.proc.startTime(pid)
	if !ok {
		return helperAttestation{}, errHelperAttestNoStart
	}
	// The read above is the connector's only if its pid is still its own now.
	if !peer.alive() {
		return helperAttestation{}, errHelperAttestPeerGone
	}
	chain := []helperAttestStep{{pid, start}}
	cur := helperAttestStep{pid, start}
	for depth := 0; depth < helperAttestMaxDepth; depth++ {
		if ref, found := h.index.AgentByPid(cur.pid); found {
			if ref.Pid != cur.pid || ref.StartTicks != cur.start {
				// The index vouches for a different process than the one
				// walked to: refuse rather than guess.
				return helperAttestation{}, errHelperAttestChanged
			}
			if !hp.ValidAgentID(ref.AgentID) || !hp.ValidPaneID(ref.PaneID) {
				return helperAttestation{}, errHelperAttestBadPaneRef
			}
			if !h.chainUnchanged(chain) || !peer.alive() {
				return helperAttestation{}, errHelperAttestChanged
			}
			return helperAttestation{PaneID: ref.PaneID, Agent: ref, PID: pid, UID: uid, Start: start, peer: peer}, nil
		}
		next := h.proc.ppid(cur.pid)
		if next <= 1 || next == cur.pid {
			return helperAttestation{}, errHelperAttestNoAgent
		}
		nextStart, ok := h.proc.startTime(next)
		if !ok {
			return helperAttestation{}, errHelperAttestNoStart
		}
		if nextStart > cur.start {
			return helperAttestation{}, errHelperAttestReused
		}
		cur = helperAttestStep{next, nextStart}
		chain = append(chain, cur)
	}
	return helperAttestation{}, errHelperAttestNoAgent
}

// chainUnchanged reads the chain again: every process has the start time it
// had, and every link still points at the same parent. A child reparented
// during the walk (its old parent possibly still a zombie with the same start
// time) fails the link check.
func (h *helperAttestor) chainUnchanged(chain []helperAttestStep) bool {
	for i, s := range chain {
		if now, ok := h.proc.startTime(s.pid); !ok || now != s.start {
			return false
		}
		if i+1 < len(chain) && h.proc.ppid(s.pid) != chain[i+1].pid {
			return false
		}
	}
	return true
}

// stillValid is checked before each message is forwarded and before each
// reply is written: the peer is the same live process, and its agent still
// passes the identity check as the same agent.
func (h *helperAttestor) stillValid(a helperAttestation) bool {
	if a.peer.alive == nil {
		return false
	}
	if now, ok := h.proc.startTime(a.PID); !ok || now != a.Start || !a.peer.alive() {
		return false
	}
	ref, ok := h.index.AgentByPid(a.Agent.Pid)
	return ok && ref == a.Agent
}
