package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ariaUsage is the token-accounting snapshot we keep per conversation
// so the UI can render a "context used" pill — same idea as Claude
// Code's status line. InputTokens is the prompt size for the most
// recent API call (which already includes the entire conversation
// history we sent), so it's the closest proxy to "current context
// size". The next turn's prompt will be roughly this plus the new
// user message + this turn's assistant reply.
type ariaUsage struct {
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	ContextWindow            int    `json:"context_window"`
	Model                    string `json:"model"`
	UpdatedAt                int64  `json:"updated_at"`
}

// ariaUsageKindFor classifies a conversation id into the kind label
// the cloud's aria_usage table uses. Conversations created by
// aria_auto.go for autonomous evaluations are prefixed "auto_pane_";
// everything else is a manual chat turn. Keeps the daemon as the
// single source of truth for the classification — the cloud just
// stores whatever string we send.
func ariaUsageKindFor(convID string) string {
	if strings.HasPrefix(convID, "auto_pane_") {
		return "auto-eval"
	}
	return "chat"
}

// reportUsageToCloud POSTs a single completion's token usage to the
// cloud's aria_usage log. Called once per tool-loop iteration. Runs in
// its own goroutine — synchronous failure modes (cloud down, DB down,
// network blip) must never delay the user-facing turn or its emitted
// events. Errors are logged at low volume so they don't drown out
// real signal.
func (a *Aria) reportUsageToCloud(kind, model string, u ariaUsage) {
	body := map[string]any{
		"kind":                kind,
		"model":               model,
		"input_tokens":        u.InputTokens,
		"output_tokens":       u.OutputTokens,
		"cache_read_tokens":   u.CacheReadInputTokens,
		"cache_create_tokens": u.CacheCreationInputTokens,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		log.Printf("aria-usage: marshal failed: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cloudBase, cloudTok := a.cloudCreds()
	req, err := http.NewRequestWithContext(ctx, "POST", cloudBase+"/api/aria/usage", bytes.NewReader(payload))
	if err != nil {
		log.Printf("aria-usage: build request failed: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cloudTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ariaClient.Load().Do(req)
	if err != nil {
		log.Printf("aria-usage: post failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("aria-usage: cloud returned %d", resp.StatusCode)
	}
}

// consumeStream reads the Anthropic SSE response, accumulates each
// content block, emits text deltas to the caller as they arrive, and
// returns the assembled assistant blocks, the final stop_reason, and
// the per-call usage snapshot (input + output tokens for THIS request).
//
// Event format reference:
//
//	message_start         — carries the model and the prompt's input_tokens
//	content_block_start   — block opens with type=text or tool_use
//	content_block_delta   — text_delta for text blocks, input_json_delta for tool_use
//	content_block_stop    — block closes; for tool_use we parse accumulated JSON
//	message_delta         — carries stop_reason + final output_tokens
//	message_stop          — end of stream
func (a *Aria) consumeStream(r io.Reader, emit func(map[string]any)) ([]ariaBlock, string, ariaUsage, error) {
	scanner := bufio.NewScanner(r)
	// 1MB max line — Anthropic delta events can be wide when the
	// model emits a big code block in one delta. Default 64KB has
	// triggered truncated events in practice on voice responses.
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	type blockState struct {
		kind      string // "text" | "tool_use" | "thinking" | "redacted_thinking"
		text      strings.Builder
		toolID    string
		toolName  string
		toolInput strings.Builder
		// thinking blocks accumulate reasoning text + a signature; both
		// must round-trip in history or DeepSeek (and Anthropic with
		// extended thinking) reject the next turn with a 400.
		thinking  strings.Builder
		signature string
		// redacted_thinking arrives as one opaque blob on
		// content_block_start with no deltas; just stash it.
		data string
	}
	blocks := map[int]*blockState{}
	order := []int{}
	stopReason := ""
	usage := ariaUsage{}
	sawTerminalEvent := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			sawTerminalEvent = true
			break
		}
		var evt struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock *struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				Thinking  string          `json:"thinking"`
				Signature string          `json:"signature"`
				Data      string          `json:"data"`
			} `json:"content_block,omitempty"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			} `json:"delta,omitempty"`
			// message_start carries usage on .message.usage; message_delta
			// carries the final output_tokens on a top-level .usage. Both
			// are optional on every other event, so json.Unmarshal silently
			// leaves them zero — we only read them on the right type.
			Message *struct {
				Model string `json:"model"`
				Usage *struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message,omitempty"`
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			log.Printf("aria: bad SSE payload bytes=%d", len(data))
			continue
		}
		switch evt.Type {
		case "message_start":
			if evt.Message != nil {
				if evt.Message.Model != "" {
					usage.Model = evt.Message.Model
				}
				if evt.Message.Usage != nil {
					usage.InputTokens = evt.Message.Usage.InputTokens
					usage.CacheReadInputTokens = evt.Message.Usage.CacheReadInputTokens
					usage.CacheCreationInputTokens = evt.Message.Usage.CacheCreationInputTokens
					// Output tokens at message_start is just the seed (~1-3);
					// the authoritative number arrives on message_delta.
					usage.OutputTokens = evt.Message.Usage.OutputTokens
				}
			}
		case "content_block_start":
			if evt.ContentBlock == nil {
				continue
			}
			bs := &blockState{kind: evt.ContentBlock.Type}
			switch bs.kind {
			case "tool_use":
				bs.toolID = evt.ContentBlock.ID
				bs.toolName = evt.ContentBlock.Name
			case "redacted_thinking":
				// Single-shot: the entire opaque blob arrives here.
				bs.data = evt.ContentBlock.Data
			case "thinking":
				// May arrive with seed text/signature; deltas fill the rest.
				if evt.ContentBlock.Thinking != "" {
					bs.thinking.WriteString(evt.ContentBlock.Thinking)
				}
				if evt.ContentBlock.Signature != "" {
					bs.signature = evt.ContentBlock.Signature
				}
			}
			blocks[evt.Index] = bs
			order = append(order, evt.Index)
		case "content_block_delta":
			bs, ok := blocks[evt.Index]
			if !ok || evt.Delta == nil {
				continue
			}
			switch evt.Delta.Type {
			case "text_delta":
				bs.text.WriteString(evt.Delta.Text)
				emit(map[string]any{"type": "delta", "text": evt.Delta.Text})
			case "input_json_delta":
				bs.toolInput.WriteString(evt.Delta.PartialJSON)
			case "thinking_delta":
				bs.thinking.WriteString(evt.Delta.Thinking)
			case "signature_delta":
				// Last one wins — the API only sends a single signature
				// per thinking block, but we accumulate defensively.
				bs.signature = evt.Delta.Signature
			}
		case "content_block_stop":
			// Nothing to do here — final tool input is parsed when we
			// build the output slice.
		case "message_delta":
			if evt.Delta != nil && evt.Delta.StopReason != "" {
				stopReason = evt.Delta.StopReason
			}
			if evt.Usage != nil {
				usage.OutputTokens = evt.Usage.OutputTokens
			}
		case "message_stop":
			sawTerminalEvent = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", ariaUsage{}, err
	}
	if !sawTerminalEvent {
		return nil, "", ariaUsage{}, errors.New("stream ended before message_stop")
	}

	// Rebuild in index order so block ordering matches what the model
	// produced — Anthropic's API requires matching tool_result ids on
	// the next turn, AND thinking blocks must precede the text/tool_use
	// they reasoned toward (provider rejects out-of-order history).
	out := make([]ariaBlock, 0, len(order))
	for _, idx := range order {
		bs := blocks[idx]
		switch bs.kind {
		case "text":
			t := bs.text.String()
			if t == "" {
				continue
			}
			out = append(out, ariaBlock{Type: "text", Text: t})
		case "tool_use":
			// Empty input JSON means the tool takes no args. Anthropic
			// still sends a content_block_start but no deltas.
			raw := bs.toolInput.String()
			if raw == "" {
				raw = "{}"
			}
			out = append(out, ariaBlock{
				Type:  "tool_use",
				ID:    bs.toolID,
				Name:  bs.toolName,
				Input: json.RawMessage(raw),
			})
		case "thinking":
			t := bs.thinking.String()
			if t == "" && bs.signature == "" {
				continue
			}
			out = append(out, ariaBlock{
				Type:      "thinking",
				Thinking:  t,
				Signature: bs.signature,
			})
		case "redacted_thinking":
			if bs.data == "" {
				continue
			}
			out = append(out, ariaBlock{
				Type: "redacted_thinking",
				Data: bs.data,
			})
		}
	}
	return out, stopReason, usage, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
