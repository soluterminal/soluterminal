package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"soluterminal-shared/voiceclean"
)

// Voice cleanup on the user's own Claude Code login. A spawned `claude -p`
// takes ~1.5 s to start and ~1 s to answer, so the daemon keeps one process
// started ahead of time (the "spare") and gives each dictation a fresh one:
// a reused process would carry earlier dictations in its conversation. The
// spare is used once, then replaced in the background. Anything that goes
// wrong (no login, a timeout, an error result) reports "failed" so the
// caller falls back to the cloud cleanup.
//
// The terminal context stays on this machine: it only reaches Anthropic
// through the user's own Claude Code, never through our cloud.

const (
	// Pinned: the bare "haiku" alias resolves to a newer thinking model,
	// which is slower and was not the model the cleanup prompt was tuned on.
	localCleanupModel    = "claude-haiku-4-5"
	localCleanupDeadline = 4 * time.Second
	localCleanupIdle     = 10 * time.Minute
	// After this many failures in a row, local cleanup pauses so every
	// dictation doesn't pay for a doomed spawn (e.g. the login expired).
	localCleanupMaxFails = 3
	localCleanupPause    = 10 * time.Minute
)

const localCleanupSchema = `{"type":"object","properties":{"cleaned":{"type":"string"}},"required":["cleaned"],"additionalProperties":false}`

var errLocalCleanupPaused = errors.New("local voice cleanup is paused after repeated failures")

// cleanProc is one started `claude -p` waiting for a single dictation.
type cleanProc struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	lines         chan string  // stdout lines; closed when the process exits
	variant       cleanVariant // which system prompt it was started with
	cleanupConfig func()
}

// cleanVariant is the system prompt a process was started with: the cleanup
// profile, and for the terminal profile whether terminal context comes too.
type cleanVariant struct {
	profile voiceclean.Profile
	withCtx bool
}

// newCleanVariant normalises a request into the prompt it needs: the
// terminal profile with or without terminal context, or the writing profile
// with or without the open conversation (local path only).
func newCleanVariant(profile voiceclean.Profile, context string) cleanVariant {
	if profile == voiceclean.ProfileWriting {
		return cleanVariant{profile: profile, withCtx: context != ""}
	}
	return cleanVariant{profile: voiceclean.ProfileCode, withCtx: context != ""}
}

func (p *cleanProc) retire() {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = killChildTree(p.cmd.Process)
		_ = p.cmd.Process.Kill()
	}
	go func() {
		_ = waitOwnedCommand(p.cmd)
		if p.cleanupConfig != nil {
			p.cleanupConfig()
		}
	}()
}

func (p *cleanProc) exited() bool {
	select {
	case _, ok := <-p.lines:
		// A line before any input is unexpected but harmless; only a
		// closed channel means the process is gone. Peeking consumes the
		// line, which is fine: nothing before the first input matters.
		return !ok
	default:
		return false
	}
}

type localCleaner struct {
	sem chan struct{} // serialises cleanups

	mu          sync.Mutex
	spare       *cleanProc
	idle        *time.Timer
	fails       int
	pausedUntil time.Time

	// Seams for tests.
	bin       func() (string, error)
	env       func(bin string) []string
	deadline  time.Duration
	idleAfter time.Duration
	now       func() time.Time
}

func newLocalCleaner() *localCleaner {
	return &localCleaner{
		sem:       make(chan struct{}, 1),
		bin:       resolveClaudeBin,
		env:       localCleanupEnv,
		deadline:  localCleanupDeadline,
		idleAfter: localCleanupIdle,
		now:       time.Now,
	}
}

