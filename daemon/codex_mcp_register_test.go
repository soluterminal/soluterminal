package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const codexCfgSample = `model = "gpt-5"

[mcp_servers.supabase]
command = "npx"
args = ["-y", "@supabase/mcp-server-supabase"]

[mcp_servers.supabase.env]
SUPABASE_ACCESS_TOKEN = "sbp_secret"
url = "https://red-herring.example"

[mcp_servers.soluterminal]
url = "http://0.0.0.0:8787/mcp"

[mcp_servers.docs]
url = "https://developers.example.com/mcp"
`

func TestCodexConfigMcpURL(t *testing.T) {
	if got := codexConfigMcpURL(codexCfgSample, "soluterminal"); got != "http://0.0.0.0:8787/mcp" {
		t.Errorf("soluterminal url = %q", got)
	}
	if got := codexConfigMcpURL(codexCfgSample, "docs"); got != "https://developers.example.com/mcp" {
		t.Errorf("docs url = %q", got)
	}
	// The url inside [mcp_servers.supabase.env] must NOT read as supabase's.
	if got := codexConfigMcpURL(codexCfgSample, "supabase"); got != "" {
		t.Errorf("supabase url picked up from sub-table: %q", got)
	}
	if got := codexConfigMcpURL(codexCfgSample, "missing"); got != "" {
		t.Errorf("missing server url = %q", got)
	}
}

func TestCodexConfigFixMcpURL(t *testing.T) {
	fixed, ok := codexConfigFixMcpURL(codexCfgSample, "soluterminal", "http://127.0.0.1:8787/mcp")
	if !ok {
		t.Fatal("fix reported no url line")
	}
	if codexConfigMcpURL(fixed, "soluterminal") != "http://127.0.0.1:8787/mcp" {
		t.Errorf("url not fixed:\n%s", fixed)
	}
	// Everything except the one line survives byte-for-byte.
	if codexConfigMcpURL(fixed, "docs") != "https://developers.example.com/mcp" {
		t.Errorf("docs entry damaged")
	}
	if !strings.Contains(fixed, `SUPABASE_ACCESS_TOKEN = "sbp_secret"`) {
		t.Errorf("env sub-table damaged")
	}
	if _, ok := codexConfigFixMcpURL("[mcp_servers.other]\nurl = \"x\"\n", "soluterminal", "y"); ok {
		t.Errorf("fixed a section that does not exist")
	}
}

