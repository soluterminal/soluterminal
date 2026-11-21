package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
)

func TestHelperCredentialClaudeExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, expiry, want string
	}{
		{"known", `1700000000123`, "2023-11-14T22:13:20.123Z"},
		{"seconds-boundary", `1700000000000`, "2023-11-14T22:13:20Z"},
		{"missing", `null`, ""},
		{"zero", `0`, ""},
		{"negative", `-1`, ""},
		{"last-rfc3339-millisecond", `253402300799999`, "9999-12-31T23:59:59.999Z"},
		{"outside-rfc3339", `253402300800000`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"SECRET_TOKEN","refreshToken":"SECRET_REFRESH","expiresAt":%s},"lastErrorClass":"SECRET_INVENTED_ERROR"}`, tc.expiry)
			got := readHelperCredentialMetadata("claude", strings.NewReader(body))
			if !got.Present || got.ExpiresAt != tc.want || got.LastErrorClass != "unknown" {
				t.Fatalf("wrong status: %+v", got)
			}
			assertHelperCredentialNoSecrets(t, got)
		})
	}
}

func TestHelperCredentialCodexExpiryAlwaysUnknown(t *testing.T) {
	// Synthetic token payload has exp and an identity canary. Neither may be
	// decoded or exposed; even an explicit expiry-shaped extra field is ignored.
	body := `{"tokens":{"access_token":"eyJhbGciOiJub25lIn0.eyJleHAiOjE3MDAwMDAwMDAsInN1YiI6IlNFQ1JFVF9VU0VSIn0.SECRET_SIGNATURE","refresh_token":"SECRET_REFRESH","expiresAt":1700000000000},"expiresAt":1700000000000,"lastErrorClass":"SECRET_ERROR"}`
	got := readHelperCredentialMetadata("codex", strings.NewReader(body))
	if !got.Present || !got.HasRefresh || got.ExpiresAt != "" || got.LastErrorClass != "unknown" {
		t.Fatalf("wrong Codex status: %+v", got)
	}
	assertHelperCredentialNoSecrets(t, got)
}

type helperCredentialErrorReader struct{ err error }

func (r helperCredentialErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestHelperCredentialReadOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		reader      io.Reader
	}{
		{"missing", "missing", helperCredentialErrorReader{fmt.Errorf("SECRET_PATH: %w", fs.ErrNotExist)}},
		{"unreadable", "unreadable", helperCredentialErrorReader{errors.New("SECRET_IO_ERROR")}},
		{"partial-read-error", "unreadable", io.MultiReader(strings.NewReader(`{"tokens":{"access_token":"SECRET_TOKEN"}}`), helperCredentialErrorReader{errors.New("SECRET_IO_ERROR")})},
		{"invalid", "invalid", strings.NewReader(`{"SECRET_MALFORMED":`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readHelperCredentialMetadata("codex", tc.reader)
			if got.State != tc.state || got.Present || got.Kind != "none" || got.HasRefresh || got.ExpiresAt != "" || got.LastErrorClass != "unknown" {
				t.Fatalf("wrong failed-read status: %+v", got)
			}
			assertHelperCredentialNoSecrets(t, got)
		})
	}
}

func TestHelperCredentialReadRemainsBounded(t *testing.T) {
	r := strings.NewReader(strings.Repeat("a", helperCredentialReadLimit+128))
	got := readHelperCredentialMetadata("codex", r)
	if got.State != "too_large" || got.Present || r.Len() != 127 || got.LastErrorClass != "unknown" {
		t.Fatalf("wrong capped-read result: status=%+v unread=%d", got, r.Len())
	}
	assertHelperCredentialNoSecrets(t, got)
}

func assertHelperCredentialNoSecrets(t *testing.T, got helperCredentialMetadata) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil || strings.Contains(string(b), "SECRET_") {
		t.Fatal("credential contents or raw error escaped metadata result")
	}
}
