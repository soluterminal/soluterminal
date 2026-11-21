//go:build windows

package main

import (
	"slices"
	"testing"
)

// ST-323 on the Windows process-tree path: processTreeFromParents puts every
// process whose recorded parent is 0 under root 0, so descendantPids(0)
// reaches System and what runs under it. A suspended pane after a failed
// respawn has Pid 0; respawn must reap nothing for it.
func TestRespawnPid0OnTheWindowsProcessTree(t *testing.T) {
	tree := processTreeFromParents(map[int]int{
		0:    0,   // System Idle: its own parent, skipped
		4:    0,   // System
		120:  4,   // Registry
		580:  4,   // smss.exe
		9000: 777, // an ordinary process
	})
	if all := tree.descendantPids(0); !slices.Contains(all, 4) || !slices.Contains(all, 580) {
		t.Fatalf("expected root 0 to reach System and its children, got %v", all)
	}
	if got := respawnDetachedCandidates(tree, 0); len(got) != 0 {
		t.Fatalf("respawn of a pid-0 pane would reap %v", got)
	}
}
