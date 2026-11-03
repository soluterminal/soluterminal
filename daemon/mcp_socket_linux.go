//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// mcpSocketPeerCredPlatform reads SO_PEERCRED: the uid and pid of the process
// that connected, as the kernel recorded them at connect time.
func mcpSocketPeerCredPlatform(c net.Conn) (uid, pid int, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if credErr != nil {
		return 0, 0, credErr
	}
	return int(cred.Uid), int(cred.Pid), nil
}

// mcpProcessStartTimePlatform is field 22 of /proc/<pid>/stat: the start time
// in clock ticks since boot. It is compared raw, never converted through the
// boot time, so a drifting btime cannot change it.
func mcpProcessStartTimePlatform(pid int) (uint64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parseProcStatStartTime(string(data))
}

// parseProcStatStartTime reads starttime after the command name, which can
// contain spaces and parentheses, so it splits after the last ')'.
func parseProcStatStartTime(stat string) (uint64, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[i+1:])
	// fields[0] is field 3 (state); starttime is field 22.
	if len(fields) < 20 {
		return 0, false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return start, err == nil && start > 0
}

// mcpSocketFilesystemOKPlatform refuses a directory on a Windows drive mounted
// into WSL (DrvFs over 9p): Unix sockets and modes there are not the Linux
// kernel's to enforce.
func mcpSocketFilesystemOKPlatform(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	fstype := mountFSType(bufio.NewScanner(f), real)
	switch fstype {
	case "9p", "drvfs":
		return fmt.Errorf("%s is on a %s filesystem (a Windows drive); the MCP socket needs the Linux filesystem", dir, fstype)
	}
	return nil
}

// mountFSType returns the filesystem type of the longest mount point
// containing path, from /proc/self/mountinfo lines.
func mountFSType(sc *bufio.Scanner, path string) string {
	best, bestType := "", ""
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || sep+1 >= len(fields) {
			continue
		}
		mount := unescapeMountPath(fields[4])
		if !mountContains(mount, path) || len(mount) < len(best) {
			continue
		}
		best, bestType = mount, fields[sep+1]
	}
	return bestType
}

func mountContains(mount, path string) bool {
	if mount == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == mount || strings.HasPrefix(path, mount+"/")
}

// unescapeMountPath undoes mountinfo's octal escapes (\040 for a space).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
