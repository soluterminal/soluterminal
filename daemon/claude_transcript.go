package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Helpers for reading the on-disk Claude Code transcript: where a session's
// .jsonl lives, and how to classify its `user` records (used by the pane reply
// tracker and the turn tracker).

// transcriptSlugRe matches every char Claude Code's project-dir encoder maps to
// '-'. Empirically the slug is the cwd with every non-alphanumeric char replaced
// by '-' (a strict superset of the "'/' and '.' -> '-'" rule; they agree for any
// path without '_'/space/'~'). Use the non-alnum rule to match the real encoder.
var transcriptSlugRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// transcriptSlug munges a pane cwd into the Claude Code project-dir slug.
func transcriptSlug(cwd string) string {
	return transcriptSlugRe.ReplaceAllString(cwd, "-")
}

// resolveTranscriptPath returns the first existing
// <configDir>/projects/<slug>/<sessionID>.jsonl across the resolved Claude
// config dirs, or "" if none exists. cwd or sessionID empty -> "".
func resolveTranscriptPath(cwd, sessionID string) string {
	if cwd == "" || sessionID == "" {
		return ""
	}
	slug := transcriptSlug(cwd)
	for _, dir := range claudeConfigDirs() {
		p := filepath.Join(dir, "projects", slug, sessionID+".jsonl")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// claudeInterruptMarkerPrefix is the text Claude Code writes as a `user` record
// when the USER stops a turn: "[Request interrupted by user]" and "[Request
// interrupted by user for tool use]" (both observed on disk; the latter follows
// the tool_result of the tool that was cancelled). It is POSITIVE evidence that
// a stop was deliberate, which is the only thing separating "you interrupted
// me" from "a restart killed me". Match
// on the block text's PREFIX, never a substring of the whole record: a /compact
// summary quotes the marker inside a much longer text and must not read as an
// interrupt.
const claudeInterruptMarkerPrefix = "[Request interrupted by user"

// claudeUserTurnKind classifies a transcript `user` record. The `user` type is overloaded on disk: real human turns share
// it with tool results, interrupt markers, and synthetic stubs (compaction
// notices, command stdout, task notifications) that are NOT turns anyone is
// waiting on.
type claudeUserTurnKind int

const (
	// claudeUserIgnore is a synthetic/meta record: a `<local-command-stdout>
	// Compacted…` stub, a hook caveat, a promptSource=system task notification.
	// Critically, the compaction stub ends real, healthy, IDLE sessions — reading
	// one as a pending turn would show "a restart interrupted you" to a user who
	// simply ran /compact and walked away.
	claudeUserIgnore     claudeUserTurnKind = iota
	claudeUserPrompt                        // a real human turn — the assistant owes a reply
	claudeUserToolResult                    // a tool result — the API REQUIRES an assistant reply next
	claudeUserInterrupt                     // "[Request interrupted by user…]" — a deliberate stop
)

// isRealClaudeUserTurn is the real-human-turn gate: a turn
// someone is actually waiting on carries promptSource typed/sdk/queued and none
// of the meta flags. promptSource=system (task notifications) and absent
// (compaction stubs, command stdout) are machinery, not turns.
func isRealClaudeUserTurn(promptSource string, isMeta, isCompactSummary, isVisibleOnly bool) bool {
	if isMeta || isCompactSummary || isVisibleOnly {
		return false
	}
	switch promptSource {
	case "typed", "sdk", "queued":
		return true
	default:
		return false
	}
}

// classifyClaudeUserRecord sorts a transcript `user` record. Content comes in BOTH shapes on disk and a human turn can wear
// either: a typed turn records `content:"text"`, while a turn submitted as
// content blocks records `content:[{type:"text"}]` with promptSource=sdk
// (verified by killing a real headless agent). Blocks are checked for the
// interrupt marker FIRST — it is the one shape that must never be mistaken for
// a pending turn.
func classifyClaudeUserRecord(promptSource string, isMeta, isCompactSummary, isVisibleOnly bool, message json.RawMessage) claudeUserTurnKind {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(message, &m) != nil || len(m.Content) == 0 {
		return claudeUserIgnore
	}
	// String content => a human turn, or a synthetic stub wearing one's clothes.
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		if !isRealClaudeUserTurn(promptSource, isMeta, isCompactSummary, isVisibleOnly) || strings.TrimSpace(s) == "" {
			return claudeUserIgnore
		}
		return claudeUserPrompt
	}
	// List content => the interrupt marker, tool_result blocks, or a block-shaped
	// human turn.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return claudeUserIgnore
	}
	for _, b := range blocks {
		if b.Type == "text" && strings.HasPrefix(strings.TrimSpace(b.Text), claudeInterruptMarkerPrefix) {
			return claudeUserInterrupt
		}
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return claudeUserToolResult
		}
	}
	if !isRealClaudeUserTurn(promptSource, isMeta, isCompactSummary, isVisibleOnly) {
		return claudeUserIgnore
	}
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return claudeUserPrompt
		}
	}
	return claudeUserIgnore
}
