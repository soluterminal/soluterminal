package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestHelperUsageClaudeProjectionAndPartial(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-11-20T00:00:00Z","sessionId":"session","requestId":"request","cwd":"SECRET_PATH","gitBranch":"SECRET_BRANCH","message":{"id":"message","model":"claude-test","content":"SECRET_CONTENT","stop_reason":"SECRET_STOP","usage":{"input_tokens":10,"output_tokens":3}}}` + "\n"
	var d helperUsageDecoder
	recs, consumed, err := d.decode("claude", []byte(line+`{"partial":`))
	if err != nil || consumed != len(line) || len(recs) != 1 || recs[0].TotalTokens != 13 {
		t.Fatalf("decode: records=%d consumed=%d err=%v", len(recs), consumed, err)
	}
	b, _ := json.Marshal(recs)
	if strings.Contains(string(b), "SECRET_") {
		t.Fatal("transcript content/path escaped metadata projection")
	}
	if recs[0].MessageID != "message" || recs[0].RequestID != "request" {
		t.Fatal("dedup identity lost")
	}
}

func TestHelperUsageCodexIncrementalContext(t *testing.T) {
	var d helperUsageDecoder
	meta := `{"type":"session_meta","payload":{"id":"019c57b9-2965-7f91-8dbe-6255fd827c09","cwd":"SECRET_PATH","creator_account_id":"SECRET_ACCOUNT","git":{"branch":"SECRET_BRANCH","repository_url":"https://example.com/SECRET_REMOTE.git"}}}` + "\n" + `{"type":"turn_context","payload":{"model":"gpt-test"}}` + "\n"
	if recs, n, err := d.decode("codex", []byte(meta)); err != nil || len(recs) != 0 || n != len(meta) {
		t.Fatalf("context: n=%d err=%v", n, err)
	}
	line := `{"timestamp":"2026-11-20T00:00:00Z","type":"token_usage_record","payload":{"response_id":"resp_test","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3,"total_tokens":13}}}` + "\n"
	recs, n, err := d.decode("codex", []byte(line))
	if err != nil || n != len(line) || len(recs) != 1 {
		t.Fatalf("usage: records=%d n=%d err=%v", len(recs), n, err)
	}
	if recs[0].Model != "gpt-test" || recs[0].Provider != "codex" || recs[0].InputTokens != 8 || recs[0].CacheReadTokens != 2 {
		t.Fatalf("wrong normalization: %+v", recs[0])
	}
	b, _ := json.Marshal(recs)
	if strings.Contains(string(b), "SECRET_") {
		t.Fatal("Codex private context escaped projection")
	}
}

func TestHelperUsageLimitsAndUnsupported(t *testing.T) {
	var d helperUsageDecoder
	if _, n, err := d.decode("claude", make([]byte, helperUsageReadLimit+1)); !errors.Is(err, errHelperUsageLimit) || n != 0 {
		t.Fatalf("limit: n=%d err=%v", n, err)
	}
	if _, n, err := d.decode("claude", make([]byte, helperUsageReadLimit)); !errors.Is(err, errHelperUsageLimit) || n != 0 {
		t.Fatalf("full unterminated line: n=%d err=%v", n, err)
	}
	if _, _, err := d.decode("other", nil); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestHelperCredentialMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body, state, kind string
		refresh                           bool
	}{
		{"claude", "claude", `{"claudeAiOauth":{"accessToken":"SECRET_ACCESS","refreshToken":"SECRET_REFRESH","subscriptionType":"SECRET_PLAN"}}`, "present", "oauth", true},
		{"claude-no-refresh", "claude", `{"claudeAiOauth":{"accessToken":"SECRET_ACCESS"}}`, "present", "oauth", false},
		{"codex", "codex", `{"tokens":{"access_token":"SECRET_ACCESS","refresh_token":"SECRET_REFRESH","account_id":"SECRET_ACCOUNT"}}`, "present", "oauth", true},
		{"codex-with-exchanged-key", "codex", `{"OPENAI_API_KEY":"SECRET_OLD_KEY","tokens":{"access_token":"SECRET_ACCESS","refresh_token":"SECRET_REFRESH"}}`, "present", "oauth", true},
		{"api", "codex", `{"OPENAI_API_KEY":"SECRET_KEY"}`, "present", "api_key", false},
		{"malformed", "claude", `{"claudeAiOauth":`, "invalid", "none", false},
		{"null", "codex", `null`, "invalid", "none", false},
		{"empty", "codex", `{}`, "invalid", "none", false},
		{"wrong-type", "claude", `{"claudeAiOauth":{"accessToken":123}}`, "invalid", "none", false},
		{"oversize", "codex", strings.Repeat(" ", helperCredentialReadLimit+1), "too_large", "none", false},
		{"unknown", "other", `{}`, "unsupported", "none", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readHelperCredentialMetadata(tc.provider, strings.NewReader(tc.body))
			if got.State != tc.state || got.Kind != tc.kind || got.HasRefresh != tc.refresh || got.Present != (tc.state == "present") || got.LastErrorClass != "unknown" {
				t.Fatalf("metadata: %+v", got)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "SECRET_") {
				t.Fatal("credential value escaped projection")
			}
		})
	}
}
