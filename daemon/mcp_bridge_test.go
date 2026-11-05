//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bridgeDaemon serves the real MCP socket handler on <data>/run/mcp.sock and
// can be killed (listener and every connection closed) and restarted on the
// same path, like a daemon restart.
type bridgeDaemon struct {
	t       *testing.T
	a       *apiServer
	path    string
	mu      sync.Mutex
	ln      net.Listener
	conns   []net.Conn
	accepts atomic.Int32
	serving sync.WaitGroup
}

func newBridgeDaemon(t *testing.T, a *apiServer) *bridgeDaemon {
	t.Helper()
	data := shortSocketDir(t)
	run := filepath.Join(data, mcpSocketDirName)
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	d := &bridgeDaemon{t: t, a: a, path: filepath.Join(run, mcpSocketName)}
	d.start()
	// Stop and wait for every handler before the test's stubs are restored.
	t.Cleanup(func() { d.kill(); d.serving.Wait() })
	return d
}

func (d *bridgeDaemon) start() {
	ln, err := net.Listen("unix", d.path)
	if err != nil {
		d.t.Fatal(err)
	}
	d.mu.Lock()
	d.ln = ln
	d.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.accepts.Add(1)
			d.mu.Lock()
			d.conns = append(d.conns, c)
			d.mu.Unlock()
			d.serving.Add(1)
			go func() {
				defer d.serving.Done()
				d.a.serveMCPSocketConn(c)
			}()
		}
	}()
}

func (d *bridgeDaemon) kill() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ln != nil {
		_ = d.ln.Close()
		d.ln = nil
	}
	for _, c := range d.conns {
		_ = c.Close()
	}
	d.conns = nil
}

// bridgeClient runs a bridge with pipes standing in for the agent.
type bridgeClient struct {
	in     *io.PipeWriter
	out    *bufio.Reader
	stderr *bytes.Buffer
	done   chan int
}

func startBridge(t *testing.T, path string, tune func(*mcpBridge)) *bridgeClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &bridgeClient{in: inW, out: bufio.NewReader(outR), stderr: &bytes.Buffer{}, done: make(chan int, 1)}
	b := newMCPBridge(path, outW, c.stderr)
	b.window = 2 * time.Second
	if tune != nil {
		tune(b)
	}
	go func() {
		c.done <- b.run(inR)
		_ = outW.Close()
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return c
}

func (c *bridgeClient) send(t *testing.T, msg any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		t.Fatalf("write to bridge: %v", err)
	}
}

func (c *bridgeClient) recv(t *testing.T) map[string]any {
	t.Helper()
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := c.out.ReadBytes('\n')
		ch <- res{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read from bridge: %v", r.err)
		}
		var m map[string]any
		if err := json.Unmarshal(r.line, &m); err != nil {
			t.Fatalf("bridge wrote %q: %v", r.line, err)
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no line from the bridge")
	}
	return nil
}

func (c *bridgeClient) exit(t *testing.T) int {
	t.Helper()
	select {
	case code := <-c.done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not exit")
	}
	return -1
}

