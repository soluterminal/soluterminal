//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// liveWindowsPath reads the merged Machine+User PATH from the registry — the same
// source a freshly-launched shell reads — with %VAR% references expanded. Machine
// PATH precedes User PATH, matching how the Windows shell composes them. Returns
// "" if neither value can be read (then callers leave the inherited PATH as-is).
func liveWindowsPath() string {
	var parts []string
	read := func(root registry.Key, path string) {
		k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
		if err != nil {
			return
		}
		defer k.Close()
		v, _, err := k.GetStringValue("Path")
		if err != nil || strings.TrimSpace(v) == "" {
			return
		}
		// The value is usually REG_EXPAND_SZ (holds %SystemRoot% etc.);
		// GetStringValue returns it unexpanded, so expand it to a literal path.
		if exp, eerr := registry.ExpandString(v); eerr == nil && exp != "" {
			v = exp
		}
		parts = append(parts, v)
	}
	read(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`)
	read(registry.CURRENT_USER, `Environment`)
	return strings.Join(parts, ";")
}

// mergeRegistryPath rebuilds env's PATH from the live registry PATH unioned with
// whatever PATH the daemon inherited, so a pane resolves commands exactly like a
// freshly-opened shell. This fixes tools (e.g. a Claude Code install) added to the
// User PATH AFTER the long-lived daemon started: the daemon's snapshot misses them
// but the registry — read here, per pane spawn — has them. No-op when the registry
// can't be read.
func mergeRegistryPath(env []string) []string {
	return unionWindowsPath(env, liveWindowsPath())
}

// findOnWindowsPath resolves a bare command name against the live registry PATH
// (unioned with the daemon's process PATH), honoring PATHEXT. The daemon's own
// PATH can be a stale snapshot, so exec.LookPath fails for a command a fresh shell
// would find (e.g. `claude` for the in-app sign-in pane). Returns the absolute
// path on success. Names that already contain a path separator are left to the
// caller's normal resolution.
func findOnWindowsPath(name string) (string, bool) {
	if name == "" || strings.ContainsAny(name, `\/`) {
		return "", false
	}
	merged := unionWindowsPath([]string{"PATH=" + os.Getenv("PATH")}, liveWindowsPath())
	pathVal := ""
	for _, kv := range merged {
		if eq := strings.IndexByte(kv, '='); eq >= 0 && strings.EqualFold(kv[:eq], "PATH") {
			pathVal = kv[eq+1:]
			break
		}
	}
	exts := windowsPathExts()
	if filepath.Ext(name) != "" {
		// An explicitly suffixed command names that exact file first. Bare
		// commands use PATHEXT only: npm also writes a POSIX shell shim with
		// no extension, which must never win over its Windows .cmd sibling.
		exts = append([]string{""}, exts...)
	}
	for _, dir := range strings.Split(pathVal, ";") {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			cand := filepath.Join(dir, name+ext)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, true
			}
		}
	}
	return "", false
}

// windowsPathExts returns the executable extensions to try (lowercased), from
// %PATHEXT% with a sensible default. Explicitly suffixed commands are handled
// by findOnWindowsPath; bare commands must not resolve npm's extensionless shims.
func windowsPathExts() []string {
	var exts []string
	pe := os.Getenv("PATHEXT")
	if pe == "" {
		pe = ".COM;.EXE;.BAT;.CMD"
	}
	for _, e := range strings.Split(pe, ";") {
		if e = strings.TrimSpace(e); e != "" {
			exts = append(exts, strings.ToLower(e))
		}
	}
	return exts
}
