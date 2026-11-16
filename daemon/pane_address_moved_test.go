package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonPortMoved(t *testing.T) {
	for _, c := range []struct {
		prev, cur string
		want      bool
	}{
		{"8787", "38787", true},
		{"38787", "8787", true},
		{"8787", "8787", false},
		{"", "38787", false}, // first boot of this build: nothing to compare
		{"9791", "8787", false},
		{"8787", "9791", false},
	} {
		if got := daemonPortMoved(c.prev, c.cur); got != c.want {
			t.Errorf("daemonPortMoved(%q, %q) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

func addressMovedTestPane(t *testing.T, m *Manager, id string, cmd ...string) {
	t.Helper()
	p := testPaneWithTail(t, id, id, "")
	p.Cmd = cmd
	m.panes[id] = p
}

// A Startup mode switch moves the port: agent panes from before the boot are
// marked, shells are not, the mark survives a restart in the same mode, and a
// respawn or a closed pane clears it (ST-230).
func TestPaneAddressMovedAcrossBoots(t *testing.T) {
	t.Cleanup(func() { paneAddressMoved.Store(nil) })
	dir := t.TempDir()
	m := NewManager("")
	addressMovedTestPane(t, m, "claude-pane", "claude", "--resume", "x")
	addressMovedTestPane(t, m, "codex-pane", "codex")
	addressMovedTestPane(t, m, "shell-pane", "bash")
	addressMovedTestPane(t, m, "muse-pane", "muse")
	addressMovedTestPane(t, m, "resumed-pane", "claude", "--resume", "y")

	boot := func(addr string) { wirePaneAddressMoved(&bootEnv{dataDir: dir, addr: addr, mgr: m}) }

	boot("127.0.0.1:8787") // first boot of this build: records the port only
	if paneAddressLost("claude-pane") {
		t.Fatal("no port move yet, nothing is marked")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, listenPortFile)); string(raw) != "8787\n" {
		t.Fatalf("listen-port = %q", raw)
	}

	paneAddressMoved.Store(nil)          // the next boot, before the marks are published
	paneAddressRespawned("resumed-pane") // auto-resumed on the new port during boot
	boot("127.0.0.1:38787")              // Windows-only -> Dual
	if !paneAddressLost("claude-pane") || !paneAddressLost("codex-pane") {
		t.Fatal("agent panes from before the move must be marked")
	}
	if paneAddressLost("shell-pane") {
		t.Fatal("a plain shell is not marked")
	}
	if paneAddressLost("muse-pane") {
		t.Fatal("Muse has no MCP or hook address; a port move does not cut it off")
	}
	if paneAddressLost("resumed-pane") {
		t.Fatal("a pane respawned during boot already has the current address")
	}

	boot("127.0.0.1:38787") // a restart in the same mode keeps the marks
	if !paneAddressLost("claude-pane") {
		t.Fatal("the mark must survive a restart until the pane is restarted")
	}

	paneAddressRespawned("claude-pane")
	if paneAddressLost("claude-pane") {
		t.Fatal("a respawn gets the current address")
	}

	addressMovedTestPane(t, m, "late-pane", "claude") // started on 38787 after the switch
	boot("127.0.0.1:8787")                            // back to Windows-only: panes started on 8787 reconnect
	if paneAddressLost("codex-pane") {
		t.Fatal("switching back to the port a pane started on clears its notice")
	}
	if !paneAddressLost("late-pane") {
		t.Fatal("a pane started on 38787 is cut off by the switch back")
	}
	boot("127.0.0.1:38787")
	if !paneAddressLost("codex-pane") || paneAddressLost("late-pane") {
		t.Fatal("each pane is judged by the port it was started on")
	}
	paneAddressRespawned("late-pane")
	if paneAddressLost("claude-pane") {
		t.Fatal("claude-pane was respawned on 38787 and is on 38787 again")
	}
	paneAddressRespawned("claude-pane")
	paneAddressRespawned("resumed-pane")

	delete(m.panes, "codex-pane")
	boot("127.0.0.1:38787")
	if paneAddressLost("codex-pane") {
		t.Fatal("a closed pane's mark is dropped")
	}
	if _, err := os.Stat(filepath.Join(dir, addressMovedPanesFile)); !os.IsNotExist(err) {
		t.Fatalf("an empty record is removed: %v", err)
	}
}

// A side daemon never marks anything, even if its port changes.
func TestPaneAddressMovedIgnoresSidePorts(t *testing.T) {
	t.Cleanup(func() { paneAddressMoved.Store(nil) })
	dir := t.TempDir()
	m := NewManager("")
	addressMovedTestPane(t, m, "claude-pane", "claude")
	wirePaneAddressMoved(&bootEnv{dataDir: dir, addr: "127.0.0.1:9791", mgr: m})
	wirePaneAddressMoved(&bootEnv{dataDir: dir, addr: "127.0.0.1:9792", mgr: m})
	if paneAddressLost("claude-pane") {
		t.Fatal("side ports are not a Startup mode switch")
	}
	if _, err := os.Stat(filepath.Join(dir, listenPortFile)); !os.IsNotExist(err) {
		t.Fatal("a side daemon must not overwrite the primary's last port")
	}
}

// The pane list carries the mark to the app.
func TestPaneDTOCarriesAddressLost(t *testing.T) {
	t.Cleanup(func() { paneAddressMoved.Store(nil) })
	p := testPaneWithTail(t, "claude-pane", "c", "")
	p.Cmd = []string{"claude"}
	store := &paneAddressMovedStore{path: filepath.Join(t.TempDir(), addressMovedPanesFile), own: "38787", panes: map[string]string{"claude-pane": "8787"}}
	paneAddressMoved.Store(store)
	if d := makePaneDTOWithProcessSnapshot(p, nil, nil, paneProcessSnapshot{}); !d.AddressLost {
		t.Fatal("pane DTO must report addressLost")
	}
}
