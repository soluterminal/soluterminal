//go:build !windows

package main

import "net"

// Only a Windows-native solud relays through the Shinoque desktop; Linux
// forwards to a discovered local listener.
func newPlatformShinoqueRelayBridge() *shinoqueRelayBridge { return nil }

func shinoqueRelayDesktopOwner(net.Conn) (shinoqueRelayOwner, string) {
	return shinoqueRelayOwner{}, "no desktop route on this platform"
}
