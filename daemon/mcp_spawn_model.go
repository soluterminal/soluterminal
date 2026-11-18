package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"soluterminal-shared/providers"
)

// spawnModelPattern bounds the model id an agent may pass to spawn_pane. Real
// ids look like "claude-opus-5-5", "opus", "gpt-5.5-codex" or
// "claude-fable-5-1[1m]"; anything else (spaces, quotes, a leading dash that
// would read as another flag) is refused rather than forwarded to the CLI.
var spawnModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,79}$`)

// codexEffortLevels are the model_reasoning_effort values spawn_pane forwards
// to a Codex model the provider catalog doesn't list. Codex accepts any string
// here without complaint, so an unknown level would silently fall back to its
// default; refusing it keeps the choice the lead made visible.
var codexEffortLevels = []string{"low", "medium", "high", "xhigh"}

// codexSpawnEffortAllowed checks effort against the model's own levels in the
// provider catalog (some models add max), falling back to codexEffortLevels
// for an uncatalogued model or none, so max needs an explicit model.
func codexSpawnEffortAllowed(model, effort string) bool {
	if p, ok := providers.Get("codex"); ok && model != "" {
		if levels := p.Models.EffortLevelsFor(model); len(levels) > 0 {
			return slices.Contains(levels, effort)
		}
	}
	return slices.Contains(codexEffortLevels, effort)
}

// spawnAgentModelArgv appends the agent's own model and reasoning-effort flags
// to a bare claude or codex argv, so a lead can staff a worker pane with a
// specific model (spawn_pane model/effort). The flags ride Pane.Cmd, so a
// resumed pane keeps the model it was started with. Returns cmd unchanged when
// both are empty. Provider routing (GLM) is deliberately not offered here:
// that sends code to a third party and stays a choice the person makes.
func spawnAgentModelArgv(cmd []string, model, effort string) ([]string, error) {
	model, effort = strings.TrimSpace(model), strings.ToLower(strings.TrimSpace(effort))
	if model == "" && effort == "" {
		return cmd, nil
	}
	if model != "" && !spawnModelPattern.MatchString(model) {
		return nil, fmt.Errorf("model %q is not a model id", model)
	}
	out := append([]string(nil), cmd...)
	switch {
	case cmdStartsClaude(cmd):
		if effort != "" && !validEffortLevel(effort) {
			return nil, fmt.Errorf("effort %q is not one of low, medium, high, xhigh, max", effort)
		}
		if model != "" {
			out = append(out, "--model", model)
		}
		if effort != "" {
			out = append(out, "--effort", effort)
		}
	case cmdStartsCodex(cmd):
		if effort != "" && !codexSpawnEffortAllowed(model, effort) {
			return nil, fmt.Errorf("effort %q is not supported by this Codex model", effort)
		}
		if model != "" {
			out = append(out, "-m", model)
		}
		if effort != "" {
			out = append(out, "-c", "model_reasoning_effort="+effort)
		}
	default:
		return nil, fmt.Errorf("model and effort need cmd \"claude\" or \"codex\"")
	}
	return out, nil
}

// Where a spawn_pane agent pane's model came from, as its result reports it.
const (
	spawnModelFromCaller  = "caller"               // the spawn_pane call named it
	spawnModelFromSetting = "worker_model_setting" // the person's worker-model setting
	spawnModelFromNone    = "none"                 // no flag: the agent's own default
)

// spawnPaneModel picks the model flag for a spawn_pane agent pane (ST-283). A
// model the caller names always wins. Otherwise a Claude worker (no lead,
// including a cmd auto worker that resolved to Claude, ST-296) takes the
// person's worker-model setting, so it no longer
// inherits whatever /model pick last rewrote the `model` key in Claude Code's
// own settings.json. Otherwise none: the agent picks its own default, exactly
// as before the setting existed. Leads, Codex and other commands never take
// the setting.
func spawnPaneModel(cmd []string, callerModel string, worker bool, setting string) (model, source string) {
	if m := strings.TrimSpace(callerModel); m != "" {
		return m, spawnModelFromCaller
	}
	if s := strings.TrimSpace(setting); worker && s != "" && cmdStartsClaude(cmd) {
		return s, spawnModelFromSetting
	}
	return "", spawnModelFromNone
}

// spawnModelNote explains a pane started without a model flag, for the
// spawn_pane result and list_panes.
func spawnModelNote(cmd []string) string {
	if cmdStartsCodex(cmd) {
		return "no model flag passed: Codex uses its own default model"
	}
	return "no model flag passed: Claude Code uses its own default (the model key in ~/.claude/settings.json, which a /model pick in any pane rewrites; a resumed session keeps its own model)"
}

// paneStartModel is the model a claude or codex pane was started with, read
// from the flags on its argv (Pane.Cmd): claude --model, codex -m/--model or
// -c model=. The last one wins, as in the CLIs. "" when none was passed or the
// argv is not a claude or codex command (bare, or in the login-shell wrapper,
// paneAgentArgv). It reads only the argv, never a transcript, so list_panes
// can call it for every pane.
func paneStartModel(cmd []string) string {
	cmd = paneAgentArgv(cmd)
	codex := cmdStartsCodex(cmd)
	if cmd == nil {
		return ""
	}
	model := ""
	for i := 1; i < len(cmd); i++ {
		name, value, joined := strings.Cut(cmd[i], "=")
		isModel := name == "--model" || (codex && name == "-m" && !joined)
		isConfig := codex && (name == "-c" || name == "--config")
		if !isModel && !isConfig {
			continue
		}
		if !joined {
			if i+1 >= len(cmd) {
				break
			}
			i++
			value = cmd[i]
		}
		if isConfig {
			key, v, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(key) != "model" {
				continue
			}
			value = strings.Trim(strings.TrimSpace(v), `"'`)
		}
		if v := strings.TrimSpace(value); v != "" {
			model = v
		}
	}
	return model
}

