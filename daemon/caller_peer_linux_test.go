//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdentifyCallerLocalTraceMiss(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	approveTestDevice(t, a, newTestDevice(t))
	stubPeerPID(t, 0, false)
	peers := []string{"127.0.0.1:23456", "[::1]:23456", "[::ffff:127.0.0.1]:23456", "@local", "", "invalid"}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil {
			peers = append(peers, net.JoinHostPort(ip.String(), "23456"))
		}
	}
	for _, peer := range peers {
		t.Run(peer, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:19797/", nil)
			r.RemoteAddr = peer
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Origin", "http://127.0.0.1:19797")
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19797}))
			if id := a.identifyCaller(r); id.Kind != callerUnverified || id.PID != 0 || id.PaneID != "" {
				t.Fatalf("local trace miss = %+v", id)
			}
			w := httptest.NewRecorder()
			if a.requireHuman(w, r, "change settings") || w.Code != http.StatusForbidden {
				t.Fatalf("local trace miss accepted: %d", w.Code)
			}
		})
	}
}

func TestLocalTraceMissSignedDeviceAndRolloutModes(t *testing.T) {
	stubPeerPID(t, 0, false)
	for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
		t.Run(mode, func(t *testing.T) {
			a := humanProofTestServer(t, mode)
			logs := captureLog(t)
			d := newTestDevice(t)
			approveTestDevice(t, a, d)
			for _, peer := range []string{"127.0.0.1:23456", "[::1]:23456", "[::ffff:127.0.0.1]:23456", "172.20.32.1:23456", "@local"} {
				t.Run(peer, func(t *testing.T) {
					request := func(signed bool) *http.Request {
						r := httptest.NewRequest("POST", "http://127.0.0.1:19797/api/pane-confirmations/absent", nil)
						if signed {
							r = d.localRequest(t, a, "POST", r.URL.String(), "", time.Now(), mode+peer)
							// Device proof works independently of fetch metadata.
							r.Header.Del("Sec-Fetch-Site")
						} else {
							r.Header.Set("Sec-Fetch-Site", "same-origin")
							r.Header.Set("Origin", "http://127.0.0.1:19797")
						}
						r.RemoteAddr = peer
						r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19797}))
						r, _ = throughProof(a, r)
						return r
					}
					unsigned := request(false)
					id := a.identifyCaller(unsigned)
					want := callerHuman
					if mode == humanProofRequire {
						want = callerUnverified
					}
					if id.Kind != want || !id.Unproven || id.Device != "" || id.PaneID != "" {
						t.Fatalf("unsigned %s caller = %+v", mode, id)
					}
					if allowed := a.requireHuman(httptest.NewRecorder(), unsigned, "answer request"); allowed != (mode != humanProofRequire) {
						t.Fatalf("unsigned %s allowed = %t", mode, allowed)
					}
					wouldRefuse := "authority: would refuse unproven person POST /api/pane-confirmations/absent from " + peer
					if got := strings.Contains(logs.String(), wouldRefuse); got != (mode == humanProofReport) {
						t.Fatalf("Report diagnostic under %s present = %t; logs=%s", mode, got, logs.String())
					}
					signed := request(true)
					id = a.identifyCaller(signed)
					if id.Kind != callerHuman || id.Device != deviceID(d.spki) || id.Unproven {
						t.Fatalf("signed %s caller = %+v", mode, id)
					}
					if !a.requireHuman(httptest.NewRecorder(), signed, "answer request") {
						t.Fatalf("signed %s request refused", mode)
					}
				})
			}
		})
	}
}

