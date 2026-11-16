package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Todos was removed from SoluTerminal (Benjamin, 2026-10-28). What is left of
// it is a one-time, read-only export of the old store (todos.json and
// todos-archive.json in the data directory) to todos-export.md, so nobody
// loses their list.
//
// The export first shipped in 0.0.1227. It stays because an install updating
// straight from an older release (the public channel still has 0.0.1225)
// reaches its first boot without Todos only now; without this, that user's
// list would never be exported. Remove it once no supported update path
// starts below 0.0.1227. The /api/todos 410 routes and the MCP stub tools are
// gone (an old call gets a 404 or "unknown tool").
//
// Nothing here deletes or rewrites todos.json, todos-archive.json or the
// export.

const (
	todosStoreFile   = "todos.json"
	todosArchiveFile = "todos-archive.json"
	todosExportFile  = "todos-export.md"
)

// legacyTodo mirrors the fields of the removed store's on-disk record. It is
// only ever read.
type legacyTodo struct {
	ID           string `json:"id"`
	Text         string `json:"text"`
	Done         bool   `json:"done"`
	CreatedAt    int64  `json:"createdAt"`
	DoneAt       int64  `json:"doneAt,omitempty"`
	Workspace    string `json:"workspace,omitempty"`
	Source       string `json:"source"`
	TaskID       string `json:"taskId,omitempty"`
	OriginalText string `json:"originalText,omitempty"`
	DuplicateOf  string `json:"duplicateOf,omitempty"`
	archived     bool
}

type legacyTodoFile struct {
	Todos []*legacyTodo `json:"todos"`
	Order []string      `json:"order"`
}

// readLegacyTodoFile reads one store file without ever writing to it. A
// missing file is an empty list.
func readLegacyTodoFile(path string) ([]*legacyTodo, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f legacyTodoFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	byID := make(map[string]*legacyTodo, len(f.Todos))
	var out []*legacyTodo
	for _, td := range f.Todos {
		if td == nil {
			continue
		}
		if td.ID != "" {
			if byID[td.ID] != nil {
				continue
			}
			byID[td.ID] = td
		}
		out = append(out, td)
	}
	// The store kept a newest-first order list next to the items; honour it
	// where it is present and keep anything it does not name after.
	if len(f.Order) > 0 {
		rank := make(map[string]int, len(f.Order))
		for i, id := range f.Order {
			if _, seen := rank[id]; !seen {
				rank[id] = i
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			ri, iok := rank[out[i].ID]
			rj, jok := rank[out[j].ID]
			if iok != jok {
				return iok
			}
			return iok && ri < rj
		})
	}
	return out, nil
}

// exportRemovedTodos writes dataDir/todos-export.md once. It does nothing
// when the store is empty or the export already exists, never overwrites an
// existing export and never touches the store files. It returns the export
// path when it wrote one.
func exportRemovedTodos(dataDir string, now time.Time) (string, error) {
	exportPath := filepath.Join(dataDir, todosExportFile)
	if _, err := os.Lstat(exportPath); err == nil {
		return "", nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	active, err := readLegacyTodoFile(filepath.Join(dataDir, todosStoreFile))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", todosStoreFile, err)
	}
	archived, err := readLegacyTodoFile(filepath.Join(dataDir, todosArchiveFile))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", todosArchiveFile, err)
	}
	// An item swept to the archive may still be in the active file if the
	// sweep was interrupted; the active copy wins.
	seen := make(map[string]bool, len(active))
	items := make([]*legacyTodo, 0, len(active)+len(archived))
	for _, td := range active {
		if td.ID != "" {
			seen[td.ID] = true
		}
		items = append(items, td)
	}
	for _, td := range archived {
		if td.ID != "" && seen[td.ID] {
			continue
		}
		td.archived = true
		items = append(items, td)
	}
	if len(items) == 0 {
		return "", nil
	}
	body := renderTodosExport(items, now)
	if err := writeFileNoClobber(exportPath, []byte(body)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", nil
		}
		return "", err
	}
	return exportPath, nil
}

