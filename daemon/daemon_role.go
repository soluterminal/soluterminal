package main

import (
	"net"
	"runtime"
	"strings"
)

// Primary versus side daemons.
//
// Every daemon a user starts shares their provider credential stores: those
// live under HOME (~/.claude, ~/.codex), not under --data. Only one daemon
// may manage them. Refreshing a saved login rotates its single-use refresh
// token, so a second daemon doing that races the user's real daemon for the
// same token. On 12 Nov a capture daemon on 127.0.0.1:18899 with its own
// --data ran refresh grants at boot against the shared store and wrote into
// the real install's solud.log.
//
// The rule, decided once at boot from the command line:
//
//	primary = the listen port is canonical (8787, or 38787 for Dual's Windows
//	          backend; any host) AND --data is the default data directory.
//
// Anything else is a side daemon: dev, test, capture, local Preview, sandbox.
// A side daemon still serves panes, workspaces and the UI. It does not start
// the background features that act on the shared credential stores (each
// registers itself with registerSideDaemonSkip and checks bootEnv.role), and
// its durable log lives in <data>/logs (resolveDurableLogDir).
type daemonRole struct {
	Primary bool
	// Reasons say why this is a side daemon (empty when primary).
	Reasons []string
}

// defaultDaemonAddr is -addr's default: a daemon started without -addr is on
// the canonical port.
const defaultDaemonAddr = "127.0.0.1:8787"

// sideDaemonSkips names the features a side daemon does not run, in
// registration order. Optional features add themselves from init(), so the
// boot log lists exactly what this build skipped.
var sideDaemonSkips []string

func registerSideDaemonSkip(feature string) {
	sideDaemonSkips = append(sideDaemonSkips, feature)
}

// decideDaemonRole applies the rule above. Pure apart from path
// normalisation, so the decision is testable on any host.
func decideDaemonRole(goos, addr, dataDir, defaultData string) daemonRole {
	var reasons []string
	if _, port, err := net.SplitHostPort(strings.TrimSpace(addr)); err != nil || !canonicalDaemonPort(port) {
		reasons = append(reasons, "non-canonical port in -addr "+addr)
	}
	if !sameDataDir(goos, dataDir, defaultData) {
		reasons = append(reasons, "non-default -data "+dataDir)
	}
	return daemonRole{Primary: len(reasons) == 0, Reasons: reasons}
}

// bootDaemonRole is decideDaemonRole for this process.
func bootDaemonRole(addr, dataDir string) daemonRole {
	return decideDaemonRole(runtime.GOOS, addr, dataDir, defaultDataDir())
}

// logLine is the one boot line that records the decision.
func (r daemonRole) logLine() string {
	if r.Primary {
		return "daemon role: primary (canonical port, default -data)"
	}
	skipped := "nothing (no shared-credential features in this build)"
	if len(sideDaemonSkips) > 0 {
		skipped = strings.Join(sideDaemonSkips, ", ")
	}
	return "daemon role: side daemon (" + strings.Join(r.Reasons, "; ") + "); skipped: " + skipped +
		"; only the primary daemon (port 8787 or 38787 with the default -data) manages shared credentials"
}
