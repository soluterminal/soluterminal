package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hp "soluterminal/helperproto"
	"soluterminal/helperproto/helpertest"
)

// innerSeen is what the API handler saw for one dispatched request.
type innerSeen struct {
	caller     helperFwdCaller
	classified callerIdentity
	identified callerIdentity
	hdr        http.Header
	host       string
	remote     string
	path       string
	body       string
}

type innerAPI struct {
	mu   sync.Mutex
	seen []innerSeen
}

func (h *innerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c, _ := helperFwdCallerOf(r)
	a := &apiServer{}
	s := innerSeen{caller: c, classified: a.classifyCaller(r), identified: a.identifyCaller(r),
		hdr: r.Header.Clone(), host: r.Host, remote: r.RemoteAddr, path: r.URL.Path, body: string(b)}
	h.mu.Lock()
	h.seen = append(h.seen, s)
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (h *innerAPI) all() []innerSeen {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]innerSeen(nil), h.seen...)
}

type hostFwdTest struct {
	h     *helperFwdHost
	p     *helpertest.Pair
	api   *innerAPI
	pane  string
	agent string
	live  map[string]bool
	// backing is each pane's current agent; empty means the agent that
	// spawned it in the test.
	backing map[string]string
}

const testHostPane = "pane_00000000000000aa"

func newHostFwdTest(t *testing.T) *hostFwdTest {
	t.Helper()
	realCallerClassification(t)
	ht := &hostFwdTest{api: &innerAPI{}, pane: testHostPane, live: map[string]bool{testHostPane: true}, backing: map[string]string{}}
	ht.h = newHelperFwdHost("Ubuntu", ht.api, func(id, agent string) bool {
		cur := ht.backing[id]
		if cur == "" {
			cur = ht.agent
		}
		return ht.live[id] && cur == agent
	})
	ht.h.enabled = true // tests only: the build's default stays off
	ht.h.logf = t.Logf
	ht.h.admitWait = 150 * time.Millisecond // refusals in tests needn't wait 5 s
	ht.p = helpertest.NewPair(t, helpertest.Options{
		Host:   hp.HostOptions{Handlers: map[string]hp.HostStreamHandler{hp.KindFwd: ht.h.handle}},
		Helper: hp.HelperOptions{OpenKinds: []string{hp.KindFwd}},
	})
	ht.agent = ht.p.Fake.AddAgent(ht.pane).Info.AgentID
	// The host learns which agent backs which pane from list replies.
	if _, err := ht.p.Host.ListAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ht
}

// A stream that arrives before the host has registered its agent (the CLI's
// first MCP initialize or SessionStart, sent while the spawn reply is still
// on its way) waits for the registration instead of being refused.
func TestHelperFwdHostWaitsForRegistration(t *testing.T) {
	realCallerClassification(t)
	api := &innerAPI{}
	live := atomic.Bool{}
	var agent atomic.Value
	agent.Store("")
	h := newHelperFwdHost("Ubuntu", api, func(id, a string) bool { return live.Load() && id == testHostPane && a == agent.Load().(string) })
	h.enabled, h.logf = true, t.Logf
	p := helpertest.NewPair(t, helpertest.Options{
		Host:   hp.HostOptions{Handlers: map[string]hp.HostStreamHandler{hp.KindFwd: h.handle}},
		Helper: hp.HelperOptions{OpenKinds: []string{hp.KindFwd}},
	})
	ht := &hostFwdTest{h: h, p: p, api: api, pane: testHostPane}
	id := p.Fake.AddAgent(testHostPane).Info.AgentID
	agent.Store(id)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = p.Host.ListAll(context.Background()) // the session learns the agent
		time.Sleep(100 * time.Millisecond)
		live.Store(true) // the pane is published
	}()
	if status, _ := ht.send(t, testHostPane, ht.attest(id), post("/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); status != 200 {
		t.Fatalf("early stream: %d", status)
	}
	if len(api.all()) != 1 {
		t.Fatal("early stream not dispatched")
	}
}

// The wait is bounded, and only for "not registered yet": a bad stream is
// refused at once.
func TestHelperFwdHostWaitIsBounded(t *testing.T) {
	ht := newHostFwdTest(t)
	ht.live[ht.pane] = false
	start := time.Now()
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != http.StatusForbidden {
		t.Fatalf("status %d", status)
	}
	if d := time.Since(start); d < ht.h.admitWait || d > 5*time.Second {
		t.Fatalf("waited %v (admitWait %v)", d, ht.h.admitWait)
	}
	start = time.Now()
	if status, _ := ht.send(t, "pane_X", ht.attest(ht.agent), post("/mcp", "{}")); status != http.StatusBadRequest || time.Since(start) > ht.h.admitWait {
		t.Fatalf("bad stream: %d after %v", status, time.Since(start))
	}
}

