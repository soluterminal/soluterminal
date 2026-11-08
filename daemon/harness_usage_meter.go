package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"soluterminal-shared/deepseekusage"
)

// Self-metered DeepSeek spend from DSH's own durable session logs.
//
// The official CSV import is the authoritative figure, but it only refreshes
// when the owner downloads it. Between imports we read the DSH session logs
// ($DSH_HOME/sessions/**/session*.jsonl.zstd), which persist one
// assistant/message per billed turn with the provider's token buckets, price
// each turn with shared/deepseekusage, and attribute it to the harness account
// that launched the session (a sidecar the terminal client writes). This is an
// estimate to reconcile against the export, not a replacement for it.
//
// Session logs are concatenated zstd frames, so stdlib/Node decoders stop after
// the first frame; klauspost/compress/zstd reads them whole.

const deepSeekMeterTTL = 60 * time.Second

type deepSeekMeterCacheState struct {
	mu   sync.Mutex
	key  string
	at   time.Time
	data map[string]deepseekusage.KeyUsage
}

var deepSeekMeterCache deepSeekMeterCacheState

// deepSeekSessionsDir / deepSeekAccountsDir are the on-disk locations the
// launcher uses (DSH_HOME is <dataDir>/harness-state/deepseek).
func deepSeekSessionsDir(dataDir string) string {
	return filepath.Join(dataDir, "harness-state", "deepseek", "sessions")
}
func deepSeekAccountsDir(dataDir string) string {
	return filepath.Join(dataDir, "harness-state", "deepseek", "session-accounts")
}

// cachedDeepSeekMeter memoizes a scan for deepSeekMeterTTL so a dashboard poll
// does not re-decompress every historical session.
func cachedDeepSeekMeter(dataDir string) map[string]deepseekusage.KeyUsage {
	sessionsDir, accountsDir := deepSeekSessionsDir(dataDir), deepSeekAccountsDir(dataDir)
	key := sessionsDir + "\x00" + accountsDir
	deepSeekMeterCache.mu.Lock()
	defer deepSeekMeterCache.mu.Unlock()
	if deepSeekMeterCache.key == key && time.Since(deepSeekMeterCache.at) < deepSeekMeterTTL {
		return deepSeekMeterCache.data
	}
	data, err := scanDeepSeekMeter(sessionsDir, accountsDir)
	if err != nil {
		data = map[string]deepseekusage.KeyUsage{}
	}
	// Workflow runs keep private invocation-owned journals outside exported
	// task logs; include both disjoint harness sources in the same meter.
	for account, usage := range scanWorkflowDeepSeekMeter(dataDir) {
		foldKeyUsage(data, account, usage)
	}
	deepSeekMeterCache.key, deepSeekMeterCache.at, deepSeekMeterCache.data = key, time.Now(), data
	return data
}

// scanDeepSeekMeter walks every session log and returns per-account usage.
func scanDeepSeekMeter(sessionsDir, accountsDir string) (map[string]deepseekusage.KeyUsage, error) {
	out := map[string]deepseekusage.KeyUsage{}
	workspaces, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	for _, workspace := range workspaces {
		if !workspace.IsDir() {
			continue
		}
		sessions, err := os.ReadDir(filepath.Join(sessionsDir, workspace.Name()))
		if err != nil {
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			file := sessionLogPath(filepath.Join(sessionsDir, workspace.Name(), session.Name()))
			if file == "" {
				continue
			}
			account, model := readSessionAccount(accountsDir, session.Name())
			usage, err := scanSessionUsage(file, model)
			if err != nil {
				continue
			}
			if usage.CacheHitInputTokens == 0 && usage.CacheMissInputTokens == 0 && usage.OutputTokens == 0 {
				continue
			}
			foldKeyUsage(out, accountLabel(account), usage)
		}
	}
	return out, nil
}

