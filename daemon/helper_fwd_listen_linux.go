//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// The helper's in-distro sockets (helperFwdMCPSocket, helperFwdHooksSocket;
// helper-protocol-v1 "Proposed for M3 fwd", ruling 8.6a.7): in its runtime
// dir, 0600, created per helper start under the same lstat and owner checks
// as the dir, removed on exit.

type helperFwdListeners struct {
	dir   string
	mcp   net.Listener
	hooks net.Listener
	// published holds the inode of each socket this helper published, so
	// close removes only its own: during an update handoff a successor may
	// already have published its sockets under the same names.
	published map[string]os.FileInfo
}

// listenHelperFwd binds both sockets in dir. checkDir is the helper's runtime
// dir check (owner, 0700, no symlink), run before anything is created.
func listenHelperFwd(dir string, checkDir func(string) error) (*helperFwdListeners, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	if err := checkMCPSocketDir(dir); err != nil {
		return nil, err
	}
	unlock, err := lockHelperFwdDir(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	l := &helperFwdListeners{dir: dir, published: map[string]os.FileInfo{}}
	var fi os.FileInfo
	if l.mcp, fi, err = bindHelperFwdSocket(dir, helperFwdMCPSocket); err != nil {
		return nil, err
	}
	l.published[helperFwdMCPSocket] = fi
	if l.hooks, fi, err = bindHelperFwdSocket(dir, helperFwdHooksSocket); err != nil {
		l.closeLocked() // the dir lock is already held here
		return nil, err
	}
	l.published[helperFwdHooksSocket] = fi
	return l, nil
}

// bindHelperFwdSocket binds a private temporary name, checks it, then renames
// it over the published name, so a client never sees a half-made socket.
func bindHelperFwdSocket(dir, name string) (net.Listener, os.FileInfo, error) {
	path := filepath.Join(dir, name)
	tmp := filepath.Join(dir, "."+name+"-"+strconv.Itoa(os.Getpid()))
	if len(path) > mcpSocketPathMax || len(tmp) > mcpSocketPathMax {
		return nil, nil, fmt.Errorf("socket path %s is longer than %d bytes", tmp, mcpSocketPathMax)
	}
	if err := removeOwnStaleSocket(tmp); err != nil {
		return nil, nil, err
	}
	ln, err := listenUnixWithUmask(tmp)
	if err != nil {
		return nil, nil, fmt.Errorf("bind %s: %w", tmp, err)
	}
	// The listener must not unlink the name it was bound to: after the
	// rename that name belongs to nothing, and the published one is removed
	// by close.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := checkOwnSocket(tmp, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(tmp)
		return nil, nil, err
	}
	if err := removeOwnStaleSocket(path); err != nil {
		_ = ln.Close()
		_ = os.Remove(tmp)
		return nil, nil, err
	}
	fi, err := os.Lstat(tmp)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(tmp)
		return nil, nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = ln.Close()
		_ = os.Remove(tmp)
		return nil, nil, fmt.Errorf("publish %s: %w", path, err)
	}
	return ln, fi, nil
}

func (l *helperFwdListeners) close() {
	// Under the same lock as publishing, so a successor can't publish
	// between our inode check and the unlink. Without the lock (the dir is
	// gone or unusable), nothing is removed.
	unlock, err := lockHelperFwdDir(l.dir)
	if err != nil {
		for _, ln := range []net.Listener{l.mcp, l.hooks} {
			if ln != nil {
				_ = ln.Close()
			}
		}
		return
	}
	defer unlock()
	l.closeLocked()
}

// closeLocked is close with the dir lock already held.
func (l *helperFwdListeners) closeLocked() {
	for name, ln := range map[string]net.Listener{helperFwdMCPSocket: l.mcp, helperFwdHooksSocket: l.hooks} {
		if ln == nil {
			continue
		}
		_ = ln.Close()
		removeHelperFwdSocketIfOurs(filepath.Join(l.dir, name), l.published[name])
	}
}

// removeHelperFwdSocketIfOurs unlinks path only while it is still the socket
// this listener published (same device and inode); a successor's socket
// under the same name is left alone.
func removeHelperFwdSocketIfOurs(path string, ours os.FileInfo) {
	if ours == nil {
		return
	}
	now, err := os.Lstat(path)
	if err != nil || !os.SameFile(now, ours) {
		return
	}
	_ = os.Remove(path)
}

// serve runs both sockets until ctx ends, then closes and removes them.
func (l *helperFwdListeners) serve(ctx context.Context, s *helperFwdServer) {
	defer l.close()
	hooks := s.hooksServer()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := hooks.Serve(newHelperFwdLimitListener(l.hooks, helperFwdMaxHookConns, s.logf)); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			s.logf("fwd: hooks.sock: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		s.acceptMCP(ctx, l.mcp)
	}()
	<-ctx.Done()
	_ = l.mcp.Close()
	_ = hooks.Close()
	wg.Wait()
}

// lockHelperFwdDir takes an exclusive flock on the runtime dir itself (no new
// file), held while one helper publishes or removes its sockets. Two helpers
// for one distro overlap only during an update handoff; this keeps the
// predecessor's cleanup from unlinking what the successor just published.
func lockHelperFwdDir(dir string) (func(), error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}, nil
}
