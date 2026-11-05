package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// shinoqueCountedScan wraps the real /proc scan with a counter.
func shinoqueCountedScan(tokens *shinoqueTokenCache) *atomic.Int32 {
	var scans atomic.Int32
	tokens.discovery.scanSolud = func(sockets map[string]bool) (bool, error) {
		scans.Add(1)
		return shinoqueSoludHolds(sockets)
	}
	return &scans
}

// shinoqueFrozenClock pins the verdict TTL clock; advance moves it.
func shinoqueFrozenClock(tokens *shinoqueTokenCache) func(time.Duration) {
	var mu sync.Mutex
	clock := time.Now()
	tokens.discovery.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	return func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
	}
}

// shinoqueSoludHolder hands listener to a child process named solud and closes
// every copy held by the test, so only the "solud" holds the socket.
func shinoqueSoludHolder(t *testing.T, listener *net.TCPListener) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "solud")
	if err := os.Symlink(sleep, binary); err != nil {
		t.Fatal(err)
	}
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "60")
	cmd.ExtraFiles = []*os.File{file}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	_ = file.Close()
	_ = listener.Close()
	deadline := time.Now().Add(5 * time.Second)
	for readComm(cmd.Process.Pid) != "solud" {
		if time.Now().After(deadline) {
			t.Fatal("solud holder did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func shinoqueRelayGet(handler http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func shinoqueListen(t *testing.T, network, address string, reusePort bool) *net.TCPListener {
	t.Helper()
	config := net.ListenConfig{}
	if reusePort {
		config.Control = func(_, _ string, raw syscall.RawConn) error {
			var sockErr error
			if err := raw.Control(func(fd uintptr) {
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return sockErr
		}
	}
	listener, err := config.Listen(context.Background(), network, address)
	if err != nil {
		t.Fatal(err)
	}
	return listener.(*net.TCPListener)
}

// A cached verdict must not survive a port takeover: the target exits and a
// solud binds the same port within the TTL. The new socket has a new inode.
func TestShinoqueSoludVerdictPortTakeover(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	port := listener.Addr().(*net.TCPAddr).Port
	served := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = served.Serve(listener) }()
	writeShinoqueDiscovery(t, path, home, port)
	tokens := &shinoqueTokenCache{}
	scans := shinoqueCountedScan(tokens)
	shinoqueFrozenClock(tokens)
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)
	for range 2 {
		if w := shinoqueRelayGet(handler); w.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
	}
	if scans.Load() != 1 {
		t.Fatalf("scans = %d, want one cached verdict", scans.Load())
	}
	_ = served.Close()
	takeover := shinoqueListen(t, "tcp4", "127.0.0.1:"+strconv.Itoa(port), false)
	shinoqueSoludHolder(t, takeover)
	if _, ok := tokens.discovery.resolve(); ok || tokens.discovery.rejection != "discovery port belongs to solud" {
		t.Fatalf("takeover accepted: %q", tokens.discovery.rejection)
	}
	if w := shinoqueRelayGet(handler); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("takeover response = %d", w.Code)
	}
}

// A solud joining the port with SO_REUSEPORT adds a socket inode while the
// original listener stays: the changed set forces a rescan.
func TestShinoqueSoludVerdictReusePortJoin(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	original := shinoqueListen(t, "tcp4", "127.0.0.1:0", true)
	defer original.Close()
	port := original.Addr().(*net.TCPAddr).Port
	writeShinoqueDiscovery(t, path, home, port)
	tokens := &shinoqueTokenCache{}
	scans := shinoqueCountedScan(tokens)
	shinoqueFrozenClock(tokens)
	if _, ok := tokens.discovery.resolve(); !ok {
		t.Fatalf("original listener rejected: %q", tokens.discovery.rejection)
	}
	shinoqueSoludHolder(t, shinoqueListen(t, "tcp4", "127.0.0.1:"+strconv.Itoa(port), true))
	if _, ok := tokens.discovery.resolve(); ok || tokens.discovery.rejection != "discovery port belongs to solud" {
		t.Fatalf("joined solud accepted: %q", tokens.discovery.rejection)
	}
	if scans.Load() != 2 {
		t.Fatalf("scans = %d, want a rescan for the new socket", scans.Load())
	}
}

// Repointing discovery at a port solud holds is refused at once.
func TestShinoqueSoludVerdictDiscoveryRepointed(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	writeShinoqueDiscovery(t, path, home, upstream.Listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	shinoqueFrozenClock(tokens)
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)
	if w := shinoqueRelayGet(handler); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	solud := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	soludPort := solud.Addr().(*net.TCPAddr).Port
	shinoqueSoludHolder(t, solud)
	writeShinoqueDiscovery(t, path, home, soludPort)
	if w := shinoqueRelayGet(handler); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("repointed response = %d", w.Code)
	}
	if tokens.discovery.rejection != "discovery port belongs to solud" {
		t.Fatalf("rejection = %q", tokens.discovery.rejection)
	}
}

