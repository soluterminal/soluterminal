package main

import (
	"encoding/json"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Every value that must not be shown contains this marker, so one substring
// check covers a whole response.
const mcpHidden = "FAKEVALUE"

const mcpMark = "[redacted: token]"

func TestMcpDisplayURL(t *testing.T) {
	long := mcpHidden + strings.Repeat("x", 32)
	for _, tc := range []struct{ name, in, want string }{
		{"query value", "https://mcp.example.com/sse?api_key=" + mcpHidden + "1&mode=" + mcpHidden + "2",
			"https://mcp.example.com/sse?api_key=" + mcpMark + "&mode=" + mcpMark},
		{"name and password", "https://someone:" + mcpHidden + "@mcp.example.com:8443/v1/mcp",
			"https://mcp.example.com:8443/v1/mcp"},
		{"name only and fragment", "https://" + mcpHidden + "@mcp.example.com/mcp#" + mcpHidden,
			"https://mcp.example.com/mcp"},
		{"plain local address", "http://127.0.0.1:8787/mcp", "http://127.0.0.1:8787/mcp"},
		{"generated value in the path", "https://hooks.example.com/services/T00000000/B00000000/" + mcpHidden + "1a2B3c4D5e6F7g8",
			"https://hooks.example.com/services/T00000000/B00000000/" + mcpMark},
		{"mixed-case value in the path", "https://mcp.example.com/k/" + mcpHidden + "qWeRtYuIoPaS/mcp",
			"https://mcp.example.com/k/" + mcpMark + "/mcp"},
		{"id in the path", "https://mcp.example.com/u/123e4567-e89b-12d3-a456-426614174000/mcp",
			"https://mcp.example.com/u/" + mcpMark + "/mcp"},
		{"ordinary path words stay", "https://server.example.com/@upstash/context7-mcp/v1/oauth2callback/claude-3-5-sonnet-20241022/modelContextProtocol/abcdefghijklmnopqrst",
			"https://server.example.com/@upstash/context7-mcp/v1/oauth2callback/claude-3-5-sonnet-20241022/modelContextProtocol/abcdefghijklmnopqrst"},
		{"package version in the path", "https://example.com/org/tool@v1.2/mcp", "https://example.com/org/tool@v1.2/mcp"},
		{"long path segment", "https://mcp.example.com/s/" + long + "/mcp",
			"https://mcp.example.com/s/" + mcpMark + "/mcp"},
		{"bare query", "https://mcp.example.com/mcp?" + mcpHidden, "https://mcp.example.com/mcp?" + mcpMark},
		{"semicolon query", "https://mcp.example.com/mcp?a=" + mcpHidden + ";b=" + mcpHidden,
			"https://mcp.example.com/mcp?a=" + mcpMark + "&b=" + mcpMark},
		{"slash in password", "https://someone:" + mcpHidden + "/x@mcp.example.com/mcp?k=" + mcpHidden,
			"https://mcp.example.com/mcp"},
		{"bad escape in password", "https://someone:" + mcpHidden + "%zz@mcp.example.com/mcp",
			"https://mcp.example.com/mcp"},
		{"no scheme", "someone:" + mcpHidden + "@mcp.example.com:8443/mcp?k=" + mcpHidden,
			"mcp.example.com:8443/mcp"},
	} {
		got := mcpDisplayURL(tc.in)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, mcpHidden) {
			t.Errorf("%s: hidden value shown in %q", tc.name, got)
		}
	}
}

func TestMcpDisplayArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		args    []string
		wantCmd string
		want    []string
	}{
		{"flag then value", "npx", []string{"-y", "some-mcp", "--api-key", mcpHidden},
			"npx", []string{"-y", "some-mcp", "--api-key", mcpMark}},
		{"flag equals value", "npx", []string{"some-mcp", "--token=" + mcpHidden},
			"npx", []string{"some-mcp", "--token=" + mcpMark}},
		{"package names keep their version", "npx", []string{"-y", "@playwright/mcp@latest", "@scope/server-github@0.6.2", "some-mcp@^1.2.3-beta.1"},
			"npx", []string{"-y", "@playwright/mcp@latest", "@scope/server-github@0.6.2", "some-mcp@^1.2.3-beta.1"}},
		{"package names inline", "npx -y @playwright/mcp@latest --port 1", nil,
			"npx -y @playwright/mcp@latest --port 1", nil},
		{"not a package", "srv", []string{"someone@mcp.example.com", "Someone@1.2.3"},
			"srv", []string{"[redacted: email]", "[redacted: email]"}},
		{"addresses stay as the url field shows them", "npx", []string{"mcp-remote", "http://127.0.0.1:9000/mcp", "--host", "127.0.0.1", "--listen=127.0.0.1:9001", "[::1]:9002"},
			"npx", []string{"mcp-remote", "http://127.0.0.1:9000/mcp", "--host", "127.0.0.1", "--listen=127.0.0.1:9001", "[::1]:9002"}},
		{"flag without a value before a header", "srv", []string{"--no-auth", "--header", "X-Custom: " + mcpHidden, "next"},
			"srv", []string{"--no-auth", mcpMark, "X-Custom: " + mcpMark, "next"}},
		{"flag without a value before a header inline", "srv --no-auth -H X-Custom:" + mcpHidden + " next", nil,
			"srv --no-auth " + mcpMark + " X-Custom: " + mcpMark + " next", nil},
		{"stand-in bytes in the config", "srv", []string{"\x000\x00", "--token", "\x00" + mcpHidden},
			"srv", []string{"0", "--token", mcpMark}},
		{"one-letter sign-in flags", "db-mcp", []string{"-u", "admin", "-p", mcpHidden, "-k", "two " + mcpHidden + " words", "next"},
			"db-mcp", []string{"-u", "admin", "-p", mcpMark, "-k", mcpMark, "next"}},
		{"one-letter name and value", "srv", []string{"-u", "admin:" + mcpHidden, "next"},
			"srv", []string{"-u", "admin:" + mcpMark, "next"}},
		{"one-letter flags inline", `srv -k ` + mcpHidden + ` -p "two ` + mcpHidden + ` words" -u admin:` + mcpHidden + ` next`, nil,
			`srv -k ` + mcpMark + ` -p "` + mcpMark + `" -u admin:` + mcpMark + ` next`, nil},
		{"one-letter flags with plain values", "srv", []string{"-p", "8080", "-k", "./key.pem", "-p", "127.0.0.1:9000", "-u", "1000:1000", "-p", "tsconfig.json", "-k", "-v", "-u", "server.py"},
			"srv", []string{"-p", "8080", "-k", "./key.pem", "-p", "127.0.0.1:9000", "-u", "1000:1000", "-p", "tsconfig.json", "-k", "-v", "-u", "server.py"}},
		{"package runner keeps -p", "npx", []string{"-y", "-p", "typescript", "-p", "@scope/tool@1.2.3", "tsc"},
			"npx", []string{"-y", "-p", "typescript", "-p", "@scope/tool@1.2.3", "tsc"}},
		{"container runner keeps -p", "/usr/bin/docker", []string{"run", "-p", "8080:80", "-u", "1000:1000", "ghcr.io/example/server"},
			"/usr/bin/docker", []string{"run", "-p", "8080:80", "-u", "1000:1000", "ghcr.io/example/server"}},
		{"one-letter flags with the value attached", "srv", []string{"-p=" + mcpHidden, "-k=two " + mcpHidden + " words", "-u=admin:" + mcpHidden, "-p:" + mcpHidden, "-p=8080", "-k=./key.pem", "next"},
			"srv", []string{"-p=" + mcpMark, "-k=" + mcpMark, "-u=admin:" + mcpMark, "-p:" + mcpMark, "-p=8080", "-k=./key.pem", "next"}},
		{"one-letter flags with the value attached inline", `srv -p=` + mcpHidden + ` -k="two ` + mcpHidden + ` words" -u=admin:` + mcpHidden + ` -p=8080 next`, nil,
			`srv -p=` + mcpMark + ` -k="` + mcpMark + `" -u=admin:` + mcpMark + ` -p=8080 next`, nil},
		{"runner's own -p with the value attached", "npx", []string{"-p=typescript", "tsc", "-p=" + mcpHidden},
			"npx", []string{"-p=typescript", "tsc", "-p=" + mcpMark}},
		{"runner as a shell line", "sh", []string{"-c", "npx -p typescript tsc"},
			"sh", []string{"-c", "npx -p typescript tsc"}},
		{"runner after /c", "cmd", []string{"/c", "npx", "-p", "typescript", "tsc"},
			"cmd", []string{"/c", "npx", "-p", "typescript", "tsc"}},
		{"-p of the program a runner starts", "npx", []string{"-y", "some-mcp", "-p", mcpHidden},
			"npx", []string{"-y", "some-mcp", "-p", mcpMark}},
		{"-p after the runner's own -p", "npx", []string{"-p", "typescript", "tsc", "-p", mcpHidden},
			"npx", []string{"-p", "typescript", "tsc", "-p", mcpMark}},
		{"-p of a script", "node server.js -p " + mcpHidden, nil,
			"node server.js -p " + mcpMark, nil},
		{"-p inside a container", "docker", []string{"run", "-p", "8080:80", "image", "db-mcp", "-p", mcpHidden},
			"docker", []string{"run", "-p", "8080:80", "image", "db-mcp", "-p", mcpMark}},
		{"runner name as a value", "srv", []string{"--engine", "docker", "-p", mcpHidden},
			"srv", []string{"--engine", "docker", "-p", mcpMark}},
		{"scheme word as its own piece", "srv", []string{"-H", "X-Custom:", "Bearer", mcpHidden, "next"},
			"srv", []string{"-H", "X-Custom:", mcpMark, mcpMark, "next"}},
		{"scheme word after a flag", "srv", []string{"--auth", "Bearer", mcpHidden, "next"},
			"srv", []string{"--auth", mcpMark, mcpMark, "next"}},
		{"one-letter flag before an address", "curl -k https://someone:" + mcpHidden + "@mcp.example.com/sse", nil,
			"curl -k https://mcp.example.com/sse", nil},
		{"one-letter flag after a flag without a value", "srv", []string{"--no-auth", "-p", mcpHidden, "next"},
			"srv", []string{"--no-auth", mcpMark, mcpMark, "next"}},
		{"header name and value as separate pieces", "srv", []string{"-H", "X-Custom:", mcpHidden, "next"},
			"srv", []string{"-H", "X-Custom:", mcpMark, "next"}},
		{"header value that ends in a colon", "srv", []string{"-H", "X-Custom:" + mcpHidden + ":", "next", "--header", "X-Custom:a:" + mcpHidden + ":", "next"},
			"srv", []string{"-H", "X-Custom: " + mcpMark, "next", "--header", "X-Custom: " + mcpMark, "next"}},
		{"header value that ends in a colon inline", "srv -H X-Custom:" + mcpHidden + ": next", nil,
			"srv -H X-Custom: " + mcpMark + " next", nil},
		{"quoted scheme word as its own piece", "srv", []string{"--auth", `"Bearer"`, mcpHidden, "next"},
			"srv", []string{"--auth", mcpMark, mcpMark, "next"}},
		{"header without quotes inline", "srv -H X-Custom: " + mcpHidden + " --port 1", nil,
			"srv -H X-Custom: " + mcpMark + " --port 1", nil},
		{"header with a scheme word inline", "srv -H X-Custom: Bearer " + mcpHidden + " --port 1", nil,
			"srv -H X-Custom: " + mcpMark + " " + mcpMark + " --port 1", nil},
		{"ordinary flags stay", "npx", []string{"-y", "some-mcp", "--port", "8080", "--mode=fast"},
			"npx", []string{"-y", "some-mcp", "--port", "8080", "--mode=fast"}},
		{"flag after flag", "srv", []string{"--token", "--secret", mcpHidden, "--port", "1"},
			"srv", []string{"--token", mcpMark, mcpMark, "--port", "1"}},
		{"value with spaces", "srv", []string{"--password", "two " + mcpHidden + " words", "next"},
			"srv", []string{"--password", mcpMark, "next"}},
		{"equals value with spaces", "srv", []string{"--password=two " + mcpHidden + " words", "next"},
			"srv", []string{"--password=" + mcpMark, "next"}},
		{"variable value with spaces", "docker", []string{"-e", "MY_SECRET=two " + mcpHidden + " words"},
			"docker", []string{"-e", "MY_SECRET=" + mcpMark}},
		{"header equals with spaces", "npx", []string{"--header=Authorization: Bearer " + mcpHidden},
			"npx", []string{"--header=Authorization: " + mcpMark}},
		{"flag colon value", "srv", []string{"--session:" + mcpHidden, "--auth:two " + mcpHidden + " words", "next"},
			"srv", []string{"--session:" + mcpMark, "--auth:" + mcpMark, "next"}},
		{"flag colon value inline", "srv --session:" + mcpHidden + " --port 1", nil,
			"srv --session:" + mcpMark + " --port 1", nil},
		{"quoted value inline", `srv --token "two ` + mcpHidden + ` words" --port 1`, nil,
			`srv --token "` + mcpMark + `" --port 1`, nil},
		{"quoted equals value inline", `srv --password="two ` + mcpHidden + ` words" --port 1`, nil,
			`srv --password="` + mcpMark + `" --port 1`, nil},
		{"quoted header inline", `srv --header "X-Custom: two ` + mcpHidden + ` words" --port 1`, nil,
			`srv --header "X-Custom: ` + mcpMark + `" --port 1`, nil},
		{"escaped quotes in a shell line", "sh", []string{"-c", `sh -c "srv \"--token\" \"two ` + mcpHidden + ` words\" --port 1"`},
			"sh", []string{"-c", `sh -c "srv \"--token\" \"` + mcpMark + `\" --port 1"`}},
		{"flag glued to an escaped quote", `sh -c "a \"--token ` + mcpHidden + `\""`, nil,
			`sh -c "a \"--token ` + mcpMark + `\""`, nil},
		{"escaped space in a shell line", "sh", []string{"-c", `srv --token two\ ` + mcpHidden + `\ words --port 1`},
			"sh", []string{"-c", `srv --token ` + mcpMark + ` --port 1`}},
		{"escaped space after equals", `srv --password=two\ ` + mcpHidden + ` --header X-Custom:a\ ` + mcpHidden + ` next`, nil,
			`srv --password=` + mcpMark + ` --header X-Custom: ` + mcpMark + ` next`, nil},
		{"escaped backslash ends the piece", `srv --token ` + mcpHidden + `\\ next`, nil,
			`srv --token ` + mcpMark + `\\ next`, nil},
		{"backslash before a flag", `srv.exe --dir C:\data\ --token ` + mcpHidden + ` next`, nil,
			`srv.exe --dir C:\data\ --token ` + mcpMark + ` next`, nil},
		{"escaped space inside quotes", `sh -c 'srv --token two\ ` + mcpHidden + `' next`, nil,
			`sh -c 'srv --token ` + mcpMark + `' next`, nil},
		{"quote left open", `srv --token "two ` + mcpHidden + ` words --port 1`, nil,
			`srv --token "` + mcpMark, nil},
		{"header", "npx", []string{"mcp-remote", "--header", "Authorization: Bearer " + mcpHidden},
			"npx", []string{"mcp-remote", "--header", "Authorization: " + mcpMark}},
		{"header equals", "npx", []string{"--header=X-Custom:" + mcpHidden},
			"npx", []string{"--header=X-Custom: " + mcpMark}},
		{"variable assignment", "docker", []string{"run", "-e", "SERVICE_PAT=" + mcpHidden, "-e", "MY_AUTH=" + mcpHidden, "image"},
			"docker", []string{"run", "-e", "SERVICE_PAT=" + mcpMark, "-e", "MY_AUTH=" + mcpMark, "image"}},
		{"variable name alone stays", "docker", []string{"run", "-e", "SERVICE_TOKEN", "ghcr.io/example/server"},
			"docker", []string{"run", "-e", "SERVICE_TOKEN", "ghcr.io/example/server"}},
		{"address argument", "npx", []string{"mcp-remote", "https://someone:" + mcpHidden + "@mcp.example.com/sse?k=" + mcpHidden},
			"npx", []string{"mcp-remote", "https://mcp.example.com/sse?k=" + mcpMark}},
		{"address in assignment", "srv", []string{"--db=postgres://someone:" + mcpHidden + "@db.example.com/app"},
			"srv", []string{"--db=postgres://db.example.com/app"}},
		{"address without scheme", "srv", []string{"mcp.example.com/sse?k=" + mcpHidden + "&v=" + mcpHidden},
			"srv", []string{"mcp.example.com/sse?k=" + mcpMark + "&v=" + mcpMark}},
		{"shell line argument", "sh", []string{"-c", "srv --auth-token " + mcpHidden + " --port 1"},
			"sh", []string{"-c", "srv --auth-token " + mcpMark + " --port 1"}},
		{"shell line in command", "sh -c 'srv --password " + mcpHidden + " --client-secret=" + mcpHidden + "' next", nil,
			"sh -c 'srv --password " + mcpMark + " --client-secret=" + mcpMark + "' next", nil},
		{"quoted assignment", `srv --exec="run --api-key ` + mcpHidden + `"`, nil,
			`srv --exec="run --api-key ` + mcpMark + `"`, nil},
		{"command ends with flag", "srv --token", []string{mcpHidden, "next"},
			"srv --token", []string{mcpMark, "next"}},
		{"apostrophe", "echo it's --token " + mcpHidden, nil,
			"echo it's --token " + mcpMark, nil},
		{"home folder", "/home/someperson/bin/srv", []string{"--dir", "/home/someperson/data"},
			"~/bin/srv", []string{"--dir", "~/data"}},
	} {
		in := []McpServerInfo{{Name: "s", Type: "stdio", Source: "user", Command: tc.command, Args: tc.args}}
		got := mcpServersForDisplay(in)[0]
		if got.Command != tc.wantCmd || !reflect.DeepEqual(got.Args, tc.want) {
			t.Errorf("%s: got %q %q, want %q %q", tc.name, got.Command, got.Args, tc.wantCmd, tc.want)
		}
		if strings.Contains(got.Command+" "+strings.Join(got.Args, " "), mcpHidden) {
			t.Errorf("%s: hidden value shown in %q %q", tc.name, got.Command, got.Args)
		}
	}
}

