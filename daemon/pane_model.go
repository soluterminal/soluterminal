package main

import (
	"regexp"
	"strings"
)

// pane_model.go implements POST /api/panes/{id}/model: change a NATIVE (Focus)
// pane's Claude model (--model) on a running conversation.
//
// Why an endpoint and not a slash command: same story as pane_effort.go — a
// Focus pane runs headless `claude -p`, which does NOT interpret /model (it's
// TUI-only), so the model has to be baked into the argv at spawn
// (nativeClaudeArgv) and changing it means respawning with `claude --resume`.
//
// Why this matters MORE than effort (verified empirically on claude 2.1.205,
// 2026-07-09): `--resume` carries the resumed session's STORED model, silently
// overriding the user's settings.json default. Every Focus respawn / mode
// switch / restart resumes, so a session is permanently trapped on whatever
// model it last ran — a settings change never takes effect. Passing --model
// once REWRITES the session's stored model (subsequent bare resumes keep it),
// so a single respawn through this endpoint heals a trapped pane for good.

// validModelID accepts "" (no flag) or a plausible claude --model value: a
// model id ("claude-fable-5-1"), an alias ("sonnet"), or the 1M-context marker
// form ("claude-fable-5-1[1m]"). Deliberately a CHARSET check, not an allowlist —
// model names churn far too often to hardcode here; the CLI itself rejects an
// unknown id at spawn and the respawn error surfaces to the caller.
var validModelRE = regexp.MustCompile(`^[A-Za-z0-9._\[\]-]{1,64}$`)

func validModelID(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || validModelRE.MatchString(s)
}
