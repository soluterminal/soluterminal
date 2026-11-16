package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReplayFramesChunksLargeSnapshots(t *testing.T) {
	t.Parallel()
	snapshot := bytes.Repeat([]byte("x"), MaxFramePayload+123)
	var wire bytes.Buffer

	if err := writeReplayFrames(&wire, snapshot); err != nil {
		t.Fatalf("writeReplayFrames: %v", err)
	}

	tpe, payload, err := ReadFrame(&wire)
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if tpe != FrameReplay {
		t.Fatalf("first frame type = %v, want FrameReplay", tpe)
	}
	if len(payload) != MaxFramePayload {
		t.Fatalf("first replay payload len = %d, want %d", len(payload), MaxFramePayload)
	}

	tpe, payload, err = ReadFrame(&wire)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if tpe != FrameReplay {
		t.Fatalf("second frame type = %v, want FrameReplay", tpe)
	}
	if len(payload) != 123 {
		t.Fatalf("second replay payload len = %d, want 123", len(payload))
	}

	tpe, payload, err = ReadFrame(&wire)
	if err != nil {
		t.Fatalf("end frame: %v", err)
	}
	if tpe != FrameReplayEnd {
		t.Fatalf("end frame type = %v, want FrameReplayEnd", tpe)
	}
	if len(payload) != 0 {
		t.Fatalf("end payload len = %d, want 0", len(payload))
	}
}

func TestWriteReplayWithPaintEmitsPaintBeforeEnd(t *testing.T) {
	t.Parallel()
	snapshot := []byte("scrollback bytes")
	paint := []byte("\x1b[2J\x1b[Hauthoritative paint")
	var wire bytes.Buffer

	if err := writeReplayWithPaint(&wire, snapshot, paint); err != nil {
		t.Fatalf("writeReplayWithPaint: %v", err)
	}

	tpe, payload, err := ReadFrame(&wire)
	if err != nil {
		t.Fatalf("replay frame: %v", err)
	}
	if tpe != FrameReplay {
		t.Fatalf("first frame = %v, want FrameReplay", tpe)
	}
	if !bytes.Equal(payload, snapshot) {
		t.Fatalf("replay payload = %q, want %q", payload, snapshot)
	}

	tpe, payload, err = ReadFrame(&wire)
	if err != nil {
		t.Fatalf("paint frame: %v", err)
	}
	if tpe != FrameSyntheticPaint {
		t.Fatalf("second frame = %v, want FrameSyntheticPaint", tpe)
	}
	if !bytes.Equal(payload, paint) {
		t.Fatalf("paint payload = %q, want %q", payload, paint)
	}

	tpe, _, err = ReadFrame(&wire)
	if err != nil {
		t.Fatalf("end frame: %v", err)
	}
	if tpe != FrameReplayEnd {
		t.Fatalf("third frame = %v, want FrameReplayEnd", tpe)
	}
}

func TestWriteReplayWithPaintNoPaintIsBackwardCompatible(t *testing.T) {
	t.Parallel()
	// When paint is nil/empty, the wire format must match the legacy
	// writeReplayFrames output exactly: replay chunks + end marker, no
	// FrameSyntheticPaint emitted. Daemons that haven't yet learned the
	// new frame type still see only frames they recognise.
	snapshot := []byte("hello world")
	var wire bytes.Buffer
	if err := writeReplayWithPaint(&wire, snapshot, nil); err != nil {
		t.Fatalf("writeReplayWithPaint: %v", err)
	}

	tpe, payload, err := ReadFrame(&wire)
	if err != nil || tpe != FrameReplay || !bytes.Equal(payload, snapshot) {
		t.Fatalf("first frame = (%v,%q,%v); want (FrameReplay,%q,nil)", tpe, payload, err, snapshot)
	}
	tpe, _, err = ReadFrame(&wire)
	if err != nil || tpe != FrameReplayEnd {
		t.Fatalf("second frame = (%v,%v); want FrameReplayEnd,nil", tpe, err)
	}
	if wire.Len() != 0 {
		t.Fatalf("trailing bytes after end frame: %d", wire.Len())
	}
}

