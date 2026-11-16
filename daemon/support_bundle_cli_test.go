package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func supportBundleCLIEnvironment(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("LOCALAPPDATA", home)
	logs := filepath.Join(home, "soluterminal")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOLU_DURABLE_LOG_DIR", logs)
	oldDial := supportBundleDial
	supportBundleDial = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		if network != "tcp" || addr != "127.0.0.1:8787" || timeout > time.Second {
			t.Errorf("unexpected network probe: %s %s %v", network, addr, timeout)
		}
		return nil, errors.New("no daemon listening")
	}
	t.Cleanup(func() { supportBundleDial = oldDial })
	return t.TempDir(), logs
}

func TestSupportBundleCLIWithoutDaemon(t *testing.T) {
	data, logs := supportBundleCLIEnvironment(t)
	const secret = "fixture-secret-do-not-export"
	fixtures := map[string]string{
		filepath.Join(data, "shell-info.json"):      `{"version":"1.2.3","platform":"test","unrecognized":"private metadata"}`,
		filepath.Join(data, "solud.version"):        "1.2.4",
		filepath.Join(data, "update_rollback.json"): `{"schema":1,"lastAttempt":{"state":"failed","error":"token=` + secret + `"}}`,
		filepath.Join(data, "env"):                  "PRIVATE-ENV-MUST-NOT-APPEAR",
		filepath.Join(logs, "solud.log"): "old\n" + strings.Repeat("ordinary log record\n", 10000) +
			`time=now msg="failure {\"access_token\":\"` + secret + `\"}"` + "\n[native-input] PRIVATE-PROMPT-MUST-NOT-APPEAR\nfinal diagnostic line\n",
		filepath.Join(logs, "launch.log"):    "launch failed PASSWORD=" + secret + "\n",
		filepath.Join(logs, "successor.log"): "successor failed token=" + secret + "\n",
	}
	for path, body := range fixtures {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, asJSON := range []bool{false, true} {
		out := filepath.Join(t.TempDir(), "bundle")
		args := []string{"--data", data, "--out", out}
		if asJSON {
			args = append(args, "--json")
		}
		var stdout, stderr bytes.Buffer
		if code := runSupportBundleCLI(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, &stderr)
		}
		body, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		all := string(body) + stdout.String() + stderr.String()
		for _, private := range []string{secret, "PRIVATE-ENV-MUST-NOT-APPEAR", "PRIVATE-PROMPT-MUST-NOT-APPEAR", "private metadata"} {
			if strings.Contains(all, private) {
				t.Errorf("private fixture escaped filtering: %s", private)
			}
		}
		for _, want := range []string{"1.2.3", "1.2.4", "failed", "[REDACTED]", "final diagnostic line", "no TCP connection"} {
			if !strings.Contains(string(body), want) {
				t.Errorf("missing diagnostic: %s", want)
			}
		}
		if len(body) > supportBundleMaxTotalBytes {
			t.Fatalf("saved file exceeds cap: %d", len(body))
		}
		if asJSON {
			var bundle supportBundleResponse
			if err := json.Unmarshal(body, &bundle); err != nil {
				t.Fatal(err)
			}
			if bundle.Schema != supportBundleSchema {
				t.Errorf("wrong schema: %d", bundle.Schema)
			}
		}
		st, _ := os.Stat(out)
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Errorf("output mode = %v", st.Mode())
		}
	}
	for path, original := range fixtures {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != original {
			t.Errorf("diagnostic input was modified: %s", path)
		}
	}
}

func TestSupportBundleCLIFileCapIncludesEncoding(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		bundle := supportBundleResponse{}
		for i := 0; i < 20; i++ {
			bundle.Sections = append(bundle.Sections, finalizeSupportSection(supportBundleSection{
				Text: strings.Repeat("\x01\"<& diagnostic é\n", 10000),
			}, supportBundleSectionMaxBytes))
		}
		clampSupportBundleTotal(&bundle)
		body, err := encodeSupportBundleFile(&bundle, asJSON)
		if err != nil || len(body) > supportBundleMaxTotalBytes {
			t.Fatalf("size=%d err=%v", len(body), err)
		}
		if asJSON && !json.Valid(body) {
			t.Fatal("clamping broke JSON")
		}
	}
}

