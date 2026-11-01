package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"soluterminal-shared/releases"
)

var chanT0 = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

func TestUpdateChannelFirstDecision(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ev           channelEvidence
		want, reason string
	}{
		{"new public download", channelEvidence{Initial: "public"}, "public", channelReasonInitial},
		{"new team download", channelEvidence{Initial: "team"}, "team", channelReasonInitial},
		{"migration release, no verdict", channelEvidence{Initial: "public"}, "public", channelReasonInitial},
		{"team account already confirmed", channelEvidence{Initial: "public", Verdict: "team", VerdictAt: chanT0}, "team", channelReasonAccount},
		{"non-team account already confirmed", channelEvidence{Initial: "team", Verdict: "public", VerdictAt: chanT0}, "public", channelReasonAccount},
		{"explicit local choice", channelEvidence{Initial: "public", Verdict: "public", VerdictAt: chanT0, Override: "team"}, "team", channelReasonOverride},
		{"unknown initial is public", channelEvidence{Initial: "nightly"}, "public", channelReasonInitial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, changed := decideUpdateChannel(nil, tc.ev, chanT0)
			if !changed || rec.Channel != tc.want || rec.Reason != tc.reason {
				t.Fatalf("decision = %+v changed=%v, want %s (%s)", rec, changed, tc.want, tc.reason)
			}
		})
	}
}

func TestUpdateChannelMovesOnlyOnAFreshPositiveVerdict(t *testing.T) {
	steps := []struct {
		name string
		ev   channelEvidence
		want string
	}{
		{"team account confirmed", channelEvidence{Verdict: "team", VerdictAt: chanT0}, "team"},
		{"sign-out: no new verdict", channelEvidence{Verdict: "team", VerdictAt: chanT0}, "team"},
		{"offline / cloud 5xx: verdict cache untouched", channelEvidence{Verdict: "team", VerdictAt: chanT0}, "team"},
		{"anonymous session: not a verdict", channelEvidence{}, "team"},
		{"the initial channel cannot move a decided install", channelEvidence{Initial: "public"}, "team"},
		{"non-team account confirmed: team -> public", channelEvidence{Verdict: "public", VerdictAt: chanT0.Add(time.Minute)}, "public"},
		{"an older team verdict replayed", channelEvidence{Verdict: "team", VerdictAt: chanT0}, "public"},
		{"sign-out again", channelEvidence{Verdict: "public", VerdictAt: chanT0.Add(time.Minute)}, "public"},
		{"sign-in to a team account: public -> team", channelEvidence{Verdict: "team", VerdictAt: chanT0.Add(2 * time.Minute)}, "team"},
	}
	var prev *updateChannelRecord
	for i, step := range steps {
		rec, _ := decideUpdateChannel(prev, step.ev, chanT0.Add(time.Duration(i)*time.Second))
		if rec.Channel != step.want {
			t.Fatalf("step %d (%s): channel %q, want %q", i, step.name, rec.Channel, step.want)
		}
		prev = &rec
	}
}

