package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type shinoqueRelayConfig struct {
	port               string
	home               string
	relayAuth          int
	privateDeviceProof int
	// relayKey 1 marks a core that pushes its relay key (shinoque_relay_key.go).
	relayKey int
	// relayV2 signs every forward with a per-request MAC and never sends the
	// raw key; the Windows bridge always sets it (shinoque_relay_bridge.go).
	relayV2 bool
}

func (c shinoqueRelayConfig) supportsRelayAuth() bool { return c.relayAuth == 2 }

// supportsPrivateDeviceProof reports HMAC device proof support from discovery.
func (c shinoqueRelayConfig) supportsPrivateDeviceProof() bool { return c.privateDeviceProof == 1 }

// Discovery is per token cache, shared by the relay and health capability probe.
// Platform-specific readers revalidate metadata and bytes before reusing parsed contents.
type shinoqueDiscoveryCache struct {
	mu         sync.Mutex
	path       string
	identity   shinoqueFileIdentity
	content    []byte
	homeID     shinoqueFileIdentity
	config     shinoqueRelayConfig
	source     string
	lastConfig shinoqueRelayConfig
	rejection  string
	// envOutcome is the last env-vs-relay.json result ("" matched, or the
	// failed check: missing, rejected, port, home), logged once per change
	// (envNoted: logged at all).
	envOutcome string
	envNoted   bool

	// generation changes whenever the discovery file, its home, the token file
	// or a forwarded connection gives reason to distrust the cached solud verdict.
	generation atomic.Uint64
	solud      shinoqueSoludVerdict
	// Test seams; nil uses the real /proc scan and clock.
	scanSolud func(sockets map[string]bool) (bool, error)
	now       func() time.Time

	// bridge, when set, is the only source: the core's push through the
	// Shinoque desktop. No env, file or default source is read (Windows).
	bridge *shinoqueRelayBridge
}

type shinoqueFileIdentity struct {
	dev, ino    uint64
	mtime, size int64
}

// The cached solud verdict for one set of listening sockets; see soludRejection.
type shinoqueSoludVerdict struct {
	mu         sync.Mutex
	key        string
	generation uint64
	expires    time.Time
	inflight   map[string]*shinoqueSoludScan
}

type shinoqueSoludScan struct {
	done   chan struct{}
	reason string
}

// invalidate drops the cached solud verdict. The next relay request rescans.
func (c *shinoqueDiscoveryCache) invalidate() { c.generation.Add(1) }

// shinoqueProcTables memoizes the kernel socket tables for one relay request,
// so a cache hit reads them once for both the solud verdict key and the
// listener check. A nil value reads the files directly.
type shinoqueProcTables struct {
	data map[string][]byte
	errs map[string]error
}

func (t *shinoqueProcTables) read(path string) ([]byte, error) {
	if t == nil {
		return os.ReadFile(path)
	}
	if data, ok := t.data[path]; ok {
		return data, t.errs[path]
	}
	data, err := os.ReadFile(path)
	if t.data == nil {
		t.data, t.errs = make(map[string][]byte), make(map[string]error)
	}
	t.data[path], t.errs[path] = data, err
	return data, err
}

// reset discards the snapshot after a scan, so the listener check rereads
// the tables just before the request is forwarded.
func (t *shinoqueProcTables) reset() {
	if t != nil {
		t.data, t.errs = nil, nil
	}
}

func (c *shinoqueDiscoveryCache) resolve() (shinoqueRelayConfig, bool) {
	return c.resolveWith(nil)
}

func (c *shinoqueDiscoveryCache) resolveWith(tables *shinoqueProcTables) (shinoqueRelayConfig, bool) {
	if c.bridge != nil {
		config, reason := c.bridge.current()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.noteResolve("push", config, reason)
		return config, reason == ""
	}
	c.mu.Lock()
	config := shinoqueRelayConfig{port: shinoquePort()}
	config.home = shinoqueTokenHome()
	source, reason := "default", ""
	_, portSet := os.LookupEnv("SHINOQUE_RELAY_PORT")
	_, homeSet := os.LookupEnv("SHINOQUE_HOME")
	if portSet || homeSet {
		source = "env"
		// The environment names the core's port or home, but only the core's
		// own discovery file says what it verifies (ST-256). Take its
		// capabilities when that file passes every check and names this same
		// port and home; otherwise they stay zero and the relay refuses
		// (shinoqueUpdateRequired), as health then reports.
		if config.home != "" {
			// relay.json and pushes name a clean path; "home/" is the same home.
			config.home = filepath.Clean(config.home)
		}
		discovered, found, why := c.discover()
		// outcome names the check, never a value: logs carry no paths or ports.
		outcome := ""
		switch {
		case !found:
			outcome = "missing"
		case why != "":
			outcome = "rejected"
		case discovered.port != config.port:
			outcome = "port"
		case discovered.home != config.home:
			outcome = "home"
		}
		if outcome == "" {
			config.relayAuth = discovered.relayAuth
			config.privateDeviceProof = discovered.privateDeviceProof
			config.relayKey = discovered.relayKey
		}
		if !c.envNoted || outcome != c.envOutcome {
			c.envOutcome, c.envNoted = outcome, true
			if outcome == "" {
				log.Printf("shinoque relay: env core matches relay.json; relay auth from the file")
			} else {
				log.Printf("shinoque relay: env core has no matching relay.json (%s); relay refused until the core writes one for this port and home", outcome)
			}
		}
	} else {
		var found bool
		var discovered shinoqueRelayConfig
		discovered, found, reason = c.discover()
		if found {
			source, config = "file", discovered
		}
	}
	generation := c.generation.Load()
	c.mu.Unlock()
	// Every source is checked: an env or default port is no more trusted than
	// a discovered one. The scan walks every process, so it runs outside c.mu.
	if reason == "" {
		if reason = c.soludRejection(config.port, generation, tables); reason != "" {
			config = shinoqueRelayConfig{}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noteResolve(source, config, reason)
	return config, reason == ""
}

// noteResolve logs a changed source or rejection. c.mu must be held.
func (c *shinoqueDiscoveryCache) noteResolve(source string, config shinoqueRelayConfig, reason string) {
	if source != c.source || config != c.lastConfig {
		log.Printf("shinoque relay: source=%s", source)
	}
	if reason != "" && (reason != c.rejection || source != c.source) {
		log.Printf("shinoque relay: discovery rejected: %s", reason)
	}
	c.source, c.lastConfig, c.rejection = source, config, reason
}
