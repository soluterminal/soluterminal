package main

import "runtime"

// Account-wide device policy (ST-250). The cloud applies the strictest
// remote-access policy across an account's computers, and it trusts a
// daemon's policy only for that daemon's own relay tunnel: the relay
// handshake carries it (dp, with the platform as pl, see relay.go), and when a
// person changes it here the relay supervisor redials so the new handshake
// says so (relaySupervisor.policyChanged).

// liveDevicePolicy returns this daemon's policy for the relay handshake. Set
// in main once the trusted-devices store exists; nil sends nothing.
var liveDevicePolicy func() string

// devicePlatform is the closed platform enum the cloud accepts.
func devicePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "darwin"
	case "linux":
		if isInsideWSL() {
			return "linux-wsl"
		}
		return "linux"
	}
	return ""
}

// policyChangeHook is the trusted-devices hook run after a person changes the
// remote-access policy: it resyncs reminder names (reminderSync) and asks the
// relay supervisor to redial so the cloud sees the new policy. It only builds
// the callback; nothing runs until a change.
func (a *apiServer) policyChangeHook(reminderSync func()) func() {
	return func() {
		if reminderSync != nil {
			reminderSync()
		}
		a.cloudSup.policyChanged()
	}
}
