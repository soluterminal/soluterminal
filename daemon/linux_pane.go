package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"soluterminal/helperproto"
)

// linuxPaneRef identifies a Linux pane's agent behind its distro's helper
// (ST-253 A′). Pid and StartTicks are the agent's Linux process identity,
// meaningful only inside the distro; the host never signals or inspects a
// local process by them.
type linuxPaneRef struct {
	Distro     string `json:"distro"`
	AgentID    string `json:"agentId"`
	Pid        int    `json:"pid,omitempty"`
	StartTicks uint64 `json:"startTicks,omitempty"`
}

// linuxPaneOpTimeout bounds one helper op made for a pane.
const linuxPaneOpTimeout = 30 * time.Second

// linuxPaneStopTimeout bounds a whole stop (term, wait, kill, reap); a
// var for tests.
var linuxPaneStopTimeout = 15 * time.Second

// linuxPaneBackend is the Windows host's view of its WSL helpers: one
// supervisor per distro behind it (helperBackend).
type linuxPaneBackend interface {
	Spawn(ctx context.Context, distro string, a helperproto.SpawnArgs) (helperproto.SpawnResult, error)
	OpenAgent(ctx context.Context, distro, agentID string) (net.Conn, error)
	Kill(ctx context.Context, distro, agentID, how string) error
	Reap(ctx context.Context, distro, agentID string) error
	Cwd(ctx context.Context, distro, agentID string) (string, error)
	ListAll(ctx context.Context, distro string) ([]helperproto.AgentEntry, error)
	// Integration starts the distro's helper if needed and reports what a
	// pane's agent config needs (W3: hooks and MCP through the helper).
	Integration(ctx context.Context, distro string) (linuxPaneIntegration, error)
}

// linuxPaneIntegration is the live helper's view of its distro: where the
// helper runs from, its runtime dir (where the in-distro hook and MCP
// sockets live), the kernel, and whether claude and codex are installed.
type linuxPaneIntegration struct {
	BinPath    string
	RuntimeDir string
	Kernel     string
	HasClaude  bool
	HasCodex   bool
}

// helperBackend holds one supervisor per distro, made on first use.
type helperBackend struct {
	newSupervisor func(distro string) *helperSupervisor

	mu   sync.Mutex
	sups map[string]*helperSupervisor
}

func newHelperBackend(newSupervisor func(distro string) *helperSupervisor) *helperBackend {
	return &helperBackend{newSupervisor: newSupervisor, sups: map[string]*helperSupervisor{}}
}

