package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"soluterminal-shared/providers"
)

// Test seams for the Windows half of resolveProviderBin, so the registry-PATH
// and %LOCALAPPDATA% logic is exercised on any host.
var (
	providerBinGOOS              = runtime.GOOS
	providerBinFindOnWindowsPath = findOnWindowsPath
	providerBinGetenv            = os.Getenv
)

// providerBinNames are the file names a bare command can have on disk. On
// Windows a home-relative candidate needs its extension: Stat, unlike
// LookPath, does not apply PATHEXT.
func providerBinNames(name string) []string {
	if providerBinGOOS == "windows" {
		return []string{name + ".exe", name + ".cmd", name}
	}
	return []string{name}
}

// resolveProviderBin resolves one manifest-declared runtime binary.
//
// On Windows the daemon's own PATH is a launch-time snapshot, so a provider
// installed after the daemon started (Codex's install.ps1 updates only the
// registry) would stay "not found" until a restart. The live registry PATH is
// read first, and the vendor's fixed install folders are checked last.
func resolveProviderBin(runtimeID string) (string, error) {
	_, _, facts, ok := providers.ByRuntimeID(runtimeID)
	if !ok || facts.Binary == nil {
		return "", fmt.Errorf("runtime %q has no resolvable binary", runtimeID)
	}
	spec := facts.Binary
	if providerBinGOOS == "windows" {
		if p, ok := providerBinFindOnWindowsPath(spec.Name); ok {
			return p, nil
		}
	}
	if p, err := exec.LookPath(spec.Name); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if spec.NoHomeMsg != "" {
			return "", errors.New(spec.NoHomeMsg)
		}
	} else {
		for _, sub := range spec.HomeSubdirs {
			for _, name := range providerBinNames(spec.Name) {
				candidate := filepath.Join(home, filepath.FromSlash(sub), name)
				if isExistingFile(candidate) {
					return candidate, nil
				}
			}
		}
		for _, glob := range spec.HomeGlobs {
			matches, _ := filepath.Glob(filepath.Join(home, filepath.FromSlash(glob), spec.Name))
			for _, match := range matches {
				if isExistingFile(match) {
					return match, nil
				}
			}
		}
	}
	if providerBinGOOS == "windows" {
		if p, ok := providerBinUnderLocalAppData(spec, home); ok {
			return p, nil
		}
	}
	if spec.LoginShellFallback {
		if p, err := loginShellCommandPath(spec.Name); err == nil {
			return p, nil
		}
	}
	return "", &providerBinNotFoundError{msg: spec.NotFoundMsg}
}

// errProviderBinNotFound matches (errors.Is) a resolver miss: the CLI is not
// installed anywhere the resolver looks. Callers that poll a runtime use it to
// tell "not installed here" from a broken install.
var errProviderBinNotFound = errors.New("provider binary not found")

// providerBinNotFoundError keeps the manifest's exact wording (the failure
// classifier matches it) while still answering errors.Is for the sentinel.
type providerBinNotFoundError struct{ msg string }

func (e *providerBinNotFoundError) Error() string        { return e.msg }
func (e *providerBinNotFoundError) Is(target error) bool { return target == errProviderBinNotFound }

// providerBinUnderLocalAppData checks the vendor's fixed Windows install
// folders. %LOCALAPPDATA% falls back to %USERPROFILE%\AppData\Local (or the
// home dir) for launchers that strip it from the daemon's environment.
func providerBinUnderLocalAppData(spec *providers.BinarySpec, home string) (string, bool) {
	if len(spec.LocalAppDataSubdirs) == 0 {
		return "", false
	}
	base := providerBinGetenv("LOCALAPPDATA")
	if base == "" {
		if profile := providerBinGetenv("USERPROFILE"); profile != "" {
			base = filepath.Join(profile, "AppData", "Local")
		} else if home != "" {
			base = filepath.Join(home, "AppData", "Local")
		}
	}
	if base == "" {
		return "", false
	}
	for _, sub := range spec.LocalAppDataSubdirs {
		for _, name := range providerBinNames(spec.Name) {
			candidate := filepath.Join(base, filepath.FromSlash(sub), name)
			if isExistingFile(candidate) {
				return candidate, true
			}
		}
	}
	return "", false
}

func resolveClaudeBin() (string, error) { return resolveProviderBin("claude-headless") }
func resolveCodexBin() (string, error)  { return resolveProviderBin("codex-headless") }
func resolveGeminiBin() (string, error) { return resolveProviderBin("gemini-headless") }
func resolveMuseBin() (string, error)   { return resolveProviderBin("muse-headless") }

// providerRuntimeIDForBinaryName maps a direct provider command name (a pane
// child or driver argv[0]) to its workflow runtime id so callers can apply the
// node-aware child PATH. Only the four managed native CLIs are recognised.
func providerRuntimeIDForBinaryName(name string) (string, bool) {
	base := strings.TrimSuffix(filepath.Base(name), ".exe")
	switch base {
	case "claude":
		return "claude-headless", true
	case "codex":
		return "codex-headless", true
	case "gemini":
		return "gemini-headless", true
	case "muse":
		return "muse-headless", true
	}
	return "", false
}

