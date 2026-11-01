package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func statShinoqueToken(path string) (os.FileInfo, error) { return os.Lstat(path) }

func shinoqueTokenMetadata(info os.FileInfo) bool {
	_, reason := shinoqueDiscoveryMetadata(info)
	return reason == ""
}

func openShinoqueToken(path string) (*os.File, error) {
	// NONBLOCK also prevents a replacement FIFO from stalling the request.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
