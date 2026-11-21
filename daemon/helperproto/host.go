package helperproto

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// ErrTooManyPanes is returned when a spawn or stream would pass MaxStreams.
var ErrTooManyPanes = errors.New("too many panes in this distro")

// ErrClosed is the error of a session closed on purpose or by the helper.
var ErrClosed = errors.New("helper session closed")

var errNoHello = errors.New("helper hello has not passed")

// HostStreamHandler serves a stream the helper opened. It owns c.
type HostStreamHandler func(s *HostSession, h StreamHeader, c net.Conn)

// HostOptions configures the host end.
type HostOptions struct {
	// Handlers serves helper-opened stream kinds. Empty in v1, where every
	// helper-opened stream is a protocol break (ruling 8.6). M3 registers
	// KindFwd here once its own ruling is in.
	Handlers map[string]HostStreamHandler
	// OpenKinds lists the reserved host-opened kinds Open may use, beyond
	// agent streams. Empty in v1; M4 adds KindScan once its ruling is in.
	OpenKinds []string
	// LogOutput receives yamux's own log lines; nil discards them.
	LogOutput io.Writer
	// Logf logs session events; nil discards them.
	Logf func(format string, args ...any)
}

// HostSession is the host's end of one helper's session. It is safe for
// concurrent use. When the session ends, Done closes and Err says why; a
// *BreakError means the helper must be killed and restarted with backoff.
type HostSession struct {
	opts HostOptions
	mux  *yamux.Session
	ctl  net.Conn
	w    *lineWriter

	helloSent atomic.Bool
	helloOK   atomic.Bool

	mu        sync.Mutex
	nextID    uint64
	pending   map[uint64]chan Reply
	abandoned map[uint64]struct{}
	agents    map[string]string // agentId → paneId
	streams   int               // open streams, control included (≤ MaxStreams)
	spawning  int               // spawns in flight, each holding a pane slot
	err       error

	done     chan struct{}
	failOnce sync.Once
}

// NewHostSession starts the client end of the session over conn (the helper's
// stdout for reads, its stdin for writes) and opens the control stream.
func NewHostSession(conn io.ReadWriteCloser, opts HostOptions) (*HostSession, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	mux, err := yamux.Client(conn, YamuxConfig(opts.LogOutput))
	if err != nil {
		return nil, err
	}
	ctl, err := mux.Open()
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	if err := WriteHeader(ctl, StreamHeader{S: KindControl}); err != nil {
		_ = mux.Close()
		return nil, err
	}
	s := &HostSession{
		opts:      opts,
		mux:       mux,
		ctl:       ctl,
		w:         newLineWriter(ctl),
		pending:   map[uint64]chan Reply{},
		abandoned: map[uint64]struct{}{},
		agents:    map[string]string{},
		streams:   1, // control
		done:      make(chan struct{}),
	}
	// readReplies ends the session: when the mux closes, the control stream
	// still yields any reply already received, then EOF.
	go s.readReplies()
	go s.acceptLoop()
	return s, nil
}

// Done closes when the session has ended.
func (s *HostSession) Done() <-chan struct{} { return s.done }

// Err reports why the session ended, or nil while it runs.
func (s *HostSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the session. The helper sees EOF and exits; agents keep running.
func (s *HostSession) Close() error {
	s.fail(ErrClosed)
	return nil
}

// HelloPassed reports whether hello succeeded and its checks held.
func (s *HostSession) HelloPassed() bool { return s.helloOK.Load() }

// PaneForAgent returns the pane an agent was spawned for, as learned from
// this session's spawn and list replies.
func (s *HostSession) PaneForAgent(agentID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.agents[agentID]
	return p, ok
}

func (s *HostSession) fail(err error) {
	s.failOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.pending = nil
		s.mu.Unlock()
		if IsBreak(err) {
			s.opts.Logf("helper session: %v", err)
		}
		close(s.done)
		_ = s.mux.Close()
	})
}

