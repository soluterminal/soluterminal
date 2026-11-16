//go:build !windows

package main

import "syscall"

// daemonListenControl is Windows-only (SO_EXCLUSIVEADDRUSE, ST-233). On
// Linux and macOS a listener's address can't be taken over by another user's
// socket without SO_REUSEPORT on both sides, which the daemon never sets.
func daemonListenControl(network, address string, c syscall.RawConn) error { return nil }
