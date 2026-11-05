package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Rollback-safe registration for the MCP bridge. Nothing here registers the
// bridge: no release writes a bridge entry yet. What it does is make every
// daemon from this one on a safe place to roll back to once a later release
// does:
//   - a bridge entry of ours (solud mcp-bridge) is kept only when it names
//     this daemon's own binary and live socket; otherwise it is turned back
//     into the HTTP entry;
//   - an entry someone else wrote is never touched;
//   - Codex: a second [mcp_servers.soluterminal] table, which an older daemon
//     appends when it finds our bridge table (it reads "no url" as "missing"),
//     is repaired to one valid table, so an upgrade heals a rollback;
//   - every write keeps a 0600 backup of our entry only, keeps the file's
//     mode, is parsed before it replaces the file, and changes no other byte.

// mcpSocketRegistration tells the registration writers whether this daemon's
// MCP socket is serving, set once by main after it publishes the socket.
type mcpSocketRegistration struct {
	once   sync.Once
	ready  chan struct{}
	mu     sync.Mutex
	active bool
	socket string

	backupDir string
}

var mcpSocketReg = &mcpSocketRegistration{ready: make(chan struct{})}

// mcpSocketRegistrationWait bounds how long a writer waits for main to
// decide; past it the socket counts as not serving.
const mcpSocketRegistrationWait = 2 * time.Minute

func setMCPRegistrationDataDir(dataDir string) {
	mcpSocketReg.mu.Lock()
	mcpSocketReg.backupDir = filepath.Join(dataDir, "state", "mcp-registration")
	mcpSocketReg.mu.Unlock()
}

// setMCPSocketServing records the socket outcome: the published path when it
// serves, "" when it does not.
func setMCPSocketServing(socket string) {
	mcpSocketReg.once.Do(func() {
		mcpSocketReg.mu.Lock()
		mcpSocketReg.active = socket != ""
		mcpSocketReg.socket = socket
		mcpSocketReg.mu.Unlock()
		close(mcpSocketReg.ready)
	})
}

// mcpSocketServing waits for main's decision and returns the live socket
// path, or "" when the socket isn't serving.
func mcpSocketServing() string {
	select {
	case <-mcpSocketReg.ready:
	case <-time.After(mcpSocketRegistrationWait):
		return ""
	}
	mcpSocketReg.mu.Lock()
	defer mcpSocketReg.mu.Unlock()
	if !mcpSocketReg.active {
		return ""
	}
	return mcpSocketReg.socket
}

// mcpBridgeExecutable is the solud path a bridge entry of this daemon names:
// the running binary under its installed name. A binary renamed by an update
// swap (solud.old) or a dev build under another name gives "".
var mcpBridgeExecutable = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	switch filepath.Base(exe) {
	case "solud", "solud.exe":
		return exe
	}
	return ""
}

// mcpBridgeEntryOurs reports whether command/args are a bridge entry a
// soluterminal daemon wrote, for any binary and socket.
func mcpBridgeEntryOurs(command string, args []string) bool {
	base := filepath.Base(command)
	return (base == "solud" || base == "solud.exe") && len(args) >= 1 && args[0] == mcpBridgeCommand
}

// mcpBridgeEntryLive reports whether a bridge entry names this daemon's own
// binary and its serving socket, so it works as written.
func mcpBridgeEntryLive(command string, args []string) bool {
	exe, socket, ok := mcpBridgeTarget()
	return ok && command == exe &&
		len(args) == 3 && args[0] == mcpBridgeCommand && args[1] == "--socket" && args[2] == socket
}

// mcpBridgeRegisterOff is the switch that keeps agents on HTTP /mcp: with it
// set, no entry moves to the bridge and a bridge entry goes back to HTTP.
func mcpBridgeRegisterOff() bool { return os.Getenv("SOLU_MCP_BRIDGE_REGISTER") == "0" }

// mcpBridgeTarget is the binary and socket a bridge entry of this daemon
// names, when it can serve one: the socket is serving, the binary runs under
// its installed name, and the switch above isn't set.
func mcpBridgeTarget() (exe, socket string, ok bool) {
	if mcpBridgeRegisterOff() {
		return "", "", false
	}
	socket = mcpSocketServing()
	exe = mcpBridgeExecutable()
	return exe, socket, socket != "" && exe != ""
}

