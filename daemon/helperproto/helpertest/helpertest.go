// Package helpertest holds in-memory fakes for the helper stdio protocol:
// a stdio transport pair, a fake helper with test-registered agents, and a
// connected host/helper session pair. W2 tests the wire with them; W3 (fwd)
// and W4 (scan) build on them before a real helper exists.
package helpertest

import (
	"context"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"soluterminal/helperproto"
)

// Pipe returns the two ends of an in-memory stdio transport. Closing one end
// gives the other EOF, as a helper sees when the host goes away.
func Pipe() (host, helper io.ReadWriteCloser) {
	toHelperR, toHelperW := io.Pipe()
	toHostR, toHostW := io.Pipe()
	return &pipeEnd{r: toHostR, w: toHelperW}, &pipeEnd{r: toHelperR, w: toHostW}
}

type pipeEnd struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *pipeEnd) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeEnd) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeEnd) Close() error {
	_ = p.w.Close()
	return p.r.Close()
}

// FakeAgent is one in-memory agent.
type FakeAgent struct {
	Info  helperproto.AgentInfo
	Spawn helperproto.SpawnArgs // zero for agents added with AddAgent

	mu     sync.Mutex
	alive  bool
	cwd    string
	killed []string
	conns  chan net.Conn
}

// Conns delivers the agent-side end of each agent stream the host opens.
// Bytes written there reach the host's stream, and the other way round.
func (a *FakeAgent) Conns() <-chan net.Conn { return a.conns }

// SetCwd sets what the cwd op returns.
func (a *FakeAgent) SetCwd(cwd string) { a.mu.Lock(); a.cwd = cwd; a.mu.Unlock() }

// SetAlive marks the agent alive or dead (list's alive, reap's busy).
func (a *FakeAgent) SetAlive(v bool) { a.mu.Lock(); a.alive = v; a.mu.Unlock() }

// Kills returns the kill methods received, in order.
func (a *FakeAgent) Kills() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.killed...)
}

// FakeHelper implements helperproto.Helper and helperproto.AgentRegistry
// with in-memory agents. The optional hooks run before the default
// behaviour; a hook that returns handled=true replaces it.
type FakeHelper struct {
	Version    string
	UID        int
	RuntimeDir string
	Networking string

	HelloErr  error
	SpawnHook func(ctx context.Context, a helperproto.SpawnArgs) (r helperproto.SpawnResult, handled bool, err error)
	ListHook  func(ctx context.Context, a helperproto.ListArgs) (r helperproto.ListResult, handled bool, err error)
	CwdHook   func(ctx context.Context, agentID string) (cwd string, handled bool, err error)

	mu      sync.Mutex
	agents  map[string]*FakeAgent
	nextPid int
	drained bool
}

// NewFakeHelper returns a fake helper that reports version and uid at hello.
func NewFakeHelper(version string, uid int) *FakeHelper {
	return &FakeHelper{
		Version:    version,
		UID:        uid,
		RuntimeDir: "/run/user/1000/soluterminal-helper",
		Networking: helperproto.NetworkingNAT,
		agents:     map[string]*FakeAgent{},
		nextPid:    1000,
	}
}

// AddAgent registers a running agent for paneID, as if an earlier helper had
// spawned it.
func (f *FakeHelper) AddAgent(paneID string) *FakeAgent {
	id, err := helperproto.NewAgentID()
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPid++
	a := &FakeAgent{
		Info: helperproto.AgentInfo{
			AgentID:    id,
			PaneID:     paneID,
			Pid:        f.nextPid,
			StartTicks: uint64(f.nextPid) * 100,
		},
		alive: true,
		conns: make(chan net.Conn, 16),
	}
	f.agents[id] = a
	return a
}

// Agent returns a registered agent.
func (f *FakeHelper) Agent(agentID string) (*FakeAgent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[agentID]
	return a, ok
}

// Drained reports whether Drain ran.
func (f *FakeHelper) Drained() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drained
}

// AgentByPid implements helperproto.AgentRegistry.
func (f *FakeHelper) AgentByPid(pid int) (helperproto.AgentInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.agents {
		if a.Info.Pid == pid {
			return a.Info, true
		}
	}
	return helperproto.AgentInfo{}, false
}

func (f *FakeHelper) Hello(_ context.Context, a helperproto.HelloArgs) (helperproto.HelloResult, error) {
	if f.HelloErr != nil {
		return helperproto.HelloResult{}, f.HelloErr
	}
	return helperproto.HelloResult{
		Proto:         helperproto.Proto,
		HelperVersion: f.Version,
		Pid:           os.Getpid(),
		UID:           f.UID,
		User:          "fake",
		Home:          "/home/fake",
		Arch:          "x86_64",
		Kernel:        "fake",
		Networking:    f.Networking,
		RuntimeDir:    f.RuntimeDir,
	}, nil
}

func (f *FakeHelper) Spawn(ctx context.Context, a helperproto.SpawnArgs) (helperproto.SpawnResult, error) {
	if f.SpawnHook != nil {
		if r, ok, err := f.SpawnHook(ctx, a); ok {
			return r, err
		}
	}
	ag := f.AddAgent(a.PaneID)
	ag.Spawn = a
	ag.SetCwd(a.Cwd)
	return helperproto.SpawnResult{AgentID: ag.Info.AgentID, Pid: ag.Info.Pid, StartTicks: ag.Info.StartTicks}, nil
}

