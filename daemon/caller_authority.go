package main

import (
	"context"
	"encoding/json"
	"errors"
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
// and a traced caller counts as the person only when it is a browser or
// signs as an approved device. macOS is the exception: there any other
// traced program is judged like one that can't be traced
// (tracedOutsiderJudgedUntraced). When process lookup is unavailable,
// untracedCaller uses approved-device proof and the shared
// Off/Report/Require policy. Report preserves desktop
// compatibility while recording requests before Require is enabled.
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
	// Device is the approved device whose signature this request carried
	// (human_proof.go).
	Device string
	// Unproven: classified only from browser fetch headers, with no traced
	// browser process and no device signature behind them.
	Unproven bool
	// Automation: a traced browser that a program drives (solubrowse,
	// Playwright).
	Automation bool
	// Forwarded: the request came through the cloud's /m/<machine> route for
	// a remote device (the marker), whatever the transport. Set once, in
	// identifyCaller; gates read it, never the header (forwardedUnsigned).
	Forwarded bool
	// PrivateDevice is the approved device whose private relay channel the
	// request arrived on, or "".
	PrivateDevice string
	// ScopedDevice is an approved alerts-only device
	// (deviceStatusApprovedAlerts) that signed this request at this
	// computer. It is never Device and never makes the caller the person:
	// only the notifications handler reads it. Not set for panes and workers.
	ScopedDevice string
}

// tracedPane excludes detached environment/header claims from operations that
// require a stable per-pane identity, such as workspace creation quotas.
func (id callerIdentity) tracedPane() bool {
	return id.Kind == callerPane && id.PaneID != "" && !id.FromEnv
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

// lookupPeerPID reports whether the client socket was found locally. A true
// result with pid 0 means the socket was observed but its owner is unavailable;
// that is distinct from an unknown tuple. A variable for platform test fixtures.
var lookupPeerPID = func(r *http.Request) (int, bool) {
	memo, _ := r.Context().Value(connPeerMemoKey{}).(*connPeerMemo)
	if pid, ok := memo.get(); ok {
		return pid, true
	}
	pid, known := lookupPeerPIDUncached(r)
	if known && pid > 0 {
		memo.set(pid)
	}
	return pid, known
}

// connPeerMemo remembers, for one TCP connection, the process found holding
// its client end, so a keep-alive connection (the app, an MCP client) is
// looked up once instead of on every request. Only a found process is kept:
// the same connection is the same socket, while a miss may be retried.
type connPeerMemo struct {
	mu  sync.Mutex
	pid int
}

type connPeerMemoKey struct{}

// withConnPeerMemo is the server's ConnContext: one memo per connection.
func withConnPeerMemo(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, connPeerMemoKey{}, &connPeerMemo{})
}

func (m *connPeerMemo) get() (int, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pid, m.pid > 0
}

func (m *connPeerMemo) set(pid int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pid = pid
	m.mu.Unlock()
}

func lookupPeerPIDUncached(r *http.Request) (int, bool) {
	server, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if _, unixSocket := server.(*net.UnixAddr); unixSocket {
		return 0, true
	}
	client, ok := tcpAddrFromString(r.RemoteAddr)
	if !ok {
		return 0, false
	}
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
	id := a.classifyCaller(r)
	id.Forwarded = isCloudForwardedPrefix(r.Header.Get("X-Forwarded-Prefix"))
	if id.Relay {
		id.PrivateDevice = privateRelayDevice(r)
	}
	if id.Kind != callerPane && id.Kind != callerWorker {
		id.ScopedDevice = localScopedDevice(r)
	}
	return id
}

type quietCallerKey struct{}

// identifyCallerQuietly is identifyCaller for a read that only reports what
// the caller may do (the device list's canManageDevices): the same answer,
// without the classification log lines, which are for acts.
func (a *apiServer) identifyCallerQuietly(r *http.Request) callerIdentity {
	return a.identifyCaller(r.WithContext(context.WithValue(r.Context(), quietCallerKey{}, true)))
}

// callerLogf logs a caller classification unless r is a quiet read.
func callerLogf(r *http.Request, format string, args ...any) {
	if quiet, _ := r.Context().Value(quietCallerKey{}).(bool); !quiet {
		log.Printf(format, args...)
	}
}

