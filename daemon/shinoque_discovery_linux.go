package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// Check both tables on every relay request. Only a missing tcp6 table (IPv6
// disabled at boot) is optional; other missing or malformed proc data refuses.
func shinoqueLocalListenerAllowed(port string) bool {
	reason := shinoqueLocalListenerRejection(port, os.Getuid(), os.ReadFile)
	if reason != "" {
		log.Printf("shinoque relay: listener check refused port=%q: %s", port, reason)
	}
	return reason == ""
}

func shinoqueLocalListenerOwned(port string, uid int, readFile func(string) ([]byte, error)) bool {
	return shinoqueLocalListenerRejection(port, uid, readFile) == ""
}

func shinoqueLocalListenerRejection(port string, uid int, readFile func(string) ([]byte, error)) string {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "invalid listener port"
	}
	ours := false
	for _, table := range []struct {
		path      string
		addrWidth int
	}{{"/proc/net/tcp", 8}, {"/proc/net/tcp6", 32}} {
		content, err := readFile(table.path)
		if err != nil {
			if table.path == "/proc/net/tcp6" && errors.Is(err, syscall.ENOENT) {
				continue
			}
			return fmt.Sprintf("cannot read %s: %v", table.path, err)
		}
		line := 1
		reject := func(reason string) string {
			return fmt.Sprintf("%s line %d: %s", table.path, line, reason)
		}
		sc := bufio.NewScanner(bytes.NewReader(content))
		if !sc.Scan() {
			return reject("missing or unreadable header")
		}
		// The IPv4 header calls the remote column rem_address; IPv6 spells
		// it remote_address. Queue and timer labels span two header fields.
		header := strings.ReplaceAll(strings.Join(strings.Fields(sc.Text()), " "), "rem_address", "remote_address")
		if header != "sl local_address remote_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode" {
			return reject("invalid header")
		}
		for sc.Scan() {
			line++
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 {
				return reject("incomplete socket row")
			}
			if !strings.HasSuffix(fields[0], ":") {
				return reject("invalid socket slot")
			}
			if _, err := strconv.ParseUint(strings.TrimSuffix(fields[0], ":"), 10, 64); err != nil {
				return reject("invalid socket slot")
			}
			addr, localPort := parseHexAddr(fields[1])
			remote, _ := parseHexAddr(fields[2])
			if len(fields[1]) != table.addrWidth+5 || len(fields[2]) != table.addrWidth+5 || addr == "" || remote == "" {
				return reject("invalid socket address")
			}
			state, stateErr := strconv.ParseUint(fields[3], 16, 8)
			owner, ownerErr := strconv.ParseUint(fields[7], 10, 32)
			_, inodeErr := strconv.ParseUint(fields[9], 10, 64)
			_, timeoutErr := strconv.ParseUint(fields[8], 10, 64)
			if len(fields[3]) != 2 || stateErr != nil || ownerErr != nil || inodeErr != nil || timeoutErr != nil {
				return reject("invalid socket state, uid, timeout or inode")
			}
			// Validate packed queue/timer fields and the retransmit counter,
			// including rows unrelated to this port, rather than skipping errors.
			for i, widths := range [][]int{{8, 8}, {2, 8}, {8}} {
				parts := strings.Split(fields[4+i], ":")
				if len(parts) != len(widths) {
					return reject("invalid queue, timer or retransmit field")
				}
				for j, width := range widths {
					if _, err := strconv.ParseUint(parts[j], 16, 32); err != nil || len(parts[j]) != width {
						return reject("invalid queue, timer or retransmit field")
					}
				}
			}
			if state != 0x0a || uint64(localPort) != n {
				continue
			}
			// Any other uid listening on this port makes the destination
			// ambiguous, even if one of our listeners was already found.
			if owner != uint64(uid) {
				return reject(fmt.Sprintf("listener belongs to another uid (%d)", owner))
			}
			// Only the IPv4 table proves ownership of the proxy's 127.0.0.1
			// destination. IPv6 rows still veto other uids above, but cannot
			// qualify: V6ONLY and WSL forwarding are not visible in proc.
			if table.path == "/proc/net/tcp" && (addr == "127.0.0.1" || addr == "0.0.0.0") {
				ours = true
			}
		}
		if sc.Err() != nil {
			return reject("cannot scan socket table")
		}
	}
	if !ours {
		return "no owned IPv4 listener on 127.0.0.1 or 0.0.0.0"
	}
	return ""
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