// localCleanupEnv is the daemon's environment minus ANTHROPIC_*, so the run
// always uses the user's Claude login (their plan) and never an API key that
// happens to be exported, plus thinking off for speed.
func localCleanupEnv(bin string) []string {
	base := withResolvedNodeBin(os.Environ(), resolveNodeBin)
	out := make([]string, 0, len(base)+1)
	for _, kv := range providerChildEnvForBin(bin, base) {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "MAX_THINKING_TOKENS=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "MAX_THINKING_TOKENS=0")
}

func localCleanupArgs(v cleanVariant) []string {
	system := voiceclean.LocalSystemPrompt(v.profile, v.withCtx)
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", localCleanupModel,
		"--system-prompt", system,
		"--json-schema", localCleanupSchema,
		"--tools", "",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "",
		"--permission-mode", "dontAsk",
		"--no-session-persistence",
	}
}

func (c *localCleaner) spawn(v cleanVariant) (*cleanProc, error) {
	bin, err := c.bin()
	if err != nil {
		return nil, err
	}
	args, cleanupConfig, err := claudeConfigFileArgs(bin, localCleanupArgs(v))
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			cleanupConfig()
		}
	}()
	cmd := exec.Command(bin, args...)
	// A neutral directory: cleanup needs no project files or CLAUDE.md.
	cmd.Dir = os.TempDir()
	cmd.Env = c.env(bin)
	cmd.SysProcAttr = pipeChildSysProcAttr()
	applyNoWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &boundedBuf{limit: 4 << 10}
	if err := prepareProviderExec(cmd); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &cleanProc{cmd: cmd, stdin: stdin, lines: make(chan string, 64), variant: v, cleanupConfig: cleanupConfig}
	started = true
	go func() {
		defer close(p.lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
	}()
	return p, nil
}

// paused reports whether repeated failures have paused local cleanup.
func (c *localCleaner) paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Before(c.pausedUntil)
}

// Prewarm starts a spare for the given prompt variant if none is ready, so
// the next dictation skips the ~1.5 s start. Cheap to call repeatedly.
func (c *localCleaner) Prewarm(v cleanVariant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now().Before(c.pausedUntil) {
		return
	}
	if c.spare != nil && c.spare.variant == v && !c.spare.exited() {
		c.armIdleLocked()
		return
	}
	if c.spare != nil {
		c.spare.retire()
		c.spare = nil
	}
	p, err := c.spawn(v)
	if err != nil {
		return
	}
	c.spare = p
	c.armIdleLocked()
}

func (c *localCleaner) armIdleLocked() {
	if c.idle != nil {
		c.idle.Stop()
	}
	c.idle = time.AfterFunc(c.idleAfter, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.spare != nil {
			c.spare.retire()
			c.spare = nil
		}
	})
}

// take hands out the spare when it matches, else starts a process now.
func (c *localCleaner) take(v cleanVariant) (*cleanProc, error) {
	c.mu.Lock()
	if c.now().Before(c.pausedUntil) {
		c.mu.Unlock()
		return nil, errLocalCleanupPaused
	}
	p := c.spare
	c.spare = nil
	c.mu.Unlock()
	if p != nil && (p.variant != v || p.exited()) {
		p.retire()
		p = nil
	}
	if p != nil {
		return p, nil
	}
	return c.spawn(v)
}

func (c *localCleaner) record(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.fails = 0
		return
	}
	c.fails++
	if c.fails >= localCleanupMaxFails {
		c.fails = 0
		c.pausedUntil = c.now().Add(localCleanupPause)
		if c.spare != nil {
			c.spare.retire()
			c.spare = nil
		}
	}
}

// Clean polishes one dictation on the user's Claude login. Status is "ok",
// "skipped" (the model answered but its output failed a guard; keep raw) or
// "failed" (no result; the caller should fall back to the cloud).
func (c *localCleaner) Clean(ctx context.Context, profile voiceclean.Profile, rawText, termContext string) cleanupOutcome {
	return c.CleanWithLanguage(ctx, profile, rawText, termContext, "")
}

