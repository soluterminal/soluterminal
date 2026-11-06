package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// npm's standard Node cmd-shim (here for @openai/codex). Only this exact wrapper is
// bypassed: a custom .cmd may contain routing or environment policy we must
// preserve. Execute the same installed JS entrypoint with the same Node choice,
// avoiding cmd.exe's second parse of paths, prompts and TOML -c values.
const npmCodexNodeShim = `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0

IF EXIST "%dp0%\node.exe" (
  SET "_prog=%dp0%\node.exe"
) ELSE (
  SET "_prog=node"
  SET PATHEXT=%PATHEXT:;.JS;=;%
)

endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%"  "%dp0%\node_modules\@openai\codex\bin\codex.js" %*
`

// Claude's current Windows npm package supplies a native executable. Older
// Node-based Claude packages use the same Node shim with cli.js as the target.
const npmClaudeNativeShim = `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0
"%dp0%\node_modules\@anthropic-ai\claude-code\bin\claude.exe"   %*
`

func npmProviderShimEntrypoint(bin string) (target string, needsNode bool) {
	name := strings.ToLower(filepath.Base(bin))
	if name != "codex.cmd" && name != "claude.cmd" {
		return "", false
	}
	f, err := os.Open(bin)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return "", false
	}
	shim := strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))
	if name == "codex.cmd" && shim == strings.TrimSpace(npmCodexNodeShim) {
		return filepath.Join(filepath.Dir(bin), "node_modules", "@openai", "codex", "bin", "codex.js"), true
	}
	if name == "claude.cmd" {
		if shim == strings.TrimSpace(npmClaudeNativeShim) {
			return filepath.Join(filepath.Dir(bin), "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe"), false
		}
		nodeShim := strings.ReplaceAll(npmCodexNodeShim, `node_modules\@openai\codex\bin\codex.js`, `node_modules\@anthropic-ai\claude-code\cli.js`)
		if shim == strings.TrimSpace(nodeShim) {
			return filepath.Join(filepath.Dir(bin), "node_modules", "@anthropic-ai", "claude-code", "cli.js"), true
		}
	}
	return "", false
}

func prepareProviderExec(c *exec.Cmd) error {
	if c.Err != nil {
		return nil // leave LookPath's ErrDot and other checks for Start to enforce
	}
	if len(c.Args) == 0 {
		return nil
	}
	bin, args, err := providerBatchArgv(c.Path, c.Args[1:], c.Env)
	if err != nil {
		return err
	}
	if bin != c.Path {
		c.Path, c.Args = bin, append([]string{bin}, args...)
	}
	return nil
}
