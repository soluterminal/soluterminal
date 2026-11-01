package main

import "soluterminal-shared/daemonfeatures"

// daemonFeatures is what this build serves. Registration runs during init,
// alongside the optional implementation. The list is sent in the relay
// handshake and /api/health so callers can detect supported capabilities.
var daemonFeatures = []string{daemonfeatures.UIPrefs}

func registerDaemonFeature(name string) {
	for _, existing := range daemonFeatures {
		if existing == name {
			return
		}
	}
	daemonFeatures = append(daemonFeatures, name)
}
