package main

import (
	"fmt"
	"sort"
	"strings"
)

// The host never puts its own secrets in a helper spawn's env (ruling 8.4,
// helper-protocol-v1 Environment; @secure, 20 Nov: an allowlist of SOLU_*
// keys plus a denylist of host secret names under any prefix).

// helperSpawnSoluKeys are the only SOLU_* keys a Linux pane may carry. Each
// is named here deliberately; a new one fails checkHelperSpawnEnv (and its
// test) until it is added with a reason.
var helperSpawnSoluKeys = map[string]string{
	"SOLU_PANE_ID":                "pane attribution for hooks (pane_spawn.go)",
	"SOLU_CONTEXT_PACK":           "the pane's context-pack directory (pane_context_pack.go)",
	"SOLU_CODEX_PANE_POLICY":      "team install policy marker, not a credential (codex_pane_policy.go)",
	"SOLU_QUOTA_WORKER":           "quota worker marker (quota_workers_accounts.go)",
	"SOLU_HARNESS_RESUME_SESSION": "the session id a resume continues (harness_session.go)",
}

// helperSpawnSecretKeys are host secrets, refused whatever their prefix.
// The team broker is deferred for Linux panes (cut b), so its capability
// and addresses are refused too.
var helperSpawnSecretKeys = map[string]bool{
	"SOLU_TEAM_BROKER_TOKEN":       true,
	"SOLU_TEAM_BROKER_BASE_URL":    true,
	"SOLU_TEAM_BROKER_DAEMON_URL":  true,
	"SOLU_TEAM_BROKER_PROVIDER":    true,
	"ANTHROPIC_AUTH_TOKEN":         true, // the broker capability for claude
	"META_API_KEY":                 true, // the broker capability for muse
	"SOLU_TOKEN":                   true,
	"SOLU_OWNER_TOKEN":             true,
	"SOLU_AUTH_TOKEN":              true,
	"SOLU_CLAIM":                   true,
	"SOLU_RELAY_TOKEN":             true,
	"SOLU_UPDATE_SIGNING_KEY":      true,
	"SOLU_UPDATE_MANIFEST_PUBKEY":  true,
	"SOLU_MACHINE_TOKEN":           true,
	"SOLU_CLOUD_TOKEN":             true,
	"SOLU_DEVICE_KEY":              true,
	"SOLU_TEAM_CREDENTIAL":         true,
	"SOLU_TEAM_CREDENTIALS_BUNDLE": true,
}

// helperSpawnSecretMarkers catch secret-shaped names not listed above.
var helperSpawnSecretMarkers = []string{"TOKEN", "SECRET", "PASSWORD", "PRIVATE_KEY", "API_KEY", "CLAIM", "SIGNING"}

// isHelperSecretName matches a denylisted name exactly or as the end of a
// longer one (X_SOLU_DEVICE_KEY, FOO_SOLU_TEAM_BROKER_BASE_URL).
func isHelperSecretName(upper string) bool {
	if helperSpawnSecretKeys[upper] {
		return true
	}
	for name := range helperSpawnSecretKeys {
		if strings.HasSuffix(upper, "_"+name) {
			return true
		}
	}
	return false
}

// checkHelperSpawnEnv refuses an env with any SOLU_* key outside the
// allowlist, or any key that names a host secret.
func checkHelperSpawnEnv(env map[string]string) error {
	var bad []string
	for k := range env {
		upper := strings.ToUpper(k)
		switch {
		case isHelperSecretName(upper):
			bad = append(bad, k+" (a host secret)")
		case strings.HasPrefix(upper, "SOLU_"):
			if _, ok := helperSpawnSoluKeys[upper]; !ok || upper != k {
				bad = append(bad, k+" (not an allowed SOLU_ key)")
			}
		default:
			for _, m := range helperSpawnSecretMarkers {
				if strings.Contains(upper, m) {
					bad = append(bad, k+" (secret-shaped)")
					break
				}
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("a Linux pane's environment may not carry %s", strings.Join(bad, ", "))
}
