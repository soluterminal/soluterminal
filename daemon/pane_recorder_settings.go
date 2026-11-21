package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The person's recording settings: how long a recording is kept and the total
// cap. Both always exist and stay within the policy bounds; a value outside
// them is refused, never clamped. Raising one is the person's only (the
// switch-on gate) and applies to every file. Lowering one is anyone's for
// recordings made from then on; only the person's lowering deletes older
// recordings sooner (Addenda C 2 and D): after another caller's lowering,
// each finished file keeps the age and cap in force when it finished, kept as
// a dated history in the settings file.

const (
	recordingSettingsFile     = "recording-settings.json"
	recordingSettingsFileMode = 0o600
	recordingSettingsMaxBody  = 1 << 10
	// recordingSettingsHistoryMax bounds the age history (Addendum D a).
	recordingSettingsHistoryMax = 16
)

type recordingSettings struct {
	DeleteAfterDays int `json:"deleteAfterDays"`
	CapMB           int `json:"capMB"`
}

// recordingSettingsEra: files finished (last written) before Until keep
// these values. By is the lowering caller's kind: agent, relay or program.
type recordingSettingsEra struct {
	Until int64 `json:"until"`
	recordingSettings
	By string `json:"by"`
}

type recordingSettingsFileData struct {
	recordingSettings
	History []recordingSettingsEra `json:"history,omitempty"`
}

func defaultRecordingSettings() recordingSettings {
	return recordingSettings{DeleteAfterDays: recordingDeleteAfterDaysDefault, CapMB: recordingCapMBDefault}
}

func (s recordingSettings) valid() error {
	if s.DeleteAfterDays < recordingDeleteAfterDaysMin || s.DeleteAfterDays > recordingDeleteAfterDaysMax {
		return fmt.Errorf("recordings are kept %d to %d days", recordingDeleteAfterDaysMin, recordingDeleteAfterDaysMax)
	}
	if s.CapMB < recordingCapMBMin || s.CapMB > recordingCapMBMax {
		return fmt.Errorf("the recordings cap is %d to %d MB", recordingCapMBMin, recordingCapMBMax)
	}
	return nil
}

func (s recordingSettings) maxAge() time.Duration {
	return time.Duration(s.DeleteAfterDays) * 24 * time.Hour
}

func (s recordingSettings) totalCap() int64 { return int64(s.CapMB) * 1_000_000 }

func validEraBy(by string) bool { return by == "agent" || by == "relay" || by == "program" }

// loadRecordingSettings reads the settings file; a missing, unreadable or
// out-of-bounds one gives the defaults. A history entry out of bounds is
// dropped.
func loadRecordingSettings(dataDir string) (recordingSettings, []recordingSettingsEra) {
	b, err := os.ReadFile(filepath.Join(dataDir, recordingSettingsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return defaultRecordingSettings(), nil
	}
	var d recordingSettingsFileData
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	if err == nil {
		err = d.recordingSettings.valid()
	}
	if err != nil {
		log.Printf("recording: settings file ignored, using the defaults: %v", err)
		return defaultRecordingSettings(), nil
	}
	var history []recordingSettingsEra
	for _, e := range d.History {
		if e.valid() == nil && e.Until > 0 && validEraBy(e.By) && len(history) < recordingSettingsHistoryMax {
			history = append(history, e)
		}
	}
	sort.Slice(history, func(i, j int) bool { return history[i].Until < history[j].Until })
	// Item 3 of Addendum C: say when the values in force aren't the defaults.
	if d.recordingSettings != defaultRecordingSettings() || len(history) > 0 {
		log.Printf("recording: settings loaded: deleteAfterDays=%d capMB=%d (defaults %d, %d); %d older entries",
			d.DeleteAfterDays, d.CapMB, recordingDeleteAfterDaysDefault, recordingCapMBDefault, len(history))
	}
	return d.recordingSettings, history
}

// settings returns the hub's current settings.
func (h *recordingHub) settings() recordingSettings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current
}

