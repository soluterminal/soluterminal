package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codex 0.157 introduced a shared app-server for interactive sessions. Its
// approval UI can appear in another session, so terminal panes use Codex's
// supported in-process mode when the installed CLI offers it. Headless tools
// and explicitly selected remote transports keep their original behavior.
func codexPaneNeedsNoDaemon(cmd []string) bool {
	return codexPaneNeedsOption(cmd, "--no-daemon")
}

// Both terminal defaults must agree on utility commands, option values and
// explicit transports. An existing one of the flags must not suppress the
// other. Stop parsing at -- so prompt text never masquerades as a CLI flag.
func codexPaneNeedsOption(cmd []string, option string) bool {
	if !cmdStartsCodex(cmd) {
		return false
	}
	skipValue, sawPositional := false, false
	for _, arg := range cmd[1:] {
		if skipValue {
			skipValue = false
			continue
		}
		if arg == "--" {
			break // remaining arguments are prompts/session names, not options
		}
		if arg == option || arg == "--remote" || strings.HasPrefix(arg, "--remote=") {
			return false
		}
		switch arg {
		case "--help", "-h", "--version", "-V":
			return false
		case "-c", "--config", "--enable", "--disable", "--remote-auth-token-env", "-i", "--image", "-m", "--model", "--local-provider", "-p", "--profile", "-s", "--sandbox", "-C", "--cd", "--add-dir", "-a", "--ask-for-approval":
			// A profile named "help" or directory named "review" is not a
			// utility command. Attached option values stay in their own token.
			skipValue = true
			continue
		}
		if strings.HasPrefix(arg, "-") || sawPositional {
			continue
		}
		sawPositional = true
		// Only the first positional argument can select a subcommand.
		// Later arguments include resume/fork session names and prompts.
		switch arg {
		case "agents", "exec", "e", "review", "login", "logout", "mcp", "plugin", "app-server", "remote-control", "completion", "update", "doctor", "sandbox", "debug", "apply", "a", "queue", "archive", "delete", "migrate-rollouts", "unarchive", "cloud", "exec-server", "features", "help":
			return false
		}
	}
	return true // an initial prompt, resume, or fork starts the interactive TUI
}

func codexSupportsNoDaemon(bin string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := hiddenCommand(ctx, bin, "--help")
	cmd.Env = providerChildEnvForBin(bin, os.Environ())
	out, err := cmd.Output()
	return err == nil && strings.Contains(string(out), "--no-daemon")
}

func codexPaneLaunchCommand(cmd []string) []string {
	if !codexPaneNeedsNoDaemon(cmd) || !codexSupportsNoDaemon(cmd[0]) {
		return cmd
	}
	isolated := make([]string, 0, len(cmd)+1)
	isolated = append(isolated, cmd[0], "--no-daemon")
	return append(isolated, cmd[1:]...)
}

// Bash imports this exported function for a shell pane. It covers `codex`
// typed later at the prompt, after the pane's original command has launched.
// The SAME function also supplies inline scrollback at interactive prompts.
// Do not export a second codex function: Bash would silently replace this one.
// The function probes the installed CLI so older versions still run normally.
// Keep it comma-free for the pane --env wire format and compatible with zsh.
const codexPaneBashFunction = `() {
  local arg help target=codex skip_value=0 saw_positional=0 need_daemon=1 need_inline=0
  if [[ -n "$SOLU_CODEX_INLINE_SHIM_DIR" ]]; then
    target="$SOLU_CODEX_INLINE_SHIM_DIR/codex"
  fi
  case $- in *i*) need_inline=1 ;; esac
  for arg in "$@"; do
    if [[ "$skip_value" == 1 ]]; then
      skip_value=0
      continue
    fi
    case "$arg" in
      --) break ;;
      --no-daemon) need_daemon=0; continue ;;
      --no-alt-screen) need_inline=0; continue ;;
      --remote|--remote=*) command "$target" "$@"; return ;;
      --help|-h|--version|-V) command "$target" "$@"; return ;;
      -c|--config|--enable|--disable|--remote-auth-token-env|-i|--image|-m|--model|--local-provider|-p|--profile|-s|--sandbox|-C|--cd|--add-dir|-a|--ask-for-approval)
        skip_value=1; continue ;;
      -*) continue ;;
    esac
    if [[ "$saw_positional" == 1 ]]; then
      continue
    fi
    saw_positional=1
    case "$arg" in
      agents|exec|e|review|login|logout|mcp|plugin|app-server|remote-control|completion|update|doctor|sandbox|debug|apply|a|queue|archive|delete|migrate-rollouts|unarchive|cloud|exec-server|features|help)
        command "$target" "$@"; return ;;
    esac
  done
  if [[ "$need_inline" == 1 ]]; then
    set -- --no-alt-screen "$@"
  fi
  if [[ "$need_daemon" == 1 ]]; then
    help=$(command "$target" --help 2>/dev/null) || help=
    if [[ "$help" == *--no-daemon* ]]; then
      set -- --no-daemon "$@"
    fi
  fi
  command "$target" "$@"
}`

func codexPaneShellEnv(child string, env []string) []string {
	if filepath.Base(child) != "bash" {
		return env
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "SOLU_PANE_ID=") && strings.TrimPrefix(entry, "SOLU_PANE_ID=") != "" {
			// Remove inherited/spawn-provided definitions first. Duplicate
			// environment keys have platform-dependent first/last semantics.
			out := make([]string, 0, len(env)+1)
			for _, kv := range env {
				if !strings.HasPrefix(kv, "BASH_FUNC_codex%%=") && !strings.HasPrefix(kv, "BASH_FUNC_claude%%=") {
					out = append(out, kv)
				}
			}
			return append(append(out, codexInlineBashEnv), paneShellBashEnv...)
		}
	}
	return env
}
