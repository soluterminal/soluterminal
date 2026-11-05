//go:build !windows

package main

import (
	"os"
	"syscall"
)

func agentExeOwnedByDaemonPlatform(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// exeFileIdentity is the device and inode of the file at path (followed, so
// /proc/<pid>/exe names the file the process runs, even once deleted).
func exeFileIdentity(path string) (exeFileID, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return exeFileID{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return exeFileID{}, false
	}
	return exeFileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}