// claudeBridgeEntryJSON is the stdio entry that runs the bridge.
func claudeBridgeEntryJSON(exe, socket string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "stdio",
		"command": exe,
		"args":    []string{mcpBridgeCommand, "--socket", socket},
		"env":     map[string]string{},
	})
	return b
}

// claudeEntryIsOurHTTP reports whether raw is exactly the HTTP entry this
// daemon writes for desired: type http (or none), our loopback URL, and no
// headers but the pane header, if any. Anything more was edited by someone,
// and stays as it is.
func claudeEntryIsOurHTTP(raw json.RawMessage, desired string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	var e struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	for k := range fields {
		if k != "type" && k != "url" && k != "headers" {
			return false
		}
	}
	if e.Type != "" && e.Type != "http" {
		return false
	}
	for k, v := range e.Headers {
		if k != claudeMcpPaneHeaderName || v != claudeMcpPaneHeaderValue {
			return false
		}
	}
	return sameLocalMcpEndpoint(e.URL, desired)
}

// claudePlanBridgeMigration moves exactly our HTTP entry to the bridge when
// this daemon can serve it. Everything else in the file stays byte for byte.
func claudePlanBridgeMigration(data []byte, desired string) (next []byte, changed bool, err error) {
	exe, socket, ok := mcpBridgeTarget()
	if !ok {
		return nil, false, nil
	}
	start, end, found, err := jsonMemberSpan(data, "mcpServers")
	if err != nil || !found {
		return nil, false, err
	}
	s, e, found, err := jsonMemberSpan(data[start:end], "soluterminal")
	if err != nil || !found || !claudeEntryIsOurHTTP(data[start+s:start+e], desired) {
		return nil, false, err
	}
	next, err = jsonSetNestedMember(data, "mcpServers", "soluterminal", claudeBridgeEntryJSON(exe, socket))
	return next, err == nil, err
}

// claudeEntryIsOurBridge reports whether ~/.claude.json's entry is a bridge
// entry a soluterminal daemon wrote.
func claudeEntryIsOurBridge(body []byte) bool {
	var cfg struct {
		McpServers map[string]claudeEntry `json:"mcpServers"`
	}
	if json.Unmarshal(body, &cfg) != nil {
		return false
	}
	e, ok := cfg.McpServers["soluterminal"]
	return ok && e.URL == "" && mcpBridgeEntryOurs(e.Command, e.Args)
}

// backupMCPEntry keeps a 0600 copy of our entry before a write; a write
// without its backup does not happen.
func backupMCPEntry(agent, configPath, entry string) error {
	mcpSocketReg.mu.Lock()
	dir := mcpSocketReg.backupDir
	mcpSocketReg.mu.Unlock()
	if dir == "" {
		return errors.New("no backup directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%d.bak", agent, time.Now().UnixNano())
	body := "# soluterminal entry from " + configPath + " before a change on " + time.Now().UTC().Format(time.RFC3339) + "\n" + entry
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if err := writeFileMode(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		return err
	}
	pruneMCPEntryBackups(dir, agent, 20)
	return nil
}

func pruneMCPEntryBackups(dir, agent string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), agent+"-") && strings.HasSuffix(e.Name(), ".bak") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// errConfigChangedMeanwhile: another program (a running Claude Code saves
// ~/.claude.json often) wrote the file after it was read. Nothing was written;
// the caller reads it again and starts over.
var errConfigChangedMeanwhile = errors.New("changed by another program while it was being updated")

