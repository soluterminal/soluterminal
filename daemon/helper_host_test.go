package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"soluterminal/helperproto"
)

// The env every Linux pane spawn carries today must pass; host secrets and
// unlisted SOLU_ keys must not.
func TestCheckHelperSpawnEnv(t *testing.T) {
	ok := map[string]string{
		"SOLU_PANE_ID": "pane_0123456789abcdef", "SOLU_CONTEXT_PACK": "/x",
		"SOLU_CODEX_PANE_POLICY": "team", "SOLU_QUOTA_WORKER": "1", "SOLU_HARNESS_RESUME_SESSION": "s",
		"CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN": "1", "BASH_FUNC_codex%%": "() { :; }", "CLAUDE_CONFIG_DIR": "/c",
	}
	if err := checkHelperSpawnEnv(ok); err != nil {
		t.Fatalf("today's pane env refused: %v", err)
	}
	for _, k := range []string{
		"SOLU_TEAM_BROKER_TOKEN", "SOLU_TEAM_BROKER_DAEMON_URL", "SOLU_TEAM_BROKER_BASE_URL", "SOLU_TEAM_BROKER_PROVIDER",
		"ANTHROPIC_AUTH_TOKEN", "META_API_KEY", "SOLU_OWNER_TOKEN", "SOLU_RELAY_TOKEN", "SOLU_UPDATE_SIGNING_KEY",
		"SOLU_SOMETHING_NEW", "solu_pane_id", "GITHUB_TOKEN", "MY_CLIENT_SECRET", "OPENAI_API_KEY", "X_CLAIM",
		"X_SOLU_DEVICE_KEY", "FOO_SOLU_TEAM_BROKER_BASE_URL",
	} {
		env := map[string]string{"SOLU_PANE_ID": "pane_0123456789abcdef", k: "v"}
		if err := checkHelperSpawnEnv(env); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s was allowed: %v", k, err)
		}
	}
	sup := newHelperSupervisor(helperSupervisorConfig{Distro: "x", Place: func(context.Context) (helperPlacement, error) {
		t.Fatal("a refused spawn must not even place the helper")
		return helperPlacement{}, nil
	}})
	_, err := sup.Spawn(context.Background(), helperproto.SpawnArgs{Env: map[string]string{"SOLU_TEAM_BROKER_TOKEN": "t"}})
	if helperproto.ErrorCode(err) != helperproto.CodeBadArgs {
		t.Fatalf("supervisor spawn with a secret: %v", err)
	}
}

func TestHelperWSLEnv(t *testing.T) {
	got := helperWSLEnv([]string{
		"SystemRoot=C:\\Windows", "Path=C:\\Windows\\system32", "USERPROFILE=C:\\Users\\u",
		"WSLENV=GITHUB_TOKEN/u:SOLU_TOKEN", "GITHUB_TOKEN=ghp_x", "SOLU_TOKEN=t", "ANTHROPIC_API_KEY=k", "RANDOM=1",
	})
	joined := strings.Join(got, "\n")
	for _, keep := range []string{"SystemRoot=", "Path=", "USERPROFILE="} {
		if !strings.Contains(joined, keep) {
			t.Fatalf("dropped %s: %q", keep, got)
		}
	}
	for _, drop := range []string{"WSLENV", "GITHUB_TOKEN", "SOLU_TOKEN", "ANTHROPIC_API_KEY", "RANDOM"} {
		if strings.Contains(joined, drop) {
			t.Fatalf("%s reached wsl.exe: %q", drop, got)
		}
	}
}

func utf16le(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = append(b, 0xff, 0xfe)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func TestWSLListing(t *testing.T) {
	quiet := "Ubuntu\r\nUbuntu Dev\r\ndocker-desktop\r\n"
	verbose := "  NAME            STATE           VERSION\r\n  Ubuntu          Stopped         2\r\n* Ubuntu Dev      Running         2\r\n  docker-desktop  Stopped         2\r\n"
	for _, raw := range [][]byte{utf16le(verbose, true), utf16le(verbose, false), []byte(verbose)} {
		if got := decodeWSLListing(raw); got != verbose {
			t.Fatalf("decode: %q", got)
		}
	}
	if got := decodeWSLListing(append(utf16le("Ubuntu", false), 'x')); got != "Ubuntu" {
		t.Fatalf("odd trailing byte: %q", got)
	}
	names := wslDistroNames(decodeWSLListing(utf16le(quiet, true)))
	if len(names) != 3 || names[1] != "Ubuntu Dev" {
		t.Fatalf("names %q", names)
	}
	if d, ok := wslDefaultDistro(verbose, quiet); !ok || d != "Ubuntu Dev" {
		t.Fatalf("default %q %v (must not stop at \"Ubuntu\")", d, ok)
	}
	// Localized headings and state words are never read.
	loc := "  NAME      ZUSTAND      VERSION\n* Ubuntu    Wird ausgeführt  2\n"
	if d, ok := wslDefaultDistro(loc, "Ubuntu\n"); !ok || d != "Ubuntu" {
		t.Fatalf("localized default %q %v", d, ok)
	}
	if _, ok := wslDefaultDistro("  NAME STATE VERSION\n  Ubuntu  Stopped  2\n", "Ubuntu\n"); ok {
		t.Fatal("no * row means no default")
	}
}

// localDistroRunner runs the placement scripts with a local sh and HOME, as
// wsl.exe --exec would inside a distro.
func localDistroRunner(home string, calls *[]string) helperDistroRunner {
	return func(ctx context.Context, script string, args []string, stdin io.Reader) ([]byte, error) {
		*calls = append(*calls, strings.SplitN(script, "\n", 2)[0])
		cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		cmd.Stdin = stdin
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, errors.New(err.Error() + ": " + stderr.String())
		}
		return out, nil
	}
}

