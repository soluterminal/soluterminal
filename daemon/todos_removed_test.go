package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const removedTodosNotice = "Todos was removed from SoluTerminal. Your todos were exported to ~/.solu/todos-export.md. For a follow-up, flag the pane for attention (set_pane_flag) or keep it in a file in the repository."

// scratchSoluHome points HOME at a fresh directory and returns its ~/.solu,
// so no test can ever see the real one.
func scratchSoluHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".solu")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeLegacyTodoFixture(t *testing.T, path string, f legacyTodoFile) []byte {
	t.Helper()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

type fileSnapshot struct {
	data  []byte
	mode  os.FileMode
	mtime time.Time
}

func snapshotFile(t *testing.T, path string) fileSnapshot {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileSnapshot{data: data, mode: info.Mode(), mtime: info.ModTime()}
}

func assertFileUnchanged(t *testing.T, path string, want fileSnapshot) {
	t.Helper()
	got := snapshotFile(t, path)
	if string(got.data) != string(want.data) || got.mode != want.mode || !got.mtime.Equal(want.mtime) {
		t.Fatalf("%s changed: mode %v→%v mtime %v→%v, bytes equal=%v", filepath.Base(path), want.mode, got.mode, want.mtime, got.mtime, string(got.data) == string(want.data))
	}
}

func ms(s string) int64 {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm.UnixMilli()
}

func TestRemovedTodosExportWritesOnceAndLeavesTheStoreAlone(t *testing.T) {
	dir := scratchSoluHome(t)
	storePath := filepath.Join(dir, todosStoreFile)
	archivePath := filepath.Join(dir, todosArchiveFile)
	writeLegacyTodoFixture(t, storePath, legacyTodoFile{
		Todos: []*legacyTodo{
			{ID: "todo_open_new", Text: "Ship the export", CreatedAt: ms("2026-10-20T09:00:00Z"), Source: "user", Workspace: "soluterminal"},
			{ID: "todo_done_a", Text: "Rotate the key", Done: true, CreatedAt: ms("2026-09-01T09:00:00Z"), DoneAt: ms("2026-09-03T10:30:00Z"), Source: "claude", TaskID: "wft_dead"},
			{ID: "todo_open_old", Text: "Status log\nline two\n# not a heading", CreatedAt: ms("2026-08-01T09:00:00Z"), Source: "claude", OriginalText: "status log, line two", DuplicateOf: "todo_open_new"},
			{ID: "todo_done_b", Text: "Write release notes", Done: true, CreatedAt: ms("2026-10-01T09:00:00Z"), DoneAt: ms("2026-10-02T08:00:00Z"), Source: "user"},
		},
		Order: []string{"todo_open_new", "todo_done_a", "todo_open_old", "todo_done_b"},
	})
	writeLegacyTodoFixture(t, archivePath, legacyTodoFile{
		Todos: []*legacyTodo{
			{ID: "todo_archived", Text: "Old finished thing", Done: true, CreatedAt: ms("2026-05-01T09:00:00Z"), DoneAt: ms("2026-05-02T09:00:00Z"), Source: "user"},
			// Interrupted sweep: also still active. The active copy wins.
			{ID: "todo_done_b", Text: "Write release notes (stale archive copy)", Done: true, DoneAt: ms("2026-10-02T08:00:00Z")},
		},
	})
	store := snapshotFile(t, storePath)
	archive := snapshotFile(t, archivePath)

	now := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	path, err := exportRemovedTodos(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(dir, todosExportFile)
	if path != exportPath {
		t.Fatalf("export path = %q, want %q", path, exportPath)
	}
	assertFileUnchanged(t, storePath, store)
	assertFileUnchanged(t, archivePath, archive)

	info, err := os.Stat(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("export mode = %v, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(exportPath)
	got := string(data)

	for _, want := range []string{
		"# Your SoluTerminal todos",
		"saved on 2026-10-28 06:00 UTC",
		"2 open, 3 done.",
		"## Open (2)",
		"## Done (3)",
		"- [ ] Ship the export\n  Created 2026-10-20 09:00 UTC · added by you · workspace soluterminal\n  ID todo_open_new\n",
		"- [ ] Status log\n  line two\n  # not a heading\n  Created 2026-08-01 09:00 UTC · added by an agent\n  Originally written as: status log, line two\n  Possible duplicate of todo_open_new\n  ID todo_open_old\n",
		"- [x] Rotate the key\n  Created 2026-09-01 09:00 UTC · done 2026-09-03 10:30 UTC · added by an agent · task wft_dead\n",
		"- [x] Old finished thing\n  Created 2026-05-01 09:00 UTC · done 2026-05-02 09:00 UTC · added by you · archived\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("export is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "stale archive copy") {
		t.Fatal("export used the archive's copy of an item that is still active")
	}
	order := []string{"## Open", "Ship the export", "Status log", "## Done", "Write release notes", "Rotate the key", "Old finished thing"}
	last := -1
	for _, marker := range order {
		i := strings.Index(got, marker)
		if i <= last {
			t.Fatalf("%q is out of order in the export:\n%s", marker, got)
		}
		last = i
	}

	// A second start never rewrites it, even after the store changes.
	if err := os.WriteFile(exportPath, []byte("edited by the user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLegacyTodoFixture(t, storePath, legacyTodoFile{Todos: []*legacyTodo{{ID: "todo_x", Text: "new"}}})
	store = snapshotFile(t, storePath)
	path, err = exportRemovedTodos(dir, now.Add(time.Hour))
	if err != nil || path != "" {
		t.Fatalf("second export = %q, %v; want no write", path, err)
	}
	if data, _ := os.ReadFile(exportPath); string(data) != "edited by the user\n" {
		t.Fatalf("existing export was overwritten: %q", data)
	}
	assertFileUnchanged(t, storePath, store)
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatalf("data dir has %d entries, want the store, the archive and the export (no temp files)", len(entries))
	}
}

func TestRemovedTodosExportSkipsAnEmptyOrMissingStore(t *testing.T) {
	dir := scratchSoluHome(t)
	if path, err := exportRemovedTodos(dir, time.Now()); err != nil || path != "" {
		t.Fatalf("missing store: export = %q, %v", path, err)
	}
	writeLegacyTodoFixture(t, filepath.Join(dir, todosStoreFile), legacyTodoFile{Todos: []*legacyTodo{}})
	if path, err := exportRemovedTodos(dir, time.Now()); err != nil || path != "" {
		t.Fatalf("empty store: export = %q, %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(dir, todosExportFile)); !os.IsNotExist(err) {
		t.Fatalf("an empty store produced an export: %v", err)
	}
}

func TestRemovedTodosExportArchiveOnly(t *testing.T) {
	dir := scratchSoluHome(t)
	writeLegacyTodoFixture(t, filepath.Join(dir, todosArchiveFile), legacyTodoFile{Todos: []*legacyTodo{
		{ID: "todo_a", Text: "Archived only", Done: true, CreatedAt: 1, DoneAt: 2, Source: "user"},
	}})
	path, err := exportRemovedTodos(dir, time.Now())
	if err != nil || path == "" {
		t.Fatalf("archive-only export = %q, %v; want a write", path, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "- [x] Archived only") || !strings.Contains(string(data), "## Open (0)\n\nNone.") {
		t.Fatalf("archive-only export:\n%s", data)
	}
}

func TestRemovedTodosExportRefusesAMalformedStore(t *testing.T) {
	dir := scratchSoluHome(t)
	storePath := filepath.Join(dir, todosStoreFile)
	if err := os.WriteFile(storePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotFile(t, storePath)
	if _, err := exportRemovedTodos(dir, time.Now()); err == nil {
		t.Fatal("malformed store exported without an error")
	}
	assertFileUnchanged(t, storePath, before)
	if _, err := os.Stat(filepath.Join(dir, todosExportFile)); !os.IsNotExist(err) {
		t.Fatalf("a malformed store produced an export: %v", err)
	}
}

func TestRemovedTodoToolsReturnTheNoticeAndDoNothing(t *testing.T) {
	dir := scratchSoluHome(t)
	storePath := filepath.Join(dir, todosStoreFile)
	writeLegacyTodoFixture(t, storePath, legacyTodoFile{Todos: []*legacyTodo{{ID: "todo_keep", Text: "keep me"}}})
	before := snapshotFile(t, storePath)
	// The stubs answer even where mutators are refused: they mutate nothing.
	t.Setenv(mcpMutationPolicyEnv, "deny")

	bus := newUIBus()
	sub := bus.subscribe()
	defer bus.unsubscribe(sub)
	a := &apiServer{mgr: NewManager(""), uiBus: bus, dataDir: dir}
	calls := map[string]map[string]any{
		"add_todo":    {"text": "remember this", "workspace": "x", "taskId": "wft_1"},
		"list_todos":  {"done": false, "includeArchived": true},
		"update_todo": {"id": "todo_keep", "done": true, "text": "changed"},
		"delete_todo": {"id": "todo_keep"},
	}
	for name, args := range calls {
		b, _ := json.Marshal(toolCallParams{Name: name, Arguments: args})
		text, isErr := mcpResultText(t, a.mcpCallTool(b, nil, ""))
		if text != removedTodosNotice || !isErr {
			t.Fatalf("%s = %q (isError=%v), want the fixed notice as a tool error", name, text, isErr)
		}
	}
	assertFileUnchanged(t, storePath, before)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("data dir has %d entries after stub calls, want only the store", len(entries))
	}
	select {
	case ev := <-sub:
		t.Fatalf("a stub tool published a UI event: %+v", ev)
	default:
	}
	if recent := bus.Recent(); len(recent) != 0 {
		t.Fatalf("a stub tool published UI events: %+v", recent)
	}
}

func TestRemovedTodoToolsStayRegisteredWithTinySchemas(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range mcpTools() {
		if !removedTodoToolNames[tool.Name] {
			if strings.Contains(strings.ToLower(tool.Description), "list_todos") || strings.Contains(tool.Description, "add_todo") {
				t.Errorf("%s still points at the removed todo tools", tool.Name)
			}
			continue
		}
		seen[tool.Name] = true
		b, _ := json.Marshal(tool)
		if len(b) > 120 {
			t.Errorf("%s schema is %d bytes, want a minimal stub", tool.Name, len(b))
		}
		if mcpToolRequiresMutationPolicy(tool.Name) {
			t.Errorf("%s is still in the mutation gate", tool.Name)
		}
		if mcpPublicToolNames[tool.Name] {
			t.Errorf("%s reached the public tool surface", tool.Name)
		}
	}
	if len(seen) != len(removedTodoToolNames) {
		t.Fatalf("registered stubs = %v, want all of %v", seen, removedTodoToolNames)
	}
	for _, text := range []string{mcpDirectServerInstructions, mcpPublicServerInstructions, orchestratorBrief, wardenPromptRules, wardenPrompt(wardenEvent{Kind: wardenDebrief}), wardenPrompt(wardenEvent{Kind: wardenTriage})} {
		if strings.Contains(strings.ToLower(text), "todo") {
			t.Errorf("agent prompt still mentions todos: %q", text)
		}
	}
	for _, name := range wardenReadAnnotateTools() {
		if removedTodoToolNames[name] {
			t.Errorf("scoped tool set still grants %s", name)
		}
	}
}

func TestRemovedTodosAPIAnswersGone(t *testing.T) {
	dir := scratchSoluHome(t)
	mux := http.NewServeMux()
	(&apiServer{metrics: NewMetrics(), dataDir: dir}).routes(mux)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/todos"},
		{http.MethodPost, "/api/todos"},
		{http.MethodPatch, "/api/todos/todo_1"},
		{http.MethodDelete, "/api/todos/todo_1"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"text":"x"}`)))
		if rec.Code != http.StatusGone {
			t.Fatalf("%s %s = %d, want 410", tc.method, tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s %s content type = %q", tc.method, tc.path, ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 2 || body["error"] != "todos_removed" || body["export"] != "~/.solu/todos-export.md" {
			t.Fatalf("%s %s body = %v", tc.method, tc.path, body)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the 410 routes wrote %d files", len(entries))
	}
}

func TestTodosExportDisplayPathForACustomDataDir(t *testing.T) {
	scratchSoluHome(t)
	other := t.TempDir()
	if got := todosExportDisplayPath(other); got != filepath.Join(other, todosExportFile) {
		t.Fatalf("custom data dir display path = %q", got)
	}
}

func TestRunRemovedTodosExportLogsOneLine(t *testing.T) {
	dir := scratchSoluHome(t)
	writeLegacyTodoFixture(t, filepath.Join(dir, todosStoreFile), legacyTodoFile{Todos: []*legacyTodo{{ID: "todo_1", Text: "one", CreatedAt: 1}}})
	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	runRemovedTodosExport(dir)
	runRemovedTodosExport(dir)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], filepath.Join(dir, todosExportFile)) {
		t.Fatalf("log = %q, want one line naming the export", buf.String())
	}
}
