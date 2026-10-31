package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Caller authority decides who may exercise a person's authority on this
// daemon: approving workflow tasks, answering permission prompts, turning on
// auto-approve, and widening approval autopilot or YOLO settings.
//
// Tokenless loopback (auth.go) admits every local program as the owner, so on
// its own the API cannot tell the SoluTerminal window from an agent running
// `curl` inside a pane. These acts therefore look at who is actually on the
// other end of the connection:
//
//   - human: the SoluTerminal app (a browser or desktop window making a
//     same-origin fetch), or a signed-in person over the cloud relay.
//   - pane: a process inside a SoluTerminal pane — an interactive agent or a
//     shell. It may act for the person only while the person has turned on
//     agent authority (Settings → Agent permissions).
//   - worker: a process the daemon spawned itself (workflow workers, managed
//     agents, preview servers). Never acts for the person: an executor
//     approving its own work is the exact bypass this file exists to stop.
//   - unverified: any other local program that is not a browser.
//
// The peer process is found from the TCP connection itself (caller_peer_*.go)
// and walked up its parents; headers alone never outrank a traced process,
// and a traced caller counts as the person only when it is a browser.
// When the connection cannot be traced — a Windows process reaching a WSL
// daemon, or WSL's consomme networking relaying a loopback connection — only
// browser fetch metadata remains, which a program can imitate on purpose.
// This is a guard against agents widening their own authority, not a sandbox:
// a program that is already hostile and running as the same OS user can edit
// the daemon's files directly. That boundary needs OS-level isolation.

type callerKind string

const (
	callerHuman      callerKind = "human"
	callerPane       callerKind = "pane"
	callerWorker     callerKind = "worker"
	callerUnverified callerKind = "unverified"
)

type callerIdentity struct {
	Kind   callerKind
	PaneID string
	PID    int
	Relay  bool
	// FromEnv: PaneID came from SOLU_PANE_ID in the environment of a process
	// with no pane above it, not from the pane's own process tree.
	FromEnv bool
}

type relayTransportKey struct{}

// markRelayTransport tags every request that arrived through the cloud relay
// tunnel. Unlike X-Forwarded-Prefix, which the cloud stamps and could leave
// off, the tag comes from the listener the request was read from.
func markRelayTransport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true)))
	})
}

func relayTransport(r *http.Request) bool {
	v, _ := r.Context().Value(relayTransportKey{}).(bool)
	return v
}

// requestViaRelay is the check for "refuse this over the relay": it holds for
// a request read from the relay tunnel even when the cloud omits the
// /m/<machine> prefix marker. A local caller that sends the marker itself only
// restricts itself.
func requestViaRelay(r *http.Request) bool {
	return relayTransport(r) || isCloudForwardedPrefix(r.Header.Get("X-Forwarded-Prefix"))
}

// lookupPeerPID finds the local process on the other end of r's connection.
// A variable so tests can stand in for the platform lookup.
var lookupPeerPID = func(r *http.Request) (int, bool) {
	client, ok := tcpAddrFromString(r.RemoteAddr)
	if !ok {
		return 0, false
	}
	server, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	serverTCP, ok := server.(*net.TCPAddr)
	if !ok {
		return 0, false
	}
	return lookupPeerPIDPlatform(client, serverTCP)
}

func tcpAddrFromString(s string) (*net.TCPAddr, bool) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil, false
	}
	addr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, false
	}
	return addr, true
}

// identifyCallerHook lets the package's tests stand a person in for the
// handler tests written before caller authority existed. Nil in production.
var identifyCallerHook func(*http.Request) callerIdentity

