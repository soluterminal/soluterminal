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

// ErrShutdown is Serve's result after the host asked the helper to exit.
var ErrShutdown = errors.New("helper shutdown requested")

// Helper does the work behind each control op. The session decodes and
// checks every request first (identifiers, limits, the GLM refusal), runs
// hello before anything else, enforces drain, and answers shutdown itself.
// Return a *Error to choose the reply's code; any other error is internal.
type Helper interface {
	Hello(ctx context.Context, a HelloArgs) (HelloResult, error)
	Spawn(ctx context.Context, a SpawnArgs) (SpawnResult, error)
	List(ctx context.Context, a ListArgs) (ListResult, error)
	Cwd(ctx context.Context, agentID string) (string, error)
	Kill(ctx context.Context, a KillArgs) error
	Reap(ctx context.Context, agentID string) error
	// Drain runs after every in-flight op has finished and every agent
	// stream's ServeAgent has returned.
	Drain(ctx context.Context) error
	// ServeAgent splices one agent stream to that agent's socket. It must
	// close the socket connection and return once c fails or is closed.
	ServeAgent(agentID string, c net.Conn)
}

// HelperStreamHandler serves a reserved host-opened stream kind. It owns c.
type HelperStreamHandler func(s *HelperSession, h StreamHeader, c net.Conn)

// HelperOptions configures the helper end.
type HelperOptions struct {
	// Handlers serves reserved host-opened kinds. Empty in v1; M4 registers
	// KindScan here once its own ruling is in.
	Handlers map[string]HelperStreamHandler
	// OpenKinds lists the helper-opened kinds Open may use. Empty in v1; M3
	// adds KindFwd once its ruling is in.
	OpenKinds []string
	// LogOutput receives yamux's own log lines; nil discards them.
	LogOutput io.Writer
	// Logf logs session events (the helper sends them to stderr); nil
	// discards them.
	Logf func(format string, args ...any)
}

// HelperSession is the helper's end of the session.
type HelperSession struct {
	h    Helper
	opts HelperOptions
	mux  *yamux.Session
	w    *lineWriter

	helloOK  atomic.Bool    // set once hello succeeded
	draining bool           // control loop only
	inflight sync.WaitGroup // everything running outside the control loop
	opsWG    sync.WaitGroup // ops a drain must wait for (not the drain itself)

	streamsMu    sync.Mutex
	streamsDrain bool
	agentStreams map[net.Conn]struct{}
	agentWG      sync.WaitGroup

	logMu  sync.Mutex
	logged map[string]bool
}

// NewHelperSession starts the server end of the session over conn (stdin for
// reads, stdout for writes). Nothing else may write to stdout.
func NewHelperSession(conn io.ReadWriteCloser, h Helper, opts HelperOptions) (*HelperSession, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	mux, err := yamux.Server(conn, YamuxConfig(opts.LogOutput))
	if err != nil {
		return nil, err
	}
	return &HelperSession{
		h:            h,
		opts:         opts,
		mux:          mux,
		agentStreams: map[net.Conn]struct{}{},
		logged:       map[string]bool{},
	}, nil
}

// Close ends the session.
func (s *HelperSession) Close() error { return s.mux.Close() }

// Serve runs the session until the host goes away (nil), asks for shutdown
// (ErrShutdown), the runtime dir is refused at hello (the *Error), or the
// protocol breaks (*BreakError). The helper process exits after it returns;
// agents keep running.
func (s *HelperSession) Serve() error {
	defer s.mux.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Ops (and a drain waiting on them) see the host vanish at once, not
	// only after the control loop returns.
	go func() {
		select {
		case <-s.mux.CloseChan():
			cancel()
		case <-ctx.Done():
		}
	}()

	ctl, err := s.mux.Accept()
	if err != nil {
		return nil // host gone before it said anything
	}
	_ = ctl.SetReadDeadline(time.Now().Add(HeaderTimeout))
	h, err := ReadHeader(ctl)
	_ = ctl.SetReadDeadline(time.Time{})
	if err != nil {
		return protocolBreak("first stream: %v", err)
	}
	if h.S != KindControl {
		return protocolBreak("first stream is %q, not control", h.S)
	}
	if h.DecodeErr != nil {
		return protocolBreak("first stream: %v", h.DecodeErr)
	}
	s.w = newLineWriter(ctl)
	go s.acceptLoop()
	err = s.controlLoop(ctx, ctl)
	cancel()
	// Ops see the cancellation; don't let one that ignores it keep a
	// helper that was told to go from exiting. Agents run on regardless.
	waitBounded(&s.inflight, shutdownWait)
	return err
}

