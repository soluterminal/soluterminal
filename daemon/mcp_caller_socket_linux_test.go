//go:build linux

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
)

// mcpPeerSocketRequest is a request as the server saw it on server.
func mcpPeerSocketRequest(server net.Conn, remote string) *http.Request {
	r, _ := http.NewRequestWithContext(context.WithValue(context.Background(), http.LocalAddrContextKey, server.LocalAddr()), "POST", "/mcp", nil)
	r.RemoteAddr = remote
	return r
}

// A loopback connection from this process is a local socket owned by it.
// WSL's user-mode networking can hand the server a rewritten source port
// (seen on IPv4 and, under load, IPv6 loopback). A rewritten tuple may or may
// not have a local row for a moment (the relay's own socket), so it proves
// nothing either way: only connections the server saw unchanged are checked,
// and a host that rewrites every one skips with the reason.
func TestMCPPeerSocketFindsLocalClient(t *testing.T) {
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		t.Skip("no /proc/net/tcp")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	checked, rewritten := 0, 0
	for i := 0; i < 20; i++ {
		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		server, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if server.RemoteAddr().String() != client.LocalAddr().String() {
			rewritten++
		} else {
			checked++
			if found, owner := mcpPeerSocket(mcpPeerSocketRequest(server, server.RemoteAddr().String())); !found || owner != os.Getpid() {
				t.Fatalf("local client: found=%t owner=%d, want true %d", found, owner, os.Getpid())
			}
		}
		client.Close()
		server.Close()
	}
	if checked == 0 {
		t.Skipf("this host rewrote the source port of all %d loopback connections (WSL user-mode networking)", rewritten)
	}
}

// An address no local socket has (a connection relayed in from Windows) is
// not found, nor is an unparsable one. Deterministic on IPv4 too: nothing
// connects from the listener's own port to itself.
func TestMCPPeerSocketAbsentClient(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if found, owner := mcpPeerSocket(mcpPeerSocketRequest(server, ln.Addr().String())); found || owner != 0 {
		t.Fatalf("absent client: found=%t owner=%d", found, owner)
	}
	if found, _ := mcpPeerSocket(mcpPeerSocketRequest(server, "not-an-address")); found {
		t.Fatal("unparsable address found")
	}
}
