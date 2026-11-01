package main

import (
	"fmt"
	"strings"
	"time"
)

// Windows retain measured availability separately from predicted availability:
// a future reset must never make a currently exhausted account eligible.
type workerQuotaWindow struct {
	Remaining  int
	ResetEpoch int64
}
type workerAccountKey struct{ Provider, Account, TeamID string }

func (c workerAccount) key() workerAccountKey {
	return workerAccountKey{c.Provider, c.Account, c.TeamID}
}

type workerQuotaScore struct {
	raw, resetting, reserve, usable, leads int
	resetAdjusted                          bool
}

func scoreWorkerQuota(c workerAccount, leads int, settings workerSettings, now time.Time) workerQuotaScore {
	headroom := c.Headroom
	if len(c.Windows) > 0 {
		headroom = 100
		horizon := now.Add(time.Duration(settings.WorkerExpectedRunMinutes) * time.Minute).Unix()
		for _, window := range c.Windows {
			remaining := window.Remaining
			if window.ResetEpoch > now.Unix() && window.ResetEpoch <= horizon {
				remaining = max(remaining, 99)
			}
			headroom = min(headroom, remaining)
		}
	}
	reserve := min(100, max(0, leads)*settings.WorkerLeadReservePct)
	usable := max(0, headroom-reserve)
	// A predicted refill is ranking information, not permission to consume a
	// lead's protected quota before the provider actually resets the window.
	if reserve > 0 {
		usable = min(usable, max(0, c.Headroom-reserve))
	}
	return workerQuotaScore{raw: c.Headroom, resetting: headroom, reserve: reserve, usable: usable, leads: leads, resetAdjusted: headroom > c.Headroom}
}

// pickWorker remains the authority for eligibility, guarded routing, reviewer
// selection and prefer-only ties. This layer supplies comparable usable quota.
func pickWorkerWithPolicy(accounts []workerAccount, prefer string, guarded bool, leads map[workerAccountKey]int, settings workerSettings, now time.Time, requirements ...[]string) (workerInfo, error) {
	needs := requiredWorkerCapabilities(guarded, requirements...)
	var err error
	accounts, err = filterWorkerCapabilities(accounts, needs)
	if err != nil {
		return workerInfo{}, err
	}
	scores := map[workerAccountKey]workerQuotaScore{}
	var usable, resetting []workerAccount
	hasUnreserved := false
	for _, c := range accounts {
		if c.Headroom <= 0 {
			continue
		} // retain pickWorker's current-exhaustion rule
		score := scoreWorkerQuota(c, leads[c.key()], settings, now)
		scores[c.key()] = score
		c.Headroom = score.resetting
		resetting = append(resetting, c)
		c.Headroom = score.usable
		usable = append(usable, c)
		if score.usable > 0 {
			hasUnreserved = true
		}
	}
	candidates := usable
	if !hasUnreserved {
		candidates = resetting
	}
	chosen, err := pickWorker(candidates, prefer, false, needs)
	if err != nil {
		return chosen, err
	}
	key := workerAccountKey{chosen.Provider, chosen.Account, chosen.TeamID}
	score := scores[key]
	rule := "headroom"
	detail := fmt.Sprintf("%d%% usable headroom (%d%% measured, %d%% reset-aware, %d%% reserved for %d running leads)", score.usable, score.raw, score.resetting, score.reserve, score.leads)
	if score.resetAdjusted {
		detail += fmt.Sprintf("; a limiting window resets within the expected %d-minute run", settings.WorkerExpectedRunMinutes)
	}
	if !hasUnreserved && score.reserve > 0 {
		rule = "reserve"
		detail = "using lead reserve because no eligible account has unreserved headroom; " + detail
	} else if withoutReserve, e := pickWorker(resetting, prefer, false, needs); e == nil && (withoutReserve.Provider != chosen.Provider || withoutReserve.Account != chosen.Account || withoutReserve.TeamID != chosen.TeamID) {
		rule = "reserve"
		detail = "protecting lead quota changes the account choice; " + detail
	} else {
		for _, other := range candidates {
			if other.key() == key || other.Headroom <= 0 {
				continue
			}
			if other.Headroom == score.usable {
				rule = "tie-break"
				if prefer != "" && chosen.Provider == prefer && other.Provider != prefer {
					detail = "equal usable headroom; prefer " + prefer + " breaks the tie; " + detail
				} else {
					detail = "equal usable headroom; stable provider/account order breaks the tie; " + detail
				}
				break
			}
		}
	}
	chosen.Reason = rule + ": " + detail
	if guarded {
		chosen.Reason = "guarded: role-guard capability required; " + chosen.Reason
	}
	return chosen, nil
}