// recordingAgeFor is the deletion age a recording started at start keeps
// while it runs (for its status): the longer of the one in force at its start
// and now.
func (h *recordingHub) recordingAgeFor(start time.Time) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return int(max(h.ageAtLocked(start), h.maxAge) / (24 * time.Hour))
}

// ageForLocked and capForLocked are the deletion age and cap a file keeps:
// the more protective (longer age, larger cap) of those in force when it
// started (its name) and when it finished (its last write), so a lowering by
// another caller during a recording never shortens it (Addendum E 3). A
// person's lowering clears the history, so it applies at once.
func (h *recordingHub) ageForLocked(f recordingFileInfo) time.Duration {
	age := h.ageAtLocked(f.mtime)
	if start, ok := recordingTrustedStart(f); ok {
		age = max(age, h.ageAtLocked(start))
	}
	return age
}

func (h *recordingHub) capForLocked(f recordingFileInfo) int64 {
	cap := h.capAtLocked(f.mtime)
	if start, ok := recordingTrustedStart(f); ok {
		cap = max(cap, h.capAtLocked(start))
	}
	return cap
}

func (h *recordingHub) ageAtLocked(t time.Time) time.Duration {
	if e := h.eraOfLocked(t); e != nil {
		return e.maxAge()
	}
	return h.maxAge
}

func (h *recordingHub) capAtLocked(t time.Time) int64 {
	if e := h.eraOfLocked(t); e != nil {
		return e.totalCap()
	}
	return h.totalCap
}

// recordingStartSlack: a name's start may be at most the longest recording
// (the time limit's maximum) plus this before the file's last write
// (Addendum F 3).
const recordingStartSlack = time.Hour

// recordingTrustedStart is the start time a file's name gives, when it is
// plausible: not after its last write, and not earlier than its last write
// minus the longest possible recording plus an hour. Otherwise the name is
// ignored and only the last write counts (Addendum F 3).
func recordingTrustedStart(f recordingFileInfo) (time.Time, bool) {
	start, ok := recordingFileStart(f.path)
	if !ok || start.After(f.mtime) ||
		start.Before(f.mtime.Add(-(time.Duration(recordingHoursMax)*time.Hour + recordingStartSlack))) {
		return time.Time{}, false
	}
	return start, true
}

// recordingFileStart reads a recording's UTC start time from its name:
// <start>.cast, or <start>_<part>.cast for a part after a roll-over.
func recordingFileStart(path string) (time.Time, bool) {
	name := filepath.Base(path)
	if i := strings.IndexAny(name, "._"); i > 0 {
		name = name[:i]
	}
	t, err := time.Parse(recordingFileTimeLayout, name)
	return t, err == nil
}

func (h *recordingHub) eraOfLocked(mtime time.Time) *recordingSettingsEra {
	ms := mtime.UnixMilli()
	for i := range h.history {
		if ms < h.history[i].Until {
			return &h.history[i]
		}
	}
	return nil
}

// changeSettings saves and applies new settings. person says the change came
// from the person at this computer: then a lowering applies to every file at
// once (per field: what the person lowered) and the history follows;
// another caller's lowering applies to
// recordings made from now on, and older files keep their values. A raise
// (the person's only) applies to every file. by is the caller's kind for the
// history and the disk-cap notice.
func (h *recordingHub) changeSettings(next recordingSettings, person bool, by string) error {
	if err := next.valid(); err != nil {
		return err
	}
	h.mu.Lock()
	cur := h.current
	history := append([]recordingSettingsEra(nil), h.history...)
	switch {
	case next == cur:
	case person:
		// Per field: what the person lowered applies to every file at once,
		// what they raised keeps every file longer, and a field they left
		// alone keeps the protection the history gives older files.
		for i := range history {
			history[i].DeleteAfterDays = recordingPersonField(history[i].DeleteAfterDays, cur.DeleteAfterDays, next.DeleteAfterDays)
			history[i].CapMB = recordingPersonField(history[i].CapMB, cur.CapMB, next.CapMB)
		}
	default:
		history = append(history, recordingSettingsEra{Until: h.now().UnixMilli(), recordingSettings: cur, By: by})
	}
	history = h.trimHistoryLocked(history, next)
	h.mu.Unlock()
	b, err := json.Marshal(recordingSettingsFileData{recordingSettings: next, History: history})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(h.dataDir, recordingSettingsFile), b, recordingSettingsFileMode); err != nil {
		return err
	}
	h.mu.Lock()
	h.current, h.history = next, history
	h.maxAge, h.totalCap = next.maxAge(), next.totalCap()
	h.mu.Unlock()
	h.sweep()
	return nil
}

