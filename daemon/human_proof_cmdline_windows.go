//go:build windows

package main

// browserCommandLine is unavailable on Windows (ports_windows.go explains
// why); automation browsers there are recognised by name and launcher only.
func browserCommandLine(int) string { return "" }
