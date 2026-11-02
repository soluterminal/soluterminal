package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// Shared scoped-client helpers remain required by managed agents and role panes.
func loopbackMCPURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No host:port split (bare port or malformed) — best-effort loopback.
		host, port = "", addr
		if strings.HasPrefix(addr, ":") {
			port = strings.TrimPrefix(addr, ":")
		}
	}
	switch host {
	case "", "0.0.0.0", "::", "*":
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/mcp", net.JoinHostPort(host, port))
}

func scopedMCPConfigJSON(mcpURL, token string) ([]byte, error) {
	body := map[string]any{
		"mcpServers": map[string]any{
			"soluterminal": map[string]any{
				"type": "http",
				"url":  mcpURL,
				"headers": map[string]any{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}
	return json.Marshal(body)
}

func mcpToolFlagNames(tools []string) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = "mcp__soluterminal__" + t
	}
	return out
}
