//go:build windows

package main

// Windows has no /proc to read agent binaries from; nothing verifies.
func agentExeOwnedByDaemonPlatform(string) bool { return false }

func exeFileIdentity(string) (exeFileID, bool) { return exeFileID{}, false }
