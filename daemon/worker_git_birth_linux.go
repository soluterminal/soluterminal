//go:build linux

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// workerGitBirthTime is when path itself (not a symlink target) was created,
// from statx's birth time. ok is false when the filesystem does not record it.
func workerGitBirthTime(path string) (time.Time, bool) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &st); err != nil || st.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	return time.Unix(st.Btime.Sec, int64(st.Btime.Nsec)), true
}

// workerGitLocalClock reports whether path's birth times come from this
// machine's clock: a local-disk file system, never a Windows drive mounted
// into WSL (/mnt/<letter>; 9p, drvfs or virtiofs, whose clock can differ
// after sleep) or a network share. Anything not on the list is not.
func workerGitLocalClock(path string) bool {
	if workerGitWindowsMount(path) {
		return false
	}
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return false
	}
	switch uint32(st.Type) {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.F2FS_SUPER_MAGIC,
		unix.TMPFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC, workerGitZFSMagic:
		return true
	}
	return false
}

const workerGitZFSMagic = 0x2fc12fc1
