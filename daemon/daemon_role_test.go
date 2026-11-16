package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDecideDaemonRole pins the primary-daemon rule: a canonical port (8787,
// or 38787 for Dual's Windows backend) on any host AND the default -data.
func TestDecideDaemonRole(t *testing.T) {
	base := t.TempDir()
	defaultData := filepath.Join(base, "home", ".solu")
	scratch := filepath.Join(base, "scratch", "data")
	cases := []struct {
		name, goos, addr, data string
		primary                bool
		reasons                int
	}{
		{"linux real daemon", "linux", "127.0.0.1:8787", defaultData, true, 0},
		{"WSL daemon on its WSL address", "linux", "172.29.132.65:8787", defaultData, true, 0},
		{"daemon bound to every interface", "linux", "0.0.0.0:8787", defaultData, true, 0},
		{"windows real daemon", "windows", "127.0.0.1:8787", defaultData, true, 0},
		{"windows Dual backend", "windows", "127.0.0.1:38787", defaultData, true, 0},
		{"default -data with trailing separator", "linux", "127.0.0.1:8787", defaultData + string(filepath.Separator), true, 0},
		{"windows default -data in another case", "windows", "127.0.0.1:8787", strings.ToUpper(defaultData), true, 0},
		{"capture daemon: side port and own -data", "linux", "127.0.0.1:18899", scratch, false, 2},
		{"side port, default -data", "linux", "127.0.0.1:18899", defaultData, false, 1},
		{"canonical port, scratch -data", "linux", "127.0.0.1:8787", scratch, false, 1},
		{"windows canonical port, scratch -data", "windows", "127.0.0.1:38787", scratch, false, 1},
		{"ephemeral port", "linux", "127.0.0.1:0", defaultData, false, 1},
		{"malformed -addr", "linux", "8787", defaultData, false, 1},
		{"linux -data differs only in case", "linux", "127.0.0.1:8787", strings.ToUpper(defaultData), false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideDaemonRole(tc.goos, tc.addr, tc.data, defaultData)
			if got.Primary != tc.primary || len(got.Reasons) != tc.reasons {
				t.Fatalf("decideDaemonRole(%q, %q) = %+v; want primary=%v with %d reasons", tc.addr, tc.data, got, tc.primary, tc.reasons)
			}
		})
	}
}

// TestDaemonRoleLogLine: one line says which role was chosen, and for a side
// daemon why and what it skipped.
func TestDaemonRoleLogLine(t *testing.T) {
	prev := sideDaemonSkips
	t.Cleanup(func() { sideDaemonSkips = prev })
	sideDaemonSkips = []string{"feature a", "feature b"}

	side := decideDaemonRole("linux", "127.0.0.1:18899", "/scratch/data", "/home/u/.solu").logLine()
	for _, want := range []string{"side daemon", "non-canonical port in -addr 127.0.0.1:18899", "non-default -data /scratch/data", "skipped: feature a, feature b"} {
		if !strings.Contains(side, want) {
			t.Errorf("side log line %q lacks %q", side, want)
		}
	}
	if strings.Contains(side, "\n") {
		t.Errorf("side log line spans lines: %q", side)
	}
	primary := decideDaemonRole("linux", "127.0.0.1:8787", "/home/u/.solu", "/home/u/.solu").logLine()
	if !strings.Contains(primary, "primary") || strings.Contains(primary, "skipped") {
		t.Errorf("primary log line = %q", primary)
	}

	sideDaemonSkips = nil
	if got := decideDaemonRole("linux", "127.0.0.1:1", "/d", "/home/u/.solu").logLine(); !strings.Contains(got, "skipped: nothing") {
		t.Errorf("log line with no registered skips = %q", got)
	}
}
