package grid

import (
	"image/color"
	"testing"
)

func TestVisibleTextRowsMatching(t *testing.T) {
	band := color.RGBA{0x37, 0x37, 0x37, 0xff}
	e := New(10, 4)
	defer e.Close()
	e.Feed([]byte("\x1b[48;2;55;55;55m❯ 1. echo \x1b[0m\r\n" + // whole row on the band
		"\x1b[48;2;55;55;55m❯ \x1b[0m1. half\r\n" + // the glyph only
		"\x1b[48;2;55;55;55m❯ \x1b[7m1.\x1b[27m rev  \x1b[0m\r\n" + // a reverse-video cell
		"❯ 1. plain"))
	text, rows, ok := e.VisibleTextRowsMatching(func(bg color.Color, reverse bool) bool { return !reverse && bg == band })
	if !ok {
		t.Fatal("no screen")
	}
	if plain, _ := e.VisibleText(); text != plain {
		t.Fatalf("text %q differs from VisibleText %q", text, plain)
	}
	want := []bool{true, false, false, false}
	for y := range want {
		if rows[y] != want[y] {
			t.Errorf("row %d matched=%v, want %v", y, rows[y], want[y])
		}
	}
}
