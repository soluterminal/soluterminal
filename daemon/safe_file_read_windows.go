//go:build windows

package main

import "os"

// userFileWindowsNames: device namespaces and reserved names are refused
// before opening, since opening one already talks to the device.
const userFileWindowsNames = true

func openUserFileHandle(abs string) (*os.File, error) {
	return os.Open(abs)
}