// The daemon restarts mid-session: the call it was running gets one error
// and is never sent again; the next call goes over a fresh connection, needs
// no new initialize, and is traced to the caller's pane.
func TestMCPBridgeDaemonRestartMidSession(t *testing.T) {
	a, _ := envClaimSpawnServer(t)
	a.mgr.panes["lead"].Pid = 999991
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999991, &start)
	var spawns atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	a.spawnPane = func(opts SpawnOpts) (*Pane, error) {
		if spawns.Add(1) == 1 {
			close(entered)
		}
		<-release
		p := transactionTestPane("pane_from_bridge")
		p.Origin = opts.Origin.clone()
		return p, nil
	}
	d := newBridgeDaemon(t, a)
	c := startBridge(t, d.path, nil)

	c.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if m := c.recv(t); m["id"] != float64(1) || m["result"] == nil {
		t.Fatalf("initialize = %v", m)
	}

	c.send(t, toolCall(2, "spawn_pane", map[string]any{"cmd": "bash", "title": "w", "wait_ready": false}))
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("spawn_pane never reached the daemon")
	}
	d.kill()
	m := c.recv(t)
	if e, _ := m["error"].(map[string]any); m["id"] != float64(2) || e == nil || e["code"] != float64(mcpBridgeLostCode) {
		t.Fatalf("in-flight spawn_pane got %v", m)
	}
	close(release)

	d.start()
	c.send(t, toolCall(3, "list_panes", map[string]any{}))
	m = c.recv(t)
	text, failed := mcpResultText(t, m["result"])
	if m["id"] != float64(3) || failed || !strings.Contains(text, `"you":true`) {
		t.Fatalf("after the restart: %v", m)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("spawn_pane ran %d times", got)
	}
	if got := d.accepts.Load(); got != 2 {
		t.Fatalf("%d connections accepted, want 2", got)
	}
	_ = c.in.Close()
	if code := c.exit(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// No daemon back within the window: the waiting call gets an error and the
// bridge exits with the reason on stderr.
func TestMCPBridgeGivesUpAfterTheWindow(t *testing.T) {
	a := confirmTestServer(t)
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999992, &start)
	d := newBridgeDaemon(t, a)
	c := startBridge(t, d.path, func(b *mcpBridge) { b.window = 300 * time.Millisecond })
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	c.recv(t)
	d.kill()
	time.Sleep(50 * time.Millisecond)
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": "x", "method": "ping"})
	if m := c.recv(t); m["id"] != "x" || m["error"] == nil {
		t.Fatalf("got %v", m)
	}
	if code := c.exit(t); code != 1 || !strings.Contains(c.stderr.String(), "couldn't reconnect") {
		t.Fatalf("exit %d, stderr %q", code, c.stderr.String())
	}
}

// Before every dial the bridge checks the socket's directory; an unsafe one
// is refused without dialing, and nothing falls back to HTTP.
func TestMCPBridgeRefusesAnUnsafeDirectory(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, data string) string{
		"group-readable run dir": func(t *testing.T, data string) string {
			run := filepath.Join(data, "run")
			if err := os.Mkdir(run, 0o750); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(run, 0o750)
			return filepath.Join(run, mcpSocketName)
		},
		"symlinked run dir": func(t *testing.T, data string) string {
			real := filepath.Join(data, "real")
			if err := os.Mkdir(real, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(data, "run")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(data, "run", mcpSocketName)
		},
		"world-writable data dir": func(t *testing.T, data string) string {
			_ = os.Chmod(data, 0o777)
			run := filepath.Join(data, "run")
			if err := os.Mkdir(run, 0o700); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(run, mcpSocketName)
		},
		"not a socket": func(t *testing.T, data string) string {
			run := filepath.Join(data, "run")
			if err := os.Mkdir(run, 0o700); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(run, mcpSocketName)
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := setup(t, shortSocketDir(t))
			var dials atomic.Int32
			var stderr bytes.Buffer
			b := newMCPBridge(path, io.Discard, &stderr)
			b.dial = func(p string) (net.Conn, error) { dials.Add(1); return net.Dial("unix", p) }
			if code := b.run(strings.NewReader("")); code != 1 || dials.Load() != 0 {
				t.Fatalf("exit %d after %d dials; stderr %q", code, dials.Load(), stderr.String())
			}
		})
	}
}

func TestMCPBridgeArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--socket", "run/mcp.sock"}, {"--socket", "/a/../b/mcp.sock"}} {
		if code := runMCPBridge(args, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestMCPBridgeMessageIDs(t *testing.T) {
	for _, tc := range []struct {
		line      string
		req, resp int
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, 1, 0},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 0, 0},
		{`{"jsonrpc":"2.0","id":null,"method":"x"}`, 0, 0},
		{`{"jsonrpc":"2.0","id":"a","result":{}}`, 0, 1},
		{`[{"id":1,"method":"a"},{"id":2,"method":"b"},{"method":"n"}]`, 2, 0},
		{`not json`, 0, 0},
	} {
		if got := len(mcpBridgeRequestIDs([]byte(tc.line))); got != tc.req {
			t.Errorf("%s: %d requests", tc.line, got)
		}
		if got := len(mcpBridgeResponseIDs([]byte(tc.line))); got != tc.resp {
			t.Errorf("%s: %d responses", tc.line, got)
		}
	}
}