// supervisor returns the distro's supervisor; a name wsl.exe wouldn't
// register never gets one (nothing else reaches `wsl.exe -d`).
func (b *helperBackend) supervisor(distro string) (*helperSupervisor, error) {
	if !validWSLDistro.MatchString(distro) {
		return nil, fmt.Errorf("not a WSL distro name: %q", distro)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sups[distro]
	if s == nil {
		s = b.newSupervisor(distro)
		b.sups[distro] = s
	}
	return s, nil
}

func (b *helperBackend) session(ctx context.Context, distro string) (*helperproto.HostSession, error) {
	s, err := b.supervisor(distro)
	if err != nil {
		return nil, err
	}
	return s.Session(ctx)
}

func (b *helperBackend) Spawn(ctx context.Context, distro string, a helperproto.SpawnArgs) (helperproto.SpawnResult, error) {
	s, err := b.supervisor(distro)
	if err != nil {
		return helperproto.SpawnResult{}, err
	}
	return s.Spawn(ctx, a)
}

func (b *helperBackend) OpenAgent(ctx context.Context, distro, agentID string) (net.Conn, error) {
	s, err := b.session(ctx, distro)
	if err != nil {
		return nil, err
	}
	return s.OpenAgent(agentID)
}

func (b *helperBackend) Kill(ctx context.Context, distro, agentID, how string) error {
	s, err := b.session(ctx, distro)
	if err != nil {
		return err
	}
	return s.Kill(ctx, agentID, how)
}

func (b *helperBackend) Reap(ctx context.Context, distro, agentID string) error {
	s, err := b.session(ctx, distro)
	if err != nil {
		return err
	}
	return s.Reap(ctx, agentID)
}

func (b *helperBackend) Cwd(ctx context.Context, distro, agentID string) (string, error) {
	s, err := b.session(ctx, distro)
	if err != nil {
		return "", err
	}
	return s.Cwd(ctx, agentID)
}

func (b *helperBackend) ListAll(ctx context.Context, distro string) ([]helperproto.AgentEntry, error) {
	s, err := b.session(ctx, distro)
	if err != nil {
		return nil, err
	}
	return s.ListAll(ctx)
}

func (b *helperBackend) Integration(ctx context.Context, distro string) (linuxPaneIntegration, error) {
	s, err := b.supervisor(distro)
	if err != nil {
		return linuxPaneIntegration{}, err
	}
	if _, err := s.Session(ctx); err != nil {
		return linuxPaneIntegration{}, err
	}
	p, hello, ok := s.Current()
	if !ok {
		return linuxPaneIntegration{}, errors.New("the distro's helper stopped")
	}
	return linuxPaneIntegration{
		BinPath: p.BinPath, RuntimeDir: hello.RuntimeDir, Kernel: hello.Kernel,
		HasClaude: p.HasClaude, HasCodex: p.HasCodex,
	}, nil
}

// Handoff hands every distro's helper over for an update (ruling 8.7).
func (b *helperBackend) Handoff(ctx context.Context) error {
	b.mu.Lock()
	sups := make([]*helperSupervisor, 0, len(b.sups))
	for _, s := range b.sups {
		sups = append(sups, s)
	}
	b.mu.Unlock()
	var errs []error
	for _, s := range sups {
		if err := s.Handoff(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.cfg.Distro, err))
		}
	}
	return errors.Join(errs...)
}

// linuxPaneRefusal names what a Linux pane can't do yet (parity cut c and
// the team-broker cut b), or "" when the request is supported.
func linuxPaneRefusal(opts SpawnOpts) string {
	switch {
	case opts.Kind != "":
		return "only terminal panes can run in a Linux distro yet"
	case opts.Role != "":
		return "pane roles aren't available in Linux panes yet"
	case spawnResumeID(opts) != "" || strings.TrimSpace(opts.ClaudeSessionID+opts.CodexThreadID+opts.MuseSessionID+opts.HarnessSessionID) != "":
		return "resuming a conversation isn't available in Linux panes yet"
	case opts.Provider != "":
		return "model providers aren't available in Linux panes yet"
	case paneRouteActive(opts.Env):
		return "team accounts aren't available in Linux panes yet"
	case strings.TrimSpace(opts.ContextPack) != "":
		return "context packs aren't available in Linux panes yet"
	}
	return ""
}

// linuxPaneShell is the default Linux pane: the user's login shell.
var linuxPaneShell = []string{"/bin/sh", "-c", `exec "${SHELL:-/bin/sh}" -l`}

// linuxPaneArgv runs a command through the user's login shell, so PATH
// set up in their profile (nvm, ~/.local/bin, …) finds it. The command's
// words travel as arguments, never inside the script.
func linuxPaneArgv(cmd []string) []string {
	if len(cmd) == 0 || slices.Equal(cmd, linuxPaneShell) {
		return append([]string(nil), linuxPaneShell...)
	}
	if len(cmd) > 4 && slices.Equal(cmd[:4], linuxPaneLoginWrap) {
		return append([]string(nil), cmd...) // already wrapped (a respawn)
	}
	return append(append([]string(nil), linuxPaneLoginWrap...), cmd...)
}

// linuxPaneLoginWrap prefixes a command to run it through the login shell.
// The inner script is POSIX, so a login shell that isn't (fish, nu, …)
// is replaced by /bin/sh -l for it.
var linuxPaneLoginWrap = []string{"/bin/sh", "-c", `case "${SHELL##*/}" in bash|zsh|sh|dash|ksh|mksh) s="$SHELL" ;; *) s=/bin/sh ;; esac
exec "$s" -lc 'exec "$0" "$@"' "$@"`, "solu-pane"}

// linuxPaneDistro is the distro a persisted pane runs in ("" when local).
func linuxPaneDistro(ref *linuxPaneRef) string {
	if ref == nil {
		return ""
	}
	return ref.Distro
}

