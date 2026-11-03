package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lsofLookupTimeout bounds one lookup; lsof walks every process's files.
const lsofLookupTimeout = 2 * time.Second

const lsofConcurrency = 4

var lsofSlots = make(chan struct{}, lsofConcurrency)

// lsofPath is the system lsof on macOS. Only that absolute path is run,
// never one found on PATH, which a program could shadow. A variable so tests
// on other hosts can point it at theirs.
var lsofPath = "/usr/sbin/lsof"

// runLsofTCPPort lists the TCP sockets using port, in field form. ok is
// false only when lsof could not answer; "nothing found" is ok with no rows.
// A variable so tests can stand in for it.
var runLsofTCPPort = func(port int) ([]byte, bool) {
	path := lsofPath
	if _, err := os.Stat(path); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofLookupTimeout)
	defer cancel()
	// At most lsofConcurrency lookups at once; a burst of new connections
	// waits within the same timeout instead of starting an lsof each.
	select {
	case lsofSlots <- struct{}{}:
		defer func() { <-lsofSlots }()
	case <-ctx.Done():
		return nil, false
	}
	cmd := exec.CommandContext(ctx, path, "-nP", "-w", "-iTCP:"+strconv.Itoa(port), "-Fpn")
	// Don't wait on a child that keeps stdout open after lsof is killed.
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		// lsof exits 1 when nothing matched.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
			return out, true
		}
		return nil, false
	}
	return out, true
}

// peerFromLsof turns one lsof lookup into lookupPeerPID's answer: the
// owning pid when lsof found the client socket. Anything else, lsof finding
// nothing (the socket closed, or belongs to another user) or failing to
// answer (slow, missing), is an unknown caller, decided by the shared
// approved-device policy (untracedCaller): failing closed on a slow lookup
// would refuse the person's own window.
func peerFromLsof(out []byte, answered bool, client, server *net.TCPAddr) (int, bool) {
	if !answered {
		lsofFailureLog.note()
		return 0, false
	}
	if pid, found := lsofPeerPID(out, client, server); found {
		return pid, true
	}
	return 0, false
}

// lsofFailureLog reports lsof failures at most once a minute.
var lsofFailureLog = &rateLimitedNote{every: time.Minute, msg: "authority: lsof did not answer for a local caller; the approved-device policy decides"}

type rateLimitedNote struct {
	mu    sync.Mutex
	last  time.Time
	every time.Duration
	msg   string
}

func (n *rateLimitedNote) note() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if now := time.Now(); now.Sub(n.last) >= n.every {
		n.last = now
		log.Print(n.msg)
	}
}

// lsofPeerPID finds, in `lsof -nP -iTCP:<port> -Fpn` output, the process
// whose socket has client as its local end and server as its remote end:
// the program that opened the connection. The daemon's accepted end has them
// the other way round, so it never matches; the daemon's own outgoing
// connections (its preview proxy) do, and are classified as its workers. Used on macOS (caller_peer_darwin.go); kept free of
// build tags so it is tested everywhere, against lsof's field output, which
// is the same on every platform.
func lsofPeerPID(out []byte, client, server *net.TCPAddr) (int, bool) {
	pid := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'n':
			if pid <= 0 {
				continue
			}
			local, remote, ok := parseLsofConnection(line[1:])
			if ok && sameTCPAddr(local, client) && sameTCPAddr(remote, server) {
				return pid, true
			}
		}
	}
	return 0, false
}

// parseLsofConnection splits lsof's "local->remote" name for a connected
// socket: "127.0.0.1:52144->127.0.0.1:8787" or "[::1]:52144->[::1]:8787".
func parseLsofConnection(name string) (local, remote *net.TCPAddr, ok bool) {
	// A state suffix such as " (ESTABLISHED)" appears without -Fn; strip it.
	if i := strings.IndexByte(name, ' '); i >= 0 {
		name = name[:i]
	}
	l, r, found := strings.Cut(name, "->")
	if !found {
		return nil, nil, false
	}
	local, okL := parseLsofEndpoint(l)
	remote, okR := parseLsofEndpoint(r)
	return local, remote, okL && okR
}

func parseLsofEndpoint(s string) (*net.TCPAddr, bool) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil, false
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || err != nil || n <= 0 || n > 65535 {
		return nil, false
	}
	return &net.TCPAddr{IP: ip, Port: n}, true
}

func sameTCPAddr(a, b *net.TCPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}
