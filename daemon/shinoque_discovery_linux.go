package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
		c.path, c.config = "", shinoqueRelayConfig{}
	}
	return config, found, reason
}

func (c *shinoqueDiscoveryCache) readDiscovery() (shinoqueRelayConfig, bool, string) {
	path := shinoqueDiscoveryPath()
	fail := func(reason string) (shinoqueRelayConfig, bool, string) { return shinoqueRelayConfig{}, true, reason }
	if path == "" {
		return fail("state directory is unavailable")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		c.path = ""
		return shinoqueRelayConfig{}, false, ""
	}
	if err != nil {
		return fail("cannot stat discovery")
	}
	inode, reason := shinoqueDiscoveryMetadata(info)
	if reason != "" {
		return fail(reason)
	}
	if c.path == path && c.mtime.Equal(info.ModTime()) && c.size == info.Size() && c.inode == inode {
		return c.config, true, shinoqueDiscoveryHome(c.config.home)
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
	var record struct {
		Version int    `json:"version"`
		Port    int    `json:"port"`
		Home    string `json:"home"`
	}
	// Never include parse errors or contents in logs: discovery is untrusted.
	if err := json.Unmarshal(content, &record); err != nil {
		return fail("invalid discovery JSON (version and port must be integers)")
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
	config := shinoqueRelayConfig{port: strconv.Itoa(record.Port), home: record.Home}
	c.path, c.mtime, c.size, c.inode, c.config = path, info.ModTime(), info.Size(), inode, config
	return config, true, ""
}