// recordingPersonField is a history entry's value after the person changes a
// field from cur to next.
func recordingPersonField(entry, cur, next int) int {
	switch {
	case next < cur:
		return min(entry, next)
	case next > cur:
		return max(entry, next)
	}
	return entry
}

// applySettings is the person's change (tests and callers with no request).
func (h *recordingHub) applySettings(s recordingSettings) error { return h.changeSettings(s, true, "") }

// trimHistoryLocked drops entries no remaining file needs, and entries that
// keep nothing longer than the current values; past the bound it drops the
// oldest entry no file needs, and if every entry is needed it merges the two
// oldest, keeping the larger values (never deleting anything sooner).
func (h *recordingHub) trimHistoryLocked(history []recordingSettingsEra, cur recordingSettings) []recordingSettingsEra {
	files, _ := h.listFilesLocked()
	needed := func(i int) bool {
		from := int64(0)
		if i > 0 {
			from = history[i-1].Until
		}
		for _, f := range files {
			if ms := f.mtime.UnixMilli(); ms >= from && ms < history[i].Until {
				return true
			}
			if start, ok := recordingTrustedStart(f); ok {
				if ms := start.UnixMilli(); ms >= from && ms < history[i].Until {
					return true
				}
			}
		}
		return false
	}
	var kept []recordingSettingsEra
	for i, e := range history {
		if (e.DeleteAfterDays > cur.DeleteAfterDays || e.CapMB > cur.CapMB) && needed(i) {
			kept = append(kept, e)
		}
	}
	for len(kept) > recordingSettingsHistoryMax {
		a, b := kept[0], kept[1]
		b.DeleteAfterDays = max(a.DeleteAfterDays, b.DeleteAfterDays)
		b.CapMB = max(a.CapMB, b.CapMB)
		kept = append([]recordingSettingsEra{b}, kept[2:]...)
	}
	return kept
}

// capStopReason is the stop reason when the cap can't be met: it names the
// latest lowering by another caller when older recordings it protects are
// what fills the cap.
func (h *recordingHub) capStopReasonLocked() string {
	if !h.capProtected || len(h.history) == 0 {
		return recordingStopDiskCap
	}
	e := h.history[len(h.history)-1]
	who := map[string]string{"agent": "an agent", "relay": "the phone or the cloud", "program": "another program"}[e.By]
	return fmt.Sprintf("%s (lowered by %s at %s UTC)", recordingStopDiskCap, who, time.UnixMilli(e.Until).UTC().Format("2006-01-02 15:04"))
}

func (h *recordingHub) capStopReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.capStopReasonLocked()
}

type recordingSettingsDTO struct {
	recordingSettings
	Bounds recordingSettingsBounds `json:"bounds"`
}

type recordingSettingsBounds struct {
	DeleteAfterDaysMin int `json:"deleteAfterDaysMin"`
	DeleteAfterDaysMax int `json:"deleteAfterDaysMax"`
	CapMBMin           int `json:"capMBMin"`
	CapMBMax           int `json:"capMBMax"`
	HoursMin           int `json:"hoursMin"`
	HoursMax           int `json:"hoursMax"`
	HoursDefault       int `json:"hoursDefault"`
}

