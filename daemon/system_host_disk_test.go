package main

import (
	"strings"
	"testing"
	"time"
)

func TestSystemLevelsSeparatesWindowsHostDisk(t *testing.T) {
	guestFree, guestTotal, hostFree, hostTotal, floor := int64(667000), int64(1048576), int64(41000), int64(2050000), int64(61500)
	rss, cpu := 100.0, 1.0
	mem := int64(10000)
	unexplained := int64(51095)
	sparse := false
	s := systemSample{DiskFreeMB: &guestFree, DiskTotalMB: &guestTotal, DiskFloorMB: 10240, MemAvailableMB: &mem, DaemonRssMB: &rss, DaemonCpuPct: &cpu, HostDisk: &systemHostDiskSample{State: hostStorageOK, Volume: `C:\`, FreeMB: &hostFree, TotalMB: &hostTotal, FloorMB: &floor, VHDXPhysicalMB: &hostTotal, Sparse: &sparse, Low: true, UnexplainedLossMB: &unexplained, Offender: &systemHostOffender{Text: `51.0 GB of WSL crash dumps (10 .dmp files) in %TEMP%\wsl-crashes`}}, EngineWarn: true, EngineHostDiskLow: true}
	_, state := systemLevels(s, time.Now(), levelState{})
	levels, _ := systemLevels(s, time.Now(), state)
	// 41 GB free on a 2 TB drive is under the heavy-work floor but not
	// "almost full": the person-facing alarm is a warning, not critical.
	var engine string
	var host systemLevelReason
	for _, r := range levels.Reasons {
		if r.Key == "engine" {
			engine = r.Text
		}
		if r.Key == "hostDisk" {
			host = r
		}
	}
	if levels.Areas["disk"] != "ok" || levels.Areas["hostDisk"] != "warn" || host.Text != `host disk: 40.0 GB free on C:\` {
		t.Fatalf("levels=%+v", levels)
	}
	for _, want := range []string{"2.0% of 2002.0 GB free", "VHDX 2002.0 GB physical, not sparse", `%TEMP%\wsl-crashes`, "unexplained host loss 49.9 GB"} {
		if !strings.Contains(host.Detail, want) {
			t.Fatalf("detail %q missing %q", host.Detail, want)
		}
	}
	if strings.Contains(host.Text, "VHDX") || strings.Contains(host.Text, "%TEMP%") {
		t.Fatalf("chip text carries detail: %q", host.Text)
	}
	if !strings.Contains(engine, "host disk floor active") {
		t.Fatalf("engine reason=%q all=%+v", engine, levels)
	}
}

func TestSystemLevelsNonWSLKeySetUnchanged(t *testing.T) {
	free, total := int64(100000), int64(200000)
	rss, cpu := 100.0, 1.0
	mem := int64(10000)
	levels, _ := systemLevels(systemSample{DiskFreeMB: &free, DiskTotalMB: &total, MemAvailableMB: &mem, DaemonRssMB: &rss, DaemonCpuPct: &cpu}, time.Now(), levelState{})
	if _, ok := levels.Areas["hostDisk"]; ok {
		t.Fatalf("native sample added hostDisk: %+v", levels.Areas)
	}
}

func TestHostDiskAlarmLevelSeparateFromHeavyWorkFloor(t *testing.T) {
	const gb = int64(1024)
	defaultFloor := func(total int64) int64 {
		return hostDiskEffectiveFloorMB(total, hostDiskFloorDefaultMB, hostDiskFloorDefaultPct)
	}
	for _, tc := range []struct {
		name               string
		free, total, floor int64
		want               string
	}{
		{"1 TB laptop with 90 GB free", 90 * gb, 1024 * gb, defaultFloor(1024 * gb), "ok"},
		{"512 GB laptop with 45 GB free", 45 * gb, 512 * gb, defaultFloor(512 * gb), "warn"},
		{"1 TB laptop with 45 GB free", 45 * gb, 1024 * gb, defaultFloor(1024 * gb), "warn"},
		{"2 TB drive with 102.9 GB free", 103 * gb, 2048 * gb, defaultFloor(2048 * gb), "ok"},
		{"1 TB laptop with 15 GB free", 15 * gb, 1024 * gb, defaultFloor(1024 * gb), "critical"},
		{"256 GB laptop with 15 GB free (5.9%)", 15 * gb, 256 * gb, defaultFloor(256 * gb), "warn"},
		{"256 GB laptop with 12 GB free (4.7%)", 12 * gb, 256 * gb, defaultFloor(256 * gb), "critical"},
		{"4 TB drive with 25 GB free", 25 * gb, 4096 * gb, defaultFloor(4096 * gb), "warn"},
		{"floor disabled still alarms", 10 * gb, 1024 * gb, 0, "critical"},
		{"operator floor lower than the alarm lowers it", 15 * gb, 1024 * gb, 8 * gb, "warn"},
		{"operator floor lower than the alarm, under it", 7 * gb, 1024 * gb, 8 * gb, "critical"},
	} {
		if got := hostDiskAlarmLevel(tc.free, tc.total, tc.floor); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}
