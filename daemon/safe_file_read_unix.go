//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// userFileWindowsNames: CON, NUL... are ordinary file names on Unix.
const userFileWindowsNames = false

// openUserFileHandle opens without blocking (a named pipe with no writer
// would otherwise hang open(2)) and without following a final symlink (the
// path is already canonical; a link swapped in since is refused).
func openUserFileHandle(abs string) (*os.File, error) {
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.EOPNOTSUPP):
		// open(2) on a Unix socket (or a device with no driver behind it).
		return nil, fmt.Errorf("%w: path is a socket or device; only regular files can be read", errUserFileNotRegular)
	case errors.Is(err, syscall.ELOOP):
		// O_NOFOLLOW hit a symlink at the final component.
		return nil, fmt.Errorf("%w: path is a symbolic link; only regular files can be read", errUserFileNotRegular)
	}
	return nil, err
}
