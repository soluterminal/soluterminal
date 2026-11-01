package main

import (
	"log"
	"os"
	"sync"
)

type shinoqueRelayConfig struct {
	port               string
	home               string
	relayAuth          int
	privateDeviceProof int
}

func (c shinoqueRelayConfig) supportsRelayAuth() bool { return c.relayAuth == 2 }

// supportsPrivateDeviceProof reports HMAC device proof support from discovery.
func (c shinoqueRelayConfig) supportsPrivateDeviceProof() bool { return c.privateDeviceProof == 1 }

// Discovery is per token cache, shared by the relay and health capability probe.
// Platform-specific readers revalidate metadata and bytes before reusing parsed contents.
type shinoqueDiscoveryCache struct {
	mu         sync.Mutex
	path       string
	content    []byte
	config     shinoqueRelayConfig
	source     string
	lastConfig shinoqueRelayConfig
	rejection  string
}

func (c *shinoqueDiscoveryCache) resolve() (shinoqueRelayConfig, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	config := shinoqueRelayConfig{port: shinoquePort()}
	config.home = shinoqueTokenHome()
	source, reason := "default", ""
	_, portSet := os.LookupEnv("SHINOQUE_RELAY_PORT")
	_, homeSet := os.LookupEnv("SHINOQUE_HOME")
	if portSet || homeSet {
		source = "env"
	} else {
		var found bool
		var discovered shinoqueRelayConfig
		discovered, found, reason = c.discover()
		if found {
			source, config = "file", discovered
		}
	}
	if source != c.source || config != c.lastConfig {
		log.Printf("shinoque relay: source=%s", source)
	}
	if reason != "" && (reason != c.rejection || source != c.source) {
		log.Printf("shinoque relay: discovery rejected: %s", reason)
	}
	c.source, c.lastConfig, c.rejection = source, config, reason
	return config, reason == ""
}
