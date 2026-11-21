package main

import (
	"context"
	"log"
	"net/http"
	"path"
	"sync"
	"sync/atomic"

	"soluterminal/helperproto"
)

// helperFwdHosts wires W3's fwd host into each distro's helper supervisor
// (linuxPaneHooks): one helperFwdHost per distro, made on first use. The API
// handler is set once main has built it; until then (and while fwd is off,
// which it is in this build) no fwd stream is served.
type helperFwdHosts struct {
	mgr     *Manager
	handler atomic.Pointer[http.Handler]

	mu    sync.Mutex
	hosts map[string]*helperFwdHost
}

func newHelperFwdHosts(mgr *Manager) *helperFwdHosts {
	return &helperFwdHosts{mgr: mgr, hosts: map[string]*helperFwdHost{}}
}

// setHandler installs the API handler fwd requests are dispatched to.
func (f *helperFwdHosts) setHandler(h http.Handler) { f.handler.Store(&h) }

// hooks is what main passes to linuxPaneBackendFromEnv.
func (f *helperFwdHosts) hooks() linuxPaneHooks {
	return linuxPaneHooks{StreamHandlers: f.streamHandlers, OnHello: f.onHello}
}

func (f *helperFwdHosts) forDistro(distro string) *helperFwdHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := f.hosts[distro]; h != nil {
		return h
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := f.handler.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	h := newHelperFwdHost(distro, api, func(paneID, agentID string) bool {
		return linuxPaneBackedBy(f.mgr, distro, paneID, agentID)
	})
	f.hosts[distro] = h
	return h
}

// streamHandlers is nil while fwd is off: a helper-opened stream stays a
// protocol break (ruling 8.6).
func (f *helperFwdHosts) streamHandlers(distro string) map[string]helperproto.HostStreamHandler {
	return f.forDistro(distro).streamHandlers()
}

// onHello runs inside the supervisor's start, which Close and Handoff wait
// on: it only reads the hello and hands the logging to its own goroutine.
// A hello is a hint (that start can still fail); what is live comes from
// Integration().
func (f *helperFwdHosts) onHello(distro string, hello helperproto.HelloResult, _ helperPlacement) {
	h := f.forDistro(distro)
	go h.observeHello(hello)
}

// linuxPaneBackedBy reports whether paneID is a running Linux pane in distro
// whose CURRENT agent is agentID: a pane respawned under the same id has a new
// agent, and the old one no longer acts for it.
func linuxPaneBackedBy(mgr *Manager, distro, paneID, agentID string) bool {
	if mgr == nil {
		return false
	}
	p, ok := mgr.Get(paneID)
	if !ok || p == nil || p.Linux == nil {
		return false
	}
	return p.Linux.Distro == distro && p.Linux.AgentID == agentID && p.Snapshot().Status == PaneRunning
}

// linuxPaneAgentCmdFor adds the per-pane Claude/Codex config to a Linux
// pane's command when fwd is enabled, the helper reported usable paths, and
// its kernel can attest (6.5+). Otherwise the command is left as it is: no
// config that would only show a broken server.
func linuxPaneAgentCmdFor(enabled bool, in linuxPaneIntegration, inErr error, cmd []string) []string {
	if !enabled || len(cmd) == 0 {
		return cmd
	}
	if b := path.Base(cmd[0]); b != "claude" && b != "codex" {
		return cmd
	}
	if inErr != nil || !helperFwdKernelSupported(in.Kernel) {
		return cmd
	}
	p, ok := linuxPaneAgentPathsFor(in.BinPath, in.RuntimeDir)
	if !ok {
		return cmd
	}
	return linuxPaneAgentCmd(p, cmd)
}

// linuxPaneWithAgentConfig is linuxPaneAgentCmdFor for a pane about to start
// in distro (asks the helper only when it would matter).
// ctx is the pane spawn's own: the helper start Integration may do is the
// one Spawn then reuses, under one budget.
func (m *Manager) linuxPaneWithAgentConfig(ctx context.Context, distro string, cmd []string) []string {
	if !helperFwdEnabled || len(cmd) == 0 || m.linux == nil {
		return cmd
	}
	if b := path.Base(cmd[0]); b != "claude" && b != "codex" {
		return cmd
	}
	in, err := linuxPaneIntegrationRetry(ctx, m.linux, distro)
	if err != nil {
		log.Printf("linux pane: %s: no agent config (%v)", distro, err)
	}
	return linuxPaneAgentCmdFor(true, in, err, cmd)
}

// linuxPaneIntegrationRetry asks once more after an error: the helper's
// session can end between Integration's two steps (a transient "helper
// stopped").
func linuxPaneIntegrationRetry(ctx context.Context, b linuxPaneBackend, distro string) (linuxPaneIntegration, error) {
	in, err := b.Integration(ctx, distro)
	if err == nil || ctx.Err() != nil {
		return in, err
	}
	return b.Integration(ctx, distro)
}
