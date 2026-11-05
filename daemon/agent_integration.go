package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Agent integration consent. SoluTerminal can add itself to Claude Code and
// Codex machine-wide: the "soluterminal" MCP server in ~/.claude.json, hooks in
// ~/.claude/settings.json, the server in ~/.codex/config.toml and a marked
// block in ~/.codex/AGENTS.md. Those reach every Claude/Codex session on the
// computer, including ones outside SoluTerminal, so they are opt-in.
//
// The choice lives in <dataDir>/agent_integration.json:
//
//   - "on"  → the four global entries are written (today's behaviour).
//   - "off" → none are written; turning it off removes them. Panes started by
//     SoluTerminal still get the tools and hooks per process (see
//     agent_integration_inject.go), so the app keeps working.
//   - unset → nothing global is written until the user answers the first-run
//     question. Existing installs that already carry the registration are
//     recorded as "on" (source "existing") with a one-time notice, and team
//     installs default to on, so nobody loses working tools on update.
//
// The pane shims run as separate solud processes and read the same file, so
// the daemon also records the facts they cannot compute themselves: the
// operator default and the MCP/hook URLs of the daemon that owns the file.

const agentIntegrationFile = "agent_integration.json"

const (
	agentIntegrationOn  = "on"
	agentIntegrationOff = "off"
	agentIntegrationAsk = "ask"
)

// Sources of a recorded choice.
const (
	agentIntegrationSourceUser      = "user"
	agentIntegrationSourceExisting  = "existing"
	agentIntegrationSourceUninstall = "uninstall"
)

type agentIntegrationRecord struct {
	State         string `json:"state,omitempty"`
	Source        string `json:"source,omitempty"`
	DecidedAtMs   int64  `json:"decidedAtMs,omitempty"`
	NoticePending bool   `json:"noticePending,omitempty"`
	// Written by the daemon at boot for the pane shims.
	OperatorDefault bool   `json:"operatorDefault,omitempty"`
	MCPURL          string `json:"mcpUrl,omitempty"`
	HookURL         string `json:"hookUrl,omitempty"`
}

// effective resolves the record to on / off / ask.
func (r agentIntegrationRecord) effective() string {
	switch r.State {
	case agentIntegrationOn:
		return agentIntegrationOn
	case agentIntegrationOff:
		return agentIntegrationOff
	}
	if r.OperatorDefault {
		return agentIntegrationOn
	}
	return agentIntegrationAsk
}

// needsAnswer reports whether the first-run question should be shown: never
// answered (and not defaulted on), or reset by an uninstall.
func (r agentIntegrationRecord) needsAnswer() bool {
	if r.State == agentIntegrationOff && r.Source == agentIntegrationSourceUninstall {
		return true
	}
	return r.effective() == agentIntegrationAsk
}

type agentIntegrationStore struct {
	mu   sync.Mutex
	path string
}

func newAgentIntegrationStore(dataDir string) *agentIntegrationStore {
	return &agentIntegrationStore{path: filepath.Join(dataDir, agentIntegrationFile)}
}

func readAgentIntegrationRecord(path string) agentIntegrationRecord {
	var rec agentIntegrationRecord
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	switch rec.State {
	case agentIntegrationOn, agentIntegrationOff:
	default:
		rec.State = ""
	}
	return rec
}

func (s *agentIntegrationStore) load() agentIntegrationRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readAgentIntegrationRecord(s.path)
}

// update applies fn to the current record and saves it atomically.
func (s *agentIntegrationStore) update(fn func(*agentIntegrationRecord)) (agentIntegrationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := readAgentIntegrationRecord(s.path)
	fn(&rec)
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return rec, err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return rec, err
	}
	return rec, writeFileAtomicSynced(s.path, b, 0o600)
}

// activeAgentIntegration is set once at daemon start. Nil (unit tests, CLI
// helpers) means "on", which keeps the registration code's old behaviour.
var activeAgentIntegration atomic.Pointer[agentIntegrationStore]

