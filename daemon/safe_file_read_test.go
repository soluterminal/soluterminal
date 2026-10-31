package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadUserFileReadsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, info, err := readUserFile(path, maxFileBytes)
	if err != nil {
		t.Fatalf("readUserFile: %v", err)
	}
	if string(data) != "one\ntwo\n" || info.Size() != 8 || !info.Mode().IsRegular() {
		t.Fatalf("data=%q size=%d mode=%v", data, info.Size(), info.Mode())
	}
}

func TestReadUserFileRefusesOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", maxFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readUserFile(path, maxFileBytes); !errors.Is(err, errUserFileTooLarge) {
		t.Fatalf("err = %v, want errUserFileTooLarge", err)
	}
	// Exactly at the cap is fine.
	if err := os.WriteFile(path, []byte(strings.Repeat("a", maxFileBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readUserFile(path, maxFileBytes); err != nil {
		t.Fatalf("at-cap read: %v", err)
	}
}

func TestReadUserFileRefusesDirectory(t *testing.T) {
	_, _, err := readUserFile(t.TempDir(), maxFileBytes)
	if !errors.Is(err, errUserFileIsDir) || !errors.Is(err, errUserFileNotRegular) {
		t.Fatalf("err = %v, want errUserFileIsDir", err)
	}
	msg, status := userFileReadError(err)
	if status != 400 || !strings.Contains(msg, "directory") {
		t.Fatalf("mapped to %d %q", status, msg)
	}
}

func TestUserFileReadErrorStatuses(t *testing.T) {
	if _, status := userFileReadError(errUserFileTooLarge); status != 413 {
		t.Fatalf("too large -> %d, want 413", status)
	}
	_, _, err := readUserFile(filepath.Join(t.TempDir(), "missing.txt"), maxFileBytes)
	if _, status := userFileReadError(err); status != 404 {
		t.Fatalf("missing -> %d (%v), want 404", status, err)
	}
}

func TestWindowsDevicePathRefusal(t *testing.T) {
	refused := []string{
		`\\.\NUL`,
		`\\.\pipe\solu`,
		`//./pipe/solu`,
		`\\?\GLOBALROOT\Device\Null`,
		`\\?\C:\Users\x\notes.txt`,
		`//?/C:/Users/x/notes.txt`,
		`\??\C:\x`,
		`C:\Users\x\NUL`,
		`C:\Users\x\nul.txt`,
		`C:\Users\x\CON`,
		`C:\Users\x\con.tar.gz`,
		`C:\Users\x\PRN`,
		`C:\Users\x\aux.log`,
		`C:\Users\x\COM1`,
		`C:\Users\x\com9.txt`,
		`C:\Users\x\LPT1`,
		`C:\Users\x\lpt9.md`,
		`C:\Users\x\NUL .txt`,
		`C:\Users\x\NUL:`,
		`C:\Users\x\CONIN$`,
		`C:\Users\x\CONOUT$`,
		`C:\Users\x\COM¹`,
		`C:\NUL\notes.txt`, // a reserved name anywhere in the path
		`C:/Users/x/nul`,
	}
	for _, p := range refused {
		if windowsDevicePathRefusal(p) == "" {
			t.Errorf("windowsDevicePathRefusal(%q) = \"\", want a refusal", p)
		}
	}
	allowed := []string{
		`C:\Users\x\notes.txt`,
		`C:\Users\x\console.log`,
		`C:\Users\x\COM0`,
		`C:\Users\x\COM10`,
		`C:\Users\x\LPT`,
		`C:\Users\x\nullable.go`,
		`C:\Users\x\auxiliary\a.txt`,
		`C:\Users\x\.con`,
		`\\server\share\notes.txt`,
		`/home/x/notes.txt`,
	}
	for _, p := range allowed {
		if got := windowsDevicePathRefusal(p); got != "" {
			t.Errorf("windowsDevicePathRefusal(%q) = %q, want allowed", p, got)
		}
	}
}
