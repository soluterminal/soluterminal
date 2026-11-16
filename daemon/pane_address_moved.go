package main

// Agent panes cut off by a daemon port move (ST-230). The Windows background
// service listens on 127.0.0.1:8787 in Windows-only mode and on :38787 in Dual
// mode, where WSL's forward answers :8787 instead. An agent's SoluTerminal MCP
// server and hook address is fixed when it starts (per-process --mcp-config and
// --settings, or ~/.claude.json), so after the Backend setting moves the port,
// a pane started earlier calls the old address: refused, or answered by the
// WSL daemon, which does not know the pane. Nothing can hand a running process
// a new address, so the daemon marks such a pane and the app offers a restart,
// which resumes the conversation on the current address (the same chip as the
// team-broker case, team_broker_reconnect.go).
//
// The daemon does not record every pane's address at launch. It keeps the
// port it last served (<data>/listen-port), and, for panes a move cuts off,
// the port they were started on. When a boot finds that port moved between
// the two canonical ports, every running agent pane from before the boot was
// started on the old one: an update or restart keeps the Startup mode, so the
// port moves between the canonical ports only with a Startup mode change
// (Dual with WSL down also runs this daemon on 8787; its 38787 listener then
// serves team-broker routes only, so the marks are still right). Those panes are kept, with that old port,
// in <data>/address-moved-panes.json until each is respawned or closed, and
// show the notice only while the daemon is on another port (switching back
// to the old mode reconnects them). Only the primary daemon on a canonical
// port reads or writes these files.

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	listenPortFile        = "listen-port"
	addressMovedPanesFile = "address-moved-panes.json"
)

type paneAddressMovedStore struct {
	mu    sync.Mutex
	path  string
	own   string            // the port this daemon serves
	panes map[string]string // pane id -> the port it was started on
}

var paneAddressMoved atomic.Pointer[paneAddressMovedStore]

func loadPaneAddressMovedStore(dataDir string) *paneAddressMovedStore {
	s := &paneAddressMovedStore{path: filepath.Join(dataDir, addressMovedPanesFile), panes: map[string]string{}}
	raw, err := os.ReadFile(s.path)
	if err != nil || len(raw) > 1<<20 {
		return s
	}
	var saved map[string]string
	if json.Unmarshal(raw, &saved) == nil {
		for id, port := range saved {
			if id != "" && canonicalDaemonPort(port) {
				s.panes[id] = port
			}
		}
	}
	return s
}

func (s *paneAddressMovedStore) persistLocked() {
	if len(s.panes) == 0 {
		_ = os.Remove(s.path)
		return
	}
	raw, err := json.Marshal(s.panes)
	if err != nil {
		return
	}
	_ = writeFileAtomicSynced(s.path, raw, 0o600) // best effort: a lost record only loses the notice
}

// lost: the pane was started on a port this daemon no longer serves.
func (s *paneAddressMovedStore) lost(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	port, ok := s.panes[id]
	return ok && port != s.own
}

func (s *paneAddressMovedStore) clear(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.panes[id]; ok {
		delete(s.panes, id)
		s.persistLocked()
	}
}

// reconcile keeps only marks of running panes and adds the given ones, which
// were started on port previous. A pane marked by an earlier move keeps the
// port it was started on.
func (s *paneAddressMovedStore) reconcile(running map[string]bool, add []string, previous string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id := range s.panes {
		if !running[id] {
			delete(s.panes, id)
			changed = true
		}
	}
	for _, id := range add {
		if _, ok := s.panes[id]; !ok {
			s.panes[id] = previous
			changed = true
		}
	}
	if changed {
		s.persistLocked()
	}
}

// daemonPortMoved reports a move between the two canonical ports.
func daemonPortMoved(previous, current string) bool {
	return previous != "" && current != "" && previous != current &&
		canonicalDaemonPort(previous) && canonicalDaemonPort(current)
}

// paneRunsAgentCmd: the pane's own command is an agent that SoluTerminal
// gives its MCP and hook address at launch. A plain shell is left out; a
// team PowerShell terminal has its own notice. Muse is left out too: it has
// no MCP client or hook surface (codex_mcp_register.go), so a port move does
// not cut it off.
func paneRunsAgentCmd(snap PaneSnapshot) bool {
	return cmdRunsClaude(snap.Cmd) || cmdStartsCodex(snap.Cmd) ||
		managedHarnessBadgeProvider(snap.Cmd, snap.HarnessProvider) != ""
}

// paneAddressLost is read by the pane list.
func paneAddressLost(id string) bool {
	s := paneAddressMoved.Load()
	return s != nil && s.lost(id)
}

// respawnedBeforeWire holds panes respawned during boot (auto-resume, orphan
// respawn) before wirePaneAddressMoved publishes the marks: they already run
// with the current address.
var respawnedBeforeWire sync.Map

// paneAddressRespawned clears a pane's mark: its new process got the current
// address.
func paneAddressRespawned(id string) {
	if s := paneAddressMoved.Load(); s != nil {
		s.clear(id)
		return
	}
	respawnedBeforeWire.Store(id, true)
}

// wirePaneAddressMoved runs at boot, after running panes are reattached.
func wirePaneAddressMoved(b *bootEnv) {
	if b.dataDir == "" {
		return
	}
	_, own, err := net.SplitHostPort(b.addr)
	if err != nil || !canonicalDaemonPort(own) {
		return // a side daemon never moves with the Backend setting
	}
	portPath := filepath.Join(b.dataDir, listenPortFile)
	previous := ""
	if raw, err := os.ReadFile(portPath); err == nil && len(raw) < 16 {
		previous = strings.TrimSpace(string(raw))
	}
	running := map[string]bool{}
	var moved []string
	movedNow := daemonPortMoved(previous, own)
	for _, p := range b.mgr.List() {
		snap := p.Snapshot()
		if snap.Status != PaneRunning {
			continue
		}
		running[snap.ID] = true
		if _, fresh := respawnedBeforeWire.Load(snap.ID); fresh {
			continue
		}
		if movedNow && paneRunsAgentCmd(snap) {
			moved = append(moved, snap.ID)
		}
	}
	store := loadPaneAddressMovedStore(b.dataDir)
	store.own = own
	store.reconcile(running, moved, previous)
	paneAddressMoved.Store(store)
	respawnedBeforeWire.Clear()
	// Only after the marks are saved, so a crash in between repeats the
	// comparison on the next boot instead of losing the notice.
	if previous != own {
		_ = writeFileAtomicSynced(portPath, []byte(own+"\n"), 0o600)
	}
}

func init() { registerBootStep(bootServer, wirePaneAddressMoved) }