// errLinuxAgentGone: the helper answered, and this pane's agent isn't
// among its live agents (or isn't the same process any more).
var errLinuxAgentGone = errors.New("the Linux pane's agent is gone")

// Boot reattach must never hold the daemon up for long: each distro is
// listed once per boot burst (linuxBootListTTL, failures included), each
// listing bounded by linuxBootListTimeout.
const (
	linuxBootListTimeout = 20 * time.Second
	linuxBootListTTL     = 10 * time.Second
)

type linuxListEntry struct {
	sem    chan struct{} // one listing per distro at a time; other distros proceed
	at     time.Time
	agents []helperproto.AgentEntry
	err    error
}

// errLinuxOutOfTime: a reattach's window ended while it waited on the helper.
var errLinuxOutOfTime = errors.New("the reattach window ended while waiting on the helper")

// linuxOpContext bounds one helper op by d and, when set, by the deadline by.
func linuxOpContext(d time.Duration, by time.Time) (context.Context, context.CancelFunc) {
	if !by.IsZero() && time.Until(by) < d {
		return context.WithDeadline(context.Background(), by)
	}
	return context.WithTimeout(context.Background(), d)
}

// listLinuxAgents lists a distro's agents. fresh skips the boot cache
// (resume must decide on what the helper says now).
func (m *Manager) listLinuxAgents(distro string, fresh bool) ([]helperproto.AgentEntry, error) {
	return m.listLinuxAgentsBy(distro, fresh, time.Time{})
}

// listLinuxAgentsBy is listLinuxAgents that gives up at by (zero: no
// deadline beyond the listing's own), queue wait included.
func (m *Manager) listLinuxAgentsBy(distro string, fresh bool, by time.Time) ([]helperproto.AgentEntry, error) {
	m.linuxListsMu.Lock()
	if m.linuxLists == nil {
		m.linuxLists = map[string]*linuxListEntry{}
	}
	e := m.linuxLists[distro]
	if e == nil {
		e = &linuxListEntry{sem: make(chan struct{}, 1)}
		m.linuxLists[distro] = e
	}
	m.linuxListsMu.Unlock()

	if by.IsZero() {
		e.sem <- struct{}{}
	} else {
		wait := time.NewTimer(time.Until(by))
		select {
		case e.sem <- struct{}{}:
			wait.Stop()
		case <-wait.C:
			return nil, errLinuxOutOfTime
		}
	}
	defer func() { <-e.sem }()
	if !fresh && !e.at.IsZero() && time.Since(e.at) < linuxBootListTTL {
		return e.agents, e.err
	}
	ctx, cancel := linuxOpContext(linuxBootListTimeout, by)
	defer cancel()
	e.agents, e.err = m.linux.ListAll(ctx, distro)
	e.at = time.Now()
	return e.agents, e.err
}

// attachLinuxConn opens a persisted Linux pane's stream when the helper
// lists its agent alive, for this pane id and the same process identity.
func (m *Manager) attachLinuxConn(info *PersistedPane, agents []helperproto.AgentEntry, by time.Time) (net.Conn, error) {
	ref := *info.Linux
	for _, a := range agents {
		if a.AgentID != ref.AgentID {
			continue
		}
		if !a.Alive || a.PaneID != info.ID || (ref.StartTicks != 0 && a.StartTicks != ref.StartTicks) {
			return nil, errLinuxAgentGone
		}
		ctx, cancel := linuxOpContext(linuxPaneOpTimeout, by)
		defer cancel()
		return m.linux.OpenAgent(ctx, ref.Distro, ref.AgentID)
	}
	return nil, errLinuxAgentGone
}

// reattachLinuxIfAlive decides a suspended Linux tile's resume on what the
// helper says now: a live agent is reattached, never replaced (pane, nil);
// a helper that can't answer replaces nothing (nil, err); only an agent the
// helper says is gone may be replaced (nil, nil). suspended, when set, is
// the tile's old object, whose viewers move to the live pane.
func (m *Manager) reattachLinuxIfAlive(info PersistedPane, suspended *Pane) (*Pane, error) {
	return m.reattachLinuxIfAliveBy(info, suspended, time.Time{})
}