func (f *FakeHelper) List(ctx context.Context, a helperproto.ListArgs) (helperproto.ListResult, error) {
	if f.ListHook != nil {
		if r, ok, err := f.ListHook(ctx, a); ok {
			return r, err
		}
	}
	f.mu.Lock()
	ids := make([]string, 0, len(f.agents))
	for id := range f.agents {
		if id > a.After {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	r := helperproto.ListResult{Agents: []helperproto.AgentEntry{}}
	for i, id := range ids {
		if i == a.Limit {
			r.Next = ids[i-1]
			break
		}
		ag := f.agents[id]
		ag.mu.Lock()
		r.Agents = append(r.Agents, helperproto.AgentEntry{
			AgentID: id, PaneID: ag.Info.PaneID, Pid: ag.Info.Pid,
			StartTicks: ag.Info.StartTicks, Alive: ag.alive,
		})
		ag.mu.Unlock()
	}
	f.mu.Unlock()
	return r, nil
}

func (f *FakeHelper) Cwd(ctx context.Context, agentID string) (string, error) {
	if f.CwdHook != nil {
		if cwd, ok, err := f.CwdHook(ctx, agentID); ok {
			return cwd, err
		}
	}
	a, ok := f.Agent(agentID)
	if !ok {
		return "", helperproto.Errorf(helperproto.CodeNotFound, "no agent %s", agentID)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cwd, nil
}

func (f *FakeHelper) Kill(_ context.Context, k helperproto.KillArgs) error {
	a, ok := f.Agent(k.AgentID)
	if !ok {
		return helperproto.Errorf(helperproto.CodeNotFound, "no agent %s", k.AgentID)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.alive {
		return helperproto.Errorf(helperproto.CodeNotFound, "agent %s is not running", k.AgentID)
	}
	a.killed = append(a.killed, k.How)
	a.alive = false
	return nil
}

func (f *FakeHelper) Reap(_ context.Context, agentID string) error {
	a, ok := f.Agent(agentID)
	if !ok {
		return helperproto.Errorf(helperproto.CodeNotFound, "no agent %s", agentID)
	}
	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if alive {
		return helperproto.Errorf(helperproto.CodeBusy, "agent %s is still running", agentID)
	}
	f.mu.Lock()
	delete(f.agents, agentID)
	f.mu.Unlock()
	return nil
}

func (f *FakeHelper) Drain(context.Context) error {
	f.mu.Lock()
	f.drained = true
	f.mu.Unlock()
	return nil
}

// ServeAgent splices the stream to a net.Pipe whose other end goes to the
// agent's Conns channel, and returns once either side closes.
func (f *FakeHelper) ServeAgent(agentID string, c net.Conn) {
	a, ok := f.Agent(agentID)
	if !ok {
		return
	}
	near, far := net.Pipe()
	select {
	case a.conns <- far:
	default: // nobody is collecting; refuse
		_ = near.Close()
		_ = far.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(near, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, near); done <- struct{}{} }()
	<-done
	_ = near.Close()
	_ = c.Close()
	_ = c.SetDeadline(time.Unix(1, 0))
	<-done
}

// Options configures NewPair.
type Options struct {
	Host    helperproto.HostOptions
	Helper  helperproto.HelperOptions
	Version string // default "0.0.0-test"
	UID     int    // default 1000
	// NoHello leaves hello to the test.
	NoHello bool
	// Fake replaces the default fake helper.
	Fake *FakeHelper
}

// Pair is a host session and a helper session connected in memory.
type Pair struct {
	Host     *helperproto.HostSession
	Helper   *helperproto.HelperSession
	Fake     *FakeHelper
	Hello    helperproto.HelloResult
	ServeErr <-chan error // Helper.Serve's result
}

// NewPair connects a host session to a fake helper and, unless NoHello is
// set, runs hello. Both ends close when the test ends.
func NewPair(t testing.TB, o Options) *Pair {
	t.Helper()
	if o.Version == "" {
		o.Version = "0.0.0-test"
	}
	if o.UID == 0 {
		o.UID = 1000
	}
	fake := o.Fake
	if fake == nil {
		fake = NewFakeHelper(o.Version, o.UID)
	}
	hostEnd, helperEnd := Pipe()
	helper, err := helperproto.NewHelperSession(helperEnd, fake, o.Helper)
	if err != nil {
		t.Fatalf("helper session: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- helper.Serve() }()
	host, err := helperproto.NewHostSession(hostEnd, o.Host)
	if err != nil {
		t.Fatalf("host session: %v", err)
	}
	p := &Pair{Host: host, Helper: helper, Fake: fake, ServeErr: serveErr}
	t.Cleanup(func() {
		_ = host.Close()
		_ = helper.Close()
		_ = hostEnd.Close()
		_ = helperEnd.Close()
	})
	if !o.NoHello {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.Hello, err = host.Hello(ctx, helperproto.HelloArgs{
			Proto: helperproto.Proto, HostVersion: o.Version, HostPid: os.Getpid(), Distro: "Ubuntu",
		}, o.UID)
		if err != nil {
			t.Fatalf("hello: %v", err)
		}
	}
	return p
}
