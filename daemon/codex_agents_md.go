package main

import (
	"log"
	"os"
	"strings"
)

// Earlier versions kept a marker-delimited block in the user's GLOBAL
// ~/.codex/AGENTS.md: the team brief on team installs, a context-pack note on
// public ones. Every Codex session on the computer reads that file, including
// ones in unrelated repositories, so SoluTerminal no longer writes it; the
// Codex sessions it starts get their instructions per process instead
// (codex_pane_policy.go). What is left here removes the old block.
const (
	codexAgentsBeginMarker = "<!-- soluterminal:managed:begin -->"
	codexAgentsEndMarker   = "<!-- soluterminal:managed:end -->"
)

// cleanupCodexAgentsMd removes the block an earlier version wrote, leaving
// everything else in the file byte-for-byte (codexAgentsRemove also drops the
// blank line that was put before the block). A file that held nothing but the
// block was created by SoluTerminal and is deleted. A file without the block,
// or with an ambiguous marker shape, is not touched.
//
// The file is copied into backupDir before it is edited or removed, as every
// file SoluTerminal changes in the user's Claude/Codex setup is; with no
// backupDir, or a failed copy, nothing is changed. A symlinked AGENTS.md (a
// dotfiles setup, often a file shared across machines or tracked in the
// user's own repository) is never edited, by @secure's rule: the user is told
// to remove the block by hand.
func cleanupCodexAgentsMd(path, backupDir string) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if data, err := os.ReadFile(path); err == nil {
			if _, changed, _ := codexAgentsRemove(string(data)); changed {
				log.Printf("codex-agents: %s is a symlink, so SoluTerminal left it unchanged; remove the block between %s and %s from it by hand", path, codexAgentsBeginMarker, codexAgentsEndMarker)
			}
		}
		return
	}
	if backupDir == "" {
		// rewriteConfigFile skips the copy without a folder; this path never
		// edits without one.
		if data, err := os.ReadFile(path); err == nil {
			if _, changed, _ := codexAgentsRemove(string(data)); changed {
				log.Printf("codex-agents: no backup folder for %s, so the old soluterminal block was left in place", path)
			}
		}
		return
	}
	// The same writer as the off/uninstall path: backup first, exclusive
	// temp file, mode kept, fsync, and a re-read before the rename so an
	// edit saved meanwhile is never lost.
	result, err := rewriteConfigFile(path, backupDir, codexAgentsRemove)
	switch {
	case err != nil:
		log.Printf("codex-agents: could not remove the old soluterminal block from %s: %v", path, err)
		return
	case result == "left as is (unrecognized shape)":
		log.Printf("codex-agents: %s has an unrecognized soluterminal block shape (leaving as is)", path)
		return
	case result != "removed":
		return
	}
	// A file that held only our block was SoluTerminal's: deleted, not left
	// empty (its copy is in the backup folder).
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == "" {
		if err := os.Remove(path); err != nil {
			log.Printf("codex-agents: could not remove %s: %v", path, err)
			return
		}
		log.Printf("codex-agents: removed %s (it held only the old soluterminal block)", path)
		return
	}
	log.Printf("codex-agents: removed the old soluterminal block from %s; your own text is unchanged", path)
}