// Root agent panes and explicitly delegated orchestrators are leads. Ordinary
// workers do not reserve another slice simply because they are agent panes.
func (a *apiServer) workerLeadCounts() map[workerAccountKey]int {
	counts := map[workerAccountKey]int{}
	panes := a.mgr.List()
	referenced := map[string]bool{}
	for _, p := range panes {
		s := p.Snapshot()
		if s.Status == PaneRunning && s.Origin != nil && s.Origin.LeadPaneID != "" {
			referenced[s.Origin.LeadPaneID] = true
		}
	}
	for _, p := range panes {
		s := p.Snapshot()
		if s.Status != PaneRunning {
			continue
		}
		provider := paneSummaryAgent(p, s)
		if provider != "claude" && provider != "codex" {
			continue
		}
		if s.Origin != nil && s.Origin.Source == paneOriginAgent && s.Role != paneRoleOrchestrator && !referenced[s.ID] {
			continue
		}
		key := workerAccountKey{Provider: provider}
		if w := originWorker(s.Origin); w != nil && w.Provider == provider && w.Account != "" {
			key.Account, key.TeamID = w.Account, w.TeamID
		} else if c := s.Credential; c != nil && c.Provider == provider && c.Account != "" {
			key.Account, key.TeamID = c.Account, c.TeamID
		} else {
			key.Account = a.credentialAccount(provider)
			key.TeamID = a.mgr.selectedCredentialTeam(provider)
		}
		// Team launch labels in the pane projection omit their team__ prefix.
		if key.TeamID != "" && key.Account != "" && !strings.HasPrefix(key.Account, "team__") {
			key.Account = "team__" + key.Account
		}
		if key.Account != "" {
			counts[key]++
		}
	}
	return counts
}

type workerSettingsPatch struct {
	MaxRunning               *int    `json:"maxRunning"`
	FinishAfterMinutes       *int    `json:"finishAfterMinutes"`
	WorkerLeadReservePct     *int    `json:"workerLeadReservePct"`
	WorkerExpectedRunMinutes *int    `json:"workerExpectedRunMinutes"`
	WorkerAutoArchive        *bool   `json:"workerAutoArchive"`
	WorkerAccounts           *string `json:"workerAccounts"`
}

func (s workerSettings) valid() bool {
	return workerAccountsRank(s.WorkerAccounts) >= 0 && s.MaxRunning >= 1 && s.MaxRunning <= 32 && s.FinishAfterMinutes >= 1 && s.FinishAfterMinutes <= 1440 && s.WorkerLeadReservePct >= 0 && s.WorkerLeadReservePct <= 100 && s.WorkerExpectedRunMinutes >= 1 && s.WorkerExpectedRunMinutes <= 1440
}
func (p workerSettingsPatch) apply(s workerSettings) (workerSettings, bool) {
	changed := false
	for _, field := range []struct {
		src *int
		dst *int
	}{{p.MaxRunning, &s.MaxRunning}, {p.FinishAfterMinutes, &s.FinishAfterMinutes}, {p.WorkerLeadReservePct, &s.WorkerLeadReservePct}, {p.WorkerExpectedRunMinutes, &s.WorkerExpectedRunMinutes}} {
		if field.src != nil {
			*field.dst = *field.src
			changed = true
		}
	}
	if p.WorkerAutoArchive != nil {
		s.WorkerAutoArchive = *p.WorkerAutoArchive
		changed = true
	}
	if p.WorkerAccounts != nil {
		s.WorkerAccounts = *p.WorkerAccounts
		changed = true
	}
	return s, changed
}

func workerAccountsRank(scope string) int {
	switch scope {
	case "active":
		return 0
	case "saved":
		return 1
	case "team":
		return 2
	}
	return -1
}

// Enabling archive authorizes automatic removal and returns credential leases. A
// pane may disable that automation, but only a person may enable it again.
// Shorter finish delays admit workers sooner; longer delays retain running
// credentials longer. Neither direction is a purely safer change.
func (s workerSettings) widens(old workerSettings) bool {
	return s.MaxRunning > old.MaxRunning ||
		s.WorkerLeadReservePct < old.WorkerLeadReservePct ||
		workerAccountsRank(s.WorkerAccounts) > workerAccountsRank(old.WorkerAccounts) ||
		(s.WorkerAutoArchive && !old.WorkerAutoArchive) ||
		s.FinishAfterMinutes != old.FinishAfterMinutes ||
		s.WorkerExpectedRunMinutes > old.WorkerExpectedRunMinutes
}
