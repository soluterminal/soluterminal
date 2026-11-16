package main

import (
	"time"
)

const (
	// delegationNudgeMinInterval bounds interruption: one line a minute per
	// pane, everything else coalesces into the next one.
	delegationNudgeMinInterval = 60 * time.Second
	// delegationNudgeTick is how often idleness is re-checked. Cheap: the tick
	// does nothing at all for a pane with an empty queue.
	delegationNudgeTick = 2 * time.Second
	// delegationNudgeMaxLineRunes keeps the injected line inside one composer
	// line — a wrapped multi-line paste is exactly what the PTY submit split
	// exists to avoid.
	delegationNudgeMaxLineRunes = 200
	// delegationNudgeTitleFloorRunes is the shortest a title may be squeezed
	// to before entries are dropped instead. Below this a title stops
	// identifying anything.
	delegationNudgeTitleFloorRunes = 12
	delegationIdleSettle           = 3 * time.Second
	delegationComposerStaleAfter   = 15 * time.Minute
)
