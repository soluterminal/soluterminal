package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Port is one detected listening TCP socket.
type Port struct {
	Port          uint16    `json:"port"`
	Addr          string    `json:"addr"` // e.g. "127.0.0.1"
	Pid           int       `json:"pid"`
	Process       string    `json:"process"` // basename of the owning process's executable
	Cmdline       string    `json:"cmdline"` // full argv, space-joined (truncated)
	Cwd           string    `json:"cwd"`     // working directory of the owning process
	PaneID        string    `json:"paneId,omitempty"`
	FirstAt       time.Time `json:"firstAt"`
	Proto         string    `json:"proto"`               // "tcp" or "tcp6"
	Scheme        string    `json:"scheme,omitempty"`    // confirmed HTTP transport
	WebStatus     string    `json:"webStatus,omitempty"` // ready, unverified, internal
	PreviewPath   string    `json:"previewPath,omitempty"`
	PaneTitle     string    `json:"paneTitle,omitempty"`
	Workspace     string    `json:"workspace,omitempty"`
	identity      string
	checkedAt     time.Time
	probeAttempts uint8
}

type PortRegistry struct {
	mgr    *Manager
	scanMu sync.Mutex

	mu         sync.RWMutex
	seen       map[string]*Port // key: proto|addr|port
	warning    string
	transports map[string]*http.Transport

	// onPaneServerUp, when set (before Start — the field is read from the
	// scan goroutine), fires for stable, identifiable preview tools owned
	// by a pane. Unknown and internal listeners remain inventory entries.
	// main.go wires it to the UI bus so clients can offer one-click preview.
	// The very first scan is silent — on daemon boot everything is "new".
	onPaneServerUp func(p Port)
	scannedOnce    bool
	notices        map[string]portNotice
	noticeAt       time.Time
	noticeScan     uint64
}

func NewPortRegistry(m *Manager) *PortRegistry {
	requireNormalBootEngine("ports.NewPortRegistry")
	return &PortRegistry{mgr: m, seen: map[string]*Port{}}
}

func (pr *PortRegistry) Start(interval time.Duration) {
	requireNormalBootEngine("ports.Start")
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		pr.scan()
		for range t.C {
			pr.scan()
		}
	}()
}