func (ht *hostFwdTest) attest(agent string) json.RawMessage {
	b, _ := json.Marshal(hp.FwdAttest{Agent: agent, PID: 400, Start: 1030, UID: 1000})
	return b
}

// send opens a fwd stream and writes raw request bytes; it returns the
// host's status and body.
func (ht *hostFwdTest) send(t *testing.T, pane string, attest json.RawMessage, raw string) (int, string) {
	t.Helper()
	st, err := ht.p.Helper.Open(hp.StreamHeader{S: hp.KindFwd, Kind: "http", Pane: pane, Attest: attest})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	go func() { _, _ = io.WriteString(st, raw) }()
	resp, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(path, body string, extra ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", path, len(body))
	for i := 0; i+1 < len(extra); i += 2 {
		fmt.Fprintf(&b, "%s: %s\r\n", extra[i], extra[i+1])
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

func TestHelperFwdHostOffByDefault(t *testing.T) {
	if helperFwdEnabled {
		t.Fatal("fwd must stay off until the PR that turns it on carries secure's marker (8.6a.5)")
	}
	h := newHelperFwdHost("Ubuntu", &innerAPI{}, func(string, string) bool { return true })
	if h.enabled || h.streamHandlers() != nil {
		t.Fatal("a default host registers a fwd handler")
	}
	if _, status, _ := h.admit(nil, hp.StreamHeader{S: hp.KindFwd, Kind: "http", Pane: testHostPane}); status != http.StatusForbidden {
		t.Fatalf("a default host admits fwd: %d", status)
	}
}

// The dispatched request is rebuilt: one Content-Type header, loopback Host,
// a non-IP RemoteAddr, and the attested pane as a verified pane caller with
// PID 0 and the distro. Nothing the caller wrote changes that.
func TestHelperFwdHostDispatchesAsTheAttestedPane(t *testing.T) {
	ht := newHostFwdTest(t)
	status, body := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		"X-Soluterminal-Pane", "pane_ffffffffffffffff",
		"Origin", "http://127.0.0.1:8787",
		"Referer", "http://127.0.0.1:8787/",
		"Sec-Fetch-Site", "same-origin",
		"Cookie", "solu_session=x",
		"Authorization", "Bearer x",
		"X-Forwarded-Prefix", "/m/machine",
		"X-Forwarded-For", "1.2.3.4",
	))
	if status != 200 || body != `{"ok":true}` {
		t.Fatalf("status %d body %q", status, body)
	}
	seen := ht.api.all()
	if len(seen) != 1 {
		t.Fatalf("api saw %d requests", len(seen))
	}
	s := seen[0]
	if s.caller != (helperFwdCaller{PaneID: ht.pane, AgentID: ht.agent, Distro: "Ubuntu"}) {
		t.Fatalf("caller %+v", s.caller)
	}
	want := callerIdentity{Kind: callerPane, PaneID: ht.pane, Helper: "Ubuntu"}
	if s.classified != want {
		t.Fatalf("classified %+v, want %+v", s.classified, want)
	}
	if id := s.identified; id.Kind != callerPane || id.PaneID != ht.pane || id.PID != 0 || id.FromEnv || id.Forwarded || id.Relay || id.Device != "" || id.ScopedDevice != "" || !id.tracedPane() {
		t.Fatalf("identified %+v", id)
	}
	if len(s.hdr) != 1 || s.hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("headers reached the handler: %v", s.hdr)
	}
	if s.host != "127.0.0.1" || s.remote != helperFwdRemoteAddr || s.path != "/mcp" || s.body != `{"jsonrpc":"2.0","id":1,"method":"ping"}` {
		t.Fatalf("request %+v", s)
	}
	r, _ := http.NewRequest("POST", "/", nil)
	r.RemoteAddr = s.remote
	if isLoopback(r) {
		t.Fatal("a fwd request passes isLoopback")
	}
}

