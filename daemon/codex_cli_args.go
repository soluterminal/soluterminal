package main

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Codex command-line facts shared by the Codex shims, live reconnect and
// pane launches.

// paneDaemonURLEnv carries this daemon's loopback address into terminal
// shells, so a CLI typed in the shell can reach it (the Codex live bridge,
// and any registered launch route). The value is an address, never a secret.
const paneDaemonURLEnv = "SOLU_TEAM_BROKER_DAEMON_URL"

// codexShellRoutingMessage is what a terminal user sees when a Codex launch
// can't go through SoluTerminal. An optional feature may append a hint
// (codexShellRoutingHint, set from its init) that only applies to its users.
const codexShellRoutingMessage = "Codex couldn't start through SoluTerminal. Make sure SoluTerminal is running, then try again."

var codexShellRoutingHint string

type codexShellRoutingError struct{}

func (codexShellRoutingError) Error() string { return codexShellRoutingMessage + codexShellRoutingHint }

var errCodexShellRouting error = codexShellRoutingError{}

// Only an address is inherited by a generic Unix shell. Resolve selection and
// a fresh scoped capability when a model invocation actually starts.
func codexUnixShellNeedsDaemonAddress(cmd []string) bool {
	if runtime.GOOS == "windows" || len(cmd) == 0 {
		return false
	}
	switch filepath.Base(cmd[0]) {
	case "bash", "zsh", "sh", "fish", "ksh":
		return true
	}
	return false
}

func codexShellOptionTakesValue(arg string) bool {
	switch arg {
	case "-c", "--config", "--enable", "--disable", "--remote-auth-token-env", "-i", "--image", "-m", "--model", "--local-provider", "-p", "--profile", "-s", "--sandbox", "-C", "--cd", "--add-dir", "-a", "--ask-for-approval", "--output-last-message", "-o", "--output-schema":
		return true
	}
	return false
}

// Match the PowerShell hook: utilities and explicit remote transports remain
// untouched; exec/review/app-server need the same route as the interactive TUI.
func codexUnixModelInvocation(args []string) (model, headless bool) {
	model = true
	skip, positional := false, false
	for _, arg := range args {
		if skip {
			skip = false
			continue
		}
		if arg == "--" {
			break
		}
		if arg == "--remote" || strings.HasPrefix(arg, "--remote=") || arg == "--help" || arg == "-h" || arg == "--version" || arg == "-V" {
			return false, false
		}
		if codexShellOptionTakesValue(arg) {
			skip = true
			continue
		}
		if strings.HasPrefix(arg, "-") || positional {
			continue
		}
		positional = true
		switch arg {
		case "exec", "e", "review", "app-server":
			headless = true
		case "agents", "login", "logout", "mcp", "plugin", "remote-control", "completion", "update", "doctor", "sandbox", "debug", "apply", "a", "queue", "archive", "delete", "migrate-rollouts", "unarchive", "cloud", "exec-server", "features", "help":
			return false, false
		}
	}
	return model, headless
}

func codexShellProviderOverride(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--oss" || arg == "--local-provider" || strings.HasPrefix(arg, "--local-provider=") {
			return true
		}
		var config string
		switch {
		case arg == "-c" || arg == "--config":
			if i+1 < len(args) {
				i++
				config = args[i]
			}
		case strings.HasPrefix(arg, "--config="):
			config = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			config = strings.TrimPrefix(arg, "-c")
		case codexShellOptionTakesValue(arg):
			i++
			continue
		}
		key, _, _ := strings.Cut(strings.TrimPrefix(config, "="), "=")
		key = strings.NewReplacer("\"", "", "'", "", " ", "", "\t", "").Replace(key)
		// cli_auth_credentials_store: "file" or "keyring" would load the
		// default login in the user's CODEX_HOME instead of the configured provider.
		if key == "model_provider" || key == "model_providers" || strings.HasPrefix(key, "model_providers.") || strings.HasPrefix(key, "model_providers[") || key == "openai_base_url" || key == "chatgpt_base_url" || key == "cli_auth_credentials_store" {
			return true
		}
	}
	return false
}

func codexShellDaemonURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errCodexShellRouting
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return nil, errCodexShellRouting
	}
	return u, nil
}

// routeChildCommand inserts a launch plan's argv overrides after the CLI
// executable.
func routeChildCommand(cmd, overrides []string) []string {
	if len(overrides) == 0 || len(cmd) == 0 {
		return cmd
	}
	actual := make([]string, 0, len(cmd)+len(overrides))
	actual = append(actual, cmd[0])
	actual = append(actual, overrides...)
	return append(actual, cmd[1:]...)
}
