//go:build !darwin && !windows

package main

// Elsewhere /proc answers directly (procfs_unix.go, ports_unix.go).

func darwinReadPpid(int) (int, bool)                  { return 0, false }
func darwinReadComm(int) (string, bool)               { return "", false }
func darwinReadEnviron(int) (map[string]string, bool) { return nil, false }
func darwinReadCmdline(int) (string, bool)            { return "", false }
