//go:build !windows

package main

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (pr *PortRegistry) scan() {
	pr.scanMu.Lock()
	defer pr.scanMu.Unlock()
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		pr.invalidate("Service discovery is unavailable: this platform does not expose Linux /proc TCP listeners.")
		return
	}
	pr.setWarning("")
	tcp4 := parseProcNetTCP("/proc/net/tcp", "tcp")
	tcp6 := parseProcNetTCP("/proc/net/tcp6", "tcp6")
	all := append(tcp4, tcp6...)

	// Build inode → pid map (via /proc/*/fd/*).
	inodeToPid := buildInodePidMap()

	// Map pids of our live panes to their pane IDs. Used to annotate ports
	// with "this is from pane X" when possible.
	paneByPid := map[int]string{}
	for _, p := range pr.mgr.List() {
		for _, pid := range p.ChildPids() {
			paneByPid[pid] = p.ID
		}
	}

	// Filter to listeners owned by our own UID — excludes root/system
	// services while still catching user-level stuff (other terminals'
	// servers, editor LSPs, etc.). Cheap: we call syscall.Getuid once.
	selfUID := syscall.Getuid()
	selfPID := os.Getpid()

	previous := pr.snapshot()

	fresh := map[string]*Port{}
	for _, e := range all {
		pid := inodeToPid[e.inode]
		if pid == 0 {
			// Couldn't map the socket inode to a pid (common: the owning
			// process's /proc/*/fd/ entries raced closed, or the kernel
			// hasn't exposed them yet). Skip silently — we'll pick it up
			// on the next scan.
			continue
		}
		if pid == selfPID {
			// Don't show the daemon's own listener (:8787).
			continue
		}
		if readUID(pid) != selfUID {
			continue
		}

		paneID := paneByPid[pid]
		key := e.proto + "|" + e.addr + "|" + strconv.Itoa(int(e.port))
		identity := strconv.FormatUint(e.inode, 10)
		if existing, had := previous[key]; had && samePortListener(existing, pid, identity) {
			// Copy, don't mutate: commitLocked diffs fresh against pr.seen
			// to spot ports that just gained pane attribution — updating
			// the shared struct in place would erase the "before".
			cp := *existing
			cp.PaneID = paneID
			cp.Pid = pid
			cp.Process = readComm(pid)
			cp.Cmdline = readCmdline(pid)
			cp.Cwd = readProcCwd(pid)
			fresh[key] = &cp
			continue
		}
		fresh[key] = &Port{
			Port:     e.port,
			Addr:     e.addr,
			Pid:      pid,
			Process:  readComm(pid),
			Cmdline:  readCmdline(pid),
			Cwd:      readProcCwd(pid),
			PaneID:   paneID,
			FirstAt:  time.Now().UTC(),
			Proto:    e.proto,
			identity: identity,
		}
	}
	pr.commitScan(fresh)
}

// readCmdline returns /proc/<pid>/cmdline with null separators replaced by
// spaces. Limited to 512 chars to keep the JSON response small.
func readCmdline(pid int) string {
	if cmd, ok := darwinReadCmdline(pid); ok {
		return cmd
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	// argv elements are NUL-separated, often with a trailing NUL.
	s := strings.TrimRight(string(b), "\x00")
	s = strings.ReplaceAll(s, "\x00", " ")
	if len(s) > 512 {
		s = s[:509] + "..."
	}
	return s
}

// readProcCwd follows the /proc/<pid>/cwd symlink.
func readProcCwd(pid int) string {
	target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	return target
}

// readUID returns the real UID of the given process, or -1 on error.
func readUID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			// "Uid:\treal\teff\tsaved\tfs" — take real.
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, _ := strconv.Atoi(fields[1])
				return n
			}
		}
	}
	return -1
}

type tcpEntry struct {
	proto string
	addr  string
	port  uint16
	inode uint64
}

// parseProcNetTCP returns all sockets in LISTEN state.
func parseProcNetTCP(path, proto string) []tcpEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []tcpEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		// fields[1] = local_addr:port (hex)
		// fields[3] = state hex ("0A" = LISTEN)
		// fields[9] = inode
		if fields[3] != "0A" {
			continue
		}
		addr, port := parseHexAddr(fields[1])
		if port == 0 {
			continue
		}
		inode, _ := strconv.ParseUint(fields[9], 10, 64)
		out = append(out, tcpEntry{proto: proto, addr: addr, port: port, inode: inode})
	}
	return out
}

func parseHexAddr(s string) (string, uint16) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", 0
	}
	addrHex := parts[0]
	portU, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return "", 0
	}
	b, err := hex.DecodeString(addrHex)
	if err != nil {
		return "", 0
	}
	var addr string
	switch len(b) {
	case 4:
		// little-endian
		addr = strconv.Itoa(int(b[3])) + "." + strconv.Itoa(int(b[2])) + "." +
			strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[0]))
	case 16:
		// /proc encodes each 32-bit IPv6 word in host byte order.
		for i := 0; i < 16; i += 4 {
			b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
		}
		addr = net.IP(b).String()
	}
	return addr, uint16(portU)
}

func buildInodePidMap() map[uint64]int {
	out := map[uint64]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			// "socket:[12345]"
			if !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inodeStr := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			inode, err := strconv.ParseUint(inodeStr, 10, 64)
			if err != nil {
				continue
			}
			out[inode] = pid
		}
	}
	return out
}

func readComm(pid int) string {
	if name, ok := darwinReadComm(pid); ok {
		return name
	}
	return providerProcessNameAt("/proc", pid)
}

// PATH shims are symlinks to solud named codex/claude. Linux gives them that
// comm too, although they only wait for the real CLI. Do not count the shim as
// a second foreground provider. Compare executable paths, including the update
// backup ring, so already-running shims remain identifiable after an update.
func providerProcessNameAt(procRoot string, pid int) string {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	comm := strings.TrimSpace(readProcFile(dir, "comm"))
	if comm != "codex" && comm != "claude" {
		return comm
	}
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		return comm
	}
	self, err := os.Readlink(filepath.Join(procRoot, "self", "exe"))
	if err == nil && filepath.IsAbs(exe) && filepath.IsAbs(self) && daemonExecutableBase(exe) == daemonExecutableBase(self) {
		return "solud"
	}
	return comm
}

func daemonExecutableBase(path string) string {
	path = strings.TrimSuffix(path, " (deleted)")
	if at := strings.LastIndex(path, ".old"); at >= 0 {
		base, suffix := path[:at], path[at+len(".old"):]
		if suffix == "" {
			return base
		}
		if strings.HasPrefix(suffix, ".") {
			if n, err := strconv.Atoi(suffix[1:]); err == nil && n >= 0 && strconv.Itoa(n) == suffix[1:] {
				return base
			}
		}
	}
	return path
}
