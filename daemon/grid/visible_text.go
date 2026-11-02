package grid

import uv "github.com/charmbracelet/ultraviolet"

// VisibleText projects the current screen without allocating styled cells.
// It has the same cell/whitespace semantics as Snapshot's plain-text readers.
// Feed and Resize invalidate the cached immutable string under writeMu, so
// polling an unchanged screen allocates nothing and cannot return a torn frame.
func (e *Emulator) VisibleText() (string, bool) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if e.w <= 0 || e.h <= 0 {
		return "", false
	}
	if e.textValid {
		return e.visibleText, true
	}
	text := make([]byte, 0, (e.w+1)*e.h)
	for y := 0; y < e.h; y++ {
		start := len(text)
		for x := 0; x < e.w; x++ {
			cell := e.vt.CellAt(x, y)
			if cell == nil || cell.Content == "" {
				text = append(text, ' ')
			} else {
				text = append(text, cell.Content...)
			}
		}
		for len(text) > start && (text[len(text)-1] == ' ' || text[len(text)-1] == '\t') {
			text = text[:len(text)-1]
		}
		text = append(text, '\n')
	}
	e.visibleText = string(text)
	e.textValid = true
	return e.visibleText, true
}

// VisibleTextFaintBlanked returns the screen as VisibleText does and, from
// the same frame, with faint (SGR 2) cells blanked. Claude and Codex draw an
// empty input box's placeholder and Claude's suggested next prompt faint, and
// what the person types in normal intensity, so the blanked copy shows what
// was actually typed. Not cached: it is read only around agent deliveries.
func (e *Emulator) VisibleTextFaintBlanked() (text, blanked string, ok bool) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if e.w <= 0 || e.h <= 0 {
		return "", "", false
	}
	plain := make([]byte, 0, (e.w+1)*e.h)
	solid := make([]byte, 0, (e.w+1)*e.h)
	trim := func(b []byte, start int) []byte {
		for len(b) > start && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
			b = b[:len(b)-1]
		}
		return append(b, '\n')
	}
	for y := 0; y < e.h; y++ {
		ps, ss := len(plain), len(solid)
		for x := 0; x < e.w; x++ {
			cell := e.vt.CellAt(x, y)
			if cell == nil || cell.Content == "" {
				plain, solid = append(plain, ' '), append(solid, ' ')
				continue
			}
			plain = append(plain, cell.Content...)
			if cell.Style.Attrs&uv.AttrFaint != 0 {
				solid = append(solid, ' ')
			} else {
				solid = append(solid, cell.Content...)
			}
		}
		plain, solid = trim(plain, ps), trim(solid, ss)
	}
	return string(plain), string(solid), true
}