// sessionLogPath returns the newest session-log file for a session directory.
func sessionLogPath(dir string) string {
	for _, name := range []string{"session.v3.jsonl.zstd", "session.jsonl.zstd"} {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

type sessionAccountSidecar struct {
	Account string `json:"account"`
	Model   string `json:"model"`
}

// readSessionAccount reads the launcher's session→account sidecar. A missing
// sidecar yields ("", "") so the turn is grouped as unattributed rather than
// silently credited to the wrong key.
func readSessionAccount(accountsDir, sessionID string) (account, model string) {
	b, err := os.ReadFile(filepath.Join(accountsDir, sessionID+".json"))
	if err != nil || len(b) > 4<<10 {
		return "", ""
	}
	var sidecar sessionAccountSidecar
	if json.Unmarshal(b, &sidecar) != nil {
		return "", ""
	}
	return sidecar.Account, sidecar.Model
}

func accountLabel(account string) string {
	if account == "" {
		return "unattributed"
	}
	return account
}

// usageScanLine sees each decoded line of a DSH session log before it is
// parsed. A test seam only: tests make it panic to prove a bad log can't
// panic the daemon.
var usageScanLine = func([]byte) {}

// dshUsageEvent is the subset of a DSH session event the meter needs.
type dshUsageEvent struct {
	Type string          `json:"type"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

// scanSessionUsage sums one session's billed turns. The model is tracked from
// request/header so a mid-session /model switch prices correctly; fallbackModel
// (the sidecar's) covers logs with no header.
//
// A corrupt or crafted log must not take the daemon down: a panic while
// decoding or reading it (as GO-2026-6865 did in the zstd decoder) is returned
// as a read error.
func scanSessionUsage(file, fallbackModel string) (usage deepseekusage.KeyUsage, err error) {
	defer func() {
		if r := recover(); r != nil {
			usage, err = deepseekusage.KeyUsage{}, fmt.Errorf("read %s: %v", filepath.Base(file), r)
		}
	}()
	f, err := os.Open(file)
	if err != nil {
		return deepseekusage.KeyUsage{}, err
	}
	defer f.Close()
	// One decoder goroutine, the caller's: with more, the decoder works in
	// goroutines of its own, where a panic would escape the recover above.
	reader, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return deepseekusage.KeyUsage{}, err
	}
	defer reader.Close()

	model := fallbackModel
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for scanner.Scan() {
		usageScanLine(scanner.Bytes())
		var event dshUsageEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch event.Type {
		case "request/header":
			var payload struct {
				Header struct {
					Config struct {
						Model string `json:"model"`
					} `json:"config"`
				} `json:"header"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && payload.Header.Config.Model != "" {
				model = payload.Header.Config.Model
			}
		case "assistant/message":
			var payload struct {
				Usage *struct {
					InputTokens      int64 `json:"inputTokens"`
					OutputTokens     int64 `json:"outputTokens"`
					CacheReadTokens  int64 `json:"cacheReadTokens"`
					CacheWriteTokens int64 `json:"cacheWriteTokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(event.Data, &payload) != nil || payload.Usage == nil {
				continue
			}
			tokens := deepseekusage.Tokens{
				CacheHitInputTokens:  payload.Usage.CacheReadTokens + payload.Usage.CacheWriteTokens,
				CacheMissInputTokens: payload.Usage.InputTokens,
				OutputTokens:         payload.Usage.OutputTokens,
			}
			cost, priced := deepseekusage.Cost(model, tokens, time.UnixMilli(event.Time))
			usage.CacheHitInputTokens += tokens.CacheHitInputTokens
			usage.CacheMissInputTokens += tokens.CacheMissInputTokens
			usage.OutputTokens += tokens.OutputTokens
			if priced {
				usage.Cost += cost
			} else {
				usage.UnpricedTokens += tokens.CacheHitInputTokens + tokens.CacheMissInputTokens + tokens.OutputTokens
			}
		}
	}
	return usage, scanner.Err()
}

// Invocation-owned roots preserve attribution without global counter deltas.
func scanWorkflowDeepSeekMeter(dataDir string) map[string]deepseekusage.KeyUsage {
	out := map[string]deepseekusage.KeyUsage{}
	roots, _ := filepath.Glob(filepath.Join(dataDir, "harness-state", "deepseek", "workflow-sessions", "*"))
	for _, root := range roots {
		raw, err := os.ReadFile(filepath.Join(root, "account.json"))
		if err != nil {
			continue
		}
		var identity struct {
			Account string `json:"account"`
			Model   string `json:"model"`
		}
		if json.Unmarshal(raw, &identity) != nil {
			continue
		}
		sessions, _ := filepath.Glob(filepath.Join(root, "*", "*"))
		// Fork logs can include inherited billed messages. Do not double count.
		if len(sessions) != 1 {
			continue
		}
		file := sessionLogPath(sessions[0])
		if file == "" {
			continue
		}
		usage, err := scanSessionUsage(file, identity.Model)
		if err != nil {
			continue
		}
		foldKeyUsage(out, accountLabel(identity.Account), usage)
	}
	return out
}
