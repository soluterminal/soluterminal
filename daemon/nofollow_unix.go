//go:build !windows

package main

import "syscall"

// noFollowFlag makes an open refuse a symlink at the final path element.
const noFollowFlag = syscall.O_NOFOLLOW
