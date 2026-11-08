//go:build !windows

package main

import (
	"os"
	"time"
)

// Only the Windows npm launch path persists these JSON files in production.
func sweepProviderConfigsAtStartup(time.Time) {}

func providerConfigCandidates(root string) (func() (string, error), func(), error) {
	f, err := os.Open(root)
	if err != nil {
		return nil, nil, err
	}
	return func() (string, error) {
		entries, err := f.ReadDir(1)
		if err != nil {
			return "", err
		}
		return entries[0].Name(), nil
	}, func() { f.Close() }, nil
}
