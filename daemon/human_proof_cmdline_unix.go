//go:build !windows

package main

// browserCommandLine is a browser process's command line, for spotting
// automation switches (human_proof.go).
func browserCommandLine(pid int) string { return readCmdline(pid) }
