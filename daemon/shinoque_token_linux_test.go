package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestShinoqueTokenFileSecurity(t *testing.T) {
	for _, warm := range []bool{false, true} {
		for _, kind := range []string{"symlink", "dangling symlink", "directory", "fifo", "mode 0644", "mode 0400", "mode 0660", "mode 0601", "setuid", "owner", "oversize"} {
			t.Run(strconv.FormatBool(warm)+"/"+kind, func(t *testing.T) {
				discovery, home := shinoqueDiscoveryFixture(t)
				writeShinoqueDiscovery(t, discovery, home, 1234)
				cache := &shinoqueTokenCache{}
				if warm && cache.read() != "installed-secret" {
					t.Fatal("valid token unavailable")
				}
				path := filepath.Join(home, "relay-token")
				switch kind {
				case "symlink", "dangling symlink", "directory", "fifo":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "symlink", "dangling symlink":
						target := filepath.Join(t.TempDir(), "token")
						if kind == "symlink" {
							if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
								t.Fatal(err)
							}
						}
						if err := os.Symlink(target, path); err != nil {
							t.Fatal(err)
						}
					case "directory":
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					case "fifo":
						if err := syscall.Mkfifo(path, 0600); err != nil {
							t.Fatal(err)
						}
					}
				case "owner":
					if os.Getuid() != 0 {
						info, err := os.Lstat("/etc/passwd")
						if err != nil {
							t.Fatal(err)
						}
						if shinoqueTokenMetadata(info) {
							t.Fatal("token owned by another uid accepted")
						}
						return
					}
					if err := os.Chown(path, 1, -1); err != nil {
						t.Fatal(err)
					}
				case "oversize":
					if err := os.WriteFile(path, []byte(strings.Repeat("x", 4097)), 0600); err != nil {
						t.Fatal(err)
					}
				case "setuid":
					if err := os.Chmod(path, 0600|os.ModeSetuid); err != nil {
						t.Fatal(err)
					}
				default:
					mode, err := strconv.ParseUint(strings.TrimPrefix(kind, "mode "), 8, 32)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, os.FileMode(mode)); err != nil {
						t.Fatal(err)
					}
				}
				if cache.read() != "" {
					t.Fatal("unsafe token accepted")
				}
				req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
				req.Header.Set("X-Forwarded-Prefix", "/m/test")
				w := httptest.NewRecorder()
				newShinoqueRelayHandler(http.NotFoundHandler(), cache, nil).ServeHTTP(w, req)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("unsafe token response = %d", w.Code)
				}
			})
		}
	}
}

func TestShinoqueTokenOpenDoesNotFollowSymlink(t *testing.T) {
	home := shinoqueTestToken(t, "secret")
	link := filepath.Join(t.TempDir(), "token")
	if err := os.Symlink(filepath.Join(home, "relay-token"), link); err != nil {
		t.Fatal(err)
	}
	if file, err := openShinoqueToken(link); err == nil {
		file.Close()
		t.Fatal("open followed a symlink (including one swapped in after lstat)")
	}
}

func TestShinoqueTokenAtomicReplacement(t *testing.T) {
	home := shinoqueTestToken(t, "first")
	cache := &shinoqueTokenCache{}
	if cache.read() != "first" {
		t.Fatal("valid token unavailable")
	}
	path := filepath.Join(home, "relay-token")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(home, "replacement")
	if err := os.WriteFile(replacement, []byte("other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if cache.read() != "other" {
		t.Fatal("same-size same-mtime replacement retained cached token")
	}
}
