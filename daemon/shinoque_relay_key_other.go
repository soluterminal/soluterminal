//go:build !linux && !windows

package main

import "net/http"

// Installed discovery, and so the relay to a core, is Linux/WSL only.
func shinoqueRelayKeyPeerPlatform(*http.Request) (int, string, bool) { return 0, "", false }
