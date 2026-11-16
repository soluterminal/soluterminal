//go:build windows

package main

// procParent is unknown on Windows, so a hook record never proves its sender
// there and terminal one-tap stays off (Review).
func procParent(int) int { return 0 }
