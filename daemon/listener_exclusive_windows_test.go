//go:build windows

package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The update successor retries its bind for 5 s (listenWithUpdateRetry).
// SO_EXCLUSIVEADDRUSE can keep a port unbindable while connections the old
// listener accepted are still active, so every rebind below must succeed
// inside that window, or exclusivity would break self-update.
const exclusiveRebindWindow = 5 * time.Second

func init() {
	if os.Getenv("SOLU_EXCL_LISTEN_CHILD") != "1" {
		return
	}
	// Child: the "predecessor". Listen exclusively, report the address,
	// serve one connection, then exit with that connection still open.
	ln, err := listenDaemon("tcp", "127.0.0.1:0")
	if err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(2)
	}
	os.Stdout.WriteString("ADDR " + ln.Addr().String() + "\n")
	conn, err := ln.Accept()
	if err != nil {
		os.Exit(3)
	}
	_, _ = conn.Write([]byte("hi\n"))
	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
}

func rebindWithin(t *testing.T, addr string, window time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	var lastErr error
	for time.Since(start) < window {
		ln, err := listenDaemon("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return time.Since(start)
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("could not rebind %s exclusively within %s: %v", addr, window, lastErr)
	return 0
}

func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

// Nothing else can bind the daemon's address while it listens, not even a
// socket that asks for SO_REUSEADDR (the takeover ST-233 closes).
func TestExclusiveListenerRefusesSharing(t *testing.T) {
	ln, err := listenDaemon("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if other, err := net.Listen("tcp", addr); err == nil {
		other.Close()
		t.Fatalf("a plain listener bound %s while the daemon held it", addr)
	}
	lc := net.ListenConfig{Control: reuseAddrControl}
	if other, err := lc.Listen(context.Background(), "tcp", addr); err == nil {
		other.Close()
		t.Fatalf("an SO_REUSEADDR listener bound %s while the daemon held it", addr)
	}
}

// The old daemon served a connection and closed it first (its side holds
// TIME_WAIT), then closed its listener: the next daemon must still bind.
func TestExclusiveListenerRebindsAfterServerClosedConnection(t *testing.T) {
	ln, err := listenDaemon("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		close(accepted)
	}()
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	if server == nil {
		t.Fatal("no connection accepted")
	}
	_ = server.Close() // server closes first: TIME_WAIT on the daemon's side
	_, _ = io.ReadAll(client)
	_ = client.Close()
	_ = ln.Close()
	took := rebindWithin(t, addr, exclusiveRebindWindow)
	t.Logf("rebound %s %s after the listener closed", addr, took)
}

// The update handoff itself: the predecessor process exits while a client
// connection is still open; the successor binds the same port.
func TestExclusiveListenerRebindsAfterOwnerProcessExits(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "SOLU_EXCL_LISTEN_CHILD=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() }()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "ADDR ")
	if !ok {
		t.Fatalf("child said %q", line)
	}
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	greeting, _ := bufio.NewReader(client).ReadString('\n')
	if greeting != "hi\n" {
		t.Fatalf("greeting %q", greeting)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("child exit: %v", err)
	}
	// The client is still open here, as browser tabs are during an update.
	took := rebindWithin(t, addr, exclusiveRebindWindow)
	t.Logf("rebound %s %s after the owner exited", addr, took)
}

// Microsoft documents that an exclusive port can stay unbindable while a
// connection the closed listener accepted is still active. The update
// handoff never hits this (the predecessor exits, updater_apply.go), but a
// graceful shutdown closes the listener first, so record the behaviour: the
// port must be bindable once that connection is gone.
func TestExclusiveListenerRebindsOnceAcceptedConnectionCloses(t *testing.T) {
	ln, err := listenDaemon("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("no connection accepted")
	}
	_ = ln.Close()
	if probe, err := listenDaemon("tcp", addr); err == nil {
		_ = probe.Close()
		t.Logf("%s: bindable while the accepted connection is still open", addr)
	} else {
		t.Logf("%s: not bindable while the accepted connection is open (%v)", addr, err)
	}
	_ = server.Close()
	_, _ = io.ReadAll(client)
	took := rebindWithin(t, addr, exclusiveRebindWindow)
	t.Logf("rebound %s %s after the accepted connection closed", addr, took)
}

// A failure to set SO_EXCLUSIVEADDRUSE must never stop the daemon: it says
// so and listens without exclusivity (@secure, ST-233 review).
func TestExclusiveListenerSetsockoptFailureStillListens(t *testing.T) {
	prev := setExclusiveAddrUse
	setExclusiveAddrUse = func(uintptr) error { return windows.WSAEINVAL }
	defer func() { setExclusiveAddrUse = prev }()

	ln, err := listenDaemon("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed because the option could not be set: %v", err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_, _ = c.Write([]byte("ok\n"))
			_ = c.Close()
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("listener without exclusivity does not accept: %v", err)
	}
	defer client.Close()
	if got, _ := bufio.NewReader(client).ReadString('\n'); got != "ok\n" {
		t.Fatalf("got %q", got)
	}
}

// A real bind error still fails the listen: the port held by another
// listener is not silently given up on.
func TestExclusiveListenerBindErrorStillFails(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if ln, err := listenDaemon("tcp", held.Addr().String()); err == nil {
		ln.Close()
		t.Fatalf("listenDaemon bound %s while another listener held it", held.Addr())
	}
}
