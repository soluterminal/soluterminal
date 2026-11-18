package main

import (
	"bytes"

	"encoding/json"

	"regexp"
	"strings"
)

// extractJSON pulls the first valid JSON object out of model output. Keep
// the complete-output fast path first, then tolerate a whole-output Markdown
// fence, and finally mine prose for a balanced object. Artifact validation is
// deliberately separate: this helper establishes JSON syntax, while the
// caller enforces the expected workflow schema.
func extractJSON(s string) []byte {
	if candidate, ok := validJSONObject([]byte(s)); ok {
		return candidate
	}
	trimmed := strings.TrimSpace(s)
	fallback := trimmed
	if unfenced, ok := stripWholeJSONFence(trimmed); ok {
		fallback = unfenced
		if candidate, valid := validJSONObject([]byte(unfenced)); valid {
			return candidate
		}
	}

	// Scan every opening brace instead of taking the first and last braces in
	// the output. That lets us continue past a balanced but syntactically
	// invalid candidate, and it prevents braces in quoted strings from ending
	// an otherwise valid object early.
	text := []byte(fallback)
	for start, b := range text {
		if b != '{' {
			continue
		}
		// Preserve compatibility with the historical {\"objective\":...}
		// shape. It is JSON string content rather than JSON itself, so ordinary
		// quote-aware brace balancing cannot identify its boundary.
		if bytes.HasPrefix(text[start:], []byte(`{\"`)) {
			for end := start + 1; end < len(text); end++ {
				if text[end] != '}' {
					continue
				}
				if decoded, ok := decodeDoubleEncoded(text[start : end+1]); ok {
					return decoded
				}
			}
		}
		if end, ok := balancedJSONObjectEnd(text, start); ok {
			if candidate, valid := validJSONObject(text[start:end]); valid {
				return candidate
			}
		}
	}
	return []byte(fallback)
}

func validJSONObject(candidate []byte) ([]byte, bool) {
	candidate = bytes.TrimSpace(candidate)
	if decoded, ok := decodeDoubleEncoded(candidate); ok {
		return decoded, true
	}
	if len(candidate) < 2 || candidate[0] != '{' || candidate[len(candidate)-1] != '}' || !json.Valid(candidate) {
		return nil, false
	}
	return candidate, true
}

// decodeDoubleEncoded recovers JSON objects emitted as JSON-string content.
// The {\" prefix is impossible in valid JSON, so this gate cannot misfire on a
// well-formed payload. Any decode or validation failure preserves the raw path.
func decodeDoubleEncoded(candidate []byte) ([]byte, bool) {
	if !bytes.HasPrefix(candidate, []byte(`{\"`)) {
		return nil, false
	}
	wrapper := make([]byte, 0, len(candidate)+2)
	wrapper = append(wrapper, '"')
	wrapper = append(wrapper, candidate...)
	wrapper = append(wrapper, '"')
	var decoded string
	if err := json.Unmarshal(wrapper, &decoded); err != nil {
		return nil, false
	}
	decoded = strings.TrimSpace(decoded)
	payload := []byte(decoded)
	if !bytes.HasPrefix(payload, []byte{'{'}) || !json.Valid(payload) {
		return nil, false
	}
	return payload, true
}

// stripWholeJSONFence recognizes only a fence that encloses the complete
// trimmed output. The opener is exactly three backticks plus an optional
// language tag; the closer is exactly three backticks on its own line.
func stripWholeJSONFence(s string) (string, bool) {
	firstNL := strings.IndexByte(s, '\n')
	lastNL := strings.LastIndexByte(s, '\n')
	if firstNL < 0 || lastNL <= firstNL {
		return "", false
	}
	opener := strings.TrimSuffix(s[:firstNL], "\r")
	if !strings.HasPrefix(opener, "```") || strings.HasPrefix(opener, "````") || strings.Contains(opener[3:], "`") {
		return "", false
	}
	if strings.TrimSpace(strings.TrimSuffix(s[lastNL+1:], "\r")) != "```" {
		return "", false
	}
	return strings.TrimSpace(s[firstNL+1 : lastNL]), true
}

// balancedJSONObjectEnd returns the exclusive end of the object beginning
// at start. Braces inside JSON strings are ignored, including after escapes.
func balancedJSONObjectEnd(text []byte, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		b := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch b {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

var previewDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
