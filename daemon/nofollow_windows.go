//go:build windows

package main

// noFollowFlag is 0 on Windows, which has no O_NOFOLLOW; the capped
// diagnostics that use it run only in WSL helper agents.
const noFollowFlag = 0
