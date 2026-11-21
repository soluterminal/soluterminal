package helperproto

// FwdAttest is the `attest` object on a `fwd` stream's first line
// (helper-protocol-v1, "Proposed for M3 fwd"; @secure ruling 8.6a). The
// helper fills it from kernel facts about the process that connected to its
// in-distro socket:
//   - PID and UID from SO_PEERCRED;
//   - Start, that process's /proc start ticks;
//   - Agent, the spawned agent its parent walk reached, matched by the
//     record's pid and startTicks.
//
// The host accepts it only on its own child's session, after hello, when
// Agent belongs to that session and backs the stream's Pane. PID, UID and
// Start are Linux facts for the host's logs; the host never uses them as
// Windows pids.
type FwdAttest struct {
	Agent string `json:"agent"`
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
	UID   int    `json:"uid"`
}