// The uid listener check runs on every request, cache hits included.
func TestShinoqueSoludVerdictHitStillChecksListenerUID(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count relayed requests only; a running solud may probe the port.
		if r.Header.Get("X-Shinoque-Relay") != "" {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	port := upstream.Listener.Addr().(*net.TCPAddr).Port
	writeShinoqueDiscovery(t, path, home, port)
	uid := os.Getuid()
	own := shinoqueTCPRow("0100007F", port, uid, "0A")
	other := shinoqueTCPRow("0100007F", port, uid+1, "0A")
	dir, readFile := shinoqueProcFixture(t, shinoqueTCPHeader+own, shinoqueTCP6Header)
	tokens := &shinoqueTokenCache{}
	scans := shinoqueCountedScan(tokens)
	shinoqueFrozenClock(tokens)
	handler := newShinoqueRelayHandlerWithListenerCheck(http.NotFoundHandler(), tokens, nil, func(port string) bool {
		return shinoqueLocalListenerOwned(port, uid, readFile)
	})
	if w := shinoqueRelayGet(handler); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(shinoqueTCPHeader+own+other), 0600); err != nil {
		t.Fatal(err)
	}
	if w := shinoqueRelayGet(handler); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("other-uid listener response = %d", w.Code)
	}
	if hits.Load() != 1 || scans.Load() != 1 {
		t.Fatalf("hits=%d scans=%d: refusal must come from the per-request check on a cached verdict", hits.Load(), scans.Load())
	}
}

func TestShinoqueSoludVerdictTTLAndInvalidation(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	writeShinoqueDiscovery(t, path, home, port)
	tokens := &shinoqueTokenCache{}
	scans := shinoqueCountedScan(tokens)
	advance := shinoqueFrozenClock(tokens)
	expect := func(want int32) {
		t.Helper()
		if tokens.read() != "installed-secret" {
			t.Fatalf("relay unavailable: %q", tokens.discovery.rejection)
		}
		if got := scans.Load(); got != want {
			t.Fatalf("scans = %d, want %d", got, want)
		}
	}
	expect(1)
	expect(1)
	advance(shinoqueSoludVerdictTTL - time.Nanosecond)
	expect(1)
	advance(time.Nanosecond)
	expect(2)

	// Discovery and home changes are seen before the verdict is consulted,
	// so the same request rescans. Same bytes, new mtime:
	stamp := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	expect(3)

	// New contents, same port.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), `"privateDeviceProof":1`, `"privateDeviceProof":0`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	expect(4)

	// Token file replaced: read after resolve, so the next request rescans.
	// The token itself never enters the key.
	replacement := filepath.Join(home, "relay-token.new")
	if err := os.WriteFile(replacement, []byte("installed-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(home, "relay-token")); err != nil {
		t.Fatal(err)
	}
	expect(4)
	expect(5)

	// Home directory replaced at the same path.
	if err := os.Rename(home, home+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("installed-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	expect(6)
	expect(7) // the new home also holds a new token file
	expect(7)
}

// A scan error refuses the request and is never cached.
func TestShinoqueSoludVerdictScanError(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	defer listener.Close()
	writeShinoqueDiscovery(t, path, home, listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	shinoqueFrozenClock(tokens)
	tokens.discovery.scanSolud = func(map[string]bool) (bool, error) { return false, errors.New("proc unavailable") }
	if _, ok := tokens.discovery.resolve(); ok || tokens.discovery.rejection != "cannot scan for solud listeners" {
		t.Fatalf("scan error accepted: %q", tokens.discovery.rejection)
	}
	handler := newShinoqueRelayHandlerWithListenerCheck(http.NotFoundHandler(), tokens, nil, func(string) bool { return true })
	if w := shinoqueRelayGet(handler); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("scan error response = %d", w.Code)
	}
	scans := shinoqueCountedScan(tokens)
	if _, ok := tokens.discovery.resolve(); !ok || scans.Load() != 1 {
		t.Fatalf("recovery ok=%t scans=%d", ok, scans.Load())
	}
}

// A failed or cut upstream connection invalidates the verdict, and the
// response stays ReverseProxy's default bodiless 502.
func TestShinoqueSoludVerdictUpstreamErrorInvalidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen func(t *testing.T) (*net.TCPListener, func())
	}{
		{"connection refused", func(t *testing.T) (*net.TCPListener, func()) {
			// Listening only on ::1 puts the port in the scanned tables while
			// the proxy's 127.0.0.1 dial is refused.
			listener, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.IPv6loopback})
			if err != nil {
				t.Skipf("IPv6 loopback unavailable: %v", err)
			}
			return listener, func() {}
		}},
		{"EOF before headers", func(t *testing.T) (*net.TCPListener, func()) {
			listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					buf := make([]byte, 4096)
					_, _ = conn.Read(buf)
					_ = conn.Close()
				}
			}()
			return listener, func() { <-done }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, home := shinoqueDiscoveryFixture(t)
			listener, wait := tc.listen(t)
			writeShinoqueDiscovery(t, path, home, listener.Addr().(*net.TCPAddr).Port)
			tokens := &shinoqueTokenCache{}
			scans := shinoqueCountedScan(tokens)
			shinoqueFrozenClock(tokens)
			handler := newShinoqueRelayHandlerWithListenerCheck(http.NotFoundHandler(), tokens, nil, func(string) bool { return true })
			for want := int32(1); want <= 2; want++ {
				w := shinoqueRelayGet(handler)
				if w.Code != http.StatusBadGateway || w.Body.Len() != 0 || len(w.Header()) != 0 {
					t.Fatalf("response = %d %v %q", w.Code, w.Header(), w.Body.String())
				}
				if scans.Load() != want {
					t.Fatalf("scans = %d, want %d: upstream error did not invalidate", scans.Load(), want)
				}
			}
			_ = listener.Close()
			wait()
		})
	}
}

