package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// memGuardEnvVar is the only way to turn the daemon guard off. Named once
	// so the Autonomy row (daemon/autonomy_api.go) cannot drift from the
	// variable the guard actually reads.
	memGuardEnvVar          = "SOLU_MEM_GUARD_MB"
	defaultDaemonMemGuardMB = uint64(6144)
	defaultAgentMemGuardMB  = uint64(2048)
	memGuardMinMB           = uint64(512)
	memGuardAgentCooldown   = 30 * time.Minute
)

var memGuardInterval = 15 * time.Second

type memGuardConfig struct {
	role      string
	dataDir   string
	softMB    uint64
	hardMB    uint64
	interval  time.Duration
	memFn     func() uint64
	restartFn func() error
	exitFn    func(int)
}

type memGuard struct {
	cfg                    memGuardConfig
	daemonRestartCommitted bool
	hardExitOnce           sync.Once
	lastAgentCapture       time.Time
}

func daemonMemGuardConfig(dataDir string) memGuardConfig {
	return memGuardConfig{
		role:     "daemon",
		dataDir:  dataDir,
		softMB:   memGuardEnvMB(memGuardEnvVar, defaultDaemonMemGuardMB),
		interval: memGuardInterval,
		memFn:    selfMemMB,
	}
}

// memGuardHeapLimitPercent is where the Go GC's soft memory limit sits,
// relative to the memory guard's restart threshold.
const memGuardHeapLimitPercent = 80

// applyMemGuardHeapLimit gives the Go garbage collector a soft memory limit
// below the memory guard's restart threshold, so the collector gives memory
// back before the guard replaces the daemon.
//
// Without it the GC's only pacer is GOGC=100: the heap may grow to twice the
// live set before a collection. A daemon with ~2.9GB live (1,600 tasks, ~70
// panes of terminal buffers) therefore peaked near 5.8GB and tripped the
// 6144MB guard every 10-20 minutes, and each guard restart re-paid a
// multi-minute boot on a loaded host. With the limit, a steady live set
// stays below the threshold and only genuine growth reaches the guard. The
// runtime caps GC CPU when a live set really does exceed the limit, so this
// cannot spin.
//
// An explicit GOMEMLIMIT is the operator's choice and is left alone, as is a
// disabled guard (softMB == 0). Returns the limit applied, if any.
func applyMemGuardHeapLimit(softMB uint64) (int64, bool) {
	if softMB == 0 || strings.TrimSpace(os.Getenv("GOMEMLIMIT")) != "" {
		return 0, false
	}
	limit := int64(softMB/100*memGuardHeapLimitPercent) << 20
	if limit <= 0 {
		return 0, false
	}
	debug.SetMemoryLimit(limit)
	return limit, true
}

func agentMemGuardConfig(dataDir string) memGuardConfig {
	soft := memGuardEnvMB("SOLU_MEM_GUARD_AGENT_MB", defaultAgentMemGuardMB)
	hardDefault := soft * 2
	if hardDefault < soft { // overflow can only come from an operator override
		hardDefault = ^uint64(0)
	}
	return memGuardConfig{
		role:     "agent",
		dataDir:  dataDir,
		softMB:   soft,
		hardMB:   memGuardEnvMB("SOLU_MEM_GUARD_AGENT_HARD_MB", hardDefault),
		interval: memGuardInterval,
		memFn:    selfMemMB,
	}
}

// memGuardEnvMB follows the RSS sampler's permissive kill-switch convention.
// Tiny non-zero thresholds are clamped to prevent an accidental restart loop.
func memGuardEnvMB(name string, defaultMB uint64) uint64 {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch raw {
	case "":
		return defaultMB
	case "0", "false", "off", "no":
		return 0
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		log.Printf("[memguard] WARNING: invalid %s=%q; using default %dMB", name, raw, defaultMB)
		return defaultMB
	}
	if value < memGuardMinMB {
		log.Printf("[memguard] WARNING: %s=%dMB is below the safety floor; clamping to %dMB", name, value, memGuardMinMB)
		return memGuardMinMB
	}
	return value
}

