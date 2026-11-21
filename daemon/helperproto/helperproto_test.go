package helperproto_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	hp "soluterminal/helperproto"
	"soluterminal/helperproto/helpertest"
)

const testPane = "pane_0123456789abcdef"

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func spawnArgs() hp.SpawnArgs {
	return hp.SpawnArgs{PaneID: testPane, Argv: []string{"bash"}, Cwd: "/home/fake", Cols: 80, Rows: 24}
}

func waitDone(t *testing.T, s *hp.HostSession) error {
	t.Helper()
	select {
	case <-s.Done():
		return s.Err()
	case <-time.After(5 * time.Second):
		t.Fatal("host session did not end")
		return nil
	}
}

func waitServe(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("helper Serve did not return")
		return nil
	}
}

func TestIdentifiers(t *testing.T) {
	good := strings.Repeat("0a", 16)
	for _, c := range []struct {
		s    string
		want bool
	}{
		{good, true},
		{strings.ToUpper(good), false},
		{good[:31], false},
		{good + "0", false},
		{"../../../../etc/passwd" + good[:10], false},
		{strings.Repeat("g", 32), false},
		{"", false},
	} {
		if got := hp.ValidAgentID(c.s); got != c.want {
			t.Errorf("ValidAgentID(%q) = %v", c.s, got)
		}
	}
	for _, c := range []struct {
		s    string
		want bool
	}{
		{testPane, true},
		{"pane_0123456789ABCDEF", false},
		{"pane_0123456789abcde", false},
		{"pane_0123456789abcdef0", false},
		{"pane-0123456789abcdef", false},
		{"pane_../../0123456789", false},
	} {
		if got := hp.ValidPaneID(c.s); got != c.want {
			t.Errorf("ValidPaneID(%q) = %v", c.s, got)
		}
	}
	id, err := hp.NewAgentID()
	if err != nil || !hp.ValidAgentID(id) {
		t.Fatalf("NewAgentID = %q, %v", id, err)
	}
}

func TestYamuxConfigIsExplicit(t *testing.T) {
	c := hp.YamuxConfig(nil)
	if c.AcceptBacklog != 256 || !c.EnableKeepAlive || c.KeepAliveInterval != 15*time.Second ||
		c.ConnectionWriteTimeout != 10*time.Second || c.MaxStreamWindowSize != 256<<10 ||
		c.StreamOpenTimeout != 30*time.Second || c.StreamCloseTimeout != 5*time.Minute {
		t.Fatalf("yamux config drifted from the spec: %+v", c)
	}
	if err := yamux.VerifyConfig(c); err != nil {
		t.Fatal(err)
	}
}

