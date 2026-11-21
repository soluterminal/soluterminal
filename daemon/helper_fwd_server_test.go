package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hp "soluterminal/helperproto"
	"soluterminal/helperproto/helpertest"
)

// fwdSeen is one forwarded request as the host received it.
type fwdSeen struct {
	header hp.StreamHeader
	method string
	path   string
	hdr    http.Header
	body   string
}

// fwdHost is a host session whose fwd handler records each request and
// answers with reply.
type fwdHost struct {
	mu    sync.Mutex
	seen  []fwdSeen
	calls atomic.Int32
	reply func(r fwdSeen) (status int, body string, hangUp bool)
}

func (h *fwdHost) handler(_ *hp.HostSession, sh hp.StreamHeader, c net.Conn) {
	defer c.Close()
	h.calls.Add(1)
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return
	}
	b, _ := io.ReadAll(req.Body)
	r := fwdSeen{header: sh, method: req.Method, path: req.URL.Path, hdr: req.Header, body: string(b)}
	h.mu.Lock()
	h.seen = append(h.seen, r)
	h.mu.Unlock()
	status, body, hangUp := 200, `{"jsonrpc":"2.0","id":1,"result":{}}`, false
	if h.reply != nil {
		status, body, hangUp = h.reply(r)
	}
	if hangUp {
		return
	}
	resp := &http.Response{StatusCode: status, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
	_ = resp.Write(c)
}

func (h *fwdHost) all() []fwdSeen {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fwdSeen(nil), h.seen...)
}

// fwdTestAttestation is what a fake attestor vouches for.
func fwdTestAttestation(alive *atomic.Bool) helperAttestation {
	return helperAttestation{
		PaneID: testAttestPaneID,
		Agent:  hp.AgentInfo{AgentID: testAttestAgentID, PaneID: testAttestPaneID, Pid: 100, StartTicks: 1000},
		PID:    400, UID: testAttestUID, Start: 1030,
		peer: helperPeer{UID: testAttestUID, PID: 400, alive: alive.Load, close: func() {}},
	}
}

// newFwdTest wires a helper fwd server to a host fwd handler over W2's
// in-memory session pair.
func newFwdTest(t *testing.T, host *fwdHost) (*helperFwdServer, *atomic.Bool) {
	t.Helper()
	p := helpertest.NewPair(t, helpertest.Options{
		Host:   hp.HostOptions{Handlers: map[string]hp.HostStreamHandler{hp.KindFwd: host.handler}},
		Helper: hp.HelperOptions{OpenKinds: []string{hp.KindFwd}},
	})
	alive := &atomic.Bool{}
	alive.Store(true)
	s := &helperFwdServer{
		attest: func(net.Conn) (helperAttestation, error) { return fwdTestAttestation(alive), nil },
		valid:  func(a helperAttestation) bool { return a.peer.alive != nil && a.peer.alive() },
		open:   p.Helper,
		logf:   t.Logf,
		now:    time.Now,
		slots:  make(chan struct{}, helperFwdMaxOpen),
		rate:   map[string]*helperFwdWindow{},
	}
	return s, alive
}

// mcpClient is the agent side of an mcp.sock connection.
type mcpClient struct {
	c  net.Conn
	rd *bufio.Reader
}

func startMCPConn(t *testing.T, s *helperFwdServer) *mcpClient {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.serveMCPConn(ctx, server) }()
	t.Cleanup(func() { cancel(); _ = client.Close(); <-done })
	return &mcpClient{c: client, rd: bufio.NewReader(client)}
}

func (m *mcpClient) send(t *testing.T, line string) {
	t.Helper()
	_ = m.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := m.c.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func (m *mcpClient) recv(t *testing.T) map[string]any {
	t.Helper()
	_ = m.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := m.rd.ReadString('\n')
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("recv %q: %v", line, err)
	}
	return out
}

