package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The latest agent reply in a pane, for read_pane_tail, ask_pane and
// wait_for_pane. The screen scrollback is a poor answer for another agent: it
// holds earlier sessions, escape debris and TUI chrome. Claude Code and Codex
// both write their conversation to disk, so the reply comes from there:
//
//   - Claude Code: the session transcript (transcriptForPane), assistant text
//     after the last real user prompt; final once an end_turn reply lands.
//   - Codex: the rollout file of the pane's thread, assistant messages after
//     the last user message; final at task_complete.
//
// Anything else, or an agent whose file cannot be found, falls back to the
// pane's current rendered screen (the replay emulator), which never contains
// old scrollback.

const paneReplyTailBytes = 2 << 20

type paneReply struct {
	Agent       string    `json:"agent,omitempty"`
	Source      string    `json:"source"` // "transcript" | "screen"
	Prompt      string    `json:"prompt,omitempty"`
	PromptAt    time.Time `json:"-"`
	CompletedAt time.Time `json:"-"`
	Reply       string    `json:"reply"`
	Complete    bool      `json:"complete"`
	PromptSeen  bool      `json:"-"`
	Path        string    `json:"-"` // the transcript or rollout read
}

// claudeLatestReply projects a Claude Code transcript tail.
func claudeLatestReply(buf []byte) paneReply {
	r := paneReply{Agent: agentClaude, Source: "transcript"}
	var parts []string
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type         string          `json:"type"`
			Subtype      string          `json:"subtype"`
			Timestamp    string          `json:"timestamp"`
			Sidechain    bool            `json:"isSidechain"`
			PromptSource string          `json:"promptSource"`
			Meta         bool            `json:"isMeta"`
			Compact      bool            `json:"isCompactSummary"`
			Visible      bool            `json:"isVisibleInTranscriptOnly"`
			Message      json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &env) != nil || env.Sidechain {
			continue
		}
		switch env.Type {
		case "user":
			switch classifyClaudeUserRecord(env.PromptSource, env.Meta, env.Compact, env.Visible, env.Message) {
			case claudeUserPrompt:
				r.Prompt = claudeMessageText(env.Message)
				r.PromptAt = parseTranscriptTime(env.Timestamp)
				r.PromptSeen = true
				r.Complete = false
				r.CompletedAt = time.Time{}
				parts = parts[:0]
			case claudeUserToolResult:
				r.Complete = false
				r.CompletedAt = time.Time{}
			case claudeUserInterrupt:
				r.Complete = true
				r.CompletedAt = time.Time{}
			}
		case "assistant":
			var msg struct {
				Stop    string `json:"stop_reason"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(env.Message, &msg) != nil {
				continue
			}
			for _, b := range msg.Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					parts = append(parts, strings.TrimSpace(b.Text))
				}
			}
			r.Complete = msg.Stop == "end_turn" || msg.Stop == "stop_sequence"
			r.CompletedAt = time.Time{}
			if r.Complete {
				r.CompletedAt = parseTranscriptTime(env.Timestamp)
			}
		}
	}
	r.Reply = strings.Join(parts, "\n\n")
	return r
}

// claudeReplyTo finds, in a Claude Code transcript tail, the newest place a
// user message satisfying match went in, and what Claude said after it. A
// queued message goes in either as its own turn (a prompt with promptSource
// "queued") or, mid-turn, with the next tool result (a queued_command
// attachment); either way the reply runs to the end of that turn, and it
// is found after later turns have run. Messages queued together go in
// together, as back-to-back prompts with one reply, so a prompt only ends
// ours once Claude has answered something since ours went in.
func claudeReplyTo(buf []byte, match func(prompt string) bool) paneReply {
	r := paneReply{Agent: agentClaude, Source: "transcript"}
	var parts []string
	ours, answered := false, false
	start := func(prompt, at string) {
		ours, answered = true, false
		r = paneReply{Agent: agentClaude, Source: "transcript", Prompt: prompt, PromptAt: parseTranscriptTime(at), PromptSeen: true}
		parts = parts[:0]
	}
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type         string          `json:"type"`
			Timestamp    string          `json:"timestamp"`
			Sidechain    bool            `json:"isSidechain"`
			PromptSource string          `json:"promptSource"`
			Meta         bool            `json:"isMeta"`
			Compact      bool            `json:"isCompactSummary"`
			Visible      bool            `json:"isVisibleInTranscriptOnly"`
			Message      json.RawMessage `json:"message"`
			Attachment   struct {
				Type   string `json:"type"`
				Prompt string `json:"prompt"`
			} `json:"attachment"`
		}
		if json.Unmarshal(line, &env) != nil || env.Sidechain {
			continue
		}
		switch env.Type {
		case "attachment":
			if env.Attachment.Type == "queued_command" && match(env.Attachment.Prompt) {
				start(strings.TrimSpace(env.Attachment.Prompt), env.Timestamp)
			}
		case "user":
			switch classifyClaudeUserRecord(env.PromptSource, env.Meta, env.Compact, env.Visible, env.Message) {
			case claudeUserPrompt:
				if prompt := claudeMessageText(env.Message); match(prompt) {
					start(prompt, env.Timestamp)
				} else if ours && answered {
					r.Complete = true // a later turn began, so ours is over
				}
			case claudeUserInterrupt:
				if ours {
					r.Complete = true
				}
			}
		case "assistant":
			if !ours || r.Complete {
				continue
			}
			answered = true
			var msg struct {
				Stop    string `json:"stop_reason"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(env.Message, &msg) != nil {
				continue
			}
			for _, b := range msg.Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					parts = append(parts, strings.TrimSpace(b.Text))
				}
			}
			r.Complete = msg.Stop == "end_turn" || msg.Stop == "stop_sequence"
		}
	}
	r.Reply = strings.Join(parts, "\n\n")
	return r
}

