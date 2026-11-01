package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Pure extraction logic for OpenAI Codex CLI rollout lines — the Codex
// counterpart of ccusage_extract.go. No I/O or daemon state, so it is unit
// tested over raw byte fixtures; codexusage_scan.go owns file reading,
// cursors and ledger tagging.
//
// A rollout (sessions/YYYY/MM/DD/rollout-<ts>-<thread>.jsonl) is JSONL with a
// top-level {"timestamp","type","payload"} envelope. The lines that matter:
//
//   - session_meta: thread identity, root session, cwd, originator, account,
//     subagent source, git remote. A resumed thread writes another one.
//   - turn_context: the model (and effort) for the turn that follows.
//   - event_msg/thread_settings_applied: the thread's model and service tier
//     (fast/priority is billed 2×; current CLIs carry the tier only here).
//   - token_usage_record (CLI ≥ ~0.150): one per model response, with the
//     OpenAI response_id and that response's usage — the billed unit.
//   - event_msg/token_count: cumulative + last-response usage. Older CLIs emit
//     only this, so it is the fallback when a file has no usage records.
//
// OpenAI usage counts input INCLUDING cached and cache-write tokens and output
// INCLUDING reasoning tokens. Records are normalized to the Claude shape the
// cloud prices: fresh input, cached input as cache-read, cache-write as
// cache-create, output as-is, reasoning carried separately as a subset.

// codexUsageProvider is the wire provider value for Codex rows.
const codexUsageProvider = "codex"

// codexFileState is the per-rollout context carried between incremental
// reads (persisted in the cursor), because the lines that identify a record
// (session_meta, turn_context) precede it, possibly by many passes.
type codexFileState struct {
	ThreadID   string `json:"threadId,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Model      string `json:"model,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	GitBranch  string `json:"gitBranch,omitempty"`
	GitRemote  string `json:"gitRemote,omitempty"`
	CLIVersion string `json:"cliVersion,omitempty"`
	Originator string `json:"originator,omitempty"`
	// ModelProvider is the session_meta.model_provider value reported by the CLI.
	ModelProvider string `json:"modelProvider,omitempty"`
	ServiceTier   string `json:"serviceTier,omitempty"`
	AccountID     string `json:"accountId,omitempty"`
	UserID        string `json:"userId,omitempty"`
	Subagent      bool   `json:"subagent,omitempty"`
	// HasRecords latches once the file has produced a token_usage_record; from
	// then on token_count events are ignored so a response is never counted
	// twice. Files from older CLIs never latch and use token_count.
	HasRecords bool `json:"hasRecords,omitempty"`
}

// codexPrefilters gate the JSON parse: only lines naming one of these types
// can matter. Everything else (messages, tool calls, reasoning) is skipped
// without an allocation. A match inside quoted content is harmless — the
// parsed top-level type is checked again.
var codexPrefilters = [][]byte{
	[]byte(`"token_usage_record"`),
	[]byte(`"token_count"`),
	[]byte(`"turn_context"`),
	[]byte(`"session_meta"`),
	[]byte(`"thread_settings_applied"`),
}

type codexUsageNumbers struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type codexEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// extractCodexUsage parses every COMPLETE line in buf, updating st and
// returning the billed records plus the bytes consumed (through the last
// '\n'). A trailing partial line is left for the next pass.
func extractCodexUsage(buf []byte, st *codexFileState) (recs []ccUsageRecord, consumed int) {
	for {
		nl := bytes.IndexByte(buf[consumed:], '\n')
		if nl < 0 {
			break
		}
		line := buf[consumed : consumed+nl]
		consumed += nl + 1
		if !codexLineMayMatter(line) {
			continue
		}
		var env codexEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		switch env.Type {
		case "session_meta":
			codexApplySessionMeta(env.Payload, st)
		case "turn_context":
			var tc struct {
				Model       string  `json:"model"`
				Cwd         string  `json:"cwd"`
				ServiceTier *string `json:"service_tier"`
			}
			if json.Unmarshal(env.Payload, &tc) == nil {
				if tc.Model != "" {
					st.Model = tc.Model
				}
				if tc.Cwd != "" {
					st.Cwd = tc.Cwd
				}
				// Older CLIs put the tier here; newer ones omit the key and
				// report it via thread_settings_applied, so only a present
				// field may change it.
				if tc.ServiceTier != nil {
					st.ServiceTier = *tc.ServiceTier
				}
			}
		case "token_usage_record":
			if rec, ok := codexRecordFromUsageRecord(env, st); ok {
				st.HasRecords = true
				recs = append(recs, rec)
			}
		case "event_msg":
			if codexApplyThreadSettings(env.Payload, st) || st.HasRecords {
				continue
			}
			if rec, ok := codexRecordFromTokenCount(env, st); ok {
				recs = append(recs, rec)
			}
		}
	}
	return recs, consumed
}

