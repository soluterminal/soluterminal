package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"soluterminal/helperproto"
)

// The Windows host's side of the WSL helper (ST-253 A′, helper-protocol-v1):
// one supervisor per distro places the helper binary, starts
// `wsl.exe -d <distro> --exec <helper> helper --proto 1`, checks hello, and
// restarts it with backoff after a protocol break or an exit. Agents outlive
// the helper; a new session reattaches them through OnSession.

// Restart backoff (helper-protocol-v1, Limits): 1 s doubling to 60 s, reset
// after 10 minutes healthy.
const (
	helperBackoffMin   = time.Second
	helperBackoffMax   = time.Minute
	helperHealthyReset = 10 * time.Minute
	helperDrainTimeout = 5 * time.Second
	helperHelloTimeout = 30 * time.Second
	// helperStartTimeout bounds one whole start: placement (wsl.exe probes
	// and the copy), the process start and hello.
	helperStartTimeout = 2 * time.Minute
)

// helperShutdownWait bounds each wait for a helper's exit.
const helperShutdownWait = 10 * time.Second

// helperBackoff is the restart delay schedule.
type helperBackoff struct{ next time.Duration }

// after returns the delay before the next start, given how long the last
// session stayed healthy.
func (b *helperBackoff) after(healthy time.Duration) time.Duration {
	if b.next == 0 || healthy >= helperHealthyReset {
		b.next = helperBackoffMin
	}
	d := b.next
	b.next *= 2
	if b.next > helperBackoffMax {
		b.next = helperBackoffMax
	}
	return d
}

// helperPlacement is what placing the binary learned from the distro.
type helperPlacement struct {
	BinPath   string // absolute path inside the distro
	UID       int    // `id -u`: the uid hello must report
	Arch      string // `uname -m`
	HasClaude bool   // `command -v claude` (for W3)
	HasCodex  bool   // `command -v codex` (for W3)
}

// helperSupervisorConfig wires a supervisor to its distro. Place and Command
// are the only parts that touch wsl.exe, so tests can run a Linux helper
// directly.
type helperSupervisorConfig struct {
	Distro  string
	Version string // the host's version; hello must report the same
	// Place makes sure the distro holds the right binary (hash-checked
	// against the signed manifest before each start) and probes the user.
	Place func(ctx context.Context) (helperPlacement, error)
	// Command builds the helper's process for a placed binary:
	// wsl.exe -d <distro> --exec <bin> helper --proto 1.
	Command func(p helperPlacement) *exec.Cmd
	// OnSession runs after each hello passes, before any caller gets the
	// session: reattach agents with ListAll there.
	OnSession func(ctx context.Context, s *helperproto.HostSession) error
	Logf      func(format string, args ...any)
	// Sleep waits between restarts; tests shorten it.
	Sleep func(ctx context.Context, d time.Duration) error
	// ExitWait bounds each wait for a helper's exit (helperShutdownWait
	// when zero).
	ExitWait time.Duration
	// StreamHandlers, when set, gives the handlers for helper-opened stream
	// kinds on each start (W3's fwd, M3); nil keeps v1's refusal of every
	// helper-opened stream (ruling 8.6).
	StreamHandlers func() map[string]helperproto.HostStreamHandler
	// OnHello, when set, sees each helper's hello result and placement right
	// after hello passes, before OnSession.
	OnHello func(helperproto.HelloResult, helperPlacement)
}

// helperSupervisor owns one distro's helper.
type helperSupervisor struct {
	cfg    helperSupervisorConfig
	stderr *helperStderrLogger // one per distro: its rate budget outlives restarts
	ctx    context.Context     // ends on Close or Handoff, aborting any start
	cancel context.CancelFunc

	mu         sync.Mutex
	sess       *helperproto.HostSession
	cmd        *exec.Cmd
	exited     chan struct{}
	placement  helperPlacement
	hello      helperproto.HelloResult // the live session's hello
	started    time.Time
	backoff    helperBackoff
	waiting    time.Time     // no start before this (backoff)
	starting   chan struct{} // non-nil while a start runs (outside mu); closed when it ends
	startErr   error         // the last start's error, read after starting closes
	restarting bool          // a restartAfterEnd loop is running
	stale      []helperChild // helpers killed but not yet seen to exit
	closed     bool
}

