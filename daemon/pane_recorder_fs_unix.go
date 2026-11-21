//go:build !windows

package main

import (
	"os"
	"syscall"
)

// openNewRecordingFile creates path for writing only if it doesn't exist, and
// never through a symlink.
func openNewRecordingFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, recordingFileMode)
}

// openExistingRecordingFile reopens a closed recording for truncation, never
// through a symlink.
func openExistingRecordingFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
}

// readRecordingTimeline opens a session timeline to read its "cast" lines,
// never through a symlink.
func readRecordingTimeline(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func recordingOwnedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
