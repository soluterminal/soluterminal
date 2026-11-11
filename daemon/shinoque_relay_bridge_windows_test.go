//go:build windows

package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The helper process listens on loopback and prints its port; it stands in
// for a desktop proxy with a chosen image name.
func TestShinoqueRelayBridgeHelperListener(t *testing.T) {
	if os.Getenv("SHINOQUE_RELAY_HELPER") != "1" {
		t.Skip("helper process only")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString(listener.Addr().String() + "\n")
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, conn)
		}
	}()
	io.Copy(io.Discard, os.Stdin) // exit when the parent closes stdin
	os.Exit(0)
}

// startRelayHelper runs a copy of the test binary named name and returns the
// address it listens on.
func startRelayHelper(t *testing.T, name string) (string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(t.TempDir(), name)
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, data, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(image, "-test.run=^TestShinoqueRelayBridgeHelperListener$")
	cmd.Env = append(os.Environ(), "SHINOQUE_RELAY_HELPER=1")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	return strings.TrimSpace(line), image
}

func relayExpectImage(t *testing.T, image string, ok bool) {
	t.Helper()
	previous := shinoqueRelayExpectedImage
	shinoqueRelayExpectedImage = func() (string, bool) {
		if !ok {
			return "", false
		}
		return shinoqueRelayLongPath(image)
	}
	t.Cleanup(func() { shinoqueRelayExpectedImage = previous })
}

func relayDial(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestShinoqueRelayDesktopOwnerOnWindows(t *testing.T) {
	address, image := startRelayHelper(t, "Shinoque.exe")

	relayExpectImage(t, image, true)
	owner, reason := shinoqueRelayDesktopOwner(relayDial(t, address))
	if reason != "" || owner.pid <= 0 || owner.pid == os.Getpid() || owner.created == 0 {
		t.Fatalf("owner = %+v, %q; want the helper process", owner, reason)
	}
	if !strings.EqualFold(filepath.Base(owner.image), "Shinoque.exe") {
		t.Fatalf("image = %q", owner.image)
	}
	// The same check, case changed: the compare is case-insensitive (C2).
	relayExpectImage(t, strings.ToUpper(image), true)
	if _, reason := shinoqueRelayDesktopOwner(relayDial(t, address)); reason != "" {
		t.Fatalf("upper-case expected path refused: %s", reason)
	}

	// Another install path, an unresolvable one, and our own listener refuse.
	relayExpectImage(t, filepath.Join(t.TempDir(), "Shinoque.exe"), true)
	if _, reason := shinoqueRelayDesktopOwner(relayDial(t, address)); reason == "" {
		t.Fatal("helper at another path admitted")
	}
	relayExpectImage(t, image, false)
	if _, reason := shinoqueRelayDesktopOwner(relayDial(t, address)); reason == "" {
		t.Fatal("unresolvable install path admitted")
	}
	own, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	go func() {
		if conn, err := own.Accept(); err == nil {
			defer conn.Close()
			io.Copy(io.Discard, conn)
		}
	}()
	relayExpectImage(t, image, true)
	if _, reason := shinoqueRelayDesktopOwner(relayDial(t, own.Addr().String())); reason == "" {
		t.Fatal("a listener of another image admitted")
	}
}

func TestShinoqueRelayDesktopOwnerRefusesSolud(t *testing.T) {
	address, image := startRelayHelper(t, "solud.exe")
	relayExpectImage(t, image, true) // even if it were the pinned path
	if _, reason := shinoqueRelayDesktopOwner(relayDial(t, address)); reason != "owner is solud" {
		t.Fatalf("reason = %q, want owner is solud", reason)
	}
}

// relayAddrConn reports the addresses it is given and carries nothing.
type relayAddrConn struct {
	net.Conn
	local, remote net.Addr
}

func (c relayAddrConn) LocalAddr() net.Addr  { return c.local }
func (c relayAddrConn) RemoteAddr() net.Addr { return c.remote }

// A connection with no row in the TCP table has no owner to read, and an
// owner renamed while it runs is refused: Windows reports its new path, not
// the pinned one (@secure C2, C6). An owner image that can't be resolved
// at all is shinoqueRelayLongPath returning false; TestShinoqueRelayLongPathForms
// covers that with missing and relative paths.
func TestShinoqueRelayDesktopOwnerUnreadable(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// The listening row's remote end is 0.0.0.0:0, so no row has this pair.
	conn := relayAddrConn{
		local:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		remote: listener.Addr(),
	}
	if _, reason := shinoqueRelayDesktopOwner(conn); reason != "cannot read the desktop route's owner" {
		t.Fatalf("reason = %q, want cannot read the desktop route's owner", reason)
	}

	address, image := startRelayHelper(t, "Shinoque.exe")
	moved := filepath.Join(filepath.Dir(image), "Moved.exe")
	if err := os.Rename(image, moved); err != nil {
		t.Fatal(err)
	}
	// Windows reports the path the image has now.
	relayExpectImage(t, moved, true)
	owner, reason := shinoqueRelayDesktopOwner(relayDial(t, address))
	want, _ := shinoqueRelayLongPath(moved)
	if reason != "" || !strings.EqualFold(owner.image, want) {
		t.Fatalf("owner = %+v, %q; want the helper reported at %s", owner, reason, want)
	}
	// With a real Shinoque.exe back at the pinned path, the renamed owner is
	// still not it.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, data, 0755); err != nil {
		t.Fatal(err)
	}
	relayExpectImage(t, image, true)
	if owner, reason := shinoqueRelayDesktopOwner(relayDial(t, address)); reason != "owner is not the Shinoque app" {
		t.Fatalf("renamed owner = %+v, %q; want owner is not the Shinoque app", owner, reason)
	}
}

func TestShinoqueRelayLongPathForms(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	long, ok := shinoqueRelayLongPath(self)
	if !ok {
		t.Fatal("own image unresolvable")
	}
	if got, ok := shinoqueRelayLongPath(`\\?\` + self); !ok || got != long {
		t.Fatalf(`\\?\ form = %q %v, want %q`, got, ok, long)
	}
	in, _ := windows.UTF16PtrFromString(self)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if n, err := windows.GetShortPathName(in, &buf[0], uint32(len(buf))); err == nil && n > 0 {
		if got, ok := shinoqueRelayLongPath(windows.UTF16ToString(buf[:n])); !ok || !strings.EqualFold(got, long) {
			t.Fatalf("8.3 form = %q %v, want %q", got, ok, long)
		}
	}
	if _, ok := shinoqueRelayLongPath(filepath.Join(t.TempDir(), "missing.exe")); ok {
		t.Fatal("missing path resolved")
	}
	if _, ok := shinoqueRelayLongPath("relative.exe"); ok {
		t.Fatal("relative path resolved")
	}
}