// agentIntegrationGlobalOn decides whether the global registrations may be
// written. It re-reads the file on every call so a choice made by another
// process (the uninstaller's cleanup, a second backend) takes effect without
// a restart; registration runs rarely, so the read is cheap enough.
func agentIntegrationGlobalOn() bool {
	s := activeAgentIntegration.Load()
	if s == nil {
		return true
	}
	return s.load().effective() == agentIntegrationOn
}

// legacyAgentRegistrationPresent reports whether an earlier version already
// wrote a SoluTerminal entry into the user's Claude Code or Codex config.
func legacyAgentRegistrationPresent() bool {
	if url, _, _ := currentClaudeMcpURL(); strings.TrimSpace(url) != "" {
		return true
	}
	if _, data, ok := readClaudeConfig(); ok && claudeEntryIsOurBridge(data) {
		return true
	}
	if cfgPath, err := codexConfigPath(); err == nil {
		if body, err := os.ReadFile(cfgPath); err == nil && codexConfigMcpURL(string(body), "soluterminal") != "" {
			return true
		}
	}
	return false
}

// bootAgentIntegration records what this boot knows: the operator default and
// the daemon URLs for the shims, and — once — an existing install's
// registration as an "on" choice with a notice. Returns the effective state.
//
// fresh says the operator verdict came from the cloud during this run or from
// an env override. A verdict read from disk at boot may be stale (a machine
// restored from an old backup, a copied data dir, a verdict another daemon
// wrote), so it may not lower a recorded operator default: that would turn a
// team user's integration from on to ask and record their existing
// registration as a choice. The lowering waits for a fresh answer
// (settleAgentIntegrationVerdict). Raising it never waits.
func bootAgentIntegration(s *agentIntegrationStore, listenAddr string, operator, fresh bool, legacyPresent func() bool) string {
	rec, err := s.update(func(r *agentIntegrationRecord) {
		deferLowering := !operator && !fresh && r.OperatorDefault
		if !deferLowering {
			r.OperatorDefault = operator
		}
		r.MCPURL = claudeMcpDesiredURL(listenAddr)
		r.HookURL = claudeHookURL(listenAddr)
		if r.State == "" && !r.OperatorDefault && legacyPresent != nil && legacyPresent() {
			r.State = agentIntegrationOn
			r.Source = agentIntegrationSourceExisting
			r.DecidedAtMs = time.Now().UnixMilli()
			r.NoticePending = true
		}
	})
	if err != nil {
		return s.load().effective()
	}
	return rec.effective()
}

// agentIntegrationSettleEvery is how often settleAgentIntegrationVerdict looks
// for a fresh operator verdict.
var agentIntegrationSettleEvery = 15 * time.Second

// settleAgentIntegrationVerdict waits for a fresh operator verdict (verdict
// reports on, known, fresh; see cachedOperatorVerdictFresh) and records it
// once, so a boot that kept a stale-looking operator default (or started in
// ask on a stale non-operator verdict) settles as soon as the cloud answers
// instead of on the next boot.
func settleAgentIntegrationVerdict(ctx context.Context, s *agentIntegrationStore, listenAddr string, verdict func() (on, known, fresh bool), legacyPresent func() bool) {
	for {
		if on, known, fresh := verdict(); known && fresh {
			before := s.load().effective()
			if after := bootAgentIntegration(s, listenAddr, on, true, legacyPresent); after != before {
				log.Printf("agent integration: %s (fresh operator verdict %t)", after, on)
				// The boot-time writers already ran while it was off or
				// ask; write the global entries now instead of on the next
				// boot. Turning to ask removes nothing, as at boot.
				if after == agentIntegrationOn {
					applyAgentIntegrationOn(listenAddr)
				}
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(agentIntegrationSettleEvery):
		}
	}
}
