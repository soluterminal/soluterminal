//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The Windows-native daemon takes the same one-daemon-per-data-directory lock
// as Linux. It used to rely on the desktop shell keeping one daemon per data
// directory, but a daemon that no window owns (ST-253, ST-246) can be started
// by sign-in, by either app or by a repair button, and the port check alone
// trips only after state is loaded and panes are restored.
//
// LockFileEx locks are mandatory on Windows: a locked byte range can't be read
// through any other handle. The lock therefore covers one byte far past the
// end of the file, so the holder text at the start stays readable to the
// refused daemon and to clients. The lock belongs to this handle; Go opens
// files non-inheritable and passes children only their stdio, so pane agents
// and update successors never hold it. The system releases it when the
// process ends.
const dataDirLockOffsetHigh = 0x7fffffff

func dataDirLockRange() *windows.Overlapped {
	return &windows.Overlapped{OffsetHigh: dataDirLockOffsetHigh}
}

func tryExclusiveFileLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, dataDirLockRange(),
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, dataDirLockRange())
}