func TestAgentReconnectFrameOrderIncludesInfoBeforeReplay(t *testing.T) {
	var wire bytes.Buffer
	_ = WriteFrame(&wire, FrameDims, []byte(`{"cols":120,"rows":32}`))
	_ = WriteFrame(&wire, FrameAgentInfo, []byte(`{"version":"0.0.1","pid":42}`))
	if err := writeReplayWithPaint(&wire, []byte("scrollback"), []byte("paint")); err != nil {
		t.Fatal(err)
	}
	want := []FrameType{FrameDims, FrameAgentInfo, FrameReplay, FrameSyntheticPaint, FrameReplayEnd}
	for index, typ := range want {
		got, _, err := ReadFrame(&wire)
		if err != nil || got != typ {
			t.Fatalf("frame %d = %v err=%v, want %v", index, got, err, typ)
		}
	}
}

func TestEnsureLocalBinOnPath(t *testing.T) {
	t.Parallel()
	sep := string(os.PathListSeparator)
	home := filepath.Join("/home", "u")
	localBin := filepath.Join(home, ".local", "bin")

	t.Run("prepends when absent", func(t *testing.T) {
		got := ensureLocalBinOnPath([]string{"HOME=" + home, "PATH=/usr/bin" + sep + "/bin"})
		want := "PATH=" + localBin + sep + "/usr/bin" + sep + "/bin"
		if !containsEnv(got, want) {
			t.Fatalf("PATH not prepended: %v (want %q)", got, want)
		}
	})

	t.Run("idempotent when already present", func(t *testing.T) {
		orig := "PATH=" + localBin + sep + "/usr/bin"
		got := ensureLocalBinOnPath([]string{"HOME=" + home, orig})
		if !containsEnv(got, orig) {
			t.Fatalf("PATH should be untouched: %v", got)
		}
		// And not duplicated.
		count := 0
		for _, p := range strings.Split(strings.TrimPrefix(envValue(got, "PATH"), ""), sep) {
			if p == localBin {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("~/.local/bin appears %d times, want 1", count)
		}
	})

	t.Run("no HOME → unchanged", func(t *testing.T) {
		// os.UserHomeDir may still resolve in the test env, so just assert no panic
		// and that a PATH stays parseable.
		in := []string{"PATH=/usr/bin"}
		got := ensureLocalBinOnPath(in)
		if envValue(got, "PATH") == "" {
			t.Fatalf("PATH disappeared: %v", got)
		}
	})

	t.Run("no PATH entry → introduces one", func(t *testing.T) {
		got := ensureLocalBinOnPath([]string{"HOME=" + home})
		if envValue(got, "PATH") != localBin {
			t.Fatalf("PATH not introduced: %v", got)
		}
	})
}

func TestPrependDirsOnPath(t *testing.T) {
	t.Parallel()
	t.Run("prepends and preserves path key spelling", func(t *testing.T) {
		env := []string{`Path=C:\Windows;C:\Old`, "FOO=bar"}
		got := prependDirsOnPath(env, []string{`C:\Users\alice\AppData\Local\Programs\OpenAI\Codex\bin`}, ";", true)
		want := `C:\Users\alice\AppData\Local\Programs\OpenAI\Codex\bin;C:\Windows;C:\Old`
		if envValue(got, "Path") != want {
			t.Fatalf("Path = %q, want %q", envValue(got, "Path"), want)
		}
		if !containsEnv(got, "FOO=bar") {
			t.Fatalf("lost unrelated env var: %v", got)
		}
	})

	t.Run("case insensitive dedup", func(t *testing.T) {
		dir := `C:\Users\alice\AppData\Local\Programs\OpenAI\Codex\bin`
		env := []string{`Path=c:\users\alice\appdata\local\programs\openai\codex\bin\;C:\Windows`}
		got := prependDirsOnPath(env, []string{dir}, ";", true)
		if envValue(got, "Path") != dir+`;C:\Windows` {
			t.Fatalf("Path was not deduped: %q", envValue(got, "Path"))
		}
	})

	t.Run("adds path when absent", func(t *testing.T) {
		got := prependDirsOnPath([]string{"FOO=bar"}, []string{`C:\Codex`}, ";", true)
		if envValue(got, "PATH") != `C:\Codex` {
			t.Fatalf("PATH not introduced: %v", got)
		}
	})

	t.Run("collapses duplicate Windows path keys", func(t *testing.T) {
		env := []string{`Path=C:\Windows`, "FOO=bar", `PATH=C:\Stale`}
		got := prependDirsOnPath(env, []string{`C:\Users\alice\AppData\Local\Programs\OpenAI\Codex\bin`}, ";", true)
		want := `C:\Users\alice\AppData\Local\Programs\OpenAI\Codex\bin;C:\Windows;C:\Stale`
		if envValue(got, "Path") != want {
			t.Fatalf("Path = %q, want %q", envValue(got, "Path"), want)
		}
		n := 0
		for _, kv := range got {
			if eq := strings.IndexByte(kv, '='); eq >= 0 && strings.EqualFold(kv[:eq], "PATH") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("expected exactly one PATH-like key, got %d in %v", n, got)
		}
	})
}

func TestWindowsChildPathHelpers(t *testing.T) {
	t.Parallel()
	t.Run("detects Windows shell commands from WSL", func(t *testing.T) {
		for _, cmd := range []string{
			"pwsh.exe",
			"powershell.exe",
			`/mnt/c/Program Files/PowerShell/7/pwsh.exe`,
			`C:\Windows\System32\cmd.exe`,
		} {
			if !isWindowsChildCommand(cmd) {
				t.Fatalf("expected %q to be treated as a Windows child command", cmd)
			}
		}
		if isWindowsChildCommand("/usr/bin/bash") {
			t.Fatal("bash should not be treated as a Windows child command")
		}
	})

	t.Run("converts WSL mount path to Windows path", func(t *testing.T) {
		got := hostPathToWindowsPath(`/mnt/c/Users/alice/AppData/Local/Programs/OpenAI/Codex/bin`)
		want := `C:\Users\alice\AppData\Local\Programs\OpenAI\Codex\bin`
		if got != want {
			t.Fatalf("hostPathToWindowsPath = %q, want %q", got, want)
		}
	})

	t.Run("converts Windows path to WSL host path on Linux", func(t *testing.T) {
		if os.PathListSeparator == ';' {
			t.Skip("native Windows keeps Windows paths unchanged")
		}
		got := windowsPathToHostPath(`C:\Users\alice\AppData\Local\Programs\OpenAI\Codex\bin`)
		want := `/mnt/c/Users/alice/AppData/Local/Programs/OpenAI/Codex/bin`
		if got != want {
			t.Fatalf("windowsPathToHostPath = %q, want %q", got, want)
		}
	})
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"=")
		}
	}
	return ""
}