func TestHelperFwdMCPRoundTrip(t *testing.T) {
	host := &fwdHost{reply: func(r fwdSeen) (int, string, bool) {
		return 200, `{"jsonrpc":"2.0","id":7,"result":{"tools":[]}}`, false
	}}
	s, _ := newFwdTest(t, host)
	m := startMCPConn(t, s)
	m.send(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	got := m.recv(t)
	if got["id"] != float64(7) || got["result"] == nil {
		t.Fatalf("reply %v", got)
	}
	seen := host.all()
	if len(seen) != 1 {
		t.Fatalf("host saw %d requests", len(seen))
	}
	r := seen[0]
	if r.header.S != hp.KindFwd || r.header.Kind != "http" || r.header.Pane != testAttestPaneID {
		t.Fatalf("stream header %+v", r.header)
	}
	var att hp.FwdAttest
	if err := json.Unmarshal(r.header.Attest, &att); err != nil {
		t.Fatal(err)
	}
	if att != (hp.FwdAttest{Agent: testAttestAgentID, PID: 400, Start: 1030, UID: testAttestUID}) {
		t.Fatalf("attest %+v", att)
	}
	if r.method != http.MethodPost || r.path != "/mcp" || r.body != `{"jsonrpc":"2.0","id":7,"method":"tools/list"}` {
		t.Fatalf("request %s %s %q", r.method, r.path, r.body)
	}
	for k := range r.hdr {
		switch k {
		case "Content-Type", "Content-Length", "User-Agent":
		default:
			t.Errorf("forwarded header %s", k)
		}
	}
}

func TestHelperFwdMCPNotificationGetsNoReply(t *testing.T) {
	host := &fwdHost{reply: func(r fwdSeen) (int, string, bool) {
		if strings.Contains(r.body, "notifications/") {
			return http.StatusAccepted, "", false
		}
		return 200, `{"jsonrpc":"2.0","id":2,"result":{}}`, false
	}}
	s, _ := newFwdTest(t, host)
	m := startMCPConn(t, s)
	m.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	m.send(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := m.recv(t); got["id"] != float64(2) {
		t.Fatalf("first line back is %v, want the reply to id 2", got)
	}
}

func TestHelperFwdMCPErrors(t *testing.T) {
	t.Run("host hangs up: lost, never resent", func(t *testing.T) {
		host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { return 0, "", true }}
		s, _ := newFwdTest(t, host)
		m := startMCPConn(t, s)
		m.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call"}`)
		got := m.recv(t)
		e, _ := got["error"].(map[string]any)
		if got["id"] != float64(3) || e["code"] != float64(mcpBridgeLostCode) {
			t.Fatalf("reply %v", got)
		}
		if n := host.calls.Load(); n != 1 {
			t.Fatalf("host called %d times", n)
		}
	})
	t.Run("an MCP message over 1 MiB is forwarded whole", func(t *testing.T) {
		host := &fwdHost{reply: func(fwdSeen) (int, string, bool) {
			return 200, `{"jsonrpc":"2.0","id":4,"result":{}}`, false
		}}
		s, _ := newFwdTest(t, host)
		m := startMCPConn(t, s)
		big := `{"jsonrpc":"2.0","id":4,"method":"x","params":{"p":"` + strings.Repeat("a", 2<<20) + `"}}`
		m.send(t, big)
		got := m.recv(t)
		seen := host.all()
		if got["id"] != float64(4) || got["result"] == nil || len(seen) != 1 || seen[0].body != big {
			t.Fatalf("reply %v, host saw %d requests", got, len(seen))
		}
	})
	t.Run("an MCP body past 4 MiB is refused", func(t *testing.T) {
		host := &fwdHost{}
		s, alive := newFwdTest(t, host)
		_, _, err := s.forward(context.Background(), fwdTestAttestation(alive), helperFwdMCPPath, make([]byte, helperFwdMaxMCPRequest+1), time.Second)
		if !errors.Is(err, errHelperFwdTooBig) || host.calls.Load() != 0 {
			t.Fatalf("forward: %v, host calls %d", err, host.calls.Load())
		}
	})
	t.Run("no free slot: busy", func(t *testing.T) {
		host := &fwdHost{}
		s, _ := newFwdTest(t, host)
		for i := 0; i < helperFwdMaxOpen; i++ {
			s.slots <- struct{}{}
		}
		m := startMCPConn(t, s)
		m.send(t, `{"jsonrpc":"2.0","id":5,"method":"ping"}`)
		got := m.recv(t)
		e, _ := got["error"].(map[string]any)
		if got["id"] != float64(5) || e["code"] != float64(-32000) || host.calls.Load() != 0 {
			t.Fatalf("reply %v, host calls %d", got, host.calls.Load())
		}
	})
	t.Run("host error status", func(t *testing.T) {
		host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { return http.StatusForbidden, "refused", false }}
		s, _ := newFwdTest(t, host)
		m := startMCPConn(t, s)
		m.send(t, `{"jsonrpc":"2.0","id":6,"method":"ping"}`)
		got := m.recv(t)
		e, _ := got["error"].(map[string]any)
		if got["id"] != float64(6) || e["code"] != float64(-32603) {
			t.Fatalf("reply %v", got)
		}
	})
	t.Run("parse error", func(t *testing.T) {
		host := &fwdHost{}
		s, _ := newFwdTest(t, host)
		m := startMCPConn(t, s)
		m.send(t, `{not json`)
		got := m.recv(t)
		e, _ := got["error"].(map[string]any)
		if e["code"] != float64(-32700) || host.calls.Load() != 0 {
			t.Fatalf("reply %v", got)
		}
	})
}

