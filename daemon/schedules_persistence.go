package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"soluterminal-shared/providers"
)

// Persistence is JSON on disk, atomic tmp+rename so a crash mid-write
// can't leave us with a truncated file. Caller must hold s.mu.
type scheduleFile struct {
	Schedules []*Schedule `json:"schedules"`
	Order     []string    `json:"order"`
}

func (s *ScheduleStore) saveUnlocked() error {
	var list []json.RawMessage
	for _, id := range s.order {
		if sched, ok := s.items[id]; ok {
			raw, ok := s.opaqueSchedules[id]
			if !ok {
				var err error
				raw, err = json.Marshal(sched)
				if err != nil {
					return err
				}
			}
			list = append(list, raw)
		}
	}
	data, err := json.MarshalIndent(struct {
		Schedules []json.RawMessage `json:"schedules"`
		Order     []string          `json:"order"`
	}{list, s.order}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(s.path, data, 0o600)
}

func (s *ScheduleStore) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var f scheduleFile
	if err := json.Unmarshal(data, &f); err != nil {
		// Don't brick startup on a corrupt/torn file — quarantine and
		// start with no schedules rather than log.Fatalf in main.
		quarantineCorruptState(s.path, err)
		return nil
	}
	var rawFile struct {
		Schedules []json.RawMessage `json:"schedules"`
	}
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return err
	}
	s.opaqueSchedules = make(map[string]json.RawMessage)
	migrated := false
	for i, sched := range f.Schedules {
		if !scheduleAvailable(sched) {
			s.opaqueSchedules[sched.ID] = rawFile.Schedules[i]
			s.items[sched.ID] = sched
			continue
		}
		// Schedule task models target Claude today. A stale Pro identifier is
		// therefore unsafe rather than recoverable; disable it before cron wiring
		// instead of handing it to an unrelated provider CLI.
		if sched.Enabled && providers.HasRetiredDeepSeekReference(sched.Model) {
			sched.Enabled = false
			sched.Runs = append([]ScheduleRun{{Status: "error", Detail: "disabled: retired DeepSeek model"}}, sched.Runs...)
			if len(sched.Runs) > maxRunHistory {
				sched.Runs = sched.Runs[:maxRunHistory]
			}
			sched.ErrorCount++
			migrated = true
		}
		s.items[sched.ID] = sched
	}
	if len(f.Order) > 0 {
		s.order = f.Order
	} else {
		// Back-fill order for files saved before we tracked it.
		for _, sched := range f.Schedules {
			s.order = append(s.order, sched.ID)
		}
	}
	if migrated {
		if err := s.saveUnlocked(); err != nil {
			return err
		}
	}
	return nil
}

func generateScheduleID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sched_" + hex.EncodeToString(b)
}

func defaultName(name, prompt string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	// Derive a terse label from the first line of the prompt — makes
	// the sidebar glanceable even when the user hasn't bothered naming.
	firstLine := strings.SplitN(strings.TrimSpace(prompt), "\n", 2)[0]
	if len(firstLine) > 48 {
		firstLine = firstLine[:45] + "…"
	}
	if firstLine == "" {
		firstLine = "unnamed"
	}
	return firstLine
}

func displayTitle(p *Pane) string {
	snap := p.Snapshot()
	if snap.Title != "" {
		return snap.Title
	}
	if len(snap.Cmd) > 0 {
		return snap.Cmd[0]
	}
	return snap.ID
}

func removeString(xs []string, x string) []string {
	for i, v := range xs {
		if v == x {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
}