// writeAgentConfig replaces path with data, which was planned from before.
//   - validate runs first; if it fails, the file isn't touched.
//   - A symlinked config is written through its target, so the link stays.
//   - The temporary file is created exclusively (O_EXCL, random name), with
//     the file's existing mode (0600 for a new file).
//   - Just before the rename the file is read again; if it no longer holds
//     before, nothing is written and errConfigChangedMeanwhile is returned.
//     (The rename itself can't be made conditional, so a write landing in
//     that last instant can still be lost.)
func writeAgentConfig(path string, before, data []byte, validate func([]byte) error) error {
	if err := validate(data); err != nil {
		return fmt.Errorf("not writing %s: the result would not parse: %w", path, err)
	}
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".solu-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(mode)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	current, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(tmp)
		return err
	}
	if (err == nil) != (before != nil) || !bytes.Equal(current, before) {
		_ = os.Remove(tmp)
		return errConfigChangedMeanwhile
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// configWriteAttempts bounds how often an update starts over because another
// program wrote the file meanwhile.
const configWriteAttempts = 3

// updateConfigFile reads path, plans the change from what it read, and
// writes it, starting over when another program changed the file in between.
// A missing file is plan(nil). changed reports a write.
func updateConfigFile(path string, plan func(before []byte) ([]byte, bool, error), write func(before, next []byte) error) (changed bool, err error) {
	for attempt := 0; attempt < configWriteAttempts; attempt++ {
		before, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		next, change, err := plan(before)
		if err != nil || !change {
			return false, err
		}
		err = write(before, next)
		if errors.Is(err, errConfigChangedMeanwhile) {
			continue
		}
		return err == nil, err
	}
	return false, fmt.Errorf("%s kept changing while it was being updated; will try again on the next start", path)
}

// updateCodexConfig applies plan to config.toml with backup, parse check and
// re-read (writeCodexConfig, updateConfigFile).
func updateCodexConfig(path string, plan func(body string) (string, bool)) (bool, error) {
	return updateConfigFile(path, func(before []byte) ([]byte, bool, error) {
		if before == nil {
			return nil, false, nil
		}
		next, changed := plan(string(before))
		return []byte(next), changed, nil
	}, func(before, next []byte) error {
		return writeCodexConfig(path, string(before), string(next))
	})
}

// updateClaudeConfig applies plan to ~/.claude.json the same way.
func updateClaudeConfig(path string, plan func(data []byte) ([]byte, bool, error)) (bool, error) {
	return updateConfigFile(path, func(before []byte) ([]byte, bool, error) {
		if before == nil {
			return nil, false, nil
		}
		return plan(before)
	}, func(before, next []byte) error {
		return writeClaudeConfig(path, before, next)
	})
}

// writeFileMode writes a new file with exactly mode, whatever the umask.
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	return os.Chmod(path, mode)
}

func validateTOML(data []byte) error {
	var v map[string]any
	return toml.Unmarshal(data, &v)
}

func validateJSONObject(data []byte) error {
	var v map[string]json.RawMessage
	return json.Unmarshal(data, &v)
}

// ---- JSON: change one member, keep every other byte ----

// jsonMemberSpan finds key's value in the JSON object obj: [start, end) of
// the value's bytes. found is false when the object has no such member.
func jsonMemberSpan(obj []byte, key string) (start, end int, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false, errors.New("not a JSON object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return 0, 0, false, err
		}
		k, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false, err
		}
		if k == key {
			end := int(dec.InputOffset())
			return end - len(raw), end, true, nil
		}
	}
	return 0, 0, false, nil
}

// jsonSetMember sets key in the JSON object obj to val, replacing only the
// old value's bytes, or inserting the member before the closing brace.
func jsonSetMember(obj []byte, key string, val []byte) ([]byte, error) {
	start, end, found, err := jsonMemberSpan(obj, key)
	if err != nil {
		return nil, err
	}
	if found {
		return append(append(append([]byte(nil), obj[:start]...), val...), obj[end:]...), nil
	}
	trimmed := bytes.TrimRight(obj, " \t\r\n")
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		return nil, errors.New("not a JSON object")
	}
	closeAt := len(trimmed) - 1
	body := bytes.TrimRight(trimmed[:closeAt], " \t\r\n")
	keyJSON, _ := json.Marshal(key)
	member := append(append(keyJSON, ": "...), val...)
	var out []byte
	if body[len(body)-1] == '{' {
		out = append(append(append([]byte(nil), body...), member...), obj[closeAt:]...)
	} else {
		out = append(append(append(append([]byte(nil), body...), ", "...), member...), obj[len(body):]...)
	}
	return out, nil
}

// jsonSetNestedMember sets outer.inner in the top-level object data, creating
// outer as an object when it is missing.
func jsonSetNestedMember(data []byte, outer, inner string, val []byte) ([]byte, error) {
	start, end, found, err := jsonMemberSpan(data, outer)
	if err != nil {
		return nil, err
	}
	if !found {
		innerKey, _ := json.Marshal(inner)
		obj := append(append(append([]byte("{"), innerKey...), ": "...), val...)
		return jsonSetMember(data, outer, append(obj, '}'))
	}
	next, err := jsonSetMember(data[start:end], inner, val)
	if err != nil {
		return nil, err
	}
	return append(append(append([]byte(nil), data[:start]...), next...), data[end:]...), nil
}