// classifyCaller is identifyCaller without the transport facts it adds.
func (a *apiServer) classifyCaller(r *http.Request) callerIdentity {
	if relayTransport(r) {
		return callerIdentity{Kind: callerHuman, Relay: true}
	}
	if pid, local := lookupPeerPID(r); local && pid > 0 {
		if id, known := a.classifyPeerProcess(pid); known {
			return id
		}
		if peerIsRelayProxy(pid) || peerIsSharedNetworkService(pid) {
			return a.untracedCaller(r, pid)
		}
		if browserOriginated(r) && peerIsBrowser(pid) {
			return a.tracedBrowserCaller(r, pid)
		}
		// Any other program outside panes and workers. On macOS it is judged
		// as if it couldn't be traced, as it was before macOS was traced: a
		// browser missing from the name list (Arc, other Chromium builds)
		// keeps working and can approve its first device. That includes an
		// agent that detaches from its pane and forges the headers (`setsid
		// -f env -i curl -H 'Sec-Fetch-Site: same-origin' …`): there only
		// require stops it. Elsewhere such a program is the person only with
		// an approved device's signature, the same proof it would be on a
		// connection that can't be traced.
		if tracedOutsiderJudgedUntraced {
			if browserOriginated(r) && localVerifiedDevice(r) == "" {
				callerLogf(r, "authority: traced pid %d (%s) is no known browser; judging it as untraced", pid, readComm(pid))
			}
			return a.untracedCaller(r, pid)
		}
		if dev := localVerifiedDevice(r); dev != "" {
			return callerIdentity{Kind: callerHuman, PID: pid, Device: dev}
		}
		return callerIdentity{Kind: callerUnverified, PID: pid}
	}
	return a.untracedCaller(r, 0)
}

