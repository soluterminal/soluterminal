package helperproto

import (
	"encoding/json"
	"time"
	"unicode"
	"unicode/utf8"
)

// M4 scan bounds, owned by helper-protocol-v1 (781ccc3c; owner docs 13b98270d).
const (
	ScanReadFilesMax = 64
	ScanContextMax   = 4 << 10
	ScanIntegerMax   = int64(1<<53 - 1)
)

// ScanFileRef is a host-held per-file checkpoint. IDs are opaque HMAC/generation
// hex strings, never paths. Context contains no cwd, git, account or user data.
type ScanFileRef struct {
	FileID   string       `json:"fileId"`
	Gen      string       `json:"gen"`
	Offset   int64        `json:"offset"`
	Tail     string       `json:"tail"`
	Skipping bool         `json:"skipping,omitempty"`
	Context  *ScanContext `json:"ctx,omitempty"`
}

// ScanContext is the entire allowed Codex continuation state. Do not embed
// codexFileState: that also carries paths and account/user identities.
type ScanContext struct {
	ThreadID    string `json:"threadId,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	Model       string `json:"model,omitempty"`
	CLIVersion  string `json:"cliVersion,omitempty"`
	ServiceTier string `json:"serviceTier,omitempty"`
	Subagent    bool   `json:"subagent,omitempty"`
	HasRecords  bool   `json:"hasRecords,omitempty"`
	InApp       bool   `json:"inApp,omitempty"`
}

// CheckScanHeader is an outbound preflight, called before HostSession.Open.
// It bounds known fields and the complete serialized header, not just the file
// count: 64 contexts of 4 KiB each cannot fit a 64 KiB header. A caller splits
// its batch until this passes; this function never truncates or drops state.
// Helper-side strict decoding of original request JSON remains a separate
// integration step; ReadHeader's generic additive-field behavior is unchanged.
func CheckScanHeader(h StreamHeader) error {
	bad := func() error { return Errorf(CodeBadArgs, "invalid scan header") }
	if h.DecodeErr != nil || h.S != KindScan || h.Agent != "" || h.Kind != "" || h.Pane != "" || len(h.Attest) != 0 {
		return bad()
	}
	if len(h.Files) > ScanReadFilesMax || h.MaxFiles < 0 || h.MaxFiles > 1000 || h.MaxRecords < 0 || h.MaxRecords > 5000 {
		return bad()
	}
	if h.After != "" && !isLowerHex(h.After, 64) {
		return bad()
	}
	if h.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, h.Since)
		if err != nil || t.UTC().Format("2006-01-02T15:04:05.000000000Z") != h.Since {
			return bad()
		}
	}
	switch h.Op {
	case "status":
		if h.Root != "credential-status" || h.After != "" || h.Since != "" || h.MaxFiles != 0 || h.MaxRecords != 0 || len(h.Files) != 0 {
			return bad()
		}
	case "list":
		if !scanUsageRoot(h.Root) || h.MaxRecords != 0 || len(h.Files) != 0 {
			return bad()
		}
	case "read":
		if !scanUsageRoot(h.Root) || h.After != "" || h.Since != "" || h.MaxFiles != 0 || len(h.Files) == 0 {
			return bad()
		}
	default:
		return bad()
	}
	for i, f := range h.Files {
		if !isLowerHex(f.FileID, 64) || !isLowerHex(f.Gen, 64) || f.Offset < 0 || f.Offset > ScanIntegerMax || (f.Tail != "" && !isLowerHex(f.Tail, 64)) {
			return bad()
		}
		if f.Offset == 0 && f.Tail != "" {
			return bad()
		}
		if i > 0 && h.Files[i-1].FileID >= f.FileID {
			return bad()
		}
		if f.Context != nil {
			if h.Root != "codex-sessions" || !scanContextValid(*f.Context) {
				return bad()
			}
			b, err := json.Marshal(f.Context)
			if err != nil || len(b) > ScanContextMax {
				return bad()
			}
		}
	}
	b, err := json.Marshal(h)
	if err != nil {
		return bad()
	}
	if len(b) > MaxLine {
		return ErrLineTooLong
	}
	return nil
}

func scanUsageRoot(root string) bool {
	return root == "claude-projects" || root == "codex-sessions"
}

func scanContextValid(c ScanContext) bool {
	return scanContextText(c.ThreadID, 512) && scanContextText(c.SessionID, 512) && scanContextText(c.Model, 256) && scanContextText(c.CLIVersion, 64) && scanContextText(c.ServiceTier, 32)
}

func scanContextText(value string, max int) bool {
	if len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}