// helperChild is a helper process and the channel its Wait closes.
type helperChild struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

func newHelperSupervisor(cfg helperSupervisorConfig) *helperSupervisor {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.ExitWait == 0 {
		cfg.ExitWait = helperShutdownWait
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &helperSupervisor{cfg: cfg, stderr: newHelperStderrLogger(cfg.Distro, cfg.Logf), ctx: ctx, cancel: cancel}
}

var errHelperClosed = errors.New("the distro's helper is stopped")

// errHelperStillRunning: an old helper could be neither drained nor shown
// to have exited. No new helper starts, and no handoff succeeds, until it is.
var errHelperStillRunning = errors.New("an old helper did not drain or exit")

// Session returns the live session, starting the helper when there is none.
// The first Linux pane calls it; after a break the supervisor calls it
// itself (restartAfterEnd). Neither a backoff nor a start holds the lock,
// so Close and Handoff can always cancel them.
func (s *helperSupervisor) Session(ctx context.Context) (*helperproto.HostSession, error) {
	var sleptUntil time.Time
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, errHelperClosed
		}
		if s.sess != nil {
			select {
			case <-s.sess.Done():
				s.endLocked(s.sess.Err())
			default:
				sess := s.sess
				s.mu.Unlock()
				return sess, nil
			}
		}
		if ch := s.starting; ch != nil { // another caller is starting it
			s.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// Sleep out the backoff, again if another failed start pushed it
		// later while this caller slept.
		if until := s.waiting; until.After(sleptUntil) && time.Until(until) > 0 {
			s.mu.Unlock()
			if err := s.cfg.Sleep(ctx, time.Until(until)); err != nil {
				return nil, err
			}
			sleptUntil = until
			continue
		}
		if !s.reapStaleLocked() {
			// A helper killed a moment ago is usually just about to be
			// reaped: wait for it, without the lock, before refusing.
			stale := append([]helperChild(nil), s.stale...)
			s.mu.Unlock()
			for _, proc := range stale {
				select {
				case <-proc.exited:
				case <-time.After(s.cfg.ExitWait):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			s.mu.Lock()
			if !s.reapStaleLocked() {
				s.waiting = time.Now().Add(s.backoff.after(0))
				s.mu.Unlock()
				return nil, errHelperStillRunning
			}
			// The lock was dropped: re-check everything (closed, a session
			// another caller started, a start in flight) before starting.
			s.mu.Unlock()
			continue
		}
		done := make(chan struct{})
		s.starting = done
		s.startErr = nil
		s.mu.Unlock()
		go s.runStart(done)
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.mu.Lock()
		if err := s.startErr; err != nil && s.sess == nil {
			s.mu.Unlock()
			return nil, err
		}
		s.mu.Unlock()
		// Loop: return the new session, or the state another caller left.
	}
}

// runStart starts a helper under its own deadline (helperStartTimeout),
// cancelled by Close/Handoff and by nothing a single caller does: a pane
// giving up early must not cancel the start every pane shares.
func (s *helperSupervisor) runStart(done chan struct{}) {
	startCtx, stop := context.WithTimeout(s.ctx, helperStartTimeout)
	proc, sess, p, hello, err := s.start(startCtx)
	stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(done)
	s.starting = nil
	s.startErr = err
	switch {
	case err != nil:
		s.waiting = time.Now().Add(s.backoff.after(0))
	case s.closed:
		_ = sess.Close()
		s.retireLocked(proc)
		s.startErr = errHelperClosed
	default:
		s.sess, s.cmd, s.exited, s.placement, s.hello, s.started = sess, proc.cmd, proc.exited, p, hello, time.Now()
		go s.watch(sess, proc)
	}
}

// start places and starts a helper and checks its hello, without the lock.
// A failed start's process is retired (killed, kept until seen to exit).
func (s *helperSupervisor) start(ctx context.Context) (helperChild, *helperproto.HostSession, helperPlacement, helperproto.HelloResult, error) {
	var proc helperChild
	p, err := s.cfg.Place(ctx)
	if err != nil {
		return proc, nil, p, helperproto.HelloResult{}, fmt.Errorf("place the helper in %s: %w", s.cfg.Distro, err)
	}
	cmd := s.cfg.Command(p)
	// Our own pipes, not cmd.*Pipe: cmd.Wait closes those, which could drop
	// the helper's last stdout bytes (a final reply) and its last stderr
	// lines (why it died). The child's ends are closed here after Start.
	stdin, stdout, stderr, childEnds, err := helperPipes()
	if err != nil {
		return proc, nil, p, helperproto.HelloResult{}, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = childEnds[0], childEnds[1], childEnds[2]
	err = cmd.Start()
	for _, f := range childEnds {
		_ = f.Close()
	}
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return proc, nil, p, helperproto.HelloResult{}, fmt.Errorf("start the helper in %s: %w", s.cfg.Distro, err)
	}
	proc = helperChild{cmd: cmd, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(proc.exited) }()
	go func() { s.stderr.pump(stderr); _ = stderr.Close() }()

	fail := func(err error) (helperChild, *helperproto.HostSession, helperPlacement, helperproto.HelloResult, error) {
		s.mu.Lock()
		s.retireLocked(proc)
		s.mu.Unlock()
		return helperChild{}, nil, p, helperproto.HelloResult{}, err
	}
	hostOpts := helperproto.HostOptions{Logf: s.cfg.Logf}
	if s.cfg.StreamHandlers != nil {
		hostOpts.Handlers = s.cfg.StreamHandlers()
	}
	sess, err := helperproto.NewHostSession(helperStdioConn{r: stdout, w: stdin}, hostOpts)
	if err != nil {
		return fail(err)
	}
	hctx, cancel := context.WithTimeout(ctx, helperHelloTimeout)
	defer cancel()
	hello, err := sess.Hello(hctx, helperproto.HelloArgs{
		Proto: helperproto.Proto, HostVersion: s.cfg.Version, HostPid: os.Getpid(), Distro: s.cfg.Distro,
	}, p.UID)
	if err == nil && s.cfg.OnHello != nil {
		s.cfg.OnHello(hello, p)
	}
	if err == nil && s.cfg.OnSession != nil {
		err = s.cfg.OnSession(hctx, sess)
	}
	if err != nil {
		_ = sess.Close()
		return fail(fmt.Errorf("helper hello in %s: %w", s.cfg.Distro, err))
	}
	return proc, sess, p, hello, nil
}

// retireLocked kills a helper without waiting (mu is held) and keeps it on
// the stale list until its exit is seen. The list blocks new starts and a
// successful handoff.
func (s *helperSupervisor) retireLocked(proc helperChild) {
	if proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	_ = proc.cmd.Process.Kill()
	s.stale = append(s.stale, proc)
}

// reapStaleLocked drops stale helpers whose exit was seen, kills the rest
// again without waiting, and reports whether none is left.
func (s *helperSupervisor) reapStaleLocked() bool {
	kept := s.stale[:0]
	for _, proc := range s.stale {
		select {
		case <-proc.exited:
		default:
			_ = proc.cmd.Process.Kill()
			kept = append(kept, proc)
		}
	}
	s.stale = kept
	return len(kept) == 0
}

// waitStaleLocked gives stale helpers up to ExitWait to exit (Handoff only).
func (s *helperSupervisor) waitStaleLocked() bool {
	for _, proc := range s.stale {
		waitHelperExit(proc.cmd, proc.exited, s.cfg.ExitWait)
	}
	return s.reapStaleLocked()
}

// watch ends a session whose helper exited or broke protocol, then starts
// a new helper after the backoff, so the distro's agents are reattached
// even if no new pane asks for a session.
func (s *helperSupervisor) watch(sess *helperproto.HostSession, proc helperChild) {
	select {
	case <-sess.Done():
	case <-proc.exited:
		s.cfg.Logf("helper[%s]: exited (%v); restarting with backoff", s.cfg.Distro, proc.cmd.ProcessState)
		_ = sess.Close()
	}
	s.mu.Lock()
	if s.sess == sess && !s.closed {
		s.endLocked(sess.Err())
	}
	s.mu.Unlock()
}

// restartAfterEnd keeps trying (each failure adds backoff) until a session
// is up or the supervisor is closed. endLocked starts it, once at a time.
func (s *helperSupervisor) restartAfterEnd() {
	defer func() {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}()
	for s.ctx.Err() == nil {
		if _, err := s.Session(s.ctx); err == nil || errors.Is(err, errHelperClosed) {
			return
		} else if s.ctx.Err() == nil {
			s.cfg.Logf("helper[%s]: restart failed: %v", s.cfg.Distro, err)
		}
	}
}

// endLocked stops the current helper and schedules the next start. Agents
// keep running; the next session reattaches them.
func (s *helperSupervisor) endLocked(cause error) {
	if s.sess == nil {
		return
	}
	if helperproto.IsBreak(cause) {
		s.cfg.Logf("helper[%s]: %v; restarting with backoff", s.cfg.Distro, cause)
	}
	_ = s.sess.Close()
	s.retireLocked(helperChild{cmd: s.cmd, exited: s.exited})
	s.waiting = time.Now().Add(s.backoff.after(time.Since(s.started)))
	s.sess, s.cmd, s.exited = nil, nil, nil
	// Whoever ends the session (watch, or a Session call that saw it end
	// first) starts the restart, so the distro's agents are reattached even
	// if that caller gives up during the backoff.
	if !s.closed && !s.restarting {
		s.restarting = true
		go s.restartAfterEnd()
	}
}

// closeLocked marks the supervisor closed, aborts any start, and waits for
// an in-flight start to finish (it retires its own process on seeing
// closed). Called with mu held; returns with mu held.
func (s *helperSupervisor) closeLocked() {
	s.closed = true
	s.cancel()
	for s.starting != nil {
		ch := s.starting
		s.mu.Unlock()
		<-ch
		s.mu.Lock()
	}
}

// Current returns the live session's placement and hello; false when no
// helper is running for the distro.
func (s *helperSupervisor) Current() (helperPlacement, helperproto.HelloResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == nil {
		return helperPlacement{}, helperproto.HelloResult{}, false
	}
	select {
	case <-s.sess.Done(): // ended; the watcher just hasn't cleared it yet
		return helperPlacement{}, helperproto.HelloResult{}, false
	default:
	}
	return s.placement, s.hello, true
}

// Spawn starts a Linux pane after refusing any env that would carry a host
// secret into the distro (ruling 8.4).
func (s *helperSupervisor) Spawn(ctx context.Context, a helperproto.SpawnArgs) (helperproto.SpawnResult, error) {
	if err := checkHelperSpawnEnv(a.Env); err != nil {
		return helperproto.SpawnResult{}, helperproto.Errorf(helperproto.CodeBadArgs, "%v", err)
	}
	sess, err := s.Session(ctx)
	if err != nil {
		return helperproto.SpawnResult{}, err
	}
	return sess.Spawn(ctx, a)
}

// Handoff is the old host's half of an update (helper-protocol-v1, Update
// handoff; ruling 8.7). nil means no helper of this host can serve the
// distro's agents any more: the live one drained, or it and every stopped
// one were seen to exit. Otherwise errHelperStillRunning, keeping the
// processes so a retry checks again. No helper starts here afterwards.
func (s *helperSupervisor) Handoff(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	if s.sess != nil {
		dctx, cancel := context.WithTimeout(ctx, helperDrainTimeout)
		err := s.sess.Drain(dctx)
		cancel()
		if err == nil {
			// Drained: it serves nothing now, and exits on EOF.
			sess, proc := s.sess, helperChild{cmd: s.cmd, exited: s.exited}
			s.sess, s.cmd, s.exited = nil, nil, nil
			go func() { _ = sess.Close(); waitHelperExit(proc.cmd, proc.exited, s.cfg.ExitWait) }()
			return s.staleErrLocked()
		}
		s.cfg.Logf("helper[%s]: drain failed (%v); shutting it down", s.cfg.Distro, err)
		sctx, cancel := context.WithTimeout(ctx, helperDrainTimeout)
		_ = s.sess.Shutdown(sctx)
		cancel()
		_ = s.sess.Close()
		s.sess = nil
	}
	if s.cmd != nil {
		proc := helperChild{cmd: s.cmd, exited: s.exited}
		s.cmd, s.exited = nil, nil
		if !waitHelperExit(proc.cmd, proc.exited, s.cfg.ExitWait) {
			s.stale = append(s.stale, proc)
		}
	}
	return s.staleErrLocked()
}

func (s *helperSupervisor) staleErrLocked() error {
	if !s.waitStaleLocked() {
		return errHelperStillRunning
	}
	return nil
}

// Close stops the helper (EOF on its stdin); agents keep running.
func (s *helperSupervisor) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	if s.sess != nil {
		_ = s.sess.Close()
		proc := helperChild{cmd: s.cmd, exited: s.exited}
		if !waitHelperExit(proc.cmd, proc.exited, s.cfg.ExitWait) {
			s.stale = append(s.stale, proc)
		}
		s.sess, s.cmd, s.exited = nil, nil, nil
	}
}

// waitHelperExit waits for the helper to exit, killing it after d. It
// reports whether the exit was observed.
func waitHelperExit(cmd *exec.Cmd, exited chan struct{}, d time.Duration) bool {
	if cmd == nil || exited == nil {
		return true
	}
	select {
	case <-exited:
		return true
	case <-time.After(d):
	}
	return killHelperProcess(cmd, exited, d)
}

// killHelperProcess kills the helper and reports whether its exit was seen.
// On Windows the process is wsl.exe: ending it ends the Linux helper too
// (Check 1, 14 Nov), and the helper exits on stdin EOF in any case; an
// observed exit is proof as far as the host can see.
func killHelperProcess(cmd *exec.Cmd, exited chan struct{}, d time.Duration) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	_ = cmd.Process.Kill()
	if exited == nil {
		return false
	}
	select {
	case <-exited:
		return true
	case <-time.After(d):
		return false
	}
}

// helperPipes makes the helper's stdin, stdout and stderr pipes: our ends
// (write stdin, read stdout and stderr) and the child's three ends.
func helperPipes() (stdin, stdout, stderr *os.File, child [3]*os.File, err error) {
	var opened []*os.File
	fail := func(e error) (*os.File, *os.File, *os.File, [3]*os.File, error) {
		for _, f := range opened {
			_ = f.Close()
		}
		return nil, nil, nil, [3]*os.File{}, e
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	opened = append(opened, inR, inW)
	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	opened = append(opened, outR, outW)
	errR, errW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	return inW, outR, errR, [3]*os.File{inR, outW, errW}, nil
}

type helperStdioConn struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (c helperStdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c helperStdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c helperStdioConn) Close() error {
	werr := c.w.Close()
	_ = c.r.Close()
	return werr
}