// untracedCaller classifies a caller whose process can't be found (pid 0),
// or was found to be a proxy carrying other programs' connections (pid > 0:
// WSL's init, the desktop app's router). Every untraceable classification goes
// through here. A request signed by an approved device is the person's
// (human_proof.go). Otherwise only browser fetch headers are left to go on:
// each request taken as the person's that way is logged for review, and when
// approved devices are required it is refused. A request that says another
// program forwarded it for a remote device (the /m/ marker) never counts on
// headers: they are the remote device's, passed along, so only a signature
// does. The marker only takes authority away, so it needn't be trusted.
func (a *apiServer) untracedCaller(r *http.Request, pid int) callerIdentity {
	// Keep this check first: Windows desktop connections and local sockets
	// remapped by WSL can have the same untraceable loopback tuple. Socket
	// absence cannot replace positive device proof. Require applies to both;
	// Off/Report keep the window working until its signing is verified.
	if dev := localVerifiedDevice(r); dev != "" {
		return callerIdentity{Kind: callerHuman, PID: pid, Device: dev}
	}
	if !browserOriginated(r) {
		return callerIdentity{Kind: callerUnverified, PID: pid}
	}
	if isCloudForwardedPrefix(r.Header.Get("X-Forwarded-Prefix")) {
		callerLogf(r, "authority: forwarded request isn't the person on headers %s %s from %s (proxy pid %d)", r.Method, r.URL.Path, r.RemoteAddr, pid)
		return callerIdentity{Kind: callerUnverified, PID: pid, Unproven: true}
	}
	mode := a.humanProofMode()
	if mode == humanProofRequire {
		callerLogf(r, "authority: refused unproven person %s %s from %s (proxy pid %d)", r.Method, r.URL.Path, r.RemoteAddr, pid)
		return callerIdentity{Kind: callerUnverified, PID: pid, Unproven: true}
	}
	callerLogf(r, "authority: unproven person %s %s from %s (proxy pid %d)", r.Method, r.URL.Path, r.RemoteAddr, pid)
	if mode == humanProofReport {
		callerLogf(r, "authority: would refuse unproven person %s %s from %s (proxy pid %d)", r.Method, r.URL.Path, r.RemoteAddr, pid)
	}
	return callerIdentity{Kind: callerHuman, PID: pid, Unproven: true}
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
	// What the daemon started is the daemon's, whatever its environment
	// says: a worker is labelled before any SOLU_PANE_ID is read (ST-10 A2),
	// so `env SOLU_PANE_ID=<pane> …` run by a worker claims nothing.
	if spawnedBySelf {
		return callerIdentity{Kind: callerWorker, PID: pid}, true
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
	return callerIdentity{}, false
}

// browserProcessNames are the processes (or their parents) that make a
// browser's network requests: Chromium-family network services, Firefox's
// parent and socket processes, WebKit's network process, and the Windows
// desktop shell's WebView2. On macOS the names are the executables' file
// names (procinfo_darwin.go): the Chromium "… Helper" network services.
// WebKit's network service is not here: see peerIsSharedNetworkService.
// Each platform adds its own exact names (platformBrowserProcessNames).
//
// The list is a convenience, not a boundary: a renamed binary passes it.
// It keeps the browsers people use working without a signature in off and
// report; require mode, an approved device's signature, is the boundary.
var browserProcessNames = map[string]bool{
	"chrome": true, "chromium": true, "chromium-browse": true, "msedge": true,
	"msedgewebview2": true, "brave": true, "vivaldi-bin": true, "opera": true,
	"firefox": true, "firefox-bin": true, "firefox-esr": true, "socket process": true,
	"webkitnetworkpr": true, "epiphany": true, "solubrowse": true,
	"google chrome": true, "google chrome helper": true, "google chrome canary": true,
	"google chrome canary helper": true, "chromium helper": true, "microsoft edge": true,
	"microsoft edge helper": true, "brave browser": true, "brave browser helper": true,
	"vivaldi": true, "vivaldi helper": true, "opera helper": true, "safari": true,
	// The kernel's 16-byte p_comm, the fallback when the executable path
	// can't be read.
	"google chrome he": true, "google chrome ca": true,
	"microsoft edge h": true, "brave browser he": true,
}

// isBrowserProcessName reports a lower-cased executable name (without .exe)
// from either list.
func isBrowserProcessName(name string) bool {
	return browserProcessNames[name] || platformBrowserProcessNames[name]
}

// peerIsBrowser reports whether pid or one of its near parents is a browser.
// A variable so tests can stand in a browser process.
var peerIsBrowser = func(pid int) bool {
	cur := pid
	for depth := 0; depth < 6 && cur > 1; depth++ {
		name := strings.ToLower(strings.TrimSpace(readComm(cur)))
		name = strings.TrimSuffix(name, ".exe")
		if isBrowserProcessName(name) {
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

// sharedNetworkServiceNames are macOS's WebKit network service (full name
// and the kernel's 16-byte p_comm). launchd starts it for whichever app hosts
// a web view, Safari, the desktop app or a WKWebView any program creates, so
// its connections can't be tied to an app: they are judged like untraceable
// ones (untracedCaller), where an approved device's signature decides.
var sharedNetworkServiceNames = map[string]bool{"com.apple.webkit.networking": true, "com.apple.webkit": true}

// peerIsSharedNetworkService reports a peer that carries several apps'
// requests. A variable so tests can stand one in.
var peerIsSharedNetworkService = func(pid int) bool {
	return sharedNetworkServiceNames[strings.TrimSuffix(strings.ToLower(strings.TrimSpace(readComm(pid))), ".exe")]
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
// widen what agents may do; agent authority never delegates them. Optional
// MCP tool names direct agents to the tool that owns the operation and its
// ownership/confirmation checks instead of duplicating those rules in REST.
func (a *apiServer) requireHuman(w http.ResponseWriter, r *http.Request, action string, mcpTools ...string) bool {
	id := a.identifyCaller(r)
	if id.Kind == callerHuman {
		a.logPersonAct(id, action)
		return true
	}
	a.refuseCaller(w, id, action, false, mcpTools...)
	return false
}

// logPersonAct records an act the person was taken to make. One taken only
// from browser headers is counted, so report mode shows what require would
// refuse.
func (a *apiServer) logPersonAct(id callerIdentity, action string) {
	if id.Unproven {
		log.Printf("authority: %s by a person without device proof (pid %d, automation %t; refused when approved devices are required)", action, id.PID, id.Automation)
		a.humanProof.noteUnproven(action, time.Now())
		return
	}
	log.Printf("authority: %s by a person (pid %d, relay %t, device %q)", action, id.PID, id.Relay, id.Device)
}

// requireHumanOrDelegated also admits an interactive pane while the person has
// turned on agent authority. Workers and unverified programs are always
// refused.
func (a *apiServer) requireHumanOrDelegated(w http.ResponseWriter, r *http.Request, action string) bool {
	id := a.identifyCaller(r)
	switch id.Kind {
	case callerHuman:
		a.logPersonAct(id, action)
		return true
	case callerPane:
		// A request forwarded for a remote device never gets the grant.
		if !id.Forwarded && a.agentAuthority.active(time.Now()) {
			log.Printf("authority: pane %s (pid %d) may %s: agent authority is on", id.PaneID, id.PID, action)
			return true
		}
	}
	a.refuseCaller(w, id, action, true)
	return false
}

func (a *apiServer) refuseCaller(w http.ResponseWriter, id callerIdentity, action string, delegable bool, mcpTools ...string) {
	if action != "create or save a workspace" || a.workspaceSaveLogs.allow(id, time.Now()) {
		log.Printf("authority: refused %s caller=%s pane=%s pid=%d", action, id.Kind, id.PaneID, id.PID)
	}
	var msg string
	errCode := "human_required"
	switch {
	case id.Unproven && id.Automation:
		msg = fmt.Sprintf("This browser is run by automation, so it can't %s unless it is an approved device.", action)
	case id.Unproven:
		errCode = "device_required"
		msg = fmt.Sprintf("This screen isn't an approved device yet, so it can't %s. Open Settings → Privacy & security → Ask to approve this browser, then approve it from a browser or app you've already approved. If that button isn't shown here, use an already-approved device instead. If you're locked out, recovery steps are in Settings → Privacy & security on the computer.", action)
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
	if len(mcpTools) > 0 {
		msg += " Agents must use the MCP tool " + strings.Join(mcpTools, " or ") + ", which applies its ownership and in-app Allow rules."
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   errCode,
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
// a prompt answers it, so an agent without the person's grant must not.
// Callers identified as agents are held back. An unverified caller (one that
// can't be traced, such as a Windows browser reaching a WSL daemon) is held
// back only when approved devices are required (human_proof.go), since the
// person's own sockets are then signed; otherwise its keystrokes go through so
// the person's are never dropped.
func (a *apiServer) promptInputGate(r *http.Request) func(*Pane) bool {
	return a.promptInputGateFor(a.identifyCaller(r))
}

// promptTakesKeys is what every text-delivery route means by a pane being at a
// prompt: keys would land in a live menu or in a dialog still drawn after input
// (menuInputDrawn), which computeState already counts as answered.
func promptTakesKeys(p *Pane) bool {
	return p.menuInputState() != menuInputNone
}

func (a *apiServer) promptInputGateFor(id callerIdentity) func(*Pane) bool {
	hold := a.promptHoldFor(id)
	ownPossible := id.Kind == callerPane && id.PaneID != ""
	if hold == nil && !ownPossible {
		return func(*Pane) bool { return true }
	}
	return func(p *Pane) bool {
		if p == nil || !promptInOwnPane(id, p) && (hold == nil || !hold()) {
			return true
		}
		if !promptTakesKeys(p) {
			return true
		}
		notePromptHeld(id, p)
		return false
	}
}

// promptHoldFor reports, at each input, whether the caller's keys are held
// back at a permission prompt; nil when they never are.
func (a *apiServer) promptHoldFor(id callerIdentity) func() bool {
	agent := id.Kind == callerPane || id.Kind == callerWorker
	// An unverified caller (untraced, and no browser headers either) is
	// never the person's keys at a prompt, in any mode: it is a program.
	unverified := id.Kind == callerUnverified
	// One taken for the person from browser headers alone (the person's own
	// untraceable desktop, for one) is held back like an agent only while
	// approved devices are required: their sockets are then signed. A socket
	// lives long, so this is decided on each input, not when it opened.
	unproven := id.Kind == callerHuman && id.Unproven
	// A socket signed by a device keeps that standing only while the device
	// stays approved: removing it withdraws the socket's say at prompts too.
	signed := id.Kind == callerHuman && id.Device != ""
	if !agent && !unverified && !unproven && !signed {
		return nil
	}
	return func() bool {
		if signed && a.deviceApproved(id.Device) {
			return false
		}
		if unverified {
			return true
		}
		if !agent {
			return a.humanProofMode() == humanProofRequire
		}
		// The same decision as answer_prompt (ST-10). Its own pane's prompts
		// a pane never answers (promptInOwnPane).
		return !a.decidePromptAnswer(mcpCallMeta{id: id}, false, nil).allowed
	}
}

// errPromptHeld: a held caller's keys at a permission prompt.
var errPromptHeld = errors.New("the pane is showing a permission prompt; only the person can answer it")

// promptWriteCheck is the prompt gate at write time, run under the writer
// lock with the write: a menu may have opened since the gate. A dialog still
// drawn after its first answer counts too (promptTakesKeys), where the next key
// answers its next step. Nil when b can pass: the caller is never held, or b
// is only the terminal's own replies.
func (a *apiServer) promptWriteCheck(id callerIdentity, p *Pane, b []byte, via string) func() error {
	hold := a.promptHoldFor(id)
	own := promptInOwnPane(id, p)
	if hold == nil && !own || isTerminalReplyOnly(b) {
		return nil
	}
	return func() error {
		if !promptTakesKeys(p) {
			return nil
		}
		if own || hold() {
			notePromptHeld(id, p)
			return errPromptHeld
		}
		notePromptAnsweredBy(id, p, via)
		return nil
	}
}

// promptInOwnPane: the caller is a pane's process writing to its own pane.
// A pane never answers its own prompt, traced or not, with or without the
// grant (ST-10 rule 4).
func promptInOwnPane(id callerIdentity, p *Pane) bool {
	return p != nil && id.Kind == callerPane && id.PaneID != "" && id.PaneID == p.ID
}

// promptAnswers rate-limits the log of an agent's keys answering a prompt
// to one line per pane, caller and prompt.
var promptAnswers = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// notePromptAnsweredBy logs an agent's keys passing at a prompt under the
// grant (ST-10 rule 7): caller kind, transport, pane, target and prompt.
func notePromptAnsweredBy(id callerIdentity, p *Pane, via string) {
	if id.Kind != callerPane {
		return
	}
	key := fmt.Sprintf("%s|%s|%d", p.ID, id.PaneID, p.inputRevision.Load())
	promptAnswers.Lock()
	if promptAnswers.seen[key] {
		promptAnswers.Unlock()
		return
	}
	if len(promptAnswers.seen) >= forwardedRefusalsMax {
		promptAnswers.seen = map[string]bool{}
	}
	promptAnswers.seen[key] = true
	promptAnswers.Unlock()
	logPromptAct(id.Kind, via, id.PaneID, p.ID, promptRevID(p), "keys at a prompt")
}

// promptHelds rate-limits the log of keys held at a prompt to one line per
// pane, caller and prompt: a refused key leaves the input revision as it
// was, and answering the prompt moves it on.
var promptHelds = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

func notePromptHeld(id callerIdentity, p *Pane) {
	key := fmt.Sprintf("%s|%s|%d|%d", p.ID, id.Kind, id.PID, p.inputRevision.Load())
	promptHelds.Lock()
	if promptHelds.seen[key] {
		promptHelds.Unlock()
		return
	}
	if len(promptHelds.seen) >= forwardedRefusalsMax {
		promptHelds.seen = map[string]bool{}
	}
	promptHelds.seen[key] = true
	promptHelds.Unlock()
	log.Printf("authority: refused %s input at a permission prompt on pane %s (pid %d, pane caller %q)", id.Kind, p.ID, id.PID, id.PaneID)
}

// paneSocketInputPolicy classifies a pane terminal socket once, at upgrade.
// The prompt gate is unchanged; human is the daemon's own classification and
// is what lets a visible viewer adopt an unowned pane by typing (handleWS).
// Panes, workers and unverified local programs never adopt.
func (a *apiServer) paneSocketInputPolicy(r *http.Request) wsInputPolicy {
	return a.socketInputPolicyFor(a.identifyCaller(r))
}

// socketInputPolicyFor is a socket's policy from its caller, classified once
// from the upgrade request. What can change while it is open (the mode, a
// device's approval, the agent grant) is read again at each frame.
func (a *apiServer) socketInputPolicyFor(id callerIdentity) wsInputPolicy {
	policy := wsInputPolicy{
		allowed: a.promptInputGateFor(id),
		human:   id.Kind == callerHuman,
		kind:    string(id.Kind),
		promptCheck: func(p *Pane, b []byte) func() error {
			return a.promptWriteCheck(id, p, b, "pane-socket")
		},
	}
	if id.Forwarded {
		policy.viewOnly = func(p *Pane, what string) bool {
			if !a.forwardedUnsigned(id) {
				return false
			}
			if what != "" { // "" is the sizing check: nothing was refused
				noteForwardedRefusal(id, "pane "+p.ID, what)
			}
			return true
		}
	}
	return policy
}

// forwardedUnsigned reports a caller that may only view: its request came
// through the cloud's /m/ route for a remote device (Forwarded) with no
// device proof that is still approved now. Such a caller reads panes but
// never writes to one or changes anything (forwardedViewOnly, the pane
// socket, the MCP writing tools). A private relay channel's device and a
// local device signature are proof while the device stays approved, checked
// at each write; the phone app over the relay is the remote-access policy's
// to govern, as before.
func (a *apiServer) forwardedUnsigned(id callerIdentity) bool {
	switch {
	case !id.Forwarded:
		return false
	case id.PrivateDevice != "":
		return !a.deviceApproved(id.PrivateDevice)
	case id.Relay:
		return false
	case id.Device != "":
		return !a.deviceApproved(id.Device)
	}
	return true
}

func (a *apiServer) deviceApproved(device string) bool {
	return a.trustedDevices != nil && a.trustedDevices.isApproved(device)
}

// forwardedViewOnlyMessage is what a view-only caller is told.
const forwardedViewOnlyMessage = "This device can only view this computer's panes. Typing and changes need the computer itself, or a device approved in Settings → Privacy & security."

// forwardedViewOnlyExempt: starting to enrol as a device (the request, and
// V3's commit reveal) only creates a pending request that an approved device
// must approve, so a view-only device may make it and so become signing.
// Approving or confirming one stays refused.
func forwardedViewOnlyExempt(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path == "/api/remote-devices/enroll" || r.URL.Path == "/api/remote-devices/enroll/reveal"
}

// forwardedViewOnlyAll: routes refused to a view-only caller whatever the
// method. /mcp is the agents' tools, reading ones included, and those read
// files with no relay confinement: a view-only phone has no use for them.
func forwardedViewOnlyAll(path string) bool {
	return path == "/mcp"
}

// forwardedViewOnly refuses every mutating request from a view-only caller
// (forwardedUnsigned), and every request to /mcp: default-deny, so a new
// route is covered without being listed. A request without the marker can't
// be Forwarded, so only a marked one pays for classifying the caller.
func (a *apiServer) forwardedViewOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if !forwardedViewOnlyAll(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if forwardedViewOnlyExempt(r) || !isCloudForwardedPrefix(r.Header.Get("X-Forwarded-Prefix")) {
			next.ServeHTTP(w, r)
			return
		}
		if id := a.identifyCaller(r); a.forwardedUnsigned(id) {
			noteForwardedRefusal(id, forwardedRefusalTarget(r.URL.Path), r.Method+" "+r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "view_only", "message": forwardedViewOnlyMessage})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// forwardedRefusalTarget is the pane a path acts on, or the path.
func forwardedRefusalTarget(path string) string {
	if rest, ok := strings.CutPrefix(path, "/api/panes/"); ok {
		if id, _, _ := strings.Cut(rest, "/"); id != "" {
			return "pane " + id
		}
	}
	return path
}

// forwardedRefusals rate-limits the refusal log to one line per caller and
// pane (or path): a view-only phone typing would otherwise log every key.
var forwardedRefusals = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

const forwardedRefusalsMax = 4096

func noteForwardedRefusal(id callerIdentity, target, what string) {
	key := fmt.Sprintf("%d|%s|%s|%s", id.PID, id.Device, id.PrivateDevice, target)
	forwardedRefusals.Lock()
	if forwardedRefusals.seen[key] {
		forwardedRefusals.Unlock()
		return
	}
	if len(forwardedRefusals.seen) >= forwardedRefusalsMax {
		forwardedRefusals.seen = map[string]bool{}
	}
	forwardedRefusals.seen[key] = true
	forwardedRefusals.Unlock()
	log.Printf("authority: refused %s on %s: a forwarded request without device proof is view-only (pid %d, device %q, private %q)", what, target, id.PID, id.Device, id.PrivateDevice)
}
