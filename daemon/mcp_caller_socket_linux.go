//go:build linux

package main

import (
	"bufio"
	"net"
	"os"
	"strconv"
	"strings"
)

// mcpPeerSocketPlatform looks for the client end of a connection in this
// host's socket tables, with the same match the trace uses
// (findConnectionInode). A connection relayed in from outside (a Windows
// program reaching a WSL daemon in mirrored or user-mode networking) has no
// such row; a local process does while its request is open.
func mcpPeerSocketPlatform(client, server *net.TCPAddr) (found bool, owner int) {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if inode, ok := mcpPeerSocketRow(path, client, server); ok {
			return true, buildInodePidMap()[inode]
		}
	}
	return false, 0
}

func mcpPeerSocketRow(path string, client, server *net.TCPAddr) (inode uint64, found bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
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
		// fields[1] = local, fields[2] = remote, fields[3] = state ("01" =
		// ESTABLISHED): a socket the trace would skip can't be traced on a
		// retry either.
		if fields[3] != "01" {
			continue
		}
		if !procNetAddrMatches(fields[1], client) || !procNetAddrMatches(fields[2], server) {
			continue
		}
		if inode, _ = strconv.ParseUint(fields[9], 10, 64); inode != 0 {
			return inode, true
		}
	}
	return 0, false
}
