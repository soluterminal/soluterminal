//go:build windows

package main

import (
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// Windows relays through the Shinoque desktop (shinoque_relay_bridge.go).
func newPlatformShinoqueRelayBridge() *shinoqueRelayBridge { return newShinoqueRelayBridge() }

// The pusher is the desktop carrying the core's push; see
// shinoqueRelayDesktopProcess for what it must be.
func shinoqueRelayKeyPeerPlatform(r *http.Request) (int, string, bool) {
	pid, known := lookupPeerPID(r)
	if !known || pid <= 0 {
		return 0, "", false
	}
	owner, reason := shinoqueRelayDesktopProcess(pid)
	if reason != "" {
		return 0, "", false
	}
	return owner.pid, owner.image, true
}

// shinoqueRelayDesktopOwner checks the server end of a connection solud just
// opened to the desktop's proxy, before anything is written on it. The table
// row of a connected socket shows it until it closes, so a short wait covers
// only the moment before Windows adds it.
func shinoqueRelayDesktopOwner(conn net.Conn) (shinoqueRelayOwner, string) {
	client, ok := conn.LocalAddr().(*net.TCPAddr)
	server, ok2 := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || !ok2 || !server.IP.IsLoopback() {
		return shinoqueRelayOwner{}, "desktop route is not a loopback TCP connection"
	}
	for attempt := 0; attempt < 3; attempt++ {
		// The row whose local end is the server's names the server's process.
		if pid, found := lookupPeerPIDPlatform(server, client); found {
			return shinoqueRelayDesktopProcess(pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return shinoqueRelayOwner{}, "cannot read the desktop route's owner"
}

// shinoqueRelayDesktopProcess admits only the Shinoque desktop app running as
// this Windows user: the SID is the boundary; the install path is hygiene,
// since %LOCALAPPDATA% is user-writable (@secure C2). One handle answers
// every question, so a reused pid cannot split them.
func shinoqueRelayDesktopProcess(pid int) (shinoqueRelayOwner, string) {
	refuse := func(reason string) (shinoqueRelayOwner, string) { return shinoqueRelayOwner{}, reason }
	self, err := currentUserSID()
	if err != nil {
		return refuse("cannot read solud's user")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return refuse("cannot open the owner")
	}
	defer windows.CloseHandle(h)
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return refuse("cannot read the owner's user")
	}
	user, err := token.GetTokenUser()
	token.Close()
	if err != nil {
		return refuse("cannot read the owner's user")
	}
	if !windows.EqualSid(user.User.Sid, self) {
		return refuse("owner runs as another Windows user")
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return refuse("cannot read the owner's image")
	}
	image, ok := shinoqueRelayLongPath(windows.UTF16ToString(buf[:size]))
	if !ok {
		return refuse("cannot resolve the owner's image")
	}
	if strings.EqualFold(filepath.Base(image), "solud.exe") {
		return refuse("owner is solud")
	}
	expected, ok := shinoqueRelayExpectedImage()
	if !ok {
		return refuse("cannot resolve the Shinoque install path")
	}
	if !strings.EqualFold(image, expected) {
		return refuse("owner is not the Shinoque app")
	}
	var created, exited, kernel, userTime windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &userTime); err != nil ||
		exited.HighDateTime != 0 || exited.LowDateTime != 0 {
		return refuse("cannot read the owner's start time")
	}
	start := int64(created.HighDateTime)<<32 | int64(created.LowDateTime)
	if start == 0 {
		return refuse("cannot read the owner's start time")
	}
	return shinoqueRelayOwner{pid: pid, created: start, image: image}, ""
}

// shinoqueRelayExpectedImage is the image a desktop owner must run; a seam
// for tests, which stand the test binary in for Shinoque.exe.
var shinoqueRelayExpectedImage = shinoqueRelayDesktopImage

// shinoqueRelayDesktopImage is the per-user install path of Shinoque.exe.
// The known folder, not %LOCALAPPDATA%, which solud's environment could change.
func shinoqueRelayDesktopImage() (string, bool) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || local == "" {
		return "", false
	}
	return shinoqueRelayLongPath(filepath.Join(local, "Shinoque", "Shinoque.exe"))
}

// shinoqueRelayLongPath is the canonical long form for comparing images: 8.3
// names expanded and any \\?\ prefix dropped. A path that can't be resolved
// (it no longer exists, for one) refuses.
func shinoqueRelayLongPath(path string) (string, bool) {
	path = strings.TrimPrefix(path, `\\?\`)
	if !filepath.IsAbs(path) {
		return "", false
	}
	in, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(in, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) >= len(buf) {
		return "", false
	}
	return filepath.Clean(strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`)), true
}
