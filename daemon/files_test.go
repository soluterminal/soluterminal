package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestCanonicalizeFilePathExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := canonicalizeFilePath("~/notes/todo.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "notes", "todo.txt")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestCanonicalizeFilePathRequiresPath(t *testing.T) {
	if _, err := canonicalizeFilePath(""); err == nil {
		t.Fatal("canonicalizeFilePath succeeded for empty path")
	}
}

func TestLooksLikeEnvFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/repo/.env", true},
		{"/repo/.env.local", true},
		{"/repo/service.env", true},
		{"/repo/service.env.example", false},
		{"/repo/environment", false},
	}

	for _, tt := range tests {
		if got := looksLikeEnvFile(tt.path); got != tt.want {
			t.Fatalf("looksLikeEnvFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestSecretPathRefusalDetectsSecretishPaths(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/repo/.env", true},
		{"/repo/.env.local", true},
		{"/repo/service.env", true},
		{"/repo/env", true},
		{"/repo/deploy.key", true},
		{"/repo/cert.pem", true},
		{"/home/me/.ssh/id_ed25519", true},
		{"/repo/api_token.txt", true},
		{"/repo/passwords.md", true},
		{"/repo/src/config.go", false},
		{"/repo/README.md", false},
	}

	for _, tt := range tests {
		got := secretPathRefusal(tt.path) != ""
		if got != tt.want {
			t.Fatalf("secretPathRefusal(%q) refused=%v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestSecretPathRefusalGuardsDaemonStateAndCreds(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/home/me/.solu/token", true},      // daemon bearer token
		{"/home/me/.solu/claim.json", true}, // machine/relay token
		{"/custom-data/harness-accounts/glm.json", true},
		{"/custom-data/harness-accounts/deepseek.json", true},
		{"/custom-data/harness-launchers/launch.mjs", true},
		{"/custom-data/harness-state/deepseek/profiles/acp/cordis.patch.yml", true},
		{"/home/me/.zcode/cli/config.json", true},
		{"/home/me/.solu/env", true}, // service env
		{"/home/me/.ssh/id_rsa", true},
		{"/home/me/.aws/credentials", true},
		{"/home/me/.config/gcloud/foo.json", true},
		{"/home/me/.netrc", true},
		{"/home/me/.npmrc", true},
		{"/home/me/.pgpass", true},
		{"/home/me/.git-credentials", true},
		{"/home/me/.docker/config.json", true},
		{"/home/me/.kube/config", true},
		{"/home/me/.claude.json", true},              // Claude Code inline tokens
		{"/home/me/.claude/.credentials.json", true}, // live OAuth tokens
		{"/home/me/.claude/.credentials.work.json", true},
		{"/home/me/.claude/settings.json", true}, // under .claude → refused over relay
		{"/home/me/.codex/auth.json", true},      // Codex plaintext auth tokens
		{"/home/me/.codex/config.toml", true},    // under .codex → refused over relay
		{"/repo/src/main.go", false},
		{"/repo/docs/README.md", false},
	}
	for _, tt := range tests {
		got := secretPathRefusal(tt.path) != ""
		if got != tt.want {
			t.Fatalf("secretPathRefusal(%q) refused=%v, want %v", tt.path, got, tt.want)
		}
	}
}

// TestDaemonOwnedSecretPathRefusal pins the LOCAL-only refusal: the daemon's
// own credential dirs are refused anywhere on the path, and token/claim/env
// files are refused under the configured data dir, while ordinary workspace
// files (including a user .env) stay reachable.
func TestDaemonOwnedSecretPathRefusal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "harness-accounts", "glm.json"), true},
		{filepath.Join(root, "harness-accounts", ".secret-key"), true},
		{filepath.Join(root, "harness-launchers", "launch.mjs"), true},
		{filepath.Join(root, "harness-state", "deepseek", "profile.json"), true},
		{filepath.Join(root, "token"), true},
		{filepath.Join(root, "claim.json"), true},
		{filepath.Join(root, "env"), true},
		{"/custom-data/harness-accounts/glm.json", true}, // component check is path-wide
		{filepath.Join(root, "workspace", "main.go"), false},
		{filepath.Join(root, "workspace", ".env"), false}, // local operator may view their own .env
		{filepath.Join(root, "..", "outside", "token"), false},
		{filepath.Join(root, "notes", "claim.json.bak"), false},
	}
	for _, tt := range tests {
		got := daemonOwnedSecretPathRefusal(tt.path, root) != ""
		if got != tt.want {
			t.Fatalf("daemonOwnedSecretPathRefusal(%q, root) refused=%v, want %v", tt.path, got, tt.want)
		}
	}
}