func (s *HostSession) readReplies() {
	lr := newLineReader(s.ctl)
	for {
		line, err := lr.next()
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				s.fail(ErrClosed)
			case errors.Is(err, ErrLineTooLong), errors.Is(err, io.ErrUnexpectedEOF):
				s.fail(protocolBreak("control: %v", err))
			default:
				s.fail(err)
			}
			return
		}
		var r Reply
		if err := json.Unmarshal(line, &r); err != nil {
			s.fail(protocolBreak("control: bad reply: %v", err))
			return
		}
		if !r.OK && r.Error == nil {
			s.fail(protocolBreak("control: reply %d failed without an error", r.ID))
			return
		}
		s.mu.Lock()
		ch, ok := s.pending[r.ID]
		if ok {
			delete(s.pending, r.ID)
		} else if _, gone := s.abandoned[r.ID]; gone {
			delete(s.abandoned, r.ID)
			ok = true
		}
		s.mu.Unlock()
		if !ok {
			s.fail(protocolBreak("control: reply to unknown id %d", r.ID))
			return
		}
		if ch != nil {
			ch <- r
		}
	}
}

// call sends one request and waits for its reply. A reply with ok:false
// returns its *Error. A reply that arrives after ctx ends is dropped.
func (s *HostSession) call(ctx context.Context, op string, args, out any) error {
	if op != OpHello && !s.helloOK.Load() {
		return errNoHello
	}
	if err := ctx.Err(); err != nil {
		return err // never send a request whose caller already gave up
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	ch := make(chan Reply, 1)
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.w.writeCtx(ctx, Request{ID: id, Op: op, Args: raw}); err != nil {
		var notSent errNotSent
		if errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, yamux.ErrStreamClosed) {
			// yamux can report a shutdown for a frame it already sent: the
			// helper may answer and exit (hello refusing its runtime dir,
			// shutdown) before this write call returns. Keep the request
			// pending; readReplies delivers a reply that came in before the
			// session ended, then ends it.
			select {
			case r := <-ch:
				return s.finish(op, r, out)
			case <-s.done:
				select {
				case r := <-ch:
					return s.finish(op, r, out)
				default:
					return s.Err()
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		s.mu.Lock()
		if s.pending != nil {
			delete(s.pending, id)
		}
		s.mu.Unlock()
		if errors.Is(err, ErrLineTooLong) || errors.As(err, &notSent) {
			return err // refused locally; nothing was sent
		}
		s.fail(err)
		return err
	}
	select {
	case r := <-ch:
		return s.finish(op, r, out)
	case <-ctx.Done():
		s.mu.Lock()
		if _, ok := s.pending[id]; ok {
			delete(s.pending, id)
			s.abandoned[id] = struct{}{}
		}
		s.mu.Unlock()
		return ctx.Err()
	case <-s.done:
		select {
		case r := <-ch: // the last reply before the helper exited
			return s.finish(op, r, out)
		default:
			return s.Err()
		}
	}
}

func (s *HostSession) finish(op string, r Reply, out any) error {
	if !r.OK {
		return r.Error
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			brk := protocolBreak("control: bad %s result: %v", op, err)
			s.fail(brk)
			return brk
		}
	}
	return nil
}

// Hello must be the first request. The session fails with a *BreakError,
// so the supervisor kills the helper, unless the reply is proto 1, reports
// helperVersion == a.HostVersion and uid == wantUID (the uid read at
// placement), and names a runtime dir and a known networking mode.
func (s *HostSession) Hello(ctx context.Context, a HelloArgs, wantUID int) (HelloResult, error) {
	var r HelloResult
	if e := validateHello(&a); e != nil {
		return r, e
	}
	if !s.helloSent.CompareAndSwap(false, true) {
		return r, errors.New("hello already sent")
	}
	if err := s.call(ctx, OpHello, a, &r); err != nil {
		return r, err
	}
	var brk *BreakError
	switch {
	case r.Proto != Proto:
		brk = protocolBreak("hello: helper proto %d", r.Proto)
	case r.HelperVersion != a.HostVersion:
		brk = protocolBreak("hello: helper version %q, host %q", r.HelperVersion, a.HostVersion)
	case r.UID != wantUID:
		brk = protocolBreak("hello: helper uid %d, placement read %d", r.UID, wantUID)
	case r.RuntimeDir == "":
		brk = protocolBreak("hello: no runtime dir")
	case r.Networking != NetworkingNAT && r.Networking != NetworkingMirrored && r.Networking != NetworkingUnknown:
		brk = protocolBreak("hello: networking %q", r.Networking)
	}
	if brk != nil {
		s.fail(brk)
		return r, brk
	}
	s.helloOK.Store(true)
	return r, nil
}

// Spawn starts a pane's agent in the distro.
func (s *HostSession) Spawn(ctx context.Context, a SpawnArgs) (SpawnResult, error) {
	var r SpawnResult
	if e := validateSpawn(&a); e != nil {
		return r, e
	}
	// One pane is one stream: reserve the pane's slot for the whole spawn,
	// so concurrent spawns can't pass MaxStreams-1 panes between them.
	s.mu.Lock()
	if 1+len(s.agents)+s.spawning >= MaxStreams || s.streams >= MaxStreams {
		s.mu.Unlock()
		return r, ErrTooManyPanes
	}
	s.spawning++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.spawning--
		s.mu.Unlock()
	}()
	if err := s.call(ctx, OpSpawn, a, &r); err != nil {
		return r, err
	}
	if !ValidAgentID(r.AgentID) || r.Pid <= 0 || r.StartTicks == 0 {
		brk := protocolBreak("spawn: bad result")
		s.fail(brk)
		return r, brk
	}
	s.mu.Lock()
	s.agents[r.AgentID] = a.PaneID
	s.mu.Unlock()
	return r, nil
}

// List returns one page of the helper's agents.
func (s *HostSession) List(ctx context.Context, a ListArgs) (ListResult, error) {
	var r ListResult
	if e := validateList(&a); e != nil {
		return r, e
	}
	if err := s.call(ctx, OpList, a, &r); err != nil {
		return r, err
	}
	if brk := checkListPage(a, r); brk != nil {
		s.fail(brk)
		return r, brk
	}
	s.mu.Lock()
	for _, e := range r.Agents {
		s.agents[e.AgentID] = e.PaneID
	}
	s.mu.Unlock()
	return r, nil
}

func checkListPage(a ListArgs, r ListResult) *BreakError {
	if len(r.Agents) > a.Limit {
		return protocolBreak("list: %d records over limit %d", len(r.Agents), a.Limit)
	}
	prev := a.After
	for _, e := range r.Agents {
		if !ValidAgentID(e.AgentID) || !ValidPaneID(e.PaneID) || e.Pid < 0 {
			return protocolBreak("list: bad record")
		}
		if e.Alive && (e.Pid == 0 || e.StartTicks == 0) {
			return protocolBreak("list: a live record without a process identity")
		}
		if e.AgentID <= prev {
			return protocolBreak("list: records out of order")
		}
		prev = e.AgentID
	}
	if r.Next != "" && (len(r.Agents) == 0 || r.Next != prev) {
		return protocolBreak("list: next is not the last record")
	}
	return nil
}

// ListAll pages through every agent the helper knows.
func (s *HostSession) ListAll(ctx context.Context) ([]AgentEntry, error) {
	var all []AgentEntry
	after := ""
	for {
		page, err := s.List(ctx, ListArgs{After: after})
		if err != nil {
			return all, err
		}
		all = append(all, page.Agents...)
		if page.Next == "" {
			return all, nil
		}
		after = page.Next
	}
}

// Cwd returns an agent's live working directory ("" when unknown).
func (s *HostSession) Cwd(ctx context.Context, agentID string) (string, error) {
	a := AgentRef{AgentID: agentID}
	if e := validateRef(OpCwd, &a); e != nil {
		return "", e
	}
	var r CwdResult
	err := s.call(ctx, OpCwd, a, &r)
	return r.Cwd, err
}

// Kill signals an agent that no longer answers on its socket. A normal kill
// is agentproto's FrameKillRequest on the agent stream instead.
func (s *HostSession) Kill(ctx context.Context, agentID, how string) error {
	a := KillArgs{AgentID: agentID, How: how}
	if e := validateKill(&a); e != nil {
		return e
	}
	return s.call(ctx, OpKill, a, nil)
}

// Reap removes a dead agent's socket and record.
func (s *HostSession) Reap(ctx context.Context, agentID string) error {
	a := AgentRef{AgentID: agentID}
	if e := validateRef(OpReap, &a); e != nil {
		return e
	}
	if err := s.call(ctx, OpReap, a, nil); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.agents, agentID)
	s.mu.Unlock()
	return nil
}

