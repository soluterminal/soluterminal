package main

// Update channels: which build ("public" or "team") an install follows.
//
// The build flavour (update_flavor.go) is what a binary IS. The channel is
// what an install FOLLOWS, decided per install and kept in
// <dataDir>/update_channel.json. The updater reads only the channel's releases
// repo and accepts only manifests signed for the channel, so an install moves
// between builds exactly when its channel changes, and never otherwise.
//
// The channel follows the account:
//
//   - An install whose account the cloud confirms as team-entitled follows
//     the team channel (a public install moves to the entitled build on sign-in).
//   - An install whose account the cloud confirms as NOT team-entitled follows
//     the public channel (a team install moves to the public build).
//   - Nothing else moves an install: signing out, an expired or anonymous
//     session, no network, or a cloud error leave the channel where it is.
//     Only a fresh, positive cloud answer about a real account (tier "team" or
//     "account") counts; an anonymous answer (tier "none") is not a verdict.
//
// The first decision on an install (no channel file yet):
//
//  1. SOLU_UPDATE_CHANNEL=team|public in the environment (~/.solu/env), an
//     explicit local choice, always wins (later decisions too).
//  2. A cached cloud verdict about the account, as above.
//  3. Otherwise the build's initial channel: its own flavour, so a new public
//     download starts on public and a new team download on team. The one-time
//     migration release (0.0.1225), which every install from before flavours
//     receives, is a team build stamped with initial channel public: such an
//     install stays on team ONLY on a positive team-account verdict, and
//     everything else (a non-team verdict, never signed in, offline, a cloud
//     error, a garbage answer) lands on public. Nothing else on the machine
//     is consulted. An install that wrongly lands on public moves to team on
//     its account's next positive verdict.
//
// A channel switch takes effect with the new channel's next release that is
// newer than the running version; the updater never moves sideways or down.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"soluterminal-shared/releases"
)

const updateChannelFile = "update_channel.json"

// buildInitialChannelStamp overrides the channel a fresh install starts on
// (default: the build flavour). Stamped "public" only on the one-time
// migration build (scripts/release-ldflags.sh SOLU_UPDATE_INITIAL_CHANNEL).
var buildInitialChannelStamp = ""

func initialUpdateChannel() string {
	if strings.TrimSpace(buildInitialChannelStamp) == "" {
		return currentBuildFlavor()
	}
	return releases.NormalizeFlavor(buildInitialChannelStamp)
}

// Reasons recorded with a decision (shown in the daemon log and status).
const (
	channelReasonOverride = "env"
	channelReasonAccount  = "account"
	channelReasonInitial  = "initial"
)

type updateChannelRecord struct {
	Channel string `json:"channel"`
	Reason  string `json:"reason"`
	// VerdictAt is the CheckedAt of the newest cloud verdict this decision
	// has seen. Only a verdict newer than it can move the install again.
	VerdictAt time.Time `json:"verdictAt,omitempty"`
	DecidedAt time.Time `json:"decidedAt"`
}

// channelEvidence is everything a decision may use.
type channelEvidence struct {
	// Override is SOLU_UPDATE_CHANNEL ("" when unset or not team/public).
	Override string
	// Verdict is the channel a cloud answer about the account implies
	// ("team" or "public"), or "" when there is no such answer.
	Verdict   string
	VerdictAt time.Time
	// Initial is the build's initial channel.
	Initial string
}

func updateChannelOverride() string {
	switch strings.TrimSpace(os.Getenv("SOLU_UPDATE_CHANNEL")) {
	case releases.FlavorTeam:
		return releases.FlavorTeam
	case releases.FlavorPublic:
		return releases.FlavorPublic
	}
	return ""
}

// channelVerdict maps a cached cloud answer to the channel it implies. Only
// a real account counts: tier "team" means team, tier "account" (signed in,
// not team-entitled) means public. No answer, or an anonymous one (tier
// "none"), is no verdict.
func channelVerdict(rec entitlementRecord) (string, time.Time) {
	if rec.CheckedAt.IsZero() {
		return "", time.Time{}
	}
	switch rec.normalizedTier() {
	case tierTeam:
		if rec.Team {
			return releases.FlavorTeam, rec.CheckedAt
		}
		return releases.FlavorPublic, rec.CheckedAt
	case tierAccount:
		return releases.FlavorPublic, rec.CheckedAt
	}
	return "", time.Time{}
}

