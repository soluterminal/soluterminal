package main

import "fmt"

const (
	agentRunsDefault         = 2
	agentRunsCeiling         = 8
	agentRunsReservedDefault = 1
	agentRunsReservedCeiling = 2
	// agentRunsHardCeiling is the fork-bomb backstop. The two settings
	// resolvers clamp independently, so their sum can never exceed it.
	agentRunsHardCeiling = agentRunsCeiling + agentRunsReservedCeiling
	agentRunsEnv         = "SOLU_AGENT_MAX_RUNS"
	agentRunsReservedEnv = "SOLU_AGENT_RESERVED_RUNS"
)

// agentBookkeepingTemplates is the priority class. Templates carry no
// weight/duration metadata, so adding a template id here is the only way to
// widen the class that may use the bookkeeping reserve.
var agentBookkeepingTemplates = map[string]bool{"board-steward": true}

type agentRunCaps struct {
	MaxRuns  resolvedCap
	Reserved resolvedCap
}

func resolveAgentRunCaps(r resourceSettings) agentRunCaps {
	return agentRunCaps{
		MaxRuns:  resolveAgentRunsCap(r.MaxAgentRuns),
		Reserved: resolveReservedRunsCap(r.ReservedBookkeepingRuns),
	}
}

func defaultAgentRunCaps() agentRunCaps {
	return resolveAgentRunCaps(resourceSettings{})
}

// admitRunSlot applies the one run-pool admission rule. The caller owns both
// maps and must hold its mutex. reserved is always a subset of held, therefore
// len(held) <= caps.MaxRuns.Value + caps.Reserved.Value <= agentRunsHardCeiling.
func admitRunSlot(held, reserved map[string]bool, key string, bookkeeping bool, caps agentRunCaps, noun string) (ok bool, detail string) {
	if held[key] {
		return false, runSlotStillGoing
	}
	if bookkeeping && len(reserved) < caps.Reserved.Value {
		held[key] = true
		reserved[key] = true
		return true, ""
	}
	general := len(held) - len(reserved)
	if general < caps.MaxRuns.Value {
		held[key] = true
		return true, ""
	}
	detail = fmt.Sprintf("busy — %d %s runs already in flight", caps.MaxRuns.Value, noun)
	if bookkeeping {
		detail += fmt.Sprintf(" (bookkeeping reserve %d/%d in use)", len(reserved), caps.Reserved.Value)
	}
	return false, detail
}

func releaseRunSlot(held, reserved map[string]bool, key string) {
	delete(held, key)
	delete(reserved, key)
}

// runSlotStillGoing is admitRunSlot's refusal when the same automation's
// previous run has not ended.
const runSlotStillGoing = "previous run still going"
