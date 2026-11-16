package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveDurableLogDirFollowsDataDir pins the decision on every OS: a
// daemon started with a non-default --data dir (a side daemon) logs under that
// dir instead of the real install's shell data dir, while the real install
// (no --data, or --data equal to the default) keeps the shell data dir.
func TestResolveDurableLogDirFollowsDataDir(t *testing.T) {
	base := t.TempDir()
	defaultData := filepath.Join(base, "home", ".solu")
	shellDir := filepath.Join(base, "localappdata", "soluterminal")
	shell := func() (string, error) { return shellDir, nil }
	scratch := filepath.Join(base, "scratch", "data")

	cases := []struct {
		name, goos, env, addr, data, want string
	}{
		{"windows real install, no --data", "windows", "", "", "", shellDir},
		{"windows --data equal to default", "windows", "", "", defaultData, shellDir},
		{"windows --data default with trailing separator", "windows", "", "", defaultData + string(filepath.Separator), shellDir},
		{"windows --data default in another case", "windows", "", "", strings.ToUpper(defaultData), shellDir},
		{"windows scratch --data", "windows", "", "", scratch, filepath.Join(scratch, "logs")},
		{"windows env override wins over --data", "windows", filepath.Join(base, "envlogs"), "", scratch, filepath.Join(base, "envlogs")},
		{"linux real install, no --data", "linux", "", "", "", shellDir},
		{"linux --data equal to default", "linux", "", "", defaultData, shellDir},
		{"linux scratch --data logs under it", "linux", "", "", scratch, filepath.Join(scratch, "logs")},
		{"linux env override", "linux", filepath.Join(base, "envlogs"), "", "", filepath.Join(base, "envlogs")},
		{"linux env override wins over --data", "linux", filepath.Join(base, "envlogs"), "", scratch, filepath.Join(base, "envlogs")},
		{"darwin scratch --data logs under it", "darwin", "", "", scratch, filepath.Join(scratch, "logs")},
		{"linux real install on its canonical port", "linux", "", "127.0.0.1:8787", "", shellDir},
		{"WSL daemon on its WSL address", "linux", "", "172.29.132.65:8787", "", shellDir},
		{"windows Dual backend", "windows", "", "127.0.0.1:38787", "", shellDir},
		{"linux side port, no --data: default data's logs, not the real file", "linux", "", "127.0.0.1:18899", "", filepath.Join(defaultData, "logs")},
		{"linux side port, --data default: same", "linux", "", "127.0.0.1:18899", defaultData, filepath.Join(defaultData, "logs")},
		{"windows side port, no --data", "windows", "", "127.0.0.1:18899", "", filepath.Join(defaultData, "logs")},
		{"linux side port and scratch --data", "linux", "", "127.0.0.1:18899", scratch, filepath.Join(scratch, "logs")},
		{"env override wins over a side port", "linux", filepath.Join(base, "envlogs"), "127.0.0.1:18899", "", filepath.Join(base, "envlogs")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDurableLogDir(tc.goos, tc.env, tc.addr, tc.data, defaultData, shell)
			if err != nil {
				t.Fatalf("resolveDurableLogDir: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveDurableLogDirShellError(t *testing.T) {
	want := errors.New("no LOCALAPPDATA")
	if _, err := resolveDurableLogDir("windows", "", "", "", "/x/.solu", func() (string, error) { return "", want }); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	// A scratch --data dir does not need LOCALAPPDATA at all.
	got, err := resolveDurableLogDir("windows", "", "", "/scratch", "/x/.solu", func() (string, error) { return "", want })
	if err != nil || got != filepath.Join("/scratch", "logs") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDurableLogDataFlag(t *testing.T) {
	cases := []struct {
		args  []string
		want  string
		found bool
	}{
		{nil, "", false},
		{[]string{"--addr", "127.0.0.1:8787"}, "", false},
		{[]string{"--addr", "127.0.0.1:8787", "--data", `C:\scratch`}, `C:\scratch`, true},
		{[]string{"-data", "/d"}, "/d", true},
		{[]string{"--data=/d"}, "/d", true},
		{[]string{"-data=/d", "--shell-version", "1"}, "/d", true},
		{[]string{"--data", "/a", "--data", "/b"}, "/b", true},
		{[]string{"--data-dir", "/x"}, "", false},
		{[]string{"--", "--data", "/x"}, "", false},
		{[]string{"--data"}, "", false},
		{[]string{"---data", "/x"}, "", false},
	}
	if got, found := durableLogFlag([]string{"-data", "/d", "-addr", "127.0.0.1:18899"}, "addr"); got != "127.0.0.1:18899" || !found {
		t.Errorf("durableLogFlag addr = %q, %v", got, found)
	}
	if got, found := durableLogFlag([]string{"--addr=127.0.0.1:9787"}, "addr"); got != "127.0.0.1:9787" || !found {
		t.Errorf("durableLogFlag --addr= = %q, %v", got, found)
	}
	for _, tc := range cases {
		got, found := durableLogDataFlag(tc.args)
		if got != tc.want || found != tc.found {
			t.Errorf("durableLogDataFlag(%q) = %q, %v; want %q, %v", tc.args, got, found, tc.want, tc.found)
		}
	}
}

// TestSupportBundleReadsTheAttachedDurableLog: the support bundle reads the
// file this daemon actually opened, so a Windows daemon logging under its
// --data dir still ships its own log rather than the shell data dir's.
func TestSupportBundleReadsTheAttachedDurableLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, durableLogName)
	if err := os.WriteFile(path, []byte("attached-log-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := durableLog
	t.Cleanup(func() { durableLog = prev })
	durableLog = &rotatingLogWriter{path: path}
	t.Setenv("SOLU_DURABLE_LOG_DIR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	text, got, _ := readSupportBundleDurableLog()
	if got != path || !strings.Contains(text, "attached-log-line") {
		t.Fatalf("read %q (%q), want the attached log %q", got, text, path)
	}
}
