package helperproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// Scan is still unregistered by default. These DTOs implement only metadata
// validation from helper-protocol-v1 SHA-256 781ccc3c, ruling 8.6b; they do not
// change StreamHeader or grant permission to open a scan stream.
var ErrInvalidScanRecord = errors.New("helperproto: invalid scan record")

// ScanCredentialRecord is the complete v1 credential-status wire allowlist.
// Credential contents, account identifiers and arbitrary errors have no field.
// Presence describes a local file's recognized credential, not provider login
// verification. Codex tokens are opaque; lastErrorClass is always unknown.
type ScanCredentialRecord struct {
	T              string `json:"t"`
	Provider       string `json:"provider"`
	State          string `json:"state"`
	Present        bool   `json:"present"`
	Kind           string `json:"kind"`
	HasRefresh     bool   `json:"hasRefresh"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
	LastErrorClass string `json:"lastErrorClass"`
}

var scanCredentialRequired = []string{"t", "provider", "state", "present", "kind", "hasRefresh", "lastErrorClass"}

func (r ScanCredentialRecord) valid() bool {
	if r.T != "cred" || (r.Provider != "claude" && r.Provider != "codex") || r.LastErrorClass != "unknown" {
		return false
	}
	switch r.State {
	case "present", "missing", "invalid", "unreadable", "too_large", "unsupported":
	default:
		return false
	}
	if r.Present != (r.State == "present") {
		return false
	}
	if !r.Present {
		return r.Kind == "none" && !r.HasRefresh && r.ExpiresAt == ""
	}
	if r.Kind != "oauth" && r.Kind != "api-key" {
		return false
	}
	if r.Kind == "api-key" && r.HasRefresh {
		return false
	}
	if r.ExpiresAt != "" && (r.Provider != "claude" || r.Kind != "oauth" || !validScanTime(r.ExpiresAt)) {
		return false
	}
	return true
}

// EncodeScanCredential returns a JSON line without its final newline. All
// validation errors are fixed text, never the rejected value or input bytes.
func EncodeScanCredential(r ScanCredentialRecord) ([]byte, error) {
	// The writer pads valid short fractional timestamps; the decoder only
	// accepts the pinned nine-digit wire form (owner clarification 14978a13).
	if r.ExpiresAt != "" {
		var err error
		r.ExpiresAt, err = formatScanTime(r.ExpiresAt)
		if err != nil {
			return nil, ErrInvalidScanRecord
		}
	}
	if !r.valid() {
		return nil, ErrInvalidScanRecord
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > MaxLine {
		return nil, ErrInvalidScanRecord
	}
	return b, nil
}

// DecodeScanCredential consumes exactly one record without its newline.
// Missing/null fields, duplicate or case-aliased keys, unknown fields and
// non-UTF-8 input fail closed. It returns no partial result on rejection.
func DecodeScanCredential(line []byte) (ScanCredentialRecord, error) {
	var r ScanCredentialRecord
	fields, err := decodeScanRecord(line, &r, scanCredentialRequired, []string{"expiresAt"})
	if err != nil || !r.valid() {
		return ScanCredentialRecord{}, ErrInvalidScanRecord
	}
	// Omitted in v1 means absent, even an empty or null field is not allowed.
	if _, present := fields["expiresAt"]; present && (r.Provider == "codex" || r.ExpiresAt == "") {
		return ScanCredentialRecord{}, ErrInvalidScanRecord
	}
	return r, nil
}

// decodeScanRecord validates the exact top-level allowlist before decoding.
// encoding/json alone accepts duplicate keys and case-insensitive aliases,
// which is too permissive for this version-matched metadata boundary.
func decodeScanRecord(line []byte, dst any, required, optional []string) (map[string]json.RawMessage, error) {
	if len(line) == 0 || len(line) > MaxLine || !utf8.Valid(line) {
		return nil, ErrInvalidScanRecord
	}
	for _, b := range line {
		if b < 0x20 || b == 0x7f {
			return nil, ErrInvalidScanRecord
		}
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	d := json.NewDecoder(bytes.NewReader(line))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrInvalidScanRecord
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] {
			return nil, ErrInvalidScanRecord
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrInvalidScanRecord
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, ErrInvalidScanRecord
		}
		fields[key] = value
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') {
		return nil, ErrInvalidScanRecord
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalidScanRecord
	}
	for _, key := range required {
		if value, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrInvalidScanRecord
		}
	}
	d = json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil {
		return nil, ErrInvalidScanRecord
	}
	return fields, nil
}