func selfMemMB() uint64 {
	goMB := goRuntimeMB()
	osMB := osSelfMemMB()
	if osMB > goMB {
		return osMB
	}
	return goMB
}

func goRuntimeMB() uint64 {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	metrics.Read(samples)
	total := samples[0].Value.Uint64()
	released := samples[1].Value.Uint64()
	if released >= total {
		return 0
	}
	return (total - released) / (1024 * 1024)
}

func goLiveHeapMB() uint64 {
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(samples)
	return samples[0].Value.Uint64() / (1024 * 1024)
}

// startMemGuard samples asynchronously. Daemons stop after a restart attempt
// reports that its handoff committed; a failed handoff is captured and retried
// on the next sample. Agents capture at most once per cooldown at the soft
// limit and only exit at their separate hard limit.
func startMemGuard(ctx context.Context, cfg memGuardConfig) {
	guard := newMemGuard(cfg)
	if guard == nil {
		return
	}
	go guard.run(ctx)
}

func newMemGuard(cfg memGuardConfig) *memGuard {
	// The agent subcommand is dispatched before daemon boot and has its own
	// memory guard. An inherited marker must not disable that protection.
	if cfg.role != "agent" {
		requireNormalBootEngine("memguard.newMemGuard")
	}
	if cfg.softMB == 0 && (cfg.role != "agent" || cfg.hardMB == 0) {
		return nil
	}
	if cfg.interval <= 0 {
		cfg.interval = memGuardInterval
	}
	if cfg.memFn == nil {
		cfg.memFn = selfMemMB
	}
	return &memGuard{cfg: cfg}
}

func (g *memGuard) run(ctx context.Context) {
	ticker := time.NewTicker(g.cfg.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			g.sample(now)
		}
	}
}

func (g *memGuard) sample(now time.Time) {
	measuredMB := g.cfg.memFn()
	if g.cfg.role == "agent" && g.cfg.hardMB > 0 && measuredMB >= g.cfg.hardMB {
		g.hardExitOnce.Do(func() {
			triggerMemGuard(g.cfg, measuredMB, g.cfg.hardMB, "HARD LIMIT")
			if g.cfg.exitFn != nil {
				g.cfg.exitFn(1)
			}
		})
		return
	}
	if g.cfg.softMB == 0 || measuredMB < g.cfg.softMB {
		return
	}
	if g.cfg.role == "agent" {
		if g.lastAgentCapture.IsZero() || now.Sub(g.lastAgentCapture) >= memGuardAgentCooldown {
			g.lastAgentCapture = now
			triggerMemGuard(g.cfg, measuredMB, g.cfg.softMB, "SOFT LIMIT")
		}
		return
	}
	if g.daemonRestartCommitted {
		return
	}
	triggerMemGuard(g.cfg, measuredMB, g.cfg.softMB, "RESTART ATTEMPT")
	if g.cfg.restartFn == nil {
		g.daemonRestartCommitted = true
		return
	}
	if err := g.cfg.restartFn(); err != nil {
		log.Printf("[memguard] ERROR role=%s measured=%dMB threshold=%dMB restart failed: %v; retrying on next sample",
			g.cfg.role, measuredMB, g.cfg.softMB, err)
		return
	}
	g.daemonRestartCommitted = true
}

func triggerMemGuard(cfg memGuardConfig, measuredMB, thresholdMB uint64, action string) {
	dir, err := captureOOMDiagnostics(cfg.dataDir, cfg.role)
	log.Printf("[memguard] %s role=%s measured=%dMB threshold=%dMB diagnostics=%s",
		action, cfg.role, measuredMB, thresholdMB, dir)
	if err != nil {
		log.Printf("[memguard] ERROR role=%s diagnostics=%s capture failed: %v", cfg.role, dir, err)
	}
	for _, consumer := range topHeapConsumers(10) {
		log.Printf("[memguard] top consumer: %s", consumer)
	}
}

