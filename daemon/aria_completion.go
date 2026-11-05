package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Only exact compiled-in instructions and tool definitions sit outside the
// smart-approval input budget. Everything else, including future fields and
// tool-loop history, shares that budget. These checks recognize secret shapes;
// they cannot identify every secret.
const ariaCompletionStaticMaxBytes = 32 * 1024

var errAriaCompletionInput = errors.New("This request needs your review.")

// sendCompletion is the single outbound boundary for /api/aria/complete.
// Validate the final serialized body before constructing or sending a request.
// A refusal contains no input, model response, or upstream error-body text.
func (a *Aria) sendCompletion(ctx context.Context, payload []byte) (*http.Response, error) {
	if !teamCloudLLMAllowed() || !ariaCompletionInputAllowed(payload) {
		return nil, errAriaCompletionInput
	}
	cloudBase, cloudTok := a.cloudCreds()
	req, err := http.NewRequestWithContext(ctx, "POST", cloudBase+"/api/aria/complete", bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("could not construct completion request")
	}
	req.Header.Set("Authorization", "Bearer "+cloudTok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := ariaClient.Load().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, errors.New("completion transport timed out")
		}
		return nil, errors.New("completion transport failed")
	}
	return resp, nil
}

func ariaCompletionInputAllowed(payload []byte) bool {
	if len(payload) > ariaCompletionStaticMaxBytes+ariaJevMaxTailBytes {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil || fields["messages"] == nil {
		return false
	}
	// Scan the serialized shape as well as decoded strings: JSON escapes must
	// not hide keyed credentials or tokens split across terminal rows.
	if completionSensitiveJSON(payload) {
		return false
	}
	staticBytes := 0
	for _, name := range []string{"system", "tools"} {
		if raw, ok := fields[name]; ok {
			if !compiledCompletionField(name, raw) {
				// Unrecognized fields are still checked and charged in full.
				continue
			}
			staticBytes += len(raw)
			delete(fields, name)
		}
	}
	staticBytes += compiledCompletionMessageBytes(fields["messages"])
	// Count actual final bytes, including whitespace and separators. Compacting
	// the variable fields here would undercount a later RawMessage caller.
	return staticBytes <= ariaCompletionStaticMaxBytes && len(payload)-staticBytes <= ariaJevMaxTailBytes
}

// Only exact, substantial compiled literals are excluded from message text.
// Short separators and every variable byte remain charged. Matching happens
// after the complete secret-shape scan and exclusions share the fixed cap.
var completionCompiledParts = func() []string {
	parts := map[string]bool{ariaAutoModeCautiousInstruction: true, ariaNativeAutoModeCautiousInstruction: true}
	verbs := regexp.MustCompile(`%[qsd]`)
	for _, template := range []string{ariaAutoObservationTemplate, ariaNativeObservationTemplate, ariaDryRunObservationTemplate} {
		for _, part := range verbs.Split(template, -1) {
			if len(part) >= 64 {
				parts[part] = true
			}
		}
	}
	out := make([]string, 0, len(parts))
	for part := range parts {
		out = append(out, part)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}()

func compiledCompletionMessageBytes(raw json.RawMessage) int {
	var messages []struct {
		Content []struct {
			Type string
			Text json.RawMessage
		}
	}
	if json.Unmarshal(raw, &messages) != nil {
		return 0
	}
	total := 0
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type != "text" {
				continue
			}
			var remaining string
			if json.Unmarshal(block.Text, &remaining) != nil {
				continue
			}
			encodedText, err := json.Marshal(remaining)
			if err != nil || !bytes.Equal(encodedText, block.Text) {
				continue
			}
			for _, part := range completionCompiledParts {
				if count := strings.Count(remaining, part); count > 0 {
					encoded, _ := json.Marshal(part)
					total += count * (len(encoded) - 2)
					remaining = strings.ReplaceAll(remaining, part, strings.Repeat("\x00", len(part)))
				}
			}
		}
	}
	return total
}

type completionRefusalMemo struct {
	pos            int64
	hash           uint64
	policy         ariaAutoModePolicy
	operator       bool
	nativeRequests map[string]bool // pending request IDs only; never input text
}

func completionPromptIdentity(p *Pane) (int64, uint64) {
	_, pos, hash, _ := currentChoicePromptIdentity(p, "")
	return pos, hash
}

func (a *Aria) rememberCompletionRefusal(p *Pane, policy ariaAutoModePolicy, operator bool, pos int64, hash uint64) {
	if pos == 0 && hash == 0 {
		return // no live terminal prompt was captured
	}
	memo := completionRefusalMemo{pos: pos, hash: hash, policy: policy, operator: operator}
	a.mu.Lock()
	if a.completionRefusals == nil {
		a.completionRefusals = map[string]completionRefusalMemo{}
	}
	a.completionRefusals[p.ID] = memo
	a.mu.Unlock()
}

func (a *Aria) rememberNativeCompletionRefusal(p *Pane, policy ariaAutoModePolicy, operator bool, requestID string) {
	pending := p.listNativePending()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.completionRefusals == nil {
		a.completionRefusals = map[string]completionRefusalMemo{}
	}
	previous := a.completionRefusals[p.ID]
	requests := map[string]bool{}
	for _, ap := range pending {
		if ap.RequestID == requestID || (previous.policy == policy && previous.operator == operator && previous.nativeRequests[ap.RequestID]) {
			requests[ap.RequestID] = true
		}
	}
	// Rebuild from the live set so answered requests do not accumulate.
	a.completionRefusals[p.ID] = completionRefusalMemo{policy: policy, operator: operator, nativeRequests: requests}
}