// Once the attested process is gone, nothing more is forwarded and the
// connection closes.
func TestHelperFwdMCPDropsAGoneCaller(t *testing.T) {
	host := &fwdHost{}
	s, alive := newFwdTest(t, host)
	m := startMCPConn(t, s)
	m.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	m.recv(t)
	alive.Store(false)
	_, _ = m.c.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"))
	_ = m.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := m.rd.ReadString('\n'); err == nil {
		t.Fatal("a reply reached a gone caller")
	}
	if n := host.calls.Load(); n != 1 {
		t.Fatalf("host called %d times after the caller went", n)
	}
}

func TestHelperFwdRefusesUnattested(t *testing.T) {
	host := &fwdHost{}
	s, _ := newFwdTest(t, host)
	s.attest = func(net.Conn) (helperAttestation, error) { return helperAttestation{}, errHelperAttestNoAgent }
	m := startMCPConn(t, s)
	_, _ = m.c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
	_ = m.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := m.rd.ReadString('\n'); err == nil {
		t.Fatal("an unattested connection got a reply")
	}
	if host.calls.Load() != 0 {
		t.Fatal("an unattested connection was forwarded")
	}
}

func TestHelperFwdRate(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &helperFwdServer{now: func() time.Time { return now }, rate: map[string]*helperFwdWindow{}}
	for i := 0; i < helperFwdPerAgentRate; i++ {
		if !s.allow("a") {
			t.Fatalf("start %d refused", i)
		}
	}
	if s.allow("a") {
		t.Fatal("start past the per-second limit allowed")
	}
	if !s.allow("b") {
		t.Fatal("another agent shares the limit")
	}
	now = now.Add(time.Second)
	if !s.allow("a") {
		t.Fatal("limit not reset after a second")
	}
	// A clock that goes back starts a new window rather than locking out.
	now = now.Add(-time.Hour)
	if !s.allow("a") {
		t.Fatal("refused after the clock moved back")
	}
}

func TestHelperFwdPathOK(t *testing.T) {
	for _, p := range []string{"/mcp", "/api/hooks/tool-call"} {
		if !helperFwdPathOK(p) {
			t.Errorf("%s refused", p)
		}
	}
	for _, p := range []string{"", "/", "/mcp/", "/mcpx", "/api/hooks/", "/api/hooks/a/b", "/api/hooks/../panes", "/api/hooks/x?y", "/api/hooks/%2e%2e", "/api/panes", "/api/hooks"} {
		if helperFwdPathOK(p) {
			t.Errorf("%s allowed", p)
		}
	}
}

