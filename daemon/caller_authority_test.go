package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func stubPeerPID(t *testing.T, pid int, ok bool) {
	t.Helper()
	prev := lookupPeerPID
	lookupPeerPID = func(*http.Request) (int, bool) { return pid, ok }
	t.Cleanup(func() { lookupPeerPID = prev })
}

func authorityTestServer(t *testing.T) *apiServer {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = nil
	t.Cleanup(func() { identifyCallerHook = prev })
	dir := t.TempDir()
	return &apiServer{mgr: NewManager(dir), agentAuthority: newAgentAuthority(dir)}
}

// startSleeper runs a short-lived child the tests can name as the caller.
func startSleeper(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process-tree fixtures use sleep")
	}
	cmd := exec.Command("sleep", "30")
	// The suite itself may run inside a SoluTerminal pane; don't let the
	// fixture inherit that pane's id.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SOLU_PANE_ID=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	// /proc/<pid>/environ can still show the pre-exec image for a moment
	// after Start returns; wait for the fixture's own environment.
	for _, kv := range env {
		key, want, _ := strings.Cut(kv, "=")
		deadline := time.Now().Add(2 * time.Second)
		for {
			if got, ok := readProcEnviron(cmd.Process.Pid); ok && got[key] == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("fixture environment never showed %s", key)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return cmd
}

func TestIdentifyCallerRelayIsHuman(t *testing.T) {
	a := authorityTestServer(t)
	stubPeerPID(t, os.Getpid(), true)
	r := httptest.NewRequest("POST", "/api/workflow/tasks/x/approve", nil)
	r = r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
	if got := a.identifyCaller(r); got.Kind != callerHuman || !got.Relay {
		t.Fatalf("relay caller = %+v, want human over relay", got)
	}
}

func TestIdentifyCallerForwardedPrefixHeaderIsNotRelay(t *testing.T) {
	a := authorityTestServer(t)
	stubPeerPID(t, 0, false)
	r := httptest.NewRequest("POST", "/api/workflow/tasks/x/approve", nil)
	r.Header.Set("X-Forwarded-Prefix", "/m/mach_fake")
	if got := a.identifyCaller(r); got.Kind != callerUnverified {
		t.Fatalf("local caller claiming the relay prefix = %+v, want unverified", got)
	}
	if !requestViaRelay(r) {
		t.Fatal("the prefix must still restrict the caller that sent it")
	}
}

func TestIdentifyCallerDaemonItselfIsWorker(t *testing.T) {
	a := authorityTestServer(t)
	stubPeerPID(t, os.Getpid(), true)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if got := a.identifyCaller(r); got.Kind != callerWorker {
		t.Fatalf("daemon-owned proxy = %+v, want worker even with browser headers", got)
	}
}

func TestIdentifyCallerChildOfDaemonIsWorker(t *testing.T) {
	a := authorityTestServer(t)
	cmd := startSleeper(t)
	stubPeerPID(t, cmd.Process.Pid, true)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if got := a.identifyCaller(r); got.Kind != callerWorker {
		t.Fatalf("daemon child = %+v, want worker", got)
	}
}

func TestIdentifyCallerInsidePaneTree(t *testing.T) {
	a := authorityTestServer(t)
	cmd := startSleeper(t)
	a.mgr.panes["p-shell"] = &Pane{ID: "p-shell", Pid: cmd.Process.Pid}
	stubPeerPID(t, cmd.Process.Pid, true)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	got := a.identifyCaller(r)
	if got.Kind != callerPane || got.PaneID != "p-shell" {
		t.Fatalf("pane process = %+v, want pane p-shell", got)
	}
}

func TestIdentifyCallerDetachedPaneProcessKeepsPaneEnv(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("needs /proc")
	}
	a := authorityTestServer(t)
	cmd := startSleeper(t, "SOLU_PANE_ID=p-escaped")
	stubPeerPID(t, cmd.Process.Pid, true)
	got := a.identifyCaller(httptest.NewRequest("POST", "/", nil))
	if got.Kind != callerPane || got.PaneID != "p-escaped" {
		t.Fatalf("process carrying SOLU_PANE_ID = %+v, want pane p-escaped", got)
	}
}

// A browser an agent launches starts with a clean environment (Playwright's
// Chromium does); its launcher still carries the pane id.
func TestIdentifyCallerCleanEnvChildOfPaneProcess(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("needs /proc")
	}
	a := authorityTestServer(t)
	cmd := exec.Command("sh", "-c", "env -i sleep 30 & echo $!; wait")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SOLU_PANE_ID=p-launcher"}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var childPID int
	if _, err := fmt.Fscan(out, &childPID); err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})
	stubPeerPID(t, childPID, true)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if got := a.identifyCaller(r); got.Kind != callerPane || got.PaneID != "p-launcher" {
		t.Fatalf("clean-env child of a pane process = %+v, want pane p-launcher", got)
	}
}