func (a *Aria) nativeCompletionRefused(p *Pane, requestID string) bool {
	if a == nil || p == nil || a.cautiousJevOnly() || p.peekNativePending(requestID) == nil {
		return false
	}
	a.mu.Lock()
	memo, ok := a.completionRefusals[p.ID]
	refused := memo.nativeRequests[requestID]
	a.mu.Unlock()
	return ok && refused && memo.policy.matches(p) && memo.operator == teamCloudLLMAllowed()
}

func (a *Aria) completionRefused(p *Pane) bool {
	if a == nil || p == nil {
		return false
	}
	if a.cautiousJevOnly() {
		return false
	}
	if p.Snapshot().Kind == "native" {
		pending := p.listNativePending()
		if len(pending) == 0 {
			return false
		}
		for _, ap := range pending {
			if !a.nativeCompletionRefused(p, ap.RequestID) {
				return false // a sibling, including a transient failure, can retry
			}
		}
		return true
	}
	a.mu.Lock()
	memo, ok := a.completionRefusals[p.ID]
	a.mu.Unlock()
	if !ok {
		return false
	}
	pos, hash := completionPromptIdentity(p)
	if pos == 0 && hash == 0 {
		return false
	}
	return ok && memo.pos == pos && memo.hash == hash && memo.policy.matches(p) && memo.operator == teamCloudLLMAllowed()
}

func compiledCompletionField(name string, raw json.RawMessage) bool {
	var candidates []any
	switch name {
	case "system":
		candidates = []any{ariaSystemPrompt, []map[string]any{
			{"type": "text", "text": ariaSystemPrompt, "cache_control": map[string]any{"type": "ephemeral"}},
		}}
	case "tools":
		candidates = []any{ariaAutonomousTools(), ariaNativeAutonomousTools(),
			ariaToolsWithCacheBreakpoint(ariaAutonomousTools()), ariaToolsWithCacheBreakpoint(ariaNativeAutonomousTools())}
	}
	for _, candidate := range candidates {
		encoded, err := json.Marshal(candidate)
		if err == nil && bytes.Equal(raw, encoded) {
			return true
		}
	}
	return false
}

// Native arguments are checked in their structured form before formatting.
// The final serialized completion still passes the same outbound boundary.
func completionStructuredInputAllowed(raw json.RawMessage) bool {
	if len(raw) > ariaJevMaxTailBytes {
		return false
	}
	return !completionSensitiveJSON(raw)
}

type completionSecretScan struct {
	remainingParses int
}

func completionSensitiveJSON(raw []byte) bool {
	if !json.Valid(raw) || cautiousSensitiveTail(string(raw)) {
		return true
	}
	scan := completionSecretScan{remainingParses: 1024}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	refused, err := scan.value(decoder, 0, "")
	return err != nil || refused
}

// Tokens preserve every object member, including duplicate keys. Decoding to
// a map first would discard an earlier credential hidden by a later safe value.
func (s *completionSecretScan) value(decoder *json.Decoder, depth int, key string) (bool, error) {
	if depth > 64 {
		return true, nil // bounded check failure, never permission to send
	}
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	if key != "" {
		childRaw, err := json.Marshal(token)
		pair, pairErr := json.Marshal(map[string]any{key: token})
		if err != nil || pairErr != nil || cautiousSensitiveTail(key+"="+string(childRaw)) || cautiousSensitiveTail(string(pair)) {
			return true, nil
		}
	}
	switch v := token.(type) {
	case string:
		return s.text(v, depth+1), nil
	case json.Delim:
		switch v {
		case '{':
			for decoder.More() {
				member, err := decoder.Token()
				if err != nil {
					return false, err
				}
				name, ok := member.(string)
				if !ok {
					return false, errAriaCompletionInput
				}
				if s.text(name, depth+1) {
					return true, nil
				}
				if refused, err := s.value(decoder, depth+1, name); refused || err != nil {
					return refused, err
				}
			}
		case '[':
			for decoder.More() {
				if refused, err := s.value(decoder, depth+1, ""); refused || err != nil {
					return refused, err
				}
			}
		default:
			return false, errAriaCompletionInput
		}
		end, err := decoder.Token()
		if err != nil {
			return false, err
		}
		if (v == '{' && end != json.Delim('}')) || (v == '[' && end != json.Delim(']')) {
			return false, errAriaCompletionInput
		}
	}
	return false, nil
}

func (s *completionSecretScan) text(text string, depth int) bool {
	if depth > 64 || cautiousSensitiveTail(text) {
		return true
	}
	// Formatted observations can contain JSON objects or string literals,
	// including tool arguments whose content is itself serialized JSON.
	for i := 0; i < len(text); i++ {
		if text[i] != '{' && text[i] != '[' && text[i] != '"' {
			continue
		}
		if s.remainingParses == 0 {
			return true
		}
		s.remainingParses--
		decoder := json.NewDecoder(strings.NewReader(text[i:]))
		decoder.UseNumber()
		refused, err := s.value(decoder, depth+1, "")
		if refused {
			return true
		}
		if err != nil {
			continue // ordinary prose/code can contain non-JSON braces
		}
		i += int(decoder.InputOffset()) - 1
	}
	return false
}
