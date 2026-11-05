package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"
)

// `solud mcp-bridge --socket <path>` connects an agent's stdio MCP client to
// the daemon's MCP socket (mcp_socket.go). It forwards each line as it is in
// both directions. It holds no token, decides nothing about identity (the
// daemon reads the bridge process's credentials from the kernel, and the
// bridge runs inside the agent's pane), and never falls back to HTTP.
//
// When the daemon restarts (an update), the connection closes. The bridge
// then:
//   - answers every call still waiting for a reply with a JSON-RPC error and
//     never sends it again, so a call such as spawn_pane can't run twice;
//   - connects again to the same absolute path on the next message, for up to
//     mcpBridgeRedialWindow with backoff, checking before each dial that the
//     socket's directory is still this user's, mode 0700 and not a symlink;
//     each connection is a fresh accept that the daemon identifies anew;
//   - exits with an error on stderr if it can't, which the agent shows.
// The daemon keeps no MCP session state, so a new connection needs no
// initialize. The bridge caches and replays nothing.

const (
	mcpBridgeCommand      = "mcp-bridge"
	mcpBridgeRedialWindow = 30 * time.Second
	// mcpBridgeDrainWait bounds how long the bridge waits for outstanding
	// replies after the client closes stdin.
	mcpBridgeDrainWait = 10 * time.Second
	// mcpBridgeLostCode is the JSON-RPC error a call gets when the connection
	// closed before its reply.
	mcpBridgeLostCode    = -32001
	mcpBridgeLostMessage = "soluterminal: the connection to the daemon closed before this call finished (the daemon may have restarted). It was not sent again; check whether it took effect before calling it again."
)

// errMCPSocketMissing marks a socket path that does not exist yet: worth
// waiting for while the daemon restarts. Any other check failure refuses.
var errMCPSocketMissing = errors.New("mcp socket not present")

type mcpBridge struct {
	path   string
	stdout io.Writer
	stderr io.Writer
	window time.Duration
	check  func(path string) error
	dial   func(path string) (net.Conn, error)
	sleep  func(time.Duration)

	outMu sync.Mutex

	mu      sync.Mutex
	conn    net.Conn
	gen     int
	pending map[string]mcpBridgeCall
	// writing counts settled calls whose reply or error line is still being
	// written; a call counts as done only once its line is out.
	writing int
	drained chan struct{} // closed and replaced whenever nothing is outstanding
}

type mcpBridgeCall struct {
	id  json.RawMessage
	gen int
}

func runMCPBridge(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(mcpBridgeCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "absolute path of the daemon's MCP socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *socket == "" || !filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket {
		fmt.Fprintln(stderr, "soluterminal MCP: --socket must be a clean absolute path")
		return 2
	}
	b := newMCPBridge(*socket, stdout, stderr)
	return b.run(stdin)
}

func newMCPBridge(path string, stdout, stderr io.Writer) *mcpBridge {
	return &mcpBridge{
		path:    path,
		stdout:  stdout,
		stderr:  stderr,
		window:  mcpBridgeRedialWindow,
		check:   mcpBridgeCheckSocket,
		dial:    func(p string) (net.Conn, error) { return net.Dial("unix", p) },
		sleep:   time.Sleep,
		pending: map[string]mcpBridgeCall{},
		drained: make(chan struct{}),
	}
}

func (b *mcpBridge) run(stdin io.Reader) int {
	if _, _, err := b.connect(); err != nil {
		fmt.Fprintf(b.stderr, "soluterminal MCP: can't reach the daemon at %s: %v\n", b.path, err)
		return 1
	}
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64*1024), mcpSocketMaxMessage)
	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ids := mcpBridgeRequestIDs(line)
		conn, gen := b.current()
		if conn == nil {
			var err error
			if conn, gen, err = b.connect(); err != nil {
				for _, id := range ids {
					b.writeLost(id)
				}
				fmt.Fprintf(b.stderr, "soluterminal MCP: lost the daemon at %s and couldn't reconnect within %s: %v\n", b.path, b.window, err)
				return 1
			}
		}
		b.track(ids, gen)
		msg := append(append([]byte(nil), line...), '\n')
		if _, err := conn.Write(msg); err != nil {
			// Possibly delivered in part: a call in flight, never resent.
			b.lost(gen)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(b.stderr, "soluterminal MCP: reading from the agent: %v\n", err)
	}
	b.waitDrained(mcpBridgeDrainWait)
	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	b.mu.Unlock()
	return 0
}

