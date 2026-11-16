package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// One serving daemon per data directory.
//
// The listen port was the only thing keeping two daemons apart, and it only
// works when both use the same port. A second daemon on another port (a
// hand-made recovery service on 48887 next to the launcher's 8787, or a
// systemd unit next to the desktop launcher) shares ~/.solu with the first:
// both reattach, kill or respawn the same pane agents and both rewrite the
// same panes.json. Even on the same port the loser only finds out at bind
// time, after pane restoration has already run. The lock is taken before any
// state is loaded, so a second daemon leaves before it touches anything.
//
// Update successors are the one legitimate overlap: the predecessor keeps the
// lock (and the port) while the successor loads state. A successor therefore
// takes the lock after it owns the listener, by which time the predecessor
// has exited and the kernel has released its lock.

// exitDataDirOwned is the daemon's exit status when another live daemon owns
// the data directory (EX_CANTCREAT). The WSL launcher treats it as final: a
// relaunch cannot succeed until the owner stops.
const exitDataDirOwned = 73

const dataDirLockName = ".daemon.lock"

// dataDirLockHolder is diagnostic only. The kernel lock is the authority; the
// file content just tells the refused daemon (and a human) who holds it.
type dataDirLockHolder struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr,omitempty"`
	Backend   string `json:"backendId,omitempty"`
	Version   string `json:"version,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

// errDataDirOwned reports that a live process holds the data-directory lock.
type errDataDirOwned struct {
	Dir    string
	Holder *dataDirLockHolder
}

func (e *errDataDirOwned) Error() string {
	who := "another soluterminal daemon"
	if h := e.Holder; h != nil && h.PID > 0 {
		who = fmt.Sprintf("soluterminal daemon pid=%d", h.PID)
		if h.Addr != "" {
			who += " on " + h.Addr
		}
		if h.Version != "" {
			who += " (version " + h.Version + ")"
		}
	}
	return fmt.Sprintf("data directory %s is already served by %s", e.Dir, who)
}

// dataDirLock is a held lock. Release is only for tests and orderly
// shutdown paths; process exit releases it too.
type dataDirLock struct {
	release func()
}

func (l *dataDirLock) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

// dataDirLockDisabled is the escape hatch for unusual setups that knowingly
// run two daemons on one data directory (none ship). Logged when used.
func dataDirLockDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SOLU_DATA_DIR_LOCK")))
	return v == "off" || v == "0" || v == "false"
}

// acquireDataDirLock takes the lifetime lock without blocking. It returns
// *errDataDirOwned when a live process holds it, or another error when the
// lock file cannot be used at all (the caller logs that and continues: a
// filesystem without locking must not stop the terminal from working).
func acquireDataDirLock(dir string, holder dataDirLockHolder) (*dataDirLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, dataDirLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	held, err := tryExclusiveFileLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !held {
		_ = f.Close()
		return nil, &errDataDirOwned{Dir: dir, Holder: readDataDirLockHolder(path)}
	}
	if holder.PID == 0 {
		holder.PID = os.Getpid()
	}
	if holder.StartedAt == "" {
		holder.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if body, mErr := json.Marshal(holder); mErr == nil {
		// Best effort: a failed write only loses the diagnostic text.
		if tErr := f.Truncate(0); tErr == nil {
			_, _ = f.WriteAt(append(body, '\n'), 0)
		}
	}
	return &dataDirLock{release: func() {
		unlockFile(f)
		_ = f.Close()
	}}, nil
}

func readDataDirLockHolder(path string) *dataDirLockHolder {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	var h dataDirLockHolder
	if json.Unmarshal(raw, &h) != nil {
		return nil
	}
	return &h
}

// acquireDataDirLockPatiently is the successor path: the predecessor holds
// the lock until it exits, which it does once the successor owns the port.
// Wait for that, then keep trying in the background rather than abandoning a
// successful update; the port is already exclusive at this point.
func acquireDataDirLockPatiently(dir string, holder dataDirLockHolder, wait time.Duration, retry time.Duration) (*dataDirLock, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := acquireDataDirLock(dir, holder)
		if err == nil {
			return lock, nil
		}
		var owned *errDataDirOwned
		if !errors.As(err, &owned) || !time.Now().Before(deadline) {
			return nil, err
		}
		time.Sleep(retry)
	}
}

// claimDataDirForBoot is called from main before any state is loaded. It
// exits the process when another daemon serves this data directory; every
// other outcome continues the boot.
//
// An update successor skips the lifetime lock until it owns the listener
// (claimDataDirAfterHandoff), but only a real one: its parent must be the
// live lock holder. It holds the handoff lock from here until it has the
// lifetime lock, so an ordinary daemon started in the moment between the
// predecessor's exit and the successor's lock (sign-in, an app, a repair
// button) sees the handoff and leaves before it loads anything (ST-253).
func claimDataDirForBoot(dir string, holder dataDirLockHolder) *dataDirLock {
	if dataDirLockDisabled() {
		log.Printf("data directory lock disabled by SOLU_DATA_DIR_LOCK; two daemons on %s would share state", dir)
		return nil
	}
	if os.Getenv("SOLU_SUCCESSOR_OF_UPDATE") == "1" {
		return claimDataDirForSuccessor(dir, holder)
	}
	lock, err := acquireDataDirLock(dir, holder)
	if err == nil {
		if handoffInProgress(dir) {
			lock.Release()
			log.Printf("refusing to start: an update successor is taking over %s. It will own it within seconds; starting a second daemon would reattach and respawn the same panes.", dir)
			os.Exit(exitDataDirOwned)
		}
		return lock
	}
	var owned *errDataDirOwned
	if errors.As(err, &owned) {
		log.Printf("refusing to start: %v. Stop that daemon first; starting a second one would reattach and respawn the same panes.", err)
		os.Exit(exitDataDirOwned)
	}
	log.Printf("data directory lock unavailable (%v); continuing without it", err)
	return nil
}

// The handoff lock: held by an update successor from its first instruction
// until it holds the lifetime lock. A separate file, so neither lock ever
// waits on the other.
const dataDirHandoffLockName = ".daemon.handoff.lock"

// dataDirHandoffHeld is the successor's handoff lock, released once the
// lifetime lock is ours.
var dataDirHandoffHeld *dataDirLock

// tryDataDirHandoffLock takes the handoff lock without blocking. Only a
// successor creates the file (create); a check opens it only if it exists, so
// an ordinary boot writes nothing new to the data directory.
func tryDataDirHandoffLock(dir string, create bool) (*dataDirLock, bool, error) {
	flags := os.O_RDWR
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, false, err
		}
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(filepath.Join(dir, dataDirHandoffLockName), flags, 0o600)
	if err != nil {
		return nil, false, err
	}
	held, err := tryExclusiveFileLock(f)
	if err != nil || !held {
		_ = f.Close()
		return nil, false, err
	}
	return &dataDirLock{release: func() {
		unlockFile(f)
		_ = f.Close()
	}}, true, nil
}

// handoffInProgress reports whether an update successor holds the handoff
// lock. An unusable handoff file is not a handoff (the lifetime lock already
// decided).
func handoffInProgress(dir string) bool {
	lock, held, err := tryDataDirHandoffLock(dir, false)
	if errors.Is(err, os.ErrNotExist) {
		// No successor has ever handed off here.
		return false
	}
	if err != nil {
		log.Printf("data directory handoff lock unavailable (%v); ignoring it", err)
		return false
	}
	if held {
		lock.Release()
		return false
	}
	return true
}

func claimHandoffForSuccessor(dir string) {
	lock, held, err := tryDataDirHandoffLock(dir, true)
	switch {
	case err != nil:
		log.Printf("update successor: handoff lock unavailable (%v); continuing without it", err)
	case !held:
		log.Printf("refusing to start: another update successor is already taking over %s", dir)
		os.Exit(exitDataDirOwned)
	default:
		dataDirHandoffHeld = lock
	}
}

// claimDataDirForSuccessor is the boot-time claim of a process started as an
// update successor. Only spawnSuccessor sets SOLU_SUCCESSOR_OF_UPDATE, always
// from the live lock holder, so:
//   - the lock is free: the predecessor never held it (lock unusable or
//     disabled there) or is gone. Take it now, unless another successor
//     holds the handoff lock (then this one is not the real successor).
//   - the holder is this process's parent: the real handoff. Hold the handoff
//     lock and take the lifetime lock after the listener
//     (claimDataDirAfterHandoff).
//   - anyone else holds it: the flag didn't come from a predecessor, and a
//     daemon already serves this directory. Leave, as an ordinary daemon would.
func claimDataDirForSuccessor(dir string, holder dataDirLockHolder) *dataDirLock {
	lock, err := acquireDataDirLock(dir, holder)
	if err == nil {
		// Free, but a real successor may be between its predecessor's exit
		// and its own lock: it holds the handoff lock, and this process
		// (which then can't be that successor) must leave.
		if handoffInProgress(dir) {
			lock.Release()
			log.Printf("refusing to start: an update successor is taking over %s, and this process is not it", dir)
			os.Exit(exitDataDirOwned)
		}
		return lock
	}
	var owned *errDataDirOwned
	if !errors.As(err, &owned) {
		log.Printf("update successor: data directory lock unavailable (%v); continuing without it", err)
		return nil
	}
	if owned.Holder != nil && owned.Holder.PID > 0 && owned.Holder.PID == os.Getppid() {
		claimHandoffForSuccessor(dir)
		return nil
	}
	log.Printf("refusing to start: SOLU_SUCCESSOR_OF_UPDATE is set but the parent process does not hold the data directory, and %v", err)
	os.Exit(exitDataDirOwned)
	return nil
}

// claimDataDirAfterHandoff completes the successor's ownership once it holds
// the listener. Its predecessor exits right after the handoff marker, so the
// lock normally frees within a second.
func claimDataDirAfterHandoff(dir string, holder dataDirLockHolder) {
	if dataDirLockDisabled() || os.Getenv("SOLU_SUCCESSOR_OF_UPDATE") != "1" {
		return
	}
	if dataDirLockHeld != nil {
		// Taken at boot: the predecessor never held it (claimDataDirForSuccessor).
		releaseHandoff()
		return
	}
	lock, err := acquireDataDirLockPatiently(dir, holder, 30*time.Second, 200*time.Millisecond)
	if err == nil {
		dataDirLockHeld = lock
		releaseHandoff()
		return
	}
	log.Printf("update successor: data directory lock not yet free (%v); retrying in the background", err)
	go func() {
		for {
			time.Sleep(5 * time.Second)
			lock, err := acquireDataDirLock(dir, holder)
			if err == nil {
				dataDirLockHeld = lock
				releaseHandoff()
				log.Printf("update successor: data directory lock acquired")
				return
			}
			var owned *errDataDirOwned
			if !errors.As(err, &owned) {
				log.Printf("update successor: data directory lock unavailable (%v); continuing without it", err)
				releaseHandoff()
				return
			}
		}
	}()
}

func releaseHandoff() {
	if dataDirHandoffHeld != nil {
		dataDirHandoffHeld.Release()
		dataDirHandoffHeld = nil
	}
}

// dataDirLockHeld keeps the lock's file open for the life of the process.
var dataDirLockHeld *dataDirLock
