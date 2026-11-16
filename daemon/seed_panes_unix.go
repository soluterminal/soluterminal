//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// seedFileOwnedPrivately requires the seed file to be owned by this user and
// not writable by group or others.
func seedFileOwnedPrivately(st fs.FileInfo) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the file's owner")
	}
	if int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not this user", sys.Uid)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("mode %o is writable by group or others", st.Mode().Perm())
	}
	return nil
}