// A cache hit keeps the request's table snapshot for the listener check; a
// scan discards it, so the check rereads the tables just before forwarding.
func TestShinoqueSoludVerdictTableSnapshot(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	defer listener.Close()
	writeShinoqueDiscovery(t, path, home, listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	scans := shinoqueCountedScan(tokens)
	shinoqueFrozenClock(tokens)
	for i, want := range []bool{false, true} {
		tables := &shinoqueProcTables{}
		if _, ok := tokens.discovery.resolveWith(tables); !ok {
			t.Fatalf("resolve %d rejected: %q", i, tokens.discovery.rejection)
		}
		if kept := tables.data != nil; kept != want {
			t.Fatalf("resolve %d: snapshot kept=%t want %t (scans=%d)", i, kept, want, scans.Load())
		}
	}
}

// The process scan runs without c.mu held.
func TestShinoqueSoludVerdictScanOutsideLock(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	defer listener.Close()
	writeShinoqueDiscovery(t, path, home, listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	scanned := false
	tokens.discovery.scanSolud = func(sockets map[string]bool) (bool, error) {
		scanned = true
		if !tokens.discovery.mu.TryLock() {
			t.Error("discovery lock held across the solud scan")
		} else {
			tokens.discovery.mu.Unlock()
		}
		return shinoqueSoludHolds(sockets)
	}
	if _, ok := tokens.discovery.resolve(); !ok || !scanned {
		t.Fatalf("ok=%t scanned=%t", ok, scanned)
	}
}

// Concurrent cold requests share one scan, and none passes resolve before it
// ends. The listener check stands in for the forward: it runs only after
// resolve succeeds, and refusing there keeps 64 simultaneous loopback dials
// (refused in bulk by some WSL network modes) out of the test.
func TestShinoqueSoludVerdictConcurrentColdRequests(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	defer listener.Close()
	writeShinoqueDiscovery(t, path, home, listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	shinoqueFrozenClock(tokens)
	var scans, verified atomic.Int32
	var scanning, passedDuringScan atomic.Bool
	tokens.discovery.scanSolud = func(sockets map[string]bool) (bool, error) {
		scans.Add(1)
		scanning.Store(true)
		defer scanning.Store(false)
		time.Sleep(50 * time.Millisecond)
		return shinoqueSoludHolds(sockets)
	}
	handler := newShinoqueRelayHandlerWithListenerCheck(http.NotFoundHandler(), tokens, nil, func(string) bool {
		if scanning.Load() {
			passedDuringScan.Store(true)
		}
		verified.Add(1)
		return false
	})
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shinoqueRelayGet(handler)
		}()
	}
	wg.Wait()
	if scans.Load() != 1 || verified.Load() != 64 || passedDuringScan.Load() {
		t.Fatalf("scans=%d verified=%d passedDuringScan=%t", scans.Load(), verified.Load(), passedDuringScan.Load())
	}
}

// 64 concurrent requests through a real listener all succeed: the relay
// transport caps upstream connections, so the burst is not refused.
func TestShinoqueRelayConcurrentDialsSucceed(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	writeShinoqueDiscovery(t, path, home, upstream.Listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)
	var wg sync.WaitGroup
	codes := make([]int, 64)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = shinoqueRelayGet(handler).Code
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusNoContent {
			t.Fatalf("request %d status = %d", i, code)
		}
	}
}

