package main

import (
	"os"
	"path/filepath"
	"time"
)

// SetCredentialSelection also wires panes restored before the API/store setup.
// Each pane reads through the manager so a concurrent spawn cannot retain an
// older callback between construction and registration.
func (m *Manager) SetCredentialSelection(selected func(string) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credentialSelection = selected
	for _, p := range m.panes {
		p.mu.Lock()
		p.credentialSelection = m.selectedCredentialAccount
		p.credentialTeamSelection = m.selectedCredentialTeam
		p.credentialLiveStatus = m.liveCredentialStatus
		p.codexLiveDataDir = m.dataDir
		p.mu.Unlock()
	}
}

func (m *Manager) selectedCredentialTeam(provider string) string {
	if paneRoute.selectedTeam == nil {
		return ""
	}
	return paneRoute.selectedTeam(m, provider)
}

func (p *Pane) credentialTeamSelectionCallback() func(string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.credentialTeamSelection
}

func (m *Manager) selectedCredentialAccount(provider string) string {
	m.mu.RLock()
	selected := m.credentialSelection
	m.mu.RUnlock()
	if selected == nil {
		return ""
	}
	return selected(provider)
}

func (p *Pane) credentialSelectionCallback() func(string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.credentialSelection
}

// paneCredentialInfo records selection at launch, not a claim that a provider
// has confirmed billing. It contains labels only; live selection is projected
// separately and never overwrites the original launch account.
type paneCredentialInfo struct {
	Provider        string `json:"provider"`
	Account         string `json:"account,omitempty"`
	Source          string `json:"source"`
	ActiveAccount   string `json:"activeAccount,omitempty"`
	RestartRequired bool   `json:"restartRequired,omitempty"`
	TeamID          string `json:"teamId,omitempty"`
	ActiveTeamID    string `json:"activeTeamId,omitempty"`
	LiveState       string `json:"liveState,omitempty"`
	Recovery        string `json:"recovery,omitempty"`
	RetryAt         int64  `json:"retryAt,omitempty"`
}

func clonePaneCredential(info *paneCredentialInfo) *paneCredentialInfo {
	if info == nil {
		return nil
	}
	copied := *info
	return &copied
}

func (m *Manager) paneLaunchCredential(opts SpawnOpts) *paneCredentialInfo {
	if m == nil {
		return nil
	}
	provider := ""
	switch {
	case cmdStartsClaude(opts.Cmd):
		provider = "claude"
	case cmdStartsCodex(opts.Cmd):
		provider = "codex"
	case cmdStartsMuse(opts.Cmd):
		provider = "muse"
	default:
		return nil // A CLI later started inside a shell has no launch evidence.
	}
	if provider == "claude" && opts.Provider != "" && opts.Provider != "anthropic" {
		return nil
	}
	source := "launch"
	if paneRouteActive(opts.Env) {
		source = "broker"
	} else {
		// Caller overrides can select an independent login. Do not attribute that
		// process to the global selection without observing the actual identity.
		for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
			if envValueFromList(opts.Env, key) != "" {
				return nil
			}
		}
	}
	account := m.selectedCredentialAccount(provider)
	if source == "broker" && opts.launchRouteLabel != "" && paneRoute.accountLabel != nil {
		account = paneRoute.accountLabel(opts.launchRouteLabel)
	}
	if account == "" {
		return nil
	}
	return &paneCredentialInfo{Provider: provider, Account: account, Source: source, TeamID: opts.launchRouteTeamID}
}

func paneCredentialProjection(recorded *paneCredentialInfo, selected func(string) string, selectedTeam ...func(string) string) *paneCredentialInfo {
	info := clonePaneCredential(recorded)
	if info == nil {
		return nil
	}
	info.ActiveAccount = ""
	info.ActiveTeamID = ""
	info.RestartRequired = false
	if selected == nil {
		return info
	}
	info.ActiveAccount = selected(info.Provider)
	if len(selectedTeam) > 0 && selectedTeam[0] != nil {
		info.ActiveTeamID = selectedTeam[0](info.Provider)
	}
	if info.Account == "" || info.ActiveAccount == "" {
		return info
	}
	if info.Source == "broker" {
		// Proxied requests can follow credential updates without restarting;
		// moving to a local login requires a new CLI launch.
		info.RestartRequired = !paneRouteAccount(info.ActiveAccount)
		if info.TeamID != "" && info.ActiveTeamID != "" {
			info.RestartRequired = info.TeamID != info.ActiveTeamID
		}
	} else if info.Source == "launch" {
		// Claude reloads file-backed OAuth credentials. Launch metadata alone
		// cannot establish whether that reload has occurred. Crossing to a managed
		// transport still requires a new launch for either provider.
		info.RestartRequired = info.Account != info.ActiveAccount && (info.Provider != "claude" || paneRouteAccount(info.ActiveAccount))
	}
	return info
}

// Read-only projection. An old file, replaced process, or disconnected bridge
// is never proof of adoption. Fingerprints stay inside the daemon.
func (m *Manager) liveCredentialStatus(paneID string) codexLiveStatus {
	st := readCodexLiveStatus(m.dataDir, paneID)
	if st.State == "live" && st.Mode != "broker" {
		home, err := os.UserHomeDir()
		if err != nil {
			return codexLiveStatus{}
		}
		live, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
		if err != nil {
			st.State = "needs-attention"
		} else if codexAuthFingerprint(live) != st.Revision {
			st.State = "switch-pending"
		}
	}
	return st
}

func (p *Pane) projectLiveCredential(info *paneCredentialInfo) *paneCredentialInfo {
	if info == nil || info.Provider != "codex" || (info.Source != "launch" && info.Source != "broker") {
		return info
	}
	p.mu.Lock()
	read := p.credentialLiveStatus
	p.mu.Unlock()
	if read == nil {
		return info
	}
	st := read(p.ID)
	if (info.Source == "broker") != (st.Mode == "broker") {
		return info
	}
	if st.PID <= 0 || st.UpdatedAt < p.CreatedAt.UnixMilli() || time.Now().UnixMilli()-st.UpdatedAt > 5000 {
		return info
	}
	owned := false
	for _, pid := range p.ChildPids() {
		if pid == st.PID {
			owned = true
			break
		}
	}
	if !owned {
		return info
	}
	return codexLiveCredentialProjection(info, st)
}

func codexLiveCredentialProjection(info *paneCredentialInfo, st codexLiveStatus) *paneCredentialInfo {
	out := clonePaneCredential(info)
	out.LiveState, out.Recovery, out.RetryAt = st.State, st.Recovery, st.RetryAt
	if info.Source == "broker" {
		return out
	} // preserve team/personal scope checks
	if st.State == "live" || st.State == "switch-pending" || st.State == "switching" {
		out.RestartRequired = false
	}
	return out
}

// credentialAccount is the account label a provider's panes run under ("" when
// unknown): the selected account when a credential manager is built in.
func (a *apiServer) credentialAccount(provider string) string {
	if activeCredentialAccount == nil {
		return ""
	}
	return activeCredentialAccount(a, provider)
}

// activeCredentialAccount resolves the selected account; nil in a build
// without a credential-selection extension.
var activeCredentialAccount func(a *apiServer, provider string) string
