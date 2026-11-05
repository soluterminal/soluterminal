package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUsageCheckpointCoalescesAndDowngrades(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()
			writes := 0
			writer := func(path string, b []byte, mode os.FileMode) error {
				writes++
				if mode != 0o600 || strings.Contains(string(b), "\n  ") {
					t.Fatal("checkpoint mode or formatting changed")
				}
				return writeFileAtomicSynced(path, b, mode)
			}
			var set func(int64)
			var save func(bool) error
			var path string
			if provider == "claude" {
				s := newCursorStore(dir)
				s.checkpoint.now = func() time.Time { return now }
				s.checkpoint.lastSave = now
				s.checkpoint.write = writer
				set = func(n int64) { s.Set("fixture", n, n, n) }
				save, path = s.save, s.path
			} else {
				s := newCodexCursorStore(dir)
				s.checkpoint.now = func() time.Time { return now }
				s.checkpoint.lastSave = now
				s.checkpoint.write = writer
				set = func(n int64) { s.Set("fixture", codexCursor{Offset: n, Size: n, ModTimeUnixNano: n}) }
				save, path = s.save, s.path
			}
			for n := int64(1); n <= 100; n++ {
				set(n)
				if err := save(false); err != nil {
					t.Fatal("save failed")
				}
			}
			if writes != 0 {
				t.Fatal("normal saves were not coalesced")
			}
			now = now.Add(usageCheckpointInterval)
			if err := save(false); err != nil || writes != 1 {
				t.Fatal("due checkpoint missing")
			}
			set(100)
			if err := save(true); err != nil || writes != 1 {
				t.Fatal("identical value dirtied checkpoint")
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("checkpoint missing")
			}
			// An old reader sees the same top-level map and fields, no envelope.
			var old map[string]struct {
				Offset int64 `json:"offset"`
			}
			if json.Unmarshal(b, &old) != nil || len(old) != 1 || old["fixture"].Offset != 100 {
				t.Fatal("legacy read failed")
			}
			fi, _ := os.Stat(path)
			if fi.Mode().Perm() != 0o600 {
				t.Fatal("checkpoint permissions changed")
			}
			set(101)
			if err := save(true); err != nil || writes != 2 {
				t.Fatal("forced shutdown save missing")
			}
		})
	}
}

func TestUsageCheckpointInterruptedWrite(t *testing.T) {
	if dir := os.Getenv("ST51_CRASH_DIR"); dir != "" {
		s := newCursorStore(dir)
		if s.Load() != nil {
			os.Exit(78)
		}
		s.Set("fixture", 200, 200, 200)
		s.checkpoint.write = func(path string, b []byte, mode os.FileMode) error {
			f, err := os.CreateTemp(filepath.Dir(path), ".cc-usage-cursors.json.tmp-*")
			if err != nil {
				os.Exit(78)
			}
			_, _ = f.Write(b[:len(b)/2])
			_ = f.Sync()
			os.Exit(77)
			return nil
		}
		_ = s.Save()
		os.Exit(78)
	}
	dir := t.TempDir()
	home := t.TempDir()
	s := newCursorStore(dir)
	s.Set("fixture", 100, 100, 100)
	if s.Save() != nil {
		t.Fatal("initial checkpoint failed")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageCheckpointInterruptedWrite$")
	cmd.Env = append(os.Environ(), "HOME="+home, "ST51_CRASH_DIR="+dir)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatal("crash fixture did not reach interrupted write")
	}
	reloaded := newCursorStore(dir)
	if reloaded.Load() != nil {
		t.Fatal("restart failed")
	}
	if off, _ := reloaded.Get("fixture"); off != 100 {
		t.Fatal("interrupted write replaced valid checkpoint")
	}
	// The orphan tempfile is ignored by the old and new readers alike.
	b, _ := os.ReadFile(reloaded.path)
	var legacy map[string]scanCursor
	if json.Unmarshal(b, &legacy) != nil || legacy["fixture"].Offset != 100 {
		t.Fatal("downgrade after crash failed")
	}
}

