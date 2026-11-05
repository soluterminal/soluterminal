package main

import (
	"os"
	"testing"
)

// The package name and version must match exactly; the version is the x.y.z
// part (npm's platform packages carry a -<os>-<arch> suffix).
func TestAgentVersionFromExeCodexPackageRules(t *testing.T) {
	const bin = "/n/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex"
	const pkgPath = "/n/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/package.json"
	for _, tc := range []struct{ name, version, want string }{
		{"@openai/codex-linux-x64", "0.166.0-linux-x64", "0.166.0"},
		{"@openai/codex-linux-x64", "0.166.0", "0.166.0"},
		{"@openai/codex-darwin-arm64", "0.167.2-darwin-arm64", "0.167.2"},
		{"@openai/codex-win32-x64", "1.0.0-win32-x64", "1.0.0"},
		{"@openai/codex", "0.166.0", "0.166.0"},
		// A lookalike name, or a version with any other suffix, verifies
		// nothing.
		{"@openai/codex-anything", "0.166.0", ""},
		{"@openai/codex-linux-x64-evil", "0.166.0", ""},
		{"@openai/codexx", "0.166.0", ""},
		{"@openai/codex-linux-x64", "0.166.0-beta", ""},
		{"@openai/codex-linux-x64", "0.166.0-linux-x64-1", ""},
		{"@openai/codex-linux-x64", "v0.166.0", ""},
	} {
		read := func(path string) ([]byte, error) {
			if path == pkgPath {
				return []byte(`{"name":"` + tc.name + `","version":"` + tc.version + `"}`), nil
			}
			return nil, os.ErrNotExist
		}
		if got := agentVersionFromExe(agentCodex, bin, read); got != tc.want {
			t.Errorf("%s@%s: %q, want %q", tc.name, tc.version, got, tc.want)
		}
	}
}