func TestStripInheritedClaudeEnv(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_SESSION_ID=2bc01d64",
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"ANTHROPIC_API_KEY=sk-keep-me",
		"SOLU_PANE_ID=pane_x",
		"HOME=/home/u",
		"CLAUDE_CONFIG_DIR=/home/u/.claude", // not a CLAUDE_CODE_* var → kept
	}
	got := stripInheritedClaudeEnv(in)
	want := map[string]bool{
		"PATH=/usr/bin":                     true,
		"ANTHROPIC_API_KEY=sk-keep-me":      true,
		"SOLU_PANE_ID=pane_x":               true,
		"HOME=/home/u":                      true,
		"CLAUDE_CONFIG_DIR=/home/u/.claude": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d kept vars, want %d: %v", len(got), len(want), got)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Errorf("unexpected kept var: %q", kv)
		}
	}
	// The session-context vars must be gone.
	for _, kv := range got {
		if kv == "CLAUDECODE=1" || kv == "CLAUDE_CODE_SESSION_ID=2bc01d64" ||
			kv == "CLAUDE_CODE_CHILD_SESSION=1" || kv == "CLAUDE_CODE_ENTRYPOINT=cli" {
			t.Errorf("CLAUDE_CODE session var leaked through: %q", kv)
		}
	}
}
