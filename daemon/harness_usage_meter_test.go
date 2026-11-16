package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"soluterminal-shared/deepseekusage"

	"github.com/klauspost/compress/zstd"
)

// writeSessionLog writes a zstd session log with the given events.
func writeSessionLog(t *testing.T, dir string, events []map[string]any) {
	t.Helper()
	writeSessionLogNamed(t, dir, "session.v3.jsonl.zstd", events)
}

func writeSessionLogNamed(t *testing.T, dir, name string, events []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	w, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		line, _ := json.Marshal(event)
		if _, err := w.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func meterHeader(model string, at time.Time) map[string]any {
	return map[string]any{
		"type": "request/header", "seq": 1, "time": at.UnixMilli(),
		"data": map[string]any{"header": map[string]any{"config": map[string]any{"model": model}}},
	}
}

func meterUsage(input, output, cacheRead, cacheWrite int64, at time.Time) map[string]any {
	return map[string]any{
		"type": "assistant/message", "seq": 2, "time": at.UnixMilli(),
		"data": map[string]any{"usage": map[string]any{
			"inputTokens": input, "outputTokens": output,
			"cacheReadTokens": cacheRead, "cacheWriteTokens": cacheWrite,
		}},
	}
}

func TestScanDeepSeekMeterPricesAndAttributes(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	accounts := filepath.Join(root, "session-accounts")
	peak := time.Date(2026, 9, 14, 2, 0, 0, 0, time.UTC) // Monday, peak
	writeSessionLog(t, filepath.Join(sessions, "--home-x--", "sess-1"), []map[string]any{
		meterHeader("deepseek-flash", peak),
		meterUsage(1_000_000, 0, 1_000_000, 1_000_000, peak),
	})
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accounts, "sess-1.json"), []byte(`{"account":"alice","model":"deepseek-flash"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := scanDeepSeekMeter(sessions, accounts)
	if err != nil {
		t.Fatal(err)
	}
	alice, ok := got["alice"]
	if !ok {
		t.Fatalf("alice missing: %+v", got)
	}
	// miss 1M @2 + (read+write) 2M @0.04 = 2.08 CNY at peak.
	if alice.Cost < 2.08-1e-9 || alice.Cost > 2.08+1e-9 {
		t.Errorf("cost = %v, want 2.08", alice.Cost)
	}
	if alice.CacheMissInputTokens != 1_000_000 || alice.CacheHitInputTokens != 2_000_000 {
		t.Errorf("tokens = %+v", alice)
	}
}

func TestScanDeepSeekMeterOffPeakIsHalfAndUnattributedWithoutSidecar(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	accounts := filepath.Join(root, "session-accounts")
	offPeak := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) // Monday, off-peak
	writeSessionLog(t, filepath.Join(sessions, "--home-x--", "sess-2"), []map[string]any{
		meterHeader("deepseek-flash", offPeak),
		meterUsage(1_000_000, 0, 0, 0, offPeak),
	})
	got, err := scanDeepSeekMeter(sessions, accounts)
	if err != nil {
		t.Fatal(err)
	}
	unattributed, ok := got["unattributed"]
	if !ok {
		t.Fatalf("expected an unattributed bucket: %+v", got)
	}
	if unattributed.Cost < 1.0-1e-9 || unattributed.Cost > 1.0+1e-9 {
		t.Errorf("off-peak cost = %v, want 1.0", unattributed.Cost)
	}
}

func TestHarnessUsageReportMergesMeterEstimate(t *testing.T) {
	a := &apiServer{dataDir: t.TempDir()}
	peak := time.Date(2026, 9, 14, 2, 0, 0, 0, time.UTC)
	writeSessionLog(t, filepath.Join(deepSeekSessionsDir(a.dataDir), "--home-x--", "sess-9"), []map[string]any{
		meterHeader("deepseek-flash", peak),
		meterUsage(1_000_000, 0, 0, 0, peak),
	})
	if err := os.MkdirAll(deepSeekAccountsDir(a.dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deepSeekAccountsDir(a.dataDir), "sess-9.json"), []byte(`{"account":"bob"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	a.registerHarnessRoutes(mux)
	a.registerExtensionRoutes(mux)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest("GET", "/api/harnesses/deepseek/usage", nil))
	if res.Code != 200 {
		t.Fatalf("GET usage: %d %s", res.Code, res.Body.String())
	}
	var report harnessUsageReport
	if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	bob := keyRow(t, report, "bob")
	if !bob.Estimated || bob.MeterCost < 2.0-1e-9 || bob.MeterCost > 2.0+1e-9 || bob.Cost != 0 {
		t.Errorf("meter-only row wrong: %+v", bob)
	}
}

// mergeMeter must keep the authoritative import and only add the estimate.
func TestMergeMeterKeepsImportedCostAddsEstimate(t *testing.T) {
	report := harnessUsageReport{Currency: "USD", Keys: []harnessKeyUsageRow{{APIKeyName: "alice", KeyUsage: deepseekusage.KeyUsage{Cost: 0.912}}}}
	mergeMeter(&report, map[string]deepseekusage.KeyUsage{
		"alice": {Cost: 9.99, CacheHitInputTokens: 100, UnpricedTokens: 25},
		"bob":   {Cost: 0.001, CacheMissInputTokens: 1000},
	})
	var alice, bob *harnessKeyUsageRow
	for i := range report.Keys {
		switch report.Keys[i].APIKeyName {
		case "alice":
			alice = &report.Keys[i]
		case "bob":
			bob = &report.Keys[i]
		}
	}
	if alice == nil || alice.Cost != 0.912 || alice.MeterCost != 9.99 || alice.MeterCurrency != "CNY" || alice.MeterUnpricedTokens != 25 {
		t.Fatalf("imported cost must survive and estimate merge: %+v", alice)
	}
	if bob == nil || !bob.Estimated || bob.Cost != 0 || bob.MeterCost != 0.001 || bob.MeterCurrency != "CNY" {
		t.Fatalf("meter-only row wrong: %+v", bob)
	}
	if report.Currency != "USD" {
		t.Fatalf("meter must not relabel imported currency: %q", report.Currency)
	}
}

func TestScanSessionUsageSurfacesUnknownAndUnverifiedTokens(t *testing.T) {
	root := t.TempDir()
	peak := time.Date(2026, 9, 14, 2, 0, 0, 0, time.UTC)
	expired := time.Date(2027, 1, 4, 2, 0, 0, 0, time.UTC)
	fileDir := filepath.Join(root, "mixed")
	writeSessionLog(t, fileDir, []map[string]any{
		meterHeader("deepseek-flash", peak),
		meterUsage(1_000_000, 0, 0, 0, peak),
		meterHeader("future-model", peak),
		meterUsage(500_000, 0, 0, 0, peak),
		meterHeader("deepseek-flash", expired),
		meterUsage(250_000, 0, 0, 0, expired),
	})
	usage, err := scanSessionUsage(filepath.Join(fileDir, "session.v3.jsonl.zstd"), "")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Cost != 2 || usage.UnpricedTokens != 750_000 || usage.CacheMissInputTokens != 1_750_000 {
		t.Fatalf("mixed priced/unpriced usage = %+v", usage)
	}
}

func TestMergeMeterWhollyUnpricedNeverLooksLikeZeroEstimate(t *testing.T) {
	report := harnessUsageReport{Currency: "CNY", Keys: []harnessKeyUsageRow{}}
	mergeMeter(&report, map[string]deepseekusage.KeyUsage{
		"unknown": {CacheMissInputTokens: 1000, UnpricedTokens: 1000},
	})
	row := keyRow(t, report, "unknown")
	if !row.Estimated || row.MeterTokens != 1000 || row.MeterUnpricedTokens != 1000 || row.MeterCurrency != "CNY" || row.MeterCost != 0 {
		t.Fatalf("unpriced meter row = %+v", row)
	}
}

// DeepSeek Harness 0.2.2 writes session.v4.jsonl.zstd with the same usage
// fields; the meter reads it, preferring it over an older log beside it.
func TestScanDeepSeekMeterReadsV4SessionLogs(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	accounts := filepath.Join(root, "session-accounts")
	peak := time.Date(2026, 9, 14, 2, 0, 0, 0, time.UTC)
	dir := filepath.Join(sessions, "--home-x--", "session-v4")
	writeSessionLogNamed(t, dir, "session.v4.jsonl.zstd", []map[string]any{
		meterHeader("deepseek-flash", peak),
		meterUsage(1_000_000, 0, 0, 0, peak),
	})
	writeSessionLog(t, dir, []map[string]any{meterHeader("deepseek-flash", peak), meterUsage(5_000_000, 0, 0, 0, peak)})
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accounts, "session-v4.json"), []byte(`{"account":"alice","model":"deepseek-flash"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := scanDeepSeekMeter(sessions, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if alice := got["alice"]; alice.CacheMissInputTokens != 1_000_000 {
		t.Fatalf("v4 log not metered on its own: %+v", got)
	}
}