func (a *apiServer) identifyCaller(r *http.Request) callerIdentity {
	if identifyCallerHook != nil {
		return identifyCallerHook(r)
	}
	if relayTransport(r) {
		return callerIdentity{Kind: callerHuman, Relay: true}
	}
	if pid, ok := lookupPeerPID(r); ok && pid > 0 {
		if id, known := a.classifyPeerProcess(pid); known {
			return id
		}
		// A traced caller must really be a browser, not just send a
		// browser's headers: otherwise an agent that detaches from its pane
		// (`setsid -f env -i curl -H 'Sec-Fetch-Site: same-origin' …`) would
		// pass as the person.
		if browserOriginated(r) && (peerIsBrowser(pid) || peerIsRelayProxy(pid)) {
			return callerIdentity{Kind: callerHuman, PID: pid}
		}
		return callerIdentity{Kind: callerUnverified, PID: pid}
	}
	if browserOriginated(r) {
		return callerIdentity{Kind: callerHuman}
	}
	return callerIdentity{Kind: callerUnverified}
}

// classifyPeerProcess reports whether pid runs inside a pane or was spawned by
// the daemon. Panes are checked first: on some hosts the pane processes are the
// daemon's own children.
func (a *apiServer) classifyPeerProcess(pid int) (callerIdentity, bool) {
	self := os.Getpid()
	if pid == self {
		// The daemon's own proxies (previews, exposure mounts) forward
		// candidate pages to the API.
		return callerIdentity{Kind: callerWorker, PID: pid}, true
	}
	refreshProcessTreeForCaller()
	panes := map[int]string{}
	if a.mgr != nil {
		for _, p := range a.mgr.List() {
			if p.Pid > 0 {
				panes[p.Pid] = p.ID
			}
		}
	}
	// A pane process among the ancestors decides first. Only a process with
	// no pane above it (one that detached) is placed by the SOLU_PANE_ID in
	// its environment: otherwise `env SOLU_PANE_ID=<another pane> curl …`
	// run inside one pane would pass as that other pane.
	var chain []int
	spawnedBySelf := false
	cur := pid
	for depth := 0; depth < 64 && cur > 1; depth++ {
		if cur == self {
			// What the daemon started is the daemon's, wherever the
			// daemon itself was launched from.
			spawnedBySelf = true
			break
		}
		if id, ok := panes[cur]; ok {
			return callerIdentity{Kind: callerPane, PaneID: id, PID: pid}, true
		}
		chain = append(chain, cur)
		next := readPpid(cur)
		if next == cur {
			break
		}
		cur = next
	}
	// A pane process that detached keeps the pane id the daemon put in its
	// environment. Ancestors count too: a browser an agent launches starts
	// with a clean environment, but its launcher does not.
	for _, proc := range chain {
		if env, ok := readProcEnviron(proc); ok {
			if id := strings.TrimSpace(env["SOLU_PANE_ID"]); id != "" {
				return callerIdentity{Kind: callerPane, PaneID: id, PID: pid, FromEnv: true}, true
			}
		}
	}
	if spawnedBySelf {
		return callerIdentity{Kind: callerWorker, PID: pid}, true
	}
	return callerIdentity{}, false
}

// browserProcessNames are the processes (or their parents) that make a
// browser's network requests: Chromium-family network services, Firefox's
// parent and socket processes, WebKit's network process, and the Windows
// desktop shell's WebView2.
var browserProcessNames = map[string]bool{
	"chrome": true, "chromium": true, "chromium-browse": true, "msedge": true,
	"msedgewebview2": true, "brave": true, "vivaldi-bin": true, "opera": true,
	"firefox": true, "firefox-bin": true, "firefox-esr": true, "socket process": true,
	"webkitnetworkpr": true, "epiphany": true, "solubrowse": true,
}

// peerIsBrowser reports whether pid or one of its near parents is a browser.
// A variable so tests can stand in a browser process.
var peerIsBrowser = func(pid int) bool {
	cur := pid
	for depth := 0; depth < 6 && cur > 1; depth++ {
		name := strings.ToLower(strings.TrimSpace(readComm(cur)))
		name = strings.TrimSuffix(name, ".exe")
		if browserProcessNames[name] {
			return true
		}
		next := readPpid(cur)
		if next == cur {
			break
		}
		cur = next
	}
	return false
}

// peerIsRelayProxy reports whether pid is a proxy that carries other
// processes' connections, so the real client cannot be traced through it:
// WSL's init (it forwards Windows localhost connections into the distro in
// NAT mode) and another solud process (the Dual-mode router). Such callers
// fall back to browser fetch metadata, like any untraceable caller.
var peerIsRelayProxy = func(pid int) bool {
	if pid == 1 {
		return true
	}
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(readComm(pid))), ".exe")
	return name == "init" || name == "solud"
}

