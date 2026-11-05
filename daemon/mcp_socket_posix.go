//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// mcpSocketPathMax keeps a path inside sockaddr_un.sun_path on every
// supported host (104 bytes on macOS, including the terminating NUL).
const mcpSocketPathMax = 103

// prepareMCPSocket binds the MCP socket under a per-process name inside
// <dataDir>/run. It runs early in main, before other goroutines start, because
// the socket's 0600 mode comes from the umask in force at bind time. Nothing
// is published until activate. Any problem disables the endpoint (nil) and
// says why; the daemon carries on without it.
func prepareMCPSocket(dataDir string) *mcpSocketEndpoint {
	if os.Getenv("SOLU_MCP_SOCKET") == "0" {
		log.Printf("mcp socket: disabled by SOLU_MCP_SOCKET=0")
		return nil
	}
	e, err := bindMCPSocket(dataDir)
	if err != nil {
		log.Printf("mcp socket: endpoint disabled: %v", err)
		return nil
	}
	return e
}

func bindMCPSocket(dataDir string) (*mcpSocketEndpoint, error) {
	dir := filepath.Join(dataDir, mcpSocketDirName)
	if err := ensureMCPSocketDir(dir); err != nil {
		return nil, err
	}
	e := &mcpSocketEndpoint{
		dir:  dir,
		path: filepath.Join(dir, mcpSocketName),
		tmp:  filepath.Join(dir, ".mcp-"+strconv.Itoa(os.Getpid())+".sock"),
		sem:  make(chan struct{}, mcpSocketMaxConns),
	}
	if len(e.path) > mcpSocketPathMax || len(e.tmp) > mcpSocketPathMax {
		return nil, fmt.Errorf("socket path %s is longer than %d bytes", e.tmp, mcpSocketPathMax)
	}
	// A leftover of an earlier process with this pid.
	if err := removeOwnStaleSocket(e.tmp); err != nil {
		return nil, err
	}
	ln, err := listenUnixWithUmask(e.tmp)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", e.tmp, err)
	}
	if err := checkOwnSocket(e.tmp, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(e.tmp)
		return nil, err
	}
	e.ln = ln
	return e, nil
}

// ensureMCPSocketDir creates dir 0700 and accepts it only as checkMCPSocketDir
// allows. It never changes the mode or owner of a directory that fails.
func ensureMCPSocketDir(dir string) error {
	if err := checkMCPDataDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return checkMCPSocketDir(dir)
}

// checkMCPDataDir refuses a data directory other users can write to: they
// could swap the run directory out from under the socket.
func checkMCPDataDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %v)", dir, fi.Mode().Perm())
	}
	return nil
}

// checkMCPSocketDir accepts dir only as a real directory owned by this user
// with mode exactly 0700, inside a data directory only this user can write,
// on a local filesystem. It changes nothing.
func checkMCPSocketDir(dir string) error {
	if err := checkMCPDataDir(filepath.Dir(dir)); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != mcpSocketEUID() {
		return fmt.Errorf("%s is not owned by uid %d", dir, mcpSocketEUID())
	}
	if mode := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); mode != 0o700 {
		return fmt.Errorf("%s has mode %v, want 0700", dir, mode)
	}
	return mcpSocketFilesystemOK(dir)
}

// mcpSocketUmaskMu serializes the umask change around bind within this
// package. The umask is process-wide; prepareMCPSocket runs before other
// goroutines exist, and a narrower umask can only make a concurrent file
// stricter, never more open.
var mcpSocketUmaskMu sync.Mutex

func listenUnixWithUmask(path string) (net.Listener, error) {
	mcpSocketUmaskMu.Lock()
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	mcpSocketUmaskMu.Unlock()
	if err != nil {
		return nil, err
	}
	// The daemon decides when a socket name goes away (activate renames
	// it); a close must not unlink a name another process now owns.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	return ln, nil
}

// isOwnSocket reports whether fi is a socket owned by this user.
func isOwnSocket(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode()&fs.ModeSocket != 0 && int(st.Uid) == mcpSocketEUID()
}

func checkOwnSocket(path string, perm fs.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !isOwnSocket(fi) {
		return fmt.Errorf("%s is not a socket owned by uid %d", path, mcpSocketEUID())
	}
	if got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != perm {
		return fmt.Errorf("%s has mode %v, want %v", path, got, perm)
	}
	return nil
}

// removeOwnStaleSocket unlinks path only when it is a socket owned by this
// user; anything else at that name is left alone and refuses the endpoint.
func removeOwnStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isOwnSocket(fi) {
		return fmt.Errorf("%s exists and is not a socket owned by uid %d; not removing it", path, mcpSocketEUID())
	}
	return os.Remove(path)
}

// activate publishes the socket as <dir>/mcp.sock once this process owns the
// data directory (after an update handoff, the predecessor has let go). An
// existing name is replaced only when it is a socket owned by this user.
func (e *mcpSocketEndpoint) activate() error {
	fi, err := os.Lstat(e.path)
	switch {
	case err == nil:
		if !isOwnSocket(fi) {
			return fmt.Errorf("%s exists and is not a socket owned by uid %d; not replacing it", e.path, mcpSocketEUID())
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.Rename(e.tmp, e.path); err != nil {
		return err
	}
	if err := checkOwnSocket(e.path, 0o600); err != nil {
		return err
	}
	e.sweepStaleTemps()
	return nil
}

// close stops the listener and drops the unpublished name, if any.
func (e *mcpSocketEndpoint) close() {
	_ = e.ln.Close()
	if fi, err := os.Lstat(e.tmp); err == nil && isOwnSocket(fi) {
		_ = os.Remove(e.tmp)
	}
}

// sweepStaleTemps removes per-process names left by daemons that exited
// before publishing theirs. A live process's name is kept.
func (e *mcpSocketEndpoint) sweepStaleTemps() {
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		pidText, ok := strings.CutPrefix(name, ".mcp-")
		if !ok {
			continue
		}
		pidText, ok = strings.CutSuffix(pidText, ".sock")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid == os.Getpid() {
			continue
		}
		if _, alive := mcpProcessStartTime(pid); alive {
			continue
		}
		path := filepath.Join(e.dir, name)
		if fi, err := os.Lstat(path); err == nil && isOwnSocket(fi) {
			_ = os.Remove(path)
		}
	}
}

// mcpBridgeCheckSocket runs before every dial the bridge makes: the socket's
// directory must pass checkMCPSocketDir, and the socket, if present, must be
// a socket owned by this user. A missing socket (the daemon restarting) is
// errMCPSocketMissing, worth waiting for; anything else refuses.
func mcpBridgeCheckSocket(path string) error {
	if err := checkMCPSocketDir(filepath.Dir(path)); err != nil {
		return err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return errMCPSocketMissing
	}
	if err != nil {
		return err
	}
	if !isOwnSocket(fi) {
		return fmt.Errorf("%s is not a socket owned by uid %d", path, mcpSocketEUID())
	}
	return nil
}
