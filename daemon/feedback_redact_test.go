package main

import (
	"os/user"
	"strings"
	"testing"
	"time"

	"soluterminal-shared/feedbackredact"
	"soluterminal-shared/feedbackredact/redacttest"
)

func TestFeedbackRedactionCorpus(t *testing.T) {
	device := feedbackredact.New(feedbackredact.Identity{})
	redacttest.Check(t, func(s string) string { return redactFeedbackWith(s, device, nil) })
}

func TestFeedbackIdentityDiscovery(t *testing.T) {
	env := map[string]string{"USER": "fake-linux", "USERNAME": "FAKE\\fake-win", "COMPUTERNAME": "fake-pc", "USERPROFILE": `C:\Users\FakeProfile`, "PATH": `/usr/bin:/mnt/c/Users/FakePath/bin`, "APPDATA": `C:\Users\FakeData\AppData\Roaming`}
	id := feedbackLocalIdentity(func(key string) string { return env[key] }, func() (string, error) { return "fake-host", nil }, func() (string, error) { return "/home/fake-home", nil }, func() (*user.User, error) { return &user.User{Username: "fake-current"}, nil }, func() (string, string) { return "fake-interop", "fake-win-pc" })
	r := feedbackredact.New(id)
	for _, name := range []string{"fake-linux", "fake-win", "FakeProfile", "FakePath", "FakeData", "fake-host", "fake-pc", "fake-home", "fake-current", "fake-interop", "fake-win-pc"} {
		got, n := r.Redact("name: " + name)
		if n != 1 || got == "name: "+name {
			t.Errorf("fake identity %q not removed", name)
		}
	}
}

func TestFeedbackKnownCodeExpiry(t *testing.T) {
	var cache feedbackCodeCache
	now := time.Now()
	cache.remember("MNPRST", now.Add(time.Minute))
	base := feedbackredact.New(feedbackredact.Identity{})
	for _, when := range []time.Time{now, now.Add(10 * time.Minute)} {
		got, n := base.WithCodes(cache.recent(when)).Redact("I saw MNPRST in the terminal beside README ACCEPT STATUS")
		if got != "I saw [redacted: code] in the terminal beside README ACCEPT STATUS" || n != 1 {
			t.Fatalf("active/recent fake code: %q count=%d", got, n)
		}
	}
	if got, _ := base.WithCodes(cache.recent(now.Add(11 * time.Minute))).Redact("I saw MNPRST in the terminal beside README ACCEPT STATUS"); got != "I saw MNPRST in the terminal beside README ACCEPT STATUS" {
		t.Fatal("expired hints must be removed")
	}
}

func TestFeedbackKnownDeviceCodeAnyLength(t *testing.T) {
	var cache feedbackCodeCache
	now := time.Now()
	cache.remember("D2", now.Add(time.Minute))
	base := feedbackredact.New(feedbackredact.Identity{})
	input := "The screen showed D2 beside README ACCEPT STATUS"
	got, n := base.WithCodes(cache.recent(now)).Redact(input)
	if got != "The screen showed [redacted: code] beside README ACCEPT STATUS" || n != 1 {
		t.Fatalf("known synthetic device code: %q count=%d", got, n)
	}
	if again, _ := base.WithCodes(cache.recent(now)).Redact(got); again != got {
		t.Fatal("known device code redaction must be idempotent")
	}
}

func TestFeedbackRelayHintsAreBounded(t *testing.T) {
	var cache feedbackCodeCache
	now := time.Now()
	cache.rememberClaim(strings.Repeat("F", 129), 600, now)
	if len(cache.recent(now)) != 0 {
		t.Fatal("oversize relay hint must not be retained")
	}
	cache.rememberClaim("FARMWK", int(^uint(0)>>1), now)
	if len(cache.recent(now.Add(19*time.Minute))) != 1 {
		t.Fatal("clamped hint should retain the legitimate privacy window")
	}
	if len(cache.recent(now.Add(20*time.Minute))) != 0 {
		t.Fatal("relay TTL must not overflow or extend retention")
	}
}
