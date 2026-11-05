package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A cold phone launch issues about this many relayed requests.
const shinoqueColdLaunchRequests = 122

// shinoqueBenchRelay serves an installed discovery against the host's real
// /proc. It uses only APIs that predate the solud verdict cache, so the same
// benchmarks run on the base commit for comparison.
func shinoqueBenchRelay(b *testing.B) (http.Handler, *shinoqueTokenCache) {
	b.Helper()
	b.Setenv("SHINOQUE_HOME", "")
	b.Setenv("SHINOQUE_RELAY_PORT", "")
	_ = os.Unsetenv("SHINOQUE_HOME")
	_ = os.Unsetenv("SHINOQUE_RELAY_PORT")
	b.Setenv("XDG_STATE_HOME", b.TempDir())
	path := shinoqueDiscoveryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		b.Fatal(err)
	}
	home := b.TempDir()
	if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("installed-secret"), 0600); err != nil {
		b.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	b.Cleanup(upstream.Close)
	data, err := json.Marshal(map[string]any{"version": 1, "port": upstream.Listener.Addr().(*net.TCPAddr).Port, "home": home, "relayAuth": 2, "privateDeviceProof": 1})
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		b.Fatal(err)
	}
	tokens := &shinoqueTokenCache{}
	return newShinoqueRelayHandler(http.NotFoundHandler(), tokens, nil), tokens
}

func shinoqueBenchGet(b *testing.B, handler http.Handler) {
	req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/m/test/b/wsl")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		b.Errorf("status = %d", w.Code)
	}
}

// Steady-state cost of one relayed request (a cache hit on the branch).
func BenchmarkShinoqueRelayRequest(b *testing.B) {
	handler, _ := shinoqueBenchRelay(b)
	shinoqueBenchGet(b, handler)
	for b.Loop() {
		shinoqueBenchGet(b, handler)
	}
}

// One op is a cold launch: a fresh daemon cache, then 122 requests one by one.
func BenchmarkShinoqueRelayColdLaunchSequential(b *testing.B) {
	shinoqueBenchRelay(b)
	for b.Loop() {
		fresh := &shinoqueTokenCache{}
		handler := newShinoqueRelayHandler(http.NotFoundHandler(), fresh, nil)
		for range shinoqueColdLaunchRequests {
			shinoqueBenchGet(b, handler)
		}
	}
}

// One op is a cold launch: a fresh daemon cache, then 122 requests from 8
// concurrent workers (about a browser's per-host limit; wider loopback bursts
// are refused by some WSL network modes).
func BenchmarkShinoqueRelayColdLaunchConcurrent(b *testing.B) {
	shinoqueBenchRelay(b)
	for b.Loop() {
		fresh := &shinoqueTokenCache{}
		handler := newShinoqueRelayHandler(http.NotFoundHandler(), fresh, nil)
		requests := make(chan struct{}, shinoqueColdLaunchRequests)
		for range shinoqueColdLaunchRequests {
			requests <- struct{}{}
		}
		close(requests)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range requests {
					shinoqueBenchGet(b, handler)
				}
			}()
		}
		wg.Wait()
	}
}
