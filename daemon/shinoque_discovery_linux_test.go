package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func shinoqueDiscoveryFixture(t *testing.T) (string, string) {
	t.Helper()
	for _, key := range []string{"SHINOQUE_HOME", "SHINOQUE_RELAY_PORT"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := shinoqueDiscoveryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("installed-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	return path, home
}

func writeShinoqueDiscovery(t *testing.T, path, home string, port int) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"version": 1, "port": port, "home": home, "relayAuth": 2, "privateDeviceProof": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestShinoqueDiscoveryRejects(t *testing.T) {
	for _, tc := range []struct{ name, record, reason string }{
		{"invalid JSON", `{secret-token`, "invalid discovery JSON"},
		{"trailing JSON", `{"version":1,"port":1234,"home":%q} {}`, "invalid discovery JSON"},
		{"fractional port", `{"version":1,"port":1234.5,"home":%q}`, "invalid discovery JSON"},
		{"string port", `{"version":1,"port":"1234","home":%q}`, "invalid discovery JSON"},
		{"fractional version", `{"version":1.5,"port":1234,"home":%q}`, "invalid discovery JSON"},
		{"old version", `{"version":0,"port":1234,"home":%q}`, "unsupported discovery version"},
		{"future version", `{"version":2,"port":1234,"home":%q}`, "unsupported discovery version"},
		{"missing version", `{"port":1234,"home":%q}`, "unsupported discovery version"},
		{"missing port", `{"version":1,"home":%q}`, "discovery port"},
		{"zero port", `{"version":1,"port":0,"home":%q}`, "discovery port"},
		{"negative port", `{"version":1,"port":-1,"home":%q}`, "discovery port"},
		{"large port", `{"version":1,"port":65536,"home":%q}`, "discovery port"},
		{"missing home", `{"version":1,"port":1234}`, "home must be absolute"},
		{"relative home", `{"version":1,"port":1234,"home":"relative"}`, "home must be absolute"},
		{"absent home", `{"version":1,"port":1234,"home":%q}`, "home is unavailable"},
		{"file home", `{"version":1,"port":1234,"home":%q}`, "home is not a directory"},
		{"oversize", strings.Repeat("x", shinoqueDiscoveryLimit+1), "discovery exceeds size limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, home := shinoqueDiscoveryFixture(t)
			if tc.name == "absent home" {
				home = filepath.Join(home, "missing")
			}
			if tc.name == "file home" {
				home = filepath.Join(home, "relay-token")
			}
			record := tc.record
			if strings.Contains(record, "%q") {
				record = fmt.Sprintf(record, home)
			}
			if err := os.WriteFile(path, []byte(record), 0600); err != nil {
				t.Fatal(err)
			}
			cache := &shinoqueTokenCache{}
			if _, ok := cache.discovery.resolve(); ok {
				t.Fatal("invalid discovery accepted")
			}
			if !strings.Contains(cache.discovery.rejection, tc.reason) {
				t.Fatalf("reason = %q, want %q", cache.discovery.rejection, tc.reason)
			}
			// A development token must not bypass invalid installed discovery.
			devHome := filepath.Join(os.Getenv("HOME"), ".shinoque")
			if err := os.MkdirAll(devHome, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(devHome, "relay-token"), []byte("dev-token"), 0600); err != nil {
				t.Fatal(err)
			}
			if cache.read() != "" {
				t.Fatal("rejection fell back to development token")
			}
			req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
			req.Header.Set("X-Forwarded-Prefix", "/m/test")
			w := httptest.NewRecorder()
			newShinoqueRelayHandler(http.NotFoundHandler(), cache, nil).ServeHTTP(w, req)
			if w.Code != 503 || strings.TrimSpace(w.Body.String()) != shinoqueUnavailable {
				t.Fatalf("response = %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestShinoqueDiscoveryFileSecurity(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling symlink", "directory", "fifo", "mode 0644", "mode 0400", "mode 0660", "mode 0601", "owner", "home owner"} {
		t.Run(kind, func(t *testing.T) {
			path, home := shinoqueDiscoveryFixture(t)
			writeShinoqueDiscovery(t, path, home, 1234)
			reason := ""
			switch kind {
			case "symlink", "dangling symlink", "directory", "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "symlink", "dangling symlink":
					target := filepath.Join(t.TempDir(), "real.json")
					if kind == "symlink" {
						writeShinoqueDiscovery(t, target, home, 1234)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0600); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
				reason = "not a regular file"
			case "owner":
				if os.Getuid() == 0 {
					if err := os.Chown(path, 1, -1); err != nil {
						t.Fatal(err)
					}
				} else {
					// Exercise the real ownership check against a root-owned regular file.
					info, err := os.Lstat("/etc/passwd")
					if err != nil {
						t.Fatal(err)
					}
					if _, reason := shinoqueDiscoveryMetadata(info); reason != "discovery is not owned by this uid" {
						t.Fatalf("reason = %q", reason)
					}
					return
				}
				reason = "discovery is not owned by this uid"
			case "home owner":
				if os.Getuid() == 0 {
					if err := os.Chown(home, 1, -1); err != nil {
						t.Fatal(err)
					}
				} else {
					writeShinoqueDiscovery(t, path, "/", 1234)
				}
				reason = "home is not owned by this uid"
			default:
				mode, err := strconv.ParseUint(strings.TrimPrefix(kind, "mode "), 8, 32)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, os.FileMode(mode)); err != nil {
					t.Fatal(err)
				}
				reason = "discovery mode must be 0600"
			}
			cache := &shinoqueDiscoveryCache{}
			if _, ok := cache.resolve(); ok || !strings.Contains(cache.rejection, reason) {
				t.Fatalf("accepted or wrong reason: %q", cache.rejection)
			}
		})
	}
}

func TestShinoqueDiscoveryDefaultsAndEnv(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	cache := &shinoqueDiscoveryCache{}
	check := func(port, home, source string) {
		t.Helper()
		got, ok := cache.resolve()
		if !ok || got.port != port || got.home != home || cache.source != source {
			t.Fatalf("config = %+v ok=%t source=%s", got, ok, cache.source)
		}
	}
	devHome := filepath.Join(os.Getenv("HOME"), ".shinoque")
	check("4760", devHome, "default")
	writeShinoqueDiscovery(t, path, home, 65535)
	check("65535", home, "file")
	t.Setenv("SHINOQUE_RELAY_PORT", "5555")
	check("5555", devHome, "env")
	t.Setenv("SHINOQUE_HOME", home)
	check("5555", home, "env")
	// Env overrides also bypass rejected discovery.
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	check("5555", home, "env")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("SHINOQUE_RELAY_PORT"); err != nil {
		t.Fatal(err)
	}
	check("4760", home, "env")
	// Explicitly empty settings preserve the old development defaults.
	t.Setenv("SHINOQUE_HOME", "")
	check("4760", devHome, "env")
	if err := os.Unsetenv("SHINOQUE_HOME"); err != nil {
		t.Fatal(err)
	}
	check("65535", home, "file")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check("4760", devHome, "default")
	t.Setenv("XDG_STATE_HOME", "")
	path = filepath.Join(os.Getenv("HOME"), ".local", "state", "shinoque", "relay.json")
	if shinoqueDiscoveryPath() != path {
		t.Fatal("wrong default discovery path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeShinoqueDiscovery(t, path, home, 1)
	check("1", home, "file")
}

func TestShinoqueDiscoveryCacheInvalidation(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	writeShinoqueDiscovery(t, path, home, 1234)
	cache := &shinoqueTokenCache{}
	if cache.read() != "installed-secret" {
		t.Fatal("installed token unavailable")
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stamp := first.ModTime().Add(time.Second)
	check := func(port string) {
		t.Helper()
		got, ok := cache.discovery.resolve()
		if !ok || got.port != port {
			t.Fatalf("cache invalidation missed: %+v ok=%t", got, ok)
		}
	}
	// Same size, new mtime.
	writeShinoqueDiscovery(t, path, home, 2345)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	check("2345")
	// New size, same mtime.
	writeShinoqueDiscovery(t, path, home, 34567)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	check("34567")
	// Atomic replacement, same size and mtime, new inode.
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	writeShinoqueDiscovery(t, replacement, home, 45678)
	if err := os.Chtimes(replacement, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	check("45678")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if cache.read() != "" {
		t.Fatal("cached discovery bypassed mode check")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if cache.read() != "installed-secret" {
		t.Fatal("discovery did not recover")
	}
	if err := os.Rename(home, home+"-moved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(home+"-moved", home) })
	if cache.read() != "" {
		t.Fatal("cached discovery bypassed home check")
	}
}

func TestShinoqueDiscoveryContentChangeWithSameMetadata(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	writeShinoqueDiscovery(t, path, home, 1234)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	cache := &shinoqueDiscoveryCache{}
	write := func(t *testing.T, content []byte) {
		t.Helper()
		if len(content) != len(original) {
			t.Fatal("replacement size changed")
		}
		// Rewriting the same inode and restoring mtime deterministically models
		// the metadata collision of an atomic replacement that reuses an inode.
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		after, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
			t.Fatal("replacement must retain inode, size and mtime")
		}
	}
	for _, tc := range []struct {
		name, old, replacement string
		auth, proof            int
		valid                  bool
	}{
		{"private capability", `"privateDeviceProof":1`, `"privateDeviceProof":0`, 2, 0, true},
		{"relay capability", `"relayAuth":2`, `"relayAuth":3`, 3, 1, true},
		{"port", `"port":1234`, `"port":2345`, 2, 1, true},
		{"invalid content", `"privateDeviceProof":1`, `"privateDeviceProof":x`, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, original)
			if got, ok := cache.resolve(); !ok || got.relayAuth != 2 || got.privateDeviceProof != 1 || got.port != "1234" {
				t.Fatalf("initial config = %+v ok=%t", got, ok)
			}
			changed := bytes.Replace(original, []byte(tc.old), []byte(tc.replacement), 1)
			if bytes.Equal(changed, original) {
				t.Fatal("replacement did not change discovery contents")
			}
			write(t, changed)
			got, ok := cache.resolve()
			if ok != tc.valid || got.relayAuth != tc.auth || got.privateDeviceProof != tc.proof {
				t.Fatalf("content change missed: config=%+v ok=%t", got, ok)
			}
			if tc.name == "port" && got.port != "2345" {
				t.Fatalf("port=%s want=2345", got.port)
			}
		})
	}
}

func TestShinoqueDiscoveryRelayPortMove(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	tokens := &shinoqueTokenCache{}
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)
	for _, response := range []string{"first", "second"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Host, "127.0.0.1:") || r.Header.Get("X-Shinoque-Relay") != "installed-secret" {
				t.Errorf("incorrect relay request")
			}
			_, _ = fmt.Fprint(w, response)
		}))
		defer server.Close()
		_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.Atoi(port)
		writeShinoqueDiscovery(t, path, home, n)
		stamp := time.Now().Add(time.Duration(n) * time.Second)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
		req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 || w.Body.String() != response {
			t.Fatalf("response = %d %q", w.Code, w.Body.String())
		}
		a := &apiServer{shinoqueTokens: tokens}
		health := httptest.NewRecorder()
		a.health(health, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		var body map[string]any
		if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["shinoqueRelay"] != true {
			t.Fatal("installed relay not advertised")
		}
	}
}

func TestShinoqueDiscoveryRejectsSoludListener(t *testing.T) {
	for _, name := range []string{"solud", "solud.exe"} {
		t.Run(name, func(t *testing.T) {
			path, home := shinoqueDiscoveryFixture(t)
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			port := listener.Addr().(*net.TCPAddr).Port
			writeShinoqueDiscovery(t, path, home, port)
			tokens := &shinoqueTokenCache{}
			if _, ok := tokens.discovery.resolve(); !ok {
				t.Fatal("ordinary listener rejected")
			}
			// Inherit a real listening socket into a process named solud. The
			// discovery is already cached: listener ownership must be rechecked.
			sleep, err := exec.LookPath("sleep")
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(t.TempDir(), name)
			if err := os.Symlink(sleep, binary); err != nil {
				t.Fatal(err)
			}
			file, err := listener.File()
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			cmd := exec.Command(binary, "60")
			cmd.ExtraFiles = []*os.File{file}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			deadline := time.Now().Add(5 * time.Second)
			for readComm(cmd.Process.Pid) != name {
				if time.Now().After(deadline) {
					t.Fatal("listener process did not start")
				}
				time.Sleep(time.Millisecond)
			}
			if _, ok := tokens.discovery.resolve(); ok || tokens.discovery.rejection != "discovery port belongs to solud" {
				t.Fatalf("solud listener accepted: %q", tokens.discovery.rejection)
			}
			req := httptest.NewRequest(http.MethodPost, "/shinoque/api/settings", nil)
			req.Header.Set("X-Forwarded-Prefix", "/m/test")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			w := httptest.NewRecorder()
			newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil).ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("forged destination response = %d", w.Code)
			}
		})
	}
}

// Installed Shinoque publishes discovery only while cross-device access is on.
// Deletion must revoke the discovered endpoint on the very next request.
func TestShinoqueDiscoveryDeletedFileFallsBack(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("X-Shinoque-Relay") != "installed-secret" {
			t.Error("incorrect installed token")
		}
		_, _ = fmt.Fprint(w, "installed")
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	writeShinoqueDiscovery(t, path, home, n)
	tokens := &shinoqueTokenCache{}
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)
	request := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
		req.Header.Set("X-Forwarded-Prefix", "/m/test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	if w := request(); w.Code != 200 || w.Body.String() != "installed" || hits != 1 {
		t.Fatalf("valid discovery: status=%d body=%q hits=%d", w.Code, w.Body.String(), hits)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Development defaults have no advertised relay capabilities, so deleting
	// discovery requires an update before remote use can resume.
	if w := request(); w.Code != 503 || strings.TrimSpace(w.Body.String()) != shinoqueUpdateRequired || hits != 1 {
		t.Fatalf("deleted discovery: status=%d body=%q hits=%d", w.Code, w.Body.String(), hits)
	}
	if tokens.discovery.source != "default" || tokens.discovery.lastConfig.port != "4760" || tokens.discovery.lastConfig.home != filepath.Join(os.Getenv("HOME"), ".shinoque") {
		t.Fatalf("discovered config survived deletion: %+v", tokens.discovery.lastConfig)
	}
}

type shinoqueLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *shinoqueLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *shinoqueLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestShinoqueDiscoveryLogging(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	var logs shinoqueLogBuffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	cache := &shinoqueTokenCache{}
	cache.read()
	writeShinoqueDiscovery(t, path, home, 1234)
	cache.read()
	cache.read()
	t.Setenv("SHINOQUE_HOME", home)
	cache.read()
	if err := os.Unsetenv("SHINOQUE_HOME"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"secret":"never-log-this-token"`), 0600); err != nil {
		t.Fatal(err)
	}
	cache.read()
	got := logs.String()
	for _, want := range []string{"source=default", "source=file", "source=env", "discovery rejected: invalid discovery JSON"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Count(got, "source=file") != 2 {
		t.Errorf("unchanged source logged repeatedly: %s", got)
	}
	if strings.Contains(got, "installed-secret") || strings.Contains(got, "never-log-this-token") {
		t.Fatal("token leaked to logs")
	}
}
