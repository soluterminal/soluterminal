package helperproto

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const scanUsageTestUUID = "01234567-89ab-cdef-0123-456789abcdef"

func scanUsageFixture() ScanUsageRecord {
	return ScanUsageRecord{
		T: "usage", FileID: strings.Repeat("a", 64), Provider: "claude",
		EventTS: "2026-11-20T08:00:01.123000000Z", SessionID: "session-existing-key",
		MessageID: "synmsg:existing:identity", RequestID: "synreq:existing:identity", Model: "claude-model",
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: 300, CacheCreateTokens: 400, TotalTokens: 820,
		CacheCreate5mTokens: 150, CacheCreate1hTokens: 250, WebSearchRequests: 1, WebFetchRequests: 2,
		ServiceTier: "standard", Speed: "normal", CCVersion: "2.0.0",
	}
}

func TestScanUsageRoundTripAndAttribution(t *testing.T) {
	claude := scanUsageFixture()
	claude.IsSidechain = true
	claude.ParentSessionID = scanUsageTestUUID
	codex := scanUsageFixture()
	codex.Provider, codex.ThreadID, codex.InApp = "codex", scanUsageTestUUID, true
	codex.ReasoningOutputTokens = 12
	codex.CacheCreate5mTokens, codex.CacheCreate1hTokens, codex.WebSearchRequests, codex.WebFetchRequests = 0, 0, 0, 0
	codex.Speed = ""
	for _, want := range []ScanUsageRecord{claude, codex} {
		line, err := EncodeScanUsage(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeScanUsage(line)
		if err != nil || got != want {
			t.Fatalf("round trip: %+v, %v", got, err)
		}
		for _, forbidden := range []string{"cwd", "git_branch", "git_repo", "originator", "account", "user_id", "workflow_role", "stop_reason", "prompt"} {
			if strings.Contains(string(line), `"`+forbidden+`"`) {
				t.Fatalf("non-allowlisted field %s sent", forbidden)
			}
		}
	}
}

func TestScanUsageWriterPadsTime(t *testing.T) {
	for _, tc := range []struct{ input, wire string }{
		{"2026-11-20T08:00:01Z", "2026-11-20T08:00:01.000000000Z"},
		{"2026-11-20T08:00:01.000Z", "2026-11-20T08:00:01.000000000Z"},
		{"2026-11-20T08:00:01.123456789Z", "2026-11-20T08:00:01.123456789Z"},
	} {
		r := scanUsageFixture()
		r.EventTS = tc.input
		line, err := EncodeScanUsage(r)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeScanUsage(line)
		if err != nil || got.EventTS != tc.wire || got.InputTokens != r.InputTokens || got.MessageID != r.MessageID {
			t.Fatal("writer did not pad timestamp or changed metadata")
		}
	}
}

func TestScanUsageRejectsInvalidMetadata(t *testing.T) {
	mutations := map[string]func(*ScanUsageRecord){
		"provider":             func(r *ScanUsageRecord) { r.Provider = "SECRET_PROVIDER" },
		"kind":                 func(r *ScanUsageRecord) { r.T = "cred" },
		"bad-file-id":          func(r *ScanUsageRecord) { r.FileID = "SECRET_PATH" },
		"uppercase-file-id":    func(r *ScanUsageRecord) { r.FileID = strings.Repeat("A", 64) },
		"id-too-long":          func(r *ScanUsageRecord) { r.MessageID = strings.Repeat("x", 513) },
		"model-too-long":       func(r *ScanUsageRecord) { r.Model = strings.Repeat("x", 257) },
		"tier-too-long":        func(r *ScanUsageRecord) { r.ServiceTier = strings.Repeat("x", 33) },
		"speed-too-long":       func(r *ScanUsageRecord) { r.Speed = strings.Repeat("x", 33) },
		"version-too-long":     func(r *ScanUsageRecord) { r.CCVersion = strings.Repeat("x", 65) },
		"negative":             func(r *ScanUsageRecord) { r.WebFetchRequests = -1 },
		"integer-too-large":    func(r *ScanUsageRecord) { r.WebSearchRequests = scanUsageIntegerMax + 1 },
		"checked-sum":          func(r *ScanUsageRecord) { r.InputTokens = scanUsageIntegerMax; r.TotalTokens = scanUsageIntegerMax },
		"wrong-total":          func(r *ScanUsageRecord) { r.TotalTokens++ },
		"split-too-large":      func(r *ScanUsageRecord) { r.CacheCreate1hTokens++ },
		"reasoning-too-large":  func(r *ScanUsageRecord) { r.ReasoningOutputTokens = r.OutputTokens + 1 },
		"bad-time":             func(r *ScanUsageRecord) { r.EventTS = "SECRET_TIME" },
		"non-utc-time":         func(r *ScanUsageRecord) { r.EventTS = "2026-11-20T08:00:01+08:00" },
		"invalid-utf8":         func(r *ScanUsageRecord) { r.Model = string([]byte{0xff}) },
		"parent-not-uuid":      func(r *ScanUsageRecord) { r.IsSidechain = true; r.ParentSessionID = "SECRET_PARENT_PATH" },
		"parent-not-sidechain": func(r *ScanUsageRecord) { r.ParentSessionID = scanUsageTestUUID },
		"codex-parent": func(r *ScanUsageRecord) {
			r.Provider = "codex"
			r.IsSidechain = true
			r.ParentSessionID = scanUsageTestUUID
		},
		"claude-thread":   func(r *ScanUsageRecord) { r.ThreadID = scanUsageTestUUID },
		"thread-not-uuid": func(r *ScanUsageRecord) { r.Provider = "codex"; r.ThreadID = "SECRET_THREAD" },
		"claude-in-app":   func(r *ScanUsageRecord) { r.InApp = true },
	}
	for _, bad := range []string{"\n", "\x7f", "\u0085", "\u202e", "\u200b", "\u2028", "\u2029"} {
		value := bad
		mutations["control-"+bad] = func(r *ScanUsageRecord) { r.RequestID = "id" + value }
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := scanUsageFixture()
			mutate(&r)
			line, err := EncodeScanUsage(r)
			if line != nil || !errors.Is(err, ErrInvalidScanRecord) || strings.Contains(err.Error(), "SECRET_") {
				t.Fatal("invalid outgoing record accepted or echoed")
			}
			// json.Marshal replaces malformed UTF-8; the raw-wire case is tested separately.
			if name == "invalid-utf8" {
				return
			}
			// Marshal directly to exercise host validation independently of the writer.
			raw, _ := json.Marshal(r)
			if got, err := DecodeScanUsage(raw); got != (ScanUsageRecord{}) || !errors.Is(err, ErrInvalidScanRecord) {
				t.Fatal("invalid incoming record accepted or partially returned")
			}
		})
	}
}