func TestHelperFwdHookCarriesNoCallerHeaders(t *testing.T) {
	host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { return 200, `{}`, false }}
	s, _ := newFwdTest(t, host)
	srv := s.hooksServer()
	client, server := net.Pipe()
	ln := &oneConnListener{c: server, done: make(chan struct{})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = client.Close() })

	req, _ := http.NewRequest(http.MethodPost, "http://x/api/hooks/tool-call", strings.NewReader(`{"session_id":"s"}`))
	req.Header.Set("X-Soluterminal-Pane", "pane_ffffffffffffffff")
	req.Header.Set("Origin", "http://127.0.0.1:8787")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Cookie", "a=b")
	req.Header.Set("Authorization", "Bearer x")
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := req.Write(client); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	seen := host.all()
	if len(seen) != 1 || seen[0].path != "/api/hooks/tool-call" || seen[0].header.Pane != testAttestPaneID {
		t.Fatalf("seen %+v", seen)
	}
	for _, h := range []string{"X-Soluterminal-Pane", "Origin", "Sec-Fetch-Site", "Cookie", "Authorization"} {
		if v := seen[0].hdr.Get(h); v != "" {
			t.Errorf("%s forwarded: %q", h, v)
		}
	}
}

func TestHelperFwdHookRefusesOtherPaths(t *testing.T) {
	host := &fwdHost{}
	s, alive := newFwdTest(t, host)
	h := s.hooksHandler()
	for _, tc := range []struct{ method, path string }{{"GET", "/api/hooks/tool-call"}, {"POST", "/mcp"}, {"POST", "/api/panes"}} {
		rec := &recordingWriter{h: http.Header{}}
		r, _ := http.NewRequest(tc.method, "http://x"+tc.path, strings.NewReader("{}"))
		r = r.WithContext(context.WithValue(r.Context(), helperFwdAttestKey{}, fwdTestAttestation(alive)))
		h.ServeHTTP(rec, r)
		if rec.code != http.StatusNotFound {
			t.Errorf("%s %s: %d", tc.method, tc.path, rec.code)
		}
	}
	rec := &recordingWriter{h: http.Header{}}
	r, _ := http.NewRequest("POST", "http://x/api/hooks/tool-call", strings.NewReader("{}"))
	h.ServeHTTP(rec, r)
	if rec.code != http.StatusForbidden {
		t.Errorf("unattested request: %d", rec.code)
	}
	if host.calls.Load() != 0 {
		t.Fatal("a refused hook reached the host")
	}
}

type recordingWriter struct {
	h    http.Header
	code int
}

func (w *recordingWriter) Header() http.Header { return w.h }
func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return len(b), nil
}
func (w *recordingWriter) WriteHeader(c int) { w.code = c }

// oneConnListener hands out one connection, then blocks until closed.
type oneConnListener struct {
	mu   sync.Mutex
	c    net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.c
	l.c = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneConnListener) Addr() net.Addr { return &net.UnixAddr{Name: "test", Net: "unix"} }

// The daemon transports' rule: "id":null is a request; a notifications/*
// method gets no reply even with an id.
func TestHelperFwdMCPNotificationRule(t *testing.T) {
	host := &fwdHost{reply: func(r fwdSeen) (int, string, bool) {
		if strings.Contains(r.body, "notifications/") {
			return http.StatusAccepted, "", false
		}
		return 200, `{"jsonrpc":"2.0","id":null,"result":{}}`, false
	}}
	s, _ := newFwdTest(t, host)
	m := startMCPConn(t, s)
	m.send(t, `{"jsonrpc":"2.0","id":11,"method":"notifications/cancelled"}`)
	m.send(t, `{"jsonrpc":"2.0","id":null,"method":"ping"}`)
	got := m.recv(t)
	if v, ok := got["id"]; !ok || v != nil || got["result"] == nil {
		t.Fatalf("first reply %v, want the result for id null", got)
	}
}

// A caller that disconnects cancels its stalled forwards at once, so they
// free their stream slots instead of holding them until the MCP timeout.
func TestHelperFwdMCPDisconnectCancelsCalls(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { <-release; return 0, "", true }}
	s, _ := newFwdTest(t, host)
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); s.serveMCPConn(context.Background(), server) }()
	_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.slots) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.slots) != 1 {
		t.Fatalf("forward not started: %d slots in use", len(s.slots))
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection handler still waiting on a stalled call after disconnect")
	}
	if n := len(s.slots); n != 0 {
		t.Fatalf("%d stream slots still held after disconnect", n)
	}
}