func TestHelperFwdHostRefuses(t *testing.T) {
	other := "pane_00000000000000bb"
	cases := []struct {
		name   string
		pane   string
		attest func(ht *hostFwdTest) json.RawMessage
		raw    string
		setup  func(ht *hostFwdTest)
		want   int
	}{
		{name: "agent unknown to this session", attest: func(ht *hostFwdTest) json.RawMessage { return ht.attest("0123456789abcdef0123456789abcdef") }, want: 403},
		{name: "agent backs another pane", pane: other, setup: func(ht *hostFwdTest) { ht.live[other] = true }, want: 403},
		{name: "pane not live", setup: func(ht *hostFwdTest) { ht.live[ht.pane] = false }, want: 403},
		{name: "invalid pane id", pane: "pane_X", want: 400},
		{name: "attest with an unknown field", attest: func(ht *hostFwdTest) json.RawMessage {
			return json.RawMessage(`{"agent":"` + ht.agent + `","pid":1,"start":1,"uid":1,"pane":"pane_00000000000000bb"}`)
		}, want: 400},
		{name: "attest without a pid", attest: func(ht *hostFwdTest) json.RawMessage {
			return json.RawMessage(`{"agent":"` + ht.agent + `","pid":0,"start":1,"uid":1}`)
		}, want: 400},
		{name: "GET", raw: "GET /mcp HTTP/1.1\r\nHost: x\r\n\r\n", want: 404},
		{name: "other path", raw: post("/api/panes", "{}"), want: 404},
		{name: "nested hook path", raw: post("/api/hooks/a/b", "{}"), want: 404},
		{name: "query", raw: post("/mcp?x=1", "{}"), want: 404},
		{name: "chunked", raw: "POST /mcp HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n", want: 411},
		{name: "hook over 1 MiB", raw: post("/api/hooks/tool-call", strings.Repeat("a", helperFwdMaxHookRequest+1)), want: 413},
		{name: "mcp over 4 MiB", raw: post("/mcp", strings.Repeat("a", helperFwdMaxMCPRequest+1)), want: 413},
		{name: "headers over 16 KiB", raw: post("/mcp", "{}", "X-Big", strings.Repeat("a", helperFwdMaxHookHeader)), want: 431},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ht := newHostFwdTest(t)
			if tc.setup != nil {
				tc.setup(ht)
			}
			pane := ht.pane
			if tc.pane != "" {
				pane = tc.pane
			}
			att := ht.attest(ht.agent)
			if tc.attest != nil {
				att = tc.attest(ht)
			}
			raw := post("/mcp", "{}")
			if tc.raw != "" {
				raw = tc.raw
			}
			if status, _ := ht.send(t, pane, att, raw); status != tc.want {
				t.Fatalf("status %d, want %d", status, tc.want)
			}
			if n := len(ht.api.all()); n != 0 {
				t.Fatalf("a refused request reached the API (%d)", n)
			}
		})
	}
}

func TestHelperFwdHostMCPUpTo4MiB(t *testing.T) {
	ht := newHostFwdTest(t)
	body := `{"p":"` + strings.Repeat("a", 2<<20) + `"}`
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", body)); status != 200 {
		t.Fatalf("status %d", status)
	}
	if s := ht.api.all(); len(s) != 1 || s[0].body != body {
		t.Fatal("body not dispatched whole")
	}
}

func TestHelperFwdHostLimits(t *testing.T) {
	t.Run("per-agent rate", func(t *testing.T) {
		ht := newHostFwdTest(t)
		now := time.Unix(1000, 0)
		ht.h.now = func() time.Time { return now }
		for i := 0; i < helperFwdPerAgentRate; i++ {
			if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != 200 {
				t.Fatalf("call %d: %d", i, status)
			}
		}
		if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != http.StatusTooManyRequests {
			t.Fatalf("call past the rate: %d", status)
		}
	})
	t.Run("open streams", func(t *testing.T) {
		ht := newHostFwdTest(t)
		for i := 0; i < helperFwdMaxOpen; i++ {
			ht.h.slots <- struct{}{}
		}
		if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != http.StatusTooManyRequests {
			t.Fatalf("call past the open cap: %d", status)
		}
		if len(ht.api.all()) != 0 {
			t.Fatal("dispatched past the cap")
		}
	})
}

func TestHelperFwdHostResponseCap(t *testing.T) {
	rec := newHelperFwdRecorder(8)
	if _, err := rec.Write([]byte("12345678")); err != nil || rec.overflow {
		t.Fatal("write at the cap refused")
	}
	if _, err := rec.Write([]byte("9")); err == nil || !rec.overflow {
		t.Fatal("write past the cap accepted")
	}
}

