//go:build windows

package main

import (
	"log"
	"syscall"

	"golang.org/x/sys/windows"
)

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE, defined by Winsock as
// ((int)(~SO_REUSEADDR)). x/sys/windows has SO_REUSEADDR but not this one.
const soExclusiveAddrUse = ^int(windows.SO_REUSEADDR)

// setExclusiveAddrUse sets SO_EXCLUSIVEADDRUSE on a socket before bind. A
// variable so tests can make it fail.
var setExclusiveAddrUse = func(fd uintptr) error {
	return windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, soExclusiveAddrUse, 1)
}

// daemonListenControl binds the daemon's loopback listeners with
// SO_EXCLUSIVEADDRUSE (ST-233): no other socket, of this user or another,
// can bind the same address and port while the daemon holds it, even with
// SO_REUSEADDR, so nothing can share or take over the daemon's port.
//
// The option is hardening, never a reason not to start: if it can't be set,
// the daemon says so once and binds without it. A real bind error (the port
// is taken) still fails the listen, as before.
func daemonListenControl(network, address string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = setExclusiveAddrUse(fd)
	}); err != nil {
		log.Printf("listener %s: exclusive bind unavailable: %v; listening without it", address, err)
		return nil
	}
	if sockErr != nil {
		log.Printf("listener %s: exclusive bind unavailable: %v; listening without it", address, sockErr)
	}
	return nil
}