// connect dials the socket, retrying while it is missing or refusing
// connections, for up to b.window. A path that fails the ownership checks
// refuses at once. It returns the connection it made, which the caller uses
// even if that connection has already been lost (a write then fails).
func (b *mcpBridge) connect() (net.Conn, int, error) {
	deadline := time.Now().Add(b.window)
	backoff := 50 * time.Millisecond
	for {
		err := b.check(b.path)
		if err == nil {
			var c net.Conn
			if c, err = b.dial(b.path); err == nil {
				b.mu.Lock()
				b.gen++
				b.conn = c
				gen := b.gen
				b.mu.Unlock()
				go b.readReplies(c, gen)
				return c, gen, nil
			}
		} else if !errors.Is(err, errMCPSocketMissing) {
			return nil, 0, err
		}
		if !time.Now().Add(backoff).Before(deadline) {
			return nil, 0, err
		}
		b.sleep(backoff)
		if backoff < time.Second {
			backoff *= 2
		}
	}
}

func (b *mcpBridge) current() (net.Conn, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn, b.gen
}

func (b *mcpBridge) track(ids []json.RawMessage, gen int) {
	if len(ids) == 0 {
		return
	}
	b.mu.Lock()
	for _, id := range ids {
		b.pending[string(id)] = mcpBridgeCall{id: id, gen: gen}
	}
	b.mu.Unlock()
}

// readReplies copies the daemon's lines to stdout, settling each reply's call.
func (b *mcpBridge) readReplies(c net.Conn, gen int) {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), mcpSocketMaxMessage)
	for sc.Scan() {
		line := sc.Bytes()
		ids := mcpBridgeResponseIDs(line)
		// Claim the calls first, so lost() can't also answer them, and
		// count them as outstanding until the line is written.
		b.mu.Lock()
		claimed := 0
		for _, id := range ids {
			if _, ok := b.pending[string(id)]; ok {
				delete(b.pending, string(id))
				claimed++
			}
		}
		b.writing += claimed
		b.mu.Unlock()
		b.writeLine(line)
		b.doneWriting(claimed)
	}
	b.lost(gen)
}

// lost ends connection gen: every call still waiting on it gets an error
// reply. A later connection's calls are left alone.
func (b *mcpBridge) lost(gen int) {
	b.mu.Lock()
	if b.gen == gen && b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	var failed []json.RawMessage
	for key, call := range b.pending {
		if call.gen == gen {
			failed = append(failed, call.id)
			delete(b.pending, key)
		}
	}
	b.writing += len(failed)
	b.mu.Unlock()
	for _, id := range failed {
		b.writeLost(id)
	}
	b.doneWriting(len(failed))
}

// doneWriting marks n settled calls' lines as written.
func (b *mcpBridge) doneWriting(n int) {
	b.mu.Lock()
	b.writing -= n
	if len(b.pending) == 0 && b.writing == 0 {
		close(b.drained)
		b.drained = make(chan struct{})
	}
	b.mu.Unlock()
}

func (b *mcpBridge) waitDrained(limit time.Duration) {
	b.mu.Lock()
	if len(b.pending) == 0 && b.writing == 0 {
		b.mu.Unlock()
		return
	}
	ch := b.drained
	b.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(limit):
	}
}

func (b *mcpBridge) writeLost(id json.RawMessage) {
	out, err := json.Marshal(rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: mcpBridgeLostCode, Message: mcpBridgeLostMessage}})
	if err == nil {
		b.writeLine(out)
	}
}

func (b *mcpBridge) writeLine(line []byte) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.stdout.Write(append(append([]byte(nil), line...), '\n'))
}

// mcpBridgeRequestIDs returns the ids of the requests in one framed message
// (a single object or a batch): messages with a method and a non-null id.
// It reads nothing else.
func mcpBridgeRequestIDs(line []byte) []json.RawMessage {
	var out []json.RawMessage
	for _, m := range mcpBridgeMessages(line) {
		if m.Method != "" && mcpBridgeHasID(m.ID) {
			out = append(out, m.ID)
		}
	}
	return out
}

// mcpBridgeResponseIDs returns the ids the daemon answered in one message.
func mcpBridgeResponseIDs(line []byte) []json.RawMessage {
	var out []json.RawMessage
	for _, m := range mcpBridgeMessages(line) {
		if m.Method == "" && mcpBridgeHasID(m.ID) {
			out = append(out, m.ID)
		}
	}
	return out
}

type mcpBridgeEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func mcpBridgeMessages(line []byte) []mcpBridgeEnvelope {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []mcpBridgeEnvelope
		if json.Unmarshal(trimmed, &batch) == nil {
			return batch
		}
		return nil
	}
	var one mcpBridgeEnvelope
	if json.Unmarshal(trimmed, &one) != nil {
		return nil
	}
	return []mcpBridgeEnvelope{one}
}

func mcpBridgeHasID(id json.RawMessage) bool {
	t := bytes.TrimSpace(id)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}