func TestHelperFwdKernelSupported(t *testing.T) {
	for k, want := range map[string]bool{
		"6.6.87.2-microsoft-standard-WSL2":   true,
		"6.5.0":                              true,
		"7.0.1":                              true,
		"6.18.58.1-microsoft-standard-WSL2":  true,
		"6.4.16":                             false,
		"5.15.167.4-microsoft-standard-WSL2": false,
		"6":                                  false,
		"":                                   false,
		"x.y":                                false,
	} {
		if got := helperFwdKernelSupported(k); got != want {
			t.Errorf("%q: %t, want %t", k, got, want)
		}
	}
	if !strings.Contains(helperFwdKernelNotice, "wsl --update") {
		t.Fatal("notice must name wsl --update")
	}
}

// A pane reaped while its request was being read is refused at dispatch.
func TestHelperFwdHostRevalidatesBeforeDispatch(t *testing.T) {
	ht := newHostFwdTest(t)
	calls := 0
	ht.h.backedBy = func(id, agent string) bool { calls++; return calls == 1 && id == ht.pane && agent == ht.agent }
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != http.StatusForbidden {
		t.Fatalf("status %d", status)
	}
	if len(ht.api.all()) != 0 {
		t.Fatal("dispatched for a pane that went away")
	}
}

// The 16 KiB header limit counts the raw header block, Host included (Go
// drops Host from the parsed header map).
func TestHelperFwdHostRawHeaderLimit(t *testing.T) {
	ht := newHostFwdTest(t)
	big := "POST /mcp HTTP/1.1\r\nHost: " + strings.Repeat("h", helperFwdMaxHookHeader) + "\r\nContent-Length: 2\r\n\r\n{}"
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), big); status != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("huge Host: %d", status)
	}
	var many strings.Builder
	many.WriteString("POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n")
	for i := 0; many.Len() <= helperFwdMaxHookHeader; i++ {
		fmt.Fprintf(&many, "X-%d: %s\r\n", i, strings.Repeat("v", 100))
	}
	many.WriteString("\r\n{}")
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), many.String()); status != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("many headers: %d", status)
	}
	// A header line longer than the read buffer, ending right after it: the
	// line's tail is never taken for the end of the headers.
	long := "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nX-L: " + strings.Repeat("l", 4096) + "\r\n\r\n{}"
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), long); status != 200 {
		t.Fatalf("long header line: %d", status)
	}
	if n := len(ht.api.all()); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
}

func TestHelperFwdObserveHello(t *testing.T) {
	h := newHelperFwdHost("Ubuntu", &innerAPI{}, nil)
	h.logf = t.Logf
	if n := h.observeHello(hp.HelloResult{Kernel: "5.15.167.4-microsoft-standard-WSL2"}); n != helperFwdKernelNotice {
		t.Fatalf("old kernel notice %q", n)
	}
	if n := h.observeHello(hp.HelloResult{Kernel: "6.6.87.2-microsoft-standard-WSL2"}); n != "" {
		t.Fatalf("new kernel notice %q", n)
	}
}

// A pane replaced under the same id has a new backing agent; the old agent
// no longer acts for it, at admission or at dispatch.
func TestHelperFwdHostOldAgentOfAReplacedPane(t *testing.T) {
	ht := newHostFwdTest(t)
	ht.backing[ht.pane] = "fedcba9876543210fedcba9876543210"
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != http.StatusForbidden {
		t.Fatalf("old agent of a replaced pane: %d", status)
	}
	if len(ht.api.all()) != 0 {
		t.Fatal("dispatched for a replaced pane's old agent")
	}
}

// A panicking handler fails its request instead of ending the daemon; the
// session goes on serving.
func TestHelperFwdHostRecoversHandlerPanic(t *testing.T) {
	ht := newHostFwdTest(t)
	inner := ht.api
	panicky := true
	ht.h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if panicky {
			panic("tool exploded")
		}
		inner.ServeHTTP(w, r)
	})
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != http.StatusInternalServerError {
		t.Fatalf("panicking handler: %d", status)
	}
	panicky = false
	if status, _ := ht.send(t, ht.pane, ht.attest(ht.agent), post("/mcp", "{}")); status != 200 {
		t.Fatalf("after a panic: %d", status)
	}
}