func TestObservedLocalSocketWithoutOwnerUsesSharedMode(t *testing.T) {
	stubPeerPID(t, 0, true) // Exact local tuple found, but no readable owner.
	for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
		t.Run(mode, func(t *testing.T) {
			a := humanProofTestServer(t, mode)
			d := newTestDevice(t)
			approveTestDevice(t, a, d)
			for _, signed := range []bool{false, true} {
				r := httptest.NewRequest("POST", "http://127.0.0.1:19797/api/pane-confirmations/absent", nil)
				if signed {
					r = d.localRequest(t, a, "POST", r.URL.String(), "", time.Now(), "observed-"+mode)
				}
				r.RemoteAddr = "127.0.0.1:23456"
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set("Origin", "http://127.0.0.1:19797")
				r, _ = throughProof(a, r)
				got := a.identifyCaller(r)
				want := callerHuman
				if mode == humanProofRequire && !signed {
					want = callerUnverified
				}
				if got.Kind != want || got.PID != 0 || got.PaneID != "" || got.Unproven == signed {
					t.Fatalf("%s signed=%t observed local without owner = %+v, want %s", mode, signed, got, want)
				}
				if signed && got.Device != deviceID(d.spki) {
					t.Fatalf("signed device = %q", got.Device)
				}
				w := httptest.NewRecorder()
				if allowed := a.requireHuman(w, r, "answer request"); allowed != (want == callerHuman) {
					t.Fatalf("%s signed=%t allowed=%t", mode, signed, allowed)
				}
			}
			// This classification does not add a gate to ordinary reads.
			w := httptest.NewRecorder()
			a.handleGetAgentAuthority(w, httptest.NewRequest("GET", "/api/settings/agent-authority", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%s settings read = %d", mode, w.Code)
			}
		})
	}
}

func TestUnixListenerRemainsLocallyObserved(t *testing.T) {
	for _, mode := range []string{humanProofOff, humanProofReport, humanProofRequire} {
		t.Run(mode, func(t *testing.T) {
			a := humanProofTestServer(t, mode)
			approveTestDevice(t, a, newTestDevice(t))
			r := httptest.NewRequest("POST", "/api/pane-confirmations/absent", nil)
			r.RemoteAddr = "@"
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.UnixAddr{Name: "/run/caller-test.sock", Net: "unix"}))
			if pid, local := lookupPeerPID(r); pid != 0 || !local {
				t.Fatalf("Unix listener = pid %d local %t", pid, local)
			}
			want := callerHuman
			if mode == humanProofRequire {
				want = callerUnverified
			}
			if got := a.identifyCaller(r); got.Kind != want || !got.Unproven {
				t.Fatalf("Unix listener under %s = %+v, want %s", mode, got, want)
			}
		})
	}
}

func TestLocalCallerRepeatedConnections(t *testing.T) {
	a := humanProofTestServer(t, humanProofRequire)
	approveTestDevice(t, a, newTestDevice(t))
	for _, listenAddr := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(listenAddr, func(t *testing.T) {
			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				if listenAddr == "[::1]:0" {
					t.Skipf("IPv6 unavailable: %v", err)
				}
				t.Fatal(err)
			}
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Keep the identity bound to its response: background service
				// discovery can probe this port between test requests.
				json.NewEncoder(w).Encode(a.identifyCaller(r))
			}))
			srv.Listener.Close()
			srv.Listener = ln
			srv.Start()
			defer srv.Close()
			transport := &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{}).DialContext}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			traced, misses := 0, 0
			for attempt := 0; attempt < 200; attempt++ {
				req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
				req.Header.Set("Sec-Fetch-Site", "same-origin")
				req.Header.Set("Origin", srv.URL)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var id callerIdentity
				err = json.NewDecoder(resp.Body).Decode(&id)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if id.Kind == callerHuman || (id.PID == 0 && id.Kind != callerUnverified) {
					t.Fatalf("connection %d = %+v", attempt, id)
				}
				if id.PID > 0 {
					if id.PID != os.Getpid() {
						t.Fatalf("connection %d pid = %d, want %d", attempt, id.PID, os.Getpid())
					}
					traced++
				} else {
					misses++
				}
			}
			t.Logf("200 connections: %d traced, %d unverified, 0 browser fallbacks", traced, misses)
		})
	}
}

