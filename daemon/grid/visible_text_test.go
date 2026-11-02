package grid

import (
	"strings"
	"sync"
	"testing"
)

func referenceVisibleText(s Snapshot) string {
	var result strings.Builder
	for y := 0; y < s.Rows; y++ {
		var row strings.Builder
		for _, c := range s.Cells[y*s.Cols : (y+1)*s.Cols] {
			if c.Content == "" {
				row.WriteByte(' ')
			} else {
				row.WriteString(c.Content)
			}
		}
		result.WriteString(strings.TrimRight(row.String(), " \t"))
		result.WriteByte('\n')
	}
	return result.String()
}

func TestVisibleTextMatchesSnapshotAcrossTerminalTransitions(t *testing.T) {
	e := New(32, 6)
	defer e.Close()
	payloads := []string{"", "hello  \r\nworld", "\x1b[31mstyled\x1b[0m", "\r\n你好 é 🙂", "\x1b[2J\x1b[Hchoice prompt", "\x1b[?1049halt screen", "\x1b[?1049l", "\x1bc"}
	for _, payload := range payloads {
		e.Feed([]byte(payload))
		got, ok := e.VisibleText()
		want := referenceVisibleText(e.Snapshot())
		if !ok || got != want {
			t.Fatalf("projection mismatch for %q: got %q want %q", payload, got, want)
		}
		cached, _ := e.VisibleText()
		if cached != want {
			t.Fatal("cached projection changed")
		}
	}
	e.Resize(16, 3)
	got, _ := e.VisibleText()
	if got != referenceVisibleText(e.Snapshot()) {
		t.Fatal("resize left stale text")
	}
}

func TestVisibleTextDoesNotAllocateWhileScreenIsUnchanged(t *testing.T) {
	e := New(160, 50)
	defer e.Close()
	e.Feed([]byte("stable prompt"))
	e.VisibleText()
	allocations := testing.AllocsPerRun(100, func() { e.VisibleText() })
	if allocations != 0 {
		t.Fatalf("cached text allocated %v times", allocations)
	}
}

func TestVisibleTextRemainsConsistentDuringConcurrentFeedAndResize(t *testing.T) {
	e := New(40, 8)
	defer e.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			e.Feed([]byte("\x1b[Hcurrent frame"))
			e.Resize(40+i%2, 8)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			text, ok := e.VisibleText()
			if !ok || strings.Count(text, "\n") != 8 {
				t.Error("incomplete projection")
				return
			}
		}
	}()
	wg.Wait()
}

func BenchmarkVisibleText(b *testing.B) {
	e := New(160, 50)
	defer e.Close()
	e.Feed([]byte("stable prompt"))
	e.VisibleText()
	b.Run("styled-snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			referenceVisibleText(e.Snapshot())
		}
	})
	b.Run("cached-plain-text", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			e.VisibleText()
		}
	})
	b.Run("updated-plain-text", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			e.Feed([]byte("\x1b[Hupdated prompt"))
			e.VisibleText()
		}
	})
}

func TestVisibleTextFaintBlankedBlanksPlaceholder(t *testing.T) {
	e := New(40, 3)
	defer e.Close()
	// Claude 2.1.310's empty input box: the glyph, then a faint placeholder.
	e.Feed([]byte("\u276f\u00a0\x1b[2mTry\x1b[0m \x1b[2m\"how do I log an error?\"\x1b[0m\r\n\u276f typed \x1b[1mbold\x1b[0m"))
	plain, blanked, ok := e.VisibleTextFaintBlanked()
	if !ok {
		t.Fatal("not readable")
	}
	if want, _ := e.VisibleText(); plain != want {
		t.Fatalf("plain = %q, want VisibleText %q", plain, want)
	}
	lines := strings.Split(blanked, "\n")
	if lines[0] != "\u276f\u00a0" {
		t.Fatalf("placeholder row = %q, want the bare glyph", lines[0])
	}
	if lines[1] != "\u276f typed bold" {
		t.Fatalf("typed row = %q", lines[1])
	}
}
