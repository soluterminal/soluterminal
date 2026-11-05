//go:build linux

package main

import (
	"encoding/binary"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func serviceRequestLocal(r *http.Request, backend string, signed bool) bool {
	if isLoopback(r) {
		return true
	}
	if backend != "wsl" || !signed {
		return false
	}
	// NAT traffic may be a LAN portproxy: gateway alone is never authority.
	// Mirrored WSL has no NAT gateway exception.
	mode, _ := serviceCommand("/usr/bin/wslinfo", "--networking-mode")
	if mode != "nat" {
		return false
	}
	routes, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return false
	}
	return serviceGatewayAllowed(backend, signed, mode, r.RemoteAddr, string(routes))
}

func serviceGatewayAllowed(backend string, signed bool, mode, remote, routes string) bool {
	return backend == "wsl" && signed && mode == "nat" && serviceNATGateway(remote, routes)
}

func serviceNATGateway(remote, routes string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return false
	}
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		gateway, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || gateway == 0 {
			continue
		}
		ip := make(net.IP, 4)
		binary.LittleEndian.PutUint32(ip, uint32(gateway))
		if peer.Equal(ip) {
			return true
		}
	}
	return false
}