func TestFindConnectionInodeAddressFamiliesAndStates(t *testing.T) {
	for _, address := range []struct{ client, server, local, remote string }{
		{"127.0.0.1", "127.0.0.1", "0100007F", "0100007F"},
		{"127.0.0.1", "127.0.0.1", "0000000000000000FFFF00000100007F", "0000000000000000FFFF00000100007F"},
		{"::1", "::1", "00000000000000000000000001000000", "00000000000000000000000001000000"},
	} {
		for _, state := range []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "0A", "0B"} {
			t.Run(address.local+"/"+state, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "tcp")
				inode := uint64(12345)
				if state == "06" || state == "07" {
					inode = 0
				}
				row := fmt.Sprintf("header\n0: %s:5BA0 %s:4D55 %s 0:0 0:0 0 1000 0 %d\n", address.local, address.remote, state, inode)
				if err := os.WriteFile(path, []byte(row), 0600); err != nil {
					t.Fatal(err)
				}
				got, readable, matched := findConnectionInode(path, &net.TCPAddr{IP: net.ParseIP(address.client), Port: 23456}, &net.TCPAddr{IP: net.ParseIP(address.server), Port: 19797})
				if !readable {
					t.Fatal("fixture table was not readable")
				}
				want := inode
				active := state == "01" || state == "02" || state == "03" || state == "04" || state == "05" || state == "08"
				if !active {
					want = 0
				}
				if got != want || matched != active {
					t.Fatalf("inode = %d matched=%t, want %d matched=%t", got, matched, want, active)
				}
			})
		}
	}
}

func TestLookupPeerPIDTableAvailabilityAndBackoff(t *testing.T) {
	client := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 23456}
	server := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 19797}
	row := "header\n0: 0100007F:5BA0 0100007F:4D55 01 0:0 0:0 0 1000 0 12345\n"
	for _, tc := range []struct {
		name, initial string
		publish       bool
		ownerPID      int
		wantPID       int
		wantLocal     bool
		wantBackoffs  int
		wantOwners    int
	}{
		{name: "no proc tables"},
		{name: "tuple absent", initial: "header\n", wantBackoffs: 1},
		{name: "tuple published after backoff", initial: "header\n", publish: true, ownerPID: 42, wantPID: 42, wantLocal: true, wantBackoffs: 1, wantOwners: 1},
		{name: "owner unavailable", initial: row, wantLocal: true, wantOwners: 1},
		{name: "owner available", initial: row, ownerPID: 42, wantPID: 42, wantLocal: true, wantOwners: 1},
		{name: "zero inode ignored", initial: "header\n0: 0100007F:5BA0 0100007F:4D55 01 0:0 0:0 0 1000 0 0\n", wantBackoffs: 1},
		{name: "time wait ignored", initial: "header\n0: 0100007F:5BA0 0100007F:4D55 06 0:0 0:0 0 1000 0 12345\n", wantBackoffs: 1},
		{name: "closed client ignored", initial: "header\n0: 0100007F:5BA0 0100007F:4D55 06 0:0 0:0 0 1000 0 0\n", wantBackoffs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tcp")
			write := func(contents string) {
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.initial != "" {
				write(tc.initial)
			}
			backoffs, owners := 0, 0
			pid, ok := lookupPeerPIDInTables(client, server, []string{path, path + "6"}, func(inode uint64) int {
				owners++
				if inode != 12345 {
					t.Fatalf("owner lookup inode = %d", inode)
				}
				return tc.ownerPID
			}, func(delay time.Duration) {
				backoffs++
				if delay != time.Millisecond {
					t.Fatalf("backoff = %s", delay)
				}
				if tc.publish {
					write(row)
				}
			})
			if pid != tc.wantPID || ok != tc.wantLocal || backoffs != tc.wantBackoffs || owners != tc.wantOwners {
				t.Fatalf("pid=%d local=%t backoffs=%d owners=%d; want pid=%d local=%t backoffs=%d owners=%d", pid, ok, backoffs, owners, tc.wantPID, tc.wantLocal, tc.wantBackoffs, tc.wantOwners)
			}
		})
	}
}
