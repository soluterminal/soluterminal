package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	hp "soluterminal/helperproto"
)

// Helper side of M3 `fwd` (ST-253, helper-protocol-v1 "Proposed for M3 fwd",
// @secure ruling 8.6a). Processes in WSL panes reach the Windows host through
// two unix sockets the helper serves in its runtime dir:
//
//   - mcp.sock speaks the daemon MCP socket's line protocol, so today's
//     `solud mcp-bridge --socket <runtimeDir>/mcp.sock` works unchanged;
//   - hooks.sock speaks HTTP/1.1, for `solud hook-forward`.
//
// Every connection is attested first (helper_attest.go); without a pane
// nothing is forwarded. Each MCP message and each hook request then becomes
// one helper-opened `fwd` stream carrying the attested pane and one HTTP
// request. The helper never adds identity headers: the host takes the pane
// from the stream's first line, and only on its own child's session.
//
// Limits, all refused rather than queued (8.6a, with its 4 MiB amendment):
// MCP messages up to 4 MiB (the daemon MCP socket's own limit), hook bodies
// up to 1 MiB, at most 32 open fwd streams per session, at most 50 new fwd streams per
// second per agent. The host checks the same limits again.

const (
	helperFwdMaxOpen        = 32
	helperFwdPerAgentRate   = 50
	helperFwdMaxMCPRequest  = mcpSocketMaxMessage
	helperFwdMaxHookRequest = 1 << 20
	helperFwdMaxResponse    = mcpSocketMaxMessage
	helperFwdHookTimeout    = 5 * time.Second
	helperFwdMCPTimeout     = 20 * time.Minute // longer than any tool's own wait
	helperFwdHostName       = "solud.helper.internal"
	helperFwdMCPPath        = "/mcp"
	helperFwdHookPathRoot   = "/api/hooks/"
	helperFwdMaxHookHeader  = 16 << 10
	helperFwdMaxHookConns   = 64
)

// The helper's in-distro socket names, inside its runtime dir. Portable: the
// Windows host names them in the argv it gives Linux panes.
const (
	helperFwdMCPSocket   = "mcp.sock"
	helperFwdHooksSocket = "hooks.sock"
)

var (
	errHelperFwdBusy    = errors.New("too many forwarded calls; try again")
	errHelperFwdGone    = errors.New("caller is no longer the attested process")
	errHelperFwdTooBig  = errors.New("request too large")
	errHelperFwdBadPath = errors.New("path not forwarded")
)

// helperFwdOpener opens a helper-initiated stream on the session
// (helperproto.HelperSession.Open with KindFwd in OpenKinds).
type helperFwdOpener interface {
	Open(h hp.StreamHeader) (net.Conn, error)
}

type helperFwdServer struct {
	attest func(net.Conn) (helperAttestation, error)
	valid  func(helperAttestation) bool
	open   helperFwdOpener
	logf   func(format string, args ...any)
	now    func() time.Time

	slots chan struct{}

	rateMu sync.Mutex
	rate   map[string]*helperFwdWindow
}

// helperFwdWindow counts one agent's fwd streams in the current second.
type helperFwdWindow struct {
	start time.Time
	n     int
}

func newHelperFwdServer(att *helperAttestor, open helperFwdOpener, logf func(string, ...any)) *helperFwdServer {
	return &helperFwdServer{
		attest: att.attestConn,
		valid:  att.stillValid,
		open:   open,
		logf:   logf,
		now:    time.Now,
		slots:  make(chan struct{}, helperFwdMaxOpen),
		rate:   map[string]*helperFwdWindow{},
	}
}

// allow takes one of the agent's per-second fwd starts.
func (s *helperFwdServer) allow(agentID string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	w := s.rate[agentID]
	if w == nil || now.Sub(w.start) >= time.Second || now.Before(w.start) {
		// Drop windows of agents that have gone quiet, so the map stays small.
		for id, old := range s.rate {
			if now.Sub(old.start) >= time.Second {
				delete(s.rate, id)
			}
		}
		w = &helperFwdWindow{start: now}
		s.rate[agentID] = w
	}
	if w.n >= helperFwdPerAgentRate {
		return false
	}
	w.n++
	return true
}

