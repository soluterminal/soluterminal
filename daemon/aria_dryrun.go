package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const ariaDryRunObservationTemplate = `[AUTONOMOUS EVALUATION — you were invoked by the pane-state watcher, not by the user.]

Pane %q (id: dryrun_%s) just entered awaiting-input. Here is its current tail:

<pane_output>
%s
</pane_output>

The user has set this standing order for this pane:
"%s"

Decide:
1. If the standing order clearly covers THIS prompt, call send_input with the appropriate reply. The user will see a 5-second cancel window before it actually fires.
2. If the order does NOT cover this prompt (wrong type, outside scope, ambiguous), respond with a single short sentence explaining why, and DO NOT call any tools. The user still has the observation card and will handle it manually.

CRITICAL — send_input newline:
- The 'input' arg MUST end with '\n' to actually press Enter and submit. Bare '1' types the digit but Claude Code never sees it because Enter wasn't pressed.
- For a numbered menu, pass input='1\n' (or whatever the choice is). For y/n, pass input='y\n'. The trailing newline is mandatory for ANY reply you intend to submit.

Be decisive. Don't narrate. Don't call read_pane_tail — the full tail is already above.`

// ARIA dry-run: regression-test surface for prompts. Given a synthetic
// pane tail + standing-order instruction, run the same model the
// autoresponder would, and return what it would HAVE done — without
// actually executing any tools.
//
// Why this matters: the autoresponder's behaviour depends on (a) the
// system prompt, (b) the tool definitions, (c) the per-pane standing
// order, (d) the model's interpretation of all the above. When any of
// those change, today's only feedback is "ARIA misfired in the wild".
// dryrun lets us replay a fixed corpus of historical observations
// against a candidate prompt or model and diff the decisions before
// shipping.
//
// Loopback-only because the surface returns LLM completions billed to
// the user's cloud token; no need to expose it to the public web.

type ariaDryRunReq struct {
	// PaneTail is the synthetic 30-line tail the model sees inside the
	// <pane_output> tag. Match what trimTrailingStatusJunk would have
	// produced for a real observation.
	PaneTail string `json:"paneTail"`
	// Instruction is the standing order the user has set on the pane,
	// or one of the synthesised AutoMode instructions ("always" /
	// "cautious") — exactly what evaluateObservation would have
	// substituted into the message.
	Instruction string `json:"instruction"`
	// Model overrides the sweeper-model setting just for this call.
	// Empty falls back to settings.SweeperModel(). Useful for "would
	// Haiku 4.5 still get this right?" experiments.
	Model string `json:"model,omitempty"`
	// PaneTitle is cosmetic — substituted into the message's
	// "Pane %q (id: ...)" preamble. Default "test-pane" if empty.
	PaneTitle string `json:"paneTitle,omitempty"`
}

type ariaDryRunResp struct {
	Model      string      `json:"model"`
	StopReason string      `json:"stopReason"`
	Blocks     []ariaBlock `json:"blocks"`
	Usage      ariaUsage   `json:"usage"`
}

func (a *apiServer) handleAriaDryRun(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "aria/dryrun is loopback-only", http.StatusForbidden)
		return
	}
	if a.aria == nil {
		http.Error(w, "aria not configured (no cloud relay)", http.StatusServiceUnavailable)
		return
	}
	if base, tok := a.aria.cloudCreds(); base == "" || tok == "" {
		http.Error(w, "aria not configured (no cloud relay)", http.StatusServiceUnavailable)
		return
	}
	var req ariaDryRunReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	if strings.TrimSpace(req.PaneTail) == "" || strings.TrimSpace(req.Instruction) == "" {
		http.Error(w, "paneTail and instruction are required", http.StatusBadRequest)
		return
	}
	title := req.PaneTitle
	if title == "" {
		title = "test-pane"
	}

	resp, err := a.aria.dryRun(r.Context(), title, req.PaneTail, req.Instruction, req.Model)
	if err != nil {
		http.Error(w, "dryrun: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// dryRun synthesises the same observation message evaluateObservation
// would have built, sends one streaming completion request, and
// returns the assembled assistant blocks. Crucially, it does NOT
// enter the runTurn tool-execution loop — a tool_use block in the
// response is data for the caller, not an action.
func (a *Aria) dryRun(ctx context.Context, paneTitle, paneTail, instruction, modelOverride string) (*ariaDryRunResp, error) {
	// Mirror the message body evaluateObservation builds. Hard-coding
	// the "5-second cancel window" phrase is fine here — we're testing
	// what the model would decide, not what the UI would render, and
	// the prompt's countdown phrasing has historically been stable.
	msg := fmt.Sprintf(ariaDryRunObservationTemplate,
		paneTitle, paneTitle, paneTail, instruction)

	model := strings.TrimSpace(modelOverride)
	if model == "" {
		model = a.settings.SweeperModel()
	}

	body := map[string]any{
		"messages":   []ariaMessage{{Role: "user", Content: []ariaBlock{{Type: "text", Text: msg}}}},
		"tools":      ariaAutonomousTools(),
		"system":     ariaSystemPrompt,
		"max_tokens": 4096,
		"stream":     true,
	}
	if model != "" {
		body["model"] = model
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	resp, err := a.sendCompletion(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("cloud: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cloud http %d", resp.StatusCode)
	}

	// No emit — we don't need streaming events on the dryrun path; the
	// caller wants the final assembled result, not the deltas.
	blocks, stopReason, usage, err := a.consumeStream(resp.Body, func(map[string]any) {})
	if err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	return &ariaDryRunResp{
		Model:      model,
		StopReason: stopReason,
		Blocks:     blocks,
		Usage:      usage,
	}, nil
}
