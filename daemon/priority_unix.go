//go:build !windows

package main

import (
	"log"
	"os"
	"syscall"
)

// lowerProcessPriority is opportunistic: heavy work still runs if either the
// CPU or IO priority adjustment is unavailable. applyDetached makes the child
// its own process-group leader, so PRIO_PGRP covers the descendants that
// exist when it runs and those forked later. A child forked in the instant
// the group is walked can keep the default levels (best effort).
func lowerProcessPriority(p *os.Process) {
	if p == nil {
		return
	}
	if err := syscall.Setpriority(syscall.PRIO_PGRP, p.Pid, 10); err != nil && err != syscall.ESRCH {
		log.Printf("warning: lower CPU priority for pgid %d: %v", p.Pid, err)
	}
	lowerProcessIOPriority(p.Pid)
}