func claudeMessageText(raw json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			out = append(out, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(out, "\n")
}

// codexInjectedUserText reports the user-role messages Codex adds itself
// (AGENTS.md, environment context), which are not the human's prompt.
func codexInjectedUserText(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "<environment_context>") ||
		strings.HasPrefix(t, "<user_instructions>") ||
		strings.HasPrefix(t, "# AGENTS.md instructions") ||
		strings.HasPrefix(t, "<permissions") ||
		strings.HasPrefix(t, "<turn_aborted>")
}

// codexLatestReply projects a Codex rollout tail.
func codexLatestReply(buf []byte) paneReply {
	r := paneReply{Agent: agentCodex, Source: "transcript"}
	var parts []string
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Last    string `json:"last_agent_message"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch {
		case env.Type == "response_item" && env.Payload.Type == "message" && env.Payload.Role == "user":
			var texts []string
			for _, c := range env.Payload.Content {
				if strings.TrimSpace(c.Text) != "" && !codexInjectedUserText(c.Text) {
					texts = append(texts, strings.TrimSpace(c.Text))
				}
			}
			if len(texts) == 0 {
				continue
			}
			r.Prompt = strings.Join(texts, "\n")
			r.PromptAt = parseTranscriptTime(env.Timestamp)
			r.PromptSeen = true
			r.Complete = false
			r.CompletedAt = time.Time{}
			parts = parts[:0]
		case env.Type == "response_item" && env.Payload.Type == "message" && env.Payload.Role == "assistant":
			for _, c := range env.Payload.Content {
				if strings.TrimSpace(c.Text) != "" {
					parts = append(parts, strings.TrimSpace(c.Text))
				}
			}
		case env.Type == "event_msg" && env.Payload.Type == "task_complete":
			r.Complete = true
			r.CompletedAt = parseTranscriptTime(env.Timestamp)
			if last := strings.TrimSpace(env.Payload.Last); last != "" {
				// The final message is the answer; earlier progress notes stay
				// only when the final one is missing.
				parts = []string{last}
			}
		case env.Type == "event_msg" && env.Payload.Type == "turn_aborted":
			r.Complete = true
			r.CompletedAt = time.Time{}
		}
	}
	r.Reply = strings.Join(parts, "\n\n")
	return r
}

// codexReplyTo finds, in a Codex rollout tail, the newest turn that ran a
// user message satisfying match, and that turn's reply. Unlike
// codexLatestReply it still finds the turn after later turns have run: a
// queued message's answer is followed by the turns queued behind it. A turn
// runs to its task_complete (or turn_aborted); user messages before that are
// steers into the same turn.
func codexReplyTo(buf []byte, match func(prompt string) bool) paneReply {
	r := paneReply{Agent: agentCodex, Source: "transcript"}
	var parts []string
	open, ours := false, false
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Last    string `json:"last_agent_message"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch {
		case env.Type == "response_item" && env.Payload.Type == "message" && env.Payload.Role == "user":
			var texts []string
			for _, c := range env.Payload.Content {
				if strings.TrimSpace(c.Text) != "" && !codexInjectedUserText(c.Text) {
					texts = append(texts, strings.TrimSpace(c.Text))
				}
			}
			if len(texts) == 0 {
				continue
			}
			if !open {
				open, ours = true, false
			}
			if prompt := strings.Join(texts, "\n"); !ours && match(prompt) {
				ours = true
				r = paneReply{Agent: agentCodex, Source: "transcript", Prompt: prompt, PromptAt: parseTranscriptTime(env.Timestamp), PromptSeen: true}
				parts = parts[:0]
			}
		case env.Type == "response_item" && env.Payload.Type == "message" && env.Payload.Role == "assistant":
			if ours {
				for _, c := range env.Payload.Content {
					if strings.TrimSpace(c.Text) != "" {
						parts = append(parts, strings.TrimSpace(c.Text))
					}
				}
			}
		case env.Type == "event_msg" && (env.Payload.Type == "task_complete" || env.Payload.Type == "turn_aborted"):
			if ours {
				r.Complete = true
				if last := strings.TrimSpace(env.Payload.Last); last != "" {
					parts = []string{last}
				}
				r.Reply = strings.Join(parts, "\n\n")
			}
			open, ours = false, false
		}
	}
	if ours {
		r.Reply = strings.Join(parts, "\n\n")
	}
	return r
}

func parseTranscriptTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

// codexRolloutPathCache remembers thread id → rollout file, so polling does
// not walk the sessions tree every half second.
var codexRolloutPathCache sync.Map

func codexRolloutPath(threadID string) string {
	id, ok := canonicalCodexThreadID(threadID)
	if !ok {
		return ""
	}
	if v, ok := codexRolloutPathCache.Load(id); ok {
		if p, _ := v.(string); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		codexRolloutPathCache.Delete(id)
	}
	for _, root := range codexSessionsRoots() {
		found := ""
		visited := 0
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fs.SkipDir
			}
			visited++
			if visited > codexRolloutWalkCap {
				return fs.SkipAll
			}
			if d.IsDir() {
				return nil
			}
			if fid, ok := rolloutFilenameThreadID(d.Name()); ok && fid == id {
				found = path
				return fs.SkipAll
			}
			return nil
		})
		if found != "" {
			codexRolloutPathCache.Store(id, found)
			return found
		}
	}
	return ""
}

var screenBlankRunRE = regexp.MustCompile(`\n{3,}`)

// paneScreenText is the pane's current rendered screen, trimmed and redacted.
func paneScreenText(p *Pane) string {
	if snap, ok := p.ScreenSnapshot(); ok {
		if text, ok := snapshotVisibleText(snap); ok {
			text = strings.Trim(text, "\n")
			return redactTerminalTail(screenBlankRunRE.ReplaceAllString(text, "\n\n"))
		}
	}
	return redactTerminalTail(tailOfPane(p, 60))
}

// paneLatestReply returns the pane's latest agent reply, falling back to the
// rendered screen.
func (a *apiServer) paneLatestReply(p *Pane) paneReply {
	snap := p.Snapshot()
	agent := paneSummaryAgent(p, snap)
	switch agent {
	case agentClaude:
		if path := a.transcriptForPane(p); path != "" && claudeTranscriptBelongsToPane(path, snap) {
			if buf, err := tailReadBounded(path, paneReplyTailBytes); err == nil {
				r := claudeLatestReply(buf)
				if r.PromptSeen {
					r.Path = path
					r.Reply = redactTerminalTail(r.Reply)
					return r
				}
			}
		}
	case agentCodex:
		if path := codexRolloutPath(snap.CodexThreadID); path != "" {
			if buf, err := tailReadBounded(path, paneReplyTailBytes); err == nil {
				r := codexLatestReply(buf)
				if r.PromptSeen {
					r.Path = path
					r.Reply = redactTerminalTail(r.Reply)
					return r
				}
			}
		}
	}
	return paneReply{Agent: agent, Source: "screen", Reply: paneScreenText(p)}
}

