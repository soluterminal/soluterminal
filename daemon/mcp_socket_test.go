//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortSocketDir is a data directory short enough for sun_path (t.TempDir
// paths can pass macOS's 104 bytes).
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func stubSocketEUID(t *testing.T, uid int) {
	t.Helper()
	prev := mcpSocketEUID
	mcpSocketEUID = func() int { return uid }
	t.Cleanup(func() { mcpSocketEUID = prev })
}

// stubSocketPeer makes every connection come from uid/pid with the start
// time *start (changeable mid-test).
func stubSocketPeer(t *testing.T, uid, pid int, start *uint64) {
	t.Helper()
	prevCred, prevStart := mcpSocketPeerCred, mcpProcessStartTime
	mcpSocketPeerCred = func(net.Conn) (int, int, error) { return uid, pid, nil }
	mcpProcessStartTime = func(p int) (uint64, bool) {
		if p != pid {
			return 0, false
		}
		return *start, *start != 0
	}
	t.Cleanup(func() { mcpSocketPeerCred, mcpProcessStartTime = prevCred, prevStart })
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func TestMCPSocketDirectoryRefusals(t *testing.T) {
	t.Run("wrong mode", func(t *testing.T) {
		data := shortSocketDir(t)
		run := filepath.Join(data, mcpSocketDirName)
		if err := os.Mkdir(run, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(run, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := bindMCPSocket(data); err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("bind = %v, want a mode refusal", err)
		}
		if got := modeOf(t, run); got != 0o755 {
			t.Fatalf("directory mode changed to %v", got)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		data := shortSocketDir(t)
		target := filepath.Join(data, "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(data, mcpSocketDirName)); err != nil {
			t.Fatal(err)
		}
		if _, err := bindMCPSocket(data); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("bind = %v, want a symlink refusal", err)
		}
	})
	t.Run("not a directory", func(t *testing.T) {
		data := shortSocketDir(t)
		if err := os.WriteFile(filepath.Join(data, mcpSocketDirName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := bindMCPSocket(data); err == nil {
			t.Fatal("bind accepted a file as the directory")
		}
	})
	t.Run("wrong owner", func(t *testing.T) {
		data := shortSocketDir(t)
		stubSocketEUID(t, os.Geteuid()+1)
		if _, err := bindMCPSocket(data); err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Fatalf("bind = %v, want an owner refusal", err)
		}
	})
	t.Run("filesystem", func(t *testing.T) {
		data := shortSocketDir(t)
		prev := mcpSocketFilesystemOK
		mcpSocketFilesystemOK = func(string) error { return fs.ErrPermission }
		t.Cleanup(func() { mcpSocketFilesystemOK = prev })
		if _, err := bindMCPSocket(data); err == nil {
			t.Fatal("bind accepted a refused filesystem")
		}
	})
}

func TestMCPSocketBindAndPublishModes(t *testing.T) {
	data := shortSocketDir(t)
	e, err := bindMCPSocket(data)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	if got := modeOf(t, e.dir); got != 0o700 {
		t.Fatalf("dir mode %v", got)
	}
	if got := modeOf(t, e.tmp); got != 0o600 {
		t.Fatalf("bound socket mode %v", got)
	}
	if _, err := os.Lstat(e.path); err == nil {
		t.Fatal("published before activate")
	}
	// A leftover socket of ours at the published name is replaced; a
	// leftover per-process name of a dead daemon is swept.
	old, err := net.Listen("unix", e.path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	dead := filepath.Join(e.dir, ".mcp-999999999.sock")
	deadLn, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	deadLn.(*net.UnixListener).SetUnlinkOnClose(false)
	deadLn.Close()
	if err := e.activate(); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(t, e.path); got != 0o600 {
		t.Fatalf("published socket mode %v", got)
	}
	if _, err := os.Lstat(e.tmp); err == nil {
		t.Fatal("per-process name still present")
	}
	if _, err := os.Lstat(dead); err == nil {
		t.Fatal("dead daemon's socket not swept")
	}
}

func TestMCPSocketNeverReplacesForeignNames(t *testing.T) {
	t.Run("regular file", func(t *testing.T) {
		data := shortSocketDir(t)
		e, err := bindMCPSocket(data)
		if err != nil {
			t.Fatal(err)
		}
		defer e.close()
		if err := os.WriteFile(e.path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.activate(); err == nil {
			t.Fatal("activate replaced a regular file")
		}
		if b, _ := os.ReadFile(e.path); string(b) != "keep" {
			t.Fatal("file changed")
		}
	})
	t.Run("another user's socket", func(t *testing.T) {
		data := shortSocketDir(t)
		e, err := bindMCPSocket(data)
		if err != nil {
			t.Fatal(err)
		}
		defer e.close()
		other, err := net.Listen("unix", e.path)
		if err != nil {
			t.Fatal(err)
		}
		other.(*net.UnixListener).SetUnlinkOnClose(false)
		other.Close()
		// As far as the check can tell, the existing socket is someone
		// else's: neither unlinked at startup nor replaced.
		stubSocketEUID(t, os.Geteuid()+1)
		if err := removeOwnStaleSocket(e.path); err == nil {
			t.Fatal("removed another user's socket")
		}
		if err := e.activate(); err == nil {
			t.Fatal("replaced another user's socket")
		}
		if fi, err := os.Lstat(e.path); err != nil || fi.Mode()&fs.ModeSocket == 0 {
			t.Fatalf("socket gone: %v", err)
		}
	})
}

// socketConn runs serveMCPSocketConn on one end of a pipe and returns the
// other end with a line reader.
func socketConn(t *testing.T, a *apiServer) (net.Conn, *bufio.Reader, chan struct{}) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		a.serveMCPSocketConn(server)
		close(done)
	}()
	t.Cleanup(func() { client.Close(); <-done })
	return client, bufio.NewReader(client), done
}

func sendLine(t *testing.T, c net.Conn, msg any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// sendLineAsync writes without failing the test: on a connection the server
// is about to close, the write may legitimately fail.
func sendLineAsync(c net.Conn, msg any) {
	b, _ := json.Marshal(msg)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write(append(b, '\n'))
}

func readReply(t *testing.T, c net.Conn, r *bufio.Reader) (map[string]any, error) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("reply %q: %v", line, err)
	}
	return m, nil
}

func toolCall(id int, name string, args map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
}

func replyText(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	return mcpResultText(t, m["result"])
}

func TestMCPSocketPeerOfAnotherUserClosed(t *testing.T) {
	a := confirmTestServer(t)
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid()+1, 999991, &start)
	c, r, done := socketConn(t, a)
	go sendLineAsync(c, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if m, err := readReply(t, c, r); err == nil {
		t.Fatalf("another user's peer got a reply: %v", m)
	}
	<-done
}

func TestMCPSocketPIDReuseDropsConnection(t *testing.T) {
	a := confirmTestServer(t)
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999991, &start)
	c, r, done := socketConn(t, a)
	sendLine(t, c, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	if _, err := readReply(t, c, r); err != nil {
		t.Fatalf("first message: %v", err)
	}
	start = 43 // the pid now names another process
	go sendLineAsync(c, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"})
	if m, err := readReply(t, c, r); err == nil {
		t.Fatalf("reused pid got a reply: %v", m)
	}
	<-done
}

// The pane comes from the peer's process ancestry; anything the client puts
// in a message (a header-like _meta, a pane id in the arguments) is not
// identity.
func TestMCPSocketPaneFromAncestryOnly(t *testing.T) {
	const inLead, outside = 999991, 999992
	for _, tc := range []struct {
		name    string
		pid     int
		wantYou string
	}{
		{"pid inside pane lead", inLead, "lead"},
		{"pid outside panes", outside, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := confirmTestServer(t)
			a.mgr.panes["lead"].Pid = inLead
			start := uint64(42)
			stubSocketPeer(t, os.Geteuid(), tc.pid, &start)
			if id := a.classifyMCPSocketPeer(tc.pid); (id.PaneID == "lead") != (tc.wantYou == "lead") || id.Kind == callerHuman || id.Relay {
				t.Fatalf("classified %+v", id)
			}
			c, r, _ := socketConn(t, a)
			call := toolCall(1, "list_panes", map[string]any{})
			call["params"].(map[string]any)["_meta"] = map[string]any{"X-Soluterminal-Pane": "lead", "pane": "lead"}
			sendLine(t, c, call)
			m, err := readReply(t, c, r)
			if err != nil {
				t.Fatal(err)
			}
			text, failed := replyText(t, m)
			if failed {
				t.Fatalf("list_panes: %s", text)
			}
			var panes []struct {
				ID  string `json:"id"`
				You bool   `json:"you"`
			}
			if err := json.Unmarshal([]byte(text), &panes); err != nil {
				var wrapped struct {
					Panes []struct {
						ID  string `json:"id"`
						You bool   `json:"you"`
					} `json:"panes"`
				}
				if err := json.Unmarshal([]byte(text), &wrapped); err != nil {
					t.Fatalf("list_panes result %q", text)
				}
				panes = wrapped.Panes
			}
			you := ""
			for _, p := range panes {
				if p.You {
					you = p.ID
				}
			}
			if you != tc.wantYou {
				t.Fatalf("you = %q, want %q", you, tc.wantYou)
			}
			// No pane, no default to one: set_pane_info needs pane_id.
			sendLine(t, c, toolCall(2, "set_pane_info", map[string]any{"working_on": "x"}))
			m, err = readReply(t, c, r)
			if err != nil {
				t.Fatal(err)
			}
			text, failed = replyText(t, m)
			if tc.wantYou == "" && (!failed || !strings.Contains(text, "pane_id is required")) {
				t.Fatalf("set_pane_info from outside panes: %q", text)
			}
		})
	}
}

// A socket caller is never the person, even when its process is a browser.
func TestMCPSocketCallerIsNeverHuman(t *testing.T) {
	a := confirmTestServer(t)
	prev := peerIsBrowser
	peerIsBrowser = func(int) bool { return true }
	t.Cleanup(func() { peerIsBrowser = prev })
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999992, &start)
	if id := a.classifyMCPSocketPeer(999992); id.Kind != callerUnverified || id.Relay || id.Unproven {
		t.Fatalf("classified %+v", id)
	}
	c, r, _ := socketConn(t, a)
	sendLine(t, c, toolCall(1, "answer_prompt", map[string]any{"pane_id": "lead", "choice": "1"}))
	m, err := readReply(t, c, r)
	if err != nil {
		t.Fatal(err)
	}
	if text, failed := replyText(t, m); !failed || !strings.Contains(text, "can't answer prompts") {
		t.Fatalf("answer_prompt from a socket caller: %q", text)
	}
}

// The socket speaks MCP framing only: an HTTP request gets a JSON-RPC parse
// error, not an HTTP route.
func TestMCPSocketIsNotHTTP(t *testing.T) {
	a := confirmTestServer(t)
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999992, &start)
	c, r, _ := socketConn(t, a)
	if _, err := io.WriteString(c, "GET /api/health HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	m, err := readReply(t, c, r)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := m["error"].(map[string]any); e == nil || e["code"] != float64(-32700) {
		t.Fatalf("reply = %v", m)
	}
}

