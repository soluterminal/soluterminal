//go:build darwin

package main

import (
	"strings"

	"golang.org/x/sys/unix"
)

// macOS process details for caller tracing, by sysctl (no /proc). Each
// returns ok=false when the process is gone or belongs to another user.

func darwinReadPpid(pid int) (int, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return 0, false
	}
	return int(kp.Eproc.Ppid), true
}

func darwinProcArgs(pid int) (procArgs, bool) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return procArgs{}, false
	}
	return parseProcArgs2(b)
}

// darwinReadComm is the executable's file name ("Google Chrome Helper",
// "com.apple.WebKit.Networking"); the kernel's p_comm is cut to 16 bytes,
// so it is only the fallback.
func darwinReadComm(pid int) (string, bool) {
	if a, ok := darwinProcArgs(pid); ok {
		if name := processNameFromArgs(a); name != "" {
			return name, true
		}
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return "", false
	}
	return strings.TrimRight(string(kp.Proc.P_comm[:]), "\x00"), true
}

func darwinReadEnviron(pid int) (map[string]string, bool) {
	a, ok := darwinProcArgs(pid)
	if !ok {
		return nil, false
	}
	return a.env, true
}

func darwinReadCmdline(pid int) (string, bool) {
	a, ok := darwinProcArgs(pid)
	if !ok {
		return "", false
	}
	s := strings.Join(a.argv, " ")
	if len(s) > 512 {
		s = s[:509] + "..."
	}
	return s, true
}
