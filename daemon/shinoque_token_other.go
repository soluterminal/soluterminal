//go:build !linux

package main

import "os"

func statShinoqueToken(path string) (os.FileInfo, error) { return os.Stat(path) }

// Installed discovery is Linux/WSL only. Keep the development token contract
// on other platforms, where Unix ownership and 0600 are not portable.
func shinoqueTokenMetadata(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Size() <= 4096
}

func openShinoqueToken(path string) (*os.File, error) {
	return os.Open(path)
}
