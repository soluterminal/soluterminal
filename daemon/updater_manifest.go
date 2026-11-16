package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
)

// transientErr marks an update-check failure that is expected to
// self-heal on a later poll — GitHub's release-assets CDN intermittently
// returns 5xx, plain network blips (DNS, timeouts, refused) come and go,
// and a 404 on an immutable /releases/download/v<version>/ asset means a
// publish is still in flight (tag resolved from the latest alias before
// every asset finished uploading, or the alias caches are briefly ahead
// of the tag). The updater swallows these from the UI badge until they
// recur maxTransientFailures times in a row, so a one-off hiccup doesn't
// alarm the user with a problem that isn't theirs. Genuine failures —
// 404 on a direct/custom manifest URL (manifest genuinely missing),
// unparseable JSON, and bad signatures — are NOT transient and surface
// immediately. A signature failure is non-transient EVERYWHERE: verified
// bytes only ever come from an immutable per-tag pair (or a direct custom
// URL), which cannot be a publish race, so a mismatch is tampering or
// misconfiguration, never something to retry past.
type transientErr struct{ err error }

func (e *transientErr) Error() string { return e.err.Error() }
func (e *transientErr) Unwrap() error { return e.err }

// transient wraps err so isTransientUpdateErr reports true for it.
func transient(err error) error { return &transientErr{err: err} }

// isTransientUpdateErr reports whether err (anywhere in its chain) was
// marked transient via transient().
func isTransientUpdateErr(err error) bool {
	var t *transientErr
	return errors.As(err, &t)
}

// retryableStatus reports whether an HTTP status from GitHub's release
// CDN is worth retrying silently: 5xx (server/CDN trouble) and 429
// (rate limit). A 4xx like 404 means the asset is genuinely absent, so
// it surfaces straight away.
func retryableStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests
}

