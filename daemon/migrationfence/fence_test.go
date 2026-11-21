package migrationfence

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPresenceRefusesWithoutReadingOrRemovingAnything(t *testing.T) {
	for _, name := range []string{InProgress, Retired} {
		t.Run(name, func(t *testing.T) {
			for _, kind := range []string{"empty", "malformed", "directory", "link"} {
				t.Run(kind, func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, name)
					switch kind {
					case "directory":
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					case "link":
						if err := os.Symlink(filepath.Join(dir, "absent"), path); err != nil {
							t.Skip(err)
						}
					default:
						if err := os.WriteFile(path, []byte(kind), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if err := Check(dir); !errors.Is(err, ErrFenced) {
						t.Fatalf("fence = %v", err)
					}
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("marker changed: %v", err)
					}
				})
			}
		})
	}
}

func TestAbsentAllowsAnOrdinaryLinuxDataDirectory(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "not-created")} {
		if err := Check(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnprovenPresenceRefuses(t *testing.T) {
	if err := Check(""); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	calls := 0
	err := check(t.TempDir(), func(string) (fs.FileInfo, error) { calls++; return nil, fs.ErrPermission })
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrFenced) || strings.Contains(err.Error(), Message) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestInvalidDataDirectoryIsUncertainRatherThanRetired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Check(path); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), Message) {
		t.Fatalf("uncertainty mislabeled as retirement: %v", err)
	}
}
