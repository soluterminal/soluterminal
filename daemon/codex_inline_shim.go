package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
)

const codexInlineShimDirName = "codex-inline-bin"
const codexInlineShimEnvKey = "SOLU_CODEX_INLINE_SHIM_DIR"

// A same-user symlink to solud gives Unix shells the inline Codex default
// through PATH without changing the user's Codex configuration.
func ensureCodexInlineShim(dataDir string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(dataDir, codexInlineShimDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Other pane shims (extensions.go) share the directory.
	for _, shim := range paneShims {
		if err := ensureShimLink(dir, shim.name, self); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func init() { registerPaneShim("codex", runCodexInlineShim) }

func ensureShimLink(dir, name, self string) error {
	path := filepath.Join(dir, name)
	if target, err := os.Readlink(path); err == nil && target == self {
		return nil
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+"-"+id)
	if err := os.Symlink(self, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

func pathWithoutCodexShim(path, shimDir string) string {
	parts := strings.Split(path, string(os.PathListSeparator))
	filtered := parts[:0]
	for _, part := range parts {
		if part != shimDir {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, string(os.PathListSeparator))
}

func codexFromPath(path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, "codex")
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

// runCodexInlineShim is called only when solud was invoked via the pane-local
// `codex` symlink. Remove the shim directory before resolving the real CLI so
// shells can select any Codex install/version without a recursive launch.
func runCodexInlineShim(cliArgs []string) int {
	shimDir := os.Getenv(codexInlineShimEnvKey)
	if shimDir == "" {
		fmt.Fprintln(os.Stderr, "codex: SoluTerminal shim environment missing")
		return 127
	}
	realPath := pathWithoutCodexShim(os.Getenv("PATH"), shimDir)
	bin, err := codexFromPath(realPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex: command not found")
		return 127
	}
	// Do this before even probing the CLI. An unavailable selected team route
	// must not start a personal login or reuse a stale parent capability.
	childEnv := codexShimEnvironment(cliArgs, os.Environ())
	if paneBroker.codexShellLaunch != nil {
		cliArgs, childEnv, err = paneBroker.codexShellLaunch(bin, cliArgs, childEnv)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex:", err)
		return 1
	}
	// Without the machine-wide registration, this pane's Codex still gets
	// SoluTerminal's tools, for this process only.
	if rec, ok := agentInjectionRecord(filepath.Dir(shimDir)); ok {
		cliArgs = codexInjectArgs(rec, os.Getenv("SOLU_CONTEXT_PACK"), cliArgs)
	}
	args := append([]string{bin}, cliArgs...)
	if inline := codexInlineTerminalCmd(args); len(inline) > 0 {
		args = inline
	}
	if live := codexLiveShellCommand(args, childEnv, shimDir); len(live) > 0 {
		c := exec.Command(live[0], live[1:]...)
		c.Env = append(filterOutEnvKeys(childEnv, []string{"PATH"}), "PATH="+realPath)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := runShimCLI(c); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			fmt.Fprintln(os.Stderr, "codex: live authentication could not start")
			return 1
		}
		return 0
	}
	args = codexPaneLaunchCommand(args)
	c := exec.Command(bin, args[1:]...)
	c.Env = append(filterOutEnvKeys(childEnv, []string{"PATH"}), "PATH="+realPath)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := runShimCLI(c); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "codex:", err)
		return 1
	}
	return 0
}

// codexShimEnvironment is the environment a typed `codex` starts from. A
// retained shell can outlive the daemon that gave it its address, so a model
// invocation follows this data directory's current owner (live panes connect
// to it); utility invocations keep the inherited environment untouched.
func codexShimEnvironment(args, env []string) []string {
	if model, _ := codexUnixModelInvocation(args); model {
		return shellDaemonEnvironment(env)
	}
	return env
}

// The shim already resolved the user's executable and broker route. PATH is
// therefore no longer an override to resolve; all authentication overrides
// still pass through the same conservative preflight as direct pane launches.
func codexLiveShellCommand(args, env []string, shimDir string) []string {
	paneID := envValueFromList(env, "SOLU_PANE_ID")
	address := envValueFromList(env, paneDaemonURLEnv)
	if paneID == "" || filepath.Base(paneID) != paneID || !filepath.IsAbs(shimDir) || filepath.Base(shimDir) != codexInlineShimDirName {
		return nil
	}
	if _, err := codexShellDaemonURL(address); err != nil {
		return nil
	}
	dir := filepath.Dir(shimDir)
	if paneBrokerActive(env) || !codexLiveEnabled(dir) || !codexLiveLaunchReady(args, filterOutEnvKeys(env, []string{"PATH"})) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	return append([]string{self, "codex-terminal", "--data-dir", dir, "--daemon-url", address, "--"}, args...)
}

// runShimCLI runs the real Codex CLI for the pane-local shim, through the
// launch broker's runner when one is registered.
func runShimCLI(c *exec.Cmd) error {
	if paneBroker.runShellCLI != nil {
		return paneBroker.runShellCLI(c)
	}
	return runShimProcess(c, nil)
}

// runShimProcess runs a pane-local shim's real CLI. An interrupt reaches the
// whole foreground process group, so the shim must outlive the CLI: killed
// first, it would hand the prompt back while the CLI still ran, and skip
// after. A caught signal is reset to its default in the child, so the CLI
// still receives the interrupt itself. after (optional) runs once the CLI has
// exited, while interrupts are still ignored.
func runShimProcess(c *exec.Cmd, after func()) error {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	err := c.Run()
	if after != nil {
		after()
	}
	return err
}
