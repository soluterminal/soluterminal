package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type hostStorageTargetIdentity struct {
	OwnerSID        string `json:"ownerSid"`
	Distro          string `json:"distro"`
	LocalAppData    string `json:"localAppData"`
	BasePath        string `json:"basePath"`
	CanonicalTarget string `json:"canonicalTarget"`
	VHDIdentifier   string `json:"vhdIdentifier,omitempty"`
}

type hostStorageMaintenanceMeasurement struct {
	PhysicalAllocationBytes *int64 `json:"physicalAllocationBytes"`
	GuestUsedBytes          *int64 `json:"guestUsedBytes"`
	HostFreeBytes           *int64 `json:"hostFreeBytes"`
	UnavailableReason       string `json:"unavailableReason,omitempty"`
}

type hostStorageMaintenanceFailure struct {
	Stage   string `json:"stage"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type hostStorageEvent struct {
	At                 time.Time                          `json:"at"`
	BootID             string                             `json:"bootId,omitempty"`
	Kind               string                             `json:"kind"`
	Source             string                             `json:"source,omitempty"`
	StartedAt          *time.Time                         `json:"startedAt,omitempty"`
	Version            any                                `json:"version,omitempty"`
	GuestFreeMB        int64                              `json:"guestFreeMB,omitempty"`
	GuestFloorMB       int64                              `json:"guestFloorMB,omitempty"`
	HostState          string                             `json:"hostState,omitempty"`
	HostVolume         string                             `json:"hostVolume,omitempty"`
	HostFreeMB         int64                              `json:"hostFreeMB,omitempty"`
	HostTotalMB        int64                              `json:"hostTotalMB,omitempty"`
	HostFloorMB        int64                              `json:"hostFloorMB,omitempty"`
	HostLow            bool                               `json:"hostLow,omitempty"`
	VHDXPhysicalMB     int64                              `json:"vhdxPhysicalMB,omitempty"`
	VHDXSparse         *bool                              `json:"vhdxSparse,omitempty"`
	BlockingLayer      string                             `json:"blockingLayer,omitempty"`
	Layer              string                             `json:"layer,omitempty"`
	Active             *bool                              `json:"active,omitempty"`
	FreeMB             int64                              `json:"freeMB,omitempty"`
	FloorMB            int64                              `json:"floorMB,omitempty"`
	Cmd                string                             `json:"cmd,omitempty"`
	Stage              string                             `json:"stage,omitempty"`
	Class              string                             `json:"class,omitempty"`
	Detail             string                             `json:"detail,omitempty"`
	Mode               string                             `json:"mode,omitempty"`
	Verdict            string                             `json:"verdict,omitempty"`
	Distro             string                             `json:"distro,omitempty"`
	FreeBytes          int64                              `json:"freeBytes,omitempty"`
	TotalBytes         int64                              `json:"totalBytes,omitempty"`
	FloorBytes         int64                              `json:"floorBytes,omitempty"`
	ReclaimedBytes     int64                              `json:"reclaimedBytes,omitempty"`
	Sparse             *bool                              `json:"sparse,omitempty"`
	Exit               int                                `json:"exit,omitempty"`
	UnexplainedLossMB  int64                              `json:"unexplainedLossMB,omitempty"`
	LossWindowMin      int                                `json:"lossWindowMin,omitempty"`
	VHDXGrowthMB       int64                              `json:"vhdxGrowthMB,omitempty"`
	Offender           string                             `json:"offender,omitempty"`
	OffenderPath       string                             `json:"offenderPath,omitempty"`
	OffenderMB         int64                              `json:"offenderMB,omitempty"`
	OffenderFiles      int                                `json:"offenderFiles,omitempty"`
	OffenderDumps      int                                `json:"offenderDumps,omitempty"`
	OffenderComplete   *bool                              `json:"offenderComplete,omitempty"`
	SuggestedPath      string                             `json:"suggestedPath,omitempty"`
	DumpBytes          int64                              `json:"dumpBytes,omitempty"`
	Producer           *storageProducerSample             `json:"producer,omitempty"`
	CleanupOperation   *storageCleanupOperation           `json:"cleanupOperation,omitempty"`
	SchemaVersion      int                                `json:"schemaVersion,omitempty"`
	OperationID        string                             `json:"operationId,omitempty"`
	Operation          string                             `json:"operation,omitempty"`
	Status             string                             `json:"status,omitempty"`
	TargetIdentity     *hostStorageTargetIdentity         `json:"targetIdentity,omitempty"`
	RequestedMode      string                             `json:"requestedMode,omitempty"`
	ActualMode         string                             `json:"actualMode,omitempty"`
	DetachedConfirmed  *bool                              `json:"detachedConfirmed,omitempty"`
	CompletedAt        time.Time                          `json:"completedAt,omitempty"`
	Before             *hostStorageMaintenanceMeasurement `json:"before,omitempty"`
	After              *hostStorageMaintenanceMeasurement `json:"after,omitempty"`
	Failure            *hostStorageMaintenanceFailure     `json:"failure,omitempty"`
	Hour               *time.Time                         `json:"hour,omitempty"`
	HostFreeMinMB      int64                              `json:"hostFreeMinMB,omitempty"`
	HostFreeMaxMB      int64                              `json:"hostFreeMaxMB,omitempty"`
	HostFreeLastMB     int64                              `json:"hostFreeLastMB,omitempty"`
	VHDXPhysicalMinMB  int64                              `json:"vhdxPhysicalMinMB,omitempty"`
	VHDXPhysicalMaxMB  int64                              `json:"vhdxPhysicalMaxMB,omitempty"`
	VHDXPhysicalLastMB int64                              `json:"vhdxPhysicalLastMB,omitempty"`
	Crossings          int                                `json:"crossings,omitempty"`
}

// The alias keeps operation construction explicit at call sites while the
// JSONL reader remains backward-compatible with legacy hostStorageEvent rows.
type hostStorageMaintenanceOperation = hostStorageEvent

type hostStorageLedger struct {
	mu              sync.Mutex
	path            string
	maxBytes        int64
	now             func() time.Time
	lastSample      *hostStorageEvent
	lastProducer    *storageProducerSample
	archivePath     string
	archiveMaxBytes int64
	hourly          *hostStorageEvent
	growthCachedAt  time.Time
	growthCached    hostGrowthStats
	growthCachedOK  bool
}

var hostStorageWSLPath = func(ctx context.Context, windowsPath string) ([]byte, error) {
	return hiddenCommand(ctx, "wslpath", "-u", windowsPath).Output()
}

func newHostStorageLedger(dataDir string) *hostStorageLedger {
	return &hostStorageLedger{path: filepath.Join(dataDir, "host_storage_events.jsonl"), maxBytes: 1 << 20, archivePath: filepath.Join(dataDir, "host_storage_hourly.jsonl"), archiveMaxBytes: 2 << 20, now: time.Now}
}

func (l *hostStorageLedger) AppendBoot(bootID, version string) {
	if l == nil {
		return
	}
	now := l.now().UTC()
	l.append(hostStorageEvent{At: now, BootID: bootID, Kind: "boot", StartedAt: &now, Version: version})
}

func (l *hostStorageLedger) AppendSample(bootID string, p diskPressureState) {
	if l == nil {
		return
	}
	ev := hostStorageEvent{At: l.now().UTC(), BootID: bootID, Kind: "sample", GuestFreeMB: p.GuestFreeMB, GuestFloorMB: p.GuestFloorMB, HostState: p.HostState, HostVolume: p.HostVolume, HostFreeMB: p.HostFreeMB, HostTotalMB: p.HostTotalMB, HostFloorMB: p.HostFloorMB, HostLow: p.HostLow, VHDXPhysicalMB: p.VHDXPhysicalMB, VHDXSparse: p.VHDXSparse, BlockingLayer: p.BlockingLayer}
	hostStorageAttribution(&ev, p)
	l.mu.Lock()
	l.updateHourlyLocked(ev, false)
	if prev := l.lastSample; prev != nil && ev.At.Sub(prev.At) < hostStorageProducerHeartbeat && prev.HostLow == ev.HostLow && prev.HostState == ev.HostState && prev.BlockingLayer == ev.BlockingLayer && abs64(prev.HostFreeMB-ev.HostFreeMB) < 256 && prev.Offender == ev.Offender && abs64(prev.UnexplainedLossMB-ev.UnexplainedLossMB) < 1024 {
		l.mu.Unlock()
		return
	}
	if err := l.appendLocked(ev); err == nil {
		l.lastSample = &ev
	}
	l.mu.Unlock()
}

func (l *hostStorageLedger) AppendCrossing(bootID, layer string, active bool, p diskPressureState) {
	if l == nil {
		return
	}
	free, floor := p.GuestFreeMB, p.GuestFloorMB
	if layer == "host" {
		free, floor = p.HostFreeMB, p.HostFloorMB
	}
	ev := hostStorageEvent{At: l.now().UTC(), BootID: bootID, Kind: "crossing", Layer: layer, Active: &active, FreeMB: free, FloorMB: floor, HostVolume: p.HostVolume, HostFreeMB: p.HostFreeMB, VHDXPhysicalMB: p.VHDXPhysicalMB, BlockingLayer: p.BlockingLayer}
	hostStorageAttribution(&ev, p)
	l.mu.Lock()
	if layer == "host" {
		l.updateHourlyLocked(hostStorageEvent{At: ev.At, HostFreeMB: p.HostFreeMB, VHDXPhysicalMB: p.VHDXPhysicalMB}, true)
	}
	_ = l.appendLocked(ev)
	l.mu.Unlock()
}

func (l *hostStorageLedger) AppendAdmissionBlocked(bootID, cmd, stage string, p diskPressureState) {
	if l == nil {
		return
	}
	ev := hostStorageEvent{At: l.now().UTC(), BootID: bootID, Kind: "admission_blocked", Cmd: cmd, Stage: stage, Class: p.Class, Layer: "host", HostVolume: p.HostVolume, HostFreeMB: p.HostFreeMB, HostFloorMB: p.HostFloorMB, GuestFreeMB: p.GuestFreeMB, VHDXPhysicalMB: p.VHDXPhysicalMB, BlockingLayer: p.BlockingLayer}
	hostStorageAttribution(&ev, p)
	l.append(ev)
}

func hostStorageAttribution(ev *hostStorageEvent, p diskPressureState) {
	if ev == nil {
		return
	}
	if p.HostLoss.Detected {
		ev.UnexplainedLossMB = p.HostLoss.UnexplainedMB
		ev.VHDXGrowthMB = p.HostLoss.VHDXGrowthMB
		ev.LossWindowMin = int(p.HostLoss.WindowEnd.Sub(p.HostLoss.WindowStart).Round(time.Minute) / time.Minute)
	}
	if p.HostOffender != nil {
		ev.Offender = p.HostOffender.Label
		ev.OffenderPath = p.HostOffender.Path
		ev.OffenderMB = p.HostOffender.Bytes / (1024 * 1024)
		ev.OffenderFiles = p.HostOffender.Files
		ev.OffenderDumps = p.HostOffender.DumpFiles
		ev.DumpBytes = p.HostOffender.DumpBytes
		complete := p.HostOffender.Complete
		ev.OffenderComplete = &complete
		ev.SuggestedPath = p.HostOffender.Path
	}
}

func (l *hostStorageLedger) AppendHostLoss(bootID string, active bool, p diskPressureState) {
	if l == nil {
		return
	}
	ev := hostStorageEvent{At: l.now().UTC(), BootID: bootID, Kind: "host_loss", Active: &active, Layer: "host", HostVolume: p.HostVolume, HostFreeMB: p.HostFreeMB, VHDXPhysicalMB: p.VHDXPhysicalMB}
	hostStorageAttribution(&ev, p)
	l.append(ev)
}

func (l *hostStorageLedger) AppendMaintenance(bootID, mode, detail string) {
	if l == nil {
		return
	}
	l.append(hostStorageEvent{At: l.now().UTC(), BootID: bootID, Kind: "maintenance", Mode: mode, Detail: detail})
}

func newHostStorageMaintenanceOperation(operation string, sample hostStorageSample) hostStorageMaintenanceOperation {
	now := time.Now().UTC()
	target := sample.VHDXPathRaw
	if target == "" {
		target = sample.VHDXPath
	}
	return hostStorageMaintenanceOperation{
		At:            now,
		Kind:          "maintenance_operation",
		SchemaVersion: 2,
		Version:       2,
		OperationID:   hostStorageOperationID(),
		Operation:     operation,
		Status:        "pending",
		StartedAt:     &now,
		TargetIdentity: &hostStorageTargetIdentity{
			OwnerSID: sample.OwnerSID, Distro: sample.Distro, LocalAppData: sample.LocalAppData,
			BasePath: sample.BasePath, CanonicalTarget: target,
		},
	}
}

func hostStorageOperationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is extraordinary. Keep the format valid while
		// ensuring callers still fail validation rather than sharing an ID.
		n := time.Now().UTC().UnixNano()
		for i := range b {
			b[i] = byte(n >> uint((i%8)*8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nonnegativeInt64Ptr(v int64) *int64 {
	if v < 0 {
		return nil
	}
	return &v
}

func (l *hostStorageLedger) AppendOperation(op hostStorageMaintenanceOperation) error {
	if l == nil {
		return nil
	}
	if err := validateHostStorageOperation(op); err != nil {
		return err
	}
	return l.append(op)
}

var hostStorageUUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func validateHostStorageOperation(op hostStorageMaintenanceOperation) error {
	if op.SchemaVersion != 2 || op.Kind != "maintenance_operation" {
		return errors.New("unsupported maintenance operation schema")
	}
	if !hostStorageUUIDRE.MatchString(op.OperationID) {
		return errors.New("malformed operation ID")
	}
	if op.Operation != "compact" && op.Operation != "prepare-trim" {
		return errors.New("malformed operation")
	}
	if op.Status != "succeeded" && op.Status != "failed" {
		return errors.New("malformed operation status")
	}
	if op.StartedAt == nil || op.StartedAt.IsZero() || op.CompletedAt.IsZero() || op.CompletedAt.Before(*op.StartedAt) {
		return errors.New("malformed operation timestamps")
	}
	if op.TargetIdentity == nil {
		return errors.New("target identity missing")
	}
	t := op.TargetIdentity
	if strings.TrimSpace(t.OwnerSID) == "" || strings.TrimSpace(t.Distro) == "" {
		return errors.New("target owner or distro missing")
	}
	local, ok := canonicalWindowsPath(t.LocalAppData)
	if !ok {
		return errors.New("malformed target LocalAppData")
	}
	base, ok := canonicalWindowsPath(t.BasePath)
	if !ok {
		return errors.New("malformed target BasePath")
	}
	target, ok := canonicalWindowsPath(t.CanonicalTarget)
	if !ok || !strings.EqualFold(target, base+`\ext4.vhdx`) || !allowedLocalAppDataLayout(local, target) {
		return errors.New("malformed canonical target")
	}
	if op.Operation == "compact" {
		if op.RequestedMode != "Full" || (op.ActualMode != "" && op.ActualMode != "Full") {
			return errors.New("malformed compaction mode")
		}
		if op.Status == "succeeded" {
			if op.ActualMode != "Full" || op.DetachedConfirmed == nil || !*op.DetachedConfirmed || op.Failure != nil {
				return errors.New("contradictory successful compaction state")
			}
		}
		if op.DetachedConfirmed != nil && !*op.DetachedConfirmed && (op.Failure == nil || op.Failure.Stage != "detach") {
			return errors.New("unconfirmed detach requires a detach failure")
		}
	} else {
		if op.RequestedMode != "fstrim" || (op.ActualMode != "" && op.ActualMode != "fstrim") || op.DetachedConfirmed != nil {
			return errors.New("malformed trim mode")
		}
	}
	if op.Status == "failed" && (op.Failure == nil || strings.TrimSpace(op.Failure.Stage) == "" || strings.TrimSpace(op.Failure.Code) == "" || strings.TrimSpace(op.Failure.Message) == "") {
		return errors.New("failed operation lacks structured failure")
	}
	if op.Status == "succeeded" && op.Failure != nil {
		return errors.New("successful operation has failure")
	}
	for _, m := range []*hostStorageMaintenanceMeasurement{op.Before, op.After} {
		if m == nil {
			continue
		}
		for _, v := range []*int64{m.PhysicalAllocationBytes, m.GuestUsedBytes, m.HostFreeBytes} {
			if v != nil && *v < 0 {
				return errors.New("negative maintenance measurement")
			}
		}
		if m.GuestUsedBytes == nil && strings.TrimSpace(m.UnavailableReason) == "" {
			return errors.New("missing guest usage requires an unavailable reason")
		}
		if (m.PhysicalAllocationBytes == nil || m.HostFreeBytes == nil) && strings.TrimSpace(m.UnavailableReason) == "" {
			return errors.New("missing physical allocation or host free space requires an unavailable reason")
		}
	}
	return nil
}

func (l *hostStorageLedger) append(ev hostStorageEvent) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(ev)
}

func (l *hostStorageLedger) appendLocked(ev hostStorageEvent) error {
	if l.path == "" {
		return errors.New("host storage ledger path is empty")
	}
	// Version-2 operation rows are local execution receipts whose exact
	// current-owner target identity is part of the safety proof. Preserve that
	// identity on disk; read paths validate first and redact before API output.
	if ev.SchemaVersion != 2 {
		redactHostStorageEvent(&ev)
	} else if ev.Failure != nil {
		ev.Failure.Message = protectHostStorageFailure(ev.Failure.Message)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode host storage event: %w", err)
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(l.path), 0700); err != nil {
		return fmt.Errorf("create host storage ledger directory: %w", err)
	}
	if st, err := os.Stat(l.path); err == nil && st.Size()+int64(len(b)) > l.maxBytes {
		_ = os.Remove(l.path + ".2")
		_ = os.Rename(l.path+".1", l.path+".2")
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open host storage ledger: %w", err)
	}
	if n, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("write host storage ledger: %w", err)
	} else if n != len(b) {
		_ = f.Close()
		return io.ErrShortWrite
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close host storage ledger: %w", err)
	}
	if err := os.Chmod(l.path, 0600); err != nil {
		return fmt.Errorf("secure host storage ledger: %w", err)
	}
	return nil
}

func protectHostStorageFailure(message string) string {
	return redactSensitive(boundDetail(message))
}

func (l *hostStorageLedger) updateHourlyLocked(ev hostStorageEvent, crossing bool) {
	if l == nil || l.archivePath == "" || ev.At.IsZero() || ev.HostFreeMB <= 0 {
		return
	}
	hour := ev.At.UTC().Truncate(time.Hour)
	if l.hourly == nil || l.hourly.Hour == nil || !l.hourly.Hour.Equal(hour) {
		if l.hourly != nil {
			l.appendArchiveLocked(*l.hourly)
		}
		l.hourly = &hostStorageEvent{
			At: hour.Add(time.Hour), Kind: "hourly_summary", Hour: &hour,
			HostFreeMinMB: ev.HostFreeMB, HostFreeMaxMB: ev.HostFreeMB, HostFreeLastMB: ev.HostFreeMB,
			VHDXPhysicalMinMB: ev.VHDXPhysicalMB, VHDXPhysicalMaxMB: ev.VHDXPhysicalMB, VHDXPhysicalLastMB: ev.VHDXPhysicalMB,
		}
	}
	h := l.hourly
	h.HostFreeMinMB = min(h.HostFreeMinMB, ev.HostFreeMB)
	h.HostFreeMaxMB = max(h.HostFreeMaxMB, ev.HostFreeMB)
	h.HostFreeLastMB = ev.HostFreeMB
	if ev.VHDXPhysicalMB > 0 {
		if h.VHDXPhysicalMinMB == 0 {
			h.VHDXPhysicalMinMB = ev.VHDXPhysicalMB
		}
		h.VHDXPhysicalMinMB = min(h.VHDXPhysicalMinMB, ev.VHDXPhysicalMB)
		h.VHDXPhysicalMaxMB = max(h.VHDXPhysicalMaxMB, ev.VHDXPhysicalMB)
		h.VHDXPhysicalLastMB = ev.VHDXPhysicalMB
	}
	if crossing {
		h.Crossings++
	}
}

func (l *hostStorageLedger) appendArchiveLocked(ev hostStorageEvent) {
	redactHostStorageEvent(&ev)
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_ = os.MkdirAll(filepath.Dir(l.archivePath), 0700)
	if st, err := os.Stat(l.archivePath); err == nil && st.Size()+int64(len(b)) > l.archiveMaxBytes {
		_ = os.Remove(l.archivePath + ".1")
		_ = os.Rename(l.archivePath, l.archivePath+".1")
	}
	f, err := os.OpenFile(l.archivePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = f.Write(b)
	_ = f.Close()
	_ = os.Chmod(l.archivePath, 0600)
}

func redactHostStorageEvent(ev *hostStorageEvent) {
	if ev == nil {
		return
	}
	ev.BootID = redactSensitive(ev.BootID)
	ev.Kind = redactSensitive(ev.Kind)
	ev.Source = redactSensitive(ev.Source)
	if version, ok := ev.Version.(string); ok {
		ev.Version = redactSensitive(version)
	}
	ev.HostState = redactSensitive(ev.HostState)
	ev.HostVolume = redactSensitive(ev.HostVolume)
	ev.BlockingLayer = redactSensitive(ev.BlockingLayer)
	ev.Layer = redactSensitive(ev.Layer)
	ev.Cmd = redactSensitive(ev.Cmd)
	ev.Stage = redactSensitive(ev.Stage)
	ev.Class = redactSensitive(ev.Class)
	ev.Detail = redactSensitive(ev.Detail)
	ev.Mode = redactSensitive(ev.Mode)
	ev.Verdict = redactSensitive(ev.Verdict)
	ev.Distro = redactSensitive(ev.Distro)
	ev.Offender = redactSensitive(ev.Offender)
	ev.OffenderPath = redactSensitive(ev.OffenderPath)
	ev.SuggestedPath = redactSensitive(ev.SuggestedPath)
	if ev.Producer != nil {
		bounded := boundProducerSample(*ev.Producer)
		ev.Producer = &bounded
	}
	if ev.CleanupOperation != nil {
		bounded := boundStorageOperation(*ev.CleanupOperation)
		ev.CleanupOperation = &bounded
	}
	if ev.TargetIdentity != nil {
		ev.TargetIdentity.LocalAppData = redactSensitive(ev.TargetIdentity.LocalAppData)
		ev.TargetIdentity.BasePath = redactSensitive(ev.TargetIdentity.BasePath)
		ev.TargetIdentity.CanonicalTarget = redactSensitive(ev.TargetIdentity.CanonicalTarget)
	}
	if ev.Failure != nil {
		ev.Failure.Message = redactSensitive(ev.Failure.Message)
	}
}

func (l *hostStorageLedger) Tail(limit int) []hostStorageEvent {
	if l == nil || limit <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []hostStorageEvent
	for _, path := range []string{l.path + ".1", l.path} {
		out = append(out, readHostStorageEvents(path, 256<<10, "")...)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func readHostStorageEvents(path string, max int64, source string) []hostStorageEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > max {
		_, _ = f.Seek(st.Size()-max, 0)
	}
	var out []hostStorageEvent
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), 512<<10)
	invalid := 0
	for s.Scan() {
		var ev hostStorageEvent
		if json.Unmarshal(s.Bytes(), &ev) == nil && !ev.At.IsZero() {
			if ev.SchemaVersion == 0 && ev.OperationID != "" && hostStorageVersionTwo(ev.Version) {
				ev.SchemaVersion = 2
			}
			if ev.SchemaVersion != 0 && validateHostStorageOperation(ev) != nil {
				invalid++
				continue
			}
			redactHostStorageEvent(&ev)
			if source != "" {
				ev.Source = source
			}
			out = append(out, ev)
		}
	}
	if invalid > 0 {
		// Bounded, non-silent diagnostic: externally written malformed v2
		// operation rows (for example a Windows/launcher record whose physical
		// measurement failed validation) stay out of Tail/status because they
		// are unsafe to present, but the operator still gets one summary line
		// per file read instead of the row vanishing without a trace. Emitting
		// one line per read, never one per row, keeps the log bounded.
		log.Printf("host storage ledger: skipped %d malformed v2 operation row(s) in %s", invalid, redactSensitive(filepath.Base(path)))
	}
	return out
}

func hostStorageVersionTwo(value any) bool {
	switch v := value.(type) {
	case float64:
		return v == 2
	case string:
		return v == "2"
	case json.Number:
		return v.String() == "2"
	default:
		return false
	}
}

func hostStorageLauncherTail(localAppDataWin string, limit int) []hostStorageEvent {
	return hostStorageLauncherTailWithGuestUsage(localAppDataWin, limit, "", nil)
}

func hostStorageLauncherTailWithGuestUsage(localAppDataWin string, limit int, guestDistro string, guestUsedBytes *int64) []hostStorageEvent {
	if strings.TrimSpace(localAppDataWin) == "" || limit <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := hostStorageWSLPath(ctx, localAppDataWin)
	if err != nil {
		return nil
	}
	events := readHostStorageEvents(filepath.Join(strings.TrimSpace(string(out)), "soluterminal", "host-disk.jsonl"), 64<<10, "launcher")
	events = dedupeHostStorageOperations(events)
	enrichHostStorageOperations(events, guestDistro, guestUsedBytes)
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events
}

func dedupeHostStorageOperations(events []hostStorageEvent) []hostStorageEvent {
	last := make(map[string]int)
	for i := range events {
		if events[i].SchemaVersion == 2 && events[i].OperationID != "" {
			last[strings.ToLower(events[i].OperationID)] = i
		}
	}
	out := make([]hostStorageEvent, 0, len(events))
	for i, ev := range events {
		if ev.SchemaVersion == 2 && ev.OperationID != "" && last[strings.ToLower(ev.OperationID)] != i {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func enrichHostStorageOperations(events []hostStorageEvent, guestDistro string, guestUsedBytes *int64) {
	guestDistro = strings.TrimSpace(guestDistro)
	for i := range events {
		ev := &events[i]
		if ev.SchemaVersion != 2 || ev.Operation != "compact" || ev.Status != "succeeded" {
			continue
		}
		if ev.After == nil {
			ev.After = &hostStorageMaintenanceMeasurement{UnavailableReason: "post-restart guest usage unavailable"}
		}
		if ev.After.GuestUsedBytes != nil {
			continue
		}
		matchesGuestDistro := guestDistro != "" && ev.TargetIdentity != nil && strings.EqualFold(strings.TrimSpace(ev.TargetIdentity.Distro), guestDistro)
		if matchesGuestDistro && guestUsedBytes != nil && *guestUsedBytes >= 0 {
			v := *guestUsedBytes
			ev.After.GuestUsedBytes = &v
			// Only clear the deferred-sample reason when the other required
			// after measurements are present; otherwise a physical or host-free
			// measurement failure must stay visible alongside the guest value.
			if ev.After.PhysicalAllocationBytes != nil && ev.After.HostFreeBytes != nil {
				ev.After.UnavailableReason = ""
			}
		} else if strings.TrimSpace(ev.After.UnavailableReason) == "" || ev.After.UnavailableReason == "post_restart_sample_pending" {
			ev.After.UnavailableReason = "post-restart guest usage unavailable"
		}
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