func TestUsageCheckpointConcurrentSave(t *testing.T) {
	s := newCursorStore(t.TempDir())
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := int64(1); i <= 20; i++ {
				s.Set(string(rune('a'+n)), i, i, i)
				if s.Save() != nil {
					t.Error("concurrent save failed")
					return
				}
			}
		}(n)
	}
	wg.Wait()
	r := newCursorStore(filepath.Dir(s.path))
	if r.Load() != nil {
		t.Fatal("reload failed")
	}
	for n := 0; n < 4; n++ {
		if off, _ := r.Get(string(rune('a' + n))); off != 20 {
			t.Fatal("older snapshot overwrote newer state")
		}
	}
}

func TestUsageCheckpointTimerAndBoundedShutdown(t *testing.T) {
	calls := make(chan bool, 3)
	first := true
	stop := startUsageCheckpoints(func(force bool) error { calls <- force; return nil }, func() time.Duration {
		if first {
			first = false
			return time.Millisecond
		}
		return time.Hour
	})
	select {
	case force := <-calls:
		if force {
			t.Fatal("timer forced checkpoint")
		}
	case <-time.After(time.Second):
		t.Fatal("independent timer did not run")
	}
	stop()
	if !<-calls {
		t.Fatal("shutdown did not force checkpoint")
	}
	blocked := make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	defer release()
	entered := make(chan struct{})
	dir := t.TempDir()
	store := newCursorStore(dir)
	store.Set("fixture", 123, 123, 123)
	store.checkpoint.write = func(path string, b []byte, mode os.FileMode) error {
		close(entered)
		<-blocked
		return writeFileAtomicSynced(path, b, mode)
	}
	var scanners usageScannerGroup
	scanners.start(func() {
		finish := startUsageCheckpoints(store.save, func() time.Duration { return time.Hour })
		finish()
	})
	<-entered
	begin := time.Now()
	if scanners.join(20 * time.Millisecond) {
		t.Fatal("scanner join completed before forced checkpoint")
	}
	if time.Since(begin) > time.Second {
		t.Fatal("scanner join exceeded its budget")
	}
	if scanners.start(func() {}) {
		t.Fatal("shutdown allowed a new scanner generation")
	}
	release()
	if !scanners.join(time.Second) {
		t.Fatal("scanner did not join after checkpoint completed")
	}
	reloaded := newCursorStore(dir)
	if reloaded.Load() != nil {
		t.Fatal("checkpoint reload failed")
	}
	if offset, _ := reloaded.Get("fixture"); offset != 123 {
		t.Fatal("forced checkpoint was not inside scanner join")
	}

}

type usagePrivacyFixture struct {
	path    string
	scan    func(bool)
	append  func(string)
	reset   func(string)
	reload  func()
	fail    func(bool)
	offset  func() int64
	privacy *privacySettings
	capture *captureServer
	ledger  *inAppLedger
	key     func() string
}

