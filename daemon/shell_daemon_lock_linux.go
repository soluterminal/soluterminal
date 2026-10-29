package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Discovery must never acquire the lifetime lock, even briefly: a daemon
// starting at that moment could mistake the shim for an existing server.
func shellDaemonLockHeld(f *os.File, pid int) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	locks, err := os.Open("/proc/locks")
	if err != nil {
		return false
	}
	defer locks.Close()
	device := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)), stat.Ino)
	return shellDaemonLockListed(io.LimitReader(locks, 4<<20), pid, device)
}

func shellDaemonLockListed(locks io.Reader, pid int, device string) bool {
	scanner := bufio.NewScanner(locks)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 8 && fields[1] == "FLOCK" && fields[2] == "ADVISORY" && fields[3] == "WRITE" && fields[4] == strconv.Itoa(pid) && fields[5] == device && fields[6] == "0" && fields[7] == "EOF" {
			return true
		}
	}
	return false
}