// Random command lines built from value-hiding flags in every supported
// form, mixed with ordinary words and quoting. Whatever the mix, no hidden
// value may come through, as a command string or as separate arguments.
func TestMcpDisplayArgsRandomLines(t *testing.T) {
	rng := rand.New(rand.NewSource(1243))
	plain := []string{"srv", "-y", "--port", "8080", "run", "it's", `"quoted words"`, "--mode=fast", "sh", "-c", "a=b", "x?y", "/c"}
	flags := []string{"--token", "--api-key", "-password", "--client-secret", "--auth", "--session-id", "--my_pat"}
	open := []string{``, `"`, `'`, `\"`, `\'`}
	for n := 0; n < 3000; n++ {
		var pieces []string
		for i, count := 0, 1+rng.Intn(8); i < count; i++ {
			if rng.Intn(2) == 0 {
				pieces = append(pieces, plain[rng.Intn(len(plain))])
				continue
			}
			q := open[rng.Intn(len(open))]
			value := mcpHidden
			if q != "" && rng.Intn(2) == 0 {
				value = "two " + mcpHidden + " words " + mcpHidden
			} else if q == "" && rng.Intn(2) == 0 {
				value = `two\ ` + mcpHidden + `\ words\ ` + mcpHidden
			}
			flag := flags[rng.Intn(len(flags))]
			switch rng.Intn(11) {
			case 0:
				pieces = append(pieces, flag, q+value+q)
			case 1:
				pieces = append(pieces, flag+"="+q+value+q)
			case 2:
				pieces = append(pieces, flag+":"+q+value+q)
			case 4:
				pieces = append(pieces, []string{"-p", "-k"}[rng.Intn(2)], q+value+q)
			case 5:
				pieces = append(pieces, "-u", q+"admin:"+value+q)
			case 6:
				pieces = append(pieces, "-H", "X-Custom:", q+value+q)
			case 10:
				pieces = append(pieces, "-H", q+"X-Custom:"+value+":"+q)
			case 7:
				pieces = append(pieces, flag, "Bearer", q+value+q)
			case 9:
				pieces = append(pieces, []string{"-p", "-k", "-u"}[rng.Intn(3)]+[]string{"=", ":"}[rng.Intn(2)]+q+"admin:"+value+q)
			case 8:
				// A runner's own -p value is shown, so it holds nothing hidden;
				// the -p of the program it starts does.
				pieces = append(pieces, "npx", "-p", "typescript", "tsc", "-p", q+value+q)
			case 3:
				pieces = append(pieces, "SOME_"+strings.ToUpper(strings.TrimLeft(flag, "-"))+"="+q+value+q)
			}
		}
		line := strings.Join(pieces, " ")
		asCommand := mcpServersForDisplay([]McpServerInfo{{Command: line}})[0]
		if strings.Contains(asCommand.Command, mcpHidden) {
			t.Fatalf("command %q\n shown as %q", line, asCommand.Command)
		}
		asArgs := mcpServersForDisplay([]McpServerInfo{{Command: "srv", Args: pieces}})[0]
		if strings.Contains(strings.Join(asArgs.Args, " "), mcpHidden) {
			t.Fatalf("args %q\n shown as %q", pieces, asArgs.Args)
		}
		wrapped := mcpServersForDisplay([]McpServerInfo{{Command: "sh", Args: []string{"-c", line}}})[0]
		if strings.Contains(strings.Join(wrapped.Args, " "), mcpHidden) {
			t.Fatalf("shell line %q\n shown as %q", line, wrapped.Args)
		}
	}
}

