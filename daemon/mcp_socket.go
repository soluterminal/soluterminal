package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// The MCP socket is a second, local-only way for an agent to reach the MCP
// tools: a per-user Unix socket at <data>/run/mcp.sock carrying the MCP stdio
// framing (one JSON-RPC message per line). Its point is caller identity. A TCP
// connection is traced to its process through the socket tables, which fails
// when the network rewrites a loopback connection (WSL's user-mode networking
// does, for most IPv4 loopback connections). A Unix socket hands the daemon the
// connecting process from the kernel itself (SO_PEERCRED, LOCAL_PEERPID), so
// the trace no longer depends on the network.
//
// Trust is the same as for HTTP /mcp, never wider:
//   - the peer must run as the daemon's own user, or it is turned away;
//   - its PID is pinned to the process start time, and the connection is
//     dropped if that process is gone or the PID was reused;
//   - its pane comes only from the process ancestry check HTTP uses
//     (classifyPeerProcess). Nothing the client sends names a pane: this
//     transport has no X-Soluterminal-Pane header, and message contents are
//     never read for identity;
//   - it is never the person (callerHuman), never a device and never the
//     relay. Nothing that needs a person's proof becomes reachable here.
//
// A process running as the same user can still ptrace or drive another
// pane's processes; that limit is the same as today and needs a sandbox or a
// separate user to close.
//
// The endpoint is not served by the HTTP API, the relay, the Shinoque /b/
// proxy or any TCP listener: it has its own accept loop and no HTTP at all.

const (
	mcpSocketDirName = "run"
	mcpSocketName    = "mcp.sock"
	// mcpSocketMaxMessage caps one framed message; a longer line closes the
	// connection.
	mcpSocketMaxMessage = 4 << 20
	// mcpSocketMaxConns caps open connections; more are closed at accept.
	mcpSocketMaxConns = 128
	// mcpSocketMaxInFlight caps concurrent calls on one connection. Calls
	// may finish out of order, as JSON-RPC allows (ask_pane waits minutes).
	mcpSocketMaxInFlight  = 16
	mcpSocketWriteTimeout = 30 * time.Second
)

// mcpSocketPeer is what the kernel says about the other end of a connection.
type mcpSocketPeer struct {
	UID   int
	PID   int
	Start uint64
}

// Platform hooks, variables so tests can stand in for the kernel.
var (
	mcpSocketPeerCred     = mcpSocketPeerCredPlatform
	mcpProcessStartTime   = mcpProcessStartTimePlatform
	mcpSocketEUID         = os.Geteuid
	mcpSocketFilesystemOK = mcpSocketFilesystemOKPlatform
)

// mcpSocketEndpoint is the bound, not yet necessarily published, socket.
type mcpSocketEndpoint struct {
	dir  string
	path string // published name, <dir>/mcp.sock
	tmp  string // name bound at startup, renamed to path by activate
	ln   net.Listener
	sem  chan struct{}
}

// serve accepts connections until ctx ends or the listener closes.
func (e *mcpSocketEndpoint) serve(ctx context.Context, a *apiServer) {
	go func() {
		<-ctx.Done()
		_ = e.ln.Close()
	}()
	backoff := 5 * time.Millisecond
	for {
		c, err := e.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			log.Printf("mcp socket: accept: %v", err)
			time.Sleep(backoff)
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Millisecond
		select {
		case e.sem <- struct{}{}:
		default:
			log.Printf("mcp socket: %d connections open; closing a new one", mcpSocketMaxConns)
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-e.sem }()
			a.serveMCPSocketConn(c)
		}()
	}
}

// mcpSocketPeerOf reads the peer's credentials and pins its start time, or
// reports why the connection must be closed.
func mcpSocketPeerOf(c net.Conn) (mcpSocketPeer, error) {
	uid, pid, err := mcpSocketPeerCred(c)
	if err != nil {
		return mcpSocketPeer{}, err
	}
	p := mcpSocketPeer{UID: uid, PID: pid}
	if pid <= 0 {
		return p, errors.New("no peer pid")
	}
	if uid != mcpSocketEUID() {
		return p, errors.New("peer runs as another user")
	}
	start, ok := mcpProcessStartTime(pid)
	if !ok {
		return p, errors.New("peer process start time unreadable")
	}
	p.Start = start
	return p, nil
}

// mcpSocketPeerAlive reports whether the pinned process is still the one
// that connected.
func mcpSocketPeerAlive(p mcpSocketPeer) bool {
	start, ok := mcpProcessStartTime(p.PID)
	return ok && start == p.Start
}

