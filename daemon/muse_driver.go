package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// newMuseSessionID mints a v4-shaped UUID for a fresh native Muse session —
// the exact shape `muse exec --session-id` accepts and
// canonicalMuseSessionID validates.
func newMuseSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// `solud muse-driver` — the long-lived child of a native Muse pane.
//
// Muse Code's only structured mode is the one-shot `muse exec --json`; the
// native pipeline needs one long-lived process per pane speaking JSONL on
// stdio (see native_muse.go). This subcommand bridges the two: it reads
// operation lines from stdin ({"op":"user","text":…} / {"op":"interrupt"}),
// runs one `muse exec --json --session-id <uuid> --prompt-file …` child per
// user turn with the child's stdout passed straight through to our stdout,
// and brackets each turn with synthetic lines the MuseAdapter understands
// (driver_init once at startup, turn_exit after each child).
//
// Safety posture: the muse child keeps its OS sandbox ON; approval prompts
// are disabled (--approval-mode never) because a headless exec has no one to
// ask — the sandbox is the guard. --user-input-auto-resolve keeps a turn
// from stalling on request_user_input.
//
// Turn discipline: strictly serial. A user line arriving mid-turn queues;
// interrupt kills the CURRENT child's process group (TERM, then KILL after
// 2s) and also drains the queue — an interrupt means "stop", not "stop and
// then run everything I queued".
func runMuseDriver() {
	fs := flag.NewFlagSet("muse-driver", flag.ExitOnError)
	sessionID := fs.String("session-id", "", "durable muse session uuid (required)")
	model := fs.String("model", "", "muse model id (optional)")
	effort := fs.String("effort", "", "muse reasoning effort (optional)")
	provider := fs.String("provider", "", "muse provider override (testing: echo)")
	_ = fs.Parse(os.Args[2:])
	if *sessionID == "" {
		fmt.Fprintln(os.Stderr, "muse-driver: --session-id is required")
		os.Exit(2)
	}

	out := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	cwd, _ := os.Getwd()
	emit(map[string]any{
		"solu_muse":  "driver_init",
		"session_id": *sessionID,
		"cwd":        cwd,
	})

	museBin, err := resolveMuseBin()
	if err != nil {
		emit(map[string]any{"solu_muse": "turn_exit", "code": 127, "stderr_tail": err.Error()})
		// Stay alive so the pane shows the error instead of dying instantly;
		// every turn will re-fail with the same message until muse appears.
	}

	type op struct {
		Op   string `json:"op"`
		Text string `json:"text"`
	}
	userCh := make(chan string, 16)
	intCh := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var o op
			if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
				continue
			}
			switch o.Op {
			case "user":
				userCh <- o.Text
			case "interrupt":
				select {
				case intCh <- struct{}{}:
				default:
				}
			}
		}
		close(userCh)
	}()

	for {
		var text string
		var ok bool
		select {
		case text, ok = <-userCh:
			if !ok {
				return // stdin closed — pane is going away
			}
		case <-intCh:
			continue // stray interrupt between turns
		}

		if museBin == "" {
			if museBin, err = resolveMuseBin(); err != nil {
				emit(map[string]any{"solu_muse": "turn_exit", "code": 127, "stderr_tail": err.Error()})
				continue
			}
		}
		code, interrupted, stderrTail := runMuseTurn(museBin, *sessionID, *model, *effort, *provider, text, intCh)
		if interrupted {
			// Deliberately drop anything queued behind an interrupt.
			for {
				select {
				case <-userCh:
					continue
				default:
				}
				break
			}
		}
		emit(map[string]any{
			"solu_muse":   "turn_exit",
			"code":        code,
			"interrupted": interrupted,
			"stderr_tail": stderrTail,
		})
	}
}

// musePasteImageRE finds daemon paste-dir image references in prompt text, in
// either the Claude `@/path` mention form or the plain-path form Muse panes
// receive (see imageInjectText). Pasted uploads always land as
// ~/.soluterminal/pastes/solu-paste-<hex>.<ext>, so matching is pinned to
// that shape rather than any absolute image path: a prompt merely discussing
// a system image path must not attach vision bytes.
var musePasteImageRE = regexp.MustCompile("@?((?:[A-Za-z]:)?[\\\\/][^\\s'\"`()\\[\\]]*?\\.soluterminal[\\\\/]pastes[\\\\/]solu-paste-[A-Za-z0-9]+\\.(?:png|jpg|jpeg|gif|webp))")

