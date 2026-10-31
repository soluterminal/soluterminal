// Package daemonfeatures names the capabilities a daemon build advertises, so
// the cloud and web can gate on WHAT a daemon supports instead of comparing
// version strings. The daemon sends its list on the relay handshake (`f`
// query param) and in /api/health (`features`); the cloud stores it per
// machine backend.
//
// Rules:
//   - Name a feature only when it is added. Behaviour every live daemon already
//     has must never get a name, or the gate would wrongly block those daemons.
//   - A daemon that advertises nothing (older build) lacks every named feature.
//   - Never rename or reuse a name; add ".v2" instead.
package daemonfeatures

import (
	"regexp"
	"sort"
	"strings"
)

const (
	// UIPrefs: this daemon persists sidebar/UI choices across app origins.
	UIPrefs = "ui-prefs.v1"
	// TeamBrokerCodex: the daemon can provision a broker-only Codex team set
	// (no bundle on the wire; model calls go through the cloud broker). Daemons
	// <= 0.0.1153 cannot — the Oct 1 2026 outage.
	TeamBrokerCodex = "team-broker.codex.v1"
	// TeamBrokerClaude: same, for broker-only Claude team sets.
	TeamBrokerClaude = "team-broker.claude.v1"
	// WorkflowOwnerDirect: an opted-in terminal owner can author through the
	// guarded file broker and submit its candidate to mandatory engine gates.
	WorkflowOwnerDirect = "workflow.owner-direct.v1"
)

// MaxFeatures bounds how many names one handshake may carry.
const MaxFeatures = 64

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

// Encode renders a feature list for the handshake query: sorted, de-duplicated,
// invalid names dropped, comma-joined.
func Encode(features []string) string {
	return strings.Join(normalize(features), ",")
}

// Parse reads a handshake value back into a set. Invalid entries are ignored
// and at most MaxFeatures names are kept.
func Parse(raw string) map[string]bool {
	out := map[string]bool{}
	for _, f := range normalize(strings.Split(raw, ",")) {
		out[f] = true
	}
	return out
}

// List returns a set's names sorted (for JSON/DB).
func List(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for f, ok := range set {
		if ok {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func normalize(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, f := range in {
		f = strings.TrimSpace(strings.ToLower(f))
		if f == "" || seen[f] || !validName.MatchString(f) {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	if len(out) > MaxFeatures {
		out = out[:MaxFeatures]
	}
	return out
}
