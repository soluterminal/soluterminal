// Package helperproto implements the helper stdio protocol v1 (ST-253, A′):
// the wire between the Windows host and the stateless `solud helper` it starts
// in each WSL distro. The spec is docs/design/one-solud/helper-protocol-v1.md
// (frozen 20 Nov 2026; this package follows its sha256 f5c7d7ce revision).
//
// One yamux session runs over the helper's stdin and stdout. The host opens a
// control stream first, then one agent stream per pane. The helper opens no
// stream in v1: fwd (M3) and scan (M4) are reserved and each needs its own
// @secure ruling before a handler is registered for it.
//
// This package holds only the wire: framing, the control codec, identifier
// checks, limits and the two session ends. Spawning agents (the helper) and
// placing and supervising the helper (the host) live elsewhere and plug in
// through the Helper interface and HostOptions.
package helperproto

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"

	"github.com/hashicorp/yamux"
)

// Proto is the protocol version both ends speak.
const Proto = 1

// Limits (ruling 8.5).
const (
	// MaxLine is the longest control line or stream header, newline excluded.
	// A longer line is a protocol break.
	MaxLine = 64 << 10
	// MaxStreams is the most streams open at once in one session, the control
	// stream included (@secure, 20 Nov: 256, one pane is one stream).
	MaxStreams = 256
	// ListLimit is the default and largest page `list` returns.
	ListLimit = 128
	// HeaderTimeout bounds how long a newly opened stream may take to send its
	// first line.
	HeaderTimeout = 10 * time.Second
)

// YamuxConfig is the session config both ends use. Every field the spec names
// is set explicitly, so a yamux default changing upstream cannot change it.
func YamuxConfig(logOutput io.Writer) *yamux.Config {
	if logOutput == nil {
		logOutput = io.Discard
	}
	return &yamux.Config{
		AcceptBacklog:          MaxStreams,
		EnableKeepAlive:        true,
		KeepAliveInterval:      15 * time.Second,
		ConnectionWriteTimeout: 10 * time.Second,
		MaxStreamWindowSize:    256 << 10,
		StreamOpenTimeout:      30 * time.Second,
		StreamCloseTimeout:     5 * time.Minute,
		LogOutput:              logOutput,
	}
}

// Stream kinds: the "s" field of a stream's first line.
const (
	KindControl = "control" // host-opened, exactly one, first
	KindAgent   = "agent"   // host-opened, raw agentproto bytes
	KindFwd     = "fwd"     // helper-opened, reserved for M3
	KindScan    = "scan"    // host-opened, reserved for M4
)

// Control ops.
const (
	OpHello    = "hello"
	OpSpawn    = "spawn"
	OpList     = "list"
	OpCwd      = "cwd"
	OpKill     = "kill"
	OpReap     = "reap"
	OpDrain    = "drain"
	OpShutdown = "shutdown"
)

// Error codes.
const (
	CodeUnsupported      = "unsupported"
	CodeBadArgs          = "bad_args"
	CodeNotFound         = "not_found"
	CodeSpawnFailed      = "spawn_failed"
	CodeBusy             = "busy"
	CodeUnsafeRuntimeDir = "unsafe_runtime_dir"
	CodeInternal         = "internal"
)

// Networking modes reported by hello.
const (
	NetworkingNAT      = "nat"
	NetworkingMirrored = "mirrored"
	NetworkingUnknown  = "unknown"
)

// HelloArgs is the host's first request.
type HelloArgs struct {
	Proto       int    `json:"proto"`
	HostVersion string `json:"hostVersion"`
	HostPid     int    `json:"hostPid"`
	Distro      string `json:"distro"`
}

// HelloResult describes the helper and the distro user it runs as.
type HelloResult struct {
	Proto         int    `json:"proto"`
	HelperVersion string `json:"helperVersion"`
	Pid           int    `json:"pid"`
	UID           int    `json:"uid"`
	User          string `json:"user"`
	Home          string `json:"home"`
	Arch          string `json:"arch"`
	Kernel        string `json:"kernel"`
	Networking    string `json:"networking"`
	RuntimeDir    string `json:"runtimeDir"`
}

// SpawnArgs asks the helper to start one pane as a detached `solud agent`.
// Env is the full set to add; the host never puts its own secrets in it
// (ruling 8.4). Provider must be empty in v1: GLM reads its token from a data
// dir the distro doesn't have.
type SpawnArgs struct {
	PaneID   string            `json:"paneId"`
	Argv     []string          `json:"argv"`
	Cwd      string            `json:"cwd"`
	Env      map[string]string `json:"env"`
	Cols     int               `json:"cols"`
	Rows     int               `json:"rows"`
	NoPty    bool              `json:"noPty"`
	Provider string            `json:"provider"`
}

// SpawnResult names the new agent. StartTicks is field 22 of
// /proc/<pid>/stat, read right after spawn; with Pid it is the agent's
// process identity.
type SpawnResult struct {
	AgentID    string `json:"agentId"`
	Pid        int    `json:"pid"`
	StartTicks uint64 `json:"startTicks"`
}

// ListArgs pages through the helper's agents, sorted by agentId.
type ListArgs struct {
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// ListResult is one page. Next is the last agentId when more remain.
type ListResult struct {
	Agents []AgentEntry `json:"agents"`
	Next   string       `json:"next,omitempty"`
}

// AgentEntry is one agent the helper knows from its runtime dir. Alive means
// the process identity still holds: the start time matches StartTicks, the
// cmdline is this agent's `solud agent --socket …`, and the uid is the
// helper's. A record without StartTicks is never alive.
type AgentEntry struct {
	AgentID    string `json:"agentId"`
	PaneID     string `json:"paneId"`
	Pid        int    `json:"pid"`
	StartTicks uint64 `json:"startTicks"`
	Alive      bool   `json:"alive"`
}

// AgentRef names one agent (cwd, reap).
type AgentRef struct {
	AgentID string `json:"agentId"`
}

// CwdResult is the agent's live working directory ("" when unknown).
type CwdResult struct {
	Cwd string `json:"cwd"`
}

// Kill methods.
const (
	KillTerm = "term"
	KillKill = "kill"
)

// KillArgs signals an agent that no longer answers on its socket.
type KillArgs struct {
	AgentID string `json:"agentId"`
	How     string `json:"how"`
}

// Empty is the result of ops that return nothing.
type Empty struct{}

// AgentInfo is what the helper knows about an agent it spawned. StartTicks is
// the agent's /proc start time; with Pid it names the process without
// pid-reuse ambiguity.
type AgentInfo struct {
	AgentID    string
	PaneID     string
	Pid        int
	StartTicks uint64
}

// AgentRegistry looks up the agent a process belongs to, by the agent's own
// pid. The helper implements it from its runtime dir; M3's caller attestation
// walks a peer's parents and asks it at each step.
type AgentRegistry interface {
	AgentByPid(pid int) (AgentInfo, bool)
}

// ValidAgentID reports whether s is a random 128-bit lowercase hex agent id
// (ruling 8.3). Only a valid id may be used in a path or socket name.
func ValidAgentID(s string) bool { return isLowerHex(s, 32) }

// ValidPaneID reports whether s has the host's pane-id format, pane_ plus 16
// lowercase hex digits (randomID in pane_spawn.go).
func ValidPaneID(s string) bool {
	const prefix = "pane_"
	return len(s) == len(prefix)+16 && s[:len(prefix)] == prefix && isLowerHex(s[len(prefix):], 16)
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// NewAgentID returns a fresh random agent id.
func NewAgentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
