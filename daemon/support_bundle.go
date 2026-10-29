package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	supportBundleSchema          = 1
	supportBundleLogLines        = 300
	supportBundleLogBytes        = 64 << 10
	supportBundleFileTailBytes   = 128 << 10
	supportBundleFileTailLines   = 400
	supportBundleMaxPanes        = 40
	supportBundleSectionMaxBytes = 128 << 10
	supportBundleMaxTotalBytes   = 1 << 20

	supportBundleKVMaxDepth = 8
	supportBundleKVMaxLines = 512
)

type supportBundleSection struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	Description       string `json:"description"`
	Text              string `json:"text"`
	Bytes             int    `json:"bytes"`
	Lines             int    `json:"lines"`
	SelectedByDefault bool   `json:"selectedByDefault"`
	Empty             bool   `json:"empty"`
	Truncated         bool   `json:"truncated"`
	Note              string `json:"note,omitempty"`
}

type supportBundleResponse struct {
	Schema      int                    `json:"schema"`
	GeneratedAt string                 `json:"generatedAt"`
	Filename    string                 `json:"filename"`
	Sections    []supportBundleSection `json:"sections"`
	Omitted     []string               `json:"omitted"`
	LogPaths    []string               `json:"logPaths"`
}

var supportBundleOmitted = []string{
	"Credential stores and claim tokens",
	"Browser cookies and session tokens",
	"Full terminal scrollback (only a bounded tail of one pane you pick can be included)",
	"Environment variables and .env files",
	"Workflow/task prompts and agent transcripts",
	"Screenshots and all other images",
	"Paired account email",
}

// supportBundleOverRelay reports whether this request arrived through the
// internet-exposed machine relay. Bundle collection is deliberately local-only.
func supportBundleOverRelay(r *http.Request) bool {
	return requestViaRelay(r)
}

func (a *apiServer) handleSupportBundle(w http.ResponseWriter, r *http.Request) {
	// This must remain the first operation: remote callers cannot trigger even
	// local diagnostic collection before the relay boundary is enforced.
	if supportBundleOverRelay(r) {
		http.Error(w, "support bundles are local-only — create one from soluterminal running on this machine", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, a.buildSupportBundle(r.URL.Query().Get("paneId")))
}

// finalizeSupportSection is the single choke point through which section text
// enters the response. Redact before clamping so a secret can never be split
// into a shape the redactor no longer recognises.
func finalizeSupportSection(s supportBundleSection, maxBytes int) supportBundleSection {
	text := redactSensitive(s.Text)
	s.Text = ""
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(text) > maxBytes {
		text = supportBundleTailBytes(text, maxBytes)
		s.Truncated = true
	}
	s.Text = text
	s.Bytes = len(text)
	s.Empty = strings.TrimSpace(text) == ""
	if s.Empty {
		s.Lines = 0
	} else {
		s.Lines = strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	}
	return s
}

// supportBundleTailBytes applies feedbackClamp's byte/rune boundary semantics
// to a tail rather than a head: keep the newest bytes and never begin midway
// through a UTF-8 rune.
func supportBundleTailBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return feedbackClamp(s, maxBytes)
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return feedbackClamp(s[start:], maxBytes)
}

// supportBundleKV turns arbitrary structured DTOs into deterministic plain
// text before redaction. Going through JSON keeps this flattener independent
// of private struct layouts while avoiding escaped paths at the redaction step.
func supportBundleKV(prefix string, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return ""
	}
	lines := make([]string, 0, 32)
	var walk func(string, any, int)
	emit := func(key, value string) {
		if len(lines) >= supportBundleKVMaxLines {
			return
		}
		// One structured value must not smuggle an unbounded number of output
		// lines past the flattener's line cap. Redact the complete scalar before
		// replacing line breaks: otherwise a literal "\\n" marker can become the
		// apparent value consumed by a keyed rule while the real secret survives.
		value = redactSensitive(value)
		value = strings.ReplaceAll(value, "\r", ` \r `)
		value = strings.ReplaceAll(value, "\n", ` \n `)
		if key == "" {
			key = "value"
		}
		lines = append(lines, key+": "+value)
	}
	joinKey := func(parent, child string) string {
		if parent == "" {
			return child
		}
		if child == "" {
			return parent
		}
		return parent + "." + child
	}
	walk = func(key string, value any, depth int) {
		if len(lines) >= supportBundleKVMaxLines {
			return
		}
		if depth > supportBundleKVMaxDepth {
			emit(key, "[depth limit]")
			return
		}
		switch x := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(joinKey(key, k), x[k], depth+1)
			}
		case []any:
			parts := make([]string, 0, len(x))
			allScalar := true
			for _, item := range x {
				if _, ok := item.(map[string]any); ok {
					allScalar = false
					break
				}
				if _, ok := item.([]any); ok {
					allScalar = false
					break
				}
				parts = append(parts, supportBundleScalar(item))
			}
			if allScalar {
				emit(key, strings.Join(parts, ", "))
				return
			}
			for i, item := range x {
				walk(joinKey(key, strconv.Itoa(i)), item, depth+1)
			}
		default:
			emit(key, supportBundleScalar(x))
		}
	}
	walk(strings.TrimSpace(prefix), decoded, 0)
	if len(lines) == supportBundleKVMaxLines {
		lines[len(lines)-1] = "bundle.note: [line limit]"
	}
	return strings.Join(lines, "\n")
}

func supportBundleScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func (a *apiServer) buildSupportBundle(paneID string) supportBundleResponse {
	return buildSupportBundle(a, paneID, nil)
}

// buildSupportBundle shares section finalization, privacy filtering and limits
// between the local HTTP API and the offline CLI. Offline collection never
// initializes an API server, stores, updater, or background daemon services.
func buildSupportBundle(a *apiServer, paneID string, offline *supportBundleOffline) supportBundleResponse {
	if a == nil {
		a = &apiServer{}
	}
	now := time.Now().UTC()
	resp := supportBundleResponse{
		Schema:      supportBundleSchema,
		GeneratedAt: now.Format(time.RFC3339),
		Filename:    "soluterminal-support-" + now.Format("2006-01-02-150405") + ".txt",
		Sections:    make([]supportBundleSection, 0, 9),
		Omitted:     append([]string(nil), supportBundleOmitted...),
		LogPaths:    []string{},
	}
	add := func(section supportBundleSection, maxBytes int) {
		resp.Sections = append(resp.Sections, finalizeSupportSection(section, maxBytes))
	}

	if offline != nil {
		for _, section := range offline.sections() {
			add(section, supportBundleSectionMaxBytes)
		}
		for i, path := range offline.logPaths() {
			text, _, note := readSupportBundleLogTail(path)
			text = dropBrowsingLines(text)
			section := supportBundleLogSection(fmt.Sprintf("fileLog%d", i), "Recent "+filepath.Base(path), text, note)
			section.Description = "Bounded tail of " + redactSensitive(path)
			add(section, supportBundleSectionMaxBytes)
			resp.LogPaths = append(resp.LogPaths, redactSensitive(path))
		}
		clampSupportBundleTotal(&resp)
		return resp
	}

	versions := supportBundleSection{
		ID: "versions", Title: "Versions and runtime", Description: "Daemon, desktop shell, web bundle, backend, and runtime identity.", SelectedByDefault: true,
	}
	if supportBundleHasDaemonIdentity(a) {
		web := CurrentWebProvenance()
		exe, _ := os.Executable()
		machineID, machineName := a.feedbackMachineIdentity()
		versions.Text = supportBundleKV("version", map[string]any{
			"daemon": a.ver, "webVersion": web.WebVersion, "webSourceSha": web.SourceSHA,
			"shellVersion": a.liveShellVersion(), "shellPlatform": a.shellPlatform,
			"backendId": a.backendID, "goos": runtime.GOOS, "goarch": runtime.GOARCH,
			"pid": os.Getpid(), "bindAddr": a.addr, "startedAt": a.startT.Format(time.RFC3339Nano),
			"uptimeSec": supportBundleUptime(a.startT), "executable": exe,
			"machineId": machineID, "machineName": machineName,
		})
	} else {
		versions.Note = "Daemon version and runtime identity are unavailable."
	}
	add(versions, supportBundleSectionMaxBytes)

	updates := supportBundleSection{ID: "updates", Title: "Update status", Description: "Cached daemon and desktop update health.", SelectedByDefault: true}
	if a.updater == nil {
		updates.Note = "The updater is disabled in this daemon."
	} else {
		updates.Text = supportBundleKV("update", a.updater.Status())
	}
	add(updates, supportBundleSectionMaxBytes)

	connection := supportBundleSection{ID: "connection", Title: "Connection health", Description: "Relay state and local pane subscriber counts; account identity is omitted.", SelectedByDefault: true}
	if a.cloudSup == nil && a.mgr == nil && strings.TrimSpace(a.addr) == "" && a.startT.IsZero() {
		connection.Note = "Connection health is unavailable."
	} else {
		cloud := cloudStatus{Configured: false, RelayState: relayStateUnconfigured}
		if a.cloudSup != nil {
			cloud = a.cloudSup.status()
		} else {
			connection.Note = "The cloud relay is not configured; local connection details are included."
		}
		cloud.OwnerEmail = ""
		panes, subscribers := supportBundleSubscriberCounts(a.mgr)
		connection.Text = supportBundleKV("connection", map[string]any{
			"cloud": cloud, "daemonUptimeSec": supportBundleUptime(a.startT), "bindAddr": a.addr,
			"livePanes": panes, "livePaneSubscribers": subscribers,
		})
	}
	add(connection, supportBundleSectionMaxBytes)

	system := supportBundleSection{ID: "system", Title: "System health", Description: "Bounded system sampler summary.", SelectedByDefault: true}
	if health := a.systemHealthSummary(); health != nil {
		system.Text = supportBundleKV("system", health)
	} else {
		system.Note = "The system health sampler is disabled."
	}
	add(system, supportBundleSectionMaxBytes)

	claude := supportBundleSection{
		ID: "claude", Title: "Claude status", Description: "CLI probe result without credential paths or tokens.", SelectedByDefault: true,
	}
	if supportBundleHasDaemonIdentity(a) {
		claudeStatus := a.currentClaudeStatus()
		claude.Text = supportBundleKV("claude", map[string]any{
			"checked": claudeStatus.checked, "installed": claudeStatus.installed,
			"version": claudeStatus.version, "authenticated": claudeStatus.authed,
		})
	} else {
		claude.Note = "The Claude CLI probe has not been initialized."
	}
	add(claude, supportBundleSectionMaxBytes)

	sessions := supportBundleSection{ID: "sessions", Title: "Session inventory", Description: "Up to 40 panes with operational fields only; titles, commands, paths, models, prompts, and transcripts are omitted.", SelectedByDefault: true}
	if a.mgr == nil {
		sessions.Note = "The pane manager is unavailable."
	} else {
		panes := a.mgr.List()
		sort.Slice(panes, func(i, j int) bool { return panes[i] != nil && (panes[j] == nil || panes[i].ID < panes[j].ID) })
		if len(panes) > supportBundleMaxPanes {
			panes = panes[:supportBundleMaxPanes]
			sessions.Truncated = true
			sessions.Note = "Only the first 40 panes by id are included."
		}
		lines := make([]string, 0, len(panes))
		for _, pane := range panes {
			if pane == nil {
				continue
			}
			s := pane.Snapshot()
			ageSec := int64(0)
			if !s.CreatedAt.IsZero() {
				ageSec = int64(time.Since(s.CreatedAt).Seconds())
				if ageSec < 0 {
					ageSec = 0
				}
			}
			lines = append(lines, fmt.Sprintf("id=%s status=%s kind=%s agent=%s provider=%s cols=%d rows=%d pid=%d ageSec=%d dimsUnverified=%t",
				s.ID, s.Status, s.Kind, s.Agent, s.Provider, s.Cols, s.Rows, s.Pid, ageSec, s.DimsUnverified))
		}
		sessions.Text = strings.Join(lines, "\n")
		if len(lines) == 0 {
			sessions.Note = "No panes are currently registered."
		}
	}
	add(sessions, supportBundleSectionMaxBytes)

	logs := supportBundleSection{ID: "logs", Title: "Recent daemon logs", Description: "Newest 300 entries from the bounded in-memory diagnostic ring.", SelectedByDefault: true}
	var promptRecords int
	logs.Text, promptRecords = supportBundleWithoutPrivateLogRecords(strings.Join(globalLogRing.tail(supportBundleLogLines), "\n"))
	if promptRecords > 0 {
		logs.Note = fmt.Sprintf("%d prompt-bearing log record(s) were omitted.", promptRecords)
	}
	if logs.Text == "" {
		if logs.Note == "" {
			logs.Note = "The in-memory diagnostic ring is empty."
		}
	}
	add(logs, supportBundleLogBytes)

	durableText, durablePath, durableNote := readSupportBundleDurableLog()
	daemonLog := supportBundleLogSection("daemonLog", "Durable daemon log tail", durableText, durableNote)
	if durablePath != "" {
		resp.LogPaths = append(resp.LogPaths, redactSensitive(durablePath))
	}
	add(daemonLog, supportBundleSectionMaxBytes)

	paneText := supportBundleSection{ID: "paneText", Title: "Focused pane tail", Description: "Optional ANSI-stripped tail: at most 40 lines and 16 KiB from the pane selected when collection started.", SelectedByDefault: false}
	if strings.TrimSpace(paneID) == "" {
		paneText.Note = "No pane was selected."
	} else if a.mgr == nil {
		paneText.Note = "The pane manager is unavailable."
	} else if _, ok := a.mgr.Get(paneID); !ok {
		paneText.Note = "The selected pane no longer exists."
	} else {
		paneText.Text = a.supportBundlePaneSnapshot(paneID)
		if paneText.Text == "" {
			paneText.Note = "The selected pane has no visible text."
		}
	}
	add(paneText, supportBundleSectionMaxBytes)

	clampSupportBundleTotal(&resp)
	return resp
}