func codexLineMayMatter(line []byte) bool {
	for _, p := range codexPrefilters {
		if bytes.Contains(line, p) {
			return true
		}
	}
	return false
}

// codexApplyThreadSettings applies an event_msg/thread_settings_applied
// payload (model + service tier) and reports whether the payload was one.
func codexApplyThreadSettings(raw json.RawMessage, st *codexFileState) bool {
	var p struct {
		Type     string `json:"type"`
		Settings *struct {
			Model       string  `json:"model"`
			ServiceTier *string `json:"service_tier"`
		} `json:"thread_settings"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Type != "thread_settings_applied" || p.Settings == nil {
		return false
	}
	if p.Settings.Model != "" {
		st.Model = p.Settings.Model
	}
	if p.Settings.ServiceTier != nil {
		st.ServiceTier = *p.Settings.ServiceTier
	}
	return true
}

func codexApplySessionMeta(raw json.RawMessage, st *codexFileState) {
	var meta struct {
		ID               string          `json:"id"`
		SessionID        string          `json:"session_id"`
		Cwd              string          `json:"cwd"`
		Originator       string          `json:"originator"`
		CLIVersion       string          `json:"cli_version"`
		ModelProvider    string          `json:"model_provider"`
		CreatorAccountID string          `json:"creator_account_id"`
		CreatorUserID    string          `json:"creator_user_id"`
		Source           json.RawMessage `json:"source"`
		Git              *struct {
			Branch        string `json:"branch"`
			RepositoryURL string `json:"repository_url"`
		} `json:"git"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return
	}
	id, idOK := canonicalCodexThreadID(meta.ID)
	if st.ThreadID != "" && (!idOK || id != st.ThreadID) {
		// A forked/subagent rollout embeds a COPY of its parent's
		// session_meta right after its own. Identity latches from the first
		// meta; only a later meta for the SAME thread (a resume appending a
		// fresh segment) may update the per-segment context below.
		return
	}
	if idOK {
		st.ThreadID = id
	}
	if id, ok := canonicalCodexThreadID(meta.SessionID); ok {
		st.SessionID = id
	} else if st.SessionID == "" {
		st.SessionID = st.ThreadID
	}
	if meta.Cwd != "" {
		st.Cwd = meta.Cwd
	}
	st.Originator = meta.Originator
	st.CLIVersion = meta.CLIVersion
	st.ModelProvider = meta.ModelProvider
	st.AccountID = meta.CreatorAccountID
	st.UserID = meta.CreatorUserID
	// source is a string ("cli", "exec", "vscode") or an object
	// ({"subagent": {...}}) for spawned subagent threads.
	st.Subagent = bytes.Contains(meta.Source, []byte(`"subagent"`))
	st.GitBranch, st.GitRemote = "", ""
	if meta.Git != nil {
		st.GitBranch = meta.Git.Branch
		st.GitRemote = meta.Git.RepositoryURL
	}
}