// browserOriginated reports whether r is a script fetch from the page the
// daemon itself serves. Browsers set Sec-Fetch-Site; older WebView2 builds omit
// it but always send Origin on a POST or PUT.
func browserOriginated(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
		origin := r.Header.Get("Origin")
		if origin == "" {
			return false
		}
		u, err := url.Parse(origin)
		return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
	}
	return false
}

// requireHuman refuses r unless a person made it. It guards the acts that
// widen what agents may do; agent authority never delegates them.
func (a *apiServer) requireHuman(w http.ResponseWriter, r *http.Request, action string) bool {
	id := a.identifyCaller(r)
	if id.Kind == callerHuman {
		log.Printf("authority: %s by a person (pid %d, relay %t)", action, id.PID, id.Relay)
		return true
	}
	a.refuseCaller(w, id, action, false)
	return false
}

// requireHumanOrDelegated also admits an interactive pane while the person has
// turned on agent authority. Workers and unverified programs are always
// refused.
func (a *apiServer) requireHumanOrDelegated(w http.ResponseWriter, r *http.Request, action string) bool {
	id := a.identifyCaller(r)
	switch id.Kind {
	case callerHuman:
		log.Printf("authority: %s by a person (pid %d, relay %t)", action, id.PID, id.Relay)
		return true
	case callerPane:
		if a.agentAuthority.active(time.Now()) {
			log.Printf("authority: pane %s (pid %d) may %s: agent authority is on", id.PaneID, id.PID, action)
			return true
		}
	}
	a.refuseCaller(w, id, action, true)
	return false
}

func (a *apiServer) refuseCaller(w http.ResponseWriter, id callerIdentity, action string, delegable bool) {
	log.Printf("authority: refused %s caller=%s pane=%s pid=%d", action, id.Kind, id.PaneID, id.PID)
	var msg string
	switch {
	case id.Kind == callerPane && id.PID > 0 && peerIsBrowser(id.PID):
		// Deliberate: a browser an agent launches must not act as the
		// person, so a browser started from inside a pane counts as that
		// pane. Say how to get out of it.
		msg = fmt.Sprintf("This browser window was started from inside a SoluTerminal pane, so it counts as an agent and can't %s. Open SoluTerminal in the desktop app or a browser you started normally.", action)
	case id.Kind == callerWorker:
		msg = fmt.Sprintf("Workflow workers and other processes started by SoluTerminal can't %s.", action)
	case id.Kind == callerPane && delegable:
		msg = fmt.Sprintf("Agents can't %s on their own. Do it in the SoluTerminal app, or turn on Settings → Agent permissions to let agents in panes do it for you.", action)
	case id.Kind == callerPane:
		msg = fmt.Sprintf("Only you can %s. Do it in the SoluTerminal app.", action)
	default:
		msg = fmt.Sprintf("To %s, use the SoluTerminal app.", action)
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   "human_required",
		"caller":  string(id.Kind),
		"message": msg,
	})
}

// ---- Agent authority setting ----------------------------------------------

const (
	agentAuthorityFile           = "agent-authority.json"
	agentAuthorityMaxRequestBody = 1 << 10
)

