//go:build !linux

package main

// Installed discovery is currently a Linux/WSL contract. Other platforms retain
// environment and development endpoints without advertised relay capabilities.
func (c *shinoqueDiscoveryCache) discover() (shinoqueRelayConfig, bool, string) {
	return shinoqueRelayConfig{}, false, ""
}
