//go:build windows

package main

import (
	"errors"
	"io/fs"
)

// seedFileOwnedPrivately: the Windows owner/ACL check is not implemented, so
// seeding is refused there rather than run without it.
func seedFileOwnedPrivately(fs.FileInfo) error {
	return errors.New("seeded panes are not supported on Windows")
}
