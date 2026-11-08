package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claim represents a persisted pairing result — the machine's
// credentials for talking to a specific cloud. Written by --claim
// and read on normal boot when no --relay-token is supplied.
type Claim struct {
	RelayURL    string `json:"relayUrl"`
	Token       string `json:"token"`
	MachineID   string `json:"machineId"`
	MachineName string `json:"machineName"`
	ClaimedAt   string `json:"claimedAt"`
}

func claimFilePath(dataDir string) string {
	return filepath.Join(dataDir, "claim.json")
}

// LoadClaim reads ~/.solu/claim.json if it exists. Returns nil if
// absent; errors only on malformed contents.
func LoadClaim(dataDir string) (*Claim, error) {
	b, err := os.ReadFile(claimFilePath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c Claim
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse claim.json: %w", err)
	}
	if c.Token == "" {
		return nil, nil
	}
	return &c, nil
}

// RemoveClaim deletes claim.json, forgetting this machine's cloud connection.
// A missing file is treated as success (idempotent — "disconnect" of an
// already-disconnected machine is fine). After calling this, kick the relay
// supervisor (signalTokenChanged) so it reconciles to the no-creds state and
// tears any live tunnel down.
//
// This is the escape hatch for a machine whose cloud record was deleted
// server-side — the bearer is then permanently rejected (401) so the supervisor
// parks on CredsInvalid forever with no way to clear the stale claim — and for a
// user who simply no longer wants cloud.
func RemoveClaim(dataDir string) error {
	if err := os.Remove(claimFilePath(dataDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// UpdateClaimToken swaps the Token field on the persisted claim.json,
// preserving every other field (RelayURL, MachineID, MachineName,
// ClaimedAt). Used by the token-rotation poller to swap the daemon's
// long-lived credential without losing pairing metadata.
//
// Atomic via SaveClaim's tempfile+rename. Errors out if claim.json
// doesn't exist — rotation only makes sense after a successful claim.
func UpdateClaimToken(dataDir, newToken string) error {
	c, err := LoadClaim(dataDir)
	if err != nil {
		return fmt.Errorf("load claim for token swap: %w", err)
	}
	if c == nil {
		return fmt.Errorf("no claim.json at %s — cannot rotate token", claimFilePath(dataDir))
	}
	c.Token = newToken
	return SaveClaim(dataDir, c)
}

// SaveClaim writes claim.json atomically and durably with 0600 perms (it
// contains the machine token). A power cut must not leave a torn pairing.
func SaveClaim(dataDir string, c *Claim) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(claimFilePath(dataDir), b, 0o600)
}

// claimStartResult is the parsed POST /api/claim/start response: a
// human-facing code, the daemon-only secret used to poll for acceptance,
// and the TTL in seconds.
type claimStartResult struct {
	Code   string `json:"code"`
	Secret string `json:"secret"`
	TTLSec int    `json:"ttlSec"`
}

// claimWaitResult is the parsed GET /api/claim/wait response. Status is one
// of "pending" | "expired" | "claimed"; the rest are populated only on
// "claimed".
type claimWaitResult struct {
	Status    string `json:"status"`
	MachineID string `json:"machineId"`
	Name      string `json:"name"`
	Token     string `json:"token"`
}

// startClaim hits the cloud's POST /api/claim/start (off the relay's HTTP
// base) and returns the code+secret+ttl. Shared by the CLI --claim flow
// (RunClaimFlow) and the server-driven pairing manager so both speak the
// exact same wire contract.
func startClaim(ctx context.Context, relayURL string) (claimStartResult, error) {
	var out claimStartResult
	httpBase, err := relayToHTTPBase(relayURL)
	if err != nil {
		return out, fmt.Errorf("invalid --relay URL: %w", err)
	}
	startResp, err := httpPostJSON(ctx, httpBase+"/api/claim/start", map[string]any{})
	if err != nil {
		return out, fmt.Errorf("claim start: %w", err)
	}
	if err := json.Unmarshal(startResp, &out); err != nil {
		return out, fmt.Errorf("claim start: parse: %w", err)
	}
	return out, nil
}

// pollClaimWait performs ONE GET /api/claim/wait poll for the given secret.
// A network/parse hiccup is reported as (result{}, err) so the caller can
// keep polling; a successful HTTP call returns the parsed status. Shared by
// the CLI and the server pairing manager.
func pollClaimWait(ctx context.Context, relayURL, secret string) (claimWaitResult, error) {
	var out claimWaitResult
	httpBase, err := relayToHTTPBase(relayURL)
	if err != nil {
		return out, fmt.Errorf("invalid --relay URL: %w", err)
	}
	// The secret rides a header, never the URL: once the claim is accepted it
	// fetches the machine token, and the cloud host's request log keeps query
	// strings. Needs a cloud that reads X-Solu-Claim-Secret (cloud/claim.go).
	body, err := httpGetWithHeader(ctx, httpBase+"/api/claim/wait", "X-Solu-Claim-Secret", secret)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	return out, nil
}

// claimFromWait builds the persisted Claim from a "claimed" wait result.
func claimFromWait(relayURL string, w claimWaitResult) *Claim {
	return &Claim{
		RelayURL:    relayURL,
		Token:       w.Token,
		MachineID:   w.MachineID,
		MachineName: w.Name,
		ClaimedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

// RunClaimFlow performs the full pair-this-machine dance and writes
// the result to claim.json. Blocks until the user accepts the code
// on the web side, the code expires, or ctx is cancelled.
func RunClaimFlow(ctx context.Context, relayURL, dataDir string) (*Claim, error) {
	httpBase, err := relayToHTTPBase(relayURL)
	if err != nil {
		return nil, fmt.Errorf("invalid --relay URL: %w", err)
	}

	// Step 1: start a claim.
	start, err := startClaim(ctx, relayURL)
	if err != nil {
		return nil, err
	}

	// Step 2: tell the user what to do.
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "┌────────────────────────────────────────────────────────┐")
	fmt.Fprintln(os.Stderr, "│  Pair this machine                                      │")
	fmt.Fprintln(os.Stderr, "├────────────────────────────────────────────────────────┤")
	fmt.Fprintf(os.Stderr, "│  Code:  %-46s │\n", formatCodeForDisplay(start.Code))
	fmt.Fprintf(os.Stderr, "│  Open:  %-46s │\n", userFacingCloudURL(httpBase)+"/")
	fmt.Fprintf(os.Stderr, "│  Expires in %d minutes.                                │\n", (start.TTLSec+59)/60)
	fmt.Fprintln(os.Stderr, "└────────────────────────────────────────────────────────┘")
	fmt.Fprintln(os.Stderr, "Sign in, enter the code under 'Claim a machine', and give it a name.")
	fmt.Fprintln(os.Stderr, "Waiting…")
	fmt.Fprintln(os.Stderr)

	// Step 3: poll /api/claim/wait.
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-poll.C:
		}

		w, err := pollClaimWait(ctx, relayURL, start.Secret)
		if err != nil {
			// Network hiccups are fine — keep polling until TTL runs out.
			continue
		}
		switch w.Status {
		case "pending":
			continue
		case "expired":
			return nil, fmt.Errorf("code expired before being accepted; run --claim again")
		case "claimed":
			c := claimFromWait(relayURL, w)
			if err := SaveClaim(dataDir, c); err != nil {
				return c, fmt.Errorf("claim succeeded but save failed: %w", err)
			}
			fmt.Fprintf(os.Stderr, "✓ Claimed as %q. Token saved to %s.\n",
				w.Name, claimFilePath(dataDir))
			return c, nil
		}
	}
}

// formatCodeForDisplay: "XYZW42" → "XYZ-W42". Easier to read aloud
// and to type without miscounting.
func formatCodeForDisplay(code string) string {
	if len(code) != 6 {
		return code
	}
	return code[:3] + "-" + code[3:]
}

// userFacingCloudURL is the address shown to a person for the default cloud:
// its custom domain rather than the hosting provider's name. Other relays
// (self-hosted, staging) are shown as they are.
func userFacingCloudURL(httpBase string) string {
	if defaultCloudHTTPBase != "" && publicAppURL != "" && strings.TrimRight(httpBase, "/") == defaultCloudHTTPBase {
		return publicAppURL
	}
	return httpBase
}

// relayToHTTPBase converts a wss:// or ws:// URL like
// "wss://cloud.example.com/relay" into the base HTTPS
// URL "https://cloud.example.com" so we can hit /api/*.
func relayToHTTPBase(relayURL string) (string, error) {
	u, err := url.Parse(relayURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("expected ws:// or wss:// URL, got %q", relayURL)
	}
	scheme := "https"
	switch u.Scheme {
	case "wss":
		scheme = "https"
	case "ws":
		scheme = "http"
	default:
		return "", fmt.Errorf("expected ws:// or wss:// URL, got %q", relayURL)
	}
	return scheme + "://" + u.Host, nil
}

func httpPostJSON(ctx context.Context, url string, body any) ([]byte, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return b, nil
}

func httpGetWithHeader(ctx context.Context, url, name, value string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(name, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return b, nil
}
