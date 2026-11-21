//go:build !linux

package main

// Linux retirement never prevents pairing or starting the Windows host.
func checkStatefulMigrationFence(string) error { return nil }
func checkStartupMigrationFence(string) error  { return nil }
