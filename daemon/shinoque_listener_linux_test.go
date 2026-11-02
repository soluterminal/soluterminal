package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

const shinoqueTCPHeader = "  sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"
const shinoqueTCP6Header = "  sl local_address remote_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"

func shinoqueTCPRow(addr string, port, uid int, state string) string {
	return fmt.Sprintf("  0: %s:%04X %s:0000 %s 00000000:00000000 00:00000000 00000000 %d 0 123 1 0000000000000000 100 0 0 10 0\n",
		addr, port, strings.Repeat("0", len(addr)), state, uid)
}

// Redirect only the two fixed proc paths to fixture files. Production always
// uses os.ReadFile on /proc/net/tcp and /proc/net/tcp6.
func shinoqueProcFixture(t *testing.T, tcp, tcp6 string) (string, func(string) ([]byte, error)) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"tcp": tcp, "tcp6": tcp6} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, func(path string) ([]byte, error) {
		switch path {
		case "/proc/net/tcp", "/proc/net/tcp6":
			return os.ReadFile(filepath.Join(dir, filepath.Base(path)))
		default:
			t.Fatalf("unexpected proc read: %s", path)
			return nil, os.ErrNotExist
		}
	}
}

func TestShinoqueLocalListenerMissingTCP6(t *testing.T) {
	const port, uid = 4760, 1000
	for _, tc := range []struct {
		name, tcp string
		want      bool
	}{
		{"IPv4 listener", shinoqueTCPRow("0100007F", port, uid, "0A"), true},
		{"no IPv4 listener", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, readFile := shinoqueProcFixture(t, shinoqueTCPHeader+tc.tcp, shinoqueTCP6Header)
			if err := os.Remove(filepath.Join(dir, "tcp6")); err != nil {
				t.Fatal(err)
			}
			if got := shinoqueLocalListenerOwned(strconv.Itoa(port), uid, readFile); got != tc.want {
				t.Fatalf("allowed with missing tcp6 = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestShinoqueLocalListenerCoversProxyTarget(t *testing.T) {
	const port, uid = 4760, 1000
	for _, tc := range []struct {
		name, addr string
		ipv6, want bool
	}{
		{"IPv4 target", "0100007F", false, true},
		{"IPv4 wildcard", "00000000", false, true},
		{"other IPv4 loopback", "0200007F", false, false},
		{"IPv6 loopback", "00000000000000000000000001000000", true, false},
		{"IPv6 wildcard with unknown v6only", "00000000000000000000000000000000", true, false},
		{"IPv6 mapped target", "0000000000000000FFFF00000100007F", true, false},
		{"IPv6 mapped other loopback", "0000000000000000FFFF00000200007F", true, false},
		{"IPv6 mapped wildcard", "0000000000000000FFFF000000000000", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tcp, tcp6 := shinoqueTCPHeader, shinoqueTCP6Header
			row := shinoqueTCPRow(tc.addr, port, uid, "0A")
			if tc.ipv6 {
				tcp6 += row
			} else {
				tcp += row
			}
			_, readFile := shinoqueProcFixture(t, tcp, tcp6)
			if got := shinoqueLocalListenerOwned(strconv.Itoa(port), uid, readFile); got != tc.want {
				t.Fatalf("allowed = %t, want %t for 127.0.0.1:%d", got, tc.want, port)
			}
		})
	}
}

func TestShinoqueLocalListenerReadErrors(t *testing.T) {
	const port, uid = 4760, 1000
	for _, tc := range []struct {
		name, path string
		err        error
		want       bool
	}{
		{"missing tcp6", "/proc/net/tcp6", syscall.ENOENT, true},
		{"missing tcp4", "/proc/net/tcp", syscall.ENOENT, false},
		{"tcp6 permission denied", "/proc/net/tcp6", syscall.EACCES, false},
		{"tcp4 permission denied", "/proc/net/tcp", syscall.EACCES, false},
		{"tcp6 read failure", "/proc/net/tcp6", syscall.EIO, false},
		{"tcp4 read failure", "/proc/net/tcp", syscall.EIO, false},
		{"tcp6 invalid parent", "/proc/net/tcp6", syscall.ENOTDIR, false},
		{"tcp6 non-errno missing marker", "/proc/net/tcp6", os.ErrNotExist, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, readFile := shinoqueProcFixture(t,
				shinoqueTCPHeader+shinoqueTCPRow("0100007F", port, uid, "0A"),
				shinoqueTCP6Header+shinoqueTCPRow("0000000000000000FFFF00000100007F", port, uid, "0A"))
			reader := func(path string) ([]byte, error) {
				if path == tc.path {
					return nil, &os.PathError{Op: "open", Path: path, Err: tc.err}
				}
				return readFile(path)
			}
			if got := shinoqueLocalListenerOwned(strconv.Itoa(port), uid, reader); got != tc.want {
				t.Fatalf("allowed = %t, want %t", got, tc.want)
			}
			if !tc.want {
				if reason := shinoqueLocalListenerRejection(strconv.Itoa(port), uid, reader); !strings.Contains(reason, tc.path) {
					t.Fatalf("refusal lacks proc path: %q", reason)
				}
			}
		})
	}
}

func TestShinoqueLocalListenerRefusalDiagnostics(t *testing.T) {
	_, readFile := shinoqueProcFixture(t, shinoqueTCPHeader+"do-not-log-proc-contents\n", shinoqueTCP6Header)
	reason := shinoqueLocalListenerRejection("4760", 1000, readFile)
	if !strings.Contains(reason, "/proc/net/tcp line 2") || !strings.Contains(reason, "incomplete socket row") || strings.Contains(reason, "do-not-log-proc-contents") {
		t.Fatalf("unexpected parse refusal: %q", reason)
	}
	var logs shinoqueLogBuffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	if shinoqueLocalListenerAllowed("0") {
		t.Fatal("invalid port accepted")
	}
	if output := logs.String(); !strings.Contains(output, "listener check refused") || !strings.Contains(output, "invalid listener port") {
		t.Fatalf("refusal reason not logged: %q", output)
	}
}

func TestShinoqueLocalListenerOwned(t *testing.T) {
	const port, uid = 4760, 1000
	row := func(addr string, owner int) string { return shinoqueTCPRow(addr, port, owner, "0A") }
	own := row("0100007F", uid)
	other := row("0100007F", uid+1)
	v6 := row("00000000000000000000000001000000", uid)
	for _, tc := range []struct {
		name, tcp, tcp6 string
		want            bool
	}{
		{"own uid", own, "", true},
		{"IPv4 wildcard", row("00000000", uid), "", true},
		{"IPv4 loopback subnet", row("0200007F", uid), "", false},
		{"another uid", other, "", false},
		{"no listener", "", "", false},
		{"established socket", shinoqueTCPRow("0100007F", port, uid, "01"), "", false},
		{"different port", shinoqueTCPRow("0100007F", port+1, uid, "0A"), "", false},
		{"own external address", row("0100000A", uid), "", false},
		{"both uids own first", own + other, "", false},
		{"both uids other first", other + own, "", false},
		{"both uids across tables", own, row("00000000000000000000000000000000", uid+1), false},
		{"foreign IPv6 loopback", own, row("00000000000000000000000001000000", uid+1), false},
		{"foreign IPv6 mapped target", own, row("0000000000000000FFFF00000100007F", uid+1), false},
		{"foreign external listener", own + row("0100000A", uid+1), "", false},
		{"multiple own listeners", own + row("00000000", uid), v6, true},
		{"unrelated foreign socket", own + shinoqueTCPRow("0100007F", port, uid+1, "01"), "", true},
		{"IPv6 only", "", v6, false},
		{"IPv6 wildcard", "", row("00000000000000000000000000000000", uid), false},
		{"IPv6 mapped loopback", "", row("0000000000000000FFFF00000100007F", uid), false},
		{"malformed row after own", own + "broken\n", "", false},
		{"malformed IPv6 after own", own, "broken\n", false},
		{"malformed address", strings.Replace(own, "0100007F", "010000ZZ", 1), "", false},
		{"malformed port", strings.Replace(own, ":1298", ":ZZZZ", 1), "", false},
		{"malformed remote port", strings.Replace(own, ":0000", ":ZZZZ", 1), "", false},
		{"malformed state", strings.Replace(own, "0A ", "ZZ ", 1), "", false},
		{"malformed uid", strings.Replace(own, " 1000 ", " invalid ", 1), "", false},
		{"malformed queues", strings.Replace(own, "00000000:00000000", "invalid", 1), "", false},
		{"truncated row", "0: 0100007F:1298 00000000:0000 0A\n", "", false},
		{"scanner error", own + strings.Repeat("x", 65536), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, readFile := shinoqueProcFixture(t, shinoqueTCPHeader+tc.tcp, shinoqueTCP6Header+tc.tcp6)
			if got := shinoqueLocalListenerOwned(strconv.Itoa(port), uid, readFile); got != tc.want {
				t.Fatalf("allowed = %t, want %t", got, tc.want)
			}
		})
	}
	for _, name := range []string{"tcp", "tcp6"} {
		for _, bad := range []string{"missing", "empty", "header", "unreadable"} {
			t.Run(name+" "+bad, func(t *testing.T) {
				dir, readFile := shinoqueProcFixture(t, shinoqueTCPHeader+own, shinoqueTCP6Header+v6)
				path := filepath.Join(dir, name)
				var err error
				switch bad {
				case "missing", "unreadable":
					err = os.Remove(path)
					if err == nil && bad == "unreadable" {
						err = os.Mkdir(path, 0700)
					}
				case "empty":
					err = os.WriteFile(path, nil, 0600)
				case "header":
					err = os.WriteFile(path, []byte("invalid header\n"+own), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := name == "tcp6" && bad == "missing"
				if got := shinoqueLocalListenerOwned(strconv.Itoa(port), uid, readFile); got != want {
					t.Fatalf("allowed = %t, want %t", got, want)
				}
			})
		}
	}
}

func TestShinoqueRelayListenerRefusal(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
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
	uid := os.Getuid()
	own := shinoqueTCPRow("0100007F", n, uid, "0A")
	other := shinoqueTCPRow("0100007F", n, uid+1, "0A")
	dir, readFile := shinoqueProcFixture(t, shinoqueTCPHeader+own, shinoqueTCP6Header)
	handler := newShinoqueRelayHandlerWithListenerCheck(http.NotFoundHandler(), &shinoqueTokenCache{}, nil, func(port string) bool {
		return shinoqueLocalListenerOwned(port, os.Getuid(), readFile)
	})
	request := func(want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
		req.Header.Set("X-Forwarded-Prefix", "/m/test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("status = %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	request(http.StatusNoContent)
	if hits.Load() != 1 {
		t.Fatal("valid listener did not receive the request")
	}
	// Reuse the handler and discovery cache: ownership must be rechecked on
	// the next request, including when a previous upstream connection is idle.
	for _, content := range []string{other, "", own + other, own + "broken\n"} {
		if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(shinoqueTCPHeader+content), 0600); err != nil {
			t.Fatal(err)
		}
		request(http.StatusServiceUnavailable)
		if hits.Load() != 1 {
			t.Fatal("refused request reached the upstream")
		}
	}
	// A discovery port move cannot reuse a successful check for the old port.
	var movedHits atomic.Int32
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		movedHits.Add(1)
	}))
	defer moved.Close()
	_, movedPort, err := net.SplitHostPort(strings.TrimPrefix(moved.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	movedN, _ := strconv.Atoi(movedPort)
	if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(shinoqueTCPHeader+own), 0600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	writeShinoqueDiscovery(t, replacement, home, movedN)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	request(http.StatusServiceUnavailable)
	if movedHits.Load() != 0 || hits.Load() != 1 {
		t.Fatalf("refused port move reached an upstream: original=%d moved=%d", hits.Load(), movedHits.Load())
	}
}
