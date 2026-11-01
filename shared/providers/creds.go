package providers

// CredFacts describes the credential families exposed by a provider. These
// records contain identifiers and compatibility facts only; consumers own all
// validation, persistence and authorization policy.
type CredFacts struct {
	Mailbox   *MailboxFacts
	TeamState *TeamStateFacts
	Store     *StoreFacts
}

// MailboxFacts contains the optional remote delivery contract. Table and route
// identifiers are trusted compiled-in values, never request input.
type MailboxFacts struct {
	RouteSegment     string
	SetTable         string
	SyncTable        string
	PolicyTable      string
	ActiveTable      string
	PolicyKind       string
	EnvelopeKey      string
	AuthKinds        []string
	Validation       string
	MaxBodyBytes     int64
	MaxEnvelopeBytes int64
	HasStatusRoute   bool
	// RestrictAccountLabels applies the filename-safe label contract used by
	// providers whose account label becomes part of a local path. It is
	// independent of envelope validation so a future opaque-envelope provider
	// does not silently inherit another provider's label policy.
	RestrictAccountLabels bool
	ReservedAccountLabel  string
}

// TeamStateFacts contains the compatibility contract for daemon provision
// state. StateFileName is intentionally an existing on-disk name.
type TeamStateFacts struct {
	StateFileName string
	// ClockSkewHeuristic preserves the legacy one-time migration rule for a
	// valid, untracked snapshot: a cloud mint clearly newer than the local
	// file wins after the daemon's one-hour cross-machine clock-skew margin.
	ClockSkewHeuristic bool
	// SnapshotRotatesLocally says the staged snapshot may legitimately diverge
	// from its assigned bundle through local token rotation. When false, an
	// untracked byte difference means the assigned copy must be restored.
	SnapshotRotatesLocally bool
}

// StoreFacts names the provider's existing local paths. Empty values mean the
// provider uses its historical provider-specific layout (Claude snapshots).
type StoreFacts struct {
	UsesAuthLockFile bool
	LiveFileName     string
	RootDirName      string
	ManagedFileName  string
	SourcesDirName   string
	TeamDirName      string
}
