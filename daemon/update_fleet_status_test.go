package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"soluterminal-shared/machineauth"
	"soluterminal-shared/releases"
	"soluterminal-shared/updatestatus"
)

func fleetTestUpdater() *Updater {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = &atomic.Bool{}
	u.autoApplyWhenIdle = func() bool { return true }
	u.status.Latest, u.status.Available = "2.0.0", true
	return u
}

func TestFleetUpdateStatusReasons(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		setup        func(*Updater)
	}{
		{"ready", "", func(u *Updater) {}},
		{"paused", "kill-switch", func(u *Updater) { u.ctrlEffective = &UpdateControl{Paused: true} }},
		{"quarantine", "quarantined", func(u *Updater) { u.ctrlEffective = &UpdateControl{HaltAtOrAbove: "2.0.0"} }},
		{"floor", "floor", func(u *Updater) { u.ctrlEffective = &UpdateControl{HaltBelow: "3.0.0"} }},
		{"cohort", "rollout-cohort", func(u *Updater) { u.ctrlEffective = &UpdateControl{RolloutPercent: intPtr(0)} }},
		{"mandatory-bypasses-cohort", "", func(u *Updater) { u.ctrlEffective = &UpdateControl{RolloutPercent: intPtr(0), MinDaemonVer: "2.0.0"} }},
		{"hold", "version-hold", func(u *Updater) { u.status.HoldVersion = "2.0.0"; u.status.HoldReason = "private user note" }},
		{"drain", "drain", func(u *Updater) { u.drainStart = time.Now(); u.drainInflight = 1 }},
		{"backoff", "drain", func(u *Updater) { u.nextRetryTime = time.Now().Add(time.Minute) }},
		{"restart", "restart-pending", func(u *Updater) { u.restartGate.Store(true) }},
		{"swapped", "restart-pending", func(u *Updater) { u.swapped = true }},
		{"applying", "applying", func(u *Updater) { u.applying = true }},
		{"busy", "busy", func(u *Updater) {
			u.busyCheck = func() (bool, string, string) { return true, "private reason", "private pane" }
		}},
		{"busy-without-candidate", "", func(u *Updater) {
			u.status.Available = false
			u.busyCheck = func() (bool, string, string) { return true, "", "" }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := fleetTestUpdater()
			tc.setup(u)
			s := u.fleetUpdateStatus()
			if s.Reason != tc.reason {
				t.Fatalf("got %+v; want reason %s", s, tc.reason)
			}
			b, _ := json.Marshal(s)
			if strings.Contains(string(b), "private") {
				t.Fatalf("leaked private context: %s", b)
			}
		})
	}
}

func TestFleetUpdateStatusDisabledAndTimes(t *testing.T) {
	t.Setenv("SOLU_AUTO_UPDATE", "0")
	u := fleetTestUpdater()
	u.autoApplyWhenIdle = func() bool { return false }
	u.status.LastCheckedMs, u.status.LastSuccessMs = 123, 122
	s := u.fleetUpdateStatus()
	if s.AutoUpdate || s.DisabledReason != "environment" || s.LastCheckMs != 123 || s.LastSuccessMs != 0 {
		t.Fatalf("status %+v", s)
	}
	u.status.Required = true
	if s = u.fleetUpdateStatus(); !s.AutoUpdate || s.DisabledReason != "" {
		t.Fatalf("required policy %+v", s)
	}
	u.current = "dev"
	if s = u.fleetUpdateStatus(); s.AutoUpdate || s.DisabledReason != "dev-build" {
		t.Fatalf("dev %+v", s)
	}
	u.current, u.fleetLastSuccessMs = "1.0.0.42", 100
	if s = u.fleetUpdateStatus(); s.Channel != "preview" || s.LastSuccessMs != 100 {
		t.Fatalf("preview %+v", s)
	}
	u.fleetPollDisabled = "signature-key-invalid"
	if s = u.fleetUpdateStatus(); s.AutoUpdate || s.DisabledReason != "signature-key-invalid" {
		t.Fatalf("key %+v", s)
	}
}

