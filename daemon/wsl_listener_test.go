package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWSLListenerAddresses(t *testing.T) {
	for _, tc := range []struct {
		primary, extra, backend string
		want                    []string
	}{
		{"127.0.0.1:8787", "172.29.132.65:8787", "wsl", []string{"127.0.0.1:8787", "172.29.132.65:8787"}},
		{"127.0.0.1:8787", "127.0.0.1:8787", "wsl", []string{"127.0.0.1:8787"}},
		{"127.0.0.1:8787", "", "host", []string{"127.0.0.1:8787"}},
		{"0.0.0.0:8787", "172.29.132.65:8787", "wsl", nil},
		{"127.0.0.1:8787", "172.29.132.65:8787", "host", nil},
		{"127.0.0.1:8787", "0.0.0.0:8787", "wsl", nil},
		{"127.0.0.1:8787", "[::]:8787", "wsl", nil},
		{"127.0.0.1:8787", "host:8787", "wsl", nil},
		{"127.0.0.1:8787", "224.0.0.1:8787", "wsl", nil},
		{"127.0.0.1:8787", "169.254.1.2:8787", "wsl", nil},
		{"127.0.0.1:8787", "172.29.132.65:9999", "wsl", nil},
		{"127.0.0.1:0", "172.29.132.65:0", "wsl", nil},
		{"127.0.0.1:99999", "172.29.132.65:99999", "wsl", nil},
	} {
		got, err := wslListenerAddresses(tc.primary, tc.extra, tc.backend)
		if !reflect.DeepEqual(got, tc.want) || (err != nil) != (tc.want == nil) {
			t.Errorf("%+v: got %v, %v", tc, got, err)
		}
	}
}

type wslTestListener struct {
	net.Listener
	closed bool
}

func (ln *wslTestListener) Close() error { ln.closed = true; return nil }

func TestWSLListenerHandoffPreflightsBothBeforeMarker(t *testing.T) {
	for _, failure := range []string{"", "preflight-interface", "bind-interface"} {
		t.Run(failure, func(t *testing.T) {
			first, second := &wslTestListener{}, &wslTestListener{}
			var events []string
			listeners, err := startupHandoffListenersWithDeps("tcp4", []string{"loopback", "interface"}, startupHandoffDeps{
				preflight: func(_, addr string) (net.Listener, error) {
					events = append(events, "preflight-"+addr)
					if failure == "preflight-"+addr {
						return nil, errors.New("unavailable")
					}
					if addr == "loopback" {
						return first, nil
					}
					return nil, nil // predecessor still owns the interface socket
				},
				writeMarker:  func() { events = append(events, "marker") },
				clearPending: func() { events = append(events, "clear") },
				acquire: func(_, addr string) (net.Listener, error) {
					events = append(events, "bind-"+addr)
					if failure == "bind-"+addr {
						return nil, errors.New("held")
					}
					return second, nil
				},
			})
			if failure == "" {
				if err != nil || !reflect.DeepEqual(listeners, []net.Listener{first, second}) || first.closed || second.closed {
					t.Fatalf("handoff lost sockets: %v %v", listeners, err)
				}
			} else if err == nil || listeners != nil || !first.closed {
				t.Fatalf("partial startup retained socket: %v %v closed=%v", listeners, err, first.closed)
			}
			want := []string{"preflight-loopback", "preflight-interface"}
			if failure != "preflight-interface" {
				want = append(want, "marker", "clear", "bind-interface")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("handoff order: %v, want %v", events, want)
			}
		})
	}
}

func TestWSLListenerBindFailureClosesAcquiredLoopback(t *testing.T) {
	first := &wslTestListener{}
	got, err := startupHandoffListenersWithDeps("tcp4", []string{"loopback", "interface"}, startupHandoffDeps{
		preflight:   func(string, string) (net.Listener, error) { return nil, nil },
		writeMarker: func() {}, clearPending: func() {},
		acquire: func(_, addr string) (net.Listener, error) {
			if addr == "loopback" {
				return first, nil
			}
			return nil, errors.New("held")
		},
	})
	if err == nil || got != nil || !first.closed {
		t.Fatalf("partial bind: %v %v closed=%v", got, err, first.closed)
	}
}

func TestWSLListenersServeLocalIntegrationsAndAuthenticatedInterface(t *testing.T) {
	t.Setenv("SOLU_LOOPBACK_REQUIRE_AUTH", "0")
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var interfaceIP string
	for _, addr := range interfaces {
		ip, _, _ := net.ParseCIDR(addr.String())
		if ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() {
			interfaceIP = ip.String()
			break
		}
	}
	if interfaceIP == "" {
		t.Skip("no non-loopback IPv4 interface")
	}
	primary, err := listenOnConfiguredPort("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	_, port, _ := net.SplitHostPort(primary.Addr().String())
	addresses, err := wslListenerAddresses(primary.Addr().String(), net.JoinHostPort(interfaceIP, port), "wsl")
	if err != nil {
		t.Fatal(err)
	}
	listeners, err := startupHandoffListenersWithDeps("tcp4", addresses, startupHandoffDeps{
		preflight: func(_, addr string) (net.Listener, error) {
			if addr == addresses[0] {
				return primary, nil
			}
			return nil, nil
		},
		writeMarker: func() {}, clearPending: func() {}, acquire: listenOnConfiguredPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := &authGate{token: "fixture-token"}
	srv := &http.Server{Handler: gate.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.URL.Path) }))}
	t.Cleanup(func() { _ = srv.Close() })
	for _, ln := range listeners {
		go func(ln net.Listener) { _ = srv.Serve(ln) }(ln)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		addr, path, token string
		status            int
	}{
		{addresses[0], "/api/health", "", 200},
		{addresses[0], "/mcp", "", 200},
		{addresses[0], "/api/hooks/tool-call", "", 200},
		{addresses[1], "/api/health", "", 401},
		{addresses[1], "/api/health", "fixture-token", 200},
	} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+tc.addr+tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || tc.status == 200 && strings.TrimSpace(string(body)) != tc.path {
			t.Fatalf("%+v: HTTP %d %s", tc, resp.StatusCode, body)
		}
	}
}