// writeFileNoClobber writes a complete file at path, mode 0600, and fails
// with fs.ErrExist rather than replace one that is already there. The bytes
// go to a private temporary file first and are hard-linked into place, so a
// crash never leaves a half-written export behind; filesystems without hard
// links fall back to an exclusive create.
func writeFileNoClobber(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	// os.CreateTemp creates the file with mode 0600.
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	linkErr := os.Link(tmpPath, path)
	if linkErr == nil || errors.Is(linkErr, fs.ErrExist) {
		return linkErr
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func renderTodosExport(items []*legacyTodo, now time.Time) string {
	var open, done []*legacyTodo
	for _, td := range items {
		if td.Done {
			done = append(done, td)
		} else {
			open = append(open, td)
		}
	}
	// Open items keep the store's newest-first order. Done items read most
	// recently finished first, with the archive interleaved by done date.
	sort.SliceStable(done, func(i, j int) bool { return done[i].DoneAt > done[j].DoneAt })

	var b strings.Builder
	b.WriteString("# Your SoluTerminal todos\n\n")
	b.WriteString("Todos was removed from SoluTerminal. This is a copy of your list, saved on " +
		formatTodoTime(now.UnixMilli(), now.Location()) + ". " +
		"The original files (" + todosStoreFile + " and " + todosArchiveFile + ") are still in this folder, unchanged.\n\n")
	fmt.Fprintf(&b, "%d open, %d done.\n", len(open), len(done))
	fmt.Fprintf(&b, "\n## Open (%d)\n\n", len(open))
	if len(open) == 0 {
		b.WriteString("None.\n")
	}
	for _, td := range open {
		writeTodoExportItem(&b, td, now.Location())
	}
	fmt.Fprintf(&b, "\n## Done (%d)\n\n", len(done))
	if len(done) == 0 {
		b.WriteString("None.\n")
	}
	for _, td := range done {
		writeTodoExportItem(&b, td, now.Location())
	}
	return b.String()
}

func writeTodoExportItem(b *strings.Builder, td *legacyTodo, loc *time.Location) {
	box := "[ ]"
	if td.Done {
		box = "[x]"
	}
	b.WriteString("- " + box + " " + indentTodoText(td.Text) + "\n")
	var meta []string
	meta = append(meta, "Created "+formatTodoTime(td.CreatedAt, loc))
	if td.Done {
		meta = append(meta, "done "+formatTodoTime(td.DoneAt, loc))
	}
	switch td.Source {
	case "claude":
		meta = append(meta, "added by an agent")
	case "", "user":
		meta = append(meta, "added by you")
	default:
		meta = append(meta, "source "+td.Source)
	}
	if ws := strings.TrimSpace(td.Workspace); ws != "" {
		meta = append(meta, "workspace "+ws)
	}
	if id := strings.TrimSpace(td.TaskID); id != "" {
		meta = append(meta, "task "+id)
	}
	if td.archived {
		meta = append(meta, "archived")
	}
	b.WriteString("  " + strings.Join(meta, " · ") + "\n")
	if orig := strings.TrimSpace(td.OriginalText); orig != "" && orig != strings.TrimSpace(td.Text) {
		b.WriteString("  Originally written as: " + indentTodoText(orig) + "\n")
	}
	if dup := strings.TrimSpace(td.DuplicateOf); dup != "" {
		b.WriteString("  Possible duplicate of " + dup + "\n")
	}
	if id := strings.TrimSpace(td.ID); id != "" {
		b.WriteString("  ID " + id + "\n")
	}
}

// indentTodoText keeps a multi-line item inside its list entry.
func indentTodoText(s string) string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if strings.TrimSpace(s) == "" {
		return "(no text)"
	}
	return strings.ReplaceAll(s, "\n", "\n  ")
}

func formatTodoTime(ms int64, loc *time.Location) string {
	if ms <= 0 {
		return "unknown date"
	}
	if loc == nil {
		loc = time.Local
	}
	return time.UnixMilli(ms).In(loc).Format("2006-01-02 15:04 MST")
}

// runRemovedTodosExport is the daemon-start hook: export once, log where.
func runRemovedTodosExport(dataDir string) {
	path, err := exportRemovedTodos(dataDir, time.Now())
	if err != nil {
		log.Printf("todos: export skipped, the original files are untouched: %v", err)
		return
	}
	if path != "" {
		log.Printf("todos: Todos was removed; your todos were exported to %s (the original files are untouched)", path)
	}
}