// forward sends one POST to path on a new fwd stream and returns the host's
// answer. It never blocks for capacity: no slot or no rate left is
// errHelperFwdBusy.
func (s *helperFwdServer) forward(ctx context.Context, a helperAttestation, path string, body []byte, timeout time.Duration) (int, []byte, error) {
	if !helperFwdPathOK(path) {
		return 0, nil, errHelperFwdBadPath
	}
	if len(body) > helperFwdMaxBody(path) {
		return 0, nil, errHelperFwdTooBig
	}
	if !s.valid(a) {
		return 0, nil, errHelperFwdGone
	}
	if !s.allow(a.Agent.AgentID) {
		return 0, nil, errHelperFwdBusy
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return 0, nil, errHelperFwdBusy
	}
	defer func() { <-s.slots }()

	attest, err := json.Marshal(a.wire())
	if err != nil {
		return 0, nil, err
	}
	st, err := s.open.Open(hp.StreamHeader{S: hp.KindFwd, Kind: "http", Pane: a.PaneID, Attest: attest})
	if err != nil {
		return 0, nil, fmt.Errorf("open fwd stream: %w", err)
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { _ = st.SetDeadline(time.Now()) })
	defer stop()

	req, err := http.NewRequest(http.MethodPost, "http://"+helperFwdHostName+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	// Only the content type crosses; nothing the caller sent does.
	req.Header = http.Header{"Content-Type": {"application/json"}}
	req.ContentLength = int64(len(body))
	if err := req.Write(st); err != nil {
		return 0, nil, fmt.Errorf("write fwd request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(st), req)
	if err != nil {
		return 0, nil, fmt.Errorf("read fwd response: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, helperFwdMaxResponse+1))
	if err != nil {
		return 0, nil, fmt.Errorf("read fwd response body: %w", err)
	}
	if len(out) > helperFwdMaxResponse {
		return 0, nil, errors.New("fwd response too large")
	}
	return resp.StatusCode, out, nil
}

// helperFwdMaxBody is the largest request body forwarded to path: an MCP
// message as large as the daemon's own MCP socket takes, a hook up to 1 MiB.
func helperFwdMaxBody(path string) int {
	if path == helperFwdMCPPath {
		return helperFwdMaxMCPRequest
	}
	return helperFwdMaxHookRequest
}

func helperFwdPathOK(path string) bool {
	if path == helperFwdMCPPath {
		return true
	}
	rest, ok := strings.CutPrefix(path, helperFwdHookPathRoot)
	return ok && rest != "" && !strings.ContainsAny(rest, "/?#%\\") && !strings.Contains(rest, "..")
}

// serveMCPConn serves one mcp.sock connection: the line protocol of
// serveMCPSocketConn, each message forwarded as POST /mcp.
func (s *helperFwdServer) serveMCPConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	a, err := s.attest(c)
	if err != nil {
		s.logf("fwd: mcp.sock refused: %v", err)
		return
	}
	defer a.Close()
	s.logf("fwd: mcp.sock pid=%d pane=%s agent=%s", a.PID, a.PaneID, a.Agent.AgentID)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Shutdown or a drop ends the blocking read below.
	stopClose := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopClose()
	var dropOnce sync.Once
	alive := func() bool {
		if s.valid(a) {
			return true
		}
		dropOnce.Do(func() {
			s.logf("fwd: mcp.sock pid=%d dropped: process gone or pid reused", a.PID)
			cancel()
			_ = c.Close()
		})
		return false
	}
	var writeMu sync.Mutex
	write := func(line []byte) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		if !alive() {
			return false
		}
		_ = c.SetWriteDeadline(time.Now().Add(mcpSocketWriteTimeout))
		_, err := c.Write(append(line, '\n'))
		return err == nil
	}
	inFlight := make(chan struct{}, mcpSocketMaxInFlight)
	var calls sync.WaitGroup
	// The caller is gone once the read side ends (mcp-bridge never
	// half-closes: it waits for its replies, then closes): cancel the calls
	// still forwarding so they free their streams, then wait for them.
	defer calls.Wait()
	defer cancel()

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
		select {
		case inFlight <- struct{}{}:
		default:
			// Never wait here: a reader blocked on a slot could not see
			// the caller disconnect, and the stalled calls would keep their
			// streams. Past the cap a request is answered busy at once.
			if reply, ok := helperFwdBusyReply(msg); ok && !write(reply) {
				return
			}
			continue
		}
		calls.Add(1)
		go func() {
			defer func() { <-inFlight; calls.Done() }()
			if !alive() {
				return
			}
			if reply, ok := s.mcpMessage(ctx, a, msg); ok && !write(reply) {
				_ = c.Close()
			}
		}()
	}
}

// mcpMessage forwards one JSON-RPC message and returns the line to send back,
// or ok=false when none is due (a notification).
func (s *helperFwdServer) mcpMessage(ctx context.Context, a helperAttestation, msg []byte) ([]byte, bool) {
	var head struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(msg, &head); err != nil {
		return helperFwdRPCError(nil, -32700, "parse error: "+err.Error()), true
	}
	// The daemon's own rule (mcp.go, mcp_socket.go): a notifications/*
	// method, or no id at all. An explicit "id":null is a request.
	notification := strings.HasPrefix(head.Method, "notifications/") || len(head.ID) == 0
	status, body, err := s.forward(ctx, a, helperFwdMCPPath, msg, helperFwdMCPTimeout)
	if notification {
		if err != nil {
			s.logf("fwd: mcp notification %q from pane %s not forwarded: %v", head.Method, a.PaneID, err)
		}
		return nil, false
	}
	switch {
	case errors.Is(err, errHelperFwdBusy):
		return helperFwdRPCError(head.ID, -32000, "soluterminal: "+err.Error()), true
	case errors.Is(err, errHelperFwdTooBig):
		return helperFwdRPCError(head.ID, -32600, "soluterminal: "+err.Error()), true
	case err != nil:
		// As mcp-bridge does for a lost connection: the call may or may not
		// have run on the host; it is never sent again.
		s.logf("fwd: mcp call from pane %s: %v", a.PaneID, err)
		return helperFwdRPCError(head.ID, mcpBridgeLostCode, mcpBridgeLostMessage), true
	}
	body = bytes.TrimSpace(body)
	if status != http.StatusOK || len(body) == 0 || bytes.IndexByte(body, '\n') >= 0 {
		return helperFwdRPCError(head.ID, -32603, fmt.Sprintf("soluterminal: host answered %d", status)), true
	}
	return body, true
}

func helperFwdRPCError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcErr          `json:"error"`
	}{"2.0", id, rpcErr{Code: code, Message: message}})
	return out
}

