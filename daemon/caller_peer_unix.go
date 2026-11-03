//go:build !windows && !darwin

package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// readPpid reads /proc live, so no snapshot needs refreshing first.
func refreshProcessTreeForCaller() {}

// lookupPeerPIDPlatform finds the process owning the client end of a local
// TCP connection: the /proc/net/tcp row whose local address is the client and
// whose remote address is this server, then the process holding that socket.
// A matched row without a readable owner is (0, true), never an unknown peer.
// macOS has its own lookup (caller_peer_darwin.go).
func lookupPeerPIDPlatform(client, server *net.TCPAddr) (int, bool) {
	// WSL can route loopback through Windows and change the source port. No
	// local socket has that tuple; retrying cannot reconstruct its owner.
	// Those misses use the shared device-proof policy in untracedCaller.
	return lookupPeerPIDInTables(client, server, []string{"/proc/net/tcp", "/proc/net/tcp6"}, findCallerInodePID, time.Sleep)
}

func lookupPeerPIDInTables(client, server *net.TCPAddr, paths []string, owner func(uint64) int, backoff func(time.Duration)) (int, bool) {
	// One extra table read, with a short backoff, covers publication/churn.
	// Missing tables and unreadable owners cannot improve on retry.
	for attempt := 0; attempt < 2; attempt++ {
		available := false
		for _, path := range paths {
			inode, readable, matched := findConnectionInode(path, client, server)
			available = available || readable
			if matched {
				pid := owner(inode)
				return pid, true
			}
		}
		if !available || attempt == 1 {
			return 0, false
		}
		backoff(time.Millisecond)
	}
	return 0, false
}

// Stop once a readable holder of the exact client socket is found, rather
// than building a map of every socket on the host. An inherited descriptor
// can still have several holders; this does not identify which one wrote.
func findCallerInodePID(inode uint64) int {
	if inode == 0 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	want := "socket:[" + strconv.FormatUint(inode, 10) + "]"
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		dir := filepath.Join("/proc", entry.Name(), "fd")
		fds, _ := os.ReadDir(dir)
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil && target == want {
				return pid
			}
		}
	}
	return 0
}

func findConnectionInode(path string, client, server *net.TCPAddr) (inode uint64, readable, matched bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		// Only active sockets with a real inode can identify a client.
		// TIME_WAIT/closed rows can belong to an earlier use of the tuple.
		switch fields[3] {
		case "01", "02", "03", "04", "05", "08": // ESTABLISHED, SYN, FIN_WAIT, CLOSE_WAIT
		default:
			continue
		}
		if !procNetAddrMatches(fields[1], client) || !procNetAddrMatches(fields[2], server) {
			continue
		}
		inode, _ := strconv.ParseUint(fields[9], 10, 64)
		if inode != 0 {
			return inode, true, true
		}
	}
	return 0, sc.Err() == nil, false
}

// procNetAddrMatches compares a /proc/net/tcp "ADDR:PORT" field with addr. An
// IPv4 peer of a dual-stack listener appears in tcp6 as ::ffff:a.b.c.d, so
// both spellings are tried.
func procNetAddrMatches(field string, addr *net.TCPAddr) bool {
	parts := strings.SplitN(field, ":", 2)
	if len(parts) != 2 {
		return false
	}
	port, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil || int(port) != addr.Port {
		return false
	}
	for _, want := range procNetHexForms(addr.IP) {
		if strings.EqualFold(parts[0], want) {
			return true
		}
	}
	return false
}

func procNetHexForms(ip net.IP) []string {
	if ip4 := ip.To4(); ip4 != nil {
		mapped := append(make([]byte, 0, 16), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff)
		return []string{procNetHex(ip4), procNetHex(append(mapped, ip4...))}
	}
	if ip16 := ip.To16(); ip16 != nil {
		return []string{procNetHex(ip16)}
	}
	return nil
}

// procNetHex renders an address the way /proc/net/tcp does: each 32-bit word
// in host (little-endian) byte order, as upper-case hex.
func procNetHex(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for i := 0; i+3 < len(b); i += 4 {
		for j := 3; j >= 0; j-- {
			c := b[i+j]
			out = append(out, digits[c>>4], digits[c&0xf])
		}
	}
	return string(out)
}

// tracedOutsiderJudgedUntraced: on Linux a traced program outside every pane
// and worker that isn't a known browser is the person only with an approved
// device's signature. A variable so tests can apply either platform's rule.
var tracedOutsiderJudgedUntraced = false

// platformBrowserProcessNames are Linux browsers' exact executable names (the
// comm, at most 15 characters): Zen (launcher and binary), LibreWolf and
// Floorp. Brave and Vivaldi are in browserProcessNames.
var platformBrowserProcessNames = map[string]bool{"zen": true, "zen-bin": true, "librewolf": true, "floorp": true}