func TestSupportBundleCLIMissingDataAndExistingOutput(t *testing.T) {
	data, _ := supportBundleCLIEnvironment(t)
	missing := filepath.Join(data, "missing")
	out := filepath.Join(t.TempDir(), "bundle.txt")
	var stdout, stderr bytes.Buffer
	args := []string{"--data", missing, "--out", out}
	if runSupportBundleCLI(args, &stdout, &stderr) != 0 {
		t.Fatal(stderr.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("offline command created data directory")
	}
	before, _ := os.ReadFile(out)
	if runSupportBundleCLI(args, &stdout, &stderr) == 0 {
		t.Fatal("existing destination was accepted")
	}
	after, _ := os.ReadFile(out)
	if !bytes.Equal(before, after) {
		t.Fatal("existing destination was modified")
	}
}

func TestSupportBundleCLIMalformedMetadata(t *testing.T) {
	data, _ := supportBundleCLIEnvironment(t)
	for _, body := range []string{`{"version":"token=hidden-fixture",`, strings.Repeat("x", supportBundleFileTailBytes+1)} {
		path := filepath.Join(data, "shell-info.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var target shellInfo
		got := supportBundleReadJSON(path, &target)
		if strings.Contains(got, "hidden-fixture") || len(got) > 200 {
			t.Fatal("malformed/oversized file was copied into diagnostics")
		}
	}
}

// Exercise actual main dispatch in a subprocess, with unavailable loopback and
// isolated HOME/log paths. No daemon server or mutable stores are initialized.
func TestSupportBundleCLIMainDispatch(t *testing.T) {
	if os.Getenv("SOLU_TEST_SUPPORT_BUNDLE_MAIN") == "1" {
		supportBundleDial = func(string, string, time.Duration) (net.Conn, error) {
			return nil, errors.New("no daemon")
		}
		os.Args = []string{"solud", "support-bundle", "--data", "missing-data", "--json"}
		main()
		return
	}
	supportBundleCLIEnvironment(t)
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupportBundleCLIMainDispatch$")
	cmd.Env = append(os.Environ(), "SOLU_TEST_SUPPORT_BUNDLE_MAIN=1")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dispatch failed: %v: %s", err, output)
	}
	path := strings.TrimSpace(string(output))
	if filepath.Dir(path) != "." || !strings.HasPrefix(path, "soluterminal-support.") {
		t.Fatalf("unexpected output path: %q", path)
	}
	body, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil || !json.Valid(body) {
		t.Fatalf("default output missing/invalid: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("CLI created unexpected state: %d entries", len(entries))
	}
}

// A side daemon (non-canonical -addr) keeps its durable log in <data>/logs
// even on the default data directory; --addr finds it and checks that port.
func TestSupportBundleCLISideDaemonAddr(t *testing.T) {
	supportBundleCLIEnvironment(t)
	t.Setenv("SOLU_DURABLE_LOG_DIR", "")
	data := defaultDataDir()
	side := &supportBundleOffline{dataDir: data, shellDir: filepath.Join(t.TempDir(), "shell"), addr: "127.0.0.1:9791"}
	want := filepath.Join(data, "logs", durableLogName)
	if paths := side.logPaths(); len(paths) == 0 || paths[0] != want {
		t.Fatalf("side daemon log paths = %v, want first %s", paths, want)
	}
	primary := &supportBundleOffline{dataDir: data, shellDir: side.shellDir}
	if paths := primary.logPaths(); len(paths) == 0 || paths[0] != filepath.Join(side.shellDir, durableLogName) {
		t.Fatalf("primary log paths = %v, want the shell data dir first", paths)
	}

	var probed []string
	supportBundleDial = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		probed = append(probed, addr)
		return nil, errors.New("no daemon listening")
	}
	var conn string
	for _, s := range side.sections() {
		if s.ID == "connection" {
			conn = s.Text
		}
	}
	if len(probed) != 1 || probed[0] != "127.0.0.1:9791" || !strings.HasPrefix(conn, "127.0.0.1:9791: ") {
		t.Fatalf("probed %v, connection %q; want only the side daemon's port", probed, conn)
	}
}

func TestSupportBundleLoopbackAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:9791":  "127.0.0.1:9791",
		" [::1]:38787 ":   "[::1]:38787",
		"localhost:8787":  "localhost:8787",
		"127.0.0.2:8787":  "127.0.0.2:8787",
		"0.0.0.0:8787":    "",
		"192.168.1.5:80":  "",
		"example.com:443": "",
		"127.0.0.1":       "",
		"127.0.0.1:0":     "",
		"127.0.0.1:70000": "",
		"127.0.0.1:http":  "",
	} {
		got, ok := supportBundleLoopbackAddr(in)
		if ok != (want != "") || got != want {
			t.Errorf("supportBundleLoopbackAddr(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestSupportBundleCLIRejectsNonLoopbackAddr(t *testing.T) {
	data, _ := supportBundleCLIEnvironment(t)
	var stdout, stderr bytes.Buffer
	out := filepath.Join(t.TempDir(), "bundle")
	if code := runSupportBundleCLI([]string{"--data", data, "--addr", "10.0.0.1:8787", "--out", out}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2 (%s)", code, stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused --addr must not write a bundle")
	}
}
