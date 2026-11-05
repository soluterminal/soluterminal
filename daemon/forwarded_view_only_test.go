package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// viewOnlyCaller is one kind of caller in the view-only tables, as
// identifyCaller would classify it.
type viewOnlyCaller struct {
	name     string
	id       callerIdentity
	viewOnly bool
}

const (
	testSignedDevice  = "dev_signed_t2"
	testPrivateDevice = "dev_private_channel"
	testRemovedDevice = "dev_removed"
)

var viewOnlyCallers = []viewOnlyCaller{
	{"marked unsigned", callerIdentity{Kind: callerUnverified, Unproven: true, Forwarded: true}, true},
	{"marked signed", callerIdentity{Kind: callerHuman, Device: testSignedDevice, Forwarded: true}, false},
	{"marked signed, device removed", callerIdentity{Kind: callerHuman, Device: testRemovedDevice, Forwarded: true}, true},
	{"private channel", callerIdentity{Kind: callerHuman, Relay: true, Forwarded: true, PrivateDevice: testPrivateDevice}, false},
	{"private channel, device removed", callerIdentity{Kind: callerHuman, Relay: true, Forwarded: true, PrivateDevice: testRemovedDevice}, true},
	{"relay", callerIdentity{Kind: callerHuman, Relay: true, Forwarded: true}, false},
	{"no marker, unverified", callerIdentity{Kind: callerUnverified}, false},
	{"no marker, person", callerIdentity{Kind: callerHuman}, false},
	{"no marker, unproven person", callerIdentity{Kind: callerHuman, Unproven: true}, false},
	{"marked pane", callerIdentity{Kind: callerPane, PaneID: "lead", Forwarded: true}, true},
}

// viewOnlyText is in the refusal, as JSON escapes it too.
const viewOnlyText = "can only view this computer's panes"

var humanProofModes = []string{humanProofOff, humanProofReport, humanProofRequire}

// viewOnlyServer has two approved devices (a signed T2 device and a private
// channel's) and the given human-proof mode.
func viewOnlyServer(t *testing.T, mode string) *apiServer {
	t.Helper()
	a := &apiServer{metrics: NewMetrics(), agentAuthority: newAgentAuthority(t.TempDir())}
	a.trustedDevices = newTrustedDevices(t.TempDir())
	seedApprovedDevice(a, testSignedDevice)
	seedApprovedDevice(a, testPrivateDevice)
	a.humanProof = newHumanProof(t.TempDir())
	if err := a.humanProof.set(mode, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := a.humanProofMode(); got != mode {
		t.Fatalf("mode = %s, want %s", got, mode)
	}
	return a
}

func seedApprovedDevice(a *apiServer, id string) {
	s := a.trustedDevices
	s.mu.Lock()
	s.record.Devices = append(s.record.Devices, &trustedDevice{ID: id, Status: deviceStatusApproved})
	s.mu.Unlock()
}

func dropTestDevice(a *apiServer, id string) {
	s := a.trustedDevices
	s.mu.Lock()
	kept := s.record.Devices[:0]
	for _, d := range s.record.Devices {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	s.record.Devices = kept
	s.mu.Unlock()
}

func callAsIdentity(t *testing.T, id callerIdentity) {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return id }
	t.Cleanup(func() { identifyCallerHook = prev })
}

// mutatingRoutePatterns are every route pattern registered on the API mux
// whose method can change something: POST, PUT, PATCH, DELETE, or none (any
// method). router.go serves the desktop router's own mux, not this API.
func mutatingRoutePatterns(t *testing.T) (mutating, reading []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\bmux\.Handle(?:Func)?\(\s*"([^"]+)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "router.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			method, path, hasMethod := strings.Cut(m[1], " ")
			readingMethod := hasMethod && (method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions)
			if hasMethod && path == "" {
				// "GET "+somePath: the path is built in code.
				if !readingMethod {
					t.Fatalf("%s: a mutating route whose path the scan can't read; write it out", f)
				}
				continue
			}
			if readingMethod {
				reading = append(reading, m[1])
				continue
			}
			mutating = append(mutating, m[1])
		}
	}
	sort.Strings(mutating)
	return mutating, reading
}

var routeWildcard = regexp.MustCompile(`\{[^}]*\}`)

// routeRequest is a request matching pattern: wildcards filled in, POST for
// a pattern without a method.
func routeRequest(pattern string) *http.Request {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = http.MethodPost, pattern
	}
	path = routeWildcard.ReplaceAllString(path, "x")
	if strings.HasSuffix(path, "/") {
		path += "x"
	}
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("X-Forwarded-Prefix", "/m/home")
	return r
}

