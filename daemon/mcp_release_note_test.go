package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The release train's note ("merged changes are NOT active until
// published") used to reach agents only through git_status, which panes no
// longer get. It now rides list_panes, on the caller's own row only.
func TestListPanesCarriesTheRepoNoteOnTheCallersOwnRow(t *testing.T) {
	a := identityTestServer(t)
	a.mgr.panes["lead"].Cwd = "/work/repo"
	a.mgr.panes["worker"].Cwd = "/work/repo"
	prev := gitStatusNoteHooks
	gitStatusNoteHooks = []func(*apiServer, string) string{
		func(_ *apiServer, path string) string {
			if path == "/work/repo" {
				return "\n\nrelease readiness: 3 commits unreleased — merged changes are NOT active until published"
			}
			return ""
		},
		func(*apiServer, string) string { return "" },
	}
	t.Cleanup(func() { gitStatusNoteHooks = prev })

	rows := func(caller string) []mcpPaneSummary {
		t.Helper()
		text, failed := mcpResultText(t, a.mcpToolListPanes(nil, caller))
		var out []mcpPaneSummary
		if failed || json.Unmarshal([]byte(text), &out) != nil {
			t.Fatalf("list_panes: %q", text)
		}
		return out
	}
	for _, r := range rows("lead") {
		switch {
		case r.ID == "lead" && r.ReleaseNote != "release readiness: 3 commits unreleased — merged changes are NOT active until published":
			t.Fatalf("own row note = %q", r.ReleaseNote)
		case r.ID != "lead" && r.ReleaseNote != "":
			t.Fatalf("%s carries the caller's note: %q", r.ID, r.ReleaseNote)
		}
	}
	// A caller with no pane has no own row, so no note.
	for _, r := range rows("") {
		if r.ReleaseNote != "" {
			t.Fatalf("%s has a note for a caller with no pane: %q", r.ID, r.ReleaseNote)
		}
	}
	// A pane outside the repository gets none.
	for _, r := range rows("other") {
		if strings.Contains(r.ReleaseNote, "release readiness") {
			t.Fatalf("%s (outside the repo) got the note", r.ID)
		}
	}
}
