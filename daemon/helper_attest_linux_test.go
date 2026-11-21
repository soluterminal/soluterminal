//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	hp "soluterminal/helperproto"
)

type oneAgentIndex struct{ ref hp.AgentInfo }

func (o oneAgentIndex) AgentByPid(pid int) (hp.AgentInfo, bool) {
	if pid == o.ref.Pid && pid > 0 {
		return o.ref, true
	}
	return hp.AgentInfo{}, false
}

// A real unix socket: SO_PEERCRED and /proc, with this test process standing
// in for the agent the helper spawned.
func TestHelperAttestRealSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	self := os.Getpid()
	start, ok := mcpProcessStartTimePlatform(self)
	if !ok {
		t.Fatal("own start time unreadable")
	}
	ref := hp.AgentInfo{AgentID: testAttestAgentID, PaneID: testAttestPaneID, Pid: self, StartTicks: start}

	h := newHelperAttestor(oneAgentIndex{ref})
	a, err := h.attestConn(server)
	if !kernelHasPeerPidfd(t, server) {
		// Before Linux 6.5 the pid can't be bound to the connector, so the
		// helper attests nothing.
		if err == nil {
			a.Close()
			t.Fatal("attested a peer on a kernel without SO_PEERPIDFD")
		}
		t.Skipf("kernel lacks SO_PEERPIDFD; attestation refused as designed: %v", err)
	}
	if err != nil {
		t.Fatalf("attestConn: %v", err)
	}
	if a.PaneID != testAttestPaneID || a.PID != self || a.UID != os.Geteuid() || a.Start != start {
		t.Fatalf("attestation = %+v", a)
	}
	if !h.stillValid(a) {
		t.Fatal("not valid")
	}
	a.Close()
	if h.stillValid(a) {
		t.Fatal("valid after Close released the pidfd")
	}

	// An index vouching for a record whose start differs from the live
	// process is refused, not trusted.
	stale := ref
	stale.StartTicks = start + 1
	if _, err := newHelperAttestor(oneAgentIndex{stale}).attestConn(server); !errors.Is(err, errHelperAttestChanged) {
		t.Fatalf("attestConn with a stale record: %v", err)
	}
	// No agent among this process's parents.
	if _, err := newHelperAttestor(oneAgentIndex{}).attestConn(server); !errors.Is(err, errHelperAttestNoAgent) {
		t.Fatalf("attestConn with no agent: %v", err)
	}
}

// kernelHasPeerPidfd asks the kernel directly whether SO_PEERPIDFD exists.
func kernelHasPeerPidfd(t *testing.T, c net.Conn) bool {
	t.Helper()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fd int
	var optErr error
	if err := raw.Control(func(s uintptr) {
		fd, optErr = unix.GetsockoptInt(int(s), unix.SOL_SOCKET, soPeerPidfd)
	}); err != nil {
		t.Fatal(err)
	}
	if optErr == nil {
		_ = unix.Close(fd)
		return true
	}
	if errors.Is(optErr, unix.ENOPROTOOPT) {
		return false
	}
	t.Fatalf("SO_PEERPIDFD probe: %v", optErr)
	return false
}