// decideUpdateChannel applies the rules above. prev is nil before the first
// decision. changed reports that the record must be saved (a new channel, or
// a newer verdict that confirmed the current one).
func decideUpdateChannel(prev *updateChannelRecord, ev channelEvidence, now time.Time) (rec updateChannelRecord, changed bool) {
	verdictNewer := ev.Verdict != "" && (prev == nil || ev.VerdictAt.After(prev.VerdictAt))
	switch {
	case ev.Override != "":
		rec = updateChannelRecord{Channel: ev.Override, Reason: channelReasonOverride}
	case prev == nil && ev.Verdict != "":
		rec = updateChannelRecord{Channel: ev.Verdict, Reason: channelReasonAccount}
	case prev == nil:
		rec = updateChannelRecord{Channel: releases.NormalizeFlavor(ev.Initial), Reason: channelReasonInitial}
	case verdictNewer:
		rec = *prev
		if ev.Verdict != prev.Channel {
			rec = updateChannelRecord{Channel: ev.Verdict, Reason: channelReasonAccount}
		}
	default:
		return *prev, false
	}
	rec.VerdictAt = ev.VerdictAt
	if prev != nil && !verdictNewer {
		rec.VerdictAt = prev.VerdictAt
	}
	if prev != nil && rec.Channel == prev.Channel && rec.Reason == prev.Reason {
		rec.DecidedAt = prev.DecidedAt
		return rec, !rec.VerdictAt.Equal(prev.VerdictAt)
	}
	rec.DecidedAt = now
	return rec, true
}

// updateChannelStore persists the decision.
type updateChannelStore struct {
	mu   sync.Mutex
	path string
	rec  *updateChannelRecord
	// unsaved is set when the last decision could not be written; the next
	// decide retries the write even if nothing new was decided.
	unsaved bool
	// write is os-level persistence; tests inject failures.
	write func(path string, data []byte, perm os.FileMode) error
}

func newUpdateChannelStore(dataDir string) *updateChannelStore {
	s := &updateChannelStore{path: filepath.Join(dataDir, updateChannelFile)}
	if b, err := os.ReadFile(s.path); err == nil {
		var rec updateChannelRecord
		if json.Unmarshal(b, &rec) == nil && (rec.Channel == releases.FlavorTeam || rec.Channel == releases.FlavorPublic) {
			s.rec = &rec
		} else {
			log.Printf("update channel: ignoring unreadable %s", s.path)
		}
	}
	return s
}

// current is the stored decision, or nil before the first one.
func (s *updateChannelStore) current() *updateChannelRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec == nil {
		return nil
	}
	rec := *s.rec
	return &rec
}

// decide applies ev and persists a changed decision. A failed write keeps the
// new decision in memory and is retried on the next decision.
func (s *updateChannelStore) decide(ev channelEvidence, now time.Time) (updateChannelRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.rec
	rec, changed := decideUpdateChannel(prev, ev, now)
	if !changed && !s.unsaved {
		return rec, false
	}
	moved := prev == nil || prev.Channel != rec.Channel
	s.rec = &rec
	write := s.write
	if write == nil {
		write = writeFileAtomicSynced
	}
	b, err := json.Marshal(rec)
	if err == nil {
		err = write(s.path, b, 0o600)
	}
	s.unsaved = err != nil
	if err != nil {
		log.Printf("update channel: persist failed (retried on the next check): %v", err)
	}
	if moved {
		log.Printf("update channel: %s (reason %s)", rec.Channel, rec.Reason)
	}
	return rec, moved
}

// channelSnapshot is the channel manifests must be signed for right now.
func (u *Updater) channelSnapshot() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.channel
}

