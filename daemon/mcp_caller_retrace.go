package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// An agent in a pane names its pane in X-Soluterminal-Pane, but only a trace
// of the connection to its process makes that true (mcpCallerForIdentity).
// A trace that misses for a local process turns a lead into an anonymous
// caller for that one call: spawn_pane then starts an "api" pane with no
// starter and no worker link, in whatever workspace was active last. So a
// header-carrying call whose trace found no process is traced once more, and
// if the caller is still unconfirmed while its socket is plainly local, the
// calls that would record or default to the caller's pane are refused with a
// retryable error instead of silently acting as no pane.
//
// The header only decides whether to look again or refuse; identity still
// comes from the trace alone.

// mcpCallerRetraceDelay gives a racing /proc read a moment before the second
// trace. Tests set it to zero.
var mcpCallerRetraceDelay = 20 * time.Millisecond

// mcpPeerSocket reports whether the client end of r's connection is a socket
// on this host (found) and, if so, the process holding it (owner, 0 when
// unreadable). Where no socket table can be read, found is false. A variable
// so tests can stand in for the platform lookup.
var mcpPeerSocket = func(r *http.Request) (found bool, owner int) {
	client, ok := tcpAddrFromString(r.RemoteAddr)
	if !ok {
		return false, 0
	}
	server, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	serverTCP, ok := server.(*net.TCPAddr)
	if !ok {
		return false, 0
	}
	return mcpPeerSocketPlatform(client, serverTCP)
}

// identifyMCPCaller is identifyCaller for a tools/call. unconfirmed reports a
// caller that claims a pane, could not be traced even on a second look, and
// whose connection comes from an established local socket held by a readable
// process, so tracing should have worked and a retry can. Callers that cannot
// be traced at all (macOS, Windows programs reaching a WSL daemon, relays,
// sockets of processes the daemon can't inspect) are never unconfirmed: they
// keep today's claimed-caller treatment.
func (a *apiServer) identifyMCPCaller(r *http.Request) (id callerIdentity, unconfirmed bool) {
	id = a.identifyCaller(r)
	header := mcpCallerPane(r)
	if header == "" || !mcpTraceMissed(id) {
		return id, false
	}
	// Only a socket on this host can be traced; a second look at anything
	// else would only cost time.
	found, owner := mcpPeerSocket(r)
	if found {
		if mcpCallerRetraceDelay > 0 {
			time.Sleep(mcpCallerRetraceDelay)
		}
		id = a.identifyCaller(r)
	}
	unconfirmed = found && owner > 0 && !peerIsRelayProxy(owner) && mcpTraceMissed(id)
	noteMCPCallerTraceMiss(header, found, owner, id)
	return id, unconfirmed
}

// mcpTraceMissed is the identity untracedCaller gives a connection whose
// process could not be found at all.
func mcpTraceMissed(id callerIdentity) bool {
	return id.Kind == callerUnverified && id.PID == 0
}

// mcpUnconfirmedCallerNotice is the retryable refusal for an unconfirmed
// caller (identifyMCPCaller).
const mcpUnconfirmedCallerNotice = "SoluTerminal couldn't confirm which pane you are; retry. Nothing was created or changed."

// mcpRefuseUnconfirmedCaller returns the refusal for a call an unconfirmed
// caller may not make, or nil. Refused: calls that record the caller as the
// starter of what they create (spawn_pane, create_schedule) and pane changes
// that default to the caller's own pane (set_pane_info without pane_id).
func mcpRefuseUnconfirmedCaller(params json.RawMessage) any {
	var call toolCallParams
	if json.Unmarshal(params, &call) != nil {
		return nil
	}
	switch call.Name {
	case "spawn_pane", "create_schedule":
	case "set_pane_info":
		if strings.TrimSpace(mcpArgString(call.Arguments, "pane_id")) != "" {
			return nil
		}
	default:
		return nil
	}
	return toolErr(mcpUnconfirmedCallerNotice)
}

// mcpCallerTraceMisses rate-limits the miss log line: one per outcome per
// interval. The claimed header is not part of the key, so a caller that varies
// it can't fill the log.
var mcpCallerTraceMisses = struct {
	sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

const mcpCallerTraceMissEvery = time.Minute

func noteMCPCallerTraceMiss(header string, found bool, owner int, retried callerIdentity) {
	outcome := "unverified"
	switch {
	case retried.Kind == callerPane && retried.PaneID != "":
		outcome = "pane " + retried.PaneID
	case !mcpTraceMissed(retried):
		outcome = string(retried.Kind)
	}
	key := fmt.Sprintf("%t\x00%t\x00%s", found, owner > 0, outcome)
	now := time.Now()
	mcpCallerTraceMisses.Lock()
	if t, ok := mcpCallerTraceMisses.last[key]; ok && now.Sub(t) < mcpCallerTraceMissEvery {
		mcpCallerTraceMisses.Unlock()
		return
	}
	if len(mcpCallerTraceMisses.last) >= 256 {
		mcpCallerTraceMisses.last = map[string]time.Time{}
	}
	mcpCallerTraceMisses.last[key] = now
	mcpCallerTraceMisses.Unlock()
	// The header is the caller's own text: quote and cap it.
	if len(header) > 64 {
		header = header[:64]
	}
	log.Printf("mcp: caller trace missed (header pane %q, found=%t, owner=%d, retry=%s)", header, found, owner, outcome)
}
