package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const offenderFixture = `[{"label":"%TEMP%\\wsl-crashes","path":"C:\\Users\\alice\\AppData\\Local\\Temp\\wsl-crashes","exists":true,"bytes":54760833024,"files":10,"dumpFiles":10,"dumpBytes":54760833024,"complete":true,"newestUtc":"2026-08-26T20:46:00Z"},{"label":"%LOCALAPPDATA%\\Temp\\wsl-crashes","path":"","exists":false,"bytes":0,"files":0,"dumpFiles":0,"complete":true},{"label":"%TEMP%","path":"C:\\Users\\alice\\AppData\\Local\\Temp","exists":true,"bytes":55834574848,"files":1300,"dumpFiles":10,"dumpBytes":2621440,"complete":true},{"label":"%SystemRoot%\\Temp","path":"C:\\Windows\\Temp","exists":true,"bytes":1288490188,"files":55,"dumpFiles":0,"complete":true}]`

func testOffenderProber(output string) (*hostOffenderProber, *atomic.Int64) {
	calls := &atomic.Int64{}
	return &hostOffenderProber{
		resolvePS: func() (string, error) { return "powershell.exe", nil },
		env:       func(string) []string { return []string{"PATH=x"} },
		run: func(context.Context, string, []string, string) ([]byte, error) {
			calls.Add(1)
			return []byte("noise\n" + output + "\n"), nil
		},
		now: time.Now, timeout: time.Second, minInterval: 10 * time.Minute,
	}, calls
}

