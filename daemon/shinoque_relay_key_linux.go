package main

import (
	"net/http"
	"os"
	"strconv"
)

// The pushing core is a WSL process on this machine, so its socket traces to
// a pid here. A caller that can't be traced (a Windows process reaching
// loopback through WSL forwarding, a unix socket) is refused.
func shinoqueRelayKeyPeerPlatform(r *http.Request) (int, string, bool) {
	pid, known := lookupPeerPID(r)
	if !known || pid <= 0 || readUID(pid) != os.Getuid() {
		return 0, "", false
	}
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil || exe == "" {
		return 0, "", false
	}
	return pid, exe, true
}
