package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"
)

// agentDiagnosticsCap is the most a WSL helper's agent may keep in its
// diagnostics dir, over all of its captures (@secure, 20 Nov).
const agentDiagnosticsCap = 1 << 20

// captureCappedOOMDiagnostics is captureOOMDiagnostics for a WSL helper's
// agent (--diagnostics-dir). Every directory is 0700 and every file is
// created O_EXCL|O_NOFOLLOW 0600; the whole dir stays within 1 MiB, so a
// capture that would pass it is cut short; and it holds no env and no argv:
// memory statistics, a heap profile and goroutine stacks only. Nothing here
// is sent anywhere; the helper's reap removes the dir.
func captureCappedOOMDiagnostics(diagDir, role string) (string, error) {
	fi, err := os.Lstat(diagDir)
	if err != nil {
		return diagDir, err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return diagDir, fmt.Errorf("%s is not a plain directory", diagDir)
	}
	used, err := diagnosticsDirSize(diagDir)
	if err != nil {
		return diagDir, err
	}
	budget := &diagBudget{left: agentDiagnosticsCap - used}
	if budget.left <= 0 {
		return diagDir, errors.New("diagnostics budget spent")
	}
	dir := filepath.Join(diagDir, fmt.Sprintf("oom-%s-%s%d",
		time.Now().UTC().Format("20060102-150405.000000000"), role, os.Getpid()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return dir, err
	}

	var errs []error
	write := func(name string, fill func(io.Writer) error) {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o600)
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", name, err))
			return
		}
		err = fill(budget.writer(f))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil && !errors.Is(err, errDiagBudget) {
			errs = append(errs, fmt.Errorf("write %s: %w", name, err))
		}
	}
	// Smallest and most useful first, so a spent budget cuts the rest.
	write("memstats.txt", func(w io.Writer) error {
		_, err := io.WriteString(w, cappedMemStatsReport(role))
		return err
	})
	write("goroutine.txt", func(w io.Writer) error { return pprof.Lookup("goroutine").WriteTo(w, 2) })
	write("heap.pprof", func(w io.Writer) error { return pprof.Lookup("heap").WriteTo(w, 0) })
	return dir, errors.Join(errs...)
}

func cappedMemStatsReport(role string) string {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	goMB := goRuntimeMB()
	osMB := osSelfMemMB()
	var b strings.Builder
	fmt.Fprintf(&b, "role=%s\npid=%d\n", role, os.Getpid())
	fmt.Fprintf(&b, "HeapAlloc=%d\nHeapSys=%d\nHeapReleased=%d\nStackSys=%d\nSys=%d\n",
		stats.HeapAlloc, stats.HeapSys, stats.HeapReleased, stats.StackSys, stats.Sys)
	fmt.Fprintf(&b, "GoRuntimeMB=%d\nOSSelfMemMB=%d\nEffectiveMB=%d\n", goMB, osMB, max(goMB, osMB))
	for _, consumer := range topHeapConsumers(20) {
		fmt.Fprintf(&b, "TopHeapConsumer=%s\n", consumer)
	}
	return b.String()
}

// diagnosticsDirSize sums regular files under dir without following links.
func diagnosticsDirSize(dir string) (int, error) {
	total := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += int(info.Size())
		}
		return nil
	})
	return total, err
}

var errDiagBudget = errors.New("diagnostics budget spent")

type diagBudget struct{ left int }

func (b *diagBudget) writer(w io.Writer) io.Writer { return &budgetWriter{w: w, b: b} }

type budgetWriter struct {
	w io.Writer
	b *diagBudget
}

func (bw *budgetWriter) Write(p []byte) (int, error) {
	if bw.b.left <= 0 {
		return 0, errDiagBudget
	}
	cut := false
	if len(p) > bw.b.left {
		p, cut = p[:bw.b.left], true
	}
	n, err := bw.w.Write(p)
	bw.b.left -= n
	if err == nil && cut {
		err = errDiagBudget
	}
	return n, err
}