// classifyMCPSocketPeer places a kernel-given peer: a pane only through its
// process ancestry, as for a traced HTTP caller; otherwise a worker or an
// unverified caller. Never the person.
func (a *apiServer) classifyMCPSocketPeer(pid int) callerIdentity {
	id, known := a.classifyPeerProcess(pid)
	if !known || id.Kind == callerHuman {
		id = callerIdentity{Kind: callerUnverified}
	}
	id.PID = pid
	id.Relay, id.Unproven = false, false
	// Only a pane process found among the ancestors outranks the daemon. A
	// process the daemon started that merely carries SOLU_PANE_ID in its
	// environment is still the daemon's, so it is refused with the workers.
	if (id.Kind != callerPane || id.FromEnv) && mcpPeerUnderDaemon(pid) {
		return callerIdentity{Kind: callerWorker, PID: pid}
	}
	return id
}

// mcpPeerUnderDaemon reports whether the daemon is pid or one of its
// ancestors.
func mcpPeerUnderDaemon(pid int) bool {
	self := os.Getpid()
	cur := pid
	for depth := 0; depth < 64 && cur > 1; depth++ {
		if cur == self {
			return true
		}
		next := readPpid(cur)
		if next == cur {
			return false
		}
		cur = next
	}
	return false
}

func (a *apiServer) serveMCPSocketConn(c net.Conn) {
	defer c.Close()
	peer, err := mcpSocketPeerOf(c)
	if err != nil {
		log.Printf("authority: mcp transport=unix refused uid=%d pid=%d: %v", peer.UID, peer.PID, err)
		return
	}
	// Identity is fixed for the connection; a reconnect is classified anew.
	id := a.classifyMCPSocketPeer(peer.PID)
	if id.Kind == callerWorker {
		// Processes the daemon started (managed agents) carry a scoped HTTP
		// session whose tool budget this transport would not enforce.
		log.Printf("authority: mcp transport=unix refused uid=%d pid=%d start=%d: started by the daemon; use its scoped session", peer.UID, peer.PID, peer.Start)
		return
	}
	log.Printf("authority: mcp transport=unix uid=%d pid=%d start=%d caller=%s pane=%s env=%t",
		peer.UID, peer.PID, peer.Start, id.Kind, id.PaneID, id.FromEnv)

	// alive is checked before reading a message, before dispatching it and
	// before writing its reply. Once the pinned process is gone or its pid
	// names another process, the connection closes at once: nothing more runs
	// for it, and no reply goes to whatever holds the connection now.
	var dropOnce sync.Once
	alive := func() bool {
		if mcpSocketPeerAlive(peer) {
			return true
		}
		dropOnce.Do(func() {
			log.Printf("authority: mcp transport=unix dropped uid=%d pid=%d start=%d: process gone or pid reused", peer.UID, peer.PID, peer.Start)
			_ = c.Close()
		})
		return false
	}
	var writeMu sync.Mutex
	write := func(resp rpcResp) bool {
		out, err := json.Marshal(resp)
		if err != nil {
			return false
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if !alive() {
			return false
		}
		_ = c.SetWriteDeadline(time.Now().Add(mcpSocketWriteTimeout))
		_, err = c.Write(append(out, '\n'))
		return err == nil
	}
	inFlight := make(chan struct{}, mcpSocketMaxInFlight)
	var calls sync.WaitGroup
	defer calls.Wait()

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), mcpSocketMaxMessage)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if !alive() {
			return
		}
		msg := append([]byte(nil), line...)
		inFlight <- struct{}{}
		calls.Add(1)
		go func() {
			defer func() { <-inFlight; calls.Done() }()
			// The wait for a slot can be long; look again before running.
			if !alive() {
				return
			}
			if resp, reply := a.mcpSocketDispatch(msg, id); reply && !write(resp) {
				_ = c.Close()
			}
		}()
	}
	if err := sc.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("mcp socket: pid %d: %v", peer.PID, err)
	}
}

// mcpSocketDispatch answers one framed message from a caller identified by
// its connection. reply is false for notifications, which get none.
func (a *apiServer) mcpSocketDispatch(msg []byte, id callerIdentity) (resp rpcResp, reply bool) {
	var req rpcReq
	if err := json.Unmarshal(msg, &req); err != nil {
		return rpcResp{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error: " + err.Error()}}, true
	}
	if strings.HasPrefix(req.Method, "notifications/") || len(req.ID) == 0 {
		return rpcResp{}, false
	}
	// No header on this transport: the pane is the traced one or none.
	callerPane := a.mcpCallerForIdentity("", id)
	if req.Method == "tools/call" {
		// The same per-call rules as HTTP /mcp (mcp.go).
		if mcpAskToolCall(req.Params) && !askCallerTraced(id) {
			callerPane = ""
		}
		var call toolCallParams
		if json.Unmarshal(req.Params, &call) == nil && call.Name == "create_workspace" && !id.tracedPane() {
			callerPane = mcpClaimedCallerPrefix + id.PaneID
		}
	}
	return a.mcpDispatchTransport(req, nil, callerPane), true
}