// Drain is the update-handoff barrier. When it returns nil the helper has
// finished every in-flight op, closed every agent stream and socket
// connection, and answers busy to everything but shutdown.
func (s *HostSession) Drain(ctx context.Context) error {
	return s.call(ctx, OpDrain, Empty{}, nil)
}

// Shutdown asks the helper to exit; agents keep running.
func (s *HostSession) Shutdown(ctx context.Context) error {
	return s.call(ctx, OpShutdown, Empty{}, nil)
}

// OpenAgent opens the agent stream for agentID: raw agentproto bytes both
// ways, spliced by the helper to that agent's unix socket. It stands in for
// dialAgent(path).
func (s *HostSession) OpenAgent(agentID string) (net.Conn, error) {
	if !ValidAgentID(agentID) {
		return nil, Errorf(CodeBadArgs, "bad agentId")
	}
	return s.open(StreamHeader{S: KindAgent, Agent: agentID})
}

// Open opens a reserved host-opened stream kind listed in OpenKinds.
func (s *HostSession) Open(h StreamHeader) (net.Conn, error) {
	if !contains(s.opts.OpenKinds, h.S) || h.S == KindControl || h.S == KindAgent {
		return nil, Errorf(CodeUnsupported, "stream kind %q is not enabled", h.S)
	}
	return s.open(h)
}

