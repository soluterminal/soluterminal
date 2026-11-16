package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayHandshakeIncludesDevicePolicy(t *testing.T) {
	gotQuery := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery <- r.URL.Query()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	liveDevicePolicy = func() string { return remoteAccessApproved }
	defer func() { liveDevicePolicy = nil }()

	relayURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_ = serveRelayClassified(context.Background(), relayURL, "tok", "win", http.NewServeMux(), nil)
	q := <-gotQuery
	if got := q.Get("dp"); got != remoteAccessApproved {
		t.Errorf("handshake dp = %q, want approved", got)
	}
	if got := q.Get("pl"); got == "" || got != devicePlatform() {
		t.Errorf("handshake pl = %q, want %q", got, devicePlatform())
	}

	liveDevicePolicy = nil
	_ = serveRelayClassified(context.Background(), relayURL, "tok", "win", http.NewServeMux(), nil)
	q = <-gotQuery
	if _, ok := q["dp"]; ok {
		t.Error("dp sent without a policy hook")
	}
	if _, ok := q["pl"]; ok {
		t.Error("pl sent without a policy")
	}
}

func TestDevicePlatformIsInTheCloudEnum(t *testing.T) {
	switch p := devicePlatform(); p {
	case "windows", "linux-wsl", "linux", "darwin":
	default:
		t.Fatalf("devicePlatform() = %q, outside the closed enum", p)
	}
}

// A policy change redials the live tunnel (same token, still exactly one
// tunnel), and changes in quick succession coalesce into spaced redials.
func TestRelaySupervisorRedialsOnPolicyChange(t *testing.T) {
	dir := t.TempDir()
	writeClaimToken(t, dir, "tok-A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ft := &fakeTunnel{}
	sup := newRelaySupervisor(ctx, dir, "wss://example.test/relay", "wsl", nil)
	sup.dial = ft.dial
	sup.pollInterval = 10 * time.Millisecond
	sup.policyRedialMin = 300 * time.Millisecond
	sup.policyRedialDelay = 50 * time.Millisecond
	go sup.run()
	eventuallyTrue(t, 2*time.Second, func() bool { return atomic.LoadInt32(&ft.live) == 1 })

	sup.policyChanged()
	eventuallyTrue(t, 2*time.Second, func() bool {
		return atomic.LoadInt32(&ft.starts) == 2 && atomic.LoadInt32(&ft.live) == 1
	})
	first := time.Now()

	// A burst of changes right after: one more redial, not before the minimum
	// spacing, and not one per change.
	for i := 0; i < 5; i++ {
		sup.policyChanged()
	}
	eventuallyTrue(t, 3*time.Second, func() bool { return atomic.LoadInt32(&ft.starts) >= 3 })
	if elapsed := time.Since(first); elapsed < 250*time.Millisecond {
		t.Fatalf("second redial after %s, want spaced by the minimum", elapsed)
	}
	time.Sleep(600 * time.Millisecond)
	if got := atomic.LoadInt32(&ft.starts); got > 4 {
		t.Fatalf("%d dials after a burst of 5 changes, want them coalesced", got)
	}
	if max := atomic.LoadInt32(&ft.maxLive); max > 1 {
		t.Fatalf("DOUBLE-DIAL: max concurrent tunnels = %d", max)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for _, tok := range ft.tokens {
		if tok != "tok-A" {
			t.Fatalf("redial used token %q", tok)
		}
	}
}

// Without a live tunnel there is nothing to redial: the next dial carries the
// new policy anyway.
func TestRelaySupervisorPolicyChangeWhileOffline(t *testing.T) {
	var s *relaySupervisor
	s.policyChanged() // nil-safe
	sup := &relaySupervisor{}
	sup.policyChanged() // no channel: never blocks
}

// A pending, spaced policy redial does not hold up the rest of the loop: a
// token change during the wait is handled at once.
func TestRelaySupervisorPolicyWaitDoesNotBlockTheLoop(t *testing.T) {
	dir := t.TempDir()
	writeClaimToken(t, dir, "tok-A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ft := &fakeTunnel{}
	sup := newRelaySupervisor(ctx, dir, "wss://example.test/relay", "wsl", nil)
	sup.dial = ft.dial
	sup.pollInterval = time.Hour // only signals move the loop
	sup.policyRedialMin = time.Hour
	sup.policyRedialDelay = 50 * time.Millisecond
	go sup.run()
	eventuallyTrue(t, 2*time.Second, func() bool { return atomic.LoadInt32(&ft.live) == 1 })

	sup.policyChanged() // redial after the short delay (none before)
	eventuallyTrue(t, 2*time.Second, func() bool { return atomic.LoadInt32(&ft.starts) == 2 })
	sup.policyChanged() // scheduled an hour out

	if err := UpdateClaimToken(dir, "tok-B"); err != nil {
		t.Fatal(err)
	}
	sup.signalTokenChanged()
	eventuallyTrue(t, 2*time.Second, func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return len(ft.tokens) >= 3 && ft.tokens[len(ft.tokens)-1] == "tok-B"
	})
	if max := atomic.LoadInt32(&ft.maxLive); max > 1 {
		t.Fatalf("DOUBLE-DIAL: max concurrent tunnels = %d", max)
	}
}

// The first redial waits a moment, so the request that changed the policy
// gets its answer; and a redial is skipped when the tunnel was already dialed
// after the change (its handshake carries the new policy).
func TestRelaySupervisorPolicyRedialWaitsAndSkipsCoveredChanges(t *testing.T) {
	dir := t.TempDir()
	writeClaimToken(t, dir, "tok-A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ft := &fakeTunnel{}
	sup := newRelaySupervisor(ctx, dir, "wss://example.test/relay", "wsl", nil)
	sup.dial = ft.dial
	sup.pollInterval = time.Hour
	sup.policyRedialMin = 10 * time.Millisecond
	sup.policyRedialDelay = 400 * time.Millisecond
	go sup.run()
	eventuallyTrue(t, 2*time.Second, func() bool { return atomic.LoadInt32(&ft.live) == 1 })

	sup.policyChanged()
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&ft.starts); got != 1 {
		t.Fatalf("redialed after %d dials within the delay, want none yet", got)
	}
	// A token change redials now; that new handshake already carries the
	// policy, so the scheduled policy redial must not dial again.
	if err := UpdateClaimToken(dir, "tok-B"); err != nil {
		t.Fatal(err)
	}
	sup.signalTokenChanged()
	eventuallyTrue(t, 2*time.Second, func() bool { return atomic.LoadInt32(&ft.starts) == 2 })
	time.Sleep(600 * time.Millisecond)
	if got := atomic.LoadInt32(&ft.starts); got != 2 {
		t.Fatalf("%d dials, want 2: the policy redial was already covered", got)
	}
}