func TestMCPSocketFramingNotificationsAndConcurrency(t *testing.T) {
	a := confirmTestServer(t)
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999992, &start)
	c, r, _ := socketConn(t, a)
	// A notification gets no reply; the two requests after it each get one.
	sendLine(t, c, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	sendLine(t, c, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "initialize"})
	sendLine(t, c, map[string]any{"jsonrpc": "2.0", "id": 8, "method": "tools/list"})
	seen := map[float64]bool{}
	for i := 0; i < 2; i++ {
		m, err := readReply(t, c, r)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := m["id"].(float64)
		seen[id] = true
	}
	if !seen[7] || !seen[8] {
		t.Fatalf("replies for %v", seen)
	}
}

func TestMCPSocketOversizedMessageCloses(t *testing.T) {
	a := confirmTestServer(t)
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999992, &start)
	c, r, done := socketConn(t, a)
	go func() {
		_, _ = c.Write([]byte(strings.Repeat("x", mcpSocketMaxMessage+1)))
	}()
	if m, err := readReply(t, c, r); err == nil {
		t.Fatalf("oversized message got %v", m)
	}
	<-done
}

// End to end over a real socket: the kernel's credentials, not a stub.
func TestMCPSocketRealPeerCredentials(t *testing.T) {
	data := shortSocketDir(t)
	e, err := bindMCPSocket(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.activate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := confirmTestServer(t)
	go e.serve(ctx, a)

	uid, pid, err := func() (int, int, error) {
		ln, err := net.Listen("unix", filepath.Join(data, "probe.sock"))
		if err != nil {
			return 0, 0, err
		}
		defer ln.Close()
		go func() {
			if c, err := net.Dial("unix", filepath.Join(data, "probe.sock")); err == nil {
				defer c.Close()
				time.Sleep(200 * time.Millisecond)
			}
		}()
		c, err := ln.Accept()
		if err != nil {
			return 0, 0, err
		}
		defer c.Close()
		return mcpSocketPeerCredPlatform(c)
	}()
	if err != nil || uid != os.Geteuid() || pid != os.Getpid() {
		t.Fatalf("peer cred = uid %d pid %d err %v, want %d %d", uid, pid, err, os.Geteuid(), os.Getpid())
	}
	if s1, ok := mcpProcessStartTimePlatform(os.Getpid()); !ok {
		t.Fatal("own start time unreadable")
	} else if s2, _ := mcpProcessStartTimePlatform(os.Getpid()); s1 != s2 {
		t.Fatalf("start time unstable: %d then %d", s1, s2)
	}

	// This test process plays the daemon, so by the kernel's own account it
	// is the daemon's process: refused.
	c, err := net.Dial("unix", e.path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	go sendLineAsync(c, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if m, err := readReply(t, c, r); err == nil {
		t.Fatalf("the daemon's own process got %v", m)
	}

	// A process outside the daemon's tree (orphaned, so its parent is not
	// this process) and outside every pane is served as an unverified
	// caller.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for an out-of-tree client")
	}
	out := filepath.Join(data, "reply.json")
	client := `import json,socket,sys
s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); f=s.makefile("rwb")
f.write(json.dumps({"jsonrpc":"2.0","id":1,"method":"initialize"}).encode()+b"\n"); f.flush()
open(sys.argv[2],"w").write(f.readline().decode())`
	cmd := exec.Command("sh", "-c", `"$0" -I -c "$1" "$2" "$3" & exit 0`, python, client, e.path, out)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("reply %q", b)
			}
			if res, _ := m["result"].(map[string]any); res == nil || res["serverInfo"] == nil {
				t.Fatalf("initialize = %v", m)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("out-of-tree client got no reply")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A pane still on the old HTTP URL is judged exactly as before (an untraced
// call with its header is a claim), while the same pane's call over the socket
// is traced to it.
func TestMCPOldURLPaneVersusSocketPane(t *testing.T) {
	a := confirmTestServer(t)
	a.gate = &authGate{token: "fixture-token"}
	a.mgr.panes["lead"].Pid = 999991
	text, failed := mcpAs(t, a, nil, "lead", "list_panes", map[string]any{})
	if failed || strings.Contains(text, `"you":true`) {
		t.Fatalf("old-URL claim: %q", text)
	}
	if text, failed := mcpAs(t, a, nil, "lead", "set_pane_info", map[string]any{"working_on": "x"}); !failed || !strings.Contains(text, "pane_id is required") {
		t.Fatalf("old-URL claim set_pane_info: %q", text)
	}
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), 999991, &start)
	c, r, _ := socketConn(t, a)
	sendLine(t, c, toolCall(1, "list_panes", map[string]any{}))
	m, err := readReply(t, c, r)
	if err != nil {
		t.Fatal(err)
	}
	if text, failed := replyText(t, m); failed || !strings.Contains(text, `"you":true`) {
		t.Fatalf("socket pane: %q", text)
	}
}

// A process the daemon started (a managed agent with a scoped HTTP session)
// can't use the socket to step outside its tool budget.
func TestMCPSocketRefusesDaemonStartedProcesses(t *testing.T) {
	a := confirmTestServer(t)
	start := uint64(42)
	// The daemon itself, and so anything it spawned, classifies as a worker.
	stubSocketPeer(t, os.Geteuid(), os.Getpid(), &start)
	if id := a.classifyMCPSocketPeer(os.Getpid()); id.Kind != callerWorker {
		t.Fatalf("classified %+v", id)
	}
	c, r, done := socketConn(t, a)
	go sendLineAsync(c, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if m, err := readReply(t, c, r); err == nil {
		t.Fatalf("worker got a reply: %v", m)
	}
	<-done
}

// The pinned process is looked up at accept, before a message is read,
// before it runs and before its reply is written. Once it is gone, nothing
// more runs and nothing more is written.
func TestMCPSocketPeerGoneBeforeDispatchOrReply(t *testing.T) {
	for _, tc := range []struct {
		name     string
		goneFrom int // the nth start-time lookup that finds another process
	}{
		{"queued call", 3},
		{"reply", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := confirmTestServer(t)
			prevCred, prevStart := mcpSocketPeerCred, mcpProcessStartTime
			t.Cleanup(func() { mcpSocketPeerCred, mcpProcessStartTime = prevCred, prevStart })
			mcpSocketPeerCred = func(net.Conn) (int, int, error) { return os.Geteuid(), 999992, nil }
			var lookups atomic.Int32
			mcpProcessStartTime = func(int) (uint64, bool) {
				if int(lookups.Add(1)) >= tc.goneFrom {
					return 43, true
				}
				return 42, true
			}
			c, r, done := socketConn(t, a)
			go sendLineAsync(c, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
			if m, err := readReply(t, c, r); err == nil {
				t.Fatalf("reply after the peer was gone: %v", m)
			}
			<-done
			if got := int(lookups.Load()); got != tc.goneFrom {
				t.Fatalf("start time looked up %d times, want %d", got, tc.goneFrom)
			}
		})
	}
}

// A process the daemon started stays the daemon's even when its environment
// names a pane (the SOLU_PANE_ID fallback), so it can't use the socket to
// shed a scoped session's budget.
func TestMCPSocketRefusesDaemonChildClaimingAPane(t *testing.T) {
	a := confirmTestServer(t)
	child := startSleeper(t, "SOLU_PANE_ID=lead")
	if id, known := a.classifyPeerProcess(child.Process.Pid); !known || id.Kind != callerPane || !id.FromEnv {
		t.Fatalf("fixture: shared classifier gave %+v (known %t), want an env claim", id, known)
	}
	if id := a.classifyMCPSocketPeer(child.Process.Pid); id.Kind != callerWorker {
		t.Fatalf("socket classified %+v, want the daemon's worker", id)
	}
	start := uint64(42)
	stubSocketPeer(t, os.Geteuid(), child.Process.Pid, &start)
	c, r, done := socketConn(t, a)
	go sendLineAsync(c, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if m, err := readReply(t, c, r); err == nil {
		t.Fatalf("daemon child got a reply: %v", m)
	}
	<-done
}