func (s *HostSession) open(h StreamHeader) (net.Conn, error) {
	if !s.helloOK.Load() {
		return nil, errNoHello
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if !s.admit() {
		return nil, ErrTooManyPanes
	}
	st, err := s.mux.Open()
	if err != nil {
		s.release()
		return nil, err
	}
	c := s.counted(st)
	if err := WriteHeader(c, h); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// admit reserves one of the session's MaxStreams; release returns it.
func (s *HostSession) admit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams >= MaxStreams {
		return false
	}
	s.streams++
	return true
}

func (s *HostSession) release() {
	s.mu.Lock()
	s.streams--
	s.mu.Unlock()
}

// counted wraps an admitted stream so its first Close releases its slot.
func (s *HostSession) counted(c net.Conn) net.Conn {
	return &countedConn{Conn: c, release: s.release}
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// acceptLoop handles streams the helper opens. In v1 no kind is registered,
// so every one is a protocol break (ruling 8.6).
func (s *HostSession) acceptLoop() {
	for {
		st, err := s.mux.Accept()
		if err != nil {
			return
		}
		go s.serveIncoming(st)
	}
}

func (s *HostSession) serveIncoming(st net.Conn) {
	_ = st.SetReadDeadline(time.Now().Add(HeaderTimeout))
	h, err := ReadHeader(st)
	_ = st.SetReadDeadline(time.Time{})
	if err != nil {
		_ = st.Close()
		s.fail(protocolBreak("helper-opened stream: %v", err))
		return
	}
	handler, ok := s.opts.Handlers[h.S]
	switch {
	case h.DecodeErr != nil && (h.S == KindControl || h.S == KindAgent):
		// Only fwd and scan handlers take a mistyped header (as bad_args).
		_ = st.Close()
		s.fail(protocolBreak("helper-opened stream: %v", h.DecodeErr))
	case !ok:
		_ = st.Close()
		s.fail(protocolBreak("helper opened a %q stream", h.S))
	case !s.helloOK.Load():
		_ = st.Close()
		s.fail(protocolBreak("helper opened a %q stream before hello", h.S))
	case !s.admit():
		_ = st.Close()
		s.fail(protocolBreak("helper passed the stream cap"))
	default:
		handler(s, h, s.counted(st))
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