type helperFwdAttestKey struct{}

// hooksHandler serves hooks.sock. The attestation comes from the connection
// (hooksConnContext); a request on an unattested connection never reaches it.
func (s *helperFwdServer) hooksHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := r.Context().Value(helperFwdAttestKey{}).(helperAttestation)
		if !ok {
			la, lazy := r.Context().Value(helperFwdLazyKey{}).(*helperFwdLazyAttest)
			if !lazy {
				http.Error(w, "not attested", http.StatusForbidden)
				return
			}
			var err error
			if a, err = la.get(); err != nil {
				s.logf("fwd: hooks.sock refused: %v", err)
				w.Header().Set("Connection", "close")
				http.Error(w, "not attested", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, helperFwdHookPathRoot) || !helperFwdPathOK(r.URL.Path) {
			http.Error(w, errHelperFwdBadPath.Error(), http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, helperFwdMaxHookRequest+1))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		status, out, err := s.forward(r.Context(), a, r.URL.Path, body, helperFwdHookTimeout)
		switch {
		case errors.Is(err, errHelperFwdTooBig):
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		case errors.Is(err, errHelperFwdBusy):
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		case errors.Is(err, errHelperFwdGone):
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		case err != nil:
			s.logf("fwd: hook from pane %s: %v", a.PaneID, err)
			http.Error(w, "not forwarded", http.StatusBadGateway)
			return
		}
		// The reply goes only to the process the request came from.
		if !s.valid(a) {
			s.logf("fwd: hooks.sock pid=%d gone before its reply", a.PID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out)
	})
}