func codexRecordFromUsageRecord(env codexEnvelope, st *codexFileState) (ccUsageRecord, bool) {
	var p struct {
		ThreadID   string            `json:"thread_id"`
		SessionID  string            `json:"session_id"`
		TurnID     string            `json:"turn_id"`
		ResponseID string            `json:"response_id"`
		Usage      codexUsageNumbers `json:"usage"`
	}
	if json.Unmarshal(env.Payload, &p) != nil {
		return ccUsageRecord{}, false
	}
	thread := st.ThreadID
	if id, ok := canonicalCodexThreadID(p.ThreadID); ok {
		thread = id
	}
	session := st.SessionID
	if id, ok := canonicalCodexThreadID(p.SessionID); ok {
		session = id
	}
	if session == "" {
		session = thread
	}
	messageID := strings.TrimSpace(p.ResponseID)
	if messageID == "" {
		// No response id (unexpected): a deterministic surrogate keeps the
		// row countable and stable across re-scans.
		messageID = "codex-syn:" + thread + ":" + env.Timestamp
	}
	requestID := "codex:" + strings.TrimSpace(p.TurnID)
	if p.TurnID == "" {
		requestID = "codex:" + thread
	}
	return codexBuildRecord(env.Timestamp, session, messageID, requestID, p.Usage, st)
}

func codexRecordFromTokenCount(env codexEnvelope, st *codexFileState) (ccUsageRecord, bool) {
	var p struct {
		Type string `json:"type"`
		Info *struct {
			Total *codexUsageNumbers `json:"total_token_usage"`
			Last  *codexUsageNumbers `json:"last_token_usage"`
		} `json:"info"`
	}
	if json.Unmarshal(env.Payload, &p) != nil || p.Type != "token_count" || p.Info == nil || p.Info.Total == nil || p.Info.Last == nil {
		return ccUsageRecord{}, false
	}
	if st.ThreadID == "" || p.Info.Last.TotalTokens <= 0 {
		return ccUsageRecord{}, false
	}
	// token_count repeats (e.g. on rate-limit refreshes) without new usage.
	// Keying on the thread's CUMULATIVE total makes every repeat of the same
	// step collapse onto one row, with no state across passes.
	messageID := "codex-tc:" + st.ThreadID + ":" + strconv.FormatInt(p.Info.Total.TotalTokens, 10)
	session := st.SessionID
	if session == "" {
		session = st.ThreadID
	}
	return codexBuildRecord(env.Timestamp, session, messageID, "codex-tc:"+st.ThreadID, *p.Info.Last, st)
}

func codexBuildRecord(ts, session, messageID, requestID string, u codexUsageNumbers, st *codexFileState) (ccUsageRecord, bool) {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteInputTokens < 0 || u.ReasoningOutputTokens < 0 {
		return ccUsageRecord{}, false
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return ccUsageRecord{}, false
	}
	cached, write := u.CachedInputTokens, u.CacheWriteInputTokens
	if cached+write > u.InputTokens {
		// Defensive: never let the cache classes exceed the input they are
		// a share of (would make fresh input negative).
		if cached > u.InputTokens {
			cached = u.InputTokens
		}
		write = u.InputTokens - cached
	}
	fresh := u.InputTokens - cached - write
	reasoning := u.ReasoningOutputTokens
	if reasoning > u.OutputTokens {
		reasoning = u.OutputTokens
	}
	model := st.Model
	if model == "" {
		model = "codex-unknown"
	}
	if tier := strings.ToLower(st.ServiceTier); (tier == "fast" || tier == "priority") && !strings.Contains(strings.ToLower(model), "-fast") {
		// Fast/priority processing is billed at 2× — the cloud prices a
		// "-fast" model id accordingly.
		model += "-fast"
	}
	rec := ccUsageRecord{
		EventTS:               ts,
		SessionID:             session,
		MessageID:             messageID,
		RequestID:             requestID,
		Model:                 model,
		IsSidechain:           st.Subagent,
		InputTokens:           int(fresh),
		OutputTokens:          int(u.OutputTokens),
		CacheReadTokens:       int(cached),
		CacheCreateTokens:     int(write),
		ServiceTier:           st.ServiceTier,
		Cwd:                   st.Cwd,
		GitBranch:             st.GitBranch,
		CCVersion:             st.CLIVersion,
		Provider:              codexUsageProvider,
		ReasoningOutputTokens: int(reasoning),
	}
	if remote := normalizeGitRemote(st.GitRemote); remote != "" {
		rec.GitRepo = remote
	}
	rec.TotalTokens = rec.InputTokens + rec.OutputTokens + rec.CacheReadTokens + rec.CacheCreateTokens
	return rec, true
}
