package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const shinoqueDiscoveryLimit = 4096

func shinoqueDiscoveryPath() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "shinoque", "relay.json")
}

func shinoqueDiscoveryMetadata(info os.FileInfo) (uint64, string) {
	if !info.Mode().IsRegular() {
		return 0, "discovery is not a regular file (symlinks are forbidden)"
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return 0, "discovery is not owned by this uid"
	}
	if info.Mode().Perm() != 0600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return 0, "discovery mode must be 0600"
	}
	if info.Size() > shinoqueDiscoveryLimit {
		return 0, "discovery exceeds size limit"
	}
	return stat.Ino, ""
}

func shinoqueDiscoveryHome(home string) string {
	if !filepath.IsAbs(home) {
		return "home must be absolute"
	}
	info, err := os.Stat(home)
	if err != nil {
		return "home is unavailable"
	}
	if !info.IsDir() {
		return "home is not a directory"
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "home is not owned by this uid"
	}
	return ""
}

// Lstat and home validation run on every request. Open without following links
// and compare the opened inode before reading, closing the lstat/open race.
func (c *shinoqueDiscoveryCache) discover() (shinoqueRelayConfig, bool, string) {
	config, found, reason := c.readDiscovery()
	if reason != "" {
		c.path, c.content, c.config = "", nil, shinoqueRelayConfig{}
	}
	if found && reason == "" && shinoqueSoludListening(config.port) {
		return shinoqueRelayConfig{}, true, "discovery port belongs to solud"
	}
	return config, found, reason
}

// Defense in depth for Linux-visible listeners, checked even on cache hits.
// Inspect only solud descriptors, without HTTP probes or sending the token.
// WSL cannot see native Windows listeners here; stripping browser authority
// headers in the proxy remains necessary on every platform and destination.
func shinoqueSoludListening(port string) bool {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return false
	}
	sockets := make(map[string]bool)
	for _, entry := range append(parseProcNetTCP("/proc/net/tcp", "tcp"), parseProcNetTCP("/proc/net/tcp6", "tcp6")...) {
		if entry.port == uint16(n) {
			sockets["socket:["+strconv.FormatUint(entry.inode, 10)+"]"] = true
		}
	}
	if len(sockets) == 0 {
		return false
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(strings.ToLower(readComm(pid)), ".exe")
		if name != "solud" {
			continue
		}
		dir := filepath.Join("/proc", process.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err == nil && sockets[target] {
				return true
			}
		}
	}
	return false
}

func (c *shinoqueDiscoveryCache) readDiscovery() (shinoqueRelayConfig, bool, string) {
	path := shinoqueDiscoveryPath()
	fail := func(reason string) (shinoqueRelayConfig, bool, string) { return shinoqueRelayConfig{}, true, reason }
	if path == "" {
		return fail("state directory is unavailable")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		c.path, c.content, c.config = "", nil, shinoqueRelayConfig{}
		return shinoqueRelayConfig{}, false, ""
	}
	if err != nil {
		return fail("cannot stat discovery")
	}
	_, reason := shinoqueDiscoveryMetadata(info)
	if reason != "" {
		return fail(reason)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail("cannot open discovery")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return fail("cannot stat opened discovery")
	}
	if _, reason := shinoqueDiscoveryMetadata(opened); reason != "" {
		return fail(reason)
	}
	if !os.SameFile(info, opened) || !info.ModTime().Equal(opened.ModTime()) || info.Size() != opened.Size() {
		return fail("discovery changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, shinoqueDiscoveryLimit+1))
	if err != nil {
		return fail("cannot read discovery")
	}
	if len(content) > shinoqueDiscoveryLimit {
		return fail("discovery exceeds size limit")
	}
	after, err := file.Stat()
	if err != nil || !after.ModTime().Equal(opened.ModTime()) || after.Size() != opened.Size() {
		return fail("discovery changed while reading")
	}
	if _, reason := shinoqueDiscoveryMetadata(after); reason != "" {
		return fail(reason)
	}
	// Reuse parsed discovery only after reading and validating the current file.
	// Different contents can share the same inode, size and mtime.
	if c.path == path && bytes.Equal(c.content, content) {
		return c.config, true, shinoqueDiscoveryHome(c.config.home)
	}
	record, err := parseShinoqueDiscovery(content)
	if err != nil {
		return fail("invalid discovery JSON")
	}
	if record.Version != 1 {
		return fail("unsupported discovery version")
	}
	if record.Port < 1 || record.Port > 65535 {
		return fail("discovery port must be from 1 to 65535")
	}
	if reason := shinoqueDiscoveryHome(record.Home); reason != "" {
		return fail(reason)
	}
	config := shinoqueRelayConfig{port: strconv.Itoa(record.Port), home: record.Home, relayAuth: record.RelayAuth, privateDeviceProof: record.PrivateDeviceProof}
	c.path, c.content, c.config = path, content, config
	return config, true, ""
}

// Accepted fields are version, port, home, relayAuth and privateDeviceProof.
// Optional capability versions must be JSON integers; absent fields are zero.
type shinoqueDiscoveryRecord struct {
	Version, Port, RelayAuth, PrivateDeviceProof int
	Home                                         string
}

func parseShinoqueDiscovery(content []byte) (shinoqueDiscoveryRecord, error) {
	var record shinoqueDiscoveryRecord
	invalid := errors.New("invalid discovery record")
	decoder := json.NewDecoder(bytes.NewReader(content))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return record, invalid
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return record, invalid
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return record, invalid
		}
		seen[name] = true
		var value any
		switch name {
		case "version":
			value = &record.Version
		case "port":
			value = &record.Port
		case "home":
			value = &record.Home
		case "relayAuth":
			value = &record.RelayAuth
		case "privateDeviceProof":
			value = &record.PrivateDeviceProof
		default:
			return record, invalid
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return record, invalid
		}
		if err := json.Unmarshal(raw, value); err != nil {
			return record, invalid
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return record, invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return record, invalid
	}
	return record, nil
}
