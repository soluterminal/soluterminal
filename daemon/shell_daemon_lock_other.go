//go:build !linux

package main

import "os"

// Retain the inherited address where kernel owner inspection is unavailable.
func shellDaemonLockHeld(*os.File, int) bool { return false }