// ---- Codex: the [mcp_servers.soluterminal] table ----

const (
	codexEntryHTTP    = "http"
	codexEntryBridge  = "bridge"
	codexEntryForeign = "foreign"
)

// codexTable is one [mcp_servers.soluterminal] table: lines [start, end) of
// the body, start being its header. Sub-tables are separate tables.
type codexTable struct {
	start, end int
	kind       string
	live       bool // a bridge entry naming this daemon's binary and socket
}

func codexSoluterminalTables(lines []string) []codexTable {
	var out []codexTable
	for i, line := range lines {
		m := tomlTableRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if n := len(out); n > 0 && out[n-1].end < 0 {
			out[n-1].end = i
		}
		if tomlTableIs(m[1], "soluterminal") {
			out = append(out, codexTable{start: i, end: -1})
		}
	}
	if n := len(out); n > 0 && out[n-1].end < 0 {
		out[n-1].end = len(lines)
	}
	for i := range out {
		out[i].kind, out[i].live = codexTableKind(lines[out[i].start+1 : out[i].end])
	}
	return out
}

// codexTransportKeys are the keys that say how Codex reaches a server.
var codexTransportKeys = map[string]bool{
	"command": true, "args": true, "env": true, "cwd": true, "env_vars": true,
	"url": true, "http_headers": true, "env_http_headers": true, "bearer_token_env_var": true,
}

// codexTableKind classifies a table body by its own keys, each of which must
// fit on one line for the table to count as ours.
func codexTableKind(body []string) (kind string, live bool) {
	var entry struct {
		URL     string   `toml:"url"`
		Command string   `toml:"command"`
		Args    []string `toml:"args"`
	}
	if err := toml.Unmarshal([]byte(strings.Join(body, "\n")), &entry); err != nil {
		return codexEntryForeign, false
	}
	for _, line := range body {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		key, rest, ok := strings.Cut(t, "=")
		if !ok {
			return codexEntryForeign, false // a continuation line
		}
		key = strings.TrimSpace(key)
		if codexTransportKeys[key] {
			var one map[string]any
			if toml.Unmarshal([]byte(key+" = "+strings.TrimSpace(rest)), &one) != nil {
				return codexEntryForeign, false
			}
		}
	}
	switch {
	case entry.URL != "" && entry.Command == "":
		return codexEntryHTTP, false
	case entry.URL == "" && mcpBridgeEntryOurs(entry.Command, entry.Args):
		return codexEntryBridge, mcpBridgeEntryLive(entry.Command, entry.Args)
	}
	return codexEntryForeign, false
}

// codexPlanRegistration repairs the soluterminal table of a config.toml body:
// duplicate tables of ours become one, and a bridge table that isn't live
// becomes the HTTP entry for desired. Other tables, comments and keys are
// kept byte for byte. changed is false when there is nothing to do or the
// tables aren't ours to touch; entry is our old table text, for the backup.
func codexPlanRegistration(body, desired string) (next, entry, note string, changed bool) {
	lines := strings.Split(body, "\n")
	tables := codexSoluterminalTables(lines)
	if len(tables) == 0 {
		return body, "", "", false
	}
	for _, t := range tables {
		if t.kind == codexEntryForeign {
			if len(tables) > 1 {
				log.Printf("codex-mcp: %d [mcp_servers.soluterminal] tables, one of them not ours; leaving them as they are", len(tables))
			}
			return body, "", "", false
		}
	}
	var old []string
	for _, t := range tables {
		old = append(old, strings.Join(lines[t.start:t.end], "\n"))
	}
	entry = strings.Join(old, "\n")

	keep := 0
	if len(tables) > 1 {
		keep = -1
		for i, t := range tables {
			if t.kind == codexEntryBridge && t.live {
				keep = i
			}
		}
		if keep < 0 {
			for i, t := range tables {
				if t.kind == codexEntryHTTP && keep < 0 {
					keep = i
				}
			}
		}
		if keep < 0 {
			keep = 0
		}
		note = fmt.Sprintf("repaired %d [mcp_servers.soluterminal] tables to one", len(tables))
	}
	kept := tables[keep]
	var table []string
	if kept.kind == codexEntryBridge && !kept.live {
		table = append(table, lines[kept.start], `url = "`+desired+`"`, codexMcpPaneHeaderLine)
		for _, line := range lines[kept.start+1 : kept.end] {
			key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && codexTransportKeys[strings.TrimSpace(key)] {
				continue
			}
			table = append(table, line)
		}
		if note != "" {
			note += "; "
		}
		note += "turned a bridge entry this daemon can't serve back into " + desired
	} else {
		table = append(table, lines[kept.start:kept.end]...)
	}
	// Settings the merged tables held (a tool_timeout_sec someone set) move
	// into the one kept, unless it already has the key.
	has := map[string]bool{}
	for _, line := range table[1:] {
		if key, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			has[strings.TrimSpace(key)] = true
		}
	}
	for i, t := range tables {
		if i == keep {
			continue
		}
		for _, line := range lines[t.start+1 : t.end] {
			key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			key = strings.TrimSpace(key)
			if !ok || codexTransportKeys[key] || has[key] {
				continue
			}
			has[key] = true
			table = insertBeforeTrailingBlank(table, line)
		}
	}
	if len(tables) == 1 && !(kept.kind == codexEntryBridge && !kept.live) {
		return body, "", "", false
	}

	var out []string
	cur := 0
	for i, t := range tables {
		out = append(out, lines[cur:t.start]...)
		if i == keep {
			out = append(out, table...)
		}
		cur = t.end
	}
	out = append(out, lines[cur:]...)
	return strings.Join(out, "\n"), entry, note, true
}