func TestPlaceHelper(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe uses GNU stat -c and sha256sum, as in a WSL distro")
	}
	home := t.TempDir()
	payload := []byte("#!/bin/sh\necho fake solud\n")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	source := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	var calls []string
	run := localDistroRunner(home, &calls)
	ctx := context.Background()

	p, err := placeHelper(ctx, run, "0.0.1250", want, source)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local/lib/soluterminal/helper/solud-0.0.1250")
	if p.BinPath != bin || p.UID != os.Getuid() || p.Arch == "" {
		t.Fatalf("placement %+v", p)
	}
	for path, mode := range map[string]os.FileMode{bin: 0o700, filepath.Dir(bin): 0o700} {
		if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", path, fi, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), ".solud-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	// Already right: probed, not re-placed.
	calls = nil
	if _, err := placeHelper(ctx, run, "0.0.1250", want, source); err != nil || len(calls) != 1 {
		t.Fatalf("second placement ran %v (%v)", calls, err)
	}
	// Changed in the distro: re-placed before the start.
	_ = os.WriteFile(bin, []byte("tampered"), 0o700)
	if _, err := placeHelper(ctx, run, "0.0.1250", want, source); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); !bytes.Equal(b, payload) {
		t.Fatal("a tampered helper was not replaced")
	}
	// The right bytes but not private any more: placed again, 0700.
	_ = os.Chmod(bin, 0o755)
	if _, err := placeHelper(ctx, run, "0.0.1250", want, source); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(bin); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode after repair %v", fi.Mode())
	}
	// A source that doesn't match the manifest is never accepted.
	bad := strings.Repeat("0", 64)
	if _, err := placeHelper(ctx, run, "0.0.1250", bad, source); err == nil {
		t.Fatal("a binary that doesn't match the manifest was accepted")
	}
	// Bad inputs never reach the distro.
	calls = nil
	for _, v := range []string{"", "1;rm -rf ~", "../x", "a b"} {
		if _, err := placeHelper(ctx, run, v, want, source); err == nil {
			t.Fatalf("version %q accepted", v)
		}
	}
	if _, err := placeHelper(ctx, run, "0.0.1250", "nothex", source); err == nil || len(calls) != 0 {
		t.Fatal("a bad manifest hash reached the distro")
	}
	// A symlinked helper directory is refused, even holding the right file.
	home2 := t.TempDir()
	elsewhere := t.TempDir()
	_ = os.Chmod(elsewhere, 0o700)
	_ = os.WriteFile(filepath.Join(elsewhere, "solud-0.0.1250"), payload, 0o700)
	_ = os.MkdirAll(filepath.Join(home2, ".local/lib/soluterminal"), 0o700)
	_ = os.Symlink(elsewhere, filepath.Join(home2, ".local/lib/soluterminal/helper"))
	if _, err := placeHelper(ctx, localDistroRunner(home2, &calls), "0.0.1250", want, source); err == nil {
		t.Fatal("placed through a symlinked directory")
	}
}

func TestPlaceHelperRefusesOtherArchitectures(t *testing.T) {
	run := func(context.Context, string, []string, io.Reader) ([]byte, error) {
		return []byte("uid=1000\narch=aarch64\nhome=/home/u\n"), nil
	}
	_, err := placeHelper(context.Background(), run, "0.0.1250", strings.Repeat("a", 64), func() (io.ReadCloser, error) {
		t.Fatal("nothing is placed on an unsupported arch")
		return nil, nil
	})
	if !errors.Is(err, errHelperArchUnsupported) {
		t.Fatalf("arm64: %v", err)
	}
}