// reattachLinuxIfAliveBy is reattachLinuxIfAlive that gives up at by.
func (m *Manager) reattachLinuxIfAliveBy(info PersistedPane, suspended *Pane, by time.Time) (*Pane, error) {
	if m.linux == nil || info.Linux == nil || !helperproto.ValidAgentID(info.Linux.AgentID) {
		return nil, nil
	}
	if !validWSLDistro.MatchString(info.Linux.Distro) {
		return nil, fmt.Errorf("not a WSL distro name: %q", info.Linux.Distro)
	}
	agents, err := m.listLinuxAgentsBy(info.Linux.Distro, true, by)
	if err != nil {
		return nil, fmt.Errorf("the Linux pane's helper isn't answering: %w", err)
	}
	if !linuxAgentListedAlive(&info, agents) {
		return nil, nil
	}
	p, _ := m.attachExistingFor(&info, agents, suspended, by)
	if p == nil {
		// Alive, but its stream didn't open: try again later rather than
		// replace a live session.
		return nil, errors.New("couldn't reattach the Linux pane's live agent; not replacing it")
	}
	if suspended != nil && suspended != p {
		suspended.detachSubscribers() // viewers reconnect to the live pane
	}
	return p, nil
}

// linuxReattachTiming bounds the reattach after a helper dies (spec: agents
// keep running and are reattached with list). Manager.linuxReattach
// overrides it in tests; zero fields take these defaults.
type linuxReattachTiming struct{ window, first, max time.Duration }

func (m *Manager) linuxReattachTimes() linuxReattachTiming {
	t := m.linuxReattach
	if t.window <= 0 {
		t.window = 2 * time.Minute
	}
	if t.first <= 0 {
		t.first = 500 * time.Millisecond
	}
	if t.max <= 0 {
		t.max = 10 * time.Second
	}
	return t
}

const (
	linuxReconnectingMsg = "Reconnecting to the Linux pane's helper…"
	linuxReconnectFailed = "The Linux pane's helper didn't come back. Retry restore to try again; a program that is still running is reattached, never replaced."
)

// linuxStreamLost marks a Linux pane whose stream ended without the agent's
// exit, unless the person is closing it, as reconnecting. It runs before the
// exit bookkeeping, so the tile is persisted with its recovery note.
func (m *Manager) linuxStreamLost(p *Pane) bool {
	if m == nil || m.linux == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Linux == nil || p.userClosing || !helperproto.ValidAgentID(p.Linux.AgentID) {
		return false
	}
	if p.linuxLostAt.IsZero() || p.gotFrame.Load() {
		p.linuxLostAt = time.Now() // a stream that worked: a fresh window
	}
	p.RecoveryError = linuxReconnectingMsg
	return true
}

// reattachLinuxAfterLoss reattaches a Linux pane after its helper died: only
// to the same agent, listed alive with the same pane id and start ticks
// (reattachLinuxIfAlive). An agent the helper says is gone leaves an
// ordinary exited pane; a helper that doesn't come back within the window
// leaves a tile the person can Resume. It stops as soon as the tile is
// closed or resumed by someone else.
func (m *Manager) reattachLinuxAfterLoss(old *Pane) {
	info := persistedFromSnapshot(old.Snapshot())
	times := m.linuxReattachTimes()
	old.mu.Lock()
	deadline := old.linuxLostAt.Add(times.window)
	old.mu.Unlock()
	wait := times.first
	note := func(msg string) {
		old.mu.Lock()
		if old.RecoveryError == linuxReconnectingMsg {
			old.RecoveryError = msg
		}
		old.mu.Unlock()
		if m.persistLinuxRecovery != nil {
			m.persistLinuxRecovery(old) // the settled tile
		}
	}
	for {
		time.Sleep(wait)
		if wait *= 2; wait > times.max {
			wait = times.max
		}
		if cur, ok := m.Get(old.ID); !ok || cur != old {
			return // closed, or reattached or replaced by a Resume
		}
		if !time.Now().Before(deadline) {
			log.Printf("linux pane %s: %s's helper didn't come back in time; Resume can retry", old.ID, info.Linux.Distro)
			note(linuxReconnectFailed)
			return
		}
		if old.closing.Load() || !old.tryAcquireRespawn() {
			continue // a close or a person's Resume is on it; see how it ends
		}
		p, err := m.reattachLinuxIfAliveBy(info, old, deadline)
		old.releaseRespawn()
		switch {
		case p != nil:
			log.Printf("linux pane %s: reattached to its agent in %s after the helper restarted", old.ID, info.Linux.Distro)
			if m.persistLinuxRecovery != nil {
				m.persistLinuxRecovery(p)
			}
			return
		case err == nil:
			log.Printf("linux pane %s: its agent in %s is gone; the pane stays exited", old.ID, info.Linux.Distro)
			note("")
			return
		case errors.Is(err, errHelperClosed):
			log.Printf("linux pane %s: %s's helper is stopped: %v", old.ID, info.Linux.Distro, err)
			note(linuxReconnectFailed)
			return
		}
	}
}

