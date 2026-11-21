//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	hp "soluterminal/helperproto"
)

// shortFwdDir is a 0700 runtime dir with a 0700 parent and a path short
// enough for sockaddr_un.
func shortFwdDir(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("", "w3f")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	dir := filepath.Join(parent, "r")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Real sockets end to end: the unchanged mcp-bridge and `solud hook-forward`
// reach the host fwd handler through mcp.sock and hooks.sock.
func TestHelperFwdSocketsEndToEnd(t *testing.T) {
	host := &fwdHost{reply: func(r fwdSeen) (int, string, bool) {
		if r.path == "/mcp" {
			return 200, `{"jsonrpc":"2.0","id":9,"result":{"ok":true}}`, false
		}
		return 200, `{}`, false
	}}
	s, _ := newFwdTest(t, host)
	dir := shortFwdDir(t)
	checked := ""
	l, err := listenHelperFwd(dir, func(d string) error { checked = d; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if checked != dir {
		t.Fatalf("runtime dir check ran on %q", checked)
	}
	mcpPath, hooksPath := filepath.Join(dir, helperFwdMCPSocket), filepath.Join(dir, helperFwdHooksSocket)
	for _, p := range []string{mcpPath, hooksPath} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, fi.Mode(), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); l.serve(ctx, s) }()

	// hook-forward: stdout stays empty, exit 0, the body arrives.
	var stderr bytes.Buffer
	if code := runHookForward([]string{"--socket", hooksPath}, strings.NewReader(`{"hook_event_name":"PreToolUse"}`), &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("hook-forward exit %d, stderr %q", code, stderr.String())
	}

	// mcp-bridge, unchanged, pointed at the helper's mcp.sock.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	bridgeDone := make(chan int, 1)
	go func() { bridgeDone <- newMCPBridge(mcpPath, outW, io.Discard).run(inR); _ = outW.Close() }()
	if _, err := inW.Write([]byte(`{"jsonrpc":"2.0","id":9,"method":"tools/list"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := readLineWithin(outR, 5*time.Second)
	if err != nil || !strings.Contains(line, `"ok":true`) {
		t.Fatalf("bridge reply %q, %v", line, err)
	}
	_ = inW.Close()
	<-bridgeDone

	seen := host.all()
	paths := map[string]string{}
	for _, r := range seen {
		paths[r.path] = r.body
		if r.header.Pane != testAttestPaneID {
			t.Errorf("pane %q", r.header.Pane)
		}
	}
	if paths["/api/hooks/tool-call"] != `{"hook_event_name":"PreToolUse"}` || paths["/mcp"] == "" {
		t.Fatalf("host saw %v", paths)
	}

	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
	for _, p := range []string{mcpPath, hooksPath} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("runtime dir not clean: %v", ents)
	}
}

func TestHelperFwdListenRefusesABadDir(t *testing.T) {
	dir := shortFwdDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := listenHelperFwd(dir, func(string) error { return nil }); err == nil {
		t.Fatal("bound in a 0755 dir")
	}
	link := filepath.Join(filepath.Dir(dir), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := listenHelperFwd(link, func(string) error { return nil }); err == nil {
		t.Fatal("bound through a symlink")
	}
}

// With no helper running, the hook neither fails nor prints.
func TestHookForwardWithoutHelperIsSilent(t *testing.T) {
	var stderr bytes.Buffer
	code := runHookForward([]string{"--socket", filepath.Join(shortFwdDir(t), "hooks.sock")}, strings.NewReader("{}"), &stderr)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func readLineWithin(r io.Reader, d time.Duration) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := bufio.NewReader(r).ReadString('\n')
		ch <- res{s, err}
	}()
	select {
	case v := <-ch:
		return v.s, v.err
	case <-time.After(d):
		return "", http.ErrHandlerTimeout
	}
}

// During an update handoff the successor publishes its sockets first; the
// predecessor's close must leave them alone.
func TestHelperFwdCloseLeavesASuccessorsSockets(t *testing.T) {
	dir := shortFwdDir(t)
	noCheck := func(string) error { return nil }
	old, err := listenHelperFwd(dir, noCheck)
	if err != nil {
		t.Fatal(err)
	}
	next, err := listenHelperFwd(dir, noCheck)
	if err != nil {
		t.Fatal(err)
	}
	old.close()
	for _, name := range []string{helperFwdMCPSocket, helperFwdHooksSocket} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !os.SameFile(fi, next.published[name]) {
			t.Fatalf("%s: successor's socket removed or replaced: %v", name, err)
		}
	}
	next.close()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("runtime dir not clean: %v", ents)
	}
}

// The real attestor behind the real listeners, including the hooks
// connection cap: this test process stands in for the agent, and its own
// hook and MCP clients are attested to that agent's pane.
func TestHelperFwdSocketsWithTheRealAttestor(t *testing.T) {
	host := &fwdHost{reply: func(r fwdSeen) (int, string, bool) {
		if r.path == "/mcp" {
			return 200, `{"jsonrpc":"2.0","id":1,"result":{}}`, false
		}
		return 200, `{}`, false
	}}
	s, _ := newFwdTest(t, host)
	self := os.Getpid()
	start, ok := mcpProcessStartTimePlatform(self)
	if !ok {
		t.Fatal("own start time unreadable")
	}
	att := newHelperAttestor(oneAgentIndex{hp.AgentInfo{AgentID: testAttestAgentID, PaneID: testAttestPaneID, Pid: self, StartTicks: start}})
	s.attest, s.valid = att.attestConn, att.stillValid

	dir := shortFwdDir(t)
	l, err := listenHelperFwd(dir, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); l.serve(ctx, s) }()
	t.Cleanup(func() { cancel(); <-served })

	probe, err := net.Dial("unix", filepath.Join(dir, helperFwdMCPSocket))
	if err != nil {
		t.Fatal(err)
	}
	hasPidfd := kernelHasPeerPidfd(t, probe)
	_ = probe.Close()

	err = hookForwardPost(filepath.Join(dir, helperFwdHooksSocket), []byte(`{"hook_event_name":"SessionStart"}`), 5*time.Second)
	if !hasPidfd {
		if err == nil || len(host.all()) != 0 {
			t.Fatalf("forwarded without SO_PEERPIDFD: %v", err)
		}
		t.Skip("kernel lacks SO_PEERPIDFD; refused as designed")
	}
	if err != nil {
		t.Fatalf("hook through the capped listener: %v", err)
	}
	c, err := net.Dial("unix", filepath.Join(dir, helperFwdMCPSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := readLineWithin(c, 5*time.Second); err != nil || !strings.Contains(line, `"result"`) {
		t.Fatalf("mcp reply %q, %v", line, err)
	}
	seen := host.all()
	if len(seen) != 2 {
		t.Fatalf("host saw %d requests", len(seen))
	}
	for _, r := range seen {
		var a hp.FwdAttest
		if err := json.Unmarshal(r.header.Attest, &a); err != nil || r.header.Pane != testAttestPaneID || a.Agent != testAttestAgentID || a.PID != self || a.Start != start {
			t.Fatalf("header %+v attest %+v %v", r.header, a, err)
		}
	}
}

// Publishing waits while another helper holds the runtime dir lock (as a
// predecessor does between its inode check and the unlink).
func TestHelperFwdPublishWaitsForTheDirLock(t *testing.T) {
	dir := shortFwdDir(t)
	noCheck := func(string) error { return nil }
	old, err := listenHelperFwd(dir, noCheck)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockHelperFwdDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		l   *helperFwdListeners
		err error
	}
	got := make(chan res, 1)
	go func() { l, err := listenHelperFwd(dir, noCheck); got <- res{l, err} }()
	select {
	case <-got:
		t.Fatal("a successor published while the dir lock was held")
	case <-time.After(200 * time.Millisecond):
	}
	// The predecessor's own cleanup, done under the lock it would hold.
	old.closeLocked()
	unlock()
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, name := range []string{helperFwdMCPSocket, helperFwdHooksSocket} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !os.SameFile(fi, r.l.published[name]) {
			t.Fatalf("%s: successor's socket missing: %v", name, err)
		}
	}
	r.l.close()
}

// The first hook of a just-started pane, through the real hooks.sock and the
// real attestor: the hook connects before the helper knows its agent, and is
// forwarded once the record appears.
func TestHelperFwdFirstHookBeforeRegistration(t *testing.T) {
	host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { return 200, `{}`, false }}
	s, _ := newFwdTest(t, host)
	self := os.Getpid()
	start, ok := mcpProcessStartTimePlatform(self)
	if !ok {
		t.Fatal("own start time unreadable")
	}
	idx := &lateIndex{ref: hp.AgentInfo{AgentID: testAttestAgentID, PaneID: testAttestPaneID, Pid: self, StartTicks: start}}
	att := newHelperAttestor(idx)
	s.attest, s.valid = att.attestConn, att.stillValid

	dir := shortFwdDir(t)
	l, err := listenHelperFwd(dir, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); l.serve(ctx, s) }()
	t.Cleanup(func() { cancel(); <-served })

	probe, err := net.Dial("unix", filepath.Join(dir, helperFwdMCPSocket))
	if err != nil {
		t.Fatal(err)
	}
	hasPidfd := kernelHasPeerPidfd(t, probe)
	_ = probe.Close()
	if !hasPidfd {
		t.Skip("kernel lacks SO_PEERPIDFD")
	}
	go func() { time.Sleep(300 * time.Millisecond); idx.ready.Store(true) }()
	if err := hookForwardPost(filepath.Join(dir, helperFwdHooksSocket), []byte(`{"hook_event_name":"SessionStart"}`), hookForwardTimeout); err != nil {
		t.Fatalf("first hook: %v", err)
	}
	if n := len(host.all()); n != 1 {
		t.Fatalf("host saw %d requests", n)
	}
}

// Attestation runs per connection, not in the accept loop: a hook whose
// attestation waits (a just-started pane) doesn't hold up another pane's
// hook arriving at the same time.
func TestHelperFwdSlowAttestationDoesNotStallOtherHooks(t *testing.T) {
	host := &fwdHost{reply: func(fwdSeen) (int, string, bool) { return 200, `{}`, false }}
	s, alive := newFwdTest(t, host)
	var n atomic.Int32
	release := make(chan struct{})
	s.attest = func(net.Conn) (helperAttestation, error) {
		if n.Add(1) == 1 {
			<-release // the first connection's agent isn't registered yet
		}
		return fwdTestAttestation(alive), nil
	}
	dir := shortFwdDir(t)
	l, err := listenHelperFwd(dir, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); l.serve(ctx, s) }()
	t.Cleanup(func() { cancel(); <-served })
	sock := filepath.Join(dir, helperFwdHooksSocket)

	slow := make(chan error, 1)
	go func() { slow <- hookForwardPost(sock, []byte(`{"n":1}`), hookForwardTimeout) }()
	for n.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if err := hookForwardPost(sock, []byte(`{"n":2}`), hookForwardTimeout); err != nil {
		t.Fatalf("second hook: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("second hook waited %v behind the first's attestation", d)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("first hook: %v", err)
	}
	if got := len(host.all()); got != 2 {
		t.Fatalf("host saw %d hooks", got)
	}
}
