//go:build !windows

package main

// procParent is pid's parent pid, 0 when unknown.
func procParent(pid int) int { return procParentAt("/proc", pid) }