func TestHelperFwdHooksServerBounds(t *testing.T) {
	s := &helperFwdServer{logf: t.Logf}
	srv := s.hooksServer()
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes != helperFwdMaxHookHeader {
		t.Fatalf("unbounded hooks server: %+v", srv)
	}
}

func TestHelperFwdLimitListener(t *testing.T) {
	inner := &queueListener{conns: make(chan net.Conn, 4)}
	ln := newHelperFwdLimitListener(inner, 1, t.Logf)
	a1, b1 := net.Pipe()
	a2, b2 := net.Pipe()
	a3, b3 := net.Pipe()
	inner.conns <- b1
	inner.conns <- b2
	c1, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// The second is closed at accept; Accept goes on to wait for the next.
	got := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); got <- c }()
	_ = a2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := a2.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection past the cap was not closed")
	}
	// Closing the first frees its place.
	_ = c1.Close()
	inner.conns <- b3
	select {
	case c := <-got:
		if c == nil {
			t.Fatal("no connection after a place freed")
		}
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("no accept after a place freed")
	}
	_ = c1.Close() // a second Close must not free a second place
	for _, c := range []net.Conn{a1, a2, a3} {
		_ = c.Close()
	}
}

type queueListener struct{ conns chan net.Conn }

func (l *queueListener) Accept() (net.Conn, error) {
	c, ok := <-l.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *queueListener) Close() error   { return nil }
func (l *queueListener) Addr() net.Addr { return &net.UnixAddr{Name: "q", Net: "unix"} }

// With every per-connection call slot stalled, one more request is answered
// busy at once, and a disconnect still frees every stream slot.
func TestHelperFwdMCPDisconnectAtCapacity(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { <-release; return 0, "", true }}
	s, _ := newFwdTest(t, host)
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); s.serveMCPConn(context.Background(), server) }()
	rd := bufio.NewReader(client)
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	for i := 1; i <= mcpSocketMaxInFlight; i++ {
		if _, err := client.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(i) + `,"method":"tools/call"}` + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.slots) < mcpSocketMaxInFlight && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(s.slots); n != mcpSocketMaxInFlight {
		t.Fatalf("%d forwards stalled, want %d", n, mcpSocketMaxInFlight)
	}
	if _, err := client.Write([]byte(`{"jsonrpc":"2.0","id":99,"method":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(line, `"id":99`) || !strings.Contains(line, "-32000") {
		t.Fatalf("over-capacity reply %q, %v", line, err)
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still blocked after a disconnect at capacity")
	}
	if n := len(s.slots); n != 0 {
		t.Fatalf("%d stream slots held after disconnect", n)
	}
}

// Hook bodies stay at 1 MiB (8.6a): a larger one is refused with 413 and
// never reaches the host; one at the limit is forwarded.
func TestHelperFwdHookBodyCap(t *testing.T) {
	host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { return 200, `{}`, false }}
	s, alive := newFwdTest(t, host)
	h := s.hooksHandler()
	post := func(n int) int {
		rec := &recordingWriter{h: http.Header{}}
		r, _ := http.NewRequest("POST", "http://x/api/hooks/tool-call", strings.NewReader(strings.Repeat("a", n)))
		r = r.WithContext(context.WithValue(r.Context(), helperFwdAttestKey{}, fwdTestAttestation(alive)))
		h.ServeHTTP(rec, r)
		return rec.code
	}
	if code := post(helperFwdMaxHookRequest + 1); code != http.StatusRequestEntityTooLarge || host.calls.Load() != 0 {
		t.Fatalf("over the cap: %d, host calls %d", code, host.calls.Load())
	}
	if code := post(helperFwdMaxHookRequest); code != 200 || host.calls.Load() != 1 {
		t.Fatalf("at the cap: %d, host calls %d", code, host.calls.Load())
	}
	if _, _, err := s.forward(context.Background(), fwdTestAttestation(alive), "/api/hooks/tool-call", make([]byte, helperFwdMaxHookRequest+1), time.Second); !errors.Is(err, errHelperFwdTooBig) {
		t.Fatalf("forward of an oversized hook: %v", err)
	}
}
