package main

import (
	"strings"
	"testing"

	"soluterminal-shared/releases"
)

// The web page tells an operator's pause from the channel's default by the
// reason (store.ts shellAutoApplyPausedByOperator: ends in "by operator").
// Keep those reasons in step with it.
func TestShellAutoApplyPausedReasonsTellOperatorFromDefault(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name     string
		ctrl     *UpdateControl
		operator bool
	}{
		{"shell_auto_apply_paused", &UpdateControl{ShellAutoApplyPaused: &yes}, true},
		{"global pause", &UpdateControl{Paused: true}, true},
		{"public default", nil, false},
		{"public default, field absent", &UpdateControl{Schema: 1}, false},
	} {
		paused, reason := tc.ctrl.shellAutoApplyPaused(releases.FlavorPublic)
		if !paused || strings.HasSuffix(reason, "by operator") != tc.operator {
			t.Fatalf("%s: paused=%v reason=%q, want operator=%v", tc.name, paused, reason, tc.operator)
		}
	}
}
