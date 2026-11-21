package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"soluterminal/migrationfence"
)

func checkStatefulMigrationFence(dataDir string) error { return migrationfence.Check(dataDir) }

func checkStartupMigrationFence(dataDir string) error {
	if err := checkStatefulMigrationFence(dataDir); err != nil {
		return err
	}
	// Migration fences both the service's verified -data directory and this
	// Linux user's default directory. Never let a new -data bypass the latter.
	home, err := migrationLinuxHome(os.UserHomeDir, func() (string, error) {
		entry, err := user.LookupId(strconv.Itoa(os.Getuid()))
		if err != nil {
			return "", err
		}
		return entry.HomeDir, nil
	})
	if err != nil {
		return err
	}
	normal := filepath.Join(home, ".solu")
	if filepath.Clean(dataDir) != filepath.Clean(normal) {
		return checkStatefulMigrationFence(normal)
	}
	return nil
}

// LookupId reads the passwd entry for the real uid; user.Current may cache a
// home derived from environment variables. Inject both sources for fixture-only
// coverage of unset HOME without changing this process's personal environment.
func migrationLinuxHome(environment, passwd func() (string, error)) (string, error) {
	for _, lookup := range []func() (string, error){environment, passwd} {
		if home, err := lookup(); err == nil && filepath.IsAbs(home) {
			return home, nil
		}
	}
	return "", fmt.Errorf("%w: Linux home unavailable; not starting", migrationfence.ErrUnavailable)
}