func TestScanUsageStrictDecodeAndNoEcho(t *testing.T) {
	base, _ := EncodeScanUsage(scanUsageFixture())
	for name, mutate := range map[string]func(string) string{
		"secret":              func(s string) string { return strings.TrimSuffix(s, "}") + `,"accessToken":"SECRET_FIXTURE_TOKEN"}` },
		"originator":          func(s string) string { return strings.TrimSuffix(s, "}") + `,"originator":"SECRET_ORIGINATOR"}` },
		"duplicate":           func(s string) string { return strings.TrimSuffix(s, "}") + `,"provider":"codex"}` },
		"alias":               func(s string) string { return strings.Replace(s, `"model":`, `"Model":`, 1) },
		"missing":             func(s string) string { return strings.Replace(s, `"input_tokens":100,`, "", 1) },
		"null":                func(s string) string { return strings.Replace(s, `"input_tokens":100`, `"input_tokens":null`, 1) },
		"optional-null":       func(s string) string { return strings.Replace(s, `"speed":"normal"`, `"speed":null`, 1) },
		"float":               func(s string) string { return strings.Replace(s, `"input_tokens":100`, `"input_tokens":100.0`, 1) },
		"short-time":          func(s string) string { return strings.Replace(s, ".123000000Z", ".123Z", 1) },
		"empty-parent":        func(s string) string { return strings.TrimSuffix(s, "}") + `,"parent_session_id":""}` },
		"empty-thread":        func(s string) string { return strings.TrimSuffix(s, "}") + `,"thread_id":""}` },
		"wrong-flag-provider": func(s string) string { return strings.TrimSuffix(s, "}") + `,"in_app":false}` },
		"raw-control":         func(s string) string { return "\t" + s },
		"invalid-utf8":        func(s string) string { return s + string([]byte{0xff}) },
		"second-value":        func(s string) string { return s + "{}" },
		"oversize":            func(s string) string { return strings.Repeat(" ", MaxLine) + s },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeScanUsage([]byte(mutate(string(base))))
			if got != (ScanUsageRecord{}) || !errors.Is(err, ErrInvalidScanRecord) || strings.Contains(err.Error(), "SECRET_") {
				t.Fatal("invalid wire record accepted, partially returned, or echoed")
			}
		})
	}
}

func TestScanUsageBoundaryCountsAndIDs(t *testing.T) {
	r := scanUsageFixture()
	r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreateTokens = scanUsageIntegerMax, 0, 0, 0
	r.TotalTokens, r.CacheCreate5mTokens, r.CacheCreate1hTokens = scanUsageIntegerMax, 0, 0
	r.SessionID, r.MessageID, r.RequestID = strings.Repeat("s", 512), strings.Repeat("m", 512), strings.Repeat("r", 512)
	r.Model, r.ServiceTier, r.Speed, r.CCVersion = strings.Repeat("x", 256), strings.Repeat("x", 32), strings.Repeat("x", 32), strings.Repeat("x", 64)
	line, err := EncodeScanUsage(r)
	if err != nil {
		t.Fatal("valid maximum rejected", err)
	}
	if got, err := DecodeScanUsage(line); err != nil || got != r {
		t.Fatal("integer or identity boundary lost on round trip")
	}
}
