package main

import "testing"

// ST-323: a pane with no agent pid gives respawn nothing to reap, even on a
// tree where root 0 has children (Windows: System, pid 4, under ppid 0).
func TestRespawnDetachedCandidatesNeedAPid(t *testing.T) {
	tree := processTreeSnapshot{children: map[int][]int{0: {4}, 4: {100}, 100: {200}, 300: {301}}}
	for _, pid := range []int{0, -1} {
		if got := respawnDetachedCandidates(tree, pid); len(got) != 0 {
			t.Fatalf("pid %d: respawn would reap %v", pid, got)
		}
	}
	if got := respawnDetachedCandidates(tree, 300); len(got) != 2 || got[0] != 300 || got[1] != 301 {
		t.Fatalf("a real agent's tree: %v", got)
	}
}
