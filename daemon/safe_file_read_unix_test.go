//go:build !windows

package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// within fails the test if fn hasn't returned in d — a read that blocks or
// streams forever must not hang the suite.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// specialFiles returns a set of non-regular paths: /dev/zero, a FIFO with no
// writer, a listening Unix socket, a directory, and a symlink to /dev/zero.
func specialFiles(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	out := map[string]string{"directory": dir}
	if _, err := os.Stat("/dev/zero"); err == nil {
		out["/dev/zero"] = "/dev/zero"
		link := filepath.Join(dir, "zero-link")
		if err := os.Symlink("/dev/zero", link); err != nil {
			t.Fatal(err)
		}
		out["symlink to /dev/zero"] = link
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	out["fifo"] = fifo
	// Unix socket paths are length-limited; keep it short.
	sockDir, err := os.MkdirTemp("", "sfr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	out["socket"] = sock
	return out
}

func TestReadUserFileRefusesSpecialFiles(t *testing.T) {
	for name, path := range specialFiles(t) {
		t.Run(name, func(t *testing.T) {
			within(t, 3*time.Second, "readUserFile("+path+")", func() {
				_, _, err := readUserFile(path, maxFileBytes)
				if err == nil {
					t.Errorf("readUserFile(%s) succeeded", path)
					return
				}
				if !errors.Is(err, errUserFileNotRegular) {
					t.Errorf("err = %v, want errUserFileNotRegular", err)
				}
			})
		})
	}
}

// The files API, MCP read_file, MCP preview_file and write-line all refuse
// the same special files, promptly and with a clear message.
func TestFileReadEntryPointsRefuseSpecialFiles(t *testing.T) {
	srv := &apiServer{aria: &Aria{}}
	for name, path := range specialFiles(t) {
		t.Run(name, func(t *testing.T) {
			within(t, 5*time.Second, "entry points on "+path, func() {
				rec := httptest.NewRecorder()
				srv.readFile(rec, httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(path), nil))
				if rec.Code == http.StatusOK || rec.Code >= 500 {
					t.Errorf("/api/file %s: %d %q", path, rec.Code, rec.Body.String())
				}
				if name != "directory" && !strings.Contains(rec.Body.String(), "only regular files") {
					t.Errorf("/api/file %s message %q lacks a clear reason", path, rec.Body.String())
				}

				if got := srv.aria.toolReadFile(path, 0, 0); !strings.HasPrefix(got, "error:") {
					t.Errorf("toolReadFile(%s) = %.80q", path, got)
				}
				text, isErr := mcpResultText(t, srv.mcpToolReadFile(map[string]any{"path": path}))
				if !isErr {
					t.Errorf("MCP read_file(%s) = %.80q", path, text)
				}
				text, isErr = mcpResultText(t, srv.mcpToolPreviewFile(map[string]any{"path": path}))
				if !isErr {
					t.Errorf("MCP preview_file(%s) = %.80q", path, text)
				}
				if _, _, status, err := writeFileLineAt(path, 0, "x", 0); err == nil || status == 200 {
					t.Errorf("writeFileLineAt(%s) status %d err %v", path, status, err)
				}
			})
		})
	}
}

func TestFileReadEntryPointsRefuseOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a\n", maxFileBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &apiServer{aria: &Aria{}}
	rec := httptest.NewRecorder()
	srv.readFile(rec, httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(path), nil))
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "file too large") {
		t.Fatalf("/api/file: %d %q", rec.Code, rec.Body.String())
	}
	if got := srv.aria.toolReadFile(path, 0, 0); !strings.Contains(got, "file too large") {
		t.Fatalf("toolReadFile = %.80q", got)
	}
	text, isErr := mcpResultText(t, srv.mcpToolPreviewFile(map[string]any{"path": path}))
	if !isErr || !strings.Contains(text, "file too large") {
		t.Fatalf("preview_file = %q", text)
	}
}

func TestFileReadEntryPointsReadNormalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &apiServer{aria: &Aria{}}
	rec := httptest.NewRecorder()
	srv.readFile(rec, httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(path), nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"alpha"`) {
		t.Fatalf("/api/file: %d %q", rec.Code, rec.Body.String())
	}
	if got := srv.aria.toolReadFile(path, 0, 0); !strings.Contains(got, `"beta"`) {
		t.Fatalf("toolReadFile = %q", got)
	}
}

// A pseudo-file that reports size 0 but has content is still capped while
// reading, not just by the up-front size check.
func TestReadUserFileCapsZeroSizePseudoFile(t *testing.T) {
	const p = "/proc/self/maps"
	info, err := os.Stat(p)
	if err != nil || info.Size() != 0 {
		t.Skip("no zero-size procfs file here")
	}
	if _, _, err := readUserFile(p, 16); !errors.Is(err, errUserFileTooLarge) {
		t.Fatalf("err = %v, want errUserFileTooLarge", err)
	}
}
