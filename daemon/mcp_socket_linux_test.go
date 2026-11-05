//go:build linux

package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProcStatStartTime(t *testing.T) {
	// The command name can hold spaces and parentheses.
	stat := "4242 (we ird) (name) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615"
	if got, ok := parseProcStatStartTime(stat); !ok || got != 987654 {
		t.Fatalf("start = %d %t", got, ok)
	}
	for _, bad := range []string{"", "4242 (x) S 1", "4242 noparen"} {
		if _, ok := parseProcStatStartTime(bad); ok {
			t.Fatalf("parsed %q", bad)
		}
	}
}

func TestMountFSTypeFindsDrvFs(t *testing.T) {
	mountinfo := strings.Join([]string{
		`22 1 8:32 / / rw,relatime - ext4 /dev/sdc rw`,
		`80 22 0:52 / /mnt/c rw,noatime - 9p C:\134 rw,aname=drvfs`,
		`81 22 0:53 / /mnt/with\040space rw - drvfs D:\134 rw`,
		`82 22 0:54 / /home/u/.solu rw - tmpfs tmpfs rw`,
	}, "\n")
	for path, want := range map[string]string{
		"/home/u/.local/share":  "ext4",
		"/mnt/c/Users/u/.solu":  "9p",
		"/mnt/cdrom":            "ext4",
		"/mnt/with space/x/run": "drvfs",
		"/home/u/.solu/run":     "tmpfs",
	} {
		if got := mountFSType(bufio.NewScanner(strings.NewReader(mountinfo)), path); got != want {
			t.Fatalf("%s: fstype %q, want %q", path, got, want)
		}
	}
}

// Without a readable mount table the filesystem can't be checked: refused.
func TestMCPSocketFilesystemCheckFailsClosed(t *testing.T) {
	prev := mcpMountInfoPath
	mcpMountInfoPath = filepath.Join(t.TempDir(), "missing-mountinfo")
	t.Cleanup(func() { mcpMountInfoPath = prev })
	if err := mcpSocketFilesystemOKPlatform(t.TempDir()); err == nil {
		t.Fatal("unreadable mountinfo passed the check")
	}
	empty := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mcpMountInfoPath = empty
	if err := mcpSocketFilesystemOKPlatform(t.TempDir()); err == nil {
		t.Fatal("a mount table without the directory passed the check")
	}
}
