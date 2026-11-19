package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type hostStorageService struct {
	launcherPolicyMu                     sync.Mutex
	launcherPolicyAbs, launcherPolicyPct int
	launcherPolicyAt                     time.Time

	dataDir              string
	bootID               string
	clock                clockSource
	settings             resourceSettingsReader
	diskProbe            func(string) (int64, bool)
	hostStorage          hostStorageSource
	hostLoss             *hostLossTracker
	hostOffender         *hostOffenderProber
	hostLedger           *hostStorageLedger
	hostCrashDumpMu      sync.Mutex
	hostCrashDumpAt      time.Time
	hostCrashDumpRunning atomic.Bool
	hostDiskDeferMax     time.Duration
	lowMu                sync.Mutex
	hostDiskLow          bool
	// afterSample runs after each sample on the sampler's own ticker (the
	// pane recorder's hourly deletion sweep rides it).
	afterSample func()
}

func (s *hostStorageService) resourceOverrides() resourceSettings {
	if s == nil || s.settings == nil {
		return resourceSettings{}
	}
	return s.settings.resourceOverrides()
}
func (s *hostStorageService) diskFloorPath() string {
	if s == nil {
		return ""
	}
	return s.dataDir
}
func (s *hostStorageService) clockNow() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (e *hostStorageService) resolveDiskFloorMB() resolvedCap {
	return resolveDiskFloorMBOverride(e.resourceOverrides().DiskFloorMB)
}
func (e *hostStorageService) resolveHostDiskFloorMB() resolvedCap {
	return resolveHostDiskFloorMBOverride(e.resourceOverrides().HostDiskFloorMB)
}
func (e *hostStorageService) resolveHostDiskFloorPct() resolvedCap {
	return resolveHostDiskFloorPctOverride(e.resourceOverrides().HostDiskFloorPct)
}
func (e *hostStorageService) resolveHostDiskHysteresisMB() resolvedCap {
	return resolveHostDiskHysteresisMBOverride(e.resourceOverrides().HostDiskHysteresisMB)
}
func (e *hostStorageService) resolveBrowserHostReserveMB() resolvedCap {
	return resolveBrowserHostReserveMBOverride(e.resourceOverrides().BrowserHostReserveMB)
}
func (e *hostStorageService) resolveAdaptiveHostDiskFloor() resolvedFlag {
	return resolveOptInFlag(e.resourceOverrides().AdaptiveHostDiskFloor, "SOLU_WORKFLOW_HOST_DISK_ADAPTIVE_FLOOR")
}
func (e *hostStorageService) resolveCrashDumpRotation() resolvedFlag {
	return resolveOptInFlag(e.resourceOverrides().RotateWSLCrashDumps, "SOLU_WORKFLOW_ROTATE_WSL_CRASH_DUMPS")
}
func (s *hostStorageService) hostLowActive() bool {
	s.lowMu.Lock()
	defer s.lowMu.Unlock()
	return s.hostDiskLow
}
func newHostStorageService(dataDir string, settings resourceSettingsReader) *hostStorageService {
	s := &hostStorageService{dataDir: dataDir, settings: settings, bootID: newHostStorageBootID(), clock: realClock{}, hostLedger: newHostStorageLedger(dataDir), hostLoss: newWfHostLossTracker(), hostOffender: newWfHostOffenderProber()}
	s.diskProbe = func(path string) (int64, bool) { return diskFreeBytes(path) }
	if isInsideWSL() && os.Getenv("SOLU_HOST_STORAGE_PROBE") != "0" {
		s.hostStorage = newWfHostStorageProber()
	}
	return s
}
func (a *apiServer) hostStorageServiceOr503(w http.ResponseWriter) *hostStorageService {
	if a.storage == nil {
		jsonError(w, http.StatusServiceUnavailable, "host storage service unavailable")
		return nil
	}
	return a.storage
}

func (e *hostStorageService) hostDiskUnblockOptions(p diskPressureState) []systemHostDiskUnblockOption {
	if e == nil || !p.HostLow {
		return nil
	}
	var options []systemHostDiskUnblockOption
	if p.HostDistro != "" && p.VHDXPath != "" {
		options = append(options, systemHostDiskUnblockOption{Kind: "compact", Command: "solud host-storage compact"})
	}
	if p.LocalAppData != "" {
		options = append(options, systemHostDiskUnblockOption{Kind: "rotate-crash-dumps", Consented: e.resolveCrashDumpRotation().Value, Action: "/api/system/host-storage/rotate-crash-dumps"})
	}
	if e.resolveHostDiskFloorMB().Source == capSourceAdaptive && e.resolveHostDiskFloorPct().Source == capSourceAdaptive && p.HostBaseFloorMB > 0 && e.hostLedger != nil {
		window := e.hostDiskDeferMax
		if window <= 0 {
			window = 30 * time.Minute
		}
		if stats, ok := e.hostLedger.GrowthStats(window); ok {
			delta := p.HostBaseFloorMB - measuredHostDiskFloorMB(stats, p.VHDXSparse != nil && *p.VHDXSparse)
			if delta > 0 {
				options = append(options, systemHostDiskUnblockOption{Kind: "enable-adaptive-floor", FloorDeltaMB: &delta, Consented: e.resolveAdaptiveHostDiskFloor().Value, Action: "/api/workflow/settings"})
			}
		}
	}
	return options
}

func (s *hostStorageService) noteHostDiskCrossing(p diskPressureState) {
	s.lowMu.Lock()
	changed := s.hostDiskLow != p.HostLow
	s.hostDiskLow = p.HostLow
	s.lowMu.Unlock()
	if changed && s.hostLedger != nil {
		s.hostLedger.AppendSample(s.bootID, p)
	}
}

// newHostStorageBootID names this daemon run in the host-storage ledger.
func newHostStorageBootID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "rc_" + hex.EncodeToString(b)
}

// sampleHost records one host-disk reading: the growth ledger, the low/ok
// crossing, opt-in crash-dump rotation and the launcher's disk policy.
func (s *hostStorageService) sampleHost(ctx context.Context) {
	p := s.diskPressure(ctx, "generic")
	s.refreshHostOffender(ctx, &p)
	if p.HostKnown {
		s.noteHostDiskCrossing(p)
	}
	if s.hostLedger != nil {
		s.hostLedger.AppendSample(s.bootID, p)
	}
	s.dispatchHostMaintenanceOnSample(ctx, p)
	s.publishLauncherPolicy(p)
	if s.afterSample != nil {
		s.afterSample()
	}
}

// runSampler samples the host disk now and then every `every`, inside WSL
// only (elsewhere there is no host disk to watch).
func (s *hostStorageService) runSampler(ctx context.Context, every time.Duration) {
	if s == nil || (s.hostStorage == nil && s.afterSample == nil) {
		return
	}
	tick := func() {
		if s.hostStorage != nil {
			s.sampleHost(ctx)
		} else {
			s.afterSample()
		}
	}
	tick()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
