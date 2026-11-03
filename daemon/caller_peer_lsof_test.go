package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

func tcpAddr(t *testing.T, s string) *net.TCPAddr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// lsof -Fpn output as macOS prints it: the daemon's accepted socket and the
// client's, a listener, and an IPv6 pair.
const lsofFixture = `p501
f12
n127.0.0.1:8787
f19
n127.0.0.1:8787->127.0.0.1:52144
p733
f27
n127.0.0.1:52144->127.0.0.1:8787
p902
f5
n[::1]:61001->[::1]:8787
`

func TestLsofPeerPIDFindsTheClientEnd(t *testing.T) {
	server := tcpAddr(t, "127.0.0.1:8787")
	// The daemon's own accepted socket (local=server) is never the client.
	if pid, ok := lsofPeerPID([]byte(lsofFixture), tcpAddr(t, "127.0.0.1:52144"), server); !ok || pid != 733 {
		t.Fatalf("client end = %d %t, want 733", pid, ok)
	}
	if pid, ok := lsofPeerPID([]byte(lsofFixture), tcpAddr(t, "[::1]:61001"), tcpAddr(t, "[::1]:8787")); !ok || pid != 902 {
		t.Fatalf("IPv6 = %d %t", pid, ok)
	}
	if _, ok := lsofPeerPID([]byte(lsofFixture), tcpAddr(t, "127.0.0.1:52145"), server); ok {
		t.Fatal("matched a port nobody holds")
	}
	if _, ok := lsofPeerPID([]byte(lsofFixture), tcpAddr(t, "127.0.0.1:52144"), tcpAddr(t, "127.0.0.1:9999")); ok {
		t.Fatal("matched a connection to another server")
	}
	// The daemon's own outgoing connection (its preview proxy) is found as
	// the daemon, which caller authority classifies as a worker.
	own := "p501\nf31\nn127.0.0.1:52200->127.0.0.1:8787\n"
	if pid, ok := lsofPeerPID([]byte(lsofFixture+own), tcpAddr(t, "127.0.0.1:52200"), server); !ok || pid != 501 {
		t.Fatalf("daemon's own client end = %d %t, want 501", pid, ok)
	}
}

func TestParseLsofConnection(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"127.0.0.1:1->127.0.0.1:2", true},
		{"[::1]:1->[::1]:2", true},
		{"127.0.0.1:1->127.0.0.1:2 (ESTABLISHED)", true},
		{"127.0.0.1:8787", false},
		{"*:8787", false},
		{"host:1->127.0.0.1:2", false},
		{"127.0.0.1:0->127.0.0.1:2", false},
	} {
		if _, _, ok := parseLsofConnection(tc.in); ok != tc.ok {
			t.Errorf("%q: ok=%t want %t", tc.in, ok, tc.ok)
		}
	}
}

