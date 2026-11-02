package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShinoqueRelayCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, fields      string
		readable, private int
	}{
		{"old discovery", "", 503, 503},
		{"old auth", `,"relayAuth": 1,"privateDeviceProof": 1`, 503, 503},
		{"proof only", `,"privateDeviceProof": 1`, 503, 503},
		{"readable only", `,"relayAuth": 2`, 200, 503},
		{"both", `,"relayAuth": 2,"privateDeviceProof": 1`, 200, 200},
		{"unknown auth version", `,"relayAuth": 3,"privateDeviceProof": 1`, 503, 503},
		{"unknown proof version", `,"relayAuth": 2,"privateDeviceProof": 2`, 200, 503},
		{"unknown versions", `,"relayAuth": 3,"privateDeviceProof": 2`, 503, 503},
		{"zero auth", `,"relayAuth": 0,"privateDeviceProof": 1`, 503, 503},
		{"zero proof", `,"relayAuth": 2,"privateDeviceProof": 0`, 200, 503},
		{"auth decimal", `,"relayAuth": 2.0,"privateDeviceProof": 1`, 503, 503},
		{"proof decimal", `,"relayAuth": 2,"privateDeviceProof": 1.0`, 503, 503},
		{"auth exponent", `,"relayAuth": 2e0,"privateDeviceProof": 1`, 503, 503},
		{"proof exponent", `,"relayAuth": 2,"privateDeviceProof": 1e0`, 503, 503},
		{"auth string", `,"relayAuth": "2","privateDeviceProof": 1`, 503, 503},
		{"proof string", `,"relayAuth": 2,"privateDeviceProof": "1"`, 503, 503},
		{"auth fraction", `,"relayAuth": 2.5,"privateDeviceProof": 1`, 503, 503},
		{"proof fraction", `,"relayAuth": 2,"privateDeviceProof": 1.5`, 503, 503},
		{"auth null", `,"relayAuth": null,"privateDeviceProof": 1`, 503, 503},
		{"proof null", `,"relayAuth": 2,"privateDeviceProof": null`, 503, 503},
		{"auth bool", `,"relayAuth": true,"privateDeviceProof": 1`, 503, 503},
		{"proof object", `,"relayAuth": 2,"privateDeviceProof": {}`, 503, 503},
		{"auth negative", `,"relayAuth": -1,"privateDeviceProof": 1`, 503, 503},
		{"proof negative", `,"relayAuth": 2,"privateDeviceProof": -1`, 200, 503},
		{"extra field", `,"relayAuth": 2,"privateDeviceProof": 1,"host":"localhost"`, 503, 503},
		{"case variant", `,"RelayAuth":2,"privateDeviceProof": 1`, 503, 503},
		{"duplicate field", `,"relayAuth": 2,"relayAuth": 2,"privateDeviceProof": 1`, 503, 503},
		{"trailing JSON", `,"relayAuth": 2,"privateDeviceProof": 1} {`, 503, 503},
		{"malformed JSON", `,"relayAuth": 2,"privateDeviceProof": `, 503, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, home := shinoqueDiscoveryFixture(t)
			if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("private-relay-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			var hits atomic.Int32
			seen := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/read" {
					http.NotFound(w, r)
					return
				}
				hits.Add(1)
				select {
				case seen <- r.Header.Clone():
				default:
					http.Error(w, "unexpected extra upstream request", http.StatusInternalServerError)
					return
				}
				_, _ = fmt.Fprint(w, "ok")
			}))
			defer server.Close()
			_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			record := fmt.Sprintf(`{"version":1,"port":%s,"home":%q%s}`, port, home, tc.fields)
			if err := os.WriteFile(path, []byte(record), 0600); err != nil {
				t.Fatal(err)
			}
			handler := newShinoqueRelayHandler(http.NotFoundHandler(), &shinoqueTokenCache{}, nil)
			var wantHits int32
			for _, private := range []bool{false, true} {
				req := httptest.NewRequest(http.MethodGet, "/shinoque/api/read", nil)
				req.Header.Set("X-Forwarded-Prefix", "/m/test")
				req.Header.Add("X-Shinoque-Private-Device", "client-device")
				req.Header.Add("X-Shinoque-Private-Device", "dev_b71SY8ikOq3NEn-6MV-j9z")
				req.Header.Set("X-Shinoque-Relay", "client-relay-value")
				want := tc.readable
				if private {
					req = req.WithContext(withPrivateRelayDevice(req.Context(), "dev_b71SY8ikOq3NEn-6MV-j9z"))
					want = tc.private
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("private=%t: status=%d body=%q want=%d", private, w.Code, w.Body.String(), want)
				}
				if want == 200 {
					wantHits++
					headers := <-seen
					devices := headers.Values("X-Shinoque-Private-Device")
					wantRelay := "private-relay-fixture"
					if private {
						if len(devices) != 1 || devices[0] != "dev_b71SY8ikOq3NEn-6MV-j9z" {
							t.Fatalf("private channel device headers = %q", devices)
						}
						wantRelay = "aa393a1e81259f25ae7887a28f04a8a53a1d09d89d094afc86b98762bd1299db"
					} else if len(devices) != 0 {
						t.Fatalf("client device headers reached readable upstream: %q", devices)
					}
					if headers.Get("X-Shinoque-Relay") != wantRelay {
						t.Fatalf("private=%t: incorrect relay credential", private)
					}
				}
				if hits.Load() != wantHits {
					t.Fatalf("private=%t: hits=%d want=%d", private, hits.Load(), wantHits)
				}
				if tc.name == "old discovery" && strings.TrimSpace(w.Body.String()) != shinoqueUpdateRequired {
					t.Fatalf("body=%q", w.Body.String())
				}
			}
		})
	}
}

