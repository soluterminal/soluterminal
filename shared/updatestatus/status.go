// Package updatestatus defines the deliberately non-secret fleet wire summary.
package updatestatus

import "regexp"

type Status struct {
	AutoUpdate     bool   `json:"autoUpdate"`
	DisabledReason string `json:"disabledReason,omitempty"`
	Channel        string `json:"channel"`
	LastCheckMs    int64  `json:"lastCheckMs,omitempty"`
	LastCheckError string `json:"lastCheckError,omitempty"`
	// LastSuccessMs means a successful successor boot, NOT a manifest check.
	LastSuccessMs int64  `json:"lastSuccessMs,omitempty"`
	Reason        string `json:"reason,omitempty"`
	LatestVersion string `json:"latestVersion,omitempty"`
	// RunningFlavor is the build this binary is; UpdateChannel is the build this
	// install follows. They differ between an install's channel switch and that
	// channel's next installable release. Channel above is the release ring.
	RunningFlavor string `json:"runningFlavor,omitempty"`
	UpdateChannel string `json:"updateChannel,omitempty"`
}

var version = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+){1,3}([-+][a-zA-Z0-9.-]+)?$`)

func Version(v string) string {
	if len(v) <= 64 && version.MatchString(v) {
		return v
	}
	return ""
}

func oneOf(s string, values ...string) string {
	for _, v := range values {
		if s == v {
			return s
		}
	}
	return ""
}

// Sanitize uses allowlists, not best-effort removal of tokens from free text.
// Unknown future fields are ignored by JSON decoding on older clouds.
func (s Status) Sanitize() Status {
	s.DisabledReason = oneOf(s.DisabledReason, "dev-build", "environment", "preference", "signature-key-invalid", "signature-key-missing", "no-releases-repo")
	s.Channel = oneOf(s.Channel, "stable", "preview")
	s.LastCheckError = oneOf(s.LastCheckError, "network", "signature", "manifest", "control", "check-failed")
	s.Reason = oneOf(s.Reason, "kill-switch", "quarantined", "floor", "rollout-cohort", "version-hold", "drain", "restart-pending", "busy", "applying")
	s.LatestVersion = Version(s.LatestVersion)
	s.RunningFlavor = oneOf(s.RunningFlavor, "public", "team")
	s.UpdateChannel = oneOf(s.UpdateChannel, "public", "team")
	if s.LastCheckMs < 0 {
		s.LastCheckMs = 0
	}
	if s.LastSuccessMs < 0 {
		s.LastSuccessMs = 0
	}
	return s
}
