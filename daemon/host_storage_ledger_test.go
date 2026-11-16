package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// launcherBuiltinMinFreeMB is the launcher's pre-change built-in 2 GiB check
// (the old desktop daemon.rs MIN_FREE constant). Workflow rollback must never
// lower the launcher policy beneath this historical safety floor.
const launcherBuiltinMinFreeMB = 2048

// launcherEffectiveFloorMB mirrors desktop/src-tauri/src/daemon.rs: a missing
// or malformed file uses the 16 GiB / 3% defaults, explicit JSON zeroes are
// unset per component, and the effective floor is the larger component.
func launcherEffectiveFloorMB(policyPath string, totalMB int64) int64 {
	abs, pct := int64(16384), int64(3)
	if b, err := os.ReadFile(policyPath); err == nil {
		var policy struct {
			MinFreeMB  int64 `json:"minFreeMB"`
			MinFreePct int64 `json:"minFreePct"`
		}
		if json.Unmarshal(b, &policy) == nil {
			if policy.MinFreeMB > 0 {
				abs = policy.MinFreeMB
			}
			if policy.MinFreePct > 0 {
				pct = policy.MinFreePct
			}
		}
	}
	return max(abs, totalMB*pct/100)
}

func TestHostStorageLedgerAppendTailDedupeAndRedaction(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	l := newHostStorageLedger(dir)
	l.now = func() time.Time { return now }
	l.AppendBoot("boot-a", "1.2.3")
	p := diskPressureState{GuestFreeMB: 667000, GuestFloorMB: 10240, HostState: hostStorageOK, HostVolume: `C:\`, HostFreeMB: 41000, HostTotalMB: 2050000, HostFloorMB: 61500, HostLow: true, BlockingLayer: "host"}
	l.AppendSample("boot-a", p)
	p.HostFreeMB += 100
	l.AppendSample("boot-a", p)
	l.AppendAdmissionBlocked("boot-a", `token=abcdefghijklmnopqrstuvwx1234567890`, "verify", p)
	l.AppendMaintenance("boot-a", "dry-run", `C:\Users\alice\AppData\Local\wsl\x`)
	events := l.Tail(20)
	if len(events) != 4 {
		t.Fatalf("dedupe failed: %#v", events)
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "abcdefghijklmnopqrst") || strings.Contains(text, `C:\\Users\\alice`) {
		t.Fatalf("secret/path leaked: %s", text)
	}
	if st, _ := os.Stat(l.path); st.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestHostStorageLedgerAttributionAndHostLossEvents(t *testing.T) {
	ledger := newHostStorageLedger(t.TempDir())
	start := time.Date(2026, 8, 26, 20, 2, 0, 0, time.UTC)
	p := diskPressureState{
		HostState: hostStorageOK, HostVolume: `C:\`, HostKnown: true, HostLow: true,
		HostFreeMB: 65865, HostFloorMB: 102400, VHDXPhysicalMB: 543380, BlockingLayer: "host",
		HostLoss:     hostLossAssessment{Detected: true, WindowStart: start, WindowEnd: start.Add(43 * time.Minute), Samples: 5, HostLossMB: 51095, UnexplainedMB: 51095, VHDXKnown: true},
		HostOffender: &hostDirUsage{Label: `%TEMP%\wsl-crashes`, Path: `C:\Users\alice\AppData\Local\Temp\wsl-crashes`, Exists: true, Bytes: 50 << 30, Files: 10, DumpFiles: 10, Complete: true},
	}
	ledger.AppendSample("boot", p)
	ledger.AppendAdmissionBlocked("boot", "capture-screens", "verify", p)
	ledger.AppendCrossing("boot", "host", true, p)
	ledger.AppendHostLoss("boot", true, p)
	p.HostLoss = hostLossAssessment{UnexplainedMB: 0}
	ledger.AppendHostLoss("boot", false, p)
	events := ledger.Tail(20)
	if len(events) != 5 {
		t.Fatalf("events=%+v", events)
	}
	for _, ev := range events[:4] {
		if ev.UnexplainedLossMB != 51095 || ev.Offender != `%TEMP%\wsl-crashes` || ev.OffenderMB != 51200 || ev.OffenderDumps != 10 || ev.OffenderComplete == nil || !*ev.OffenderComplete {
			t.Fatalf("missing attribution: %+v", ev)
		}
		if !strings.HasPrefix(ev.SuggestedPath, `~\`) || strings.Contains(strings.ToLower(ev.SuggestedPath), "alice") {
			t.Fatalf("unredacted suggested path: %+v", ev)
		}
	}
	if events[3].Kind != "host_loss" || events[3].Active == nil || !*events[3].Active || events[4].Kind != "host_loss" || events[4].Active == nil || *events[4].Active {
		t.Fatalf("host loss latch events=%+v", events[3:])
	}

	// A newly attributed sample must not be swallowed by the ordinary free-MB
	// dedupe even if every pressure reading is otherwise identical.
	ledger2 := newHostStorageLedger(t.TempDir())
	plain := p
	plain.HostLoss, plain.HostOffender = hostLossAssessment{}, nil
	ledger2.AppendSample("boot", plain)
	plain.HostOffender = p.HostOffender
	ledger2.AppendSample("boot", plain)
	if got := len(ledger2.Tail(10)); got != 2 {
		t.Fatalf("attribution-only sample was deduped: %d events", got)
	}
}

func TestHostStorageLedgerRotationAndReboot(t *testing.T) {
	dir := t.TempDir()
	l := newHostStorageLedger(dir)
	l.maxBytes = 180
	for i := 0; i < 8; i++ {
		l.AppendMaintenance("boot-a", "dry-run", strings.Repeat("x", 80))
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		if _, err := os.Stat(l.path + suffix); err != nil {
			t.Fatalf("missing %s: %v", suffix, err)
		}
	}
	reboot := newHostStorageLedger(dir)
	reboot.AppendBoot("boot-b", "2")
	if tail := reboot.Tail(20); len(tail) == 0 || tail[len(tail)-1].BootID != "boot-b" {
		t.Fatalf("reboot tail %#v", tail)
	}
}

func TestHostStorageLedgerNilAndMissingLauncherTail(t *testing.T) {
	var l *hostStorageLedger
	l.AppendBoot("x", "v")
	l.AppendSample("x", diskPressureState{})
	l.AppendCrossing("x", "host", true, diskPressureState{})
	l.AppendAdmissionBlocked("x", "cmd", "stage", diskPressureState{})
	l.AppendMaintenance("x", "m", "d")
	if got := l.Tail(10); got != nil {
		t.Fatal(got)
	}
	if got := hostStorageLauncherTail(`Z:\definitely-missing`, 10); len(got) != 0 {
		t.Fatal(got)
	}
	if got := readHostStorageEvents(filepath.Join(t.TempDir(), "missing"), 100, ""); got != nil {
		t.Fatal(got)
	}
}

func TestHostStorageLauncherTailReadsBoundedRedactedFixture(t *testing.T) {
	orig := hostStorageWSLPath
	defer func() { hostStorageWSLPath = orig }()
	root := t.TempDir()
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) { return []byte(root + "\n"), nil }
	dir := filepath.Join(root, "soluterminal")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := fmt.Sprintf("{\"at\":\"2026-08-25T10:00:00Z\",\"kind\":\"preflight\",\"verdict\":\"block\",\"freeBytes\":123,\"totalBytes\":456,\"floorBytes\":234,\"detail\":\"C:\\\\Users\\\\alice\\\\secret\"}\n")
	if err := os.WriteFile(filepath.Join(dir, "host-disk.jsonl"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	events := hostStorageLauncherTail(`C:\Users\alice\AppData\Local`, 10)
	if len(events) != 1 || events[0].Source != "launcher" || events[0].Verdict != "block" || events[0].FreeBytes != 123 || strings.Contains(events[0].Detail, "alice") {
		t.Fatalf("events=%+v", events)
	}
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("missing") }
	if got := hostStorageLauncherTail(`Z:\missing`, 10); len(got) != 0 {
		t.Fatal(got)
	}
}

func validCompactOperation(status string) hostStorageMaintenanceOperation {
	sample := registeredHostStorageSample(`C:\Users\owner\AppData\Local\wsl\Ubuntu\ext4.vhdx`)
	sample.LocalAppData = `C:\Users\owner\AppData\Local`
	sample.BasePath = `C:\Users\owner\AppData\Local\wsl\Ubuntu`
	sample.OwnerSID = "S-1-5-21-1000"
	op := newHostStorageMaintenanceOperation("compact", sample)
	op.RequestedMode = "Full"
	op.Status = status
	op.CompletedAt = op.At.Add(time.Minute)
	beforePhysical, beforeGuest, beforeHost := int64(700), int64(600), int64(800)
	afterPhysical, afterHost := int64(500), int64(1000)
	op.Before = &hostStorageMaintenanceMeasurement{PhysicalAllocationBytes: &beforePhysical, GuestUsedBytes: &beforeGuest, HostFreeBytes: &beforeHost}
	op.After = &hostStorageMaintenanceMeasurement{PhysicalAllocationBytes: &afterPhysical, HostFreeBytes: &afterHost, UnavailableReason: "post_restart_sample_pending"}
	if status == "succeeded" {
		op.ActualMode = "Full"
		confirmed := true
		op.DetachedConfirmed = &confirmed
	} else {
		op.Failure = &hostStorageMaintenanceFailure{Stage: "preflight", Code: "capability_missing", Message: "Optimize-VHD missing"}
	}
	return op
}

func TestHostStorageLedgerV2SuccessFailureAndExactMeasurements(t *testing.T) {
	ledger := newHostStorageLedger(t.TempDir())
	success := validCompactOperation("succeeded")
	if err := ledger.AppendOperation(success); err != nil {
		t.Fatal(err)
	}
	failed := validCompactOperation("failed")
	failed.OperationID = hostStorageOperationID()
	failed.ActualMode = "Full"
	failed.Failure = &hostStorageMaintenanceFailure{Stage: "optimize", Code: "optimize_failed", Message: "native child exit 9"}
	confirmed := true
	failed.DetachedConfirmed = &confirmed
	if err := ledger.AppendOperation(failed); err != nil {
		t.Fatal(err)
	}
	detach := validCompactOperation("failed")
	detach.OperationID = hostStorageOperationID()
	detach.ActualMode = "Full"
	unconfirmed := false
	detach.DetachedConfirmed = &unconfirmed
	detach.Failure = &hostStorageMaintenanceFailure{Stage: "detach", Code: "detach_unconfirmed", Message: "Attached remained true"}
	if err := ledger.AppendOperation(detach); err != nil {
		t.Fatal(err)
	}

	events := ledger.Tail(10)
	if len(events) != 3 {
		t.Fatalf("events=%+v", events)
	}
	raw, err := os.ReadFile(ledger.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `C:\\Users\\owner\\AppData\\Local\\wsl\\Ubuntu\\ext4.vhdx`) {
		t.Fatalf("exact canonical target identity was not preserved in the local receipt: %s", raw)
	}
	if events[0].Status != "succeeded" || events[0].ActualMode != "Full" || events[0].DetachedConfirmed == nil || !*events[0].DetachedConfirmed {
		t.Fatalf("success=%+v", events[0])
	}
	if events[0].Before == nil || *events[0].Before.PhysicalAllocationBytes != 700 || *events[0].Before.GuestUsedBytes != 600 || *events[0].Before.HostFreeBytes != 800 || *events[0].After.PhysicalAllocationBytes != 500 || *events[0].After.HostFreeBytes != 1000 {
		t.Fatalf("measurements not preserved: %+v", events[0])
	}
	if events[1].Failure == nil || events[1].Failure.Stage != "optimize" || events[2].Failure == nil || events[2].Failure.Stage != "detach" || events[2].DetachedConfirmed == nil || *events[2].DetachedConfirmed {
		t.Fatalf("failure records=%+v", events[1:])
	}
}

func TestHostStorageLedgerAppendOperationReportsPersistenceFailure(t *testing.T) {
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger := newHostStorageLedger(blockedParent)
	if err := ledger.AppendOperation(validCompactOperation("succeeded")); err == nil {
		t.Fatal("AppendOperation reported success for an unwritable ledger destination")
	}
}

func TestHostStorageLauncherV2DedupeLegacyAndGuestEnrichment(t *testing.T) {
	orig := hostStorageWSLPath
	defer func() { hostStorageWSLPath = orig }()
	root := t.TempDir()
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) { return []byte(root + "\n"), nil }
	dir := filepath.Join(root, "soluterminal")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	op := validCompactOperation("succeeded")
	first, _ := json.Marshal(op)
	op.CompletedAt = op.CompletedAt.Add(time.Minute)
	op.At = op.CompletedAt
	second, _ := json.Marshal(op)
	legacy := `{"at":"2026-09-21T00:00:00Z","kind":"preflight","verdict":"warn"}`
	body := legacy + "\n" + string(first) + "\n" + string(second) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "host-disk.jsonl"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	guestUsed := int64(555)
	events := hostStorageLauncherTailWithGuestUsage(`C:\Users\owner\AppData\Local`, 10, "Ubuntu", &guestUsed)
	if len(events) != 2 || events[0].Kind != "preflight" || events[1].OperationID != op.OperationID {
		t.Fatalf("dedupe/legacy events=%+v", events)
	}
	if events[1].After == nil || events[1].After.GuestUsedBytes == nil || *events[1].After.GuestUsedBytes != 555 || events[1].After.UnavailableReason != "" {
		t.Fatalf("guest enrichment=%+v", events[1].After)
	}
	events = hostStorageLauncherTailWithGuestUsage(`C:\Users\owner\AppData\Local`, 10, "Debian", &guestUsed)
	if events[1].After.GuestUsedBytes != nil || events[1].After.UnavailableReason == "" {
		t.Fatalf("another distro's guest usage was attributed to Ubuntu: %+v", events[1].After)
	}
	events = hostStorageLauncherTailWithGuestUsage(`C:\Users\owner\AppData\Local`, 10, "Ubuntu", nil)
	if events[1].After.GuestUsedBytes != nil || events[1].After.UnavailableReason == "" {
		t.Fatalf("unsafe sample was fabricated: %+v", events[1].After)
	}
}

func TestHostStorageGuestEnrichmentPreservesPhysicalFailureReason(t *testing.T) {
	op := validCompactOperation("succeeded")
	op.After.PhysicalAllocationBytes = nil
	op.After.UnavailableReason = "physical allocation unavailable; post_restart_sample_pending"
	guestUsed := int64(555)
	events := []hostStorageEvent{op}
	enrichHostStorageOperations(events, "Ubuntu", &guestUsed)
	after := events[0].After
	if after == nil || after.GuestUsedBytes == nil || *after.GuestUsedBytes != 555 {
		t.Fatalf("guest enrichment did not fill guest usage: %+v", after)
	}
	if after.PhysicalAllocationBytes != nil {
		t.Fatalf("failed physical measurement was fabricated: %+v", after)
	}
	if !strings.Contains(after.UnavailableReason, "physical allocation unavailable") {
		t.Fatalf("physical failure reason was hidden: %q", after.UnavailableReason)
	}
}

func TestHostStorageLedgerRejectsMalformedV2Operations(t *testing.T) {
	for name, mutate := range map[string]func(*hostStorageMaintenanceOperation){
		"mode":     func(op *hostStorageMaintenanceOperation) { op.ActualMode = "Prezeroed" },
		"path":     func(op *hostStorageMaintenanceOperation) { op.TargetIdentity.CanonicalTarget = `D:\other\ext4.vhdx` },
		"negative": func(op *hostStorageMaintenanceOperation) { value := int64(-1); op.After.HostFreeBytes = &value },
		"missing physical reason": func(op *hostStorageMaintenanceOperation) {
			op.After.PhysicalAllocationBytes = nil
			op.After.UnavailableReason = ""
		},
		"contradictory detach": func(op *hostStorageMaintenanceOperation) { value := false; op.DetachedConfirmed = &value },
		"bad id":               func(op *hostStorageMaintenanceOperation) { op.OperationID = "not-a-uuid" },
	} {
		t.Run(name, func(t *testing.T) {
			op := validCompactOperation("succeeded")
			mutate(&op)
			ledger := newHostStorageLedger(t.TempDir())
			if err := ledger.AppendOperation(op); err == nil {
				t.Fatal("malformed operation accepted")
			}
			if got := ledger.Tail(10); len(got) != 0 {
				t.Fatalf("malformed operation persisted: %+v", got)
			}
		})
	}
}

func TestHostStorageLedgerDiagnosesMalformedV2RowsWithoutDroppingValidRows(t *testing.T) {
	// An externally written v2 operation row that fails validation must not be
	// silently discarded: valid rows stay readable and the skipped malformed
	// row produces a bounded diagnostic instead of vanishing from Tail/status.
	prevWriter, prevFlags := log.Writer(), log.Flags()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}()

	valid := validCompactOperation("succeeded")
	validJSON, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	// Same shape as the Windows/launcher failure this finding reproduces: a
	// null physical measurement with no recorded reason is malformed.
	malformed := validCompactOperation("succeeded")
	malformed.OperationID = hostStorageOperationID()
	malformed.After.PhysicalAllocationBytes = nil
	malformed.After.UnavailableReason = ""
	malformedJSON, err := json.Marshal(malformed)
	if err != nil {
		t.Fatal(err)
	}
	body := string(validJSON) + "\n" + string(malformedJSON) + "\n"

	ledger := newHostStorageLedger(t.TempDir())
	if err := os.WriteFile(ledger.path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	events := ledger.Tail(10)
	if len(events) != 1 || events[0].OperationID != valid.OperationID || events[0].Status != "succeeded" {
		t.Fatalf("valid v2 row was not preserved alongside a malformed row: %+v", events)
	}
	if out := logs.String(); !strings.Contains(out, "skipped 1 malformed v2 operation row") {
		t.Fatalf("malformed v2 row was silently dropped: log=%q", out)
	}

	// The same reader serves launcher/Windows-written records under the user's
	// LocalAppData, so that path must diagnose the malformed row too.
	logs.Reset()
	orig := hostStorageWSLPath
	defer func() { hostStorageWSLPath = orig }()
	root := t.TempDir()
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) { return []byte(root + "\n"), nil }
	launcherDir := filepath.Join(root, "soluterminal")
	if err := os.MkdirAll(launcherDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launcherDir, "host-disk.jsonl"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	launcherEvents := hostStorageLauncherTail(`C:\Users\owner\AppData\Local`, 10)
	if len(launcherEvents) != 1 || launcherEvents[0].OperationID != valid.OperationID {
		t.Fatalf("launcher read dropped the valid v2 row: %+v", launcherEvents)
	}
	if out := logs.String(); !strings.Contains(out, "skipped 1 malformed v2 operation row") {
		t.Fatalf("launcher malformed v2 row was silently dropped: log=%q", out)
	}
}

func TestHostStorageLedgerImportsVersionOnlyV2Record(t *testing.T) {
	op := validCompactOperation("succeeded")
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	wire := strings.Replace(string(b), `"schemaVersion":2,`, "", 1) + "\n"
	path := filepath.Join(t.TempDir(), "launcher.jsonl")
	if err := os.WriteFile(path, []byte(wire), 0600); err != nil {
		t.Fatal(err)
	}
	events := readHostStorageEvents(path, 64<<10, "launcher")
	if len(events) != 1 || events[0].SchemaVersion != 2 || events[0].OperationID != op.OperationID {
		t.Fatalf("version-only v2 record was not imported: %+v wire=%s", events, wire)
	}
}

func TestPublishLauncherPolicyRetriesFailedWriteAndInvalidatesChangedValues(t *testing.T) {
	orig := hostStorageWSLPath
	defer func() { hostStorageWSLPath = orig }()
	root := t.TempDir()
	calls := 0
	hostStorageWSLPath = func(context.Context, string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("transient wslpath failure")
		}
		return []byte(root + "\n"), nil
	}
	e := hostPressureEngine(nil, 667*1024)
	p := diskPressureState{LocalAppData: `C:\Users\alice\AppData\Local`}
	e.publishLauncherPolicy(p)
	e.publishLauncherPolicy(p)
	policyPath := filepath.Join(root, "soluterminal", "host-disk-policy.json")
	b, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("failed publish was incorrectly cached: calls=%d err=%v", calls, err)
	}
	if calls != 2 || strings.Contains(string(b), `"minFreeMB":4096`) {
		t.Fatalf("initial policy calls=%d body=%s", calls, b)
	}
	if _, err := os.Stat(filepath.Join(root, "soluterminal", ".host-disk-policy.tmp")); !os.IsNotExist(err) {
		t.Fatalf("atomic temp file remained after publish: %v", err)
	}

	t.Setenv("SOLU_WORKFLOW_MIN_FREE_HOST_DISK_MB", "4096")
	e.publishLauncherPolicy(p)
	b, err = os.ReadFile(policyPath)
	if err != nil || !strings.Contains(string(b), `"minFreeMB":4096`) || calls != 3 {
		t.Fatalf("changed policy did not invalidate cache: calls=%d err=%v body=%s", calls, err, b)
	}
	e.publishLauncherPolicy(p)
	if calls != 3 {
		t.Fatalf("unchanged policy was not deduplicated: calls=%d", calls)
	}
}

func TestHostStorageLedgerProducerOperationSeparationAndCaps(t *testing.T) {
	dir := t.TempDir()
	l := newHostStorageLedger(dir)
	l.now = func() time.Time { return time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC) }
	l.AppendBoot("boot-a", "1")
	cats := map[string]int64{storageCatWorkflowWorktrees: 42, `C:\Users\alice\secret`: 100}
	l.AppendProducerSample("boot-a", storageProducerSample{At: l.now(), Complete: true, Categories: cats})
	skips := []storageCleanupSkip{}
	for i := 0; i < hostStorageOperationReasonCap+4; i++ {
		skips = append(skips, storageCleanupSkip{Reason: "reason" + itoa64(int64(i)), Count: 1})
	}
	l.AppendCleanupOperation(storageCleanupOperation{At: l.now(), Kind: "worktree_gc", Layer: "guest", Executed: true, Complete: true, Skipped: skips, Detail: `C:\Users\alice\secret`})

	producers := l.TailProducerSamples(10)
	if len(producers) != 1 || len(producers[0].Categories) != 1 || producers[0].Categories[storageCatWorkflowWorktrees] != 42 {
		t.Fatalf("producer IDs were not bounded to known categories: %+v", producers)
	}
	l.AppendProducerSample("boot-a", storageProducerSample{At: l.now().Add(6 * time.Hour), Complete: true, Categories: cats})
	if got := len(l.TailProducerSamples(10)); got != 2 {
		t.Fatalf("unchanged producer samples need a periodic continuity heartbeat; got %d", got)
	}
	ops := l.TailOperations(10)
	if len(ops) != 1 || len(ops[0].Skipped) != hostStorageOperationReasonCap {
		t.Fatalf("operation reason cap = %+v", ops)
	}
	if ops[0].Executed != true || ops[0].Verified {
		t.Fatalf("operation execution flags = %+v", ops[0])
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "alice") || !strings.Contains(string(b), `"kind":"boot"`) || !strings.Contains(string(b), `"kind":"producer_sample"`) || !strings.Contains(string(b), `"kind":"cleanup_operation"`) {
		t.Fatalf("ledger leak or lost rows: %s", b)
	}
}

func TestHostStorageLedgerLegacyMaintenanceNormalization(t *testing.T) {
	l := newHostStorageLedger(t.TempDir())
	now := time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	l.AppendMaintenance("boot-a", "dry-run", "plan only")
	success := validCompactOperation("succeeded")
	success.At = now.Add(time.Minute)
	started := success.At
	success.StartedAt = &started
	success.CompletedAt = started.Add(time.Minute)
	beforePhysical, afterPhysical := int64(1000), int64(400)
	beforeHost, afterHost := int64(500), int64(600)
	success.Before.PhysicalAllocationBytes, success.After.PhysicalAllocationBytes = &beforePhysical, &afterPhysical
	success.Before.HostFreeBytes, success.After.HostFreeBytes = &beforeHost, &afterHost
	if err := l.AppendOperation(success); err != nil {
		t.Fatal(err)
	}
	failed := validCompactOperation("failed")
	failed.At = now.Add(2 * time.Minute)
	started = failed.At
	failed.StartedAt = &started
	failed.CompletedAt = started.Add(time.Minute)
	if err := l.AppendOperation(failed); err != nil {
		t.Fatal(err)
	}
	_ = l.append(hostStorageEvent{At: now.Add(3 * time.Minute), BootID: "boot-a", Kind: "maintenance", Mode: "sparse", ReclaimedBytes: 1 << 40})

	ops := l.TailOperations(10)
	if len(ops) != 4 {
		t.Fatalf("normalized operations = %+v", ops)
	}
	dry := ops[0]
	if dry.Executed || !dry.DryRun || dry.ReclaimedBytes != nil {
		t.Fatalf("legacy dry run was treated as execution: %+v", dry)
	}
	verified := ops[1]
	if !verified.Executed || !verified.Verified || verified.ReclaimedBytes == nil || *verified.ReclaimedBytes != 600 || verified.HostFreeDeltaBytes == nil || *verified.HostFreeDeltaBytes != 100 {
		t.Fatalf("verified compaction normalization = %+v", verified)
	}
	if ops[2].Executed || ops[2].ReclaimedBytes != nil {
		t.Fatalf("failed exit inferred execution: %+v", ops[2])
	}
	if ops[3].Executed || ops[3].ReclaimedBytes != nil {
		t.Fatalf("reclaimedBytes alone inferred execution: %+v", ops[3])
	}
	// The stored generation set is unchanged: normalization is in-memory only.
	if _, err := os.Stat(l.path); err != nil {
		t.Fatalf("legacy rows were rewritten or deleted: %v", err)
	}
}

func TestHostStorageDumpGrowthDrivesCrashDumpCategory(t *testing.T) {
	const gib = int64(1) << 30
	start := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	p := diskPressureState{HostOffenderScannedAt: start, HostOffender: &hostDirUsage{
		Label: `%TEMP%\wsl-crashes`, Exists: true, Bytes: 50 * gib, DumpBytes: 30 * gib, DumpFiles: 3, Complete: true,
	}}
	rows := StorageWindowsHostRows(p)
	dump := storageTestRow(rows, storageCatWindowsCrashDumps)
	temp := storageTestRow(rows, storageCatWindowsTemp)
	if dump == nil || dump.ApparentBytes != 30*gib {
		t.Fatalf("crash dump row did not use dumpBytes: %+v", rows)
	}
	if temp == nil || temp.ApparentBytes != 20*gib {
		t.Fatalf("other Temp row absorbed dump bytes: %+v", rows)
	}
	if dump.ApparentBytes == p.HostOffender.Bytes {
		t.Fatal("crash-dump category was relabeled from the whole Temp directory")
	}
	// The persisted sample keeps dump bytes separate too.
	ev := hostStorageEvent{}
	hostStorageAttribution(&ev, p)
	if ev.DumpBytes != 30*gib || ev.OffenderMB != 50*gib/(1024*1024) {
		t.Fatalf("persisted attribution = %+v", ev)
	}
}

func storageTestRow(rows []storageCategoryRow, category string) *storageCategoryRow {
	for i := range rows {
		if rows[i].Category == category {
			return &rows[i]
		}
	}
	return nil
}

func TestHostStorageLedgerKeepsHourlyArchiveAcrossLiveDedupe(t *testing.T) {
	dir := t.TempDir()
	l := newHostStorageLedger(dir)
	if l.maxBytes != 1<<20 || l.archiveMaxBytes != 2<<20 || filepath.Base(l.archivePath) != "host_storage_hourly.jsonl" {
		t.Fatalf("retention caps=%d/%d", l.maxBytes, l.archiveMaxBytes)
	}
	now := time.Date(2026, 8, 27, 10, 5, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	p := diskPressureState{HostState: hostStorageOK, HostFreeMB: 100_000, VHDXPhysicalMB: 550_000}
	l.AppendSample("boot", p)
	now = now.Add(25 * time.Minute)
	p.HostFreeMB = 99_900 // suppressed in the live ledger, retained hourly
	p.VHDXPhysicalMB = 550_100
	l.AppendSample("boot", p)
	l.AppendCrossing("boot", "host", true, p)
	now = now.Add(35 * time.Minute)
	p.HostFreeMB = 99_000
	p.VHDXPhysicalMB = 551_000
	l.AppendSample("boot", p) // starts the next bucket and flushes 10:00

	events := readHostStorageEvents(l.archivePath, 1<<20, "")
	if len(events) != 1 {
		t.Fatalf("archive events=%+v", events)
	}
	got := events[0]
	for _, ev := range l.Tail(50) {
		if ev.Kind == "hourly_summary" {
			t.Fatal("archive row appeared in live tail")
		}
	}
	if got.Kind != "hourly_summary" || got.HostFreeMinMB != 99_900 || got.HostFreeMaxMB != 100_000 || got.HostFreeLastMB != 99_900 || got.VHDXPhysicalLastMB != 550_100 || got.Crossings != 1 {
		t.Fatalf("hourly summary=%+v", got)
	}
}
