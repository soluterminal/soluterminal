//go:build darwin

package main

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// mcpSocketPeerCredPlatform reads LOCAL_PEERCRED for the uid and
// LOCAL_PEERPID for the pid of the process that connected.
func mcpSocketPeerCredPlatform(c net.Conn) (uid, pid int, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Xucred
	var credErr, pidErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, 0, err
	}
	if credErr != nil {
		return 0, 0, credErr
	}
	if pidErr != nil {
		return 0, 0, pidErr
	}
	return int(cred.Uid), pid, nil
}

// mcpProcessStartTimePlatform is the process start time from the kernel's
// process table, in microseconds.
func mcpProcessStartTimePlatform(pid int) (uint64, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return 0, false
	}
	t := kp.Proc.P_starttime
	start := uint64(t.Sec)*1_000_000 + uint64(t.Usec)
	return start, start > 0
}

// macOS has no DrvFs.
func mcpSocketFilesystemOKPlatform(string) error { return nil }