// paneStartModelNote is list_panes' plain word for a claude or codex pane
// started with no model flag; "" otherwise.
func paneStartModelNote(cmd []string) string {
	if paneAgentArgv(cmd) == nil || paneStartModel(cmd) != "" {
		return ""
	}
	return "none passed: the agent's own default"
}

// paneAgentArgv is a pane's claude or codex argv: Pane.Cmd itself when it
// starts with the agent, or the agent's words recovered from the login-shell
// wrapper tryLoginShellWrap stores when the agent isn't on the daemon's PATH
// (`bash -lc "<init>; 'claude' '--model' 'opus'; exec bash -i"`). nil for
// anything else.
func paneAgentArgv(cmd []string) []string {
	if cmdStartsClaude(cmd) || cmdStartsCodex(cmd) {
		return cmd
	}
	if len(cmd) != 3 || (cmd[1] != "-lc" && cmd[1] != "-c") {
		return nil
	}
	for _, name := range []string{"claude", "codex"} {
		for _, lead := range []string{"; ", "exec "} {
			if i := strings.Index(cmd[2], lead+"'"+name+"'"); i >= 0 {
				return singleQuotedWords(cmd[2][i+len(lead):])
			}
		}
	}
	return nil
}

// singleQuotedWords reads the leading run of space-separated words that
// shellSingleQuote wrote (each word in single quotes, a quote inside it
// closed, escaped and reopened), stopping at the first character that isn't
// part of one.
func singleQuotedWords(s string) []string {
	var out []string
	for strings.HasPrefix(s, "'") {
		var word strings.Builder
		for {
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return out
			}
			word.WriteString(s[1 : 1+end])
			s = s[2+end:]
			if !strings.HasPrefix(s, `\''`) {
				break
			}
			word.WriteByte('\'')
			s = s[2:]
		}
		out = append(out, word.String())
		if !strings.HasPrefix(s, " ") {
			break
		}
		s = s[1:]
	}
	return out
}