func (pr *PortRegistry) List() []*Port {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	out := make([]*Port, 0, len(pr.seen))
	for _, p := range pr.seen {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		if out[i].Addr != out[j].Addr {
			return out[i].Addr < out[j].Addr
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

func (pr *PortRegistry) snapshot() map[string]*Port {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	out := make(map[string]*Port, len(pr.seen))
	for key, p := range pr.seen {
		out[key] = p
	}
	return out
}

func (pr *PortRegistry) setWarning(message string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.warning = message
}
func (pr *PortRegistry) Warning() string { pr.mu.RLock(); defer pr.mu.RUnlock(); return pr.warning }

func (pr *PortRegistry) invalidate(message string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.warning = message
	pr.commitLocked(map[string]*Port{})
}

// Include the listener lifetime, not just its port. An old browser must never
// silently attach to a different application that reused the same address.
func portSelector(p *Port) string {
	identity := p.identity
	if identity == "" {
		identity = fmt.Sprintf("legacy:%d:%d", p.Pid, p.FirstAt.UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%s|%d|%s", p.Proto, p.Addr, p.Port, identity)))
}

// A Unix socket may be shared by a reloader and its workers. The process
// holding a descriptor is metadata; the kernel socket identity owns lifetime.
func samePortListener(existing *Port, pid int, identity string) bool {
	if identity != "" && existing.identity != "" {
		return existing.identity == identity
	}
	return existing.Pid == pid && existing.identity == identity
}

func portHost(p *Port) string {
	addr := p.Addr
	if addr == "" || addr == "0.0.0.0" {
		addr = "127.0.0.1"
	}
	if addr == "::" {
		addr = "::1"
	}
	return net.JoinHostPort(addr, strconv.Itoa(int(p.Port)))
}

// Resolve only registry-owned sockets. The selector is an identity, never an
// arbitrary proxy destination. Legacy numeric URLs prefer IPv4 loopback.
func (pr *PortRegistry) resolve(port uint16, selector string) *Port {
	var selected *Port
	for _, p := range pr.List() {
		if p.Port == port && p.PaneID != "" && (selector == "" || portSelector(p) == selector) {
			if selector != "" {
				return p
			}
			if selected != nil && (selected.Pid != p.Pid || selected.Scheme != p.Scheme) {
				return nil
			}
			if selected == nil {
				selected = p
			}
		}
	}
	return selected
}

// Has returns true if the registry currently knows a pane-owned
// listening socket on the given port (on any local address). Used as
// the allowlist gate for the reverse-proxy endpoint, while List still
// reports all same-user ports the scanner can see.
func (pr *PortRegistry) Has(port uint16) bool {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	for _, p := range pr.seen {
		if p.Port == port && p.PaneID != "" {
			return true
		}
	}
	return false
}

// commitLocked preserves all inventory entries and emits a bounded set of
// stable preview notices. Caller holds pr.mu. Callbacks run outside the lock.
func (pr *PortRegistry) commitLocked(fresh map[string]*Port) {
	if len(pr.transports) > 0 {
		live := map[string]bool{}
		for _, p := range fresh {
			live[portSelector(p)] = true
		}
		for key, transport := range pr.transports {
			if !live[key] {
				transport.CloseIdleConnections()
				delete(pr.transports, key)
			}
		}
	}
	now := time.Now()
	callbacks := pr.previewNotices(fresh, now)
	pr.scannedOnce = true
	pr.seen = fresh
	if pr.onPaneServerUp != nil {
		for _, p := range callbacks {
			go pr.onPaneServerUp(p)
		}
	}
}

// previewNotices keeps automatic notices conservative. Unknown services remain
// in the port inventory; discovery never sends network traffic to identify them.
func (pr *PortRegistry) previewNotices(fresh map[string]*Port, now time.Time) []Port {
	if pr.notices == nil {
		pr.notices = map[string]portNotice{}
	}
	pr.noticeScan++
	keys := make([]string, 0, len(fresh))
	for key := range fresh {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []Port
	for _, key := range keys {
		p := fresh[key]
		if p.PaneID == "" {
			continue
		}
		id := fmt.Sprintf("%s|%d", p.PaneID, p.Port)
		n, exists := pr.notices[id]
		if !exists || n.pid != p.Pid {
			if !exists && len(pr.notices) >= 256 {
				continue
			}
			n = portNotice{pid: p.Pid, since: now, quiet: !pr.scannedOnce || (!n.lastToast.IsZero() && now.Sub(n.lastToast) < time.Minute), lastToast: n.lastToast}
		}
		if n.scan+1 < pr.noticeScan {
			n.since = now
		}
		n.scan = pr.noticeScan
		n.lastSeen = now
		if !n.quiet && now.Sub(n.since) >= 5*time.Second {
			// One notice per scan, and never retry a transient listener forever.
			if now.Sub(n.since) >= 30*time.Second {
				n.quiet = true
			} else if label := previewProcessLabel(p); label != "" && p.WebStatus == "ready" && (pr.noticeAt.IsZero() || now.Sub(pr.noticeAt) >= 5*time.Second) {
				n.quiet = true
				n.lastToast = now
				pr.noticeAt = now
				cp := *p
				cp.Process = label
				out = append(out, cp)
			}
		}
		pr.notices[id] = n
	}
	// Expire absent entries after observing this scan, so a delayed scan never
	// forgets a still-running baseline or an already-announced listener.
	for id, n := range pr.notices {
		if n.scan != pr.noticeScan && now.Sub(n.lastSeen) >= time.Minute {
			delete(pr.notices, id)
		}
	}
	return out
}

type portNotice struct {
	pid                        int
	scan                       uint64
	since, lastSeen, lastToast time.Time
	quiet                      bool
}

// scan() is defined per-OS: ports_unix.go reads /proc/net/tcp and
// /proc/*/fd/*, ports_windows.go asks iphlpapi's GetExtendedTcpTable.
// Both build a fresh map and hand it to commitLocked.