func TestWfHostOffenderParsePickTextRedactionAndCache(t *testing.T) {
	p, calls := testOffenderProber(offenderFixture)
	usages, fresh := p.Measure(context.Background())
	if !fresh || calls.Load() != 1 {
		t.Fatalf("fresh=%v calls=%d", fresh, calls.Load())
	}
	for _, usage := range usages {
		if strings.Contains(strings.ToLower(usage.Path), "alice") || strings.Contains(strings.ToLower(usage.Label), "alice") {
			t.Fatalf("unredacted offender usage: %+v", usage)
		}
	}
	o := pickHostOffender(usages, 1<<30)
	if o == nil || o.Label != `%TEMP%\wsl-crashes` || !strings.HasPrefix(o.Path, `~\`) {
		t.Fatalf("picked offender=%+v", o)
	}
	if got, want := hostOffenderText(o), `51.0 GB of WSL crash dumps (10 .dmp files) in %TEMP%\wsl-crashes`; got != want {
		t.Fatalf("text=%q want %q", got, want)
	}
	if _, fresh := p.Measure(context.Background()); fresh || calls.Load() != 1 {
		t.Fatalf("cached measure fresh=%v calls=%d", fresh, calls.Load())
	}
}

// The live blocker read "5.0 GB of WSL crash dumps (5 .dmp files) in %TEMP%"
// when %TEMP% was 4.95 GB holding 2.5 MB of dumps: both measurements were
// right, the sentence welded them. A directory is only "made of crash dumps"
// when the dumps actually account for it.
func TestWfHostOffenderTextSeparatesDumpBytesFromDirectoryBytes(t *testing.T) {
	minor := &hostDirUsage{Label: `%TEMP%`, Exists: true, Bytes: 5315022848, Files: 3543, DumpFiles: 5, DumpBytes: 2621440, Complete: true}
	got := hostOffenderText(minor)
	if strings.Contains(got, "5.0 GB of WSL crash dumps") {
		t.Fatalf("attributed the whole directory to dumps: %q", got)
	}
	if want := `5.0 GB (3543 files) in %TEMP%, of which 2.5 MB is WSL crash dumps (5 .dmp files)`; got != want {
		t.Fatalf("minor-dumps text=%q want %q", got, want)
	}

	dominant := &hostDirUsage{Label: `%TEMP%\wsl-crashes`, Exists: true, Bytes: 54760833024, Files: 10, DumpFiles: 10, DumpBytes: 54760833024, Complete: true}
	if want := `51.0 GB of WSL crash dumps (10 .dmp files) in %TEMP%\wsl-crashes`; hostOffenderText(dominant) != want {
		t.Fatalf("dominant-dumps text=%q want %q", hostOffenderText(dominant), want)
	}

	// A probe that counted dumps without sizing them must report the count,
	// never the directory total dressed up as a dump total.
	countOnly := &hostDirUsage{Label: `%TEMP%`, Exists: true, Bytes: 5315022848, Files: 3543, DumpFiles: 5, Complete: true}
	if want := `5.0 GB (3543 files) in %TEMP% (incl. 5 .dmp)`; hostOffenderText(countOnly) != want {
		t.Fatalf("count-only text=%q want %q", hostOffenderText(countOnly), want)
	}
}

// The probe has to actually measure dump bytes, or the text above can only ever
// fall back to the count-only wording.
func TestWfHostOffenderScriptMeasuresDumpBytes(t *testing.T) {
	script := hostOffenderScript()
	for _, want := range []string{`$dumpBytes=[int64]0;`, `$dumpBytes+=[int64]$f.Length`, `dumpBytes=$dumpBytes`} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
}

func TestWfHostOffenderIncompleteSmallAndErrorCache(t *testing.T) {
	o := &hostDirUsage{Label: `%TEMP%\wsl-crashes`, Exists: true, Bytes: 2 << 30, DumpFiles: 1, Complete: false}
	if !strings.HasSuffix(hostOffenderText(o), "(measurement incomplete)") {
		t.Fatalf("incomplete text=%q", hostOffenderText(o))
	}
	if got := pickHostOffender([]hostDirUsage{{Exists: true, Bytes: 100}}, 1<<30); got != nil {
		t.Fatalf("small offender selected: %+v", got)
	}
	p, _ := testOffenderProber(offenderFixture)
	first, _ := p.Measure(context.Background())
	p.minInterval = 0
	p.run = func(context.Context, string, []string, string) ([]byte, error) {
		return nil, errors.New("probe failed token=abcdefghijklmnopqrstuvwx1234567890")
	}
	kept, fresh := p.Measure(context.Background())
	if fresh || len(kept) != len(first) || p.lastErr == "" || strings.Contains(p.lastErr, "abcdefghijklmnopqrstuvwx1234567890") {
		t.Fatalf("error cache fresh=%v kept=%d lastErr=%q", fresh, len(kept), p.lastErr)
	}
}

func TestWfHostOffenderTimeoutKeepsPreviousCache(t *testing.T) {
	p, _ := testOffenderProber(offenderFixture)
	first, _ := p.Measure(context.Background())
	p.minInterval = 0
	p.timeout = 5 * time.Millisecond
	p.run = func(ctx context.Context, _ string, _ []string, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	kept, fresh := p.Measure(context.Background())
	if fresh || len(kept) != len(first) || !strings.Contains(p.lastErr, "deadline") {
		t.Fatalf("timeout cache fresh=%v kept=%d lastErr=%q", fresh, len(kept), p.lastErr)
	}
}

func TestWfHostOffenderFailedAttemptIsRateLimitedWithoutAStaleCache(t *testing.T) {
	p, calls := testOffenderProber(offenderFixture)
	p.run = func(context.Context, string, []string, string) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("PowerShell unavailable")
	}
	if usages, fresh := p.Measure(context.Background()); fresh || len(usages) != 0 {
		t.Fatalf("first failed measure usages=%+v fresh=%v", usages, fresh)
	}
	if usages, fresh := p.Measure(context.Background()); fresh || len(usages) != 0 || calls.Load() != 1 {
		t.Fatalf("rate-limited failed measure usages=%+v fresh=%v calls=%d", usages, fresh, calls.Load())
	}
	if _, _, ok := p.Latest(); ok {
		t.Fatal("a failed first measurement must not masquerade as a successful cache")
	}
}

func TestWfHostOffenderScriptIsBoundedAndNamesCandidates(t *testing.T) {
	script := hostOffenderScript()
	for _, want := range []string{`%TEMP%\wsl-crashes`, `%LOCALAPPDATA%\Temp\wsl-crashes`, `%TEMP%`, `%SystemRoot%\Temp`, "Select-Object -First 20000", "ConvertTo-Json -Compress"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
}

func TestWfHostOffenderParentTempDoesNotAttributeAllBytesToDumps(t *testing.T) {
	usages := []hostDirUsage{
		{Label: `%TEMP%\wsl-crashes`, Path: `C:\Users\fixture\Temp\wsl-crashes`, Exists: true, Bytes: 225075200, Files: 5, DumpFiles: 5, Complete: true},
		{Label: `%TEMP%`, Path: `C:\Users\fixture\Temp`, Exists: true, Bytes: 9648 * 1024 * 1024, Files: 3423, DumpFiles: 5, Complete: true},
	}
	selected := pickHostOffender(usages, 1<<30)
	if selected == nil || selected.Label != `%TEMP%` {
		t.Fatalf("expected large Temp parent: %+v", selected)
	}
	want := `9.4 GB (3423 files) in %TEMP% (incl. 5 .dmp)`
	if got := hostOffenderText(selected); got != want {
		t.Fatalf("misattributed directory usage: %q, want %q", got, want)
	}
	if selected.Bytes != 9648*1024*1024 || selected.DumpFiles != 5 {
		t.Fatal("wording fix changed measured usage")
	}
}

func TestWfHostOffenderDumpAttributionRequiresMeasuredBytes(t *testing.T) {
	for _, tc := range []struct {
		name, label, path string
		files, dumps      int
	}{
		{"mixed crash folder", `%TEMP%\wsl-crashes`, `C:\Temp\wsl-crashes`, 20, 5},
		{"generic dumps", `%TEMP%`, `C:\Temp`, 5, 5},
		{"dedicated dumps", `%TEMP%\wsl-crashes`, `C:\Temp\wsl-crashes`, 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &hostDirUsage{Label: tc.label, Path: tc.path, Bytes: 2 << 30, Files: tc.files, DumpFiles: tc.dumps, Complete: false}
			text := hostOffenderText(usage)
			if strings.Contains(text, "GB of WSL crash dumps") {
				t.Fatalf("incorrect attribution: %s", text)
			}
			if !strings.HasSuffix(text, "(measurement incomplete)") {
				t.Fatal("partial measurement qualifier lost")
			}
		})
	}
}