func newUsagePrivacyFixture(t *testing.T, provider string) *usagePrivacyFixture {
	t.Helper()
	home := t.TempDir()
	data := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	f := &usagePrivacyFixture{privacy: newPrivacySettings(data, func() bool { return true }), capture: &captureServer{}}
	ledger := newInAppLedger(data)
	f.ledger = ledger
	server := httptest.NewServer(http.HandlerFunc(f.capture.handler))
	t.Cleanup(server.Close)
	client := server.Client()
	var row func(string) string
	var targets *usageSkipSnapshot
	var files func() []string
	var pathOffset func(string) int64
	var revision func() uint64
	var save func() error
	if provider == "claude" {
		root := filepath.Join(home, ".claude")
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		f.path = filepath.Join(root, "projects", "fixture", "session.jsonl")
		files = func() []string { return listTranscripts([]string{root}) }
		s := newCursorStore(data)
		s.uploadAllowed = f.privacy.usageUploadAllowed
		f.scan = func(on bool) {
			base, token := "", ""
			if on {
				base, token = server.URL, "token"
			}
			scanOnce(context.Background(), s, ledger, client, base, token, nil, targets)
		}
		f.reload = func() {
			s = newCursorStore(data)
			s.uploadAllowed = f.privacy.usageUploadAllowed
			if s.Load() != nil {
				t.Fatal("reload failed")
			}
		}
		f.fail = func(fail bool) {
			s.checkpoint.write = nil
			if fail {
				s.checkpoint.write = func(string, []byte, os.FileMode) error { return errors.New("injected checkpoint failure") }
			}
		}
		f.offset = func() int64 { off, _ := s.Get(f.path); return off }
		f.key = func() string { s.mu.Lock(); defer s.mu.Unlock(); return s.m[f.path].WorkflowAttributionKey }
		pathOffset = func(path string) int64 { off, _ := s.Get(path); return off }
		revision = func() uint64 { return s.revision() }
		save = func() error { return s.Save() }
		row = func(id string) string {
			return `{"type":"assistant","timestamp":"2026-11-04T00:00:00Z","sessionId":"session","requestId":"req` + id + `","message":{"id":"msg` + id + `","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
		}
	} else {
		root := filepath.Join(home, ".codex", "sessions")
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
		f.path = filepath.Join(root, "2026", "11", "04", "rollout-fixture.jsonl")
		files = func() []string { return listCodexRollouts([]string{root}) }
		login := usageFixtureCodexLogin{}
		s := newCodexCursorStore(data)
		s.uploadAllowed = f.privacy.usageUploadAllowed
		f.scan = func(on bool) {
			base, token := "", ""
			if on {
				base, token = server.URL, "token"
			}
			codexUsageScanPass(context.Background(), s, ledger, client, base, token, login, []string{root}, time.Now(), targets)
		}
		f.reload = func() {
			s = newCodexCursorStore(data)
			s.uploadAllowed = f.privacy.usageUploadAllowed
			if s.Load() != nil {
				t.Fatal("reload failed")
			}
		}
		f.fail = func(fail bool) {
			s.checkpoint.write = nil
			if fail {
				s.checkpoint.write = func(string, []byte, os.FileMode) error { return errors.New("injected checkpoint failure") }
			}
		}
		f.offset = func() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.m[f.path].Offset }
		f.key = func() string { s.mu.Lock(); defer s.mu.Unlock(); return s.m[f.path].AttributionKey }
		pathOffset = func(path string) int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.m[path].Offset }
		revision = func() uint64 { return s.revision() }
		save = func() error { return s.Save() }
		row = func(id string) string {
			return codexMetaLine(codexTestThread, codexTestThread, "codex-tui", `"cli"`) + codexTurnLine("gpt-6-sol") + codexRecordLine("resp"+id, 10, 0, 5, 0)
		}
	}
	if os.MkdirAll(filepath.Dir(f.path), 0o700) != nil {
		t.Fatal("fixture mkdir failed")
	}
	f.append = func(id string) {
		file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal("fixture open failed")
		}
		_, err = file.WriteString(row(id))
		_ = file.Close()
		if err != nil {
			t.Fatal("fixture write failed")
		}
	}
	f.reset = func(id string) {
		if os.WriteFile(f.path, []byte(row(id)), 0o600) != nil {
			t.Fatal("fixture reset failed")
		}
	}
	unregister := f.privacy.registerUsage(save, func(ctx context.Context) error {
		var err error
		targets, err = snapshotUsageSkips(files())
		if err != nil {
			return err
		}
		defer func() { targets = nil }()
		return drainUsageSkips(ctx, revision, func() { f.scan(false) }, save, func() bool { return targets.reached(pathOffset) })
	})
	t.Cleanup(unregister)
	return f
}

func TestUsagePrivacySkipFailureToggleAndTruncate(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			f := newUsagePrivacyFixture(t, provider)
			on, off := true, false
			if f.privacy.update(&off, nil) != nil {
				t.Fatal("disable failed")
			}
			f.append("off1")
			f.append("off2")
			f.fail(true)
			f.scan(false)
			if f.offset() != 0 {
				t.Fatal("failed skip exposed undurable offset")
			}
			if f.privacy.update(&on, nil) == nil || f.privacy.usageUploadAllowed() {
				t.Fatal("failed skip allowed opt-in")
			}
			f.scan(true)
			if recs, _ := f.capture.snapshot(); len(recs) != 0 {
				t.Fatal("failed skip uploaded rows")
			}
			f.fail(false)
			f.scan(false)
			f.reload()
			// A crash cannot recover an offset preceding the durable skip.
			if f.privacy.update(&on, nil) != nil {
				t.Fatal("opt-in retry failed")
			}
			f.scan(true)
			if recs, _ := f.capture.snapshot(); len(recs) != 0 {
				t.Fatal("restart replayed opted-out rows")
			}
			if f.privacy.update(&off, nil) != nil {
				t.Fatal("disable failed")
			}
			f.reset("truncated-off")
			f.scan(false)
			f.reload()
			if f.privacy.update(&on, nil) != nil {
				t.Fatal("truncation opt-in failed")
			}
			f.scan(true)
			if recs, _ := f.capture.snapshot(); len(recs) != 0 {
				t.Fatal("truncation exposed opted-out rows")
			}
			if f.privacy.update(&off, nil) != nil {
				t.Fatal("disable failed")
			}
			if os.Remove(f.path) != nil {
				t.Fatal("remove fixture failed")
			}
			f.scan(false)
			f.reset("returned-off")
			// The toggle drains rows written since the previous disabled scan too.
			if f.privacy.update(&on, nil) != nil {
				t.Fatal("returning file opt-in failed")
			}
			f.scan(true)
			if recs, _ := f.capture.snapshot(); len(recs) != 0 {
				t.Fatal("returning file exposed opted-out rows")
			}
			f.append("new-on")
			f.scan(true)
			if recs, _ := f.capture.snapshot(); len(recs) != 1 {
				t.Fatal("new enabled row was not uploaded once")
			}
		})
	}
}

func TestUsageAckCrashReplaysStableKeys(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			f := newUsagePrivacyFixture(t, provider)
			f.append("acked")
			f.scan(true)
			f.reload()
			f.scan(true)
			recs, _ := f.capture.snapshot()
			if len(recs) != 2 || recs[0].MessageID == "" || recs[0].MessageID != recs[1].MessageID || recs[0].RequestID != recs[1].RequestID {
				t.Fatal("crash replay did not preserve stable record keys")
			}
		})
	}
}

func TestUsageToggleOffStopsBeforeBarrier(t *testing.T) {
	p := newPrivacySettings(t.TempDir(), func() bool { return true })
	off := false
	p.usageMu.Lock()
	done := make(chan error, 1)
	go func() { done <- p.update(&off, nil) }()
	deadline := time.Now().Add(time.Second)
	for p.usageUploadAllowed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.usageUploadAllowed() {
		p.usageMu.Unlock()
		t.Fatal("off state waited for scan barrier")
	}
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++; w.WriteHeader(204) }))
	defer srv.Close()
	client := &http.Client{Transport: usageUploadTransport{allowed: p.usageUploadAllowed}}
	_, err := client.Get(srv.URL)
	p.usageMu.Unlock()
	if err == nil || posts != 0 || <-done != nil {
		t.Fatal("new request escaped disabled state")
	}
}

func TestUsageOffSurvivesFailedCursorFlush(t *testing.T) {
	p := newPrivacySettings(t.TempDir(), func() bool { return true })
	unregister := p.registerUsage(func() error { return errors.New("injected disk error") }, func(context.Context) error { return nil })
	defer unregister()
	off := false
	if p.update(&off, nil) != nil {
		t.Fatal("cursor failure prevented disabling")
	}
	restarted := newPrivacySettings(filepath.Dir(p.path), func() bool { return true })
	if restarted.usageUploadAllowed() {
		t.Fatal("restart re-enabled uploads after failed cursor flush")
	}
}

func TestUsageOptInSnapshotConvergesWithActiveWriters(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			f := newUsagePrivacyFixture(t, provider)
			off, on := false, true
			if f.privacy.update(&off, nil) != nil {
				t.Fatal("disable failed")
			}
			f.append("before-request")
			original := f.scan
			scans := 0
			f.scan = func(on bool) {
				if !on {
					scans++
					f.append("after-request")
				}
				original(on)
			}
			if f.privacy.update(&on, nil) != nil {
				t.Fatal("active writer prevented opt-in")
			}
			if scans > 3 {
				t.Fatal("drain chased appends beyond snapshot")
			}
			recs, _ := f.capture.snapshot()
			if len(recs) != 0 {
				t.Fatal("drain uploaded rows")
			}
		})
	}
}

func TestUsageSkipSnapshotFailureStaysOff(t *testing.T) {
	err := drainUsageSkips(context.Background(), func() uint64 { return 0 }, func() {}, func() error { return nil }, func() bool { return false })
	if err == nil {
		t.Fatal("incomplete skip snapshot allowed upload resumption")
	}
}

// Account metadata is intentionally non-nil: nil in every fixture mode hid the
// production bug where only ON resolved the account and invented a stronger key.
type usageFixtureCodexLogin struct{}

func (usageFixtureCodexLogin) livePath() string               { return "" }
func (usageFixtureCodexLogin) requireCodexFileStorage() error { return nil }
func (usageFixtureCodexLogin) UsageAccountResolver() codexAccountResolver {
	return func(account, user string) string {
		if account == "acct-1" && user == "user-1" {
			return "fixture-account"
		}
		return ""
	}
}

func TestUsageAttributionIdenticalAcrossOffDrainOn(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			f := newUsagePrivacyFixture(t, provider)
			session := "session"
			if provider == "codex" {
				session = codexTestThread
			}
			f.ledger.Record(session, true)
			f.ledger.RecordWorkflow(session, "fixture-workflow", "fixture-role", "5m", "applied")
			// Claude's account is ledger-backed; Codex must use its non-nil resolver.
			if provider == "claude" {
				f.ledger.RecordAccount(session, "fixture-account")
			}
			off, on := false, true
			if f.privacy.update(&off, nil) != nil {
				t.Fatal("disable failed")
			}
			f.append("off-pass")
			f.scan(false)
			offKey := f.key()
			if offKey == "" || !strings.Contains(offKey, "fixture-account") || !strings.Contains(offKey, "fixture-workflow") || !strings.Contains(offKey, "fixture-role") {
				t.Fatal("OFF omitted locally available attribution")
			}
			if provider == "claude" && (!strings.Contains(offKey, "5m") || !strings.Contains(offKey, "applied")) {
				t.Fatal("OFF omitted cache TTL attribution")
			}
			f.append("off-drain")
			if f.privacy.update(&on, nil) != nil {
				t.Fatal("opt-in drain failed")
			}
			if f.key() != offKey {
				t.Fatal("drain changed attribution with unchanged local metadata")
			}
			f.scan(true)
			if f.key() != offKey {
				t.Fatal("first ON pass changed attribution")
			}
			rows, posts := f.capture.snapshot()
			if len(rows) != 0 || posts != 0 {
				t.Fatalf("OFF-window upload rows=%d posts=%d; want zero", len(rows), posts)
			}
			f.append("on-new")
			f.scan(true)
			rows, posts = f.capture.snapshot()
			if len(rows) != 1 || posts != 1 {
				t.Fatal("new ON row did not use the real upload path")
			}
			if rows[0].Account != "fixture-account" || rows[0].WorkflowInvocationID != "fixture-workflow" || rows[0].WorkflowRole != "fixture-role" {
				t.Fatal("uploaded row lost resolved attribution")
			}
		})
	}
}

func TestUsageGenuinelyStrongerAttributionReplaysAckedRows(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			f := newUsagePrivacyFixture(t, provider)
			f.append("acked")
			f.scan(true)
			initial, _ := f.capture.snapshot()
			if len(initial) != 1 {
				t.Fatal("initial row was not acknowledged")
			}
			before := f.key()
			session := "session"
			if provider == "codex" {
				session = codexTestThread
			}
			f.ledger.RecordWorkflow(session, "late-workflow", "late-role", "5m", "applied")
			f.scan(true)
			replayed, _ := f.capture.snapshot()
			if len(replayed) != 2 || replayed[1].MessageID != initial[0].MessageID || replayed[1].RequestID != initial[0].RequestID {
				t.Fatal("genuine enrichment did not replay the acknowledged stable key")
			}
			if f.key() == before || replayed[1].WorkflowInvocationID != "late-workflow" {
				t.Fatal("genuinely stronger attribution was not committed")
			}
			f.scan(true)
			if rows, _ := f.capture.snapshot(); len(rows) != 2 {
				t.Fatal("stronger-key replay repeated without metadata changes")
			}
		})
	}
}
