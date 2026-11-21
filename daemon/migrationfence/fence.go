// Package migrationfence implements the Linux side of ST-253's retirement fence.
// It only checks presence in an explicit data directory: no claim reads, cloud
// calls, marker writes or marker removal. Stateless helpers do not call it.
package migrationfence

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	InProgress = "migration-in-progress.json"
	Retired    = "retired-to-host.json"
	ExitCode   = 78
	Message    = "this PC moved to the Windows host"
)

var (
	ErrFenced      = errors.New(Message)
	ErrUnavailable = errors.New("can't check migration state")
)

func Check(dataDir string) error { return check(dataDir, os.Lstat) }

func check(dataDir string, stat func(string) (fs.FileInfo, error)) error {
	if strings.TrimSpace(dataDir) == "" {
		return fmt.Errorf("%w in %q; not starting", ErrUnavailable, dataDir)
	}
	for _, name := range []string{Retired, InProgress} {
		_, err := stat(filepath.Join(dataDir, name))
		if err == nil {
			return ErrFenced
		} // Content and file type never grant access.
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w in %q; not starting", ErrUnavailable, dataDir)
		}
	}
	return nil
}
