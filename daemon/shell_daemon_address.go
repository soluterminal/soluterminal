package main

import (
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

// Retained pane shells can outlive the daemon that supplied their address.
// Discover only the owner of this shim's data directory, never a global port
// or another checkout. Unlocked metadata is historical, not discovery evidence.
func shellDaemonEnvironment(env []string) []string {
	old := envValueFromList(env, paneDaemonURLEnv)
	if _, err := codexShellDaemonURL(old); err != nil {
		return env
	}
	shim := envValueFromList(env, codexInlineShimEnvKey)
	if !filepath.IsAbs(shim) || filepath.Base(shim) != codexInlineShimDirName {
		return env
	}
	f, err := os.Open(filepath.Join(filepath.Dir(shim), dataDirLockName))
	if err != nil {
		return env
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return env
	}
	var holder dataDirLockHolder
	decoder := json.NewDecoder(io.LimitReader(f, 4097))
	if decoder.Decode(&holder) != nil || decoder.Decode(new(any)) != io.EOF || holder.PID <= 0 {
		return env
	}
	if !shellDaemonLockHeld(f, holder.PID) {
		return env
	}
	host, port, err := net.SplitHostPort(holder.Addr)
	if err != nil {
		return env
	}
	switch host {
	case "", "0.0.0.0", "127.0.0.1":
		host = "127.0.0.1"
	case "::", "::1":
		host = "::1"
	default:
		return env
	}
	address := (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}).String()
	if _, err := codexShellDaemonURL(address); err != nil || address == old {
		return env
	}
	return append(filterOutEnvKeys(env, []string{paneDaemonURLEnv}), paneDaemonURLEnv+"="+address)
}