func TestIdentifyCallerBrowserVersusOtherPrograms(t *testing.T) {
	a := authorityTestServer(t)
	cases := []struct {
		name    string
		headers map[string]string
		host    string
		want    callerKind
	}{
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, "127.0.0.1:8787", callerHuman},
		{"older webview exact origin", map[string]string{"Origin": "http://127.0.0.1:8787"}, "127.0.0.1:8787", callerHuman},
		{"origin on another port", map[string]string{"Origin": "http://127.0.0.1:3000"}, "127.0.0.1:8787", callerUnverified},
		{"same-site but not same-origin", map[string]string{"Sec-Fetch-Site": "same-site"}, "127.0.0.1:8787", callerUnverified},
		{"curl", nil, "127.0.0.1:8787", callerUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubPeerPID(t, 0, false)
			r := httptest.NewRequest("POST", "/", nil)
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := a.identifyCaller(r); got.Kind != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestRequireHumanOrDelegated(t *testing.T) {
	a := authorityTestServer(t)
	cmd := startSleeper(t)
	a.mgr.panes["p1"] = &Pane{ID: "p1", Pid: cmd.Process.Pid}
	stubPeerPID(t, cmd.Process.Pid, true)
	call := func() (bool, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		return a.requireHumanOrDelegated(w, httptest.NewRequest("POST", "/", nil), "approve tasks"), w
	}

	if ok, w := call(); ok || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Agent permissions") {
		t.Fatalf("pane without a grant: ok=%v code=%d body=%s", ok, w.Code, w.Body.String())
	}
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := call(); !ok {
		t.Fatal("pane with a grant was refused")
	}
	past := time.Now().Add(-time.Minute)
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true, Until: &past}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := call(); ok {
		t.Fatal("pane with an expired grant was admitted")
	}

	// A grant never covers what the daemon spawned itself.
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	stubPeerPID(t, os.Getpid(), true)
	if ok, w := call(); ok || !strings.Contains(w.Body.String(), "Workflow workers") {
		t.Fatalf("worker under a grant: ok=%v body=%s", ok, w.Body.String())
	}
}

func TestRequireHumanIgnoresAgentGrant(t *testing.T) {
	a := authorityTestServer(t)
	cmd := startSleeper(t)
	a.mgr.panes["p1"] = &Pane{ID: "p1", Pid: cmd.Process.Pid}
	stubPeerPID(t, cmd.Process.Pid, true)
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if a.requireHuman(w, httptest.NewRequest("PUT", "/", nil), "change approval autopilot") {
		t.Fatal("a grant must not let agents widen it")
	}
}