// codexTableText is the soluterminal table text of body, for backups.
func codexTableText(body string) string {
	lines := strings.Split(body, "\n")
	var parts []string
	for _, t := range codexSoluterminalTables(lines) {
		parts = append(parts, strings.Join(lines[t.start:t.end], "\n"))
	}
	return strings.Join(parts, "\n")
}

// writeCodexConfig backs up our table, then writes next only if it parses.
func writeCodexConfig(path, before, next string) error {
	if err := backupMCPEntry("codex", path, codexTableText(before)); err != nil {
		return fmt.Errorf("backup before writing %s: %w", path, err)
	}
	return writeAgentConfig(path, []byte(before), []byte(next), validateTOML)
}

// writeClaudeConfig backs up our entry, then writes next only if it parses
// as a JSON object.
func writeClaudeConfig(path string, before []byte, next []byte) error {
	entry := "(none)"
	if start, end, found, err := jsonMemberSpan(before, "mcpServers"); err == nil && found {
		if s, e, ok, err := jsonMemberSpan(before[start:end], "soluterminal"); err == nil && ok {
			entry = string(before[start+s : start+e])
		}
	}
	if err := backupMCPEntry("claude", path, entry); err != nil {
		return fmt.Errorf("backup before writing %s: %w", path, err)
	}
	return writeAgentConfig(path, before, next, validateJSONObject)
}

// claudeHTTPEntryJSON is the HTTP entry this daemon registers for desired.
func claudeHTTPEntryJSON(desired string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "http",
		"url":     desired,
		"headers": map[string]string{claudeMcpPaneHeaderName: claudeMcpPaneHeaderValue},
	})
	return b
}

// claudeEntry is the soluterminal member of ~/.claude.json's mcpServers.
type claudeEntry struct {
	Type    string   `json:"type"`
	URL     string   `json:"url"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// claudePlanBridgeEntry decides about a stdio soluterminal entry: keep it
// (live bridge entry, or not ours), or restore our HTTP entry. handled is
// false when the entry isn't stdio, leaving it to the URL policy.
func claudePlanBridgeEntry(data []byte, desired string) (next []byte, handled, changed bool, err error) {
	var cfg struct {
		McpServers map[string]claudeEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false, false, err
	}
	e, ok := cfg.McpServers["soluterminal"]
	if !ok || e.URL != "" || e.Command == "" {
		return nil, false, false, nil
	}
	if !mcpBridgeEntryOurs(e.Command, e.Args) || mcpBridgeEntryLive(e.Command, e.Args) {
		return nil, true, false, nil
	}
	next, err = jsonSetNestedMember(data, "mcpServers", "soluterminal", claudeHTTPEntryJSON(desired))
	return next, true, err == nil, err
}

// insertBeforeTrailingBlank adds line to a table's lines, ahead of the blank
// lines that separate it from the next table.
func insertBeforeTrailingBlank(table []string, line string) []string {
	at := len(table)
	for at > 1 && strings.TrimSpace(table[at-1]) == "" {
		at--
	}
	out := append(append(append([]string(nil), table[:at]...), line), table[at:]...)
	return out
}