// The real lsof, on a real connection: the command and parsing the Mac uses,
// run wherever lsof is installed.
func TestLsofFindsARealClient(t *testing.T) {
	if _, err := os.Stat(lsofPath); err != nil {
		found, err := exec.LookPath("lsof")
		if err != nil {
			t.Skip("lsof not installed")
		}
		// Linux keeps lsof elsewhere; production only ever runs lsofPath.
		prev := lsofPath
		lsofPath = found
		t.Cleanup(func() { lsofPath = prev })
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if c := <-accepted; c != nil {
		defer c.Close()
	}
	client := conn.LocalAddr().(*net.TCPAddr)
	server := ln.Addr().(*net.TCPAddr)
	out, ok := runLsofTCPPort(client.Port)
	if !ok {
		t.Fatal("lsof did not answer")
	}
	if pid, found := lsofPeerPID(out, client, server); !found || pid != os.Getpid() {
		t.Fatalf("real lsof: pid %d found %t, want %d\n%s", pid, found, os.Getpid(), out)
	}
	// Nothing on a port no socket uses.
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	free := l2.Addr().(*net.TCPAddr).Port
	l2.Close()
	if out, ok := runLsofTCPPort(free); !ok {
		t.Fatal("lsof with no match should still answer")
	} else if _, found := lsofPeerPID(out, &net.TCPAddr{IP: client.IP, Port: free}, server); found {
		t.Fatal("matched a free port")
	}
}

// Only a found socket is an answer. Nothing found, or no answer from lsof,
// leaves the caller to the approved-device policy rather than refusing the
// person's own window whenever lsof is slow.
func TestPeerFromLsofOnlyAnswersWhenFound(t *testing.T) {
	server := tcpAddr(t, "127.0.0.1:8787")
	loop := tcpAddr(t, "127.0.0.1:52144")
	if pid, known := peerFromLsof([]byte(lsofFixture), true, loop, server); pid != 733 || !known {
		t.Fatalf("found = %d %t", pid, known)
	}
	if pid, known := peerFromLsof(nil, true, tcpAddr(t, "127.0.0.1:52999"), server); pid != 0 || known {
		t.Fatalf("not found = %d %t, want unknown", pid, known)
	}
	if pid, known := peerFromLsof(nil, false, loop, server); pid != 0 || known {
		t.Fatalf("lsof failed = %d %t, want unknown", pid, known)
	}
}

// The macOS processes that make a browser's or the desktop app's requests,
// by executable name and as the kernel's 16-byte p_comm cuts them.
func TestMacOSBrowserNamesAreRecognised(t *testing.T) {
	for _, name := range []string{
		"Safari",
		"Google Chrome", "Google Chrome Helper", "Google Chrome Canary Helper",
		"Microsoft Edge", "Microsoft Edge Helper",
		"Brave Browser", "Brave Browser Helper",
		"Chromium Helper", "Vivaldi Helper", "Opera Helper", "firefox",
	} {
		full := lowerNoExe(name)
		cut := full
		if len(cut) > 16 {
			cut = cut[:16]
		}
		if !browserProcessNames[full] {
			t.Errorf("%q not recognised", name)
		}
		if !browserProcessNames[cut] {
			t.Errorf("%q cut to p_comm %q not recognised", name, cut)
		}
	}
	for _, name := range []string{"node", "python3", "curl", "zsh", "Terminal", "iTerm2", "solud", "com.apple.WebKit.Networking"} {
		if browserProcessNames[lowerNoExe(name)] {
			t.Errorf("%q taken for a browser", name)
		}
	}
	// WebKit's network service serves any app's web view: judged like an
	// untraceable connection, by full name and p_comm.
	for _, name := range []string{"com.apple.WebKit.Networking", "com.apple.WebKit.Networking"[:16]} {
		if !sharedNetworkServiceNames[lowerNoExe(name)] {
			t.Errorf("%q not recognised as a shared network service", name)
		}
	}
}

// A found caller is remembered for its connection; a miss is not.
func TestLookupPeerPIDIsRememberedPerConnection(t *testing.T) {
	ctx := withConnPeerMemo(context.Background(), nil)
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	r.RemoteAddr = "not-an-address"
	if pid, known := lookupPeerPID(r); pid != 0 || known {
		t.Fatalf("unparseable peer = %d %t", pid, known)
	}
	memo := ctx.Value(connPeerMemoKey{}).(*connPeerMemo)
	if _, ok := memo.get(); ok {
		t.Fatal("a miss was remembered")
	}
	memo.set(4242)
	if pid, known := lookupPeerPID(r); pid != 4242 || !known {
		t.Fatalf("remembered = %d %t", pid, known)
	}
	// Without a memo (tests, other listeners) every request looks up.
	plain := httptest.NewRequest("GET", "/", nil)
	plain.RemoteAddr = "not-an-address"
	if _, known := lookupPeerPID(plain); known {
		t.Fatal("no memo, unparseable peer was known")
	}
}

// A real connection on this host: looked up once, then served from the memo.
func TestLookupPeerPIDRealConnectionMemo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{ConnContext: withConnPeerMemo, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, _ := lookupPeerPID(r)
		memo := r.Context().Value(connPeerMemoKey{}).(*connPeerMemo)
		remembered, _ := memo.get()
		fmt.Fprintf(w, "%d %d", pid, remembered)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
	for i := 0; i < 2; i++ {
		resp, err := client.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var pid, remembered int
		fmt.Sscanf(string(b), "%d %d", &pid, &remembered)
		if pid == 0 {
			t.Skip("this host can't trace its own loopback connections")
		}
		if pid != os.Getpid() || remembered != pid {
			t.Fatalf("request %d: pid %d remembered %d, want %d", i, pid, remembered, os.Getpid())
		}
	}
}

// Only the system lsof is run: nothing on PATH stands in for it.
func TestLsofRunsOnlyTheSystemPath(t *testing.T) {
	prev := lsofPath
	t.Cleanup(func() { lsofPath = prev })
	lsofPath = t.TempDir() + "/lsof-missing"
	t.Setenv("PATH", t.TempDir())
	if _, ok := runLsofTCPPort(1); ok {
		t.Fatal("ran an lsof other than lsofPath")
	}
}
