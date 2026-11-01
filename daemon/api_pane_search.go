package main

import (
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

type paneSearchMatch struct {
	Line  int    `json:"line"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

type paneSearchResult struct {
	PaneID     string            `json:"paneId"`
	PaneTitle  string            `json:"paneTitle"`
	PaneCmd    []string          `json:"paneCmd"`
	Status     PaneStatus        `json:"status"`
	MatchCount int               `json:"matchCount"`
	Matches    []paneSearchMatch `json:"matches"`
}

type paneSearchResponse struct {
	Query        string             `json:"query"`
	TotalMatches int                `json:"totalMatches"`
	Capped       bool               `json:"capped"`
	Results      []paneSearchResult `json:"results"`
}

const (
	searchPerPaneLimit  = 8
	searchOverallLimit  = 40
	searchExcerptWindow = 200
)

// asciiLower preserves byte offsets used by paneSearchMatch while making the
// command/output-oriented search case-insensitive. Unicode bytes pass through
// unchanged, so match offsets always index the original UTF-8 string safely.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func (a *apiServer) searchPanes(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, paneSearchResponse{Query: q, Results: []paneSearchResult{}})
		return
	}
	needle := asciiLower(q)

	panes := a.mgr.List()
	// Most-recently-active panes first, so both the per-pane and the overall caps
	// keep the matches a user is most likely looking for — recent activity over
	// stale panes. List() uses creation order, so search explicitly ranks activity.
	sort.SliceStable(panes, func(i, j int) bool {
		return atomic.LoadInt64(&panes[i].LastActivityMs) > atomic.LoadInt64(&panes[j].LastActivityMs)
	})

	results := make([]paneSearchResult, 0, len(panes))
	total := 0
	capped := false

	for _, p := range panes {
		snap := p.Snapshot()
		if snap.Status != PaneRunning {
			continue
		}
		// Native panes (Kind=="native") hold raw stream-json JSONL in p.buffer, so
		// searching that matches JSON envelopes, not conversation text. Search the
		// PARSED native events instead; terminal panes search the PTY scrollback.
		var stripped string
		if snap.Kind == "native" {
			stripped = nativeSearchText(p)
		} else {
			stripped = stripANSI(p.buffer.Snapshot())
		}
		if stripped == "" {
			continue
		}
		lowered := asciiLower(stripped)

		// Sliding window of the newest searchPerPaneLimit matches: the text runs
		// oldest→newest, so scanning forward and dropping the FRONT once full keeps
		// the LATEST hits. The old "keep the first 8" surfaced a busy pane's stalest
		// matches and dropped the fresh ones — the opposite of what's wanted.
		window := make([]paneSearchMatch, 0, searchPerPaneLimit)
		foundMore := false
		cursor := 0
		lineNum := 1
		for {
			rel := strings.Index(lowered[cursor:], needle)
			if rel < 0 {
				break
			}
			matchPos := cursor + rel
			for i := cursor; i < matchPos; i++ {
				if stripped[i] == '\n' {
					lineNum++
				}
			}
			lineStart := matchPos
			for lineStart > 0 && stripped[lineStart-1] != '\n' {
				lineStart--
			}
			lineEnd := matchPos + len(needle)
			for lineEnd < len(stripped) && stripped[lineEnd] != '\n' {
				lineEnd++
			}
			excerpt, start, end := buildSearchExcerpt(
				stripped[lineStart:lineEnd],
				matchPos-lineStart,
				len(needle),
			)
			if len(window) == searchPerPaneLimit {
				window = window[1:]
				foundMore = true
			}
			window = append(window, paneSearchMatch{
				Line:  lineNum,
				Start: start,
				End:   end,
				Text:  excerpt,
			})
			cursor = matchPos + len(needle)
		}

		if len(window) == 0 {
			continue
		}
		// Newest-first within the pane (matches now run oldest→newest).
		for i, j := 0, len(window)-1; i < j; i, j = i+1, j-1 {
			window[i], window[j] = window[j], window[i]
		}
		// Honour the overall cap; recent panes (sorted first) get first claim.
		if remaining := searchOverallLimit - total; len(window) > remaining {
			window = window[:remaining]
			capped = true
		}

		results = append(results, paneSearchResult{
			PaneID:     snap.ID,
			PaneTitle:  snap.Title,
			PaneCmd:    snap.Cmd,
			Status:     snap.Status,
			MatchCount: len(window),
			Matches:    window,
		})
		total += len(window)
		if foundMore {
			capped = true
		}
		if total >= searchOverallLimit {
			capped = true
			break
		}
	}

	writeJSON(w, 200, paneSearchResponse{
		Query:        q,
		TotalMatches: total,
		Capped:       capped,
		Results:      results,
	})
}

// nativeSearchText concatenates the human-readable text of a native pane's parsed
// events (the ring — the same recent window the transcript renders), so cross-pane
// search matches conversation text instead of the raw stream-json JSONL sitting in
// p.buffer. Each field goes on its own line so line counting + excerpting stay sane
// and a match never bleeds across two events. Ordered oldest→newest, matching the
// terminal scrollback, so the caller's newest-first windowing works identically.
func nativeSearchText(p *Pane) string {
	events := p.NativeSince(0)
	if len(events) == 0 {
		return ""
	}
	var b strings.Builder
	for i := range events {
		writeNativeEventText(&b, &events[i])
	}
	return b.String()
}

func writeNativeEventText(b *strings.Builder, ev *NativeEvent) {
	write := func(s string) {
		if s == "" {
			return
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	write(ev.Text)    // assistant / user / thinking / slash text
	write(ev.Content) // tool_result content
	write(ev.Name)    // tool name
	write(ev.Message) // error message
	write(ev.Detail)  // error / status detail
	if len(ev.Args) > 0 {
		write(string(ev.Args)) // tool args: commands, file paths, patterns
	}
	if len(ev.Output) > 0 {
		write(string(ev.Output)) // tool_result structured output (stdout/stderr)
	}
	for i := range ev.Items {
		write(ev.Items[i].Text) // todo items
	}
}

func buildSearchExcerpt(line string, matchRel int, matchLen int) (string, int, int) {
	if len(line) <= searchExcerptWindow {
		return line, matchRel, matchRel + matchLen
	}
	leftBudget := searchExcerptWindow / 3
	start := matchRel - leftBudget
	if start < 0 {
		start = 0
	}
	end := start + searchExcerptWindow
	if end > len(line) {
		end = len(line)
		start = end - searchExcerptWindow
		if start < 0 {
			start = 0
		}
	}
	excerpt := line[start:end]
	prefix := ""
	suffix := ""
	if start > 0 {
		prefix = "…"
		excerpt = excerpt[1:]
	}
	if end < len(line) {
		suffix = "…"
		excerpt = excerpt[:len(excerpt)-1]
	}
	final := prefix + excerpt + suffix
	matchStart := (matchRel - start) + len(prefix)
	if start > 0 {
		matchStart--
	}
	if matchStart < 0 {
		matchStart = 0
	}
	matchEnd := matchStart + matchLen
	if matchEnd > len(final) {
		matchEnd = len(final)
	}
	return final, matchStart, matchEnd
}