func TestShinoqueRelayCapabilityChanges(t *testing.T) {
	path, home := shinoqueDiscoveryFixture(t)
	var hits atomic.Int32
	const requestMarker = "capability-change"
	// Local service discovery may probe the listener independently of this test.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Relay-Request") == requestMarker {
			hits.Add(1)
		}
	})
	server := httptest.NewServer(upstream)
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	handler := newShinoqueRelayHandler(http.NotFoundHandler(), &shinoqueTokenCache{}, nil)
	write := func(auth, proof int) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"version": 1, "port": n, "home": home, "relayAuth": auth, "privateDeviceProof": proof})
		if err != nil {
			t.Fatal(err)
		}
		// Atomic replacement retains mtime so the inode also participates.
		stamp := time.Unix(1700000000, 0)
		replacement := path + ".new"
		if err := os.WriteFile(replacement, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(replacement, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}
	check := func(readable, private int) {
		t.Helper()
		for _, want := range []struct {
			device string
			status int
		}{{"", readable}, {"dev_b71SY8ikOq3NEn-6MV-j9z", private}} {
			before := hits.Load()
			req := httptest.NewRequest(http.MethodGet, "/shinoque/", nil)
			req.Header.Set("X-Forwarded-Prefix", "/m/test")
			req.Header.Set("X-Test-Relay-Request", requestMarker)
			req = req.WithContext(withPrivateRelayDevice(req.Context(), want.device))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != want.status {
				t.Fatalf("device=%q: status=%d want=%d", want.device, w.Code, want.status)
			}
			if want.status == 200 {
				before++
			}
			if hits.Load() != before {
				t.Fatalf("hits=%d want=%d", hits.Load(), before)
			}
		}
	}
	write(2, 1)
	check(200, 200)
	write(3, 1)
	check(503, 503)
	write(2, 2)
	check(200, 503)
	write(3, 2)
	check(503, 503)
	write(2, 1)
	check(200, 200)
	write(2, 0)
	check(200, 503)
	write(1, 1)
	check(503, 503)
	write(2, 1)
	check(200, 200)
	if err := os.WriteFile(filepath.Join(home, "relay-token"), []byte("rotated-token"), 0600); err != nil {
		t.Fatal(err)
	}
	write(2, 0)
	check(200, 503)
	second := httptest.NewServer(upstream)
	defer second.Close()
	_, port, err = net.SplitHostPort(strings.TrimPrefix(second.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ = strconv.Atoi(port)
	write(1, 1)
	check(503, 503)
	write(2, 1)
	check(200, 200)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	check(503, 503)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	check(200, 200)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	check(503, 503)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(503, 503)
}
