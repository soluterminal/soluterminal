package main

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// The desktop keeps local integrations on loopback and adds only WSL's exact
// NAT interface address for Windows. Mirrored/fallback launches need no second
// socket. The additional listener serves the same handler and auth gate.
func wslListenerAddresses(primary, extra, backend string) ([]string, error) {
	addresses := []string{primary}
	if extra == "" {
		return addresses, nil
	}
	host, port, err := net.SplitHostPort(primary)
	if err != nil || host != "127.0.0.1" || backend != "wsl" {
		return nil, fmt.Errorf("--wsl-addr requires --backend-id wsl and an IPv4 loopback --addr")
	}
	extraHost, extraPort, err := net.SplitHostPort(extra)
	ip, parseErr := netip.ParseAddr(extraHost)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || parseErr != nil || !ip.Is4() || (!ip.IsGlobalUnicast() && extraHost != "127.0.0.1") || extraPort != port || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("--wsl-addr must be a concrete IPv4 address on the same nonzero port as --addr")
	}
	if extra != primary {
		addresses = append(addresses, extra)
	}
	return addresses, nil
}

// Preflight every socket before acknowledging an update successor. A failed
// bind closes every socket acquired so far; a partial set must never start
// integrations or retain listener ownership after startup fails.
func startupHandoffListenersWithDeps(network string, addresses []string, deps startupHandoffDeps) (listeners []net.Listener, err error) {
	listeners = make([]net.Listener, len(addresses))
	defer func() {
		if err != nil {
			for _, ln := range listeners {
				if ln != nil {
					_ = ln.Close()
				}
			}
			listeners = nil
		}
	}()
	for i, addr := range addresses {
		listeners[i], err = deps.preflight(network, addr)
		if err != nil {
			return listeners, fmt.Errorf("preflight %s: %w", addr, err)
		}
	}
	deps.writeMarker()
	deps.clearPending()
	for i, addr := range addresses {
		if listeners[i] == nil {
			listeners[i], err = deps.acquire(network, addr)
			if err != nil {
				return listeners, fmt.Errorf("bind %s: %w", addr, err)
			}
		}
	}
	return listeners, nil
}