func TestMcpServersForDisplayLeavesInputAlone(t *testing.T) {
	in := []McpServerInfo{{Name: "s", Type: "http", Source: "user", URL: "http://someone:" + mcpHidden + "@127.0.0.1:1/mcp",
		Command: "srv", Args: []string{"--token", mcpHidden}, Status: "dead"}}
	out := mcpServersForDisplay(in)
	if in[0].URL != "http://someone:"+mcpHidden+"@127.0.0.1:1/mcp" || in[0].Args[1] != mcpHidden {
		t.Fatalf("input changed: %+v", in[0])
	}
	if out[0].Name != "s" || out[0].Type != "http" || out[0].Source != "user" || out[0].Status != "dead" {
		t.Fatalf("other fields changed: %+v", out[0])
	}
}

// The route reads real config shapes from a scratch home and must answer
// with the short form for every source, including Codex's.
func TestListMcpServersRouteReturnsShortForm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the route takes a POSIX cwd")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	// Port 1 on loopback refuses at once, so the liveness probe stays local.
	write(filepath.Join(home, ".claude.json"), map[string]any{
		"mcpServers": map[string]any{
			"a-query": server("type", "http", "url", "http://127.0.0.1:1/mcp?api_key="+mcpHidden),
			"b-login": server("type", "sse", "url", "http://someone:"+mcpHidden+"@127.0.0.1:1/sse"),
		},
		"projects": map[string]any{cwd: map[string]any{"mcpServers": map[string]any{
			"c-flag": server("command", "npx", "args", []any{"-y", "some-mcp", "--api-key", mcpHidden}),
		}}},
	})
	write(filepath.Join(cwd, ".mcp.json"), map[string]any{"mcpServers": map[string]any{
		"d-equals": server("command", "srv", "args", []any{"--token=" + mcpHidden}),
	}})
	codex := "[mcp_servers.e-codex]\nurl = \"http://127.0.0.1:1/mcp?key=" + mcpHidden + "\"\n" +
		"[mcp_servers.f-codex]\ncommand = \"srv --secret " + mcpHidden + "\"\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	(&apiServer{}).listMcpServers(rec, httptest.NewRequest("GET", "/api/mcp/servers?cwd="+url.QueryEscape(cwd), nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), mcpHidden) {
		t.Fatalf("hidden value in response: %s", rec.Body.String())
	}
	var got []McpServerInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []McpServerInfo{
		{Name: "a-query", Type: "http", Source: "user", URL: "http://127.0.0.1:1/mcp?api_key=" + mcpMark, Status: "dead"},
		{Name: "b-login", Type: "sse", Source: "user", URL: "http://127.0.0.1:1/sse", Status: "dead"},
		{Name: "c-flag", Type: "stdio", Source: "project", Command: "npx", Args: []string{"-y", "some-mcp", "--api-key", mcpMark}},
		{Name: "d-equals", Type: "stdio", Source: ".mcp.json", Command: "srv", Args: []string{"--token=" + mcpMark}},
		{Name: "e-codex", Type: "http", Source: "codex", URL: "http://127.0.0.1:1/mcp?key=" + mcpMark, Status: "dead"},
		{Name: "f-codex", Type: "stdio", Source: "codex", Command: "srv --secret " + mcpMark},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
