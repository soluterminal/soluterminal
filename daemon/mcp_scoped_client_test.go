package main

import (
	"encoding/json"
	"testing"
)

func TestScopedMCPConfig(t *testing.T) {
	t.Parallel()
	cfg, err := scopedMCPConfigJSON("http://127.0.0.1:8787/mcp", "tok123")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	srv, ok := parsed.MCPServers["soluterminal"]
	if !ok || srv.Type != "http" || srv.URL != "http://127.0.0.1:8787/mcp" {
		t.Fatalf("config server = %+v", srv)
	}
	if srv.Headers["Authorization"] != "Bearer tok123" {
		t.Fatalf("auth header = %q, want scoped bearer", srv.Headers["Authorization"])
	}

}
func TestLoopbackMCPURL(t *testing.T) {
	t.Parallel()
	// Wildcard/unspecified hosts MUST normalize to loopback — the child dials
	// this, and 0.0.0.0 (the desktop app's --addr) is not a connect address.
	cases := map[string]string{
		":8787":            "http://127.0.0.1:8787/mcp",
		"0.0.0.0:8787":     "http://127.0.0.1:8787/mcp",
		"[::]:8787":        "http://127.0.0.1:8787/mcp",
		"127.0.0.1:9000":   "http://127.0.0.1:9000/mcp",
		"localhost:8787":   "http://localhost:8787/mcp",
		"192.168.1.5:8787": "http://192.168.1.5:8787/mcp",
	}
	for in, want := range cases {
		if got := loopbackMCPURL(in); got != want {
			t.Fatalf("loopbackMCPURL(%q) = %q, want %q", in, got, want)
		}
	}
}