// paneReplyTo is paneLatestReply for an ask_pane whose message a working
// Codex or Claude queued: the reply is what followed that message, found even
// after the turns queued behind it have run. Until the message shows up in
// the rollout or transcript the reply is an empty transcript reply, so the
// wait goes on.
func (a *apiServer) paneReplyTo(p *Pane, base paneTurnBaseline) paneReply {
	if base.Queued == "" {
		return a.paneLatestReply(p)
	}
	snap := p.Snapshot()
	match := func(prompt string) bool { return queuedPromptMatches(prompt, base.Queued) }
	switch paneSummaryAgent(p, snap) {
	case agentClaude:
		if path := a.transcriptForPane(p); path != "" && claudeTranscriptBelongsToPane(path, snap) {
			if buf, err := tailReadBounded(path, paneReplyTailBytes); err == nil {
				r := claudeReplyTo(buf, match)
				r.Path = path
				r.Reply = redactTerminalTail(r.Reply)
				return r
			}
		}
	case agentCodex:
		if path := codexRolloutPath(snap.CodexThreadID); path != "" {
			if buf, err := tailReadBounded(path, paneReplyTailBytes); err == nil {
				r := codexReplyTo(buf, match)
				r.Path = path
				r.Reply = redactTerminalTail(r.Reply)
				return r
			}
		}
	}
	return a.paneLatestReply(p)
}

// codexRolloutFresh bounds how stale a rollout may be for its open turn to
// count: a Codex killed mid-turn never writes the turn's end.
const codexRolloutFresh = 10 * time.Minute

// codexRolloutTurnOpen reports that the pane's Codex rollout has a turn that
// started and has not ended (no task_complete or turn_aborted yet), and was
// written recently. It sees a turn while its answer streams, when the
// Working row is not drawn.
func codexRolloutTurnOpen(p *Pane) bool {
	snap := p.Snapshot()
	path := codexRolloutPath(snap.CodexThreadID)
	if path == "" {
		return false
	}
	if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) > codexRolloutFresh {
		return false
	}
	buf, err := tailReadBounded(path, paneReplyTailBytes)
	if err != nil {
		return false
	}
	r := codexLatestReply(buf)
	return r.PromptSeen && !r.Complete
}

// claudeTranscriptBelongsToPane rejects a transcript that transcriptForPane
// only guessed from the folder when it was last written before the pane
// started. A fresh Claude pane has no transcript until its first prompt, and
// the guess would otherwise hand back an older session's reply as if this
// pane had said it.
func claudeTranscriptBelongsToPane(path string, snap PaneSnapshot) bool {
	if path == strings.TrimSpace(snap.ClaudeTranscriptPath) {
		return true
	}
	if snap.ClaudeSessionID != "" && sessionIDForTranscript(path) == snap.ClaudeSessionID {
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return snap.CreatedAt.IsZero() || !fi.ModTime().Before(snap.CreatedAt)
}

// formatPaneReply renders a reply for read_pane_tail's reply mode.
func formatPaneReply(label string, r paneReply, state PaneRuntimeState) string {
	status := "finished"
	if !r.Complete {
		status = "still working"
	}
	if state == StateChoicePrompt {
		status = "waiting on an approval menu"
	}
	var b strings.Builder
	b.WriteString(label)
	b.WriteString(" · ")
	b.WriteString(status)
	b.WriteString("\n")
	if r.Prompt != "" {
		prompt := r.Prompt
		if len(prompt) > 400 {
			prompt = prompt[:400] + "…"
		}
		b.WriteString("Asked: ")
		b.WriteString(prompt)
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(r.Reply) == "" {
		b.WriteString("(no reply yet)")
	} else {
		b.WriteString(r.Reply)
	}
	return b.String()
}
