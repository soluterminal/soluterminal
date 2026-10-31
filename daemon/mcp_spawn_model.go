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