func supportBundleHasDaemonIdentity(a *apiServer) bool {
	return a != nil && (strings.TrimSpace(a.ver) != "" || strings.TrimSpace(a.addr) != "" ||
		strings.TrimSpace(a.backendID) != "" || strings.TrimSpace(a.shellVersion) != "" ||
		strings.TrimSpace(a.shellPlatform) != "" || strings.TrimSpace(a.dataDir) != "" || !a.startT.IsZero())
}

var supportBundlePrivateLogMarkers = []string{
	"[native-input]",
	"aria: send_input",
	// The removed todo normalizer logged up to 120 characters of the model
	// reply when its JSON did not parse. That reply routinely echoed the user's
	// private todo text, which token redaction cannot make safe. Older daemon
	// logs still hold those records, so they stay omitted from the bundle.
	"invalid json reply",
}

// supportBundleWithoutPrivateLogRecords removes whole records known to carry
// submitted user text. Token redaction cannot make ordinary prompt prose safe,
// so prompt-bearing records never enter either default log section.
func supportBundleWithoutPrivateLogRecords(text string) (string, int) {
	if text == "" {
		return "", 0
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	omitted := 0
	for _, line := range lines {
		private := false
		for _, marker := range supportBundlePrivateLogMarkers {
			if strings.Contains(line, marker) {
				private = true
				break
			}
		}
		if private {
			omitted++
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), omitted
}

// supportBundleRedactDurableLog decodes the quoted message values emitted by
// slog before redaction. Redacting the serialized record alone is insufficient:
// both TextHandler and JSONHandler escape quotes inside messages, hiding JSON
// credential labels from the ordinary redaction rules.
func supportBundleRedactDurableLog(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		var record any
		if json.Unmarshal([]byte(line), &record) == nil {
			record = supportBundleRedactLogValue(record)
			if encoded, err := json.Marshal(record); err == nil {
				lines[i] = redactSensitive(string(encoded))
				continue
			}
		}
		lines[i] = redactSensitive(supportBundleRedactSlogTextValues(line))
	}
	return strings.Join(lines, "\n")
}

func supportBundleRedactLogValue(value any) any {
	switch x := value.(type) {
	case string:
		return redactSensitive(x)
	case []any:
		for i := range x {
			x[i] = supportBundleRedactLogValue(x[i])
		}
	case map[string]any:
		for key := range x {
			x[key] = supportBundleRedactLogValue(x[key])
		}
	}
	return value
}

func supportBundleRedactSlogTextValues(line string) string {
	var out strings.Builder
	last := 0
	for i := 0; i+1 < len(line); i++ {
		if line[i] != '=' || line[i+1] != '"' {
			continue
		}
		start := i + 1
		end := start + 1
		escaped := false
		for ; end < len(line); end++ {
			switch {
			case escaped:
				escaped = false
			case line[end] == '\\':
				escaped = true
			case line[end] == '"':
				decoded, err := strconv.Unquote(line[start : end+1])
				if err != nil {
					end = len(line)
					break
				}
				out.WriteString(line[last:start])
				out.WriteString(strconv.Quote(redactSensitive(decoded)))
				last = end + 1
				i = end
				end = len(line)
			}
		}
	}
	if last == 0 {
		return line
	}
	out.WriteString(line[last:])
	return out.String()
}

func (a *apiServer) supportBundlePaneSnapshot(paneID string) string {
	if a == nil || a.mgr == nil {
		return ""
	}
	p, ok := a.mgr.Get(paneID)
	if !ok || p == nil {
		return ""
	}
	text := redactSensitive(stripANSI(p.buffer.Snapshot()))
	text = feedbackTailLines(text, feedbackMaxSnapshotLines)
	return supportBundleTailBytes(text, feedbackMaxSnapshotBytes)
}

func supportBundleUptime(start time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	seconds := int64(time.Since(start).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

func supportBundleSubscriberCounts(mgr *Manager) (panes, subscribers int) {
	if mgr == nil {
		return 0, 0
	}
	for _, pane := range mgr.List() {
		if pane == nil {
			continue
		}
		pane.subsMu.RLock()
		count := len(pane.subs)
		pane.subsMu.RUnlock()
		if count > 0 {
			panes++
			subscribers += count
		}
	}
	return panes, subscribers
}

// supportBundleLogSection is shared by HTTP and offline log collection,
// including decoding slog values and dropping prompt-bearing records.
func supportBundleLogSection(id, title, text, note string) supportBundleSection {
	text, omitted := supportBundleWithoutPrivateLogRecords(supportBundleRedactDurableLog(text))
	if omitted > 0 {
		note = strings.TrimSpace(note + fmt.Sprintf(" %d prompt-bearing log record(s) were omitted.", omitted))
	}
	return supportBundleSection{ID: id, Title: title, Description: "Newest bounded tail of a local diagnostic log.", Text: text, Note: note, SelectedByDefault: true}
}

func readSupportBundleDurableLog() (text, path, note string) {
	dir := strings.TrimSpace(os.Getenv("SOLU_DURABLE_LOG_DIR"))
	if strings.EqualFold(dir, "off") {
		return "", "", "Durable logging is disabled by SOLU_DURABLE_LOG_DIR=off."
	}
	if dir == "" {
		// Without an override, the log this daemon opened at boot is the
		// authoritative answer: a Windows daemon given a non-default --data
		// dir logs under it, not in the shell data dir.
		if durableLog != nil && durableLog.path != "" {
			return readSupportBundleLogTail(durableLog.path)
		}
		resolved, err := soluShellDataDir()
		if err != nil {
			return "", "", "The daemon log directory could not be resolved."
		}
		dir = resolved
	}
	return readSupportBundleLogTail(filepath.Join(dir, durableLogName))
}

func readSupportBundleLogTail(path string) (text, logPath, note string) {
	// Refuse special files before opening (in particular FIFOs) so diagnostics
	// do not hang on a broken installation. Symlinks are deliberately omitted.
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return "", path, "The log is missing or is not a readable regular file."
	}
	f, err := os.Open(path)
	if err != nil {
		return "", path, "The log is not available."
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", path, "The log is not a readable regular file."
	}
	start := st.Size() - supportBundleFileTailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", path, "The log tail could not be read."
	}
	b, err := io.ReadAll(io.LimitReader(f, supportBundleFileTailBytes))
	if err != nil {
		return "", path, "The log tail could not be read."
	}
	if start > 0 {
		if newline := strings.IndexByte(string(b), '\n'); newline >= 0 {
			b = b[newline+1:]
		} else {
			b = nil
		}
	}
	text = feedbackTailLines(string(b), supportBundleFileTailLines)
	if strings.TrimSpace(text) == "" {
		note = "The log is empty."
	}
	return text, path, note
}

func clampSupportBundleTotal(resp *supportBundleResponse) {
	for {
		total := 0
		largest := -1
		for i := range resp.Sections {
			total += resp.Sections[i].Bytes
			if largest < 0 || resp.Sections[i].Bytes > resp.Sections[largest].Bytes {
				largest = i
			}
		}
		if total <= supportBundleMaxTotalBytes || largest < 0 {
			return
		}
		over := total - supportBundleMaxTotalBytes
		section := &resp.Sections[largest]
		target := section.Bytes - over
		if target < 0 {
			target = 0
		}
		section.Text = supportBundleTailBytes(section.Text, target)
		section.Bytes = len(section.Text)
		section.Empty = strings.TrimSpace(section.Text) == ""
		if section.Empty {
			section.Lines = 0
		} else {
			section.Lines = strings.Count(strings.TrimRight(section.Text, "\n"), "\n") + 1
		}
		section.Truncated = true
	}
}

// dropBrowsingLines removes the desktop shell's external-window log lines,
// which record the full URL of every page the user opened (browsing history,
// sometimes with OAuth codes) and carry no diagnostic value for a bundle.
func dropBrowsingLines(text string) string {
	if !strings.Contains(text, "open_external_window") {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.Contains(l, "open_external_window") {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}