// The guard: every mutating route, a new one included, refuses a view-only
// caller before its handler runs, in every mode; every other caller reaches
// it, and every reading route serves everyone.
func TestViewOnlyGuardCoversEveryMutatingRoute(t *testing.T) {
	mutating, reading := mutatingRoutePatterns(t)
	if len(mutating) < 100 || len(reading) < 50 {
		t.Fatalf("found %d mutating and %d reading routes: the pattern scan is broken", len(mutating), len(reading))
	}
	for _, mode := range humanProofModes {
		a := viewOnlyServer(t, mode)
		for _, c := range viewOnlyCallers {
			callAsIdentity(t, c.id)
			reached := false
			h := a.forwardedViewOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			for _, pattern := range mutating {
				reached = false
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, routeRequest(pattern))
				refused := rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "view_only")
				want := c.viewOnly && !forwardedViewOnlyExempt(routeRequest(pattern))
				if refused != want || reached == refused {
					t.Fatalf("%s, %s: %s refused=%v reached=%v, want refused=%v", mode, c.name, pattern, refused, reached, want)
				}
			}
			for _, pattern := range reading {
				reached = false
				h.ServeHTTP(httptest.NewRecorder(), routeRequest(pattern))
				if !reached {
					t.Fatalf("%s, %s: reading route %s was refused", mode, c.name, pattern)
				}
			}
		}
	}
}

// The view-only check wraps the whole API, outside the team gate, and main
// serves every transport through it.
func TestTheAPIHandlerIsViewOnlyForForwardedUnsignedCallers(t *testing.T) {
	a := viewOnlyServer(t, humanProofOff)
	callAsIdentity(t, viewOnlyCallers[0].id)
	mux := http.NewServeMux()
	a.routes(mux)
	rec := httptest.NewRecorder()
	a.apiHandler(mux).ServeHTTP(rec, routeRequest("POST /api/panes/{id}/inject"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "view_only") {
		t.Fatalf("inject through apiHandler = %d %s", rec.Code, rec.Body)
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "workflowHandler := api.apiHandler(mux)") {
		t.Fatal("main.go no longer serves the API through apiHandler")
	}
}

// identifyCaller sets Forwarded from the marker, once, on every transport,
// and names a private channel's device.
func TestIdentifyCallerMarksForwardedRequests(t *testing.T) {
	prev, prevHook := lookupPeerPID, identifyCallerHook
	lookupPeerPID = func(*http.Request) (int, bool) { return 0, false }
	identifyCallerHook = nil // the real classification
	t.Cleanup(func() { lookupPeerPID, identifyCallerHook = prev, prevHook })
	a := viewOnlyServer(t, humanProofOff)
	local := httptest.NewRequest("POST", "/api/panes/x/inject", nil)
	if id := a.identifyCaller(local); id.Forwarded {
		t.Fatalf("no marker: %+v", id)
	}
	for _, prefix := range []string{"/m/home", "/m/home/b/wsl", "/m/home/shinoque"} {
		r := httptest.NewRequest("POST", "/api/panes/x/inject", nil)
		r.Header.Set("X-Forwarded-Prefix", prefix)
		if id := a.identifyCaller(r); !id.Forwarded || id.Kind != callerUnverified || !a.forwardedUnsigned(id) {
			t.Fatalf("%s locally: %+v", prefix, id)
		}
		private := r.WithContext(withPrivateRelayDevice(context.WithValue(r.Context(), relayTransportKey{}, true), testPrivateDevice))
		if id := a.identifyCaller(private); !id.Forwarded || !id.Relay || id.PrivateDevice != testPrivateDevice || a.forwardedUnsigned(id) {
			t.Fatalf("%s on the private channel: %+v", prefix, id)
		}
	}
}

// A view-only caller gets no agent tools at all: /mcp is refused to it
// whatever the method, before the handler (the reading tools read files with
// no relay confinement), and the handler refuses any tool call too. A signed
// forwarded caller is unchanged.
func TestViewOnlyCallersGetNoMCP(t *testing.T) {
	a := viewOnlyServer(t, humanProofOff)
	a.agentAuthority = grantedAgentAuthority(t)
	a.gate = &authGate{token: "fixture-token"}
	a.mgr = NewManager(t.TempDir())
	a.uiBus = newUIBus()
	mcp := a.forwardedViewOnly(http.HandlerFunc(a.mcp))
	request := func(method string, body any) *http.Request {
		var r *http.Request
		if body == nil {
			r = httptest.NewRequest(method, "/mcp", nil)
		} else {
			raw, _ := json.Marshal(body)
			r = httptest.NewRequest(method, "/mcp", bytes.NewReader(raw))
		}
		r.Header.Set("Authorization", "Bearer fixture-token")
		r.Header.Set("X-Forwarded-Prefix", "/m/home")
		return r
	}
	call := func(name string, args map[string]any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
	}
	readSecret := call("read_file", map[string]any{"path": filepath.Join(os.Getenv("HOME"), ".ssh", "id_ed25519")})
	callAsIdentity(t, viewOnlyCallers[0].id)
	for name, r := range map[string]*http.Request{
		"tools/call read_file":  request("POST", readSecret),
		"tools/call git_status": request("POST", call("git_status", map[string]any{"path": "/"})),
		"tools/list":            request("POST", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}),
		"GET stream":            request("GET", nil),
	} {
		rec := httptest.NewRecorder()
		mcp.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "view_only") {
			t.Fatalf("%s for a view-only caller = %d %s", name, rec.Code, rec.Body)
		}
	}
	// The handler on its own refuses any tool too.
	rec := httptest.NewRecorder()
	a.mcp(rec, request("POST", call("search_files", map[string]any{"query": "token"})))
	if !strings.Contains(rec.Body.String(), viewOnlyText) {
		t.Fatalf("the MCP handler gave a view-only caller a tool: %s", rec.Body)
	}
	// A signed forwarded caller still reaches the tools.
	callAsIdentity(t, viewOnlyCallers[1].id)
	rec = httptest.NewRecorder()
	mcp.ServeHTTP(rec, request("POST", call("get_pane_state", map[string]any{"pane_id": "none"})))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), viewOnlyText) {
		t.Fatalf("a signed device was refused /mcp: %d %s", rec.Code, rec.Body)
	}
}