func TestCodexConfigPaneHeaderState(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing", codexCfgSample, codexPaneHeaderMissing},
		{"ours", strings.Replace(codexCfgSample,
			`url = "http://0.0.0.0:8787/mcp"`,
			`url = "http://0.0.0.0:8787/mcp"`+"\n"+codexMcpPaneHeaderLine, 1), codexPaneHeaderOurs},
		{"foreign assignment", strings.Replace(codexCfgSample,
			`url = "http://0.0.0.0:8787/mcp"`,
			`url = "http://0.0.0.0:8787/mcp"`+"\n"+`env_http_headers = { "X-Other" = "OTHER_ENV" }`, 1), codexPaneHeaderForeign},
		{"foreign sub-table", codexCfgSample + "\n[mcp_servers.soluterminal.env_http_headers]\nX_Other = \"OTHER_ENV\"\n", codexPaneHeaderForeign},
		{"other server ignored", `[mcp_servers.other]
url = "https://other.example/mcp"
env_http_headers = { "X-Soluterminal-Pane" = "SOLU_PANE_ID" }

[mcp_servers.soluterminal]
url = "http://127.0.0.1:8787/mcp"
`, codexPaneHeaderMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexConfigPaneHeaderState(c.body); got != c.want {
				t.Fatalf("state = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCodexConfigInsertPaneHeader(t *testing.T) {
	got, ok := codexConfigInsertPaneHeader(codexCfgSample)
	if !ok {
		t.Fatal("insert reported no soluterminal url")
	}
	wantPair := `url = "http://0.0.0.0:8787/mcp"` + "\n" + codexMcpPaneHeaderLine
	if !strings.Contains(got, wantPair) {
		t.Fatalf("header was not inserted immediately after url:\n%s", got)
	}
	if !strings.Contains(got, `[mcp_servers.supabase.env]
SUPABASE_ACCESS_TOKEN = "sbp_secret"
url = "https://red-herring.example"`) {
		t.Fatal("supabase env sub-table was damaged")
	}
	if !strings.Contains(got, `[mcp_servers.docs]
url = "https://developers.example.com/mcp"`) {
		t.Fatal("docs entry was damaged")
	}
	if got != strings.Replace(codexCfgSample,
		`url = "http://0.0.0.0:8787/mcp"`, wantPair, 1) {
		t.Fatal("bytes outside the insertion were changed")
	}
	if unchanged, ok := codexConfigInsertPaneHeader("[mcp_servers.other]\nurl = \"x\"\n"); ok || unchanged != "[mcp_servers.other]\nurl = \"x\"\n" {
		t.Fatal("insert changed a config without soluterminal")
	}
}

func TestCodexServerTableName(t *testing.T) {
	cases := []struct {
		header string
		name   string
		ok     bool
	}{
		{"mcp_servers.soluterminal", "soluterminal", true},
		{`mcp_servers."solu-terminal"`, "solu-terminal", true},
		{"mcp_servers.soluterminal.env", "", false},
		{"mcp_servers.soluterminal.tools.apply", "", false},
		{"other_table", "", false},
		{"mcp_servers.", "", false},
	}
	for _, c := range cases {
		name, ok := codexServerTableName(c.header)
		if name != c.name || ok != c.ok {
			t.Errorf("codexServerTableName(%q) = %q,%v want %q,%v", c.header, name, ok, c.name, c.ok)
		}
	}
}

func TestRegisterCodexMcpDirect(t *testing.T) {
	useMCPRegistration(t, "", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(codexCfgSample), 0644); err != nil {
		t.Fatal(err)
	}
	registerCodexMcpDirect(path, codexCfgSample, "http://127.0.0.1:8787/mcp", "fix", "http://0.0.0.0:8787/mcp")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if codexConfigMcpURL(string(after), "soluterminal") != "http://127.0.0.1:8787/mcp" {
		t.Errorf("direct fix did not heal:\n%s", after)
	}

	// add: appends a fresh section without touching the rest.
	base := "model = \"gpt-5\"\n"
	path2 := filepath.Join(dir, "config2.toml")
	if err := os.WriteFile(path2, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}
	registerCodexMcpDirect(path2, base, "http://127.0.0.1:8787/mcp", "add", "")
	after2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if codexConfigMcpURL(string(after2), "soluterminal") != "http://127.0.0.1:8787/mcp" {
		t.Errorf("direct add missing:\n%s", after2)
	}
	if !strings.HasPrefix(string(after2), base) {
		t.Errorf("existing content damaged:\n%s", after2)
	}
	if !strings.Contains(string(after2), codexMcpPaneHeaderLine) {
		t.Errorf("direct add missing pane header:\n%s", after2)
	}
	if got := codexConfigPaneHeaderState(string(after2)); got != codexPaneHeaderOurs {
		t.Errorf("direct add pane header state = %q", got)
	}
}

func TestEnsureCodexMcpPaneHeader(t *testing.T) {
	useMCPRegistration(t, "", "")
	desired := "http://127.0.0.1:8787/mcp"
	base := "[mcp_servers.soluterminal]\nurl = \"" + desired + "\"\n"
	tests := []struct {
		name      string
		body      string
		wantState string
		wantSame  bool
	}{
		{"missing is upgraded", base, codexPaneHeaderOurs, false},
		{"ours is unchanged", strings.Replace(base, "\n", "\n"+codexMcpPaneHeaderLine+"\n", 1), codexPaneHeaderOurs, true},
		{"foreign assignment is unchanged", base + `env_http_headers = { "X-Other" = "OTHER" }` + "\n", codexPaneHeaderForeign, true},
		{"foreign subtable is unchanged", base + "\n[mcp_servers.soluterminal.env_http_headers]\nX_Other = \"OTHER\"\n", codexPaneHeaderForeign, true},
		{"foreign url is unchanged", "[mcp_servers.soluterminal]\nurl = \"https://example.com/mcp\"\n", codexPaneHeaderMissing, true},
		{"other local port is unchanged", "[mcp_servers.soluterminal]\nurl = \"http://127.0.0.1:9999/mcp\"\n", codexPaneHeaderMissing, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			oldTime := time.Unix(123456789, 0)
			if err := os.Chtimes(path, oldTime, oldTime); err != nil {
				t.Fatal(err)
			}
			ensureCodexMcpPaneHeader(path, desired)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if state := codexConfigPaneHeaderState(string(got)); state != tt.wantState {
				t.Fatalf("state=%q want=%q; body:\n%s", state, tt.wantState, got)
			}
			if tt.wantSame && string(got) != tt.body {
				t.Fatalf("file changed:\n%s", got)
			}
			if tt.wantSame {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !info.ModTime().Equal(oldTime) {
					t.Fatal("no-op header ensure rewrote the file")
				}
			}
		})
	}
}

func TestEnsureCodexMcpRegistrationGlobalWritesAreGated(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		skip       string
		withConfig bool
	}{
		{"skip env", ":8787", "1", true},
		{"side port", ":18890", "", true},
		{"codex absent", ":8787", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", t.TempDir())
			t.Setenv("SOLU_SKIP_MCP_REGISTER", tt.skip)
			configPath := filepath.Join(home, ".codex", "config.toml")
			if tt.withConfig {
				if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configPath, []byte(codexCfgSample), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ensureCodexMcpRegistration(tt.listenAddr)
			if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); !os.IsNotExist(err) {
				t.Fatalf("AGENTS.md was touched: %v", err)
			}
			if tt.withConfig {
				got, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != codexCfgSample {
					t.Fatal("config.toml was touched")
				}
			} else if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("config.toml was created: %v", err)
			}
		})
	}
}
