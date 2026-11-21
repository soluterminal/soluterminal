package helperproto

import (
	"bytes"
	"encoding/json"
	"unicode"
	"unicode/utf8"
)

// ScanUsageRecord is the M4 metadata allowlist (owner 13b98270d,
// helper-protocol-v1 SHA-256 781ccc3c). It deliberately does not embed the cloud
// usage DTO: new cloud attribution or transcript fields must not cross Linux
// automatically. These codecs have no runtime callers; scan stays unregistered.
type ScanUsageRecord struct {
	T                     string `json:"t"`
	FileID                string `json:"fileId"`
	Provider              string `json:"provider"`
	EventTS               string `json:"event_ts"`
	SessionID             string `json:"session_id"`
	MessageID             string `json:"message_id"`
	RequestID             string `json:"request_id"`
	Model                 string `json:"model"`
	InputTokens           int64  `json:"input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
	CacheReadTokens       int64  `json:"cache_read_tokens"`
	CacheCreateTokens     int64  `json:"cache_create_tokens"`
	TotalTokens           int64  `json:"total_tokens"`
	IsSidechain           bool   `json:"is_sidechain"`
	ServiceTier           string `json:"service_tier,omitempty"`
	Speed                 string `json:"speed,omitempty"`
	CCVersion             string `json:"cc_version,omitempty"`
	CacheCreate5mTokens   int64  `json:"cache_create_5m_tokens,omitempty"`
	CacheCreate1hTokens   int64  `json:"cache_create_1h_tokens,omitempty"`
	WebSearchRequests     int64  `json:"web_search_requests,omitempty"`
	WebFetchRequests      int64  `json:"web_fetch_requests,omitempty"`
	ReasoningOutputTokens int64  `json:"reasoning_output_tokens,omitempty"`
	ParentSessionID       string `json:"parent_session_id,omitempty"`
	ThreadID              string `json:"thread_id,omitempty"`
	InApp                 bool   `json:"in_app,omitempty"`
}

const scanUsageIntegerMax int64 = 1<<53 - 1

var scanUsageRequired = []string{
	"t", "fileId", "provider", "event_ts", "session_id", "message_id", "request_id", "model",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_create_tokens", "total_tokens", "is_sidechain",
}
var scanUsageOptional = []string{
	"service_tier", "speed", "cc_version", "cache_create_5m_tokens", "cache_create_1h_tokens",
	"web_search_requests", "web_fetch_requests", "reasoning_output_tokens", "parent_session_id", "thread_id", "in_app",
}

func (r ScanUsageRecord) valid() bool {
	if r.T != "usage" || (r.Provider != "claude" && r.Provider != "codex") || !scanUsageFileID(r.FileID) || !validScanTime(r.EventTS) {
		return false
	}
	for _, field := range []struct {
		value string
		limit int
	}{
		{r.SessionID, 512}, {r.MessageID, 512}, {r.RequestID, 512}, {r.Model, 256},
		{r.ServiceTier, 32}, {r.Speed, 32}, {r.CCVersion, 64},
	} {
		if !scanUsageText(field.value, field.limit) {
			return false
		}
	}
	for _, n := range []int64{
		r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreateTokens, r.TotalTokens,
		r.CacheCreate5mTokens, r.CacheCreate1hTokens, r.WebSearchRequests, r.WebFetchRequests, r.ReasoningOutputTokens,
	} {
		if n < 0 || n > scanUsageIntegerMax {
			return false
		}
	}
	var total int64
	for _, n := range []int64{r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreateTokens} {
		if n > scanUsageIntegerMax-total {
			return false
		}
		total += n
	}
	if total != r.TotalTokens || r.CacheCreate5mTokens > r.CacheCreateTokens || r.CacheCreate1hTokens > r.CacheCreateTokens-r.CacheCreate5mTokens || r.ReasoningOutputTokens > r.OutputTokens {
		return false
	}
	if r.ParentSessionID != "" && (r.Provider != "claude" || !r.IsSidechain || !scanUsageUUID(r.ParentSessionID)) {
		return false
	}
	if r.ThreadID != "" && (r.Provider != "codex" || !scanUsageUUID(r.ThreadID)) {
		return false
	}
	return !r.InApp || r.Provider == "codex"
}

// EncodeScanUsage validates normalized usage, without changing token counts or
// identity keys, and pads UTC timestamps to the exact nine-digit wire form.
// Rejected values are never included in an error or partially encoded result.
func EncodeScanUsage(r ScanUsageRecord) ([]byte, error) {
	var err error
	r.EventTS, err = formatScanTime(r.EventTS)
	if err != nil || !r.valid() {
		return nil, ErrInvalidScanRecord
	}
	line, err := json.Marshal(r)
	if err != nil || len(line) > MaxLine {
		return nil, ErrInvalidScanRecord
	}
	return line, nil
}

// DecodeScanUsage accepts one newline-free wire record. Exact keys, duplicates,
// nulls, number bounds and derived sums are checked before a record is returned.
func DecodeScanUsage(line []byte) (ScanUsageRecord, error) {
	var r ScanUsageRecord
	fields, err := decodeScanRecord(line, &r, scanUsageRequired, scanUsageOptional)
	if err != nil || !r.valid() {
		return ScanUsageRecord{}, ErrInvalidScanRecord
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ScanUsageRecord{}, ErrInvalidScanRecord
		}
	}
	for _, key := range []string{"parent_session_id", "thread_id"} {
		if _, present := fields[key]; present && ((key == "parent_session_id" && r.ParentSessionID == "") || (key == "thread_id" && r.ThreadID == "")) {
			return ScanUsageRecord{}, ErrInvalidScanRecord
		}
	}
	// The derived flag is Codex-only, even when its explicit value is false.
	if _, present := fields["in_app"]; present && r.Provider != "codex" {
		return ScanUsageRecord{}, ErrInvalidScanRecord
	}
	return r, nil
}

func scanUsageText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

func scanUsageFileID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// UUID shape only, with no trimming, normalization or re-keying of identities.
func scanUsageUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
