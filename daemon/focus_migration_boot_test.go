package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A saved Focus (native) pane goes through the steps the boot restore in
// main.go runs for each panes.json entry whose agent is gone: the Codex/Muse
// argv id recovery, healPersistedPane, the Focus conversion
// (retireNativePersistedPane), attach (nothing to reattach), boot's
// "conversation known and identity not blocked" branch, suspendAfterRestart
// under the default "manual" mode, save, and load again. It must come back as a
// terminal pane that resumes the same conversation, with its id, title, cwd and
// conversation id kept and no recovery error.
func TestBootConvertsSavedFocusPanesToResumableTerminalPanes(t *testing.T) {
	cases := []struct {
		name     string
		saved    PersistedPane
		wantCmd  []string
		threadID string
		museID   string
	}{
		{
			name: "Codex",
			saved: PersistedPane{
				ID: "pane_codexfocus", Title: "Codex Focus pane", Cwd: "/work/codex",
				Kind: "native", Agent: agentCodex, Cmd: []string{"codex", "app-server"},
				CodexThreadID: "019a7b2c-3d4e-7f60-8a91-b2c3d4e5f607",
			},
			wantCmd:  []string{"codex", "resume", "019a7b2c-3d4e-7f60-8a91-b2c3d4e5f607"},
			threadID: "019a7b2c-3d4e-7f60-8a91-b2c3d4e5f607",
		},
		{
			name: "Codex saved without an agent field",
			saved: PersistedPane{
				ID: "pane_codexnoagent", Title: "Old Codex Focus pane", Cwd: "/work/old",
				Kind: "native", Cmd: []string{"codex", "app-server", "--stdio"},
				CodexThreadID: "019a7b2c-3d4e-7f60-8a91-b2c3d4e5f608",
			},
			wantCmd:  []string{"codex", "resume", "019a7b2c-3d4e-7f60-8a91-b2c3d4e5f608"},
			threadID: "019a7b2c-3d4e-7f60-8a91-b2c3d4e5f608",
		},
		{
			name: "Muse",
			saved: PersistedPane{
				ID: "pane_musefocus", Title: "Muse Focus pane", Cwd: "/work/muse",
				Kind: "native", Agent: agentMuse, Cmd: []string{"/opt/solud", "muse-driver", "--session-id", "7c9e6679-7425-40de-944b-e07fc1f90ae7"},
				MuseSessionID: "7c9e6679-7425-40de-944b-e07fc1f90ae7",
			},
			wantCmd: []string{"muse", "resume", "7c9e6679-7425-40de-944b-e07fc1f90ae7"},
			museID:  "7c9e6679-7425-40de-944b-e07fc1f90ae7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			saved := tc.saved
			// The Focus agent is gone (an update restart or a reboot).
			saved.AgentPid = -1
			saved.SocketPath = filepath.Join(dir, "agents", saved.ID+".sock")
			raw, err := json.Marshal([]PersistedPane{saved})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "panes.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := NewPaneStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			records := store.List()
			if len(records) != 1 || records[0].Kind != "native" {
				t.Fatalf("saved Focus pane not loaded: %+v", records)
			}

			info := records[0]
			if strings.TrimSpace(info.CodexThreadID) == "" {
				if tid := resumeThreadIDFromCodexCmd(info.Cmd); tid != "" {
					info.CodexThreadID = tid
				}
			}
			if strings.TrimSpace(info.MuseSessionID) == "" {
				if sid := resumeSessionIDFromMuseCmd(info.Cmd); sid != "" {
					info.MuseSessionID = sid
				}
			}
			info, _ = healPersistedPane(info)
			info, converted := retireNativePersistedPane(info)
			if !converted {
				t.Fatal("boot did not convert the saved Focus pane")
			}
			mgr := NewManager(dir)
			if p, err := mgr.AttachExisting(&info); err != nil || p != nil {
				t.Fatalf("attach = (%v, %v), want nothing to reattach", p, err)
			}
			if persistedPaneConversationID(info) == "" || info.IdentityBlocked {
				t.Fatalf("boot would not offer a resume: conversation=%q identityBlocked=%v", persistedPaneConversationID(info), info.IdentityBlocked)
			}
			p := suspendAfterRestart(mgr, &info, recoveryResumeManual)
			if p == nil {
				t.Fatal("the converted pane was dropped instead of suspended")
			}
			if err := store.Put(p); err != nil {
				t.Fatal(err)
			}

			reloaded, err := NewPaneStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			after := reloaded.List()
			if len(after) != 1 {
				t.Fatalf("panes after boot = %d, want 1", len(after))
			}
			got := after[0]
			if got.Kind != "" || isRetiredNativePane(got.Kind, got.Cmd) {
				t.Fatalf("pane still reads as Focus: kind=%q cmd=%q", got.Kind, got.Cmd)
			}
			if !reflect.DeepEqual(got.Cmd, tc.wantCmd) {
				t.Fatalf("cmd = %q, want %q", got.Cmd, tc.wantCmd)
			}
			if got.ID != saved.ID || got.Title != saved.Title || got.Cwd != saved.Cwd {
				t.Fatalf("identity changed: id=%q title=%q cwd=%q, want %q %q %q", got.ID, got.Title, got.Cwd, saved.ID, saved.Title, saved.Cwd)
			}
			if got.CodexThreadID != tc.threadID || got.MuseSessionID != tc.museID {
				t.Fatalf("conversation ids = codex %q muse %q, want %q %q", got.CodexThreadID, got.MuseSessionID, tc.threadID, tc.museID)
			}
			if got.AgentPid != 0 || got.SocketPath != "" {
				t.Fatalf("the Focus agent must not be reattached: pid=%d socket=%q", got.AgentPid, got.SocketPath)
			}
			snap, ok := mgr.Get(saved.ID)
			if !ok || snap == nil {
				t.Fatal("converted pane missing from the manager")
			}
			if s := snap.Snapshot(); s.Kind != "" || !s.Resumable || s.Status != PaneExited || s.RecoveryError != "" || s.IdentityBlocked || s.RestartPaused {
				t.Fatalf("pane snapshot = kind %q resumable %v status %q recoveryError %q identityBlocked %v paused %v, want a resumable exited terminal pane offering Resume",
					s.Kind, s.Resumable, s.Status, s.RecoveryError, s.IdentityBlocked, s.RestartPaused)
			}
		})
	}
}