// A second local OS account reaching the loopback port must not be able to
// read the daemon's credential files, even though it holds no token.
func TestLocalFileReadRefusesDaemonOwnedSecrets(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(rel, body string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	secrets := []string{
		mk("harness-accounts/glm.json", `{"personal":{"apiKey":"k"}}`),
		mk("harness-accounts/.secret-key", "deadbeef"),
		mk("harness-launchers/launch.mjs", "// launcher"),
		mk("harness-state/deepseek/profile.json", "{}"),
		mk("token", "abc"),
		mk("claim.json", "{}"),
		mk("env", "GROQ_API_KEY=abc"),
	}
	srv := &apiServer{dataDir: dir}
	for _, p := range secrets {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(p), nil)
		srv.readFile(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("local read %s code = %d, want 403 (body %q)", p, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "apiKey") || strings.Contains(rec.Body.String(), "deadbeef") {
			t.Errorf("local read %s leaked secret bytes: %q", p, rec.Body.String())
		}
	}
	// A user file (even a .env) outside those dirs stays readable locally.
	userEnv := mk("workspace/.env", "USER=1")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(userEnv), nil)
	srv.readFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local read of a workspace .env code = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// The write side of the same boundary: a local peer cannot overwrite the
// launcher scripts or key material.
func TestLocalFileWriteRefusesDaemonOwnedSecrets(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "harness-accounts", "glm.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &apiServer{dataDir: dir}
	body := `{"path":` + strconv.Quote(p) + `,"lineNo":0,"newText":"stolen","expectedMtime":0}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/file/line", strings.NewReader(body))
	srv.writeFileLine(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("local write to daemon credentials code = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if data, _ := os.ReadFile(p); string(data) != "old\n" {
		t.Fatalf("local write must not modify daemon credentials; got %q", data)
	}
}

// TestReadFileRefusesSecretsOnlyOverRelay locks the transport contract: the
// local operator can read their own .env (flagged isEnv so the UI masks it),
// but the same request over the cloud relay is refused with 403.
func TestReadFileRefusesSecretsOnlyOverRelay(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &apiServer{}

	// Local (no relay marker): .env reads fine and is flagged isEnv.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(envPath), nil)
	srv.readFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local read .env code = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"isEnv":true`) {
		t.Fatalf("local read .env should flag isEnv; body %q", rec.Body.String())
	}

	// Over the relay (cloud marker present): refused.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(envPath), nil)
	req.Header.Set("X-Forwarded-Prefix", "/m/mach_1")
	srv.readFile(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("relay read .env code = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
}

// TestWriteFileLineRefusesSecretsOverRelay closes the write-side hole: a
// relayed line-edit of a secret file is refused before touching disk.
func TestWriteFileLineRefusesSecretsOverRelay(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &apiServer{}
	body := `{"path":` + strconv.Quote(envPath) + `,"lineNo":0,"newText":"TOKEN=hijacked","expectedMtime":0}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/file/line", strings.NewReader(body))
	req.Header.Set("X-Forwarded-Prefix", "/m/mach_1")
	srv.writeFileLine(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("relay write .env code = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	data, _ := os.ReadFile(envPath)
	if string(data) != "TOKEN=abc\n" {
		t.Fatalf("relay write must not modify file; got %q", data)
	}
}

func TestWriteFileAtomicPreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(path, []byte("new\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("content = %q, want new", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
}

func TestWriteFileAtomicDoesNotFollowPredictableTempSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on many Windows hosts")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	victim := filepath.Join(dir, "victim.txt")
	predictableTmp := path + ".solu-tmp"
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("victim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, predictableTmp); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "victim\n" {
		t.Fatalf("victim content = %q, want unchanged", data)
	}
	if _, err := os.Lstat(predictableTmp); err != nil {
		t.Fatalf("predictable temp symlink was unexpectedly removed: %v", err)
	}
}
