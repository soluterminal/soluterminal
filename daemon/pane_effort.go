package main

import (
	"strings"
)

// pane_effort.go implements POST /api/panes/{id}/effort: change a NATIVE (Focus)
// pane's reasoning-effort level (--effort) on a running conversation.
//
// Why an endpoint and not a slash command: a Focus pane runs headless
// `claude -p`, which does NOT interpret /effort (it's TUI-only), so the level
// has to be baked into the argv at spawn (nativeClaudeArgv). Changing it on a
// live pane therefore means respawning the headless agent with the new --effort,
// reusing the exact keep-conversation recipe as refresh (respawnInPlace +
// `claude --resume <session>`). Provider-agnostic: this drives effort for both
// Anthropic and GLM panes identically — only the per-pane DEFAULT differs (GLM
// defaults to "max" at creation; see createPane). Terminal panes are excluded:
// they run the interactive claude TUI where /effort already works directly.

// validEffortLevel reports whether s is an accepted --effort value. "" is valid
// and means "no flag" — claude's built-in default (also used to RESET effort).
func validEffortLevel(s string) bool {
	switch strings.TrimSpace(s) {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}
