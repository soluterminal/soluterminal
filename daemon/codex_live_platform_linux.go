//go:build linux

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func codexLiveSupported() bool { return true }

func codexLivePaneHasBridge(p *Pane) bool {
	for _, pid := range p.ChildPids() {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if len(args) > 1 && args[1] == "codex-terminal" {
			return true
		}
	}
	return false
}

func codexLiveSpawnCommand(self, dir, daemonURL string, cmd []string, brokerOverrides ...[]string) ([]string, error) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		return nil, err
	}
	args := []string{self, "codex-terminal", "--data-dir", dir, "--daemon-url", daemonURL}
	if len(brokerOverrides) > 0 && len(brokerOverrides[0]) > 0 {
		encoded, _ := json.Marshal(brokerOverrides[0])
		args = append(args, "--broker-config", string(encoded))
	}
	args = append(args, "--")
	args = append(args, cmd...)
	args = daemonCommandArgv(args[0], args[1:]...)
	for i := range args {
		args[i] = shellSingleQuote(args[i])
	}
	return []string{bash, "-lc", loginShellInit() + "; " + strings.Join(args, " ") + "; " + codexLiveExitShellEnv(daemonURL) + "exec " + shellSingleQuote(bash) + " -i"}, nil
}

// The pane drops to an interactive shell when Codex exits. Spawn only gives
// shell panes the daemon address, so without it the shim declines its live
// path and a `codex resume` typed there runs without the bridge (the pane
// then reports reconnect-required). Export it to that shell alone, so Codex's
// own tool shells are unaffected.
func codexLiveExitShellEnv(daemonURL string) string {
	if _, err := codexShellDaemonURL(daemonURL); err != nil {
		return ""
	}
	return "export " + paneDaemonURLEnv + "=" + shellSingleQuote(daemonURL) + "; "
}

func codexLivePrivateDirectory(path string) error { return os.Chmod(path, 0700) }
