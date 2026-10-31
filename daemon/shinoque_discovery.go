package main

import (
	"log"
	"os"
	"sync"
	"time"
)

type shinoqueRelayConfig struct {
	port string
	home string
}

// Discovery is per token cache, shared by the relay and health capability probe.
// Platform-specific readers revalidate metadata even when contents are cached.
type shinoqueDiscoveryCache struct {
	mu         sync.Mutex
	path       string
	mtime      time.Time
	size       int64
	inode      uint64
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