// A view-only device may start enrolling (the request, and V3's reveal):
// that only makes a pending request an approved device must approve. It can't
// approve, verify or confirm one itself.
func TestAViewOnlyDeviceCanStartEnrollingButNotApprove(t *testing.T) {
	a := trustedDevicesTestServer(t)
	a.metrics = NewMetrics()
	a.trustedDevices.record.Policy = remoteAccessApproved
	callAsIdentity(t, viewOnlyCallers[0].id)
	mux := http.NewServeMux()
	a.routes(mux)
	h := a.apiHandler(mux)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Forwarded-Prefix", "/m/home")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	d := newTestDevice(t)
	if rec := send("POST", "/api/remote-devices/enroll", `{"name":"Shinoque Code on Windows","publicKey":"`+d.spki+`","codeVersion":2}`); rec.Code != http.StatusOK {
		t.Fatalf("enrol from a view-only device = %d %s", rec.Code, rec.Body)
	}
	if rec := send("POST", "/api/remote-devices/enroll/reveal", `{}`); strings.Contains(rec.Body.String(), "view_only") {
		t.Fatalf("V3 reveal refused as view-only: %d %s", rec.Code, rec.Body)
	}
	id := deviceID(d.spki)
	for _, path := range []string{"/api/remote-devices/" + id + "/approve", "/api/remote-devices/" + id + "/verify", "/api/remote-devices/" + id + "/verify/confirm"} {
		if rec := send("POST", path, `{}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "view_only") {
			t.Fatalf("%s from a view-only device = %d %s", path, rec.Code, rec.Body)
		}
	}
	if a.trustedDevices.isApproved(id) {
		t.Fatal("a view-only device approved itself")
	}
	if a.trustedDevices.find(id) == nil {
		t.Fatal("the enrolment was not recorded as pending")
	}
}

// socketForCallers serves pane p's terminal socket with each caller's policy,
// chosen by name in the test-only X-Test-Caller header.
func socketForCallers(t *testing.T, a *apiServer, p *Pane, m *Manager) *adoptTestPane {
	t.Helper()
	byName := map[string]callerIdentity{}
	for _, c := range viewOnlyCallers {
		byName[c.name] = c.id
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/panes/{id}/io", handleWS(m, func(r *http.Request) wsInputPolicy {
		return a.socketInputPolicyFor(byName[r.Header.Get("X-Test-Caller")])
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &adoptTestPane{p: p, m: m, srv: srv}
}

// waitClaimDecided waits until the daemon decided the n-th claim on p1.
func waitClaimDecided(t *testing.T, logs *lockedBuffer, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(logs.String(), "ownership: pane=p1 claim reason=") < n {
		if time.Now().After(deadline) {
			t.Fatalf("claim %d was never decided: %q", n, logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// On the pane socket a view-only caller can't take the pane, type, resize or
// redraw, in every mode; every other caller types as before. No prompt is up.
func TestViewOnlySocketTable(t *testing.T) {
	for _, mode := range humanProofModes {
		for _, c := range viewOnlyCallers {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				logs := captureLog(t)
				resetForwardedRefusals()
				h0 := newAdoptTestPane(t, false)
				a := viewOnlyServer(t, mode)
				h := socketForCallers(t, a, h0.p, h0.m)
				conn := h.dial(t, callerKind(c.name), "")
				conn.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
				if c.viewOnly {
					conn.expectOwnership(t, false, "none")
				} else {
					waitClaimDecided(t, logs, 1)
				}
				conn.send(t, websocket.BinaryMessage, "1")
				conn.send(t, websocket.TextMessage, `{"type":"input","data":"\r"}`)
				conn.send(t, websocket.TextMessage, `{"type":"resize","cols":40,"rows":10}`)
				if c.viewOnly {
					h0.expectNoWrite(t)
					if h0.owner() == conn.subID {
						t.Fatal("a view-only socket owns the pane")
					}
					if n := strings.Count(logs.String(), "is view-only"); n != 1 {
						t.Fatalf("refusals logged %d times, want once per caller and pane: %q", n, logs.String())
					}
					return
				}
				h0.expectWrite(t, "1")
				h0.expectWrite(t, "\r")
			})
		}
	}
}

// A device removed while its socket is open loses its input at once: the
// signed T2 device's and the private channel's.
func TestRemovingADeviceMakesItsOpenSocketViewOnly(t *testing.T) {
	for _, c := range []viewOnlyCaller{viewOnlyCallers[1], viewOnlyCallers[3]} {
		t.Run(c.name, func(t *testing.T) {
			h0 := newAdoptTestPane(t, false)
			a := viewOnlyServer(t, humanProofOff)
			h := socketForCallers(t, a, h0.p, h0.m)
			conn := h.dial(t, callerKind(c.name), "")
			conn.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			conn.expectOwnership(t, true, "you")
			conn.send(t, websocket.BinaryMessage, "a")
			h0.expectWrite(t, "a")
			dropTestDevice(a, c.id.Device+c.id.PrivateDevice)
			conn.send(t, websocket.BinaryMessage, "b")
			h0.expectNoWrite(t)
		})
	}
}

// injectAtPrompt is /inject through the whole API (view-only guard, routes,
// prompt gate) into a pane showing a Claude permission menu.
func injectAtPrompt(t *testing.T, mode string, c viewOnlyCaller) (int, string, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames := newTypingHoldPane(t, "pane_view_only_inject")
	seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
	if p.computeState() != StateChoicePrompt {
		t.Fatal("fixture is not a menu on screen")
	}
	a := viewOnlyServer(t, mode)
	a.mgr = testManagerWithPane(p)
	a.uiBus = newUIBus()
	callAsIdentity(t, c.id)
	mux := http.NewServeMux()
	a.routes(mux)
	req := routeRequest("POST /api/panes/" + p.ID + "/inject")
	req.Body = http.NoBody
	body, _ := json.Marshal(map[string]string{"text": "1"})
	req = httptest.NewRequest("POST", "/api/panes/"+p.ID+"/inject", bytes.NewReader(body))
	if c.id.Forwarded {
		req.Header.Set("X-Forwarded-Prefix", "/m/home")
	}
	rec := httptest.NewRecorder()
	a.apiHandler(mux).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), frames
}

// At a permission prompt, /inject: view-only callers and unverified programs
// are refused in every mode; a signed device, the private channel, the relay
// and the person answer; the person's untraceable desktop (browser headers)
// keeps today's rule, held only under require.
func TestInjectAtAPromptTable(t *testing.T) {
	for _, mode := range humanProofModes {
		for _, c := range viewOnlyCallers {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				code, body, frames := injectAtPrompt(t, mode, c)
				var want string
				switch {
				case c.viewOnly:
					want = "view_only"
				case c.id.Kind == callerUnverified:
					// A program, untraced and without browser headers: held
					// at a prompt in every mode.
					want = "human_required"
				case c.id.Unproven && mode == humanProofRequire:
					want = "human_required"
				}
				if want != "" {
					if code != http.StatusForbidden || !strings.Contains(body, want) {
						t.Fatalf("inject = %d %s, want %s", code, body, want)
					}
					expectNoFrame(t, frames, "nothing typed")
					return
				}
				if code != http.StatusOK {
					t.Fatalf("inject = %d %s, want it typed", code, body)
				}
				expectFrame(t, frames, "the answer", "1")
			})
		}
	}
}

// The prompt gate holds at write time and sees a dialog still drawn after its
// first answer (menuInputDrawn), for every caller kind it holds; the cached
// state counts that dialog answered.
func TestPromptWriteCheckCoversADrawnMenu(t *testing.T) {
	p, frames := newTypingHoldPane(t, "pane_drawn_menu")
	seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
	typeMenuSteps(t, p, frames, []menuStep{{"\x1b[B", questionFocused(2, "")}})
	if got := p.menuInputState(); got != menuInputDrawn {
		t.Fatalf("fixture: menu input state %d, want drawn", got)
	}
	a := viewOnlyServer(t, humanProofRequire)
	held := []callerIdentity{
		{Kind: callerPane, PaneID: "lead"},
		{Kind: callerWorker},
		{Kind: callerUnverified},
		{Kind: callerHuman, Unproven: true},
		{Kind: callerHuman, Device: testRemovedDevice},
	}
	for _, id := range held {
		if check := a.promptWriteCheck(id, p, []byte("1"), "pane-socket"); check == nil || check() == nil {
			t.Fatalf("%+v not held at a drawn menu", id)
		}
		if check := a.promptWriteCheck(id, p, []byte("\x1b[I"), "pane-socket"); check != nil {
			t.Fatalf("%+v: a focus report must pass", id)
		}
	}
	for _, id := range []callerIdentity{{Kind: callerHuman}, {Kind: callerHuman, Device: testSignedDevice}} {
		if check := a.promptWriteCheck(id, p, []byte("1"), "pane-socket"); check != nil && check() != nil {
			t.Fatalf("%+v held at a drawn menu", id)
		}
	}
}

// A request forwarded for a remote device never gets the person's grant to
// agents, even traced to a pane.
func TestForwardedCallersNeverGetTheAgentGrant(t *testing.T) {
	a := viewOnlyServer(t, humanProofOff)
	a.agentAuthority = grantedAgentAuthority(t)
	pane := callerIdentity{Kind: callerPane, PaneID: "lead"}
	marked := pane
	marked.Forwarded = true
	if hold := a.promptHoldFor(pane); hold() {
		t.Fatal("an unmarked pane with the grant is held")
	}
	if hold := a.promptHoldFor(marked); !hold() {
		t.Fatal("a marked pane got the grant at a prompt")
	}
	if a.decidePromptAnswer(mcpCallMeta{id: marked, transport: "tcp"}, false, nil).allowed || !a.decidePromptAnswer(mcpCallMeta{id: pane, transport: "tcp"}, false, nil).allowed {
		t.Fatal("MCP prompt answers ignore the marker")
	}
	for _, c := range []struct {
		id   callerIdentity
		want int
	}{{pane, http.StatusOK}, {marked, http.StatusForbidden}} {
		callAsIdentity(t, c.id)
		rec := httptest.NewRecorder()
		ok := a.requireHumanOrDelegated(rec, httptest.NewRequest("POST", "/x", nil), "test")
		if got := map[bool]int{true: http.StatusOK, false: http.StatusForbidden}[ok]; got != c.want {
			t.Fatalf("%+v: delegated = %v", c.id, ok)
		}
	}
}

// A schedule run from a view-only caller is refused and nothing runs; the
// REST schedule routes are the person's (agents use the MCP tools).
func TestFiringAScheduleFromAMarkedCallerRunsNothing(t *testing.T) {
	p, frames := newTypingHoldPane(t, "pane_view_only_schedule")
	a := viewOnlyServer(t, humanProofOff)
	a.mgr = testManagerWithPane(p)
	store, err := NewScheduleStore(t.TempDir(), a.mgr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	a.scheds = store
	enabled := false
	sched, err := store.Create(ScheduleInput{Name: "cleanup", Kind: "prompt", Cron: "0 3 * * *", PaneID: p.ID, Prompt: "rm -rf ./build", Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.routes(mux)
	fire := func(id callerIdentity, marked bool) *httptest.ResponseRecorder {
		callAsIdentity(t, id)
		req := httptest.NewRequest("POST", "/api/schedules/"+sched.ID+"/fire", nil)
		if marked {
			req.Header.Set("X-Forwarded-Prefix", "/m/home")
		}
		rec := httptest.NewRecorder()
		a.apiHandler(mux).ServeHTTP(rec, req)
		return rec
	}
	if rec := fire(viewOnlyCallers[0].id, true); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "view_only") {
		t.Fatalf("fire from a marked caller = %d %s", rec.Code, rec.Body)
	}
	if rec := fire(callerIdentity{Kind: callerPane, PaneID: "lead"}, false); rec.Code != http.StatusForbidden {
		t.Fatalf("fire over REST from an agent = %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "nothing typed")
	if runs := store.List()[0].Runs; len(runs) != 0 {
		t.Fatalf("a refused fire ran: %+v", runs)
	}
}

func resetForwardedRefusals() {
	forwardedRefusals.Lock()
	forwardedRefusals.seen = map[string]bool{}
	forwardedRefusals.Unlock()
}

// messagePane is a Codex pane whose delivery screen and draft guard read an
// empty input box, so only the real grid (a menu, when seeded) can stop a
// message.
func messagePane(t *testing.T, id string) (*Pane, <-chan directAutoFrameRead) {
	t.Helper()
	p, frames := newTypingHoldPane(t, id)
	idle := codex0166Screen(t, "idle")
	var screen, guard atomic.Value
	screen.Store(idle)
	stubDeliveryScreen(t, &screen)
	guard.Store(strings.Replace(idle, "› Ask Codex to do anything", "›", 1))
	stubDraftGuard(t, p, &guard)
	return p, frames
}

// An agent's message is not typed into a dialog still drawn after its first
// answer: the cached state counts it answered, the write-time check doesn't.
func TestAgentMessageIsNeverTypedIntoADrawnMenu(t *testing.T) {
	p, frames := messagePane(t, "pane_message_drawn_menu")
	seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
	typeMenuSteps(t, p, frames, []menuStep{{"\x1b[B", questionFocused(2, "")}})
	if got := p.menuInputState(); got != menuInputDrawn {
		t.Fatalf("fixture: menu input state %d, want drawn", got)
	}
	_, _, err := deliverAgentMessage(p, "[from @a] please review", false, deliveryNoSteer, "@a")
	if !errors.Is(err, errDeliveryChoicePrompt) {
		t.Fatalf("err = %v, want the menu refusal", err)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// A menu that opens between the paste and the Enter gets no Enter: the
// message waits in the box for the person.
func TestAMenuOpeningAfterThePasteGetsNoEnter(t *testing.T) {
	p, frames := messagePane(t, "pane_message_menu_after_paste")
	var once sync.Once
	prevSleep := deliverySleep
	deliverySleep = func(d time.Duration) {
		once.Do(func() { seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen) })
		prevSleep(d)
	}
	_, outcome, err := deliverAgentMessage(p, "[from @a] please review", false, deliveryNoSteer, "@a")
	if err != nil || outcome != deliveryLeftInComposer {
		t.Fatalf("outcome = %q, err = %v; want it left in the box", outcome, err)
	}
	expectFrame(t, frames, "the paste", "\x1b[200~[from @a] please review\x1b[201~")
	expectNoFrame(t, frames, "no Enter")
}

// desiredSize is the size the pane's subscribers vote for.
func desiredSize(p *Pane) (uint16, uint16) {
	p.subsMu.RLock()
	defer p.subsMu.RUnlock()
	return p.desiredSizeLocked()
}

// A view-only socket never sets the shared terminal's size: not by the size
// it connects with as a phone, nor by being active or visible; a device
// removed mid-session stops voting too. A phone over the relay still does.
func TestAViewOnlySocketNeverSetsThePaneSize(t *testing.T) {
	for _, c := range []struct {
		caller viewOnlyCaller
		votes  bool
	}{{viewOnlyCallers[0], false}, {viewOnlyCallers[5], true}, {viewOnlyCallers[1], true}} {
		t.Run(c.caller.name, func(t *testing.T) {
			h0 := newAdoptTestPane(t, false)
			a := viewOnlyServer(t, humanProofOff)
			h := socketForCallers(t, a, h0.p, h0.m)
			person := h.dial(t, callerKind(viewOnlyCallers[7].name), "?cols=120&rows=40")
			person.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
			person.expectOwnership(t, true, "you")
			phone := h.dial(t, callerKind(c.caller.name), "?cols=40&rows=20&mobile=1")
			phone.send(t, websocket.TextMessage, `{"type":"visibility","visible":true}`)
			phone.send(t, websocket.TextMessage, `{"type":"active"}`)
			deadline := time.Now().Add(2 * time.Second)
			for {
				cols, rows := desiredSize(h0.p)
				phoneSized := cols == 40 && rows == 20
				if phoneSized == c.votes && (c.votes || cols == 120) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("size %dx%d, phone votes %v", cols, rows, c.votes)
				}
				time.Sleep(time.Millisecond)
			}
			if c.caller.id.Device == "" {
				return
			}
			dropTestDevice(a, c.caller.id.Device)
			// The removed device's sub no longer votes: only the bounded
			// reconnect hold on a phone's last width is left, and once it
			// has aged out the person's size applies.
			desiredSize(h0.p)
			h0.p.mobileSizeMu.Lock()
			held := h0.p.lastSizeHeld
			h0.p.lastMobileAt = time.Now().Add(-mobileSizeHoldWindow)
			h0.p.mobileSizeMu.Unlock()
			if !held {
				t.Fatal("the removed device's phone still voted instead of being held")
			}
			if cols, rows := desiredSize(h0.p); cols != 120 || rows != 40 {
				t.Fatalf("after the device was removed and the hold aged out: %dx%d, want the person's 120x40", cols, rows)
			}
		})
	}
}

// At a live permission prompt on the pane socket, an unverified program's
// keys are held in every mode, logged once per pane and prompt; the person's
// untraceable desktop (browser headers) answers unless approved devices are
// required. With no prompt up, both type (TestViewOnlySocketTable).
func TestAnUnverifiedProgramIsHeldAtAPromptOnTheSocket(t *testing.T) {
	for _, mode := range humanProofModes {
		for _, c := range []struct {
			caller viewOnlyCaller
			held   bool
		}{{viewOnlyCallers[6], true}, {viewOnlyCallers[8], mode == humanProofRequire}} {
			t.Run(mode+"/"+c.caller.name, func(t *testing.T) {
				logs := captureLog(t)
				promptHelds.Lock()
				promptHelds.seen = map[string]bool{}
				promptHelds.Unlock()
				p, frames := newTypingHoldPane(t, "p1")
				seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
				if p.computeState() != StateChoicePrompt {
					t.Fatal("fixture is not a menu on screen")
				}
				a := viewOnlyServer(t, mode)
				h := socketForCallers(t, a, p, testManagerWithPane(p))
				conn := h.dial(t, callerKind(c.caller.name), "")
				conn.send(t, websocket.TextMessage, `{"type":"claim","reason":"takeover"}`)
				conn.expectOwnership(t, true, "you")
				conn.send(t, websocket.BinaryMessage, "1")
				conn.send(t, websocket.BinaryMessage, "1")
				if !c.held {
					expectFrame(t, frames, "the answer", "1")
					return
				}
				expectNoFrame(t, frames, "nothing typed")
				if n := strings.Count(logs.String(), "input at a permission prompt on pane p1"); n != 1 {
					t.Fatalf("held keys logged %d times, want once per pane and prompt: %q", n, logs.String())
				}
			})
		}
	}
}

// Send on an unsent message is held for an unverified program at a prompt.
func TestUnsentSendFromAnUnverifiedProgramIsRefusedAtAPrompt(t *testing.T) {
	p, frames := newTypingHoldPane(t, "pane_unsent_at_prompt")
	seedGridMenu(t, p, claudeAskUserQuestionWrappedScreen)
	p.recordUnsentMessage("@a", "please review", false, p.inputRevision.Load())
	a := viewOnlyServer(t, humanProofOff)
	a.mgr = testManagerWithPane(p)
	a.uiBus = newUIBus()
	callAsIdentity(t, viewOnlyCallers[6].id)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/panes/"+p.ID+"/unsent-message", strings.NewReader(`{"id":"`+p.Snapshot().UnsentMessage.ID+`","action":"send"}`))
	req.SetPathValue("id", p.ID)
	a.actOnUnsentMessage(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_required") {
		t.Fatalf("unsent send = %d %s", rec.Code, rec.Body)
	}
	expectNoFrame(t, frames, "nothing typed")
}

// Opening or restarting a pane over REST runs a command: the person's.
// Agents use spawn_pane.
func TestOpeningOrRestartingAPaneOverRESTIsThePersons(t *testing.T) {
	a := viewOnlyServer(t, humanProofOff)
	a.mgr = NewManager(t.TempDir())
	for _, id := range []callerIdentity{viewOnlyCallers[6].id, {Kind: callerPane, PaneID: "lead"}} {
		callAsIdentity(t, id)
		rec := httptest.NewRecorder()
		a.createPane(rec, httptest.NewRequest("POST", "/api/panes", strings.NewReader(`{"cmd":["bash"]}`)))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_required") {
			t.Fatalf("%s create = %d %s", id.Kind, rec.Code, rec.Body)
		}
		rec = httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/panes/x/restart", nil)
		req.SetPathValue("id", "x")
		a.handleRestartPane(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "human_required") {
			t.Fatalf("%s restart = %d %s", id.Kind, rec.Code, rec.Body)
		}
	}
}

// enrolThroughAPI asks to enrol key through the whole API, marked as
// forwarded for a remote device or not.
func enrolThroughAPI(t *testing.T, h http.Handler, spki string, marked bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/remote-devices/enroll", strings.NewReader(`{"name":"phone","publicKey":"`+spki+`","codeVersion":2}`))
	if marked {
		req.Header.Set("X-Forwarded-Prefix", "/m/home")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func enrolTestServer(t *testing.T) (*apiServer, http.Handler, *time.Time) {
	t.Helper()
	a := trustedDevicesTestServer(t)
	a.metrics = NewMetrics()
	a.trustedDevices.record.Policy = remoteAccessApproved
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	a.trustedDevices.now = func() time.Time { return now }
	callAsIdentity(t, viewOnlyCallers[0].id) // marked requests are view-only
	mux := http.NewServeMux()
	a.routes(mux)
	return a, a.apiHandler(mux), &now
}

// A flood of requests forwarded for remote devices never drops a request
// already waiting: forwarded callers get forwardedPendingLimit waiting at
// once, then 429; a full list refuses everyone (429) rather than evicting.
func TestAForwardedEnrolmentFloodNeverDropsAWaitingRequest(t *testing.T) {
	a, h, _ := enrolTestServer(t)
	genuine := newTestDevice(t)
	callAsIdentity(t, callerIdentity{Kind: callerHuman, Unproven: true})
	if rec := enrolThroughAPI(t, h, genuine.spki, false); rec.Code != http.StatusOK {
		t.Fatalf("genuine enrol = %d %s", rec.Code, rec.Body)
	}
	callAsIdentity(t, viewOnlyCallers[0].id)
	for i := 0; i < forwardedPendingLimit+3; i++ {
		rec := enrolThroughAPI(t, h, newTestDevice(t).spki, true)
		capped := i >= forwardedPendingLimit
		if capped != (rec.Code == http.StatusTooManyRequests) || (!capped && rec.Code != http.StatusOK) {
			t.Fatalf("forwarded request %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if d := a.trustedDevices.find(deviceID(genuine.spki)); d == nil || d.Status != deviceStatusPending {
		t.Fatal("the flood dropped a waiting request")
	}
	// The rest of the list is still open to the computer itself, and a full
	// list refuses rather than evicts.
	callAsIdentity(t, callerIdentity{Kind: callerHuman, Unproven: true})
	if rec := enrolThroughAPI(t, h, newTestDevice(t).spki, false); rec.Code != http.StatusOK {
		t.Fatalf("local enrol beside the forwarded ones = %d %s", rec.Code, rec.Body)
	}
	if rec := enrolThroughAPI(t, h, newTestDevice(t).spki, false); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("enrol into a full list = %d %s", rec.Code, rec.Body)
	}
	if d := a.trustedDevices.find(deviceID(genuine.spki)); d == nil {
		t.Fatal("a full list evicted a waiting request")
	}
}

// Forwarded callers share forwardedEnrolPerHour attempts an hour (asking
// again for the same key counts), then 429 until the hour has passed.
func TestForwardedEnrolmentAttemptsAreCapped(t *testing.T) {
	_, h, now := enrolTestServer(t)
	d := newTestDevice(t)
	for i := 0; i < forwardedEnrolPerHour; i++ {
		if rec := enrolThroughAPI(t, h, d.spki, true); rec.Code != http.StatusOK {
			t.Fatalf("attempt %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if rec := enrolThroughAPI(t, h, d.spki, true); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "too_many_enrolments") {
		t.Fatalf("attempt past the cap = %d %s", rec.Code, rec.Body)
	}
	*now = now.Add(forwardedEnrolWindow)
	if rec := enrolThroughAPI(t, h, d.spki, true); rec.Code != http.StatusOK {
		t.Fatalf("attempt after the hour = %d %s", rec.Code, rec.Body)
	}
}

// Waiting requests still clear on their lifetime, freeing the forwarded
// allowance and the list.
func TestWaitingRequestsStillExpire(t *testing.T) {
	a, h, now := enrolTestServer(t)
	for i := 0; i < forwardedPendingLimit; i++ {
		if rec := enrolThroughAPI(t, h, newTestDevice(t).spki, true); rec.Code != http.StatusOK {
			t.Fatalf("forwarded request %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if rec := enrolThroughAPI(t, h, newTestDevice(t).spki, true); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the forwarded allowance = %d %s", rec.Code, rec.Body)
	}
	*now = now.Add(devicePendingLifetime + forwardedEnrolWindow)
	if rec := enrolThroughAPI(t, h, newTestDevice(t).spki, true); rec.Code != http.StatusOK {
		t.Fatalf("after the requests expired = %d %s", rec.Code, rec.Body)
	}
	if n := len(a.trustedDevices.record.Devices); n != 1 {
		t.Fatalf("%d requests left, want only the new one", n)
	}
}

// Closes the V3 commit-overwrite hole that predates the forwarded
// allowance: a waiting request whose code was shown is never replaced by a V3
// commit with its (public) key, from a forwarded caller or not (409).
func TestV3CommitOverwriteHoleIsClosedForAWaitingRequest(t *testing.T) {
	a, h, _ := enrolTestServer(t)
	genuine := newTestDevice(t)
	callAsIdentity(t, callerIdentity{Kind: callerHuman, Unproven: true})
	if rec := enrolThroughAPI(t, h, genuine.spki, false); rec.Code != http.StatusOK {
		t.Fatalf("genuine enrol = %d %s", rec.Code, rec.Body)
	}
	before := *a.trustedDevices.find(deviceID(genuine.spki))
	callAsIdentity(t, viewOnlyCallers[0].id)
	body := `{"name":"attacker","publicKey":"` + genuine.spki + `","codeVersion":3,"commit":"` + deviceCommitV3(deviceID(genuine.spki), strings.Repeat("02", 32)) + `"}`
	req := httptest.NewRequest("POST", "/api/remote-devices/enroll", strings.NewReader(body))
	req.Header.Set("X-Forwarded-Prefix", "/m/home")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("V3 commit over a waiting request = %d %s", rec.Code, rec.Body)
	}
	after := a.trustedDevices.find(deviceID(genuine.spki))
	if after == nil || after.Name != before.Name || after.Code != before.Code || after.Status != deviceStatusPending {
		t.Fatalf("the waiting request changed: %+v", after)
	}
}

// An approved device clears a flood of waiting requests at once: it can
// remove each one; a view-only caller can't.
func TestAnApprovedDeviceRemovesWaitingRequestsAtOnce(t *testing.T) {
	a, h, _ := enrolTestServer(t)
	seedApprovedDevice(a, testSignedDevice)
	var flood []string
	for i := 0; i < forwardedPendingLimit; i++ {
		d := newTestDevice(t)
		if rec := enrolThroughAPI(t, h, d.spki, true); rec.Code != http.StatusOK {
			t.Fatalf("forwarded request %d = %d %s", i+1, rec.Code, rec.Body)
		}
		flood = append(flood, deviceID(d.spki))
	}
	remove := func(id string, marked bool) int {
		req := httptest.NewRequest("DELETE", "/api/remote-devices/"+id, nil)
		if marked {
			req.Header.Set("X-Forwarded-Prefix", "/m/home")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := remove(flood[0], true); code != http.StatusForbidden {
		t.Fatalf("a view-only caller removed a request: %d", code)
	}
	for _, signed := range []callerIdentity{
		{Kind: callerHuman, Device: testSignedDevice},                  // at the computer
		{Kind: callerHuman, Device: testSignedDevice, Forwarded: true}, // a signed remote device
	} {
		callAsIdentity(t, signed)
		for _, id := range flood[:1] {
			if code := remove(id, signed.Forwarded); code != http.StatusNoContent {
				t.Fatalf("%+v removing %s = %d", signed, id, code)
			}
		}
		flood = flood[1:]
	}
	if len(flood) != forwardedPendingLimit-2 || a.trustedDevices.find(flood[0]) == nil {
		t.Fatal("removal reached past the requests asked for")
	}
	callAsIdentity(t, callerIdentity{Kind: callerHuman, Device: testSignedDevice})
	if code := remove(flood[0], false); code != http.StatusNoContent {
		t.Fatalf("removing the last waiting request = %d", code)
	}
	for _, d := range a.trustedDevices.record.Devices {
		if d.Status != deviceStatusApproved {
			t.Fatalf("a waiting request is left: %+v", d)
		}
	}
}
