package feedbackredact_test

import (
	"strings"
	"testing"
	"time"

	"soluterminal-shared/feedbackredact"
	"soluterminal-shared/feedbackredact/redacttest"
)

func TestFeedbackRedactionCorpus(t *testing.T) { redacttest.Check(t, feedbackredact.Redact) }

func TestFeedbackIdentity(t *testing.T) {
	r := feedbackredact.New(feedbackredact.Identity{Hostnames: []string{"fake-host", "fake-host.example.invalid"}, Usernames: []string{"fakeuser", "FAKEWIN", "x"}})
	input := "fake-host.example.invalid FAKE-HOST fakeuser FAKEWIN x extra fakeuserland"
	want := "[redacted: host] [redacted: host] [redacted: user] [redacted: user] [redacted: user] extra fakeuserland"
	got, count := r.Redact(input)
	if got != want || count != 5 {
		t.Fatalf("identity: %q, count=%d", got, count)
	}
	if again, n := r.Redact(got); again != got || n != 0 {
		t.Fatalf("identity repeat: %q, count=%d", again, n)
	}
}

func TestFeedbackRedactionCountAndMarkers(t *testing.T) {
	r := feedbackredact.New(feedbackredact.Identity{})
	got, count := r.Redact("[redacted: token] token=fake fake@example.invalid")
	if got != "[redacted: token] token=[redacted: token] [redacted: email]" || count != 2 {
		t.Fatalf("count=%d output=%q", count, got)
	}
	if _, count = r.Redact(got); count != 0 {
		t.Fatalf("second pass adds %d replacements", count)
	}
}

func TestFeedbackAdversarialOneMiB(t *testing.T) {
	// Unterminated key blocks, many prompt-like prefixes, IPv6 candidates and
	// opaque runs exercise the expensive branches with bounded and unbounded runs.
	inputs := map[string]string{
		"unbroken":         strings.Repeat("a", 1<<20),
		"near matches":     strings.Repeat("password: \nAuthorization: \n::xyz.. @ /home/ = ?token=& ", (1<<20)/55+1),
		"unterminated key": "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("x", 1<<20),
		"escaped quotes":   strings.Repeat(`"token":"a\"`, (1<<20)/12+1),
		"backslashes":      `password="` + strings.Repeat(`\`, 1<<20),
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			// Linear time is proven by scaling, not by a wall-clock budget that
			// depends on the runner: 8x the input must cost well under 64x
			// (quadratic). The 10 s ceiling is only a backstop for a blow-up
			// that would also stall the small run.
			small := fastestRedact(input[:1<<17])
			large := fastestRedact(input[:1<<20])
			if large >= 10*time.Second {
				t.Fatalf("1 MiB took %s (ceiling 10s)", large)
			}
			// Below 5 ms the small run is timer and scheduler noise; flooring it
			// lets a fast large run pass while a slow one still has to scale.
			if ratio := float64(large) / float64(max(small, 5*time.Millisecond)); ratio >= 16 {
				t.Fatalf("8x input took %.1fx as long (128 KiB %s, 1 MiB %s; limit 16x)", ratio, small, large)
			}
			got := feedbackredact.Redact(input[:1<<20])
			if feedbackredact.Redact(got) != got {
				t.Fatal("adversarial input is not idempotent")
			}
		})
	}
}

// fastestRedact returns the best of three runs, so a GC pause or a noisy
// neighbour on a shared runner does not decide the ratio.
func fastestRedact(input string) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range 3 {
		start := time.Now()
		feedbackredact.Redact(input)
		best = min(best, time.Since(start))
	}
	return best
}

func FuzzFeedbackIdempotence(f *testing.F) {
	for _, c := range redacttest.Cases(f) {
		f.Add(c.Input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got := feedbackredact.Redact(input)
		if feedbackredact.Redact(got) != got {
			t.Fatal("redaction must be idempotent")
		}
	})
}
