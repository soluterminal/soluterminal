//go:build darwin

package main

import (
	"net"
)

// macOS has no /proc. The process on the other end of a local connection is
// found with lsof, which reads the kernel's socket tables through libproc,
// and its parents, name and environment with sysctl (procinfo_darwin.go).
// A caller lsof can't place, or a lookup that fails, is decided by the
// shared approved-device policy, as on any host (untracedCaller).

func refreshProcessTreeForCaller() {}

func lookupPeerPIDPlatform(client, server *net.TCPAddr) (int, bool) {
	out, ok := runLsofTCPPort(client.Port)
	return peerFromLsof(out, ok, client, server)
}

// tracedOutsiderJudgedUntraced: on macOS a traced program outside every pane
// and worker that isn't a known browser goes through untracedCaller, as every
// macOS caller did before macOS was traced. Browsers missing from the name
// list (Arc, other Chromium builds) keep working and can approve their first
// device; the accepted residual is that a detached same-user process sending
// browser headers is an unproven person in off and report. A variable so
// tests can apply either platform's rule.
var tracedOutsiderJudgedUntraced = true

// platformBrowserProcessNames: macOS's names are in browserProcessNames.
var platformBrowserProcessNames = map[string]bool{}
