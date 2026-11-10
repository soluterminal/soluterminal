package main

import (
	"sync"
	"time"
)

// Human-facing codes only. Nothing is persisted or logged. Retain codes until
// ten minutes after their expiry so a recently copied pairing screen is safe.
// @secure approved contextual + digit-bearing shape rules, with exact local
// hints for all-letter claim codes (security STATE.md b4a59ce, 2026-11-09).
type feedbackCodeCache struct {
	mu    sync.Mutex
	codes map[string]time.Time
}

var feedbackPairingCodes feedbackCodeCache

// The cloud claim contract has a ten-minute lifetime. Bound untrusted relay
// hints before compiling literals, and clamp before converting seconds to a
// Duration so a malformed TTL cannot overflow or retain hints indefinitely.
// Contextual rules remain unrestricted by length; real claim/device codes are
// six/nineteen characters. No secret used to poll a claim is stored here.
func (c *feedbackCodeCache) rememberClaim(code string, ttlSeconds int, now time.Time) {
	if len(code) > 128 {
		return
	}
	ttlSeconds = min(max(ttlSeconds, 0), 600)
	c.remember(code, now.Add(time.Duration(ttlSeconds)*time.Second))
}

func (c *feedbackCodeCache) remember(code string, expires time.Time) {
	if code == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codes == nil {
		c.codes = make(map[string]time.Time)
	}
	until := expires.Add(10 * time.Minute)
	if until.After(c.codes[code]) {
		c.codes[code] = until
	}
	c.prune(time.Now())
}

func (c *feedbackCodeCache) recent(now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	out := make([]string, 0, len(c.codes))
	for code := range c.codes {
		out = append(out, code)
	}
	return out
}

// prune is always called under c.mu.
func (c *feedbackCodeCache) prune(now time.Time) {
	for code, until := range c.codes {
		if !now.Before(until) {
			delete(c.codes, code)
		}
	}
}