// hooksServer is the http.Server for hooks.sock. Each connection is attested
// once, lazily, on its first request, in that connection's own goroutine:
// ConnContext runs in the shared accept loop, so attesting there (which can
// wait for a just-started agent's record) would stall every pane's hooks.
func (s *helperFwdServer) hooksServer() *http.Server {
	var mu sync.Mutex
	lazy := map[net.Conn]*helperFwdLazyAttest{}
	return &http.Server{
		Handler: s.hooksHandler(),
		// A hook is one small request: its headers and body arrive within
		// the timeout, the reply is written within twice that, and an idle
		// keep-alive connection is closed. The listener also caps the
		// number of open connections.
		ReadHeaderTimeout: helperFwdHookTimeout,
		ReadTimeout:       helperFwdHookTimeout,
		WriteTimeout:      2 * helperFwdHookTimeout,
		IdleTimeout:       helperFwdHookTimeout,
		MaxHeaderBytes:    helperFwdMaxHookHeader,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			// Nothing slow here: only remember the socket. The kernel
			// credentials belong to the socket under the cap wrapper.
			raw := c
			if lc, ok := c.(*helperFwdLimitConn); ok {
				raw = lc.Conn
			}
			la := &helperFwdLazyAttest{conn: raw, attest: s.attest}
			mu.Lock()
			lazy[c] = la
			mu.Unlock()
			return context.WithValue(ctx, helperFwdLazyKey{}, la)
		},
		ConnState: func(c net.Conn, st http.ConnState) {
			if st != http.StateClosed && st != http.StateHijacked {
				return
			}
			mu.Lock()
			la := lazy[c]
			delete(lazy, c)
			mu.Unlock()
			if la != nil {
				la.close()
			}
		},
	}
}

type helperFwdLazyKey struct{}

// helperFwdLazyAttest attests one hooks.sock connection on first use.
type helperFwdLazyAttest struct {
	conn   net.Conn
	attest func(net.Conn) (helperAttestation, error)

	mu     sync.Mutex
	done   bool
	closed bool
	a      helperAttestation
	err    error
}

func (l *helperFwdLazyAttest) get() (helperAttestation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return helperAttestation{}, net.ErrClosed
	}
	if !l.done {
		l.done = true
		l.a, l.err = l.attest(l.conn)
	}
	return l.a, l.err
}

func (l *helperFwdLazyAttest) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.done && l.err == nil {
		l.a.Close()
	}
}

// acceptMCP accepts mcp.sock connections until ln closes, at most
// mcpSocketMaxConns at once (more are closed at accept).
func (s *helperFwdServer) acceptMCP(ctx context.Context, ln net.Listener) {
	sem := make(chan struct{}, mcpSocketMaxConns)
	var conns sync.WaitGroup
	defer conns.Wait()
	backoff := 5 * time.Millisecond
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			s.logf("fwd: mcp.sock accept: %v", err)
			time.Sleep(backoff)
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Millisecond
		select {
		case sem <- struct{}{}:
		default:
			s.logf("fwd: mcp.sock: %d connections open; closing a new one", mcpSocketMaxConns)
			_ = c.Close()
			continue
		}
		conns.Add(1)
		go func() {
			defer func() { <-sem; conns.Done() }()
			s.serveMCPConn(ctx, c)
		}()
	}
}

// helperFwdLimitListener closes connections accepted past max open ones, as
// the mcp.sock accept loop does, instead of letting them wait.
type helperFwdLimitListener struct {
	net.Listener
	sem  chan struct{}
	logf func(string, ...any)
}

func newHelperFwdLimitListener(ln net.Listener, max int, logf func(string, ...any)) net.Listener {
	return &helperFwdLimitListener{Listener: ln, sem: make(chan struct{}, max), logf: logf}
}

func (l *helperFwdLimitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &helperFwdLimitConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
			l.logf("fwd: hooks.sock: %d connections open; closing a new one", cap(l.sem))
			_ = c.Close()
		}
	}
}

type helperFwdLimitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *helperFwdLimitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// helperFwdBusyReply is the immediate answer to a message that found every
// per-connection call slot in use: a busy error for a request, nothing for a
// notification (the daemon's rule).
func helperFwdBusyReply(msg []byte) ([]byte, bool) {
	var head struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(msg, &head); err != nil {
		return helperFwdRPCError(nil, -32700, "parse error: "+err.Error()), true
	}
	if strings.HasPrefix(head.Method, "notifications/") || len(head.ID) == 0 {
		return nil, false
	}
	return helperFwdRPCError(head.ID, -32000, "soluterminal: "+errHelperFwdBusy.Error()), true
}