func TestPutAgentAuthority(t *testing.T) {
	a := authorityTestServer(t)
	put := func(body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/settings/agent-authority", strings.NewReader(body))
		r.Host = "127.0.0.1:8787"
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		a.handlePutAgentAuthority(w, r)
		return w
	}
	browser := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }

	stubPeerPID(t, 0, false)
	if w := put(`{"panes":true}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-browser turn-on: %d %s", w.Code, w.Body.String())
	}
	if w := put(`{"panes":true}`, func(r *http.Request) {
		browser(r)
		*r = *r.WithContext(context.WithValue(r.Context(), relayTransportKey{}, true))
	}); w.Code != http.StatusForbidden {
		t.Fatalf("relay turn-on: %d %s", w.Code, w.Body.String())
	}
	if w := put(`{"panes":true,"hours":1000}`, browser); w.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range hours: %d", w.Code)
	}
	w := put(`{"panes":true,"hours":2}`, browser)
	if w.Code != http.StatusOK {
		t.Fatalf("browser turn-on: %d %s", w.Code, w.Body.String())
	}
	var got agentAuthorityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !got.Active || got.Until == nil {
		t.Fatalf("response %+v err=%v", got, err)
	}
	if a2 := newAgentAuthority(a.agentAuthority.path[:len(a.agentAuthority.path)-len(agentAuthorityFile)]); !a2.active(time.Now()) {
		t.Fatal("grant was not persisted")
	}
	// Anyone may turn it off.
	if w := put(`{"panes":false}`, nil); w.Code != http.StatusOK || a.agentAuthority.active(time.Now()) {
		t.Fatalf("turn-off: %d active=%v", w.Code, a.agentAuthority.active(time.Now()))
	}
}

// TestLookupPeerPIDFindsThisProcess drives the real platform lookup over a
// loopback connection: the client end belongs to the test process. WSL's
// consomme networking relays some loopback connections (the server then sees
// a port no local socket owns), so a few attempts are allowed; one traced
// connection proves the lookup.
func TestLookupPeerPIDFindsThisProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("peer lookup is implemented for Linux and Windows")
	}
	got := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, _ := lookupPeerPID(r)
		got <- pid
	}))
	defer srv.Close()
	seen := []int{}
	for attempt := 0; attempt < 8; attempt++ {
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{}).DialContext}}
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		pid := <-got
		if pid == os.Getpid() {
			return
		}
		seen = append(seen, pid)
	}
	t.Fatalf("peer pid never resolved to this process %d; saw %v", os.Getpid(), seen)
}

// Typing "1" into a pane at a permission prompt answers it, so an agent
// without the person's grant is held back there — and only there.
func TestPromptInputGate(t *testing.T) {
	a := authorityTestServer(t)
	target := choicePromptPane(t, "pane_target")
	if target.computeStateCached() != StateChoicePrompt {
		t.Fatal("fixture is not at a choice prompt")
	}
	a.mgr.panes[target.ID] = target
	idle := &Pane{ID: "pane_idle", Status: PaneRunning, buffer: NewRingBuffer(1 << 10), subs: map[int64]*subEntry{}}
	cmd := startSleeper(t)
	a.mgr.panes["pane_agent"] = &Pane{ID: "pane_agent", Pid: cmd.Process.Pid}
	req := httptest.NewRequest("GET", "/api/panes/pane_target/io", nil)

	stubPeerPID(t, cmd.Process.Pid, true)
	gate := a.promptInputGate(req)
	if gate(target) {
		t.Fatal("an agent without the grant could type into a permission prompt")
	}
	if !gate(idle) {
		t.Fatal("an agent was held back from a pane with no prompt")
	}
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	if !gate(target) {
		t.Fatal("the person's grant did not let the agent answer")
	}
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: false}); err != nil {
		t.Fatal(err)
	}

	stubPeerPID(t, os.Getpid(), true) // a worker / the daemon's own proxy
	if a.promptInputGate(req)(target) {
		t.Fatal("a worker could type into a permission prompt")
	}
	stubPeerPID(t, 0, false) // untraceable: never drop the person's keys
	if !a.promptInputGate(req)(target) {
		t.Fatal("an untraceable caller was held back")
	}
}

func TestSendInputHeldAtPermissionPrompt(t *testing.T) {
	target := choicePromptPane(t, "pane_send_target")
	a := &apiServer{mgr: testManagerWithPane(target), uiBus: newUIBus()}
	text, isErr := mcpResultText(t, scopedCallForTest(t, a, nil, "send_input",
		map[string]any{"pane_id": target.ID, "text": "1"}))
	if !isErr || !strings.Contains(text, "permission prompt") {
		t.Fatalf("send_input into a prompt without the grant = %q (err=%v)", text, isErr)
	}
}

// A traced caller that detached from its pane (no pane id, re-parented away)
// and forges a browser's headers is not the person unless it is a browser.
func TestIdentifyCallerTracedNonBrowserWithForgedHeaders(t *testing.T) {
	a := authorityTestServer(t)
	stubPeerPID(t, 1, true) // outside every pane, environment unreadable
	prev, prevProxy := peerIsBrowser, peerIsRelayProxy
	t.Cleanup(func() { peerIsBrowser, peerIsRelayProxy = prev, prevProxy })
	peerIsRelayProxy = func(int) bool { return false }
	r := httptest.NewRequest("POST", "/api/workflow/tasks/x/approve", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")

	peerIsBrowser = func(int) bool { return false }
	if got := a.identifyCaller(r); got.Kind != callerUnverified {
		t.Fatalf("forged headers from a non-browser = %+v, want unverified", got)
	}
	peerIsBrowser = func(int) bool { return true }
	if got := a.identifyCaller(r); got.Kind != callerHuman {
		t.Fatalf("browser process = %+v, want human", got)
	}
}

func TestPeerIsBrowserRejectsThisTestProcess(t *testing.T) {
	if peerIsBrowser(os.Getpid()) {
		t.Fatal("the Go test binary was taken for a browser")
	}
}

func TestRelayProxiesFallBackToBrowserHeaders(t *testing.T) {
	a := authorityTestServer(t)
	stubPeerPID(t, 1, true) // WSL init forwarding a Windows browser's connection
	prev := peerIsBrowser
	t.Cleanup(func() { peerIsBrowser = prev })
	peerIsBrowser = func(int) bool { return false }
	r := httptest.NewRequest("POST", "/", nil)
	if got := a.identifyCaller(r); got.Kind != callerUnverified {
		t.Fatalf("relay proxy without browser headers = %+v", got)
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if got := a.identifyCaller(r); got.Kind != callerHuman {
		t.Fatalf("browser request through the WSL relay = %+v, want human", got)
	}
}

func TestRefusalExplainsBrowserStartedFromAPane(t *testing.T) {
	a := authorityTestServer(t)
	prev := peerIsBrowser
	t.Cleanup(func() { peerIsBrowser = prev })
	peerIsBrowser = func(int) bool { return true }
	w := httptest.NewRecorder()
	a.refuseCaller(w, callerIdentity{Kind: callerPane, PaneID: "p1", PID: 4242}, "approve tasks", true)
	if !strings.Contains(w.Body.String(), "started from inside a SoluTerminal pane") {
		t.Fatalf("refusal = %s", w.Body.String())
	}
}

// A process inside a pane is that pane even when it puts another pane's id in
// its environment (`env SOLU_PANE_ID=<victim> curl …`).
func TestIdentifyCallerPaneAncestryOutranksEnv(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("needs /proc")
	}
	a := authorityTestServer(t)
	cmd := exec.Command("sh", "-c", "env SOLU_PANE_ID=p-victim sleep 30 & echo $!; wait")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SOLU_PANE_ID=p-rogue"}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var childPID int
	if _, err := fmt.Fscan(out, &childPID); err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if env, ok := readProcEnviron(childPID); ok && env["SOLU_PANE_ID"] == "p-victim" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never showed its environment")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rogue := testPaneWithTail(t, "p-rogue", "rogue", "")
	rogue.Pid = cmd.Process.Pid
	a.mgr.panes[rogue.ID] = rogue
	stubPeerPID(t, childPID, true)
	if got := a.identifyCaller(httptest.NewRequest("POST", "/", nil)); got.Kind != callerPane || got.PaneID != "p-rogue" || got.FromEnv {
		t.Fatalf("child of pane p-rogue claiming p-victim = %+v, want pane p-rogue by ancestry", got)
	}
}

// A process that left its pane (setsid) and sets another pane's id in its
// environment is placed only by that claim: still an agent, never the person,
// and no pane for ownership.
func TestIdentifyCallerDetachedEnvClaimIsAnAgentWithoutOwnership(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("needs /proc")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("needs setsid")
	}
	a := authorityTestServer(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command("setsid", "-f", "env", "SOLU_PANE_ID=p-victim", "sh", "-c", "echo $$ > "+pidFile+"; exec sleep 30")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for pid == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {
			fmt.Sscan(strings.TrimSpace(string(b)), &pid)
		}
		if time.Now().After(deadline) {
			t.Fatal("detached process never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	for deadline = time.Now().Add(2 * time.Second); ; {
		if env, ok := readProcEnviron(pid); ok && env["SOLU_PANE_ID"] == "p-victim" {
			if comm := readComm(pid); strings.TrimSpace(comm) == "sleep" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("detached process never showed its environment")
		}
		time.Sleep(10 * time.Millisecond)
	}
	victim := testPaneWithTail(t, "p-victim", "victim", "")
	a.mgr.panes[victim.ID] = victim
	stubPeerPID(t, pid, true)
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	got := a.identifyCaller(req)
	if got.Kind != callerPane || got.PaneID != "p-victim" || !got.FromEnv {
		t.Fatalf("detached env claim = %+v, want an unverified pane claim", got)
	}
	// Still an agent everywhere else: never the person.
	rec := httptest.NewRecorder()
	if a.requireHuman(rec, req, "answer an agent's request to close or change a pane") || rec.Code != 403 {
		t.Fatalf("requireHuman admitted a detached env claim: %d", rec.Code)
	}
	// For ownership it is no pane, whatever it or its header claims.
	mcpReq := httptest.NewRequest("POST", "/mcp", nil)
	mcpReq.Header.Set("X-Soluterminal-Pane", "p-victim")
	if got := a.mcpTracedCallerPane(mcpReq); got != mcpClaimedCallerPrefix+"p-victim" {
		t.Fatalf("caller = %q, want an unverified claim", got)
	}
	if a.paneAgentAuthority(&PaneOrigin{Source: paneOriginUser}, "p-victim", splitMCPCaller(a.mcpTracedCallerPane(mcpReq)).ID, false) != paneAgentRefused {
		t.Fatal("a detached env claim may act on the pane it claims")
	}
}
