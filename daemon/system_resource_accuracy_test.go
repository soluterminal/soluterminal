package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSystemMemoryAccountingAndEffectiveCapacity(t *testing.T) {
	now := time.Now()
	s := systemSample{SampledAt: now, DaemonRssMB: floatPtr(2100), GoHeapMB: floatPtr(1500), GoRuntimeMB: floatPtr(4600), GuardUsedMB: floatPtr(4600), MemGuardMB: 6144, EngineWarn: true, EngineRollback: true, EngineRollbackCap: 1, EngineEffectiveCap: 4, EngineFloorCap: 4, EngineRollbackReason: "persistent pressure"}
	_, state := systemLevels(s, now, levelState{})
	levels, _ := systemLevels(s, now, state)
	var memory string
	for _, r := range levels.Reasons {
		if r.Key == "daemonMemory" {
			memory = r.Text
		}
		if r.Key == "engine" {
			t.Fatalf("the retired engine is in the verdict's reasons: %+v", levels.Reasons)
		}
	}
	if !strings.Contains(memory, "guard accounting") || strings.Contains(memory, "RSS") {
		t.Fatalf("mislabelled accounting: %s", memory)
	}
	a := &apiServer{system: seededSystemSampler(s), startT: now, dataDir: t.TempDir()}
	rec := httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	var dto systemStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if *dto.Daemon.RssMB != 2100 || *dto.Daemon.GoHeapMB != 1500 || *dto.Daemon.GoRuntimeMB != 4600 || *dto.Daemon.GuardUsedMB != 4600 {
		t.Fatalf("mixed memory metrics: %+v", dto.Daemon)
	}
	s.DaemonRssMB = nil
	levels, _ = systemLevels(s, now, state)
	if levels.Areas["daemonMemory"] != "warn" {
		t.Fatalf("missing RSS hid guard pressure: %+v", levels)
	}
	a.system = seededSystemSampler(s)
	rec = httptest.NewRecorder()
	a.systemStatus(rec, httptest.NewRequest("GET", "/api/system/status", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Daemon.RssMB != nil || *dto.Daemon.GuardUsedMB != 4600 {
		t.Fatal("unavailable resident memory was replaced by guard accounting")
	}
}
