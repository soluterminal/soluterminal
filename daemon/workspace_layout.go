package main

import (
	"fmt"
	"math"
	"reflect"
	"time"
)

const defaultLayoutColumns = 4
const defaultLayoutAspect = 1.6

func workspaceLayoutConfig(w *Workspace) (int, float64) {
	columns, aspect := w.LayoutColumns, w.LayoutAspect
	if columns < 1 {
		columns = defaultLayoutColumns
	}
	if columns > 24 {
		columns = 24
	}
	if math.IsNaN(aspect) || math.IsInf(aspect, 0) || aspect <= 0 {
		aspect = defaultLayoutAspect
	}
	return columns, aspect
}

func layoutLeaves(node LayoutNode) []LayoutNode {
	if node == nil {
		return nil
	}
	if node["type"] != "split" {
		return []LayoutNode{node}
	}
	a, _ := layoutChild(node["a"])
	b, _ := layoutChild(node["b"])
	return append(layoutLeaves(a), layoutLeaves(b)...)
}

func stackLayout(nodes []LayoutNode, direction string, weights []float64) LayoutNode {
	if len(nodes) == 0 {
		return nil
	}
	if len(nodes) == 1 {
		return nodes[0]
	}
	if weights == nil {
		weights = make([]float64, len(nodes))
		for i := range weights {
			weights[i] = 1
		}
	}
	total := 0.0
	for _, weight := range weights {
		total += weight
	}
	middle := len(nodes) / 2
	left := 0.0
	for _, weight := range weights[:middle] {
		left += weight
	}
	return LayoutNode{"type": "split", "direction": direction, "ratio": left / total,
		"a": stackLayout(nodes[:middle], direction, weights[:middle]), "b": stackLayout(nodes[middle:], direction, weights[middle:])}
}

func balanceWorkspaceLayout(w *Workspace) {
	leaves := layoutLeaves(w.Layout)
	n := len(leaves)
	if n == 0 {
		return
	}
	maxColumns, aspect := workspaceLayoutConfig(w)
	columns := min(n, maxColumns, max(1, int(math.Ceil(math.Sqrt(float64(n)*math.Max(.25, aspect))))))
	rows := (n + columns - 1) / columns
	grid := make([]LayoutNode, 0, rows)
	offset := 0
	for r := 0; r < rows; r++ {
		count := n / rows
		if r < n%rows {
			count++
		}
		grid = append(grid, stackLayout(leaves[offset:offset+count], "horizontal", nil))
		offset += count
	}
	w.Layout = stackLayout(grid, "vertical", nil)
}

func layoutRatio(node LayoutNode) float64 {
	if ratio, ok := node["ratio"].(float64); ok && ratio > 0 && ratio < 1 {
		return ratio
	}
	return .5
}

func splitLargestLayout(node LayoutNode, paneID string, aspect float64) LayoutNode {
	if node == nil {
		return LayoutNode{"type": "pane", "paneId": paneID}
	}
	bestArea := -1.0
	bestID := -1
	index := 0
	horizontal := true
	var visit func(LayoutNode, float64, float64)
	visit = func(n LayoutNode, width, height float64) {
		if n["type"] != "split" {
			if width*height > bestArea {
				bestArea = width * height
				bestID = index
				horizontal = width >= height
			}
			index++
			return
		}
		a, _ := layoutChild(n["a"])
		b, _ := layoutChild(n["b"])
		ratio := layoutRatio(n)
		if n["direction"] == "horizontal" {
			visit(a, width*ratio, height)
			visit(b, width*(1-ratio), height)
		} else {
			visit(a, width, height*ratio)
			visit(b, width, height*(1-ratio))
		}
	}
	visit(node, aspect, 1)
	index = 0
	var replace func(LayoutNode) LayoutNode
	replace = func(n LayoutNode) LayoutNode {
		if n["type"] != "split" {
			i := index
			index++
			if i != bestID {
				return n
			}
			direction := "vertical"
			if horizontal {
				direction = "horizontal"
			}
			return LayoutNode{"type": "split", "direction": direction, "ratio": .5, "a": n, "b": LayoutNode{"type": "pane", "paneId": paneID}}
		}
		next := cloneLayoutNode(n)
		a, _ := layoutChild(n["a"])
		b, _ := layoutChild(n["b"])
		next["a"] = replace(a)
		next["b"] = replace(b)
		return next
	}
	return replace(node)
}

// New tiles follow the approved workspace policy, including agent spawns.
// Session migration is an identity replacement and must use ReplacePane.
func addWorkspaceLayoutPane(w *Workspace, paneID, direction, target string) {
	_, aspect := workspaceLayoutConfig(w)
	if w.LayoutMode == "free" {
		w.Layout = splitLargestLayout(w.Layout, paneID, aspect)
		return
	}
	w.Layout = insertPaneIntoLayout(w.Layout, paneID, direction, "")
	balanceWorkspaceLayout(w)
}