func captureOOMDiagnostics(dataDir, role string) (string, error) {
	diagnosticsRoot := filepath.Join(dataDir, "diagnostics")
	dir := filepath.Join(diagnosticsRoot, fmt.Sprintf("oom-%s-%s%d",
		time.Now().UTC().Format("20060102-150405.000000000"), role, os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}

	var errs []error
	writeProfile := func(name, profile string, debug int) {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", name, err))
			return
		}
		p := pprof.Lookup(profile)
		if p == nil {
			err = fmt.Errorf("profile %q unavailable", profile)
		} else {
			err = p.WriteTo(f, debug)
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("write %s: %w", name, err))
		}
	}
	writeProfile("heap.pprof", "heap", 0)
	writeProfile("heap.txt", "heap", 1)
	writeProfile("goroutine.txt", "goroutine", 2)

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	goMB := goRuntimeMB()
	osMB := osSelfMemMB()
	var report strings.Builder
	fmt.Fprintf(&report, "role=%s\npid=%d\n", role, os.Getpid())
	fmt.Fprintf(&report, "HeapAlloc=%d\nHeapSys=%d\nHeapReleased=%d\nStackSys=%d\nSys=%d\n",
		stats.HeapAlloc, stats.HeapSys, stats.HeapReleased, stats.StackSys, stats.Sys)
	fmt.Fprintf(&report, "GoRuntimeMB=%d\nOSSelfMemMB=%d\nEffectiveMB=%d\n", goMB, osMB, max(goMB, osMB))
	for _, consumer := range topHeapConsumers(20) {
		fmt.Fprintf(&report, "TopHeapConsumer=%s\n", consumer)
	}
	if err := os.WriteFile(filepath.Join(dir, "memstats.txt"), []byte(report.String()), 0o644); err != nil {
		errs = append(errs, fmt.Errorf("write memstats.txt: %w", err))
	}
	if err := rotateOOMDiagnostics(diagnosticsRoot, role); err != nil {
		errs = append(errs, err)
	}
	return dir, errors.Join(errs...)
}

func processMemoryUsageMB(privateUsage, workingSetSize uintptr) uint64 {
	used := privateUsage
	if workingSetSize > used {
		used = workingSetSize
	}
	return uint64(used) / (1024 * 1024)
}

func rotateOOMDiagnostics(diagnosticsRoot, role string) error {
	dirs, err := filepath.Glob(filepath.Join(diagnosticsRoot, "oom-*-"+role+"*"))
	if err != nil {
		return fmt.Errorf("glob diagnostics: %w", err)
	}
	sort.Strings(dirs)
	if len(dirs) <= 3 {
		return nil
	}
	var errs []error
	for _, old := range dirs[:len(dirs)-3] {
		if err := os.RemoveAll(old); err != nil {
			errs = append(errs, fmt.Errorf("remove old diagnostics %s: %w", old, err))
		}
	}
	return errors.Join(errs...)
}

func topHeapConsumers(n int) []string {
	if n <= 0 {
		return nil
	}
	count, _ := runtime.MemProfile(nil, false)
	records := make([]runtime.MemProfileRecord, count)
	for {
		needed, ok := runtime.MemProfile(records, false)
		if ok {
			records = records[:needed]
			break
		}
		records = make([]runtime.MemProfileRecord, needed)
	}

	byFunction := make(map[string]int64)
	for _, record := range records {
		if record.InUseBytes() <= 0 {
			continue
		}
		stack := record.Stack()
		name := "unknown"
		if len(stack) > 0 {
			if fn := runtime.FuncForPC(stack[0]); fn != nil {
				name = fn.Name()
			}
		}
		byFunction[name] += record.InUseBytes()
	}
	type consumer struct {
		name  string
		bytes int64
	}
	consumers := make([]consumer, 0, len(byFunction))
	for name, bytes := range byFunction {
		consumers = append(consumers, consumer{name: name, bytes: bytes})
	}
	sort.Slice(consumers, func(i, j int) bool {
		if consumers[i].bytes == consumers[j].bytes {
			return consumers[i].name < consumers[j].name
		}
		return consumers[i].bytes > consumers[j].bytes
	})
	if len(consumers) > n {
		consumers = consumers[:n]
	}
	out := make([]string, 0, len(consumers))
	for _, consumer := range consumers {
		out = append(out, fmt.Sprintf("%s inuse=%dMB", consumer.name, consumer.bytes/(1024*1024)))
	}
	return out
}
