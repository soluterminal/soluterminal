package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderConfigSweep(t *testing.T) {
	for _, tc := range []struct {
		name string
		idle bool
	}{
		{"old leftovers", true},
		{"live pane", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			boot := time.Now()
			old := providerConfigPrefix + "old"
			other := providerConfigPrefix + "other-user"
			fresh := providerConfigPrefix + "new"
			unexpected := providerConfigPrefix + "unexpected"
			for _, name := range []string{old, other, fresh, unexpected, "unrelated", providerConfigPrefix} {
				path := filepath.Join(root, name)
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "1.json"), []byte(`{"header":"private"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(root, unexpected, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			// Ownership/time policy is platform-specific; keep the portable
			// filesystem behavior covered by the full remote Linux suite.
			eligible := func(path string, started time.Time) (bool, error) {
				if started != boot {
					t.Fatal("lost boot cutoff")
				}
				return filepath.Base(path) != other && filepath.Base(path) != fresh, nil
			}
			removed, err := sweepProviderConfigDirs(root, boot, eligible, func() (bool, error) { return tc.idle, nil })
			if err == nil { // unexpected contents or a live process must be reported
				t.Fatal("missing diagnostic")
			}
			wantRemoved := 0
			if tc.idle {
				wantRemoved = 1
			}
			if removed != wantRemoved {
				t.Fatalf("removed %d, want %d: %v", removed, wantRemoved, err)
			}
			for _, name := range []string{old, other, fresh, unexpected, "unrelated", providerConfigPrefix} {
				_, statErr := os.Stat(filepath.Join(root, name, "1.json"))
				if name == old && tc.idle {
					if !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("old config remains: %v", statErr)
					}
				} else if statErr != nil {
					t.Fatalf("protected config %s was touched: %v", name, statErr)
				}
			}
		})
	}
}

func TestProviderConfigSweepStartupErrorsAreSoft(t *testing.T) {
	for _, failure := range []string{"inventory", "directory", "ownership"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, providerConfigPrefix+"old"), 0700); err != nil {
				t.Fatal(err)
			}
			if failure == "directory" {
				root = filepath.Join(root, "missing")
			}
			var logs []string
			logProviderConfigSweep(root, time.Now(), func(string, time.Time) (bool, error) {
				return false, errors.New("ownership check failed")
			}, func() (bool, error) {
				if failure == "inventory" {
					return false, errors.New("inventory failed")
				}
				return true, nil
			}, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
			// The same void startup wrapper returns normally after any failure.
			if len(logs) != 1 || !strings.Contains(logs[0], "removed=0") || strings.Contains(logs[0], "result=<nil>") {
				t.Fatalf("missing fail-soft startup log: %v", logs)
			}
		})
	}
}

func TestProviderConfigSweepEntryBound(t *testing.T) {
	root := t.TempDir()
	for i := range 65 {
		if err := os.Mkdir(filepath.Join(root, providerConfigPrefix+fmt.Sprint(i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := sweepProviderConfigDirs(root, time.Now(), func(string, time.Time) (bool, error) { return true, nil }, func() (bool, error) { return true, nil })
	if removed != 64 || err == nil {
		t.Fatalf("sweep not bounded: removed=%d err=%v", removed, err)
	}
}

func TestProviderConfigSweepDoesNotFollowLinks(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	secret := filepath.Join(target, "1.json")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, providerConfigPrefix+"link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	removed, err := sweepProviderConfigDirs(root, time.Now(), func(string, time.Time) (bool, error) {
		t.Fatal("symlink reached ownership check")
		return true, nil
	}, func() (bool, error) { return true, nil })
	if removed != 0 || err != nil {
		t.Fatalf("symlink affected sweep: %d %v", removed, err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("linked config touched: %v", err)
	}
}