// syncChannel brings u.channel up to date with the stored decision and the
// latest evidence. It runs at the start of every Check, before any network
// I/O. It never switches while Apply or Rollback holds the restart
// reservation (taken before either captures its candidate and held through
// the swap) or while an installer is being staged and launched, so a
// candidate is always installed on the channel that chose it.
//
// Before main wires the evidence, a stored decision is still honoured but
// nothing new is decided, so a first decision always sees the cached account
// verdict.
func (u *Updater) syncChannel() {
	if u.channelStore == nil {
		return
	}
	var rec *updateChannelRecord
	if u.channelEvidence != nil {
		decided, _ := u.channelStore.decide(u.channelEvidence(), time.Now())
		rec = &decided
	} else {
		rec = u.channelStore.current()
	}
	if rec == nil {
		return
	}
	// Read before taking u.mu: the path may come from the entitlements
	// cache, which has its own lock.
	_, teamPath := u.teamUpdatesSource()
	u.mu.Lock()
	defer u.mu.Unlock()
	if rec.Channel == u.channel {
		u.status.UpdateChannel = rec.Channel
		u.channelWaiting = ""
		return
	}
	if u.shellApplying || u.swapped || (u.restartGate != nil && u.restartGate.Load()) {
		// An update, rollback or installer launch holds a candidate from the
		// current channel. Switching now could let it finish installing the
		// previous channel's build, so the switch waits: the decision is
		// already stored and the next check applies it.
		log.Printf("auto-update: switch to the %s channel waits for the update in progress", rec.Channel)
		return
	}
	url, viaCloud := u.manifestURL, false
	if !u.manifestURLOverride {
		var ok bool
		url, viaCloud, ok = channelSource(rec.Channel, teamPath != "")
		if !ok {
			// This build has no source for that channel yet: an entitled
			// public install whose paired cloud has not named its team
			// update endpoint (the first fresh entitlements answer does, a
			// few seconds after pairing). Keep following the current
			// channel rather than stranding the install with nothing to
			// poll; the next check switches once the source exists.
			if u.channelWaiting != rec.Channel {
				u.channelWaiting = rec.Channel
				log.Printf("auto-update: the %s channel is chosen (reason %s); this build waits for the paired cloud to name its %s update source, and keeps following %s until then", rec.Channel, rec.Reason, rec.Channel, u.channel)
			}
			return
		}
	}
	prev := u.channel
	u.channel = rec.Channel
	u.channelWaiting = ""
	u.status.UpdateChannel = rec.Channel
	u.manifestURL = url
	u.teamViaCloud = viaCloud
	// Nothing learned about the previous channel may be applied on the new
	// one: forget its manifest, its verified-pin cache and its control file.
	// The next fetch (this same Check) repopulates them for the new channel.
	u.manifest = nil
	u.verifiedManifest = nil
	u.control = nil
	u.controlFetchedAt = time.Time{}
	u.ctrlEffective = nil
	u.status.Latest = ""
	u.status.LastSuccessMs = 0
	u.status.Notes = ""
	u.status.SourceSha = ""
	u.status.SourceProvenance = ""
	u.status.Available = false
	u.status.Required = false
	u.status.RequiredReason = ""
	if !u.rollbackDrain && u.localDrainCandidateID == "" {
		u.clearWorkflowDrainLocked()
	}
	u.recomputeShellLocked()
	log.Printf("auto-update: now following the %s channel (was %s, reason %s)", rec.Channel, prev, rec.Reason)
}

// entitlementChannelEvidence builds the evidence from the entitlements cache:
// only the cloud's cached answer about the account. The cache's
// existing-install fallback (entitlements.go verdict step 3) is deliberately
// not evidence: it is not a cloud verdict.
func entitlementChannelEvidence(e *teamEntitlements) channelEvidence {
	ev := channelEvidence{Override: updateChannelOverride(), Initial: initialUpdateChannel()}
	if e != nil {
		e.mu.Lock()
		rec := e.record
		e.mu.Unlock()
		ev.Verdict, ev.VerdictAt = channelVerdict(rec)
	}
	return ev
}