func TestChannelVerdictCountsOnlyRealAccounts(t *testing.T) {
	for _, tc := range []struct {
		rec  entitlementRecord
		want string
	}{
		{entitlementRecord{}, ""},
		{entitlementRecord{Tier: tierNone, CheckedAt: chanT0}, ""},
		{entitlementRecord{Tier: tierAccount, CheckedAt: chanT0}, "public"},
		{entitlementRecord{Tier: tierTeam, Team: true, CheckedAt: chanT0}, "team"},
		{entitlementRecord{Team: true, CheckedAt: chanT0}, "team"},    // record from before tiers
		{entitlementRecord{Team: false, CheckedAt: chanT0}, "public"}, // record from before tiers
	} {
		if got, _ := channelVerdict(tc.rec); got != tc.want {
			t.Fatalf("channelVerdict(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
}

func TestUpdateChannelSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := newUpdateChannelStore(dir)
	if rec, moved := s.decide(channelEvidence{Initial: "public", Verdict: "team", VerdictAt: chanT0}, chanT0); !moved || rec.Channel != "team" {
		t.Fatalf("first decision = %+v moved=%v", rec, moved)
	}
	// After a restart nothing new is known (signed out, offline): the
	// install stays where it was.
	s = newUpdateChannelStore(dir)
	if rec, moved := s.decide(channelEvidence{Initial: "public"}, chanT0.Add(time.Hour)); moved || rec.Channel != "team" {
		t.Fatalf("after restart = %+v moved=%v", rec, moved)
	}
}

// entitlementsAgainst points an entitlements cache at a fake cloud.
func entitlementsAgainst(t *testing.T, handler http.HandlerFunc) *teamEntitlements {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	e := newTeamEntitlements(t.TempDir(), nil)
	e.accountBase = func() string { return srv.URL }
	e.client = srv.Client()
	return e
}

func TestUpdateChannelThroughTheEntitlementsCache(t *testing.T) {
	answer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	t.Run("team -> public needs an explicit non-team account verdict", func(t *testing.T) {
		var reply http.HandlerFunc
		e := entitlementsAgainst(t, func(w http.ResponseWriter, r *http.Request) { reply(w, r) })
		store := newUpdateChannelStore(t.TempDir())
		decide := func() string {
			rec, _ := store.decide(entitlementChannelEvidence(e), time.Now())
			return rec.Channel
		}
		reply = answer(200, `{"team":true,"tier":"team"}`)
		e.refresh(context.Background(), "token")
		if got := decide(); got != "team" {
			t.Fatalf("team account: %q", got)
		}
		for _, outage := range []struct {
			name  string
			reply http.HandlerFunc
		}{
			{"cloud 5xx", answer(503, "down")},
			{"expired session (401)", answer(401, "unauthenticated")},
			{"anonymous session", answer(200, `{"team":false,"tier":"none"}`)},
			{"unparseable answer", answer(200, `not json`)},
		} {
			reply = outage.reply
			e.refresh(context.Background(), "token")
			if got := decide(); got != "team" {
				t.Fatalf("%s moved the install to %q", outage.name, got)
			}
		}
		// Signed out: the web stops forwarding a token, so refresh asks no one.
		e.refresh(context.Background(), "")
		if got := decide(); got != "team" {
			t.Fatalf("sign-out moved the install to %q", got)
		}
		// Offline: the cloud cannot be reached at all.
		e.accountBase = func() string { return "http://127.0.0.1:1" }
		e.refresh(context.Background(), "token")
		if got := decide(); got != "team" {
			t.Fatalf("offline moved the install to %q", got)
		}
		time.Sleep(2 * time.Millisecond) // a strictly newer CheckedAt
		e = entitlementsAgainst(t, answer(200, `{"team":false,"tier":"account"}`))
		e.refresh(context.Background(), "token")
		if got := decide(); got != "public" {
			t.Fatalf("non-team account verdict: %q, want public", got)
		}
	})
	t.Run("public -> team on sign-in to a team account", func(t *testing.T) {
		e := entitlementsAgainst(t, answer(200, `{"team":true,"tier":"team"}`))
		store := newUpdateChannelStore(t.TempDir())
		if rec, _ := store.decide(channelEvidence{Initial: "public"}, chanT0); rec.Channel != "public" {
			t.Fatalf("fresh public install: %q", rec.Channel)
		}
		e.refresh(context.Background(), "") // not signed in yet
		if rec, _ := store.decide(entitlementChannelEvidence(e), time.Now()); rec.Channel != "public" {
			t.Fatalf("before sign-in: %q", rec.Channel)
		}
		e.refresh(context.Background(), "token") // signs in to a team account
		if rec, _ := store.decide(entitlementChannelEvidence(e), time.Now()); rec.Channel != "team" || rec.Reason != channelReasonAccount {
			t.Fatalf("after sign-in: %+v", rec)
		}
	})
}

func TestUpdaterFollowsTheChannelAcrossSources(t *testing.T) {
	setTeamUpdatesPath(t, testTeamUpdatesPath)
	t.Setenv("SOLU_UPDATE_MANIFEST_URL", "")
	t.Setenv("SOLU_UPDATE_CHANNEL", "")
	setBuildFlavorStamp(t, "solu-build-flavor=public")
	priv := flavorKey(t)
	bodies := map[string][]byte{}
	teamReleases := teamUpdatesBase(testTeamCloud, testTeamUpdatesPath) + "releases/"
	serveRelease(bodies, priv, releases.ReleasesBase(releases.Repo), releases.FlavorPublic, "2.0.0")
	serveRelease(bodies, priv, teamReleases, releases.FlavorTeam, "3.0.0")
	sources := map[string]string{releases.FlavorPublic: releases.ReleasesBase(releases.Repo), releases.FlavorTeam: teamReleases}
	rt := &recordingRoundTripper{bodies: bodies}

	u := NewUpdater("1.0.0", "", "windows-amd64", nil, nil)
	u.homeDir = t.TempDir()
	u.SetCloudCreds(testTeamCloud, testMachineToken)
	u.httpClient = &http.Client{Transport: &teamUpdatesAuth{next: rt, creds: u.cloudCreds}}
	u.channelStore = newUpdateChannelStore(t.TempDir())
	ev := channelEvidence{Initial: initialUpdateChannel()}
	u.channelEvidence = func() channelEvidence { return ev }

	check := func(wantChannel, wantLatest string) {
		t.Helper()
		rt.seen = nil
		st, err := u.Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.UpdateChannel != wantChannel || st.Latest != wantLatest || !st.Available {
			t.Fatalf("status = channel %q latest %q available %v, want %s %s", st.UpdateChannel, st.Latest, st.Available, wantChannel, wantLatest)
		}
		for _, url := range rt.seen {
			// A 404 under the public repo is retried under its legacy owner.
			legacy := wantChannel == releases.FlavorPublic && strings.HasPrefix(url, releases.ReleasesBase(releases.LegacyRepo))
			if !strings.HasPrefix(url, sources[wantChannel]) && !legacy {
				t.Fatalf("%s channel requested %s", wantChannel, url)
			}
		}
	}
	check("public", "2.0.0")
	ev.Verdict, ev.VerdictAt = "team", chanT0 // signs in to a team account
	check("team", "3.0.0")
	ev.Verdict, ev.VerdictAt = "", time.Time{} // signs out
	check("team", "3.0.0")
	ev.Verdict, ev.VerdictAt = "public", chanT0.Add(time.Hour) // account no longer team
	check("public", "2.0.0")
	if u.manifest == nil || u.manifest.Flavor != releases.FlavorPublic {
		t.Fatalf("manifest after switching back = %+v", u.manifest)
	}
}

func TestChannelSwitchForgetsTheOtherChannelsManifest(t *testing.T) {
	disableControlSignature(t)
	u := directFlavorUpdater(t, releases.FlavorTeam, flavorManifest(releases.FlavorTeam, "3.0.0"), nil)
	u.manifestURLOverride = true
	u.channelStore = newUpdateChannelStore(t.TempDir())
	ev := channelEvidence{Initial: "team"}
	u.channelEvidence = func() channelEvidence { return ev }
	if st, err := u.Check(context.Background()); err != nil || !st.Available || st.Latest != "3.0.0" {
		t.Fatalf("team: %+v %v", st, err)
	}
	// The account is confirmed non-team, but the (overridden) URL still
	// serves the team manifest: nothing may be installed from it.
	ev.Verdict, ev.VerdictAt = "public", chanT0
	st, err := u.Check(context.Background())
	var fe *manifestFlavorError
	if !errors.As(err, &fe) || st.Available || st.Latest != "" || st.UpdateChannel != "public" {
		t.Fatalf("after the switch: %+v %v", st, err)
	}
	if u.manifest != nil {
		t.Fatalf("the team manifest survived the switch to public: %+v", u.manifest)
	}
	if err := u.Apply(context.Background(), func(int) {}, true); err == nil {
		t.Fatal("apply proceeded with the previous channel's release")
	}
}

func TestMigrationReleaseIsQuietUnlessNewer(t *testing.T) {
	disableControlSignature(t)
	// An install that already runs the migration release (1.0.0) reads it
	// back from the public repo: not newer, so "up to date", not an error.
	u := directFlavorUpdater(t, releases.FlavorPublic, flavorManifest("migration", "1.0.0"), nil)
	st, err := u.Check(context.Background())
	if err != nil || st.LastError != "" || st.Available || st.LastSuccessMs == 0 {
		t.Fatalf("own migration release: %+v %v", st, err)
	}
	// A newer manifest of another flavour is a real failure.
	u = directFlavorUpdater(t, releases.FlavorPublic, flavorManifest("migration", "1.0.1"), nil)
	st, err = u.Check(context.Background())
	var fe *manifestFlavorError
	if !errors.As(err, &fe) || st.Available || st.LastError == "" {
		t.Fatalf("newer foreign release: %+v %v", st, err)
	}
}

func TestInitialChannelStamp(t *testing.T) {
	prev := buildInitialChannelStamp
	t.Cleanup(func() { buildInitialChannelStamp = prev })
	setBuildFlavorStamp(t, "solu-build-flavor=team")
	buildInitialChannelStamp = ""
	if got := initialUpdateChannel(); got != "team" {
		t.Fatalf("team build initial channel = %q", got)
	}
	buildInitialChannelStamp = "public" // the one-time migration build
	if got := initialUpdateChannel(); got != "public" {
		t.Fatalf("migration build initial channel = %q", got)
	}
	setBuildFlavorStamp(t, "")
	buildInitialChannelStamp = "Team"
	if got := initialUpdateChannel(); got != "public" {
		t.Fatalf("mis-stamped initial channel = %q, want public", got)
	}
}

// switchingTransport serves fixed bodies and switches the updater's channel
// the moment the candidate is requested, as a concurrent Check would.
type switchingTransport struct {
	u      *Updater
	target string
	bodies staticRoundTripper
}

func (s *switchingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() == s.target {
		s.u.mu.Lock()
		s.u.channel = releases.FlavorTeam
		s.u.mu.Unlock()
	}
	return s.bodies.RoundTrip(r)
}

func TestApplyRefusesACandidateFromThePreviousChannel(t *testing.T) {
	payload := []byte("public build")
	u, binaryURL := leaseTestUpdater(t, payload)
	u.channel = releases.FlavorPublic
	u.httpClient = &http.Client{Transport: &switchingTransport{u: u, target: binaryURL, bodies: staticRoundTripper{binaryURL: payload}}}
	u.commitSleepFn = func(time.Duration) {}
	u.swapBinaryFn = func(string, string) error {
		t.Fatal("the previous channel's binary reached the swap")
		return nil
	}
	err := u.Apply(context.Background(), func(int) { t.Fatal("exited") }, true)
	if err == nil || !strings.Contains(err.Error(), "update channel changed") {
		t.Fatalf("Apply = %v, want a channel-change refusal", err)
	}
	self, _ := u.executable()
	if _, statErr := os.Stat(self + ".new"); !os.IsNotExist(statErr) {
		t.Fatalf("the refused download was left next to the binary: %v", statErr)
	}
}

func TestApplyShellRefusesAnInstallerFromThePreviousChannel(t *testing.T) {
	payload := []byte("public installer")
	sum := sha256.Sum256(payload)
	url := "https://updates.example.test/setup.exe"
	u := NewUpdater("0.0.932", "0.0.900", "windows-amd64", nil, nil)
	u.homeDir = t.TempDir()
	u.channel = releases.FlavorPublic
	u.manifest = &Manifest{Version: "0.0.932", Flavor: releases.FlavorPublic,
		Shell: &ManifestShell{Version: "0.0.932", Platforms: map[string]ManifestPlatform{
			"windows-amd64": {URL: url, SHA256: hex.EncodeToString(sum[:])},
		}}}
	u.httpClient = &http.Client{Transport: &switchingTransport{u: u, target: url, bodies: staticRoundTripper{url: payload}}}
	u.shellDisplayPath = func(path, _ string) (string, error) { return path, nil }
	u.launchShell = func(string, string, *shellLaunchLog) (launchResult, error) {
		t.Fatal("the previous channel's installer was launched")
		return launchResult{}, nil
	}
	dir := t.TempDir()
	_, err := u.ApplyShell(context.Background(), dir, false)
	if err == nil || !strings.Contains(err.Error(), "update channel changed") {
		t.Fatalf("ApplyShell = %v, want a channel-change refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "shell-update", "setup.exe")); !os.IsNotExist(statErr) {
		t.Fatalf("the refused installer stayed staged: %v", statErr)
	}
}

func TestTeamVerdictOnAPublicBuildKeepsTheCurrentChannel(t *testing.T) {
	// A public build has no team update endpoint (only team builds are
	// stamped with it), so an entitlement verdict cannot move it.
	setTeamUpdatesPath(t, "")
	t.Setenv("SOLU_UPDATE_MANIFEST_URL", "")
	setBuildFlavorStamp(t, "solu-build-flavor=public")
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.channelStore = newUpdateChannelStore(t.TempDir())
	u.channelEvidence = func() channelEvidence {
		return channelEvidence{Initial: "public", Verdict: "team", VerdictAt: chanT0}
	}
	u.syncChannel()
	if u.channel != releases.FlavorPublic || u.manifestURL != releases.LatestManifestURL(releases.Repo) {
		t.Fatalf("stranded: channel %q url %q", u.channel, u.manifestURL)
	}
}

func TestUpdateChannelStoreRetriesAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	s := newUpdateChannelStore(dir)
	fail := true
	s.write = func(path string, data []byte, perm os.FileMode) error {
		if fail {
			return errors.New("disk full")
		}
		return writeFileAtomicSynced(path, data, perm)
	}
	ev := channelEvidence{Initial: "public", Verdict: "team", VerdictAt: chanT0}
	if rec, _ := s.decide(ev, chanT0); rec.Channel != "team" {
		t.Fatalf("decision = %+v", rec)
	}
	fail = false
	s.decide(ev, chanT0.Add(time.Minute)) // nothing new: the write is retried
	if got := newUpdateChannelStore(dir).current(); got == nil || got.Channel != "team" {
		t.Fatalf("after the retry the file holds %+v", got)
	}
}

func TestChannelSwitchWaitsForAnUpdateInProgress(t *testing.T) {
	disableControlSignature(t)
	payload := []byte("public build")
	u, binaryURL := leaseTestUpdater(t, payload)
	u.channel = releases.FlavorPublic
	u.manifestURLOverride = true
	u.manifest.Flavor = releases.FlavorPublic
	u.channelStore = newUpdateChannelStore(t.TempDir())
	ev := channelEvidence{Initial: "public"}
	u.channelEvidence = func() channelEvidence { return ev }
	u.syncChannel()
	// The account turns out to be team-entitled while the public candidate
	// downloads, and a concurrent check runs.
	u.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == binaryURL {
			ev.Verdict, ev.VerdictAt = "team", chanT0
			u.syncChannel()
			if got := u.channelSnapshot(); got != releases.FlavorPublic {
				t.Errorf("channel switched to %q under an update in progress", got)
			}
		}
		return staticRoundTripper{binaryURL: payload}.RoundTrip(r)
	})}
	u.commitSleepFn = func(time.Duration) {}
	swapped := false
	u.swapBinaryFn = func(string, string) error { swapped = true; return errors.New("stop here") }
	_ = u.Apply(context.Background(), func(int) {}, true)
	if !swapped {
		t.Fatal("the update did not reach its swap")
	}
	// Once the update is over, the stored decision takes effect.
	u.syncChannel()
	if got := u.channelSnapshot(); got != releases.FlavorTeam || u.manifest != nil {
		t.Fatalf("after the update: channel %q manifest %+v", got, u.manifest)
	}
	// The same holds while an installer is staged.
	u.mu.Lock()
	u.shellApplying = true
	u.mu.Unlock()
	ev.Verdict, ev.VerdictAt = "public", chanT0.Add(time.Hour)
	u.syncChannel()
	if got := u.channelSnapshot(); got != releases.FlavorTeam {
		t.Fatalf("channel switched to %q while an installer was staged", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// migrationInstall is an install from before flavours that has just started
// the migration release: a team build that starts on the public channel. Its
// entitlements cache answers as the cloud did. Whether it holds saved
// provider accounts is not an input to the channel decision at all (nor to the
// features: entitlements_saved_accounts_private_test.go).
func migrationInstall(t *testing.T, handler http.HandlerFunc) (*teamEntitlements, *updateChannelStore) {
	t.Helper()
	setBuildFlavorStamp(t, "solu-build-flavor=team")
	prev := buildInitialChannelStamp
	buildInitialChannelStamp = "public"
	t.Cleanup(func() { buildInitialChannelStamp = prev })
	t.Setenv("SOLU_UPDATE_CHANNEL", "")
	t.Setenv("SOLU_TEAM_FEATURES", "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	e := newTeamEntitlements(dir, nil)
	e.accountBase = func() string { return srv.URL }
	e.client = srv.Client()
	return e, newUpdateChannelStore(dir)
}

func TestMigrationKeepsTeamOnlyOnAPositiveTeamVerdict(t *testing.T) {
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	for _, tc := range []struct {
		name    string
		answer  http.HandlerFunc
		token   string
		offline bool
		want    string
	}{
		{"never signed in", reply(200, `{"team":true,"tier":"team"}`), "", false, "public"},
		{"offline", nil, "token", true, "public"},
		{"cloud 5xx", reply(503, "down"), "token", false, "public"},
		{"garbage answer", reply(200, "<html>"), "token", false, "public"},
		{"expired session", reply(401, "unauthenticated"), "token", false, "public"},
		{"anonymous session", reply(200, `{"team":false,"tier":"none"}`), "token", false, "public"},
		{"non-team account", reply(200, `{"team":false,"tier":"account"}`), "token", false, "public"},
		{"team account", reply(200, `{"team":true,"tier":"team"}`), "token", false, "team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.answer
			if handler == nil {
				handler = reply(200, `{"team":true,"tier":"team"}`)
			}
			e, store := migrationInstall(t, handler)
			if tc.offline {
				e.accountBase = func() string { return "http://127.0.0.1:1" }
			}
			e.refresh(context.Background(), tc.token)
			rec, _ := store.decide(entitlementChannelEvidence(e), time.Now())
			if rec.Channel != tc.want {
				t.Fatalf("channel = %q (%s), want %s", rec.Channel, rec.Reason, tc.want)
			}
		})
	}
}

func TestMigrationOnPublicSelfHealsOnATeamVerdict(t *testing.T) {
	var team bool
	e, store := migrationInstall(t, func(w http.ResponseWriter, _ *http.Request) {
		if team {
			_, _ = w.Write([]byte(`{"team":true,"tier":"team"}`))
			return
		}
		w.WriteHeader(503)
	})
	e.refresh(context.Background(), "token") // the cloud is down at first start
	if rec, _ := store.decide(entitlementChannelEvidence(e), time.Now()); rec.Channel != "public" {
		t.Fatalf("first start: %q", rec.Channel)
	}
	team = true
	e.refresh(context.Background(), "token") // later, the team account is confirmed
	if rec, _ := store.decide(entitlementChannelEvidence(e), time.Now()); rec.Channel != "team" || rec.Reason != channelReasonAccount {
		t.Fatalf("after the verdict: %+v", rec)
	}
}
