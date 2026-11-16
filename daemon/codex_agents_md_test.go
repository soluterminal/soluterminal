package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyCodexAgentsBlock is the shape earlier versions wrote into the global
// ~/.codex/AGENTS.md (team text abbreviated).
func legacyCodexAgentsBlock() string {
	return codexAgentsBeginMarker + "\n" +
		"This block is maintained by the soluterminal daemon; edits inside the markers are overwritten. Content outside is never touched.\n\n" +
		"soluterminal is the user's multi-pane terminal workspace. The workflow task engine is unavailable.\n\n" +
		codexAgentsContextPackLine + "\n\n" + codexAgentsEndMarker
}

func writeAgentsMd(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

// The user's own text survives byte for byte: appended (the shape the old
// sync wrote, with the blank line it added), at the top, in the middle, with
// CRLF line ends and without a final newline.
func TestCleanupCodexAgentsMdKeepsTheUsersText(t *testing.T) {
	block := legacyCodexAgentsBlock()
	cases := map[string]struct{ before, after string }{
		"appended":         {"# Mine\nKeep this.\n\n" + block + "\n", "# Mine\nKeep this.\n"},
		"appended, no nl":  {"# Mine\n\n" + block + "\n", "# Mine\n"},
		"block first":      {block + "\nUser text after.\n", "User text after.\n"},
		"middle, crlf":     {"top\r\n\n" + block + "\nbottom\r\n", "top\r\nbottom\r\n"},
		"no final newline": {"mine\n\n" + block, "mine\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeAgentsMd(t, tc.before)
			cleanupCodexAgentsMd(path, t.TempDir())
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.after {
				t.Fatalf("after cleanup = %q, want %q", got, tc.after)
			}
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
				t.Fatalf("mode changed to %v", info.Mode().Perm())
			}
			if _, err := os.Stat(path + ".solu-tmp"); !os.IsNotExist(err) {
				t.Fatal("temporary file left behind")
			}
		})
	}
}

// A file that held only our block was SoluTerminal's: it is deleted, not left
// empty.
func TestCleanupCodexAgentsMdDeletesAFileThatWasOnlyOurs(t *testing.T) {
	for _, body := range []string{legacyCodexAgentsBlock() + "\n", legacyCodexAgentsBlock(), "\n\n" + legacyCodexAgentsBlock() + "\n"} {
		path := writeAgentsMd(t, body)
		cleanupCodexAgentsMd(path, t.TempDir())
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("file holding only our block was kept: %v", err)
		}
	}
}

func TestCleanupCodexAgentsMdLeavesOtherFilesAlone(t *testing.T) {
	cases := map[string]string{
		"no block":          "# My own instructions\n",
		"end before begin":  codexAgentsEndMarker + "\nuser\n" + codexAgentsBeginMarker,
		"begin without end": "user\n" + codexAgentsBeginMarker + "\nstale",
		"duplicate begin":   codexAgentsBeginMarker + "\n" + codexAgentsBeginMarker + "\n" + codexAgentsEndMarker,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeAgentsMd(t, body)
			cleanupCodexAgentsMd(path, t.TempDir())
			got, err := os.ReadFile(path)
			if err != nil || string(got) != body {
				t.Fatalf("file changed: %q, %v", got, err)
			}
		})
	}
	// A missing file is not created, and a directory in its place is kept.
	missing := filepath.Join(t.TempDir(), "AGENTS.md")
	cleanupCodexAgentsMd(missing, t.TempDir())
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("cleanup created a file")
	}
	dir := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanupCodexAgentsMd(dir, t.TempDir())
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatal("a directory at the path was replaced")
	}
}

// The registration pass cleans up and never writes the global file any more.
func TestCodexRegistrationNoLongerWritesGlobalAgentsMd(t *testing.T) {
	home, _, _ := isolateAgentHome(t)
	agents := filepath.Join(home, ".codex", "AGENTS.md")
	cleanupCodexAgentsMd(agents, t.TempDir())
	if _, err := os.Stat(agents); !os.IsNotExist(err) {
		t.Fatal("cleanup wrote a global AGENTS.md")
	}
	for _, c := range agentIntegrationChanges() {
		if strings.HasSuffix(c.Path, "AGENTS.md") {
			t.Fatalf("the integration still lists a global AGENTS.md change: %+v", c)
		}
	}
}

// Every edit or removal keeps a copy of the file first, in the backup folder
// (agentIntegrationBackupDir); with no folder, or a failed copy, nothing is
// changed.
func TestCleanupCodexAgentsMdBacksUpFirst(t *testing.T) {
	backupOf := func(t *testing.T, dir string) string {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("backup folder = %v, %v; want one copy", entries, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	edited := "# Mine\n\n" + legacyCodexAgentsBlock() + "\n"
	path := writeAgentsMd(t, edited)
	backups := filepath.Join(t.TempDir(), "backups")
	cleanupCodexAgentsMd(path, backups)
	if got, _ := os.ReadFile(path); string(got) != "# Mine\n" {
		t.Fatalf("edit = %q", got)
	}
	if got := backupOf(t, backups); got != edited {
		t.Fatalf("backup after an edit = %q, want the file before it", got)
	}

	onlyOurs := legacyCodexAgentsBlock() + "\n"
	path = writeAgentsMd(t, onlyOurs)
	backups = filepath.Join(t.TempDir(), "backups")
	cleanupCodexAgentsMd(path, backups)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file holding only our block was kept")
	}
	if got := backupOf(t, backups); got != onlyOurs {
		t.Fatalf("backup after a remove = %q, want the removed file", got)
	}

	// No backup folder, or one that can't be written: the file stays.
	path = writeAgentsMd(t, edited)
	cleanupCodexAgentsMd(path, "")
	if got, _ := os.ReadFile(path); string(got) != edited {
		t.Fatalf("changed without a backup folder: %q", got)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupCodexAgentsMd(path, filepath.Join(blocker, "backups"))
	if got, _ := os.ReadFile(path); string(got) != edited {
		t.Fatalf("changed although the backup failed: %q", got)
	}
}

// A symlinked AGENTS.md (a dotfiles setup) is never edited: replacing it
// would turn the user's link into a plain file.
func TestCleanupCodexAgentsMdLeavesASymlinkAlone(t *testing.T) {
	body := "# Mine\n\n" + legacyCodexAgentsBlock() + "\n"
	target := filepath.Join(t.TempDir(), "dotfiles-AGENTS.md")
	if err := os.WriteFile(target, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	backups := filepath.Join(t.TempDir(), "backups")
	cleanupCodexAgentsMd(link, backups)
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
	if got, _ := os.ReadFile(target); string(got) != body {
		t.Fatalf("the link's target changed: %q", got)
	}
	if _, err := os.Stat(backups); !os.IsNotExist(err) {
		t.Fatal("a backup was taken for a file that was not changed")
	}
}