// fakeSocketDaemon serves <data>/run/mcp.sock with handle for each connection.
func fakeSocketDaemon(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	data := shortSocketDir(t)
	run := filepath.Join(data, mcpSocketDirName)
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(run, mcpSocketName)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); handle(c) }()
		}
	}()
	return path
}

// readAll collects every line the bridge writes until it exits.
func (c *bridgeClient) readAll(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for {
		line, err := c.out.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var m map[string]any
			if json.Unmarshal(line, &m) != nil {
				t.Fatalf("bridge wrote %q", line)
			}
			out = append(out, m)
		}
		if err != nil {
			return out
		}
	}
}

// A connection that drops the moment it is made: the call gets an error
// reply, never a crash on a connection that is already gone.
func TestMCPBridgeConnectionLostRightAway(t *testing.T) {
	path := fakeSocketDaemon(t, func(c net.Conn) { c.Close() })
	c := startBridge(t, path, func(b *mcpBridge) { b.window = 300 * time.Millisecond })
	// Read as a real client does, while it is still sending.
	collected := make(chan []map[string]any, 1)
	go func() { collected <- c.readAll(t) }()
	for i := 1; i <= 5; i++ {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i, "method": "ping"})
		if _, err := c.in.Write(append(b, '\n')); err != nil {
			break // the bridge gave up and exited
		}
	}
	_ = c.in.Close()
	code := c.exit(t)
	lines := <-collected
	got := map[float64]bool{}
	for _, m := range lines {
		id, _ := m["id"].(float64)
		if m["error"] == nil {
			t.Fatalf("expected only errors, got %v", m)
		}
		got[id] = true
	}
	if code != 0 && code != 1 {
		t.Fatalf("exit %d", code)
	}
	// Every call the bridge sent got its error; a call it couldn't send
	// (no daemon within the window) got one too before the bridge gave up.
	if len(got) == 0 {
		t.Fatalf("no replies: %v (stderr %q)", lines, c.stderr.String())
	}
}

// The last reply, and the error for a call lost after stdin closed, reach
// stdout before the bridge exits.
func TestMCPBridgeWritesTheLastLineBeforeExiting(t *testing.T) {
	t.Run("delayed reply", func(t *testing.T) {
		path := fakeSocketDaemon(t, func(c net.Conn) {
			defer c.Close()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				var m map[string]any
				_ = json.Unmarshal(sc.Bytes(), &m)
				time.Sleep(200 * time.Millisecond)
				out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{}})
				_, _ = c.Write(append(out, '\n'))
			}
		})
		c := startBridge(t, path, nil)
		c.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "ping"})
		_ = c.in.Close()
		lines := c.readAll(t)
		if c.exit(t) != 0 || len(lines) != 1 || lines[0]["id"] != float64(9) || lines[0]["result"] == nil {
			t.Fatalf("lines %v", lines)
		}
	})
	t.Run("lost call", func(t *testing.T) {
		path := fakeSocketDaemon(t, func(c net.Conn) {
			bufio.NewReader(c).ReadBytes('\n')
			time.Sleep(200 * time.Millisecond)
			c.Close()
		})
		c := startBridge(t, path, nil)
		c.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "ping"})
		_ = c.in.Close()
		lines := c.readAll(t)
		if c.exit(t) != 0 || len(lines) != 1 || lines[0]["id"] != float64(9) || lines[0]["error"] == nil {
			t.Fatalf("lines %v", lines)
		}
	})
}