// linuxAgentListedAlive reports whether the listing shows this pane's agent
// alive with the same identity.
func linuxAgentListedAlive(info *PersistedPane, agents []helperproto.AgentEntry) bool {
	ref := *info.Linux
	for _, a := range agents {
		if a.AgentID == ref.AgentID {
			return a.Alive && a.PaneID == info.ID && (ref.StartTicks == 0 || a.StartTicks == ref.StartTicks)
		}
	}
	return false
}

// validWSLDistro accepts the distro names wsl.exe registers; nothing else
// reaches `wsl.exe -d`.
var validWSLDistro = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// linuxPaneEnv is what a Linux pane's agent adds to its environment: the
// caller's KEY=VALUE entries first, then the daemon's own values, which win
// (as on local panes): a request can't set its own SOLU_PANE_ID. The
// supervisor refuses host secrets (ruling 8.4).
func linuxPaneEnv(id string, extra []string) map[string]string {
	env := map[string]string{}
	for _, kv := range extra {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	env["SOLU_PANE_ID"] = id
	env["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"] = "1"
	return env
}

// spawnLinuxPane starts a pane in a WSL distro through its helper. The pane
// gets no Windows process identity: Pid 0, no socket path, no scope.
func (m *Manager) spawnLinuxPane(opts SpawnOpts) (*Pane, error) {
	if m.linux == nil {
		return nil, &SpawnError{msg: "Linux panes aren't switched on on this machine"}
	}
	if !validWSLDistro.MatchString(opts.Distro) {
		return nil, &SpawnError{msg: "not a WSL distro name: " + opts.Distro}
	}
	if why := linuxPaneRefusal(opts); why != "" {
		return nil, &SpawnError{msg: why}
	}
	// The repo's .solu-no-autoanswer lock lives in the distro, where this
	// host can't read it: fail closed. No automatic answers in a Linux
	// pane unless a person unlocks it.
	opts.AutoMode, opts.NeverAutoAnswer = "", true
	if opts.Cols == 0 {
		opts.Cols = defaultCols
	}
	if opts.Rows == 0 {
		opts.Rows = defaultRows
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperStartTimeout+linuxPaneOpTimeout)
	defer cancel()
	argv := linuxPaneArgv(m.linuxPaneWithAgentConfig(ctx, opts.Distro, opts.Cmd))
	if len(opts.Cmd) == 0 {
		opts.Cmd = argv
	}
	if opts.Title == "" {
		opts.Title = "Linux (" + opts.Distro + ")"
	}
	id := strings.TrimSpace(opts.ID)
	if id == "" {
		var err error
		if id, err = randomID(); err != nil {
			return nil, err
		}
	}
	m.mu.RLock()
	existing := m.panes[id]
	m.mu.RUnlock()
	if existing != nil && existing != opts.ReplaceSuspended {
		return nil, fmt.Errorf("pane id already exists: %s", id)
	}

	res, err := m.linux.Spawn(ctx, opts.Distro, helperproto.SpawnArgs{
		PaneID: id, Argv: argv, Cwd: opts.Cwd, Env: linuxPaneEnv(id, opts.Env),
		Cols: int(opts.Cols), Rows: int(opts.Rows),
	})
	if err != nil {
		return nil, &SpawnError{msg: "Linux pane in " + opts.Distro + ": " + err.Error()}
	}
	ref := linuxPaneRef{Distro: opts.Distro, AgentID: res.AgentID, Pid: res.Pid, StartTicks: res.StartTicks}
	stop := m.linuxPaneStopper(ref)
	conn, err := m.linux.OpenAgent(ctx, opts.Distro, res.AgentID)
	if err != nil {
		_ = stop()
		return nil, fmt.Errorf("connect to the Linux pane's agent: %w", err)
	}
	now := time.Now()
	createdAt := opts.CreatedAt
	if createdAt.IsZero() {
		createdAt = now.UTC()
	}
	paneAddressRespawned(id)
	p := m.newSpawnedPane(opts, id, createdAt, now, conn, nil, "")
	p.Linux = &ref
	p.linuxStop = stop
	if err := m.publishSpawnedPane(p, opts, "", func() {
		_ = conn.Close()
		_ = stop()
	}); err != nil {
		return nil, err
	}
	return p, nil
}

// linuxPaneStopper force-stops a Linux pane's agent through its helper:
// SIGTERM, a short wait, then SIGKILL (the helper checks the agent's
// identity and signals through a pidfd), and reaps its files.
func (m *Manager) linuxPaneStopper(ref linuxPaneRef) func() error {
	return func() error {
		if !helperproto.ValidAgentID(ref.AgentID) {
			return nil // never spawned (an imported pane): nothing to stop
		}
		// One deadline for the whole stop, so a person's close answers (409
		// and its token) well inside the app's 30 s request limit even when
		// WSL hangs: kill wait 3 s + this 15 s + live cwd 2 s.
		ctx, cancel := context.WithTimeout(context.Background(), linuxPaneStopTimeout)
		defer cancel()
		pause := func(d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := m.linux.Kill(ctx, ref.Distro, ref.AgentID, helperproto.KillTerm)
		if helperproto.ErrorCode(err) == helperproto.CodeNotFound {
			_ = m.linux.Reap(ctx, ref.Distro, ref.AgentID)
			return nil
		}
		// Give SIGTERM up to 2 s, returning as soon as the agent is gone.
		for i := 0; i < 10; i++ {
			err := m.linux.Reap(ctx, ref.Distro, ref.AgentID)
			if err == nil || helperproto.ErrorCode(err) == helperproto.CodeNotFound {
				return nil
			}
			if err := pause(200 * time.Millisecond); err != nil {
				return err
			}
		}
		if err := m.linux.Kill(ctx, ref.Distro, ref.AgentID, helperproto.KillKill); err != nil && helperproto.ErrorCode(err) != helperproto.CodeNotFound {
			return err
		}
		// A SIGKILLed agent can still be dying: reap answers busy until its
		// identity no longer holds.
		for i := 0; i < 10; i++ {
			err := m.linux.Reap(ctx, ref.Distro, ref.AgentID)
			switch {
			case err == nil, helperproto.ErrorCode(err) == helperproto.CodeNotFound:
				return nil
			case helperproto.ErrorCode(err) != helperproto.CodeBusy:
				return err // the helper didn't answer: not confirmed stopped
			}
			if err := pause(300 * time.Millisecond); err != nil {
				return err
			}
		}
		return errors.New("the agent is still running after SIGKILL")
	}
}

// paneLiveCwd is a pane's live working directory: for a Linux pane, asked
// of its helper (the deepest descendant's cwd in the distro); otherwise
// read from this machine's process table. "" when unknown.
func (m *Manager) paneLiveCwd(p *Pane) string {
	if p.Linux == nil {
		return liveCwdOfPane(p)
	}
	if m.linux == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cwd, err := m.linux.Cwd(ctx, p.Linux.Distro, p.Linux.AgentID)
	if err != nil {
		return ""
	}
	return cwd
}

// linuxConfirmToken is this tile's confirm token, made on first use.
func (p *Pane) linuxConfirmToken() string {
	p.linuxConfirmOnce.Do(func() {
		var b [16]byte
		if _, err := rand.Read(b[:]); err == nil {
			p.linuxConfirm = hex.EncodeToString(b[:])
		}
	})
	return p.linuxConfirm
}
