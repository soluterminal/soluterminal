package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"soluterminal-shared/feedbackredact"
)

func TestFeedbackGeneratedDeviceCodesAreRemembered(t *testing.T) {
	// Hash-only derivation accepts these obviously synthetic inputs; no real
	// device key, nonce or comparison code is read from a running daemon.
	cases := []struct {
		name     string
		generate func() string
	}{
		{"v1", func() string { return deviceCode("fake-device-public-key", "") }},
		{"v2", func() string { return deviceCode("fake-device-public-key", "fake-daemon-public-key") }},
		{"v3", func() string {
			return deviceCodeV3("fake-device-public-key", "fake-daemon-public-key", "fake-enrolment", "fake-device-nonce", "fake-daemon-nonce")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := tc.generate()
			found := false
			for _, known := range feedbackPairingCodes.recent(time.Now()) {
				if known == code {
					found = true
					break
				}
			}
			if !found {
				t.Fatal("generated synthetic comparison was not recorded for exact local redaction")
			}
		})
	}
}

func TestFeedbackStartClaimRemembersUnlabelledCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/claim/start" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(claimStartResult{Code: "FARMWK", Secret: "obviously-fake-poll-secret", TTLSec: 60})
	}))
	defer server.Close()
	if _, err := startClaim(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http")); err != nil {
		t.Fatal(err)
	}
	redactor := feedbackredact.New(feedbackredact.Identity{}).WithCodes(feedbackPairingCodes.recent(time.Now()))
	got, count := redactor.Redact("The screen showed FARMWK in the middle of a sentence")
	if got != "The screen showed [redacted: code] in the middle of a sentence" || count != 1 {
		t.Fatal("minted synthetic all-letter claim must be removed without a label")
	}
}
