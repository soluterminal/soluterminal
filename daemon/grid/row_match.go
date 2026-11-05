package grid

import (
	"image/color"

	uv "github.com/charmbracelet/ultraviolet"
)

// VisibleTextRowsMatching returns the screen as VisibleText does and, from
// the same frame, whether each row's every cell satisfies match (its
// background colour, exactly as the emulator stores it, and whether the cell
// is reverse video). A missing cell never matches, so a row is reported only
// when the TUI painted all of it. Not cached: callers read it only when the plain
// text already shows something worth checking.
func (e *Emulator) VisibleTextRowsMatching(match func(bg color.Color, reverse bool) bool) (text string, rows []bool, ok bool) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if e.w <= 0 || e.h <= 0 {
		return "", nil, false
	}
	plain := make([]byte, 0, (e.w+1)*e.h)
	rows = make([]bool, e.h)
	for y := 0; y < e.h; y++ {
		start := len(plain)
		all := true
		for x := 0; x < e.w; x++ {
			cell := e.vt.CellAt(x, y)
			if cell == nil {
				all = false
				plain = append(plain, ' ')
				continue
			}
			if all && !match(cell.Style.Bg, cell.Style.Attrs&uv.AttrReverse != 0) {
				all = false
			}
			if cell.Content == "" {
				plain = append(plain, ' ')
			} else {
				plain = append(plain, cell.Content...)
			}
		}
		for len(plain) > start && (plain[len(plain)-1] == ' ' || plain[len(plain)-1] == '\t') {
			plain = plain[:len(plain)-1]
		}
		plain = append(plain, '\n')
		rows[y] = all
	}
	return string(plain), rows, true
}