// shutdownWait bounds how long Serve waits for running ops at exit.
const shutdownWait = 5 * time.Second

func waitBounded(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

func (s *HelperSession) controlLoop(ctx context.Context, ctl net.Conn) error {
	lr := newLineReader(ctl)
	for {
		line, err := lr.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, ErrLineTooLong) || errors.Is(err, io.ErrUnexpectedEOF) {
				return protocolBreak("control: %v", err)
			}
			return nil // session closed
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return protocolBreak("control: bad request: %v", err)
		}
		switch {
		case req.Op == OpShutdown:
			s.reply(req.ID, Empty{}, nil)
			return ErrShutdown
		case !isKnownOp(req.Op):
			s.reply(req.ID, nil, Errorf(CodeUnsupported, "unknown op %q", req.Op))
		case !s.helloOK.Load():
			if req.Op != OpHello {
				s.reply(req.ID, nil, Errorf(CodeBadArgs, "hello must be first"))
				continue
			}
			res, err := s.doHello(ctx, req.Args)
			s.reply(req.ID, res, err)
			if ErrorCode(err) == CodeUnsafeRuntimeDir {
				return err
			}
		case req.Op == OpHello:
			s.reply(req.ID, nil, Errorf(CodeBadArgs, "hello already done"))
		case s.draining:
			s.reply(req.ID, nil, Errorf(CodeBusy, "draining"))
		case req.Op == OpDrain:
			// From here every op but shutdown is busy. The barrier runs off
			// the control loop, so the host's fallback shutdown (after its
			// drain timeout) is still read while drain waits.
			s.draining = true
			s.streamsMu.Lock()
			s.streamsDrain = true // refuse new agent streams at once
			s.streamsMu.Unlock()
			s.inflight.Add(1)
			go func(id uint64) {
				defer s.inflight.Done()
				s.reply(id, Empty{}, s.drain(ctx))
			}(req.ID)
		default:
			s.inflight.Add(1)
			s.opsWG.Add(1)
			go func(req Request) {
				defer s.inflight.Done()
				defer s.opsWG.Done()
				res, err := s.dispatch(ctx, req.Op, req.Args)
				s.reply(req.ID, res, err)
			}(req)
		}
	}
}

func isKnownOp(op string) bool {
	switch op {
	case OpHello, OpSpawn, OpList, OpCwd, OpKill, OpReap, OpDrain, OpShutdown:
		return true
	}
	return false
}

func (s *HelperSession) doHello(ctx context.Context, raw json.RawMessage) (any, error) {
	var a HelloArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if e := validateHello(&a); e != nil {
		return nil, e
	}
	res, err := s.h.Hello(ctx, a)
	if err != nil {
		return nil, err
	}
	s.helloOK.Store(true)
	return res, nil
}

func (s *HelperSession) dispatch(ctx context.Context, op string, raw json.RawMessage) (any, error) {
	switch op {
	case OpSpawn:
		var a SpawnArgs
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if e := validateSpawn(&a); e != nil {
			return nil, e
		}
		return s.h.Spawn(ctx, a)
	case OpList:
		var a ListArgs
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if e := validateList(&a); e != nil {
			return nil, e
		}
		return s.h.List(ctx, a)
	case OpCwd:
		var a AgentRef
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if e := validateRef(op, &a); e != nil {
			return nil, e
		}
		cwd, err := s.h.Cwd(ctx, a.AgentID)
		return CwdResult{Cwd: cwd}, err
	case OpKill:
		var a KillArgs
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if e := validateKill(&a); e != nil {
			return nil, e
		}
		return Empty{}, s.h.Kill(ctx, a)
	case OpReap:
		var a AgentRef
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if e := validateRef(op, &a); e != nil {
			return nil, e
		}
		return Empty{}, s.h.Reap(ctx, a.AgentID)
	}
	return nil, Errorf(CodeUnsupported, "unknown op %q", op)
}