func TestUpdateStatusReportsRunningFlavorAndChannel(t *testing.T) {
	for _, tc := range []struct{ stamp, channel, running string }{
		{buildFlavorMarkerPrefix + releases.FlavorPublic, releases.FlavorTeam, releases.FlavorPublic},
		{buildFlavorMarkerPrefix + releases.FlavorTeam, releases.FlavorPublic, releases.FlavorTeam},
		{buildFlavorMarkerPrefix + releases.FlavorTeam, releases.FlavorTeam, releases.FlavorTeam},
	} {
		setBuildFlavorStamp(t, tc.stamp)
		u := fleetTestUpdater()
		u.status.UpdateChannel = tc.channel
		if st := u.Status(); st.RunningFlavor != tc.running || st.UpdateChannel != tc.channel {
			t.Fatalf("update status %+v, want running %s channel %s", st, tc.running, tc.channel)
		}
		if s := u.fleetUpdateStatus(); s.RunningFlavor != tc.running || s.UpdateChannel != tc.channel || s.Channel != "stable" {
			t.Fatalf("fleet status %+v, want running %s channel %s", s, tc.running, tc.channel)
		}
	}
}

// The channel's repo can still serve another flavour's release that is not newer
// (the one-time migration release). Check treats that as a successful answer
// with no release of the channel, and the status must let the app say so.
func TestUpdateStatusAfterAnAnswerWithNoReleaseOfTheChannel(t *testing.T) {
	disableControlSignature(t)
	setBuildFlavorStamp(t, buildFlavorMarkerPrefix+releases.FlavorTeam)
	u := directFlavorUpdater(t, releases.FlavorPublic, flavorManifest(releases.FlavorTeam, "1.0.0"), nil)
	u.status.UpdateChannel = releases.FlavorPublic
	st, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || st.Latest != "" || st.LastError != "" || st.LastSuccessMs == 0 ||
		st.RunningFlavor != releases.FlavorTeam || st.UpdateChannel != releases.FlavorPublic {
		t.Fatalf("status %+v", st)
	}
}