// Return a custom layout with only the affected contiguous split row/column
// redistributed. A perpendicular nested group retains its parent's area.
func removeCustomLayoutPane(node LayoutNode, paneID string) (LayoutNode, bool) {
	if node == nil {
		return nil, false
	}
	if node["type"] != "split" {
		if node["type"] == "pane" && node["paneId"] == paneID {
			return nil, true
		}
		return node, false
	}
	direction, _ := node["direction"].(string)
	type sibling struct {
		node   LayoutNode
		weight float64
	}
	var siblings []sibling
	var flatten func(LayoutNode, float64)
	flatten = func(n LayoutNode, weight float64) {
		if n["type"] == "split" && n["direction"] == direction {
			a, _ := layoutChild(n["a"])
			b, _ := layoutChild(n["b"])
			ratio := layoutRatio(n)
			flatten(a, weight*ratio)
			flatten(b, weight*(1-ratio))
		} else {
			siblings = append(siblings, sibling{n, weight})
		}
	}
	flatten(node, 1)
	var nodes []LayoutNode
	var weights []float64
	released := 0.0
	changed := false
	for _, s := range siblings {
		next, did := removeCustomLayoutPane(s.node, paneID)
		changed = changed || did
		if next == nil {
			released += s.weight
		} else {
			nodes = append(nodes, next)
			weights = append(weights, s.weight)
		}
	}
	if !changed {
		return node, false
	}
	if len(nodes) == 0 {
		return nil, true
	}
	for i := range weights {
		weights[i] += released / float64(len(weights))
	}
	return stackLayout(nodes, direction, weights), true
}

func removeWorkspaceLayoutPane(w *Workspace, paneID string) bool {
	var changed bool
	if w.LayoutMode == "free" {
		w.Layout, changed = removeCustomLayoutPane(w.Layout, paneID)
	} else {
		w.Layout, changed = removePaneFromLayout(w.Layout, paneID)
		if changed {
			balanceWorkspaceLayout(w)
		}
	}
	return changed
}

// Compare layout decisions with a small ratio tolerance for client/Go floating
// point round-trips. Non-layout payloads remain ordinary workspace updates.
func sameWorkspaceLayoutDecision(a, b *Workspace) bool {
	mode := func(w *Workspace) string {
		if w.LayoutMode == "" {
			return "auto"
		}
		return w.LayoutMode
	}
	ac, aa := workspaceLayoutConfig(a)
	bc, ba := workspaceLayoutConfig(b)
	if mode(a) != mode(b) || ac != bc || math.Abs(aa-ba) > 1e-8 || len(a.PaneIds) != len(b.PaneIds) {
		return false
	}
	ids := map[string]int{}
	for _, id := range a.PaneIds {
		ids[id]++
	}
	for _, id := range b.PaneIds {
		ids[id]--
		if ids[id] < 0 {
			return false
		}
	}
	var equal func(LayoutNode, LayoutNode) bool
	equal = func(x, y LayoutNode) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		if len(x) != len(y) {
			return false
		}
		for key, value := range x {
			other, ok := y[key]
			if !ok {
				return false
			}
			if key == "a" || key == "b" {
				xc, xok := layoutChild(value)
				yc, yok := layoutChild(other)
				if xok || yok {
					if !xok || !yok || !equal(xc, yc) {
						return false
					}
					continue
				}
			}
			if key == "ratio" {
				xf, xok := value.(float64)
				yf, yok := other.(float64)
				if xok && yok {
					if math.Abs(xf-yf) > 1e-8 {
						return false
					}
					continue
				}
			}
			if !reflect.DeepEqual(value, other) {
				return false
			}
		}
		return true
	}
	return equal(a.Layout, b.Layout)
}

func workspaceSavedViewChanged(a, b *Workspace) bool {
	return a == nil || a.LayoutVersion != b.LayoutVersion || !sameWorkspaceLayoutDecision(a, b) || !reflect.DeepEqual(a.Panes, b.Panes) || !reflect.DeepEqual(a.Browsers, b.Browsers) || !reflect.DeepEqual(a.Files, b.Files)
}

// Replace a migrated session atomically in the exact old slot. It is not a
// count change, so neither auto tiling nor largest-tile placement applies.
func (s *WorkspaceStore) ReplacePane(name, oldID string, pane PaneDef) (*Workspace, error) {
	if name == "" || name == "_current" || oldID == "" || pane.ID == "" || oldID == pane.ID {
		return nil, fmt.Errorf("named workspace and distinct pane IDs required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	original, ok := s.data[name]
	if !ok {
		return nil, fmt.Errorf("workspace %q not found", name)
	}
	w := cloneWorkspace(original)
	for _, id := range w.PaneIds {
		if id == pane.ID {
			return nil, fmt.Errorf("replacement pane is already in workspace")
		}
	}
	found := false
	var replace func(LayoutNode) LayoutNode
	replace = func(n LayoutNode) LayoutNode {
		if n == nil {
			return nil
		}
		if n["type"] == "pane" && n["paneId"] == oldID {
			next := cloneLayoutNode(n)
			next["paneId"] = pane.ID
			found = true
			return next
		}
		if n["type"] != "split" {
			return n
		}
		next := cloneLayoutNode(n)
		a, _ := layoutChild(n["a"])
		b, _ := layoutChild(n["b"])
		next["a"] = replace(a)
		next["b"] = replace(b)
		return next
	}
	w.Layout = replace(w.Layout)
	if !found {
		return nil, fmt.Errorf("pane %q is not in workspace layout", oldID)
	}
	for i, id := range w.PaneIds {
		if id == oldID {
			w.PaneIds[i] = pane.ID
		}
	}
	replaced := false
	for i, def := range w.Panes {
		if def.ID == oldID {
			w.Panes[i] = clonePaneDef(pane)
			replaced = true
		}
	}
	if !replaced {
		w.Panes = append(w.Panes, clonePaneDef(pane))
	}
	// The retired pane is closed: tombstone it as RemovePane would, so a stale
	// save cannot resurrect it.
	w.RemovedPaneIDs = append(w.RemovedPaneIDs, oldID)
	w.LayoutVersion++
	w.ActiveAt = time.Now().UTC()
	next := s.candidate()
	next[name] = w
	if err := s.commit(next); err != nil {
		return nil, err
	}
	s.clearRecentAddLocked(name, oldID)
	s.markRecentAddLocked(name, pane.ID, w.ActiveAt)
	return cloneWorkspace(w), nil
}