// drain is the barrier: wait for in-flight ops, refuse new agent streams,
// close the open ones and wait for their splices to end, then let the
// Helper finish.
func (s *HelperSession) drain(ctx context.Context) error {
	s.opsWG.Wait()
	s.streamsMu.Lock()
	for c := range s.agentStreams {
		_ = c.Close()
		// A local close does not wake a blocked yamux Read; a past deadline does.
		_ = c.SetDeadline(time.Unix(1, 0))
	}
	s.streamsMu.Unlock()
	s.agentWG.Wait()
	return s.h.Drain(ctx)
}

func (s *HelperSession) reply(id uint64, res any, err error) {
	r := Reply{ID: id, OK: err == nil}
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = Errorf(CodeInternal, "%v", err)
		}
		r.Error = e
	} else {
		raw, merr := json.Marshal(res)
		if merr != nil {
			r.OK, r.Error = false, Errorf(CodeInternal, "result: %v", merr)
		} else {
			r.Result = raw
		}
	}
	if werr := s.w.write(r); errors.Is(werr, ErrLineTooLong) {
		_ = s.w.write(Reply{ID: id, Error: Errorf(CodeInternal, "reply over 64 KiB")})
	}
}

func (s *HelperSession) acceptLoop() {
	for {
		st, err := s.mux.Accept()
		if err != nil {
			return
		}
		go s.serveStream(st)
	}
}

func (s *HelperSession) serveStream(st net.Conn) {
	_ = st.SetReadDeadline(time.Now().Add(HeaderTimeout))
	h, err := ReadHeader(st)
	_ = st.SetReadDeadline(time.Time{})
	if err != nil {
		_ = st.Close()
		s.logOnce("header", "stream refused: %v", err)
		return
	}
	switch h.S {
	case KindAgent:
		if h.DecodeErr != nil {
			_ = st.Close()
			s.logOnce("header", "stream refused: %v", h.DecodeErr)
			return
		}
		s.serveAgent(h, st)
	case KindControl:
		_ = st.Close()
		s.logOnce("control", "a second control stream was refused")
	default:
		handler, ok := s.opts.Handlers[h.S]
		if !ok || !s.helloPassed() {
			_ = st.Close()
			s.logOnce("kind:"+h.S, "stream kind %q refused", h.S)
			return
		}
		handler(s, h, st)
	}
}

func (s *HelperSession) serveAgent(h StreamHeader, st net.Conn) {
	if !ValidAgentID(h.Agent) {
		_ = st.Close()
		s.logOnce("agent-id", "agent stream with a bad id refused")
		return
	}
	s.streamsMu.Lock()
	if s.streamsDrain || !s.helloOK.Load() || s.mux.NumStreams() > MaxStreams {
		s.streamsMu.Unlock()
		_ = st.Close()
		return
	}
	s.agentStreams[st] = struct{}{}
	s.agentWG.Add(1)
	s.streamsMu.Unlock()
	defer func() {
		s.streamsMu.Lock()
		delete(s.agentStreams, st)
		s.streamsMu.Unlock()
		_ = st.Close()
		s.agentWG.Done()
	}()
	s.h.ServeAgent(h.Agent, st)
}

func (s *HelperSession) helloPassed() bool { return s.helloOK.Load() }

// Open opens a helper-initiated stream of a kind listed in OpenKinds.
func (s *HelperSession) Open(h StreamHeader) (net.Conn, error) {
	if !contains(s.opts.OpenKinds, h.S) || h.S == KindControl || h.S == KindAgent {
		return nil, Errorf(CodeUnsupported, "stream kind %q is not enabled", h.S)
	}
	if !s.helloPassed() {
		return nil, errNoHello
	}
	if s.mux.NumStreams() >= MaxStreams {
		return nil, ErrTooManyPanes
	}
	st, err := s.mux.Open()
	if err != nil {
		return nil, err
	}
	if err := WriteHeader(st, h); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

func (s *HelperSession) logOnce(key, format string, args ...any) {
	s.logMu.Lock()
	seen := s.logged[key]
	s.logged[key] = true
	s.logMu.Unlock()
	if !seen {
		s.opts.Logf(format, args...)
	}
}
