package main

// Per-pane USER TITLE OVERRIDE endpoints.
//
// A pane's Activity-tab header shows Claude Code's auto-generated ai-title by
// default. These two endpoints let the user override it:
//
//   POST /api/panes/{id}/title          — set/clear the override to a typed value
//   POST /api/panes/{id}/title/generate — generate a fresh title with the
//                                         user's own `claude -p` (Haiku)
//
// Both write the SAME field (Pane.TitleOverride, persisted), which the
// conversation endpoint surfaces as "titleOverride" so the client renders it
// over ai-title. Clearing it (empty/whitespace) falls back to ai-title.
//
// Neither handler ever panics: a missing pane is 404, no resolvable transcript
// or empty conversation is 422, and a missing Claude Code or a failed run is
// 502 — each with a short plain-text body.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type setTitleReq struct {
	Title string `json:"title"`
}

// setPaneTitle handles POST /api/panes/{id}/title. It sets the pane's override
// to the trimmed title (an empty/whitespace title CLEARS it, falling back to
// ai-title) and persists. Modeled on renamePane's decode + persist-with-revert.
func (a *apiServer) setPaneTitle(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "change a pane title", "rename_pane") {
		return
	}
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var req setTitleReq
	if !decodeJSON(w, r, &req, defaultJSONBodyBytes) {
		return
	}
	trimmed := strings.TrimSpace(req.Title)

	prev := p.Snapshot().TitleOverride
	if p.SetTitleOverride(trimmed) && a.panes != nil {
		// Persist the new override so it survives daemon restart. On persist
		// failure revert the in-memory value (mirror renamePane) and 500.
		if err := a.panes.Put(p); err != nil {
			p.SetTitleOverride(prev)
			http.Error(w, "persist pane: "+err.Error(), 500)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"titleOverride": trimmed})
}

// generatePaneTitle handles POST /api/panes/{id}/title/generate. It reads the
// pane's recent transcript, asks the user's own Claude Code (`claude -p`,
// Haiku, no tools) for a short title, stores it into the override (persist),
// and returns it.
//
// Error mapping (always plain text, never a panic):
//   - no resolvable transcript / empty conversation → 422
//   - Claude Code missing / the run failed            → 502
func (a *apiServer) generatePaneTitle(w http.ResponseWriter, r *http.Request) {
	if !a.requireHuman(w, r, "generate a pane title", "rename_pane") {
		return
	}
	id := r.PathValue("id")
	pane, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	// Resolve + read the pane's recent transcript, the same bounded-tail path
	// the conversation endpoint uses. No transcript / unreadable / no usable
	// turns → 422 (there's simply nothing to title yet).
	path := a.transcriptForPane(pane)
	if path == "" {
		http.Error(w, "no conversation to title", 422)
		return
	}
	buf, err := tailReadBounded(path, convTailWindowBytes)
	if err != nil {
		http.Error(w, "no conversation to title", 422)
		return
	}
	turns, _ := projectConversation(buf, 40)
	convText := buildTitleConvText(turns, titleConvMaxChars)
	if strings.TrimSpace(convText) == "" {
		http.Error(w, "no conversation to title", 422)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), titleCLITimeoutSeconds*time.Second)
	defer cancel()
	title, err := generateTitleViaCLI(ctx, convText)
	if err != nil {
		if errors.Is(err, errTitleCLIUnavailable) {
			http.Error(w, "Claude Code isn't installed or signed in", 502)
			return
		}
		http.Error(w, "title generation failed", 502)
		return
	}

	prev := pane.Snapshot().TitleOverride
	if pane.SetTitleOverride(title) && a.panes != nil {
		if perr := a.panes.Put(pane); perr != nil {
			pane.SetTitleOverride(prev)
			http.Error(w, "persist pane: "+perr.Error(), 500)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"titleOverride": title})
}

// titleConvMaxChars caps the conversation text we hand the title model. A few
// thousand chars of the NEWEST turns is plenty of signal for a 6-word title
// while keeping the request tiny.
const titleConvMaxChars = 4000

// buildTitleConvText flattens projected turns into a compact plain-text
// conversation for the title prompt: user prompts as "User: ...", assistant
// prose as "Assistant: ...", and tool_use as "[tool: <Tool> <toolHint>]". It
// keeps the NEWEST turns within maxChars (oldest dropped first), so the title
// reflects what the session is currently working on. Pure (no IO).
func buildTitleConvText(turns []convTurn, maxChars int) string {
	// Render each turn to a single line, then keep the newest lines that fit.
	lines := make([]string, 0, len(turns))
	for i := range turns {
		t := &turns[i]
		switch {
		case t.Kind == "tool_use":
			s := "[tool: " + t.Tool
			if hint := strings.TrimSpace(t.ToolHint); hint != "" {
				s += " " + hint
			}
			s += "]"
			lines = append(lines, s)
		case t.Role == "user":
			if txt := strings.TrimSpace(t.Text); txt != "" {
				lines = append(lines, "User: "+txt)
			}
		case t.Role == "assistant":
			if txt := strings.TrimSpace(t.Text); txt != "" {
				lines = append(lines, "Assistant: "+txt)
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}

	// Accumulate from the END (newest) until adding another line would exceed
	// maxChars, then emit in chronological order.
	total := 0
	start := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		add := len(lines[i]) + 1 // + newline
		if total+add > maxChars && start != len(lines) {
			break
		}
		total += add
		start = i
	}
	kept := lines[start:]
	// Guard the very-first-line-too-long case: clamp it so we never exceed the
	// cap by more than one line's tail.
	if len(kept) == 1 && len(kept[0]) > maxChars {
		kept[0] = kept[0][:maxChars]
	}
	return strings.Join(kept, "\n")
}
