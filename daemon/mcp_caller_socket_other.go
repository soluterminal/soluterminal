//go:build !linux

package main

import "net"

// mcpPeerSocketPlatform: only Linux is known to trace every local caller, so
// elsewhere no connection counts as one that should have been traced.
func mcpPeerSocketPlatform(client, server *net.TCPAddr) (found bool, owner int) {
	return false, 0
}
