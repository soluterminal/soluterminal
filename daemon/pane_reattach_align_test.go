package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"soluterminal/grid"
)

// numberedHistory prints n numbered lines and a prompt, the way a long shell
// session looks in the ring.
func numberedHistory(n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d lorem ipsum dolor sit amet\r\n", i)
	}
	b.WriteString("demo$ ")
	return []byte(b.String())
}

// clientScreenAfterReattach replays the raw tail and then the reattach paint
// into a scratch emulator at the client's size, which is what the client's
// xterm does with the replay frame.
func clientScreenAfterReattach(t *testing.T, raw []byte, modelCols, modelRows, clientCols, clientRows int) []string {
	t.Helper()
	model := grid.New(modelCols, modelRows)
	defer func() { _ = model.Close() }()
	model.Feed(raw)
	snap := model.Snapshot()

	off := reattachPaintRowOffset(clientCols, clientRows, raw, snap)
	paint := serializeGridSnapshotAt(snap, snap.CursorY, 0, 0, off, clientRows)

	client := grid.New(clientCols, clientRows)
	defer func() { _ = client.Close() }()
	client.Feed(raw)
	client.Feed(paint)
	cs := client.Snapshot()
	rows := make([]string, cs.Rows)
	for y := 0; y < cs.Rows; y++ {
		var line strings.Builder
		for x := 0; x < cs.Cols; x++ {
			c := cs.Cells[y*cs.Cols+x].Content
			if c == "" {
				c = " "
			}
			line.WriteString(c)
		}
		rows[y] = strings.TrimRight(line.String(), " ")
	}
	return rows
}

var numberedLine = regexp.MustCompile(`^(\d+) lorem`)

func assertContiguousTail(t *testing.T, rows []string, last int) {
	t.Helper()
	var nums []string
	for _, r := range rows {
		if m := numberedLine.FindStringSubmatch(r); m != nil {
			nums = append(nums, m[1])
		}
	}
	if len(nums) == 0 {
		t.Fatalf("no numbered lines on screen: %q", rows)
	}
	seen := map[string]bool{}
	for _, n := range nums {
		if seen[n] {
			t.Fatalf("line %s appears twice on screen: %q", n, rows)
		}
		seen[n] = true
	}
	if nums[len(nums)-1] != fmt.Sprint(last) {
		t.Fatalf("last numbered line = %s, want %d: %q", nums[len(nums)-1], last, rows)
	}
}

func TestReattachPaintNoDuplicateTailWhenClientTaller(t *testing.T) {
	raw := numberedHistory(400)
	rows := clientScreenAfterReattach(t, raw, 120, 30, 130, 39)
	assertContiguousTail(t, rows, 400)
	if !strings.HasPrefix(rows[len(rows)-1], "demo$") {
		t.Fatalf("prompt not on the bottom row: %q", rows[len(rows)-1])
	}
}

func TestReattachPaintNoDuplicateTailWhenClientShorter(t *testing.T) {
	raw := numberedHistory(400)
	rows := clientScreenAfterReattach(t, raw, 120, 30, 120, 18)
	assertContiguousTail(t, rows, 400)
	if !strings.HasPrefix(rows[len(rows)-1], "demo$") {
		t.Fatalf("prompt not on the bottom row: %q", rows[len(rows)-1])
	}
}

func TestReattachPaintShortHistoryStaysTopAligned(t *testing.T) {
	// Fewer lines than either height: nothing scrolled, so the paint must not
	// move (offset 0) and the history stays at the top.
	raw := numberedHistory(5)
	rows := clientScreenAfterReattach(t, raw, 120, 30, 130, 39)
	assertContiguousTail(t, rows, 5)
	if !strings.HasPrefix(rows[0], "1 lorem") {
		t.Fatalf("short history moved off the top row: %q", rows[:6])
	}
}

func TestReattachPaintRowOffsetZeroWhenHeightsMatchOrUnknown(t *testing.T) {
	model := grid.New(120, 30)
	defer func() { _ = model.Close() }()
	raw := numberedHistory(400)
	model.Feed(raw)
	snap := model.Snapshot()
	if got := reattachPaintRowOffset(130, 30, raw, snap); got != 0 {
		t.Fatalf("same height: offset %d, want 0", got)
	}
	if got := reattachPaintRowOffset(0, 0, raw, snap); got != 0 {
		t.Fatalf("unknown client size (older client): offset %d, want 0", got)
	}
	if got := reattachPaintRowOffset(130, 39, nil, snap); got != 0 {
		t.Fatalf("empty replay: offset %d, want 0", got)
	}
}

// The paint must never address a row past the client's bottom, whatever the
// offset: xterm clamps such CUPs onto its last row, overwriting the prompt.
func TestReattachPaintNeverAddressesBelowClientBottom(t *testing.T) {
	model := grid.New(80, 30)
	defer func() { _ = model.Close() }()
	model.Feed(numberedHistory(400))
	snap := model.Snapshot()

	const clientRows = 35
	paint := serializeGridSnapshotAt(snap, snap.CursorY, 0, 0, 9, clientRows)
	cup := regexp.MustCompile(`\x1b\[(\d+);\d+H`)
	for _, m := range cup.FindAllStringSubmatch(string(paint), -1) {
		var row int
		if _, err := fmt.Sscan(m[1], &row); err != nil {
			t.Fatal(err)
		}
		if row > clientRows {
			t.Fatalf("paint addresses row %d on a %d-row client", row, clientRows)
		}
	}
}

// A client that writes the replay at replay-start's cols parses it at the
// model's width, whatever term_cols it announced; the repaint must be lined up
// at that width. Simulated at a narrower announced width, the 100-column line
// wraps, the cursor lands two rows lower, and the shifted paint overwrites the
// prompt row.
func TestReattachPaintAlignsAtReplayWidth(t *testing.T) {
	raw := []byte("A\r\n" + strings.Repeat("X", 100) + "\r\nprompt$ ")
	model := grid.New(220, 30)
	defer func() { _ = model.Close() }()
	model.Feed(raw)
	snap := model.Snapshot()

	opts := subOptions{termCols: 40, termRows: 39, replayWidth: true}
	if got := reattachAlignCols(opts, snap.Cols); got != 220 {
		t.Fatalf("align cols = %d, want the replay width 220", got)
	}
	if got := reattachPaintRowOffset(reattachAlignCols(opts, snap.Cols), 39, raw, snap); got != 0 {
		t.Fatalf("offset at the replay width = %d, want 0", got)
	}
	rows := clientScreenAfterReattach(t, raw, 220, 30, 220, 39)
	if rows[0] != "A" || rows[1] != strings.Repeat("X", 100) || rows[2] != "prompt$" {
		t.Fatalf("client screen after reattach: %q", rows[:4])
	}
}

// Older clients write the replay at the size they announced, so their repaint
// is still lined up at term_cols.
func TestReattachAlignColsOlderClientUsesTermCols(t *testing.T) {
	if got := reattachAlignCols(subOptions{termCols: 40, termRows: 39}, 220); got != 40 {
		t.Fatalf("align cols = %d, want term_cols 40", got)
	}
	if got := reattachAlignCols(subOptions{termCols: 40, termRows: 39, replayWidth: true}, 0); got != 40 {
		t.Fatalf("unknown model width: align cols = %d, want term_cols 40", got)
	}
}