func recordingSettingsSnapshot(s recordingSettings) recordingSettingsDTO {
	return recordingSettingsDTO{recordingSettings: s, Bounds: recordingSettingsBounds{
		DeleteAfterDaysMin: recordingDeleteAfterDaysMin, DeleteAfterDaysMax: recordingDeleteAfterDaysMax,
		CapMBMin: recordingCapMBMin, CapMBMax: recordingCapMBMax,
		HoursMin: recordingHoursMin, HoursMax: recordingHoursMax, HoursDefault: recordingHoursDefault,
	}}
}

// recordingCallerKind names a non-person caller for the history: relay,
// agent (a pane or worker) or program.
func recordingCallerKind(r *http.Request, id callerIdentity) string {
	switch {
	case requestViaRelay(r):
		return "relay"
	case id.Kind == callerPane || id.Kind == callerWorker:
		return "agent"
	}
	return "program"
}

// getRecordingSettings: anyone may read the values.
func (a *apiServer) getRecordingSettings(w http.ResponseWriter, _ *http.Request) {
	hub := a.mgr.recordingHub()
	if hub == nil {
		http.Error(w, errRecordingNoDataDir.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, recordingSettingsSnapshot(hub.settings()))
}

// putRecordingSettings changes either value. Each field is compared with the
// stored one: raising any needs the person at this computer; lowering is open,
// and only the person's lowering deletes older recordings sooner.
func (a *apiServer) putRecordingSettings(w http.ResponseWriter, r *http.Request) {
	hub := a.mgr.recordingHub()
	if hub == nil {
		http.Error(w, errRecordingNoDataDir.Error(), http.StatusServiceUnavailable)
		return
	}
	// expect* are the values the page read before the person decided: when
	// sent, the change applies only if they are still the stored values, so a
	// raise elsewhere in between can't turn a confirmed raise into an
	// unconfirmed lowering.
	var body struct {
		DeleteAfterDays       *int `json:"deleteAfterDays"`
		CapMB                 *int `json:"capMB"`
		ExpectDeleteAfterDays *int `json:"expectDeleteAfterDays"`
		ExpectCapMB           *int `json:"expectCapMB"`
	}
	if !decodeRecordingJSON(w, r, &body, recordingSettingsMaxBody) {
		return
	}
	if body.DeleteAfterDays == nil && body.CapMB == nil {
		http.Error(w, `send "deleteAfterDays" and/or "capMB"`, http.StatusBadRequest)
		return
	}
	hub.settingsMu.Lock()
	defer hub.settingsMu.Unlock()
	cur := hub.settings()
	if (body.ExpectDeleteAfterDays != nil && *body.ExpectDeleteAfterDays != cur.DeleteAfterDays) ||
		(body.ExpectCapMB != nil && *body.ExpectCapMB != cur.CapMB) {
		http.Error(w, "the recording settings changed since they were read; read them again", http.StatusConflict)
		return
	}
	next := cur
	if body.DeleteAfterDays != nil {
		next.DeleteAfterDays = *body.DeleteAfterDays
	}
	if body.CapMB != nil {
		next.CapMB = *body.CapMB
	}
	if err := next.valid(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	person := false
	if next.DeleteAfterDays > cur.DeleteAfterDays || next.CapMB > cur.CapMB {
		if !a.admitRecordingPerson(w, r, "raise how long recordings are kept or their cap") {
			return
		}
		person = true
	} else {
		person = a.recordingPersonQuiet(r)
	}
	if err := hub.changeSettings(next, person, recordingCallerKind(r, a.identifyCallerQuietly(r))); err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	applies := "from now on"
	if person {
		applies = "at once"
	}
	log.Printf("recording: settings deleteAfterDays %d->%d capMB %d->%d (%s %s), applies %s", cur.DeleteAfterDays, next.DeleteAfterDays,
		cur.CapMB, next.CapMB, recordingTransport(r), a.identifyCallerQuietly(r).Kind, applies)
	writeJSON(w, http.StatusOK, recordingSettingsSnapshot(hub.settings()))
}