// A check that succeeded on the previous channel says nothing about this one.
func TestChannelSwitchForgetsTheLastSuccessfulCheck(t *testing.T) {
	u := fleetTestUpdater()
	u.manifestURLOverride = true
	u.status.LastSuccessMs = 123
	u.channelStore = newUpdateChannelStore(t.TempDir())
	u.channelEvidence = func() channelEvidence {
		return channelEvidence{Verdict: releases.FlavorTeam, VerdictAt: time.Now(), Initial: releases.FlavorPublic}
	}
	u.syncChannel()
	if st := u.Status(); st.UpdateChannel != releases.FlavorTeam || st.LastSuccessMs != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestFleetStatusSanitizesFlavorFields(t *testing.T) {
	s := updatestatus.Status{Channel: "stable", RunningFlavor: "team-private", UpdateChannel: "https://example.test/repo"}.Sanitize()
	if s.RunningFlavor != "" || s.UpdateChannel != "" {
		t.Fatalf("unknown values survived: %+v", s)
	}
	var r fleetStatusReporter
	now := time.Now()
	r.last, r.lastAttempt, r.lastDelivered = updatestatus.Status{Channel: "stable", UpdateChannel: "public"}, now.Add(-time.Minute), true
	if !r.due(updatestatus.Status{Channel: "stable", UpdateChannel: "team"}, now) {
		t.Fatal("a channel change must be reported on the next sample")
	}
}

func TestFleetUpdateStatusReportsSuppressedCheckFailures(t *testing.T) {
	disableManifestSignature(t)
	u := fleetTestUpdater()
	u.manifestURL = "https://updates.test/update.json?secret=do-not-send"
	u.httpClient = &http.Client{Transport: &scriptedRT{statuses: []int{500}}}
	st, err := u.Check(context.Background())
	if err == nil || st.LastError != "" {
		t.Fatalf("expected UI-suppressed failure: %v %+v", err, st)
	}
	s := u.fleetUpdateStatus()
	if s.LastCheckError == "" || s.LastCheckMs == 0 || s.LatestVersion != "2.0.0" {
		t.Fatalf("missing failure/cache %+v", s)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "https") {
		t.Fatalf("leak %s", b)
	}
}

func TestFleetUpdateStatusControlFailureAndRecovery(t *testing.T) {
	disableManifestSignature(t)
	u := fleetTestUpdater()
	u.manifestURL = "https://updates.test/update.json"
	u.httpClient = &http.Client{Transport: &scriptedRT{
		statuses: []int{200, 500, 200, 404},
		body:     []byte(`{"flavor":"public","version":"2.0.0","platforms":{"` + platformKeyCurrent() + `":{"url":"https://updates.test/solud","sha256":"abc"}}}`),
	}}
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := u.fleetUpdateStatus()
	if s.LastCheckError != "control" || s.Reason != "kill-switch" {
		t.Fatalf("control failure %+v", s)
	}
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = u.fleetUpdateStatus()
	if s.LastCheckError != "" || s.Reason != "" || s.LastSuccessMs != 0 {
		t.Fatalf("recovery %+v", s)
	}
}

func TestFleetUpdateStatusPollRefusal(t *testing.T) {
	t.Setenv("SOLU_UPDATE_MANIFEST_PUBKEY", "invalid-test-key")
	u := fleetTestUpdater()
	u.StartBackgroundPoll(context.Background())
	s := u.fleetUpdateStatus()
	if s.AutoUpdate || s.DisabledReason != "signature-key-invalid" || s.LastCheckMs != 0 {
		t.Fatalf("poll refusal %+v", s)
	}
}

func TestFleetStatusReporterCadence(t *testing.T) {
	now := time.Now()
	s := updatestatus.Status{Channel: "stable"}
	r := fleetStatusReporter{}
	if !r.due(s, now) {
		t.Fatal("initial report missing")
	}
	r.last, r.lastAttempt, r.lastDelivered = s, now, true
	s.LastCheckMs = 123
	if r.due(s, now.Add(time.Minute)) {
		t.Fatal("timestamp-only poll triggered report")
	}
	s.Reason = "busy"
	if r.due(s, now.Add(time.Second)) || !r.due(s, now.Add(time.Minute)) {
		t.Fatal("transition cadence")
	}
	r.lastDelivered = false
	if r.due(s, now.Add(time.Minute)) || !r.due(s, now.Add(5*time.Minute)) {
		t.Fatal("failed delivery retry cadence")
	}
	r.lastDelivered = true
	if !r.due(r.last, now.Add(5*time.Minute)) {
		t.Fatal("heartbeat missing")
	}
}

func TestFleetStatusPostSignedAndSanitized(t *testing.T) {
	const token = "test-fleet-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/api/internal/update-status" {
			t.Error(r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing auth")
		}
		// Match the signing contract using the request timestamp.
		if result, err := machineauth.VerifyHeaders(r.Header, token, b, time.Now()); err != nil || result.Legacy {
			t.Errorf("invalid signature: %v", err)
		}
		if strings.Contains(string(b), "secret") {
			t.Errorf("leaked %s", b)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	if !postFleetStatus(context.Background(), server.URL, token, updatestatus.Status{Channel: "stable", LastCheckError: "secret"}) {
		t.Fatal("delivery failed")
	}
	for _, err := range []error{errors.New("signature secret"), &url.Error{Op: "Get", URL: "https://secret", Err: context.DeadlineExceeded}, errors.New("manifest secret")} {
		if strings.Contains(fleetCheckError(err), "secret") {
			t.Fatal("error leak")
		}
	}
}