// Open streaming responses do not hold dial slots: with more streams open than
// there are slots, a new request still succeeds promptly.
func TestShinoqueRelayStreamsDoNotBlockRequests(t *testing.T) {
	const streams = 10
	path, home := shinoqueDiscoveryFixture(t)
	release := make(chan struct{})
	var open atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" && r.Header.Get("X-Shinoque-Relay") != "" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			open.Add(1)
			<-release
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	writeShinoqueDiscovery(t, path, home, upstream.Listener.Addr().(*net.TCPAddr).Port)
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), &shinoqueTokenCache{}, nil)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(release)
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/shinoque/stream", nil)
			req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for open.Load() < streams {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d streams opened", open.Load(), streams)
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan int, 1)
	go func() { done <- shinoqueRelayGet(handler).Code }()
	select {
	case code := <-done:
		if code != http.StatusNoContent {
			t.Fatalf("request beside open streams: status = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request blocked behind open streams")
	}
}

// The client's own abort is not evidence about the listener: no rescan
// follows it. A deadline still invalidates.
func TestShinoqueSoludVerdictClientCancelKeepsVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctx    func() (context.Context, context.CancelFunc)
		rescan bool
	}{
		{"cancelled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, false},
		{"deadline exceeded", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, home := shinoqueDiscoveryFixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			writeShinoqueDiscovery(t, path, home, upstream.Listener.Addr().(*net.TCPAddr).Port)
			tokens := &shinoqueTokenCache{}
			scans := shinoqueCountedScan(tokens)
			shinoqueFrozenClock(tokens)
			handler := newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil)
			if w := shinoqueRelayGet(handler); w.Code != http.StatusNoContent {
				t.Fatalf("status = %d", w.Code)
			}
			ctx, cancel := tc.ctx()
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil).WithContext(ctx)
			req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("aborted status = %d", w.Code)
			}
			want := int32(1)
			if tc.rescan {
				want = 2
			}
			if w := shinoqueRelayGet(handler); w.Code != http.StatusNoContent || scans.Load() != want {
				t.Fatalf("status = %d scans = %d, want %d", w.Code, scans.Load(), want)
			}
		})
	}
}

// Dial slots cover only the dial: cancelled waiters never take or leak one,
// and failed dials release theirs.
func TestShinoqueLimitDialsReleasesSlots(t *testing.T) {
	const limit = 8
	var inDial atomic.Int32
	release := make(chan struct{})
	dial := shinoqueLimitDials(func(ctx context.Context, network, address string) (net.Conn, error) {
		inDial.Add(1)
		defer inDial.Add(-1)
		<-release
		return nil, errors.New("dial failed")
	}, limit)
	fill := func() *sync.WaitGroup {
		var wg sync.WaitGroup
		for range limit {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = dial(context.Background(), "tcp", "127.0.0.1:1")
			}()
		}
		deadline := time.Now().Add(5 * time.Second)
		for inDial.Load() < limit {
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d dials started", inDial.Load(), limit)
			}
			time.Sleep(time.Millisecond)
		}
		return &wg
	}
	wg := fill()
	for range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := dial(ctx, "tcp", "127.0.0.1:1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter err = %v", err)
		}
	}
	if inDial.Load() != limit {
		t.Fatalf("in dial = %d, want %d", inDial.Load(), limit)
	}
	close(release)
	wg.Wait()
	// Every slot came back: a full set of dials can start again.
	release = make(chan struct{})
	wg = fill()
	close(release)
	wg.Wait()
}

