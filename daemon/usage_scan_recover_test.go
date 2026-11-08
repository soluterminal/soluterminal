package main

import (
	"path/filepath"
	"testing"
	"time"
)

// A panic while reading a DSH session log (a corrupt or crafted file, as
// GO-2026-6865 could cause in the zstd decoder) is a read error or unknown
// usage, never a daemon crash.
func TestUsageScansSurviveAPanickingRead(t *testing.T) {
	prev := usageScanLine
	usageScanLine = func([]byte) { panic("decoder blew up") }
	t.Cleanup(func() { usageScanLine = prev })
	at := time.Date(2026, 9, 14, 2, 0, 0, 0, time.UTC)

	dir := filepath.Join(t.TempDir(), "meter")
	writeSessionLog(t, dir, []map[string]any{meterHeader("deepseek-flash", at), meterUsage(1000, 0, 0, 0, at)})
	usage, err := scanSessionUsage(filepath.Join(dir, "session.v3.jsonl.zstd"), "")
	if err == nil || usage.CacheMissInputTokens != 0 || usage.Cost != 0 {
		t.Fatalf("scanSessionUsage after a panic = %+v, %v", usage, err)
	}

	root := t.TempDir()
	writeSessionLog(t, filepath.Join(root, "a", "b"), []map[string]any{meterUsage(1000, 0, 0, 0, at)})
	if in, out := readDSHInvocationUsage(root); in != nil || out != nil {
		t.Fatalf("readDSHInvocationUsage after a panic = %v, %v", in, out)
	}
}