func (c *localCleaner) CleanWithLanguage(ctx context.Context, profile voiceclean.Profile, rawText, termContext, language string) cleanupOutcome {
	ctx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return cleanupOutcome{Text: rawText, Status: "failed", Detail: "busy"}
	}

	v := newCleanVariant(profile, termContext)
	if !v.withCtx {
		termContext = ""
	}
	p, err := c.take(v)
	if err != nil {
		if !errors.Is(err, errLocalCleanupPaused) {
			c.record(false)
		}
		return cleanupOutcome{Text: rawText, Status: "failed", Detail: "start: " + err.Error()}
	}
	// One dictation per process, then a fresh spare for the next one.
	defer func() {
		p.retire()
		go c.Prewarm(v)
	}()

	out := c.run(ctx, p, v.profile, rawText, termContext, language)
	out.Provider = "local/claude"
	c.record(out.Status != "failed")
	return out
}

type cleanupOutcome struct {
	Provider string
	Text     string
	Status   string // "ok" | "skipped" | "failed"
	Detail   string
	// Tokens the run used on the user's plan (from the result event).
	InputTokens, OutputTokens int
}

func (c *localCleaner) run(ctx context.Context, p *cleanProc, profile voiceclean.Profile, rawText, termContext string, language ...string) cleanupOutcome {
	_, user := voiceclean.LocalPromptForLanguage(profile, rawText, termContext, voiceclean.FirstLanguage(language))
	msg, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": user},
	})
	if _, err := p.stdin.Write(append(msg, '\n')); err != nil {
		return cleanupOutcome{Text: rawText, Status: "failed", Detail: "write: " + err.Error()}
	}
	for {
		select {
		case <-ctx.Done():
			return cleanupOutcome{Text: rawText, Status: "failed", Detail: "timeout"}
		case line, ok := <-p.lines:
			if !ok {
				return cleanupOutcome{Text: rawText, Status: "failed", Detail: "claude exited"}
			}
			if out, done := parseLocalCleanupLine(line, rawText); done {
				return out
			}
		}
	}
}

// localResultEvent is the final stream-json "result" event of a run.
type localResultEvent struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	IsError          bool   `json:"is_error"`
	Result           string `json:"result"`
	StructuredOutput *struct {
		Cleaned *string `json:"cleaned"`
	} `json:"structured_output"`
	Usage struct {
		Input       int `json:"input_tokens"`
		CacheRead   int `json:"cache_read_input_tokens"`
		CacheCreate int `json:"cache_creation_input_tokens"`
		Output      int `json:"output_tokens"`
	} `json:"usage"`
}

// parseLocalCleanupLine reads one stream-json line; done is true on the
// final "result" event.
func parseLocalCleanupLine(line, rawText string) (cleanupOutcome, bool) {
	var ev localResultEvent
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "result" {
		return cleanupOutcome{}, false
	}
	out := classifyLocalResult(ev, rawText)
	out.InputTokens = ev.Usage.Input + ev.Usage.CacheRead + ev.Usage.CacheCreate
	out.OutputTokens = ev.Usage.Output
	return out, true
}

func classifyLocalResult(ev localResultEvent, rawText string) cleanupOutcome {
	if ev.IsError || ev.Subtype != "success" {
		return cleanupOutcome{Text: rawText, Status: "failed",
			Detail: fmt.Sprintf("result %s: %s", ev.Subtype, truncateRunes(ev.Result, 120, "…"))}
	}
	if ev.StructuredOutput == nil || ev.StructuredOutput.Cleaned == nil {
		return cleanupOutcome{Text: rawText, Status: "failed", Detail: "no structured output"}
	}
	cleaned := strings.TrimSpace(*ev.StructuredOutput.Cleaned)
	if cleaned == "" {
		return cleanupOutcome{Text: rawText, Status: "skipped", Detail: "empty cleaned output"}
	}
	if reason := voiceclean.LooksLikeCommentary(cleaned, rawText); reason != "" {
		return cleanupOutcome{Text: rawText, Status: "skipped", Detail: reason}
	}
	return cleanupOutcome{Text: cleaned, Status: "ok"}
}
