package helperproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func scanCredentialFixture() ScanCredentialRecord {
	return ScanCredentialRecord{T: "cred", Provider: "claude", State: "present", Present: true, Kind: "oauth", HasRefresh: true, ExpiresAt: "2023-11-14T22:13:20.123000000Z", LastErrorClass: "unknown"}
}

func TestScanCredentialRoundTrip(t *testing.T) {
	want := scanCredentialFixture()
	b, err := EncodeScanCredential(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeScanCredential(b)
	if err != nil || got != want {
		t.Fatalf("round trip: got=%+v err=%v", got, err)
	}
	for _, state := range []string{"missing", "invalid", "unreadable", "too_large", "unsupported"} {
		r := ScanCredentialRecord{T: "cred", Provider: "codex", State: state, Kind: "none", LastErrorClass: "unknown"}
		b, err := EncodeScanCredential(r)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DecodeScanCredential(b); err != nil || got != r {
			t.Fatalf("state %s round trip failed", state)
		}
	}
}

func TestScanCredentialRejectsUntrustedRecords(t *testing.T) {
	base, _ := json.Marshal(scanCredentialFixture())
	for name, mutate := range map[string]func(string) string{
		"credential-content": func(s string) string { return strings.TrimSuffix(s, "}") + `,"accessToken":"SECRET_TOKEN"}` },
		"account":            func(s string) string { return strings.TrimSuffix(s, "}") + `,"account":"SECRET_ACCOUNT"}` },
		"duplicate-key":      func(s string) string { return strings.TrimSuffix(s, "}") + `,"provider":"codex"}` },
		"case-alias":         func(s string) string { return strings.Replace(s, `"provider":`, `"Provider":`, 1) },
		"null-presence":      func(s string) string { return strings.Replace(s, `"present":true`, `"present":null`, 1) },
		"missing-presence":   func(s string) string { return strings.Replace(s, `"present":true,`, ``, 1) },
		"wrong-presence":     func(s string) string { return strings.Replace(s, `"present":true`, `"present":false`, 1) },
		"unobserved-error":   func(s string) string { return strings.Replace(s, `"unknown"`, `"SECRET_PROVIDER_ERROR"`, 1) },
		"bad-time": func(s string) string {
			return strings.Replace(s, `2023-11-14T22:13:20.123000000Z`, `SECRET_BAD_TIME`, 1)
		},
		"comma-time":     func(s string) string { return strings.Replace(s, `20.123000000Z`, `20,123000000Z`, 1) },
		"short-fraction": func(s string) string { return strings.Replace(s, `20.123000000Z`, `20.123Z`, 1) },
		"no-fraction":    func(s string) string { return strings.Replace(s, `20.123000000Z`, `20Z`, 1) },
		"one-digit-hour": func(s string) string { return strings.Replace(s, `T22:`, `T2:`, 1) },
		"non-utc-time": func(s string) string {
			return strings.Replace(s, `2023-11-14T22:13:20.123000000Z`, `2023-11-14T22:13:20+08:00`, 1)
		},
		"raw-control":  func(s string) string { return "\t" + s },
		"invalid-utf8": func(s string) string { return s + string([]byte{0xff}) },
		"second-value": func(s string) string { return s + `{}` },
		"array":        func(s string) string { return "[" + s + "]" },
		"truncated":    func(s string) string { return strings.TrimSuffix(s, "}") },
	} {
		t.Run(name, func(t *testing.T) {
			r, err := DecodeScanCredential([]byte(mutate(string(base))))
			if !errors.Is(err, ErrInvalidScanRecord) || r != (ScanCredentialRecord{}) || strings.Contains(err.Error(), "SECRET_") {
				t.Fatal("record was accepted, partially returned, or echoed")
			}
		})
	}
	if _, err := DecodeScanCredential(bytes.Repeat([]byte("x"), MaxLine+1)); !errors.Is(err, ErrInvalidScanRecord) {
		t.Fatal("oversized record accepted")
	}
}

func TestScanCredentialCodexOmitsExpiry(t *testing.T) {
	r := ScanCredentialRecord{T: "cred", Provider: "codex", State: "present", Present: true, Kind: "api-key", LastErrorClass: "unknown"}
	b, err := EncodeScanCredential(r)
	if err != nil || bytes.Contains(b, []byte("expiresAt")) {
		t.Fatal("Codex expiry was sent")
	}
	for _, expiry := range []string{`""`, `null`, `"2023-11-14T22:13:20Z"`} {
		line := strings.TrimSuffix(string(b), "}") + `,"expiresAt":` + expiry + `}`
		if _, err := DecodeScanCredential([]byte(line)); !errors.Is(err, ErrInvalidScanRecord) {
			t.Fatal("Codex expiry field accepted")
		}
	}
}

func TestScanCredentialEncoderRejectsContradictions(t *testing.T) {
	for _, mutate := range []func(*ScanCredentialRecord){
		func(r *ScanCredentialRecord) { r.State = "SECRET_STATE" },
		func(r *ScanCredentialRecord) { r.Kind = "SECRET_KIND" },
		func(r *ScanCredentialRecord) { r.Provider = "SECRET_PROVIDER" },
		func(r *ScanCredentialRecord) { r.Present = false },
		func(r *ScanCredentialRecord) { r.State = "invalid"; r.Present = false },
		func(r *ScanCredentialRecord) { r.Kind = "api-key" },
		func(r *ScanCredentialRecord) { r.Kind = "api-key"; r.HasRefresh = false },
		func(r *ScanCredentialRecord) { r.ExpiresAt = "SECRET_TIME" },
	} {
		r := scanCredentialFixture()
		mutate(&r)
		b, err := EncodeScanCredential(r)
		if !errors.Is(err, ErrInvalidScanRecord) || b != nil || strings.Contains(err.Error(), "SECRET_") {
			t.Fatal("invalid outgoing metadata accepted or echoed")
		}
	}
}

func TestScanCredentialWriterPadsWireTime(t *testing.T) {
	for _, tc := range []struct{ input, wire string }{
		{"2023-11-14T22:13:20Z", "2023-11-14T22:13:20.000000000Z"},
		{"2023-11-14T22:13:20.000Z", "2023-11-14T22:13:20.000000000Z"},
		{"2023-11-14T22:13:20.1230Z", "2023-11-14T22:13:20.123000000Z"},
		{"2023-11-14T22:13:20.123456789Z", "2023-11-14T22:13:20.123456789Z"},
	} {
		r := scanCredentialFixture()
		r.ExpiresAt = tc.input
		b, err := EncodeScanCredential(r)
		if err != nil {
			t.Fatal("valid writer timestamp rejected")
		}
		want := r
		want.ExpiresAt = tc.wire
		if got, err := DecodeScanCredential(b); err != nil || got != want {
			t.Fatal("wire expiry was not padded to nine digits")
		}
	}
}
