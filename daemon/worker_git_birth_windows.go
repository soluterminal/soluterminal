//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// workerGitBirthTime is when path itself (not a reparse-point target) was
// created. File-system tunneling can only make it read older, never newer.
func workerGitBirthTime(path string) (time.Time, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, false
	}
	st, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, st.CreationTime.Nanoseconds()), true
}

// workerGitLocalClock reports whether path is on a fixed local drive, whose
// birth times come from this machine's clock: never a UNC path (a network
// share, \\wsl$) or a mapped network or removable drive.
func workerGitLocalClock(path string) bool {
	vol := filepath.VolumeName(path)
	if len(vol) != 2 || vol[1] != ':' {
		return false
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return false
	}
	return windows.GetDriveType(root) == windows.DRIVE_FIXED
}
