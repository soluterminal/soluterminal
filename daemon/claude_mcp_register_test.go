package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeMcpAction(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"", "add"},
		{"   ", "add"},
		// Bind addresses can't be dialed — the exact misconfiguration that
		// silently stripped every soluterminal tool from one install.
		{"http://0.0.0.0:8787/mcp", "fix"},
		{"http://[::]:8787/mcp", "fix"},
		{"not a url", "fix"},
		// Healthy or deliberate entries stay untouched…
		{"http://127.0.0.1:8787/mcp", "keep"},
		{"http://localhost:8787/mcp", "keep"},
		// …including another daemon's port (multi-backend setups share one
		// config) and remote setups.
		{"http://127.0.0.1:18787/mcp", "keep"},
		{"http://my-desktop.tailnet.ts.net:8787/mcp", "keep"},
	}
	for _, c := range cases {
		if got := claudeMcpAction(c.url); got != c.want {
			t.Errorf("claudeMcpAction(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestClaudeMcpNeedsHeaderUpgrade(t *testing.T) {
	const desired = "http://127.0.0.1:8787/mcp"
	tests := []struct {
		name      string
		action    string
		existing  string
		hasHeader bool
		want      bool
	}{
		{name: "matching keep without header", action: "keep", existing: desired, want: true},
		{name: "matching keep with header", action: "keep", existing: desired, hasHeader: true},
		{name: "localhost keep without header", action: "keep", existing: "http://localhost:8787/mcp", want: true},
		{name: "localhost keep with header", action: "keep", existing: "http://localhost:8787/mcp", hasHeader: true},
		{name: "foreign keep without header", action: "keep", existing: "http://127.0.0.1:18787/mcp"},
		{name: "foreign keep with header", action: "keep", existing: "http://127.0.0.1:18787/mcp", hasHeader: true},
		{name: "add", action: "add", existing: desired},
		{name: "fix", action: "fix", existing: desired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeMcpNeedsHeaderUpgrade(tt.action, tt.existing, desired, tt.hasHeader); got != tt.want {
				t.Fatalf("claudeMcpNeedsHeaderUpgrade() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudeMcpAddArgs(t *testing.T) {
	desired := "http://127.0.0.1:8787/mcp"
	want := []string{"mcp", "add", "--transport", "http", "--scope", "user", "soluterminal", desired, "--header", claudeMcpPaneHeader}
	got := claudeMcpAddArgs(desired)
	if len(got) != len(want) {
		t.Fatalf("claudeMcpAddArgs() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("claudeMcpAddArgs()[%d] = %q, want %q (all args: %#v)", i, got[i], want[i], got)
		}
	}
}

func TestShouldSelfRegister(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"0.0.0.0:8787", true},
		// Sandbox/dev/secondary daemons must never adopt the shared entry.
		{"127.0.0.1:18890", false},
		{"127.0.0.1:18787", false},
		// Dual mode's Windows backend owns the Windows-side registration.
		{"127.0.0.1:38787", true},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := shouldSelfRegister(c.addr); got != c.want {
			t.Errorf("shouldSelfRegister(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestClaudeMcpDesiredURL(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"127.0.0.1:8787", "http://127.0.0.1:8787/mcp"},
		// Binding wide for Tailscale/LAN doesn't change how local claude dials us.
		{"0.0.0.0:9000", "http://127.0.0.1:9000/mcp"},
		{"[::]:8787", "http://127.0.0.1:8787/mcp"},
		{"garbage", "http://127.0.0.1:8787/mcp"},
	}
	for _, c := range cases {
		if got := claudeMcpDesiredURL(c.addr); got != c.want {
			t.Errorf("claudeMcpDesiredURL(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}

func TestRegisterClaudeMcpDirect(t *testing.T) {
	useMCPRegistration(t, "", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")

	// Broken bind-address entry alongside unrelated keys → fixed in place,
	// nothing else touched.
	seed := `{"numStartups":7,"mcpServers":{"soluterminal":{"type":"http","url":"http://0.0.0.0:8787/mcp"},"other":{"type":"stdio","command":"x"}}}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}
	registerClaudeMcpDirect("http://127.0.0.1:8787/mcp", "fix", "http://0.0.0.0:8787/mcp", false, "test")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("rewritten config no longer parses: %v", err)
	}
	if got := cfg["numStartups"]; got != float64(7) {
		t.Errorf("unrelated key clobbered: numStartups=%v", got)
	}
	servers := cfg["mcpServers"].(map[string]any)
	solu := servers["soluterminal"].(map[string]any)
	if solu["url"] != "http://127.0.0.1:8787/mcp" || solu["type"] != "http" {
		t.Errorf("entry not healed: %v", solu)
	}
	headers, ok := solu["headers"].(map[string]any)
	if !ok || headers[claudeMcpPaneHeaderName] != claudeMcpPaneHeaderValue {
		t.Errorf("pane header not registered: %v", solu["headers"])
	}
	if _, ok := servers["other"].(map[string]any); !ok {
		t.Errorf("sibling MCP entry lost")
	}

	// Unparseable config → left byte-for-byte untouched.
	garbage := `{"mcpServers": not-json`
	if err := os.WriteFile(path, []byte(garbage), 0644); err != nil {
		t.Fatal(err)
	}
	registerClaudeMcpDirect("http://127.0.0.1:8787/mcp", "add", "", false, "test")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != garbage {
		t.Errorf("unparseable config was overwritten")
	}
}

// Dual mode moved the Windows backend from :8787 to :38787 (WSL's localhost
// forward owns Windows' :8787 there). A Windows machine's existing entry on the
// other canonical port must follow the Windows daemon, in either direction;
// nothing else may be taken over, and Linux keeps the plain keep-policy.
func TestMcpTakeoverSiblingEntry(t *testing.T) {
	const win, dual = "http://127.0.0.1:8787/mcp", "http://127.0.0.1:38787/mcp"
	cases := []struct {
		name, goos, existing, desired string
		want                          bool
	}{
		{"windows Dual adopts the Windows-only entry", "windows", win, dual, true},
		{"windows back to Windows-only adopts the Dual entry", "windows", dual, win, true},
		{"localhost spelling counts", "windows", "http://localhost:8787/mcp", dual, true},
		{"already ours", "windows", dual, dual, false},
		{"linux WSL daemon keeps the keep-policy", "linux", dual, win, false},
		{"custom port is the user's choice", "windows", "http://127.0.0.1:9999/mcp", dual, false},
		{"remote host is never ours", "windows", "http://my-desktop.tailnet.ts.net:8787/mcp", dual, false},
		{"different path is not our endpoint", "windows", "http://127.0.0.1:8787/other", dual, false},
		{"no entry is the add path, not takeover", "windows", "", dual, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mcpTakeoverSiblingEntry(c.goos, c.existing, c.desired); got != c.want {
				t.Fatalf("mcpTakeoverSiblingEntry(%q, %q, %q) = %v, want %v", c.goos, c.existing, c.desired, got, c.want)
			}
		})
	}
}