// releaseVersionRe validates a version string parsed from the UNVERIFIED
// latest-alias bytes before it is spliced into a pinned download URL. The
// resolve body is not signature-checked (it is only a version hint), so
// nothing outside a plain dotted version — no slashes, no "..", nothing
// URL-significant — may ever reach URL space from it.
var releaseVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.-]+)?$`)

func validReleaseVersion(v string) bool { return releaseVersionRe.MatchString(v) }

// fetchManifest retrieves and (when a key is configured) signature-verifies
// the update manifest.
//
// GitHub serves /releases/latest/download/<asset> through PER-ASSET redirect
// caches: right after a publish, update.json can still redirect to the
// previous release while update.json.sig already redirects to the new one
// (observed publishing v0.0.987 — the correctly-rejected mixed pair surfaced
// as `manifest signature verification failed` and delayed activation). So
// for latest-alias URLs the mutable alias is used ONLY to resolve the
// current version; the bytes that get verified and trusted are always
// fetched from that release's immutable /releases/download/v<version>/
// URLs, which cannot disagree about which release they belong to.
// Custom/direct manifest URLs (SOLU_UPDATE_MANIFEST_URL pointing at a local
// server, tests) keep the original single-fetch path.
func (u *Updater) fetchManifest(ctx context.Context) (*Manifest, error) {
	if u.teamViaCloudNow() {
		// The team channel through the cloud (team_updates.go). Without a
		// paired cloud there is nowhere to ask; either way the install stays
		// on its version and never falls back to the public repo.
		if u.manifestURLNow() == "" {
			return nil, u.teamUpdatesUnavailable()
		}
		m, err := u.fetchManifestResolveThenPin(ctx)
		return m, teamUpdatesError(err)
	}
	if strings.Contains(u.manifestURLNow(), "/releases/latest/download/") {
		return u.fetchManifestResolveThenPin(ctx)
	}
	return u.fetchManifestDirect(ctx)
}

// fetchManifestDirect is the single-fetch path for custom/direct manifest
// URLs: one GET, signature check against the URL's own .sig, parse.
func (u *Updater) fetchManifestDirect(ctx context.Context) (*Manifest, error) {
	manifestURL := u.manifestURLNow()
	body, err := u.fetchUpdateAsset(ctx, manifestURL, false)
	if err != nil {
		return nil, err
	}
	if err := u.verifySignatureAt(ctx, detachedSigURL(manifestURL), body); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version == "" || len(m.Platforms) == 0 {
		return nil, errors.New("manifest missing version or platforms")
	}
	if err := checkManifestFlavor(&m, u.channelSnapshot(), u.current); err != nil {
		return nil, err
	}
	return &m, nil
}

// fetchManifestResolveThenPin is the latest-alias path: resolve the current
// version from the mutable alias, then fetch + verify the manifest
// exclusively from the release's immutable per-tag URLs.
func (u *Updater) fetchManifestResolveThenPin(ctx context.Context) (*Manifest, error) {
	// Step 1: resolve the version. These bytes are NOT signature-verified
	// and are never trusted for anything beyond the version string, which
	// is strictly validated before it touches a URL.
	resolveBody, err := u.fetchUpdateAsset(ctx, u.manifestURLNow(), false)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(resolveBody, &probe); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if probe.Version == "" {
		return nil, errors.New("manifest missing version")
	}
	if !validReleaseVersion(probe.Version) {
		return nil, fmt.Errorf("manifest version %q is not a valid release version", probe.Version)
	}

	// Steady state: the immutable pair for this version was fetched and
	// Ed25519-verified on an earlier poll — reuse it, one GET per poll.
	// Safe because verifiedManifest is only ever written after successful
	// verification below. This leans on the repo convention that a version
	// tag's assets are NEVER clobbered in place (publish.sh's tag guard
	// forces a version bump for every change); a repaired-in-place
	// v<X>/update.json would be invisible to daemons whose cache already
	// holds X until they restart. Repair by shipping a new version instead.
	u.mu.Lock()
	if u.verifiedManifest != nil && u.verifiedManifest.Version == probe.Version {
		m := u.verifiedManifest
		u.mu.Unlock()
		return m, nil
	}
	u.mu.Unlock()

	// Step 2: fetch + verify from the immutable per-tag URLs. Derivation
	// mirrors controlURL() and publish.sh's DOWNLOAD_BASE
	// (…/releases/download/v<version>). A 404 here is the mid-publish
	// window (or latest-vs-tag cache skew) — transient, self-heals within
	// a poll. A bad signature on an immutable pair can never be a race and
	// stays a hard, fail-closed error.
	m, err := u.fetchPinnedManifest(ctx, probe.Version)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.verifiedManifest = m
	u.mu.Unlock()
	return m, nil
}

// fetchPinnedManifest retrieves one immutable release manifest and verifies
// its detached Ed25519 signature before parsing it. It deliberately does not
// populate verifiedManifest: that cache belongs to the mutable latest-alias
// resolver, and a user-selected rollback must never poison it.
func (u *Updater) fetchPinnedManifest(ctx context.Context, version string) (*Manifest, error) {
	if !validReleaseVersion(version) {
		return nil, fmt.Errorf("manifest version %q is not a valid release version", version)
	}
	manifestURL := u.manifestURLNow()
	i := strings.Index(manifestURL, "/releases/")
	if i < 0 {
		return nil, errors.New("rollback requires a release manifest URL")
	}
	base := manifestURL[:i] + "/releases/download/v" + version + "/"
	body, err := u.fetchUpdateAsset(ctx, base+"update.json", true)
	if err != nil {
		return nil, err
	}
	if err := u.verifySignatureAtOpts(ctx, base+"update.json.sig", body, true); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version == "" || len(m.Platforms) == 0 {
		return nil, errors.New("manifest missing version or platforms")
	}
	if m.Version != version {
		return nil, fmt.Errorf("pinned manifest version %q does not match requested version %q", m.Version, version)
	}
	// Both the latest-alias path and a user-selected rollback land here, so
	// neither can install the other flavour's build, nor a release published
	// before flavours (a full-tree build).
	if err := checkManifestFlavor(&m, u.channelSnapshot(), u.current); err != nil {
		return nil, err
	}
	return &m, nil
}

// fetchUpdateAsset GETs url with the shared 1MB bound and the updater's
// standard error classification: network errors and 5xx/429 are transient,
// other non-200s are not — except that with notFound404Transient set a 404
// is transient too (immutable per-tag assets, where a 404 means the publish
// is still in flight rather than "genuinely absent").
func (u *Updater) fetchUpdateAsset(ctx context.Context, assetURL string, notFound404Transient bool) ([]byte, error) {
	resp, err := getReleaseAsset(ctx, u.httpClient, assetURL)
	if err != nil {
		// Network-level failure (DNS, timeout, connection refused) —
		// treat as transient; the next poll usually recovers.
		return nil, transient(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		mErr := &updateHTTPError{label: updateAssetLabel(assetURL), code: resp.StatusCode}
		if retryableStatus(resp.StatusCode) || (notFound404Transient && resp.StatusCode == http.StatusNotFound) {
			return nil, transient(mErr)
		}
		return nil, mErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// manifestURLNow is the current channel's manifest URL. syncChannel can
// change it, so fetches read it under the lock. On the team channel through
// the cloud it is derived from the paired cloud's origin and that cloud's
// endpoint path ("" while either is missing), so pairing or a new cloud
// takes effect on the next check.
func (u *Updater) manifestURLNow() string {
	u.mu.Lock()
	url, viaCloud := u.manifestURL, u.teamViaCloud
	u.mu.Unlock()
	if !viaCloud {
		return url
	}
	return teamUpdatesManifestURL(u.teamUpdatesSource())
}

// teamViaCloudNow reports that the current channel is the team channel read
// through the cloud.
func (u *Updater) teamViaCloudNow() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.teamViaCloud
}

// noUpdateSource reports that this build has nothing to poll for its
// channel (a team channel in a build without the team endpoint).
func (u *Updater) noUpdateSource() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return !u.teamViaCloud && u.manifestURL == ""
}

// detachedSigURL returns the URL of the detached signature for raw: ".sig"
// appended to the PATH component, preserving any query string — naive
// concatenation would corrupt "update.json?token=1" into
// "update.json?token=1.sig". RawPath is extended in lockstep so a
// percent-encoded path (url.Parse decodes Path but keeps RawPath) is not
// re-normalized into a different path than the body was fetched from.
// Falls back to raw+".sig" when raw doesn't parse.
func detachedSigURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw + ".sig"
	}
	parsed.Path += ".sig"
	if parsed.RawPath != "" {
		parsed.RawPath += ".sig"
	}
	return parsed.String()
}

// verifySignatureAt fetches a detached Ed25519 signature from sigURL and
// verifies it over body using the configured manifest public key. Shared by the
// manifest and the update control file so both ride one verified-signature path.
//
// Fail-closed default: in release builds (requireSignedManifest stamped via
// ldflags) a missing/blank public key is a hard error, not a silent degrade to
// checksum-only. Dev builds (`go run`, tests) leave the flag unset and stay
// permissive so unsigned local manifests keep working.
func (u *Updater) verifySignatureAt(ctx context.Context, sigURL string, body []byte) error {
	return u.verifySignatureAtOpts(ctx, sigURL, body, false)
}

// verifySignatureAtOpts is verifySignatureAt with one extra knob:
// notFound404Transient marks a 404 on the signature asset as transient —
// used only for immutable per-tag URLs, where a missing .sig means the
// publish is still uploading assets, not that signing was skipped. The
// verification outcome itself is never affected: a fetched-but-invalid
// signature is a hard error on every path.
func (u *Updater) verifySignatureAtOpts(ctx context.Context, sigURL string, body []byte, notFound404Transient bool) error {
	pub, err := updateManifestPublicKey()
	if err != nil {
		return err
	}
	if pub == nil {
		if mustVerifySignatures() {
			return errors.New("signed manifest required but no public key compiled in")
		}
		return nil
	}
	resp, err := getReleaseAsset(ctx, u.httpClient, sigURL)
	if err != nil {
		return transient(fmt.Errorf("manifest signature fetch: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		sErr := fmt.Errorf("manifest signature %s: HTTP %d", updateAssetLabel(sigURL), resp.StatusCode)
		if retryableStatus(resp.StatusCode) || (notFound404Transient && resp.StatusCode == http.StatusNotFound) {
			return transient(sErr)
		}
		return sErr
	}
	rawSig, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return err
	}
	sig, err := decodeBase64Flexible(strings.TrimSpace(string(rawSig)))
	if err != nil {
		return fmt.Errorf("manifest signature decode: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("manifest signature length %d, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, body, sig) {
		return errors.New("manifest signature verification failed")
	}
	return nil
}

// updateManifestPublicKey is the key update.json must be signed with: the one
// compiled in. Only a dev build (version "dev") takes SOLU_UPDATE_MANIFEST_PUBKEY
// in its place; a release build never lets its environment choose the key.
func updateManifestPublicKey() (ed25519.PublicKey, error) {
	key := ""
	if version == "dev" {
		key = strings.TrimSpace(os.Getenv("SOLU_UPDATE_MANIFEST_PUBKEY"))
	}
	if key == "" {
		key = strings.TrimSpace(defaultUpdateManifestPubKey)
	}
	if key == "" {
		return nil, nil
	}
	raw, err := decodeBase64Flexible(key)
	if err != nil {
		return nil, fmt.Errorf("manifest public key decode: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("manifest public key length %d, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

func decodeBase64Flexible(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func (u *Updater) platformKey(m *Manifest) string {
	key := platformKeyCurrent()
	if _, ok := m.Platforms[key]; ok {
		return key
	}
	return ""
}

// platformKeyCurrent returns the key used in manifest["platforms"][...]
// for the current runtime — e.g. "windows-amd64", "linux-amd64",
// "darwin-arm64". Keep this in sync with publish.sh's artifact naming.
func platformKeyCurrent() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}