// A panicking scan releases its waiters with a refusal, caches nothing, and
// leaves no in-flight entry to block later requests.
func TestShinoqueSoludVerdictScanPanicDoesNotWedge(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	defer listener.Close()
	writeShinoqueDiscovery(t, path, home, listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	shinoqueFrozenClock(tokens)
	entered, proceed := make(chan struct{}), make(chan struct{})
	var enter sync.Once
	tokens.discovery.scanSolud = func(map[string]bool) (bool, error) {
		enter.Do(func() { close(entered) })
		<-proceed
		panic("scan failed")
	}
	go func() {
		defer func() { _ = recover() }()
		tokens.discovery.resolve()
	}()
	<-entered
	// A waiter that is scheduled late runs (and panics in) its own scan
	// instead of joining; either way it must return a refusal.
	waiter := make(chan bool, 1)
	go func() {
		ok := false
		defer func() {
			_ = recover()
			waiter <- ok
		}()
		_, ok = tokens.discovery.resolve()
	}()
	// Let the waiter join the in-flight scan before it panics.
	time.Sleep(50 * time.Millisecond)
	close(proceed)
	select {
	case ok := <-waiter:
		if ok {
			t.Fatal("waiter accepted a panicked scan")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter wedged by a panicked scan")
	}
	scans := shinoqueCountedScan(tokens)
	if _, ok := tokens.discovery.resolve(); !ok || scans.Load() != 1 {
		t.Fatalf("after panic: ok=%t scans=%d", ok, scans.Load())
	}
}

// Invalidations during a scan join it instead of starting more: a burst of
// failing requests runs one /proc scan at a time. The joined result is not
// cached for the newer generation, which rescans on its next request.
func TestShinoqueSoludVerdictInvalidationBurstCoalesces(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	listener := shinoqueListen(t, "tcp4", "127.0.0.1:0", false)
	defer listener.Close()
	writeShinoqueDiscovery(t, path, home, listener.Addr().(*net.TCPAddr).Port)
	tokens := &shinoqueTokenCache{}
	shinoqueFrozenClock(tokens)
	var scans, active, maxActive atomic.Int32
	entered, proceed := make(chan struct{}, 64), make(chan struct{})
	tokens.discovery.scanSolud = func(sockets map[string]bool) (bool, error) {
		scans.Add(1)
		if n := active.Add(1); n > maxActive.Load() {
			maxActive.Store(n)
		}
		defer active.Add(-1)
		entered <- struct{}{}
		<-proceed
		return shinoqueSoludHolds(sockets)
	}
	results := make(chan bool, 32)
	resolve := func() {
		_, ok := tokens.discovery.resolve()
		results <- ok
	}
	go resolve()
	<-entered
	for range 31 {
		tokens.discovery.invalidate()
		go resolve()
	}
	// Let the invalidated requests reach the in-flight scan.
	time.Sleep(100 * time.Millisecond)
	close(proceed)
	for range 32 {
		if !<-results {
			t.Fatalf("burst request refused: %q", tokens.discovery.rejection)
		}
	}
	// A request scheduled after the scan ends may run its own, but scans
	// never overlap.
	if maxActive.Load() != 1 {
		t.Fatalf("scans=%d maxActive=%d, want one scan at a time", scans.Load(), maxActive.Load())
	}
	before := scans.Load()
	tokens.discovery.invalidate()
	if _, ok := tokens.discovery.resolve(); !ok || scans.Load() != before+1 {
		t.Fatalf("ok=%t scans=%d: a newer generation reused a verdict", ok, scans.Load())
	}
}

// Miss cost: every request rescans, as every request did before the cache.
func BenchmarkShinoqueRelayRequestMiss(b *testing.B) {
	handler, tokens := shinoqueBenchRelay(b)
	for b.Loop() {
		tokens.discovery.invalidate()
		if w := shinoqueRelayGet(handler); w.Code != http.StatusNoContent {
			b.Fatalf("status = %d", w.Code)
		}
	}
}