// musePasteImagePaths extracts the existing paste-dir images referenced by a
// prompt, deduplicated in first-seen order. Missing, empty, oversized, or
// non-regular files are skipped: the text mention stays in the prompt as the
// caption reference either way, so a vanished upload degrades to text instead
// of failing the turn.
func musePasteImagePaths(prompt string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range musePasteImageRE.FindAllStringSubmatch(prompt, -1) {
		p := strings.TrimRight(m[1], ".,;:!?)]}'\"")
		if seen[p] {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxPasteImageBytes {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// runMuseTurn runs one muse exec child to completion (or interrupt). The
// child's stdout is inherited (fd passthrough) so Spark JSONL lines reach the
// pane without a copy loop; stderr is captured for the turn_exit tail.
func runMuseTurn(museBin, sessionID, model, effort, provider, prompt string, intCh <-chan struct{}) (code int, interrupted bool, stderrTail string) {
	pf, err := os.CreateTemp("", "solu-muse-prompt-*.txt")
	if err != nil {
		return 1, false, "prompt tempfile: " + err.Error()
	}
	defer os.Remove(pf.Name())
	if _, err := pf.WriteString(prompt); err != nil {
		pf.Close()
		return 1, false, "prompt write: " + err.Error()
	}
	pf.Close()

	// --disable-approval (exec's flag; --approval-mode is TUI-only) with the
	// OS sandbox left ON — the sandbox is the guard for a headless run that
	// has no one to ask. --user-input-auto-resolve keeps request_user_input
	// from stalling the turn.
	args := []string{
		"exec", "--json",
		"--session-id", sessionID,
		"--prompt-file", pf.Name(),
		"--disable-approval",
		"--user-input-auto-resolve",
	}
	// Pasted images ride as --image flags: muse exec never resolves `@path`
	// or bare-path text mentions into vision bytes (that inline resolution is
	// Claude Code syntax), so without this the model only ever saw the path
	// string. The text mention stays in the prompt as the caption reference.
	for _, img := range musePasteImagePaths(prompt) {
		args = append(args, "--image", img)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	if base := os.Getenv("SOLU_TEAM_BROKER_BASE_URL"); base != "" {
		args = append(args, "--base-url", base)
	}

	var stderr bytes.Buffer
	// Stays on the driver's console: its stdout can be the pane's console itself.
	c := exec.Command(museBin, args...)
	c.Env = providerChildEnvForBin(museBin, os.Environ())
	c.Stdout = os.Stdout
	c.Stderr = &stderr
	c.SysProcAttr = museTurnSysProcAttr()
	if err := startWithoutCoreDumps(c); err != nil {
		return 127, false, "start muse: " + err.Error()
	}

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for {
		select {
		case err := <-done:
			code = 0
			if err != nil {
				code = 1
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				}
			}
			return code, interrupted, tailOf(stderr.String(), 500)
		case <-intCh:
			interrupted = true
			// TERM the whole group (the muse launcher execs the real binary,
			// but children may linger); escalate to KILL if it won't die.
			museSignalTurn(c.Process, false)
			select {
			case err := <-done:
				code = 130
				_ = err
				return code, true, tailOf(stderr.String(), 500)
			case <-time.After(2 * time.Second):
				museSignalTurn(c.Process, true)
				<-done
				return 137, true, tailOf(stderr.String(), 500)
			}
		}
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// nativeMuseArgv builds the native Muse pane child argv: the daemon binary's
// own muse-driver subcommand (see runMuseDriver). The caller mints
// opts.MuseSessionID first — the driver requires it.
func nativeMuseArgv(sessionID, model, effort string) []string {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = os.Args[0]
	}
	argv := []string{self, "muse-driver", "--session-id", sessionID}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
	}
	return argv
}
