//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// codexForeground is the pane's single foreground native Codex, read once so
// every reconnect decision describes the same process.
type codexForeground struct {
	// cwd is where it actually works: a later `cd` or exit-to-shell relaunch
	// moves it away from the pane's recorded directory. A deleted directory is
	// still reported so the caller refuses instead of falling back.
	cwd string
	// underShell: an interactive shell prompt sits above it, which a
	// reconnect replaces, and the pane's recorded command no longer describes it.
	underShell bool
	// args is `codex <args>` when the invocation is proven safe to replay:
	// default login, no auth or provider override, and naming no other
	// conversation (shell variables are not carried over). Nil otherwise.
	args []string
}

func codexForegroundOf(p *Pane, thread string) (codexForeground, bool) {
	if p == nil || p.Pid <= 0 {
		return codexForeground{}, false
	}
	return codexForegroundAt("/proc", p.ID, p.Pid, thread, descendantPids(p.Pid))
}

func codexForegroundAt(procRoot, paneID string, root int, thread string, pids []int) (codexForeground, bool) {
	var fg codexForeground
	dir, found := codexForegroundNativeAt(procRoot, pids)
	if !found {
		return fg, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil {
		return fg, false
	}
	env := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if envValueFromList(env, "SOLU_PANE_ID") != paneID {
		return fg, false
	}
	if fg.cwd, err = os.Readlink(filepath.Join(dir, "cwd")); err != nil || !filepath.IsAbs(fg.cwd) {
		return codexForeground{}, false
	}
	pid, _ := strconv.Atoi(filepath.Base(dir))
	for depth := 0; depth < 16; depth++ {
		pid = procParentAt(procRoot, pid)
		if pid <= 1 || pid == root {
			break
		}
		pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
		argv := strings.Split(strings.TrimRight(readProcFile(pidDir, "cmdline"), "\x00"), "\x00")
		if interactiveShellProcess(strings.TrimSpace(readProcFile(pidDir, "comm")), argv) {
			fg.underShell = true
		}
	}
	fg.args = codexReplayableArgs(env, strings.Split(strings.TrimRight(readProcFile(dir, "cmdline"), "\x00"), "\x00"), thread)
	return fg, true
}

// codexForegroundNativeAt finds the pane's single foreground native Codex.
func codexForegroundNativeAt(procRoot string, pids []int) (string, bool) {
	found := ""
	for _, pid := range pids {
		dir := filepath.Join(procRoot, strconv.Itoa(pid))
		if providerProcessNameAt(procRoot, pid) != "codex" || !codexProcessIsForeground(dir) {
			continue
		}
		// The pane's `codex` shim is solud under that name. Only the real CLI
		// binary carries the conversation's argv and environment.
		exe, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil || filepath.Base(strings.TrimSuffix(exe, " (deleted)")) != "codex" {
			continue
		}
		if found != "" {
			return "", false
		}
		found = dir
	}
	return found, found != ""
}

func codexReplayableArgs(env, argv []string, thread string) []string {
	if _, ok := canonicalCodexThreadID(thread); !ok || len(argv) == 0 || argv[0] == "" || paneRouteActive(env) {
		return nil
	}
	for _, key := range []string{"CODEX_HOME", "OPENAI_API_KEY", "OPENAI_BASE_URL", "SOLU_CODEX_RELAY_DIR"} {
		if envValueFromList(env, key) != "" {
			return nil
		}
	}
	// The pane's shell function adds these terminal defaults ahead of any
	// subcommand; spawn adds them back, and they would hide `resume <id>`.
	var args []string
	for _, arg := range argv[1:] {
		if arg != "--no-daemon" && arg != "--no-alt-screen" {
			args = append(args, arg)
		}
	}
	if codexShellProviderOverride(args) {
		return nil // another provider or credential store
	}
	for i, arg := range args {
		if arg == "resume" || arg == "fork" {
			// A different (or unnamed) conversation than the pane's recorded one
			// is not proven to be the thread we would resume.
			if arg == "fork" || i+1 >= len(args) {
				return nil
			}
			if id, ok := canonicalCodexThreadID(args[i+1]); !ok || id != thread {
				return nil
			}
			break
		}
	}
	return append([]string{"codex"}, args...)
}

func procParentAt(procRoot string, pid int) int {
	stat := readProcFile(filepath.Join(procRoot, strconv.Itoa(pid)), "stat")
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// Only a shell with options alone is a prompt the user can return to. A
// command string (`bash -lc '…'`, `--command`) or a script operand
// (`bash /path/launcher`, env shebangs) is a launch wrapper.
func interactiveShellProcess(comm string, argv []string) bool {
	switch comm {
	case "bash", "zsh", "sh", "dash", "fish", "ksh":
	default:
		return false
	}
	for _, arg := range argv[min(1, len(argv)):] {
		if arg == "" {
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "--command" || strings.HasPrefix(arg, "--command=") {
			return false
		}
		if !strings.HasPrefix(arg, "--") && strings.Contains(arg, "c") {
			return false
		}
	}
	return true
}