// agentAuthorityRecord is agent-authority.json. Panes on means agents running
// in panes may approve tasks, answer permission prompts and turn on
// auto-approve for the person. Until, when set, ends the grant.
type agentAuthorityRecord struct {
	Panes     bool       `json:"panes"`
	Until     *time.Time `json:"until,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

type agentAuthority struct {
	mu     sync.Mutex
	path   string
	record agentAuthorityRecord
}

func newAgentAuthority(dataDir string) *agentAuthority {
	s := &agentAuthority{path: filepath.Join(dataDir, agentAuthorityFile)}
	if b, err := os.ReadFile(s.path); err == nil {
		var rec agentAuthorityRecord
		if json.Unmarshal(b, &rec) == nil {
			s.record = rec
		}
	}
	return s
}

func (s *agentAuthority) active(now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record.Panes && (s.record.Until == nil || now.Before(*s.record.Until))
}

func (s *agentAuthority) snapshot() agentAuthorityRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record
}

func (s *agentAuthority) set(rec agentAuthorityRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return err
	}
	s.record = rec
	return nil
}

type agentAuthorityResponse struct {
	Panes  bool       `json:"panes"`
	Until  *time.Time `json:"until,omitempty"`
	Active bool       `json:"active"`
}

func (a *apiServer) agentAuthorityResponse() agentAuthorityResponse {
	rec := a.agentAuthority.snapshot()
	return agentAuthorityResponse{Panes: rec.Panes, Until: rec.Until, Active: a.agentAuthority.active(time.Now())}
}

func (a *apiServer) handleGetAgentAuthority(w http.ResponseWriter, _ *http.Request) {
	if a.agentAuthority == nil {
		http.Error(w, "agent permissions are unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.agentAuthorityResponse())
}

// handlePutAgentAuthority turns agent authority on or off. Turning it on is a
// person's act on the computer itself: never over the relay, never by an
// agent. Anyone may turn it off.
func (a *apiServer) handlePutAgentAuthority(w http.ResponseWriter, r *http.Request) {
	if a.agentAuthority == nil {
		http.Error(w, "agent permissions are unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Panes *bool `json:"panes"`
		// Hours limits the grant; 0 or absent keeps it until turned off.
		Hours int `json:"hours"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, agentAuthorityMaxRequestBody)).Decode(&body); err != nil || body.Panes == nil {
		http.Error(w, `body must set "panes" to true or false`, http.StatusBadRequest)
		return
	}
	if body.Hours < 0 || body.Hours > 24*30 {
		http.Error(w, `"hours" must be between 0 and 720`, http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	rec := agentAuthorityRecord{Panes: *body.Panes, UpdatedAt: now}
	if rec.Panes {
		if requestViaRelay(r) {
			http.Error(w, "agent permissions can only be turned on at the computer itself", http.StatusForbidden)
			return
		}
		if !a.requireHuman(w, r, "turn on agent permissions") {
			return
		}
		if body.Hours > 0 {
			until := now.Add(time.Duration(body.Hours) * time.Hour)
			rec.Until = &until
		}
	}
	if err := a.agentAuthority.set(rec); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	log.Printf("authority: agent permissions set panes=%t until=%v", rec.Panes, rec.Until)
	writeJSON(w, http.StatusOK, a.agentAuthorityResponse())
}

// promptInputGate decides, once per connection, whether input from r may reach
// a pane while that pane is showing a permission prompt. Typing "1" or "y" into
// a prompt answers it, so an agent without the person's grant must not. Only
// callers identified as agents are held back; an untraceable caller (a Windows
// browser reaching a WSL daemon, a relayed loopback connection) keeps today's
// behaviour so the person's own keystrokes are never dropped.
func (a *apiServer) promptInputGate(r *http.Request) func(*Pane) bool {
	return a.promptInputGateFor(a.identifyCaller(r))
}

func (a *apiServer) promptInputGateFor(id callerIdentity) func(*Pane) bool {
	if id.Kind != callerPane && id.Kind != callerWorker {
		return func(*Pane) bool { return true }
	}
	return func(p *Pane) bool {
		if id.Kind == callerPane && a.agentAuthority.active(time.Now()) {
			return true
		}
		return p == nil || p.computeStateCached() != StateChoicePrompt
	}
}

// paneSocketInputPolicy classifies a pane terminal socket once, at upgrade.
// The prompt gate is unchanged; human is the daemon's own classification and
// is what lets a visible viewer adopt an unowned pane by typing (handleWS).
// Panes, workers and unverified local programs never adopt.
func (a *apiServer) paneSocketInputPolicy(r *http.Request) wsInputPolicy {
	id := a.identifyCaller(r)
	return wsInputPolicy{
		allowed: a.promptInputGateFor(id),
		human:   id.Kind == callerHuman,
		kind:    string(id.Kind),
	}
}
