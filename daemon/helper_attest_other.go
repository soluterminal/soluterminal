//go:build !linux

package main

import (
	"errors"
	"net"
)

// The helper runs only inside a Linux distro.
func helperPeerOfPlatform(net.Conn) (helperPeer, error) {
	return helperPeer{}, errors.New("helper attestation is Linux-only")
}
