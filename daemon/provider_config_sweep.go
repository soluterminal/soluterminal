package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const providerConfigPrefix = "solu-claude-config-"

func logProviderConfigSweep(root string, started time.Time, eligible func(string, time.Time) (bool, error), idle func() (bool, error), logf func(string, ...any)) {
	removed, err := sweepProviderConfigDirs(root, started, eligible, idle)
	logf("Claude config startup sweep: removed=%d, result=%v", removed, err)
}

// The startup caller runs before any pane launch. Eligibility must establish
// user ownership and creation before boot; idle must rule out surviving users
// of the old files, including panes belonging to another daemon.
func sweepProviderConfigDirs(root string, started time.Time, eligible func(string, time.Time) (bool, error), idle func() (bool, error)) (removed int, err error) {
	ok, err := idle()
	if err != nil || !ok {
		if err == nil {
			err = errors.New("deferred: a surviving provider or daemon may still use config files")
		}
		return 0, err
	}
	next, closeScan, err := providerConfigCandidates(root)
	if err != nil {
		return 0, err
	}
	defer closeScan()
	deadline := time.Now().Add(2 * time.Second)
	// Bound directory enumeration, candidates, contents and elapsed work. Do
	// not use RemoveAll: unexpected trees are preserved for inspection.
	candidates := 0
	for scanned := 0; scanned < 4096; scanned++ {
		if time.Now().After(deadline) || candidates >= 64 {
			return removed, errors.Join(err, errors.New("config sweep work limit reached"))
		}
		name, readErr := next()
		if errors.Is(readErr, io.EOF) {
			return removed, err
		}
		if readErr != nil {
			return removed, errors.Join(err, readErr)
		}
		if !strings.HasPrefix(name, providerConfigPrefix) || name == providerConfigPrefix {
			continue
		}
		path := filepath.Join(root, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			err = errors.Join(err, statErr)
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		candidates++
		owned, checkErr := eligible(path, started)
		if checkErr != nil {
			err = errors.Join(err, checkErr)
			continue
		}
		if !owned {
			continue
		}
		if removeErr := removeProviderConfigDir(path); removeErr != nil {
			err = errors.Join(err, removeErr)
		} else {
			removed++
		}
	}
	return removed, errors.Join(err, errors.New("config sweep entry limit reached"))
}

func removeProviderConfigDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(65)
	f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 64 {
		return errors.New("config directory exceeds file limit")
	}
	for _, entry := range entries {
		index, numberErr := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
		if numberErr != nil || index < 0 || entry.Name() != strconv.Itoa(index)+".json" || !entry.Type().IsRegular() {
			return fmt.Errorf("config directory contains unexpected entry")
		}
	}
	// Never chmod or reset the DACL, even on failure. The same private ACL
	// established by newPrivateProviderConfigDir protects every remaining byte.
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