// providerChildEnvForCommand prefers the caller's own argv[0] when it names a
// path, so a pane bound to a specific install keeps that install's sibling
// node. A bare command is resolved with exec.LookPath only — the exact lookup
// exec.Command performs for the spawn that follows — so this pane-start hot
// path never pays for the home-glob scan or the login-shell fallback, and a
// command the spawn cannot find is left for the spawn to report.
func providerChildEnvForCommand(cmd string, base []string) []string {
	if _, ok := providerRuntimeIDForBinaryName(cmd); !ok {
		return base
	}
	if strings.ContainsAny(cmd, `/\`) {
		return providerChildEnvForBin(cmd, base)
	}
	bin, err := exec.LookPath(cmd)
	if err != nil || bin == "" {
		return base
	}
	return providerChildEnvForBin(bin, base)
}

// providerScriptHeaderBytes bounds how much of a resolved provider binary we
// ever read while deciding whether it is a node script.
const providerScriptHeaderBytes = 4096

// providerChildEnv returns base with the resolved provider's node bin
// directory prepended when the provider needs one. A failed resolution leaves
// base untouched: the runtime's own RunHeadless reports the missing binary.
func providerChildEnv(runtimeID string, base []string) []string {
	bin, err := resolveProviderBin(runtimeID)
	if err != nil || bin == "" {
		return base
	}
	return providerChildEnvForBin(bin, base)
}

// providerChildEnvForBin is providerChildEnv for a caller that already
// resolved the binary, so a spawn path never resolves twice.
func providerChildEnvForBin(bin string, base []string) []string {
	if dir := providerNodeBinDir(bin, base); dir != "" {
		return prependPathDir(base, dir)
	}
	return base
}

// providerNodeBinDir reports the directory to prepend to a provider child's
// PATH, or "" when the binary runs without an explicit node directory. The
// daemon's own PATH is deliberately never touched: only the returned child
// environment changes.
//
// A version manager's bin directory is preferred over the login-shell node
// because the provider was resolved there: its sibling node is the version the
// install expects. A node-shebang script outside such a layout falls back to
// the shared resolveNodeBin, matching what workerEnv already does for every
// worker, and a shell script with no node dependency is left alone. That
// fallback applies only when the child's own PATH (base) has no node: a PATH
// that already runs the script keeps its directory order, so e.g. /usr/bin is
// never hoisted above ~/.local/bin for a pane child.
func providerNodeBinDir(bin string, base []string) string {
	if bin == "" {
		return ""
	}
	target := bin
	// A wrapper in ~/.local/bin is often a symlink into the real install; the
	// sibling node lives beside the TARGET, not beside the link.
	if resolved, err := filepath.EvalSymlinks(bin); err == nil && resolved != "" {
		target = resolved
	}
	dir := filepath.Dir(target)
	hasSiblingNode := isExistingFile(filepath.Join(dir, "node"))
	if hasSiblingNode && isNodeVersionBinDir(dir) {
		return dir
	}
	if providerHasNodeShebang(target) {
		if hasSiblingNode {
			return dir
		}
		if envPathHasNode(base) {
			return ""
		}
		if nodeBin, err := resolveNodeBin(); err == nil && nodeBin != "" {
			return filepath.Dir(nodeBin)
		}
	}
	return ""
}

// envPathHasNode reports whether the PATH carried by env already contains an
// executable node, i.e. whether a `#!/usr/bin/env node` child runs as is. Like
// getenv, the first PATH entry in env wins.
func envPathHasNode(env []string) bool {
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			continue
		}
		for _, dir := range filepath.SplitList(kv[len("PATH="):]) {
			if dir == "" {
				continue
			}
			info, err := os.Stat(filepath.Join(dir, "node"))
			if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				return true
			}
		}
		return false
	}
	return false
}

// isNodeVersionBinDir recognises the bin directories owned by the node version
// managers the resolvers already search: nvm, asdf, and Volta. The check keys
// on the marker segment so an unrelated directory named "bin" is not treated as
// a node install.
func isNodeVersionBinDir(dir string) bool {
	slash := filepath.ToSlash(filepath.Clean(dir))
	if strings.HasSuffix(slash, "/.asdf/shims") || strings.HasSuffix(slash, "/.volta/bin") {
		return true
	}
	if !strings.HasSuffix(slash, "/bin") {
		return false
	}
	for _, marker := range []string{"/.nvm/versions/node/", "/.asdf/installs/nodejs/", "/.volta/tools/image/node/"} {
		if strings.Contains(slash, marker) {
			return true
		}
	}
	return false
}

// providerHasNodeShebang reports whether path begins with a bounded shebang
// that ultimately invokes node, including the `#!/usr/bin/env node` form codex
// ships. Only a bounded header is read; an ELF or a shell wrapper returns false.
func providerHasNodeShebang(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	header := make([]byte, providerScriptHeaderBytes)
	n, _ := f.Read(header)
	if n < 2 || header[0] != '#' || header[1] != '!' {
		return false
	}
	firstLine := header[:n]
	if i := bytes.IndexByte(firstLine, '\n'); i >= 0 {
		firstLine = firstLine[:i]
	}
	firstLine = bytes.TrimRight(firstLine, "\r")
	fields := strings.Fields(string(firstLine[2:]))
	if len(fields) == 0 {
		return false
	}
	interp := fields[0]
	if filepath.Base(interp) == "env" {
		if len(fields) < 2 {
			return false
		}
		// `env -S node ...` is a common shebang when the interpreter needs
		// arguments; the node executable follows the split-string switch.
		index := 1
		if fields[index] == "-S" {
			index++
		}
		if index >= len(fields) {
			return false
		}
		interp = fields[index]
	}
	return filepath.Base(interp) == "node"
}
