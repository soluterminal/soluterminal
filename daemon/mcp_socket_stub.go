//go:build !linux && !darwin

package main

import (
	"errors"
	"net"
)

// The MCP socket needs kernel peer credentials, implemented for Linux and
// macOS. Elsewhere (Windows until its own change) the endpoint stays off and
// agents keep HTTP /mcp.

func prepareMCPSocket(string) *mcpSocketEndpoint { return nil }

func (e *mcpSocketEndpoint) activate() error { return errors.New("mcp socket unsupported") }

func (e *mcpSocketEndpoint) close() {}

func mcpSocketPeerCredPlatform(net.Conn) (int, int, error) {
	return 0, 0, errors.New("peer credentials unsupported")
}

func mcpProcessStartTimePlatform(int) (uint64, bool) { return 0, false }

func mcpSocketFilesystemOKPlatform(string) error { return errors.New("unsupported") }
