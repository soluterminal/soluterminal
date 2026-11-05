//go:build !linux

package main

// Installed discovery is currently a Linux/WSL contract. Other platforms retain
// environment and development endpoints without advertised relay capabilities.
func (c *shinoqueDiscoveryCache) discover() (shinoqueRelayConfig, bool, string) {
	return shinoqueRelayConfig{}, false, ""
}

// Other platforms lack Linux /proc socket ownership data; retain their behavior.
func shinoqueLocalListenerAllowed(port string) bool { return true }

// Discovery is never found here, so there is no file-sourced port to scan.
func (c *shinoqueDiscoveryCache) soludRejection(port string, generation uint64, tables *shinoqueProcTables) string {
	return ""
}

func shinoqueLocalListenerAllowedFrom(port string, tables *shinoqueProcTables) bool { return true }
