//go:build darwin

package main

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// workerGitBirthTime is when path itself (not a symlink target) was created.
func workerGitBirthTime(path string) (time.Time, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Birthtimespec.Unix()), true
}

// workerGitLocalClock reports whether path is on a local APFS or HFS+ volume,
// whose birth times come from this machine's clock (not a network share).
func workerGitLocalClock(path string) bool {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return false
	}
	switch unix.ByteSliceToString(st.Fstypename[:]) {
	case "apfs", "hfs":
		return true
	}
	return false
}