func TestReadHeaderLeavesRawBytes(t *testing.T) {
	r := bytes.NewReader([]byte(`{"s":"agent","agent":"x"}` + "\n\x00\x00\x00\x00\x05raw"))
	h, err := hp.ReadHeader(r)
	if err != nil || h.S != hp.KindAgent || h.Agent != "x" {
		t.Fatalf("header %+v, %v", h, err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "\x00\x00\x00\x00\x05raw" {
		t.Fatalf("header read consumed raw bytes: %q", rest)
	}
	long := bytes.Repeat([]byte("a"), hp.MaxLine+1)
	if _, err := hp.ReadHeader(bytes.NewReader(long)); !errors.Is(err, hp.ErrLineTooLong) {
		t.Fatalf("over-long header: %v", err)
	}
}

func TestControlRoundTrip(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	if !p.Host.HelloPassed() || p.Hello.UID != 1000 || p.Hello.Arch != "x86_64" {
		t.Fatalf("hello: %+v", p.Hello)
	}
	c := ctx(t)
	sp, err := p.Host.Spawn(c, spawnArgs())
	if err != nil {
		t.Fatal(err)
	}
	if pane, ok := p.Host.PaneForAgent(sp.AgentID); !ok || pane != testPane {
		t.Fatalf("PaneForAgent = %q, %v", pane, ok)
	}
	ag, _ := p.Fake.Agent(sp.AgentID)
	if ag.Spawn.Argv[0] != "bash" || ag.Spawn.Cols != 80 {
		t.Fatalf("spawn args not delivered: %+v", ag.Spawn)
	}
	ag.SetCwd("/home/fake/src")
	if cwd, err := p.Host.Cwd(c, sp.AgentID); err != nil || cwd != "/home/fake/src" {
		t.Fatalf("cwd = %q, %v", cwd, err)
	}
	if err := p.Host.Reap(c, sp.AgentID); hp.ErrorCode(err) != hp.CodeBusy {
		t.Fatalf("reap of a live agent: %v", err)
	}
	if err := p.Host.Kill(c, sp.AgentID, hp.KillTerm); err != nil {
		t.Fatal(err)
	}
	if k := ag.Kills(); len(k) != 1 || k[0] != hp.KillTerm {
		t.Fatalf("kills = %v", k)
	}
	if err := p.Host.Reap(c, sp.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Host.PaneForAgent(sp.AgentID); ok {
		t.Fatal("reaped agent still mapped")
	}
	missing := strings.Repeat("ab", 16)
	if _, err := p.Host.Cwd(c, missing); hp.ErrorCode(err) != hp.CodeNotFound {
		t.Fatalf("cwd of unknown agent: %v", err)
	}
}

func TestListPagesToTheEnd(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	for i := 0; i < 300; i++ {
		p.Fake.AddAgent(testPane)
	}
	c := ctx(t)
	page, err := p.Host.List(c, hp.ListArgs{})
	if err != nil || len(page.Agents) != hp.ListLimit || page.Next == "" {
		t.Fatalf("first page: %d records, next %q, %v", len(page.Agents), page.Next, err)
	}
	all, err := p.Host.ListAll(c)
	if err != nil || len(all) != 300 {
		t.Fatalf("ListAll: %d, %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].AgentID >= all[i].AgentID {
			t.Fatal("ListAll out of order")
		}
	}
}

func TestBadListPageIsABreak(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	fake.ListHook = func(context.Context, hp.ListArgs) (hp.ListResult, bool, error) {
		a, b := strings.Repeat("b", 32), strings.Repeat("a", 32)
		return hp.ListResult{Agents: []hp.AgentEntry{{AgentID: a, PaneID: testPane}, {AgentID: b, PaneID: testPane}}}, true, nil
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	if _, err := p.Host.List(ctx(t), hp.ListArgs{}); !hp.IsBreak(err) {
		t.Fatalf("out-of-order page: %v", err)
	}
	if !hp.IsBreak(waitDone(t, p.Host)) {
		t.Fatal("session should end with a break")
	}
}

func TestHostChecksBeforeSending(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{NoHello: true})
	c := ctx(t)
	if _, err := p.Host.Spawn(c, spawnArgs()); err == nil {
		t.Fatal("spawn before hello was sent")
	}
	if _, err := p.Host.Hello(c, hp.HelloArgs{Proto: 1, HostVersion: "0.0.0-test"}, 1000); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*hp.SpawnArgs){
		"pane id": func(a *hp.SpawnArgs) { a.PaneID = "pane_../x" },
		"argv":    func(a *hp.SpawnArgs) { a.Argv = nil },
		"glm":     func(a *hp.SpawnArgs) { a.Provider = "glm" },
		"env key": func(a *hp.SpawnArgs) { a.Env = map[string]string{"A,B": "1"} },
		"env NUL": func(a *hp.SpawnArgs) { a.Env = map[string]string{"A": "x\x00y"} },
		"env value comma injection": func(a *hp.SpawnArgs) {
			a.Env = map[string]string{"X": "a,SOLU_QUOTA_WORKER=1"}
		},
		"size":     func(a *hp.SpawnArgs) { a.Cols = 0 },
		"provider": func(a *hp.SpawnArgs) { a.Provider = "other" },
	} {
		a := spawnArgs()
		mut(&a)
		if _, err := p.Host.Spawn(c, a); hp.ErrorCode(err) != hp.CodeBadArgs {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := p.Host.Kill(c, strings.Repeat("a", 32), "hup"); hp.ErrorCode(err) != hp.CodeBadArgs {
		t.Errorf("kill how: %v", err)
	}
	if _, err := p.Host.OpenAgent("../x"); hp.ErrorCode(err) != hp.CodeBadArgs {
		t.Errorf("open agent: %v", err)
	}
}

func TestHelloMismatchIsABreak(t *testing.T) {
	for name, o := range map[string]func(*hp.HelloArgs, *int){
		"version": func(a *hp.HelloArgs, _ *int) { a.HostVersion = "0.0.1" },
		"uid":     func(_ *hp.HelloArgs, uid *int) { *uid = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			p := helpertest.NewPair(t, helpertest.Options{NoHello: true})
			a, uid := hp.HelloArgs{Proto: 1, HostVersion: "0.0.0-test"}, 1000
			o(&a, &uid)
			if _, err := p.Host.Hello(ctx(t), a, uid); !hp.IsBreak(err) {
				t.Fatalf("hello: %v", err)
			}
			if !hp.IsBreak(waitDone(t, p.Host)) || p.Host.HelloPassed() {
				t.Fatal("mismatch must end the session")
			}
		})
	}
}

func TestUnsafeRuntimeDirEndsTheHelper(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	fake.HelloErr = hp.Errorf(hp.CodeUnsafeRuntimeDir, "agents/ is a symlink")
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake, NoHello: true})
	_, err := p.Host.Hello(ctx(t), hp.HelloArgs{Proto: 1, HostVersion: "0.0.0-test"}, 1000)
	if hp.ErrorCode(err) != hp.CodeUnsafeRuntimeDir {
		t.Fatalf("hello: %v", err)
	}
	if err := waitServe(t, p.ServeErr); hp.ErrorCode(err) != hp.CodeUnsafeRuntimeDir {
		t.Fatalf("Serve: %v", err)
	}
}

func TestAgentStreamCarriesRawBytes(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	ag := p.Fake.AddAgent(testPane)
	conn, err := p.Host.OpenAgent(ag.Info.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var far net.Conn
	select {
	case far = <-ag.Conns():
	case <-time.After(5 * time.Second):
		t.Fatal("agent stream never reached the agent")
	}
	frame := []byte{0x00, 0, 0, 0, 3, 'a', 'b', '\n'}
	go func() { _, _ = conn.Write(frame) }()
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(far, got); err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("host→agent: %q, %v", got, err)
	}
	go func() { _, _ = far.Write([]byte("replay")) }()
	got = make([]byte, 6)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "replay" {
		t.Fatalf("agent→host: %q, %v", got, err)
	}
}

func TestDrainIsABarrier(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	release := make(chan struct{})
	entered := make(chan struct{})
	fake.CwdHook = func(context.Context, string) (string, bool, error) {
		close(entered)
		<-release
		return "/slow", true, nil
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	ag := fake.AddAgent(testPane)
	conn, err := p.Host.OpenAgent(ag.Info.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	<-ag.Conns()
	c := ctx(t)

	cwdDone := make(chan string, 1)
	go func() { cwd, _ := p.Host.Cwd(c, ag.Info.AgentID); cwdDone <- cwd }()
	<-entered
	drained := make(chan error, 1)
	go func() { drained <- p.Host.Drain(c) }()
	select {
	case err := <-drained:
		t.Fatalf("drain replied while an op was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if cwd := <-cwdDone; cwd != "/slow" {
		t.Fatalf("in-flight op lost: %q", cwd)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if !fake.Drained() {
		t.Fatal("Helper.Drain did not run")
	}
	// The agent stream was closed before the reply.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("agent stream after drain: %v", err)
	}
	// Every later op but shutdown is busy, and new agent streams are refused.
	if _, err := p.Host.Spawn(c, spawnArgs()); hp.ErrorCode(err) != hp.CodeBusy {
		t.Fatalf("spawn after drain: %v", err)
	}
	if err := p.Host.Reap(c, ag.Info.AgentID); hp.ErrorCode(err) != hp.CodeBusy {
		t.Fatalf("reap after drain: %v", err)
	}
	conn2, err := p.Host.OpenAgent(ag.Info.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn2.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("new agent stream after drain: %v", err)
	}
	if err := p.Host.Shutdown(c); err != nil {
		t.Fatal(err)
	}
	if err := waitServe(t, p.ServeErr); !errors.Is(err, hp.ErrShutdown) {
		t.Fatalf("Serve after shutdown: %v", err)
	}
}

func TestLateReplyAfterCancelIsDropped(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	release := make(chan struct{})
	fake.CwdHook = func(_ context.Context, id string) (string, bool, error) {
		if id == strings.Repeat("a", 32) {
			<-release
			return "/late", true, nil
		}
		return "", false, nil
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Host.Cwd(short, strings.Repeat("a", 32)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cwd: %v", err)
	}
	close(release)
	ag := fake.AddAgent(testPane)
	ag.SetCwd("/next")
	if cwd, err := p.Host.Cwd(ctx(t), ag.Info.AgentID); err != nil || cwd != "/next" {
		t.Fatalf("session broke on a late reply: %q, %v", cwd, err)
	}
	if p.Host.Err() != nil {
		t.Fatal(p.Host.Err())
	}
}

func TestStreamCap(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	for i := 0; i < hp.MaxStreams-1; i++ {
		p.Fake.AddAgent(testPane)
	}
	c := ctx(t)
	if _, err := p.Host.ListAll(c); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Host.Spawn(c, spawnArgs()); !errors.Is(err, hp.ErrTooManyPanes) {
		t.Fatalf("spawn past the cap: %v", err)
	}
}

func TestHelperOpenedStreamIsABreakInV1(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{Helper: hp.HelperOptions{OpenKinds: []string{hp.KindFwd}}})
	// The host may break the session as soon as the header lands, before the
	// helper's write call returns, so Open itself may report the shutdown.
	if st, err := p.Helper.Open(hp.StreamHeader{S: hp.KindFwd, Kind: "http", Pane: testPane}); err == nil {
		defer st.Close()
	}
	if err := waitDone(t, p.Host); !hp.IsBreak(err) {
		t.Fatalf("host session: %v", err)
	}
}

func TestHelperOpenNeedsOptIn(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	if _, err := p.Helper.Open(hp.StreamHeader{S: hp.KindFwd}); hp.ErrorCode(err) != hp.CodeUnsupported {
		t.Fatalf("fwd without opt-in: %v", err)
	}
	if _, err := p.Host.Open(hp.StreamHeader{S: hp.KindScan}); hp.ErrorCode(err) != hp.CodeUnsupported {
		t.Fatalf("scan without opt-in: %v", err)
	}
}

func TestRegisteredFwdHandlerServes(t *testing.T) {
	got := make(chan hp.StreamHeader, 1)
	p := helpertest.NewPair(t, helpertest.Options{
		Host: hp.HostOptions{Handlers: map[string]hp.HostStreamHandler{
			hp.KindFwd: func(_ *hp.HostSession, h hp.StreamHeader, c net.Conn) {
				got <- h
				_, _ = c.Write([]byte("ok"))
				_ = c.Close()
			},
		}},
		Helper: hp.HelperOptions{OpenKinds: []string{hp.KindFwd}},
	})
	st, err := p.Helper.Open(hp.StreamHeader{S: hp.KindFwd, Kind: "http", Pane: testPane, Attest: json.RawMessage(`{"pid":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	h := <-got
	if h.Pane != testPane || string(h.Attest) != `{"pid":1}` {
		t.Fatalf("header %+v", h)
	}
	b, _ := io.ReadAll(st)
	if string(b) != "ok" || p.Host.Err() != nil {
		t.Fatalf("reply %q, session %v", b, p.Host.Err())
	}
}

func TestScanStreams(t *testing.T) {
	t.Run("refused without a handler", func(t *testing.T) {
		p := helpertest.NewPair(t, helpertest.Options{Host: hp.HostOptions{OpenKinds: []string{hp.KindScan}}})
		st, err := p.Host.Open(hp.StreamHeader{S: hp.KindScan, Root: "claude-projects"})
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := st.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("unhandled scan: %v", err)
		}
		if p.Host.Err() != nil {
			t.Fatal("a refused host-opened stream must not break the session")
		}
	})
	t.Run("served with a handler", func(t *testing.T) {
		p := helpertest.NewPair(t, helpertest.Options{
			Host: hp.HostOptions{OpenKinds: []string{hp.KindScan}},
			Helper: hp.HelperOptions{Handlers: map[string]hp.HelperStreamHandler{
				hp.KindScan: func(_ *hp.HelperSession, h hp.StreamHeader, c net.Conn) {
					_, _ = c.Write([]byte(`{"root":"` + h.Root + `"}` + "\n"))
					_ = c.Close()
				},
			}},
		})
		st, err := p.Host.Open(hp.StreamHeader{S: hp.KindScan, Root: "codex-sessions", Since: "2026-11-20T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(st).ReadString('\n')
		if err != nil || line != `{"root":"codex-sessions"}`+"\n" {
			t.Fatalf("scan: %q, %v", line, err)
		}
	})
}

func TestHelperExitsOnStdinEOF(t *testing.T) {
	hostEnd, helperEnd := helpertest.Pipe()
	helper, err := hp.NewHelperSession(helperEnd, helpertest.NewFakeHelper("v", 1000), hp.HelperOptions{})
	if err != nil {
		t.Fatal(err)
	}
	serve := make(chan error, 1)
	go func() { serve <- helper.Serve() }()
	host, err := hp.NewHostSession(hostEnd, hp.HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Hello(ctx(t), hp.HelloArgs{Proto: 1, HostVersion: "v"}, 1000); err != nil {
		t.Fatal(err)
	}
	_ = host.Close()
	_ = hostEnd.Close()
	if err := waitServe(t, serve); err != nil {
		t.Fatalf("Serve on EOF: %v", err)
	}
}

// rawHost drives a helper session with hand-written control lines.
type rawHost struct {
	ctl   net.Conn
	lines *bufio.Reader
	serve chan error
}

func newRawHost(t *testing.T) *rawHost {
	t.Helper()
	hostEnd, helperEnd := helpertest.Pipe()
	helper, err := hp.NewHelperSession(helperEnd, helpertest.NewFakeHelper("v", 1000), hp.HelperOptions{})
	if err != nil {
		t.Fatal(err)
	}
	serve := make(chan error, 1)
	go func() { serve <- helper.Serve() }()
	mux, err := yamux.Client(hostEnd, hp.YamuxConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mux.Close(); _ = hostEnd.Close(); _ = helper.Close() })
	ctl, err := mux.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := hp.WriteHeader(ctl, hp.StreamHeader{S: hp.KindControl}); err != nil {
		t.Fatal(err)
	}
	return &rawHost{ctl: ctl, lines: bufio.NewReader(ctl), serve: serve}
}

func (r *rawHost) send(t *testing.T, line string) {
	t.Helper()
	if _, err := r.ctl.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
}

func (r *rawHost) reply(t *testing.T) hp.Reply {
	t.Helper()
	_ = r.ctl.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := r.lines.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var rep hp.Reply
	if err := json.Unmarshal(line, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestHelperChecksEveryRequest(t *testing.T) {
	r := newRawHost(t)
	r.send(t, `{"id":1,"op":"spawn","args":{}}`)
	if rep := r.reply(t); rep.OK || rep.Error.Code != hp.CodeBadArgs {
		t.Fatalf("op before hello: %+v", rep)
	}
	r.send(t, `{"id":2,"op":"hello","args":{"proto":1,"hostVersion":"v","hostPid":1,"distro":"Ubuntu"}}`)
	if rep := r.reply(t); !rep.OK {
		t.Fatalf("hello: %+v", rep.Error)
	}
	cases := []struct{ req, code string }{
		{`{"id":3,"op":"frobnicate"}`, hp.CodeUnsupported},
		{`{"id":4,"op":"spawn","args":{"paneId":"pane_../../etc","argv":["sh"],"cols":80,"rows":24}}`, hp.CodeBadArgs},
		{`{"id":5,"op":"spawn","args":{"paneId":"` + testPane + `","argv":["sh"],"cols":80,"rows":24,"provider":"glm"}}`, hp.CodeBadArgs},
		{`{"id":6,"op":"cwd","args":{"agentId":"../../x"}}`, hp.CodeBadArgs},
		{`{"id":7,"op":"kill","args":{"agentId":"` + strings.Repeat("a", 32) + `","how":"stop"}}`, hp.CodeBadArgs},
		{`{"id":8,"op":"list","args":{"after":"zz"}}`, hp.CodeBadArgs},
		{`{"id":9,"op":"hello","args":{"proto":1,"hostVersion":"v"}}`, hp.CodeBadArgs},
		{`{"id":10,"op":"spawn","args":"nope"}`, hp.CodeBadArgs},
		// A comma in an env value would split into a second variable on the
		// agent's comma-joined --env wire.
		{`{"id":12,"op":"spawn","args":{"paneId":"` + testPane + `","argv":["sh"],"cols":80,"rows":24,"env":{"X":"a,SOLU_QUOTA_WORKER=1"}}}`, hp.CodeBadArgs},
	}
	for _, c := range cases {
		r.send(t, c.req)
		if rep := r.reply(t); rep.OK || rep.Error.Code != c.code {
			t.Errorf("%s: %+v", c.req, rep)
		}
	}
	// Unknown fields are ignored (additive changes stay compatible).
	r.send(t, `{"id":11,"op":"list","args":{"limit":5,"future":true}}`)
	if rep := r.reply(t); !rep.OK {
		t.Fatalf("unknown field: %+v", rep.Error)
	}
}

func TestHelperBreaksOnOverlongLine(t *testing.T) {
	r := newRawHost(t)
	go func() { _, _ = r.ctl.Write(append(bytes.Repeat([]byte("x"), hp.MaxLine+1), '\n')) }()
	if err := waitServe(t, r.serve); !hp.IsBreak(err) {
		t.Fatalf("Serve: %v", err)
	}
}

func TestHelperBreaksOnBadJSON(t *testing.T) {
	r := newRawHost(t)
	// The helper breaks the session as soon as the line lands, which can
	// beat this write call returning: yamux then reports a shutdown for a
	// line it delivered (as in #1319), so the write's result isn't checked.
	_, _ = r.ctl.Write([]byte(`{"id":1,` + "\n"))
	if err := waitServe(t, r.serve); !hp.IsBreak(err) {
		t.Fatalf("Serve: %v", err)
	}
}

// rawHelper answers a host session with hand-written control lines.
func rawHelper(t *testing.T) (*hp.HostSession, net.Conn, *bufio.Reader) {
	t.Helper()
	hostEnd, helperEnd := helpertest.Pipe()
	mux, err := yamux.Server(helperEnd, hp.YamuxConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mux.Close(); _ = helperEnd.Close() })
	host, err := hp.NewHostSession(hostEnd, hp.HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctl, err := mux.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if h, err := hp.ReadHeader(ctl); err != nil || h.S != hp.KindControl {
		t.Fatalf("first stream: %+v, %v", h, err)
	}
	return host, ctl, bufio.NewReader(ctl)
}

func TestHostBreaksOnReplyToUnknownID(t *testing.T) {
	host, ctl, _ := rawHelper(t)
	_, _ = ctl.Write([]byte(`{"id":99,"ok":true,"result":{}}` + "\n"))
	if err := waitDone(t, host); !hp.IsBreak(err) {
		t.Fatalf("host: %v", err)
	}
}

func TestHostBreaksOnOverlongReply(t *testing.T) {
	host, ctl, _ := rawHelper(t)
	go func() { _, _ = ctl.Write(append(bytes.Repeat([]byte("x"), hp.MaxLine+1), '\n')) }()
	if err := waitDone(t, host); !hp.IsBreak(err) {
		t.Fatalf("host: %v", err)
	}
}

func TestHostBreaksOnFailureWithoutError(t *testing.T) {
	host, ctl, lines := rawHelper(t)
	go func() { _, _ = host.Hello(context.Background(), hp.HelloArgs{Proto: 1, HostVersion: "v"}, 1000) }()
	if _, err := lines.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	_, _ = ctl.Write([]byte(`{"id":1,"ok":false}` + "\n"))
	if err := waitDone(t, host); !hp.IsBreak(err) {
		t.Fatalf("host: %v", err)
	}
}

func TestStreamCapHoldsUnderConcurrentOpens(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	ag := p.Fake.AddAgent(testPane)
	const tries = hp.MaxStreams + 44
	results := make(chan error, tries)
	start := make(chan struct{})
	for i := 0; i < tries; i++ {
		go func() {
			<-start
			_, err := p.Host.OpenAgent(ag.Info.AgentID)
			results <- err
		}()
	}
	close(start)
	opened, refused := 0, 0
	for i := 0; i < tries; i++ {
		switch err := <-results; {
		case err == nil:
			opened++
		case errors.Is(err, hp.ErrTooManyPanes):
			refused++
		default:
			t.Fatalf("open: %v", err)
		}
	}
	if opened != hp.MaxStreams-1 || refused != tries-opened {
		t.Fatalf("opened %d, refused %d; want %d streams beside control", opened, refused, hp.MaxStreams-1)
	}
}

func TestClosedStreamReleasesItsSlot(t *testing.T) {
	p := helpertest.NewPair(t, helpertest.Options{})
	ag := p.Fake.AddAgent(testPane)
	var conns []net.Conn
	for i := 0; i < hp.MaxStreams-1; i++ {
		c, err := p.Host.OpenAgent(ag.Info.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	if _, err := p.Host.OpenAgent(ag.Info.AgentID); !errors.Is(err, hp.ErrTooManyPanes) {
		t.Fatalf("past the cap: %v", err)
	}
	_ = conns[0].Close()
	_ = conns[0].Close() // a second Close must not release twice
	if _, err := p.Host.OpenAgent(ag.Info.AgentID); err != nil {
		t.Fatalf("after a close: %v", err)
	}
	if _, err := p.Host.OpenAgent(ag.Info.AgentID); !errors.Is(err, hp.ErrTooManyPanes) {
		t.Fatalf("double release: %v", err)
	}
}

func TestCanceledCallIsNeverSent(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	spawns := 0
	fake.SpawnHook = func(context.Context, hp.SpawnArgs) (hp.SpawnResult, bool, error) {
		spawns++
		return hp.SpawnResult{}, false, nil
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Host.Spawn(c, spawnArgs()); !errors.Is(err, context.Canceled) {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := p.Host.List(ctx(t), hp.ListArgs{}); err != nil { // a round trip after
		t.Fatal(err)
	}
	if spawns != 0 {
		t.Fatalf("a canceled spawn reached the helper %d time(s)", spawns)
	}
}

func TestLiveRecordWithoutIdentityIsABreak(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	fake.ListHook = func(context.Context, hp.ListArgs) (hp.ListResult, bool, error) {
		return hp.ListResult{Agents: []hp.AgentEntry{{AgentID: strings.Repeat("a", 32), PaneID: testPane, Pid: 7, Alive: true}}}, true, nil
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	if _, err := p.Host.List(ctx(t), hp.ListArgs{}); !hp.IsBreak(err) {
		t.Fatalf("live record without startTicks: %v", err)
	}
}

func TestDrainDoesNotHangWhenTheHostVanishes(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	entered := make(chan struct{})
	fake.CwdHook = func(c context.Context, _ string) (string, bool, error) {
		close(entered)
		<-c.Done() // only the session's cancellation ends this op
		return "", true, c.Err()
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	go func() { _, _ = p.Host.Cwd(context.Background(), strings.Repeat("a", 32)) }()
	<-entered
	go func() { _ = p.Host.Drain(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // let drain reach its barrier
	_ = p.Host.Close()
	if err := waitServe(t, p.ServeErr); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestShutdownIsReadWhileDrainWaits(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	entered := make(chan struct{})
	fake.CwdHook = func(c context.Context, _ string) (string, bool, error) {
		close(entered)
		<-c.Done()
		return "", true, c.Err()
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	go func() { _, _ = p.Host.Cwd(context.Background(), strings.Repeat("a", 32)) }()
	<-entered
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Host.Drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain should still be waiting: %v", err)
	}
	// The spec's fallback: shutdown after a drain timeout.
	if err := p.Host.Shutdown(ctx(t)); err != nil {
		t.Fatalf("shutdown while drain waits: %v", err)
	}
	if err := waitServe(t, p.ServeErr); !errors.Is(err, hp.ErrShutdown) {
		t.Fatalf("Serve: %v", err)
	}
}

func TestConcurrentSpawnsHoldTheCap(t *testing.T) {
	fake := helpertest.NewFakeHelper("0.0.0-test", 1000)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	fake.SpawnHook = func(context.Context, hp.SpawnArgs) (hp.SpawnResult, bool, error) {
		entered <- struct{}{}
		<-release
		return hp.SpawnResult{}, false, nil
	}
	p := helpertest.NewPair(t, helpertest.Options{Fake: fake})
	for i := 0; i < hp.MaxStreams-2; i++ {
		fake.AddAgent(testPane)
	}
	c := ctx(t)
	if _, err := p.Host.ListAll(c); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := p.Host.Spawn(c, spawnArgs()); results <- err }()
	}
	// One spawn holds the last pane slot; the other is refused at once.
	if err := <-results; !errors.Is(err, hp.ErrTooManyPanes) {
		t.Fatalf("second concurrent spawn: %v", err)
	}
	<-entered
	close(release)
	if err := <-results; err != nil {
		t.Fatalf("first spawn: %v", err)
	}
}
