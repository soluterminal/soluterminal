//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"soluterminal/helperproto"
)

// helperRuntimeDir is where the helper keeps its agents' sockets and records:
// $XDG_RUNTIME_DIR/soluterminal-helper when set, else
// ~/.local/state/soluterminal/helper (helper-protocol-v1, Files).
func helperRuntimeDir(getenv func(string) string, home string) (string, error) {
	if x := getenv("XDG_RUNTIME_DIR"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "soluterminal-helper"), nil
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("no home directory")
	}
	return filepath.Join(home, ".local", "state", "soluterminal", "helper"), nil
}

// prepareHelperRuntimeDir creates the runtime dir and agents/ as 0700 when
// missing, then checks both (ruling 8.2).
func prepareHelperRuntimeDir(dir string, uid int) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "create %s: %v", filepath.Dir(dir), err)
	}
	for _, d := range []string{dir, filepath.Join(dir, "agents")} {
		if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "create %s: %v", d, err)
		}
	}
	return checkHelperRuntimeDir(dir, uid)
}

// checkHelperRuntimeDir refuses the runtime dir, so no agent starts, when it
// or agents/ is a symlink or not a directory, isn't owned by uid, is open to
// group or others, or sits on a Windows drive (DrvFs), where Unix modes and
// sockets aren't the Linux kernel's to enforce. It uses lstat, never stat,
// and runs at hello and before each spawn.
func checkHelperRuntimeDir(dir string, uid int) error {
	for _, d := range []string{dir, filepath.Join(dir, "agents")} {
		fi, err := os.Lstat(d)
		if err != nil {
			return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "%s: %v", d, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "%s is not a plain directory", d)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != uid {
			return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "%s is not owned by uid %d", d, uid)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "%s is mode %04o, want 0700", d, fi.Mode().Perm())
		}
	}
	// sun_path holds 108 bytes, NUL included: refuse a dir no socket fits in.
	if probe, _, _, _ := helperAgentPaths(dir, strings.Repeat("0", 32)); len(probe) >= 108 {
		return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "%s is too long for agent sockets", dir)
	}
	if err := mcpSocketFilesystemOKPlatform(dir); err != nil {
		return helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "%s: %v", dir, err)
	}
	return nil
}

func helperAgentPaths(dir, agentID string) (sock, record, logFile string, err error) {
	if !helperproto.ValidAgentID(agentID) {
		return "", "", "", fmt.Errorf("bad agent id")
	}
	base := filepath.Join(dir, "agents", agentID)
	return base + ".sock", base + ".json", base + ".log", nil
}
