//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadUserFileRefusesWindowsDevices(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		`\\.\NUL`,
		`//./NUL`,
		`\\.\pipe\solu-test`,
		`\\?\GLOBALROOT\Device\Null`,
		`NUL`,
		`CON`,
		filepath.Join(dir, "nul.txt"),
		filepath.Join(dir, "COM1"),
		filepath.Join(dir, "lpt9.log"),
		filepath.Join(dir, "CONIN$"),
	} {
		if _, _, err := readUserFile(p, maxFileBytes); !errors.Is(err, errUserFileNotRegular) {
			t.Errorf("readUserFile(%q) err = %v, want errUserFileNotRegular", p, err)
		}
	}
	ok := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(ok, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readUserFile(ok, maxFileBytes); err != nil {
		t.Fatalf("regular file: %v", err)
	}
}
