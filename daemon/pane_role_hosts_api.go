package main

import (
	"net/http"
	"os"
	"strings"
	"time"
)

type orchestratorHostAvailability struct {
	Agent     string `json:"agent"`
	Label     string `json:"label"`
	Supported bool   `json:"supported"`
	Installed bool   `json:"installed"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type orchestratorHostsDTO struct {
	PolicyVersion string                         `json:"policyVersion"`
	Hosts         []orchestratorHostAvailability `json:"hosts"`
}

type cachedOrchestratorHostAvailability struct {
	value orchestratorHostAvailability
	at    time.Time
}

func orchestratorHostLabel(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "", agentClaude:
		return "Claude"
	case agentCodex:
		return "Codex"
	case agentMuse:
		return "Muse"
	default:
		return strings.TrimSpace(agent)
	}
}

func (a *apiServer) orchestratorHostAvailability(agent string) orchestratorHostAvailability {
	if a != nil && a.orchestratorHostProbe != nil {
		return a.orchestratorHostProbe(agent)
	}
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		agent = agentClaude
	}
	label := orchestratorHostLabel(agent)
	base := orchestratorHostAvailability{Agent: agent, Label: label, Supported: orchestratorHostSupported(agent)}
	if !base.Supported && agent != agentMuse {
		base.Reason = orchestratorHostUnsupportedReason(agent)
		return base
	}
	installedKnown := false
	if a != nil {
		a.orchestratorHostCacheMu.Lock()
		if cached, ok := a.orchestratorHostCache[agent]; ok && time.Since(cached.at) < time.Minute {
			if agent != agentCodex {
				a.orchestratorHostCacheMu.Unlock()
				return cached.value
			}
			base.Installed = cached.value.Installed
			installedKnown = true
		}
		a.orchestratorHostCacheMu.Unlock()
	}

	if !installedKnown {
		var err error
		switch agent {
		case agentCodex:
			_, err = resolveCodexBin()
		case agentMuse:
			_, err = resolveMuseBin()
		default:
			_, err = resolveClaudeBin()
		}
		base.Installed = err == nil
	}
	if !base.Supported {
		base.Reason = orchestratorHostUnsupportedReason(agent)
	} else if !base.Installed {
		base.Reason = label + " CLI not found on this machine (PATH, ~/.local/bin, nvm, asdf)"
	} else if agent == agentCodex {
		path, pathErr := codexConfigPath()
		body, readErr := os.ReadFile(path)
		registered := pathErr == nil && readErr == nil && codexConfigMcpURL(string(body), "soluterminal") != "" &&
			codexConfigPaneHeaderState(string(body)) == codexPaneHeaderOurs
		if !registered {
			// Name this daemon's own address: it is not 8787 on every install.
			addr := "127.0.0.1:8787"
			if a != nil && a.addr != "" {
				addr = a.addr
			}
			mcpURL := loopbackMCPURL(addr)
			host := strings.TrimSuffix(strings.TrimPrefix(mcpURL, "http://"), "/mcp")
			base.Reason = "soluterminal is not registered as a Codex MCP server — restart the daemon on " + host + " or run: codex mcp add soluterminal --url " + mcpURL
		} else {
			base.Available = true
		}
	} else {
		base.Available = true
	}
	if a != nil {
		a.orchestratorHostCacheMu.Lock()
		if a.orchestratorHostCache == nil {
			a.orchestratorHostCache = make(map[string]cachedOrchestratorHostAvailability)
		}
		a.orchestratorHostCache[agent] = cachedOrchestratorHostAvailability{value: base, at: time.Now()}
		a.orchestratorHostCacheMu.Unlock()
	}
	return base
}

func (a *apiServer) handleOrchestratorHosts(w http.ResponseWriter, _ *http.Request) {
	hosts := make([]orchestratorHostAvailability, 0, 3)
	for _, agent := range []string{agentClaude, agentCodex, agentMuse} {
		hosts = append(hosts, a.orchestratorHostAvailability(agent))
	}
	writeJSON(w, http.StatusOK, orchestratorHostsDTO{PolicyVersion: orchestratorPolicyVersion, Hosts: hosts})
}

// Compatibility version for installed pane hosts.
const orchestratorPolicyVersion = "12"
