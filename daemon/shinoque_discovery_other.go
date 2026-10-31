//go:build !linux

package main

// Installed discovery is currently a Linux/WSL contract. Other platforms keep
// using the existing environment and development defaults.
func (c *shinoqueDiscoveryCache) discover() (shinoqueRelayConfig, bool, string) {
	return shinoqueRelayConfig{}, false, ""
}
