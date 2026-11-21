package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"time"
)

// These reducers are helper-local preparation for M4. They do not register a
// scan stream or define its wire format; that extension requires its own ruling.
const helperUsageReadLimit = 8 << 20

var errHelperUsageLimit = errors.New("helper usage input exceeds limit")

// helperUsageDecoder carries Codex's per-file context between complete-line
// chunks. Allocate one per transcript, and reset it when that file is replaced
// or truncated. On success the caller retains the unconsumed suffix. A limit
// error is a terminal overflow outcome: surface it or skip the oversized line
// with a bounded source reader; never retry the same bytes or grow that suffix.
type helperUsageDecoder struct {
	codex codexFileState
}

func (d *helperUsageDecoder) decode(provider string, buf []byte) ([]ccUsageRecord, int, error) {
	if len(buf) > helperUsageReadLimit || (len(buf) == helperUsageReadLimit && bytes.IndexByte(buf, '\n') < 0) {
		return nil, 0, errHelperUsageLimit
	}
	var recs []ccUsageRecord
	var consumed int
	switch provider {
	case "claude":
		recs, consumed = extractUsageLines(buf)
	case "codex":
		recs, consumed = extractCodexUsage(buf, &d.codex)
	default:
		return nil, 0, errors.New("unsupported helper usage provider")
	}
	for i := range recs {
		recs[i] = helperUsageMetadata(recs[i])
	}
	return recs, consumed, nil
}

// Reconstruct rather than embed the parser result: transcript additions must
// not silently acquire permission to leave Linux. Paths, repository URLs,
// account/user identities, arbitrary stop reasons and workflow tags stay local.
// Metadata strings are still transcript-supplied; the eventual wire handler
// must enforce its agreed length/charset limits. This internal projection does
// not define the pending scan response schema.
func helperUsageMetadata(r ccUsageRecord) ccUsageRecord {
	return ccUsageRecord{
		EventTS: r.EventTS, SessionID: r.SessionID, MessageID: r.MessageID,
		RequestID: r.RequestID, Model: r.Model, Provider: r.Provider,
		IsSidechain: r.IsSidechain,
		InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
		CacheReadTokens: r.CacheReadTokens, CacheCreateTokens: r.CacheCreateTokens,
		CacheCreate5mTokens: r.CacheCreate5mTokens, CacheCreate1hTokens: r.CacheCreate1hTokens,
		TotalTokens: r.TotalTokens, ReasoningOutputTokens: r.ReasoningOutputTokens,
		ServiceTier: r.ServiceTier, Speed: r.Speed, CCVersion: r.CCVersion,
		WebSearchRequests: r.WebSearchRequests, WebFetchRequests: r.WebFetchRequests,
	}
}

// helperCredentialMetadata reports file contents, not provider verification.
// State/kind/error strings are constants; expiry is Claude's explicit timestamp
// normalized to RFC 3339 UTC. No token, account id, plan, fingerprint or raw
// error is retained. This reducer never opens a store or verifies a login.
// Ruling 8.6b: Codex tokens are never decoded; lastErrorClass is always unknown.
type helperCredentialMetadata struct {
	State          string
	Kind           string
	HasRefresh     bool
	Present        bool
	ExpiresAt      string
	LastErrorClass string
}

func helperCredentialResult(state, kind string, refresh bool) helperCredentialMetadata {
	return helperCredentialMetadata{
		State: state, Kind: kind, HasRefresh: refresh,
		Present: state == "present", LastErrorClass: "unknown",
	}
}

const helperCredentialReadLimit = 64 << 10

func readHelperCredentialMetadata(provider string, r io.Reader) helperCredentialMetadata {
	if provider != "claude" && provider != "codex" {
		return helperCredentialResult("unsupported", "none", false)
	}
	invalid := helperCredentialResult("invalid", "none", false)
	b, err := io.ReadAll(io.LimitReader(r, helperCredentialReadLimit+1))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return helperCredentialResult("missing", "none", false)
		}
		return helperCredentialResult("unreadable", "none", false)
	}
	if len(b) > helperCredentialReadLimit {
		return helperCredentialResult("too_large", "none", false)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return invalid
	}
	switch provider {
	case "claude":
		var c struct {
			OAuth *struct {
				Access    string `json:"accessToken"`
				Refresh   string `json:"refreshToken"`
				ExpiresAt int64  `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(b, &c) != nil || c.OAuth == nil || c.OAuth.Access == "" {
			return invalid
		}
		result := helperCredentialResult("present", "oauth", c.OAuth.Refresh != "")
		// RFC 3339 has four-digit years. Non-positive or out-of-range
		// expiry values are omitted rather than inventing an expiry time.
		if c.OAuth.ExpiresAt > 0 && c.OAuth.ExpiresAt <= 253402300799999 {
			result.ExpiresAt = time.UnixMilli(c.OAuth.ExpiresAt).UTC().Format(time.RFC3339Nano)
		}
		return result
	case "codex":
		var c struct {
			Key    string `json:"OPENAI_API_KEY"`
			Tokens *struct {
				Access  string `json:"access_token"`
				Refresh string `json:"refresh_token"`
			} `json:"tokens"`
		}
		if json.Unmarshal(b, &c) != nil {
			return invalid
		}
		// Older ChatGPT auth files can retain an exchanged API key as well
		// as tokens. Match codexAuthTokens: the token login takes priority.
		if c.Tokens != nil && c.Tokens.Access != "" {
			return helperCredentialResult("present", "oauth", c.Tokens.Refresh != "")
		}
		if c.Key != "" {
			return helperCredentialResult("present", "api_key", false)
		}
		return invalid
	default:
		return helperCredentialResult("unsupported", "none", false)
	}
}
