package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	previewBundleManifestName = "manifest.json"
	previewSealMarkerName     = ".soluterminal-seal.json"
	previewBundleMaxBytes     = int64(500 << 20)
	previewCacheMaxBytes      = int64(500 << 20)
)

type previewBundleManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Digest        string    `json:"digest"`
	Size          int64     `json:"size"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
}

type previewTreeFile struct {
	rel  string
	abs  string
	size int64
}

var previewBundlePins = struct {
	sync.Mutex
	refs map[string]int
}{refs: map[string]int{}}

var previewSealPins = struct {
	sync.Mutex
	refs map[string]bool
}{refs: map[string]bool{}}

type previewSealMarker struct {
	IdentityUnavailable bool      `json:"identityUnavailable,omitempty"`
	SchemaVersion       int       `json:"schemaVersion"`
	OwnershipID         string    `json:"ownershipId"`
	CanonicalPath       string    `json:"canonicalPath"`
	CreatedAt           time.Time `json:"createdAt"`
	Device              uint64    `json:"device"`
	Inode               uint64    `json:"inode"`
}

func previewCreateOwnedSeal(cacheRoot string) (string, previewSealMarker, func(), error) {
	return previewCreateOwnedSealIdentity(cacheRoot, generatedIdentity)
}

func previewCreateOwnedSealIdentity(cacheRoot string, identify func(string) (generatedFSIdentity, error)) (string, previewSealMarker, func(), error) {
	tmp, err := os.MkdirTemp(cacheRoot, ".seal-")
	if err != nil {
		return "", previewSealMarker{}, nil, err
	}
	identity, err := generatedCreationIdentity(tmp, identify)
	if err != nil {
		_ = os.Remove(tmp)
		return "", previewSealMarker{}, nil, err
	}
	id, err := generatedRandomID()
	if err != nil {
		_ = generatedRemoveTree(context.Background(), tmp, identity)
		return "", previewSealMarker{}, nil, err
	}
	abs, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		_ = generatedRemoveTree(context.Background(), tmp, identity)
		return "", previewSealMarker{}, nil, err
	}
	marker := previewSealMarker{SchemaVersion: 1, OwnershipID: id, CanonicalPath: filepath.Clean(abs), CreatedAt: time.Now().UTC(), Device: identity.Device, Inode: identity.Inode}
	marker.IdentityUnavailable = !identity.Valid
	raw, _ := json.MarshalIndent(marker, "", "  ")
	if err := writeFileAtomicSynced(filepath.Join(tmp, previewSealMarkerName), append(raw, '\n'), 0o600); err != nil {
		_ = generatedRemoveTree(context.Background(), tmp, identity)
		return "", previewSealMarker{}, nil, err
	}
	previewSealPins.Lock()
	previewSealPins.refs[tmp] = true
	previewSealPins.Unlock()
	return tmp, marker, func() {
		previewSealPins.Lock()
		delete(previewSealPins.refs, tmp)
		previewSealPins.Unlock()
	}, nil
}

func previewReadSealMarker(path string) (previewSealMarker, error) {
	var marker previewSealMarker
	raw, err := generatedReadMarker(path, previewSealMarkerName)
	if err != nil {
		return marker, err
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return marker, err
	}
	if marker.IdentityUnavailable {
		return marker, errWFGeneratedUnsupported
	}
	abs, canonicalErr := filepath.EvalSymlinks(path)
	identity, identityErr := generatedIdentity(path)
	if marker.SchemaVersion != 1 || marker.OwnershipID == "" || canonicalErr != nil || filepath.Clean(abs) != marker.CanonicalPath || identityErr != nil || identity.Device != marker.Device || identity.Inode != marker.Inode {
		return marker, errors.New("ownership_marker_mismatch")
	}
	return marker, nil
}

func previewRemoveSeal(ctx context.Context, path string, expected previewSealMarker) error {
	identity := generatedFSIdentity{Device: expected.Device, Inode: expected.Inode, Valid: !expected.IdentityUnavailable}
	return generatedRemoveMarkedTree(ctx, path, identity, previewSealMarkerName, func(raw []byte) error {
		var current previewSealMarker
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if current != expected {
			return errors.New("ownership_marker_mismatch")
		}
		return nil
	})
}

func previewRemoveInactiveSeal(ctx context.Context, path string, marker previewSealMarker) error {
	ctx, release, guardErr := retirementCleanupContext(ctx, nil, "", path)
	if guardErr != nil {
		return guardErr
	}
	defer release()
	if err := previewLockContext(ctx, &previewSealPins.Mutex); err != nil {
		return err
	}
	defer previewSealPins.Unlock()
	if previewSealPins.refs[path] {
		return errors.New("active_seal")
	}
	return previewRemoveSeal(ctx, path, marker)
}

func previewSealActive(path string) bool {
	active, _ := previewSealActiveContext(context.Background(), path)
	return active
}

func previewSealActiveContext(ctx context.Context, path string) (bool, error) {
	if err := previewLockContext(ctx, &previewSealPins.Mutex); err != nil {
		return false, err
	}
	defer previewSealPins.Unlock()
	return previewSealPins.refs[path], ctx.Err()
}

func previewPinBundle(dir string) func() {
	dir = filepath.Clean(dir)
	previewBundlePins.Lock()
	previewBundlePins.refs[dir]++
	previewBundlePins.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			previewBundlePins.Lock()
			previewBundlePins.refs[dir]--
			if previewBundlePins.refs[dir] <= 0 {
				delete(previewBundlePins.refs, dir)
			}
			previewBundlePins.Unlock()
		})
	}
}

func previewInFlightBundlePins(cacheRoot string) map[string]bool {
	pinned, _ := previewInFlightBundlePinsContext(context.Background(), cacheRoot)
	return pinned
}

func previewInFlightBundlePinsContext(ctx context.Context, cacheRoot string) (map[string]bool, error) {
	pinned := map[string]bool{}
	if err := previewLockContext(ctx, &previewBundlePins.Mutex); err != nil {
		return nil, err
	}
	defer previewBundlePins.Unlock()
	for dir, refs := range previewBundlePins.refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if refs > 0 && previewPathWithin(cacheRoot, dir) {
			pinned[dir] = true
		}
	}
	return pinned, ctx.Err()
}

// Maintenance must not wait past its inherited deadline on a preview lock.
// Keep the work on the owning goroutine; cancellation never detaches a sweep.
func previewLockContext(ctx context.Context, mu *sync.Mutex) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func previewPathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func previewResolveBeneath(root, candidate string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolvedCandidate) != filepath.Clean(resolvedRoot) && !previewPathWithin(resolvedRoot, resolvedCandidate) {
		return "", fmt.Errorf("path escapes preview root")
	}
	return resolvedCandidate, nil
}

func previewTreeFiles(root string, ignoreManifest bool) ([]previewTreeFile, int64, error) {
	return previewTreeFilesFiltered(root, ignoreManifest, nil)
}

func previewTreeFilesFiltered(root string, ignoreManifest bool, ignore func(string, os.DirEntry) bool) ([]previewTreeFile, int64, error) {
	return previewTreeFilesFilteredContext(context.Background(), root, ignoreManifest, ignore)
}

func previewTreeFilesContext(ctx context.Context, root string, ignoreManifest bool) ([]previewTreeFile, int64, error) {
	return previewTreeFilesFilteredContext(ctx, root, ignoreManifest, nil)
}

func previewTreeFilesFilteredContext(ctx context.Context, root string, ignoreManifest bool, ignore func(string, os.DirEntry) bool) ([]previewTreeFile, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil || !info.IsDir() {
		return nil, 0, fmt.Errorf("preview output is not a directory")
	}
	var files []previewTreeFile
	var total int64
	err = filepath.WalkDir(resolvedRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == resolvedRoot {
			return nil
		}
		rel, err := filepath.Rel(resolvedRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if ignore != nil && ignore(rel, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if ignoreManifest && rel == previewBundleManifestName {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		resolved := path
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, err = filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolve symlink %s: %w", rel, err)
			}
			if !previewPathWithin(resolvedRoot, resolved) {
				return fmt.Errorf("symlink %s escapes preview output", rel)
			}
		}
		fileInfo, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("preview output contains non-regular entry %s", rel)
		}
		total += fileInfo.Size()
		files = append(files, previewTreeFile{rel: rel, abs: resolved, size: fileInfo.Size()})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return files, total, nil
}

func previewDigestFiles(files []previewTreeFile, maxBytes int64) (string, int64, error) {
	h := sha256.New()
	var total int64
	for _, file := range files {
		total += file.size
		if maxBytes > 0 && total > maxBytes {
			return "", total, fmt.Errorf("preview output exceeds %d bytes", maxBytes)
		}
		_, _ = io.WriteString(h, file.rel)
		_, _ = h.Write([]byte{0})
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(file.size))
		_, _ = h.Write(size[:])
		f, err := os.Open(file.abs)
		if err != nil {
			return "", total, err
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", total, copyErr
		}
		if closeErr != nil {
			return "", total, closeErr
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), total, nil
}

func previewTreeDigest(dir string) (string, error) {
	files, _, err := previewTreeFiles(dir, false)
	if err != nil {
		return "", err
	}
	// Spawn outputs are governed by the same bounded-output budget as sealed
	// static bundles. Dependency trees are outside recipe.BuildDir and are not
	// part of this digest.
	digest, _, err := previewDigestFiles(files, previewBundleMaxBytes)
	return digest, err
}

func previewBuildDigest(dir string) (string, error) {
	var ignore func(string, os.DirEntry) bool
	if filepath.Base(filepath.Clean(dir)) == ".next" {
		// next start legitimately writes tracing and cache data beside its
		// immutable production output. Neither is served build output, and pnpm
		// cache symlinks must never poison the accepted digest walk.
		ignore = func(rel string, _ os.DirEntry) bool {
			return rel == "cache" || strings.HasPrefix(rel, "cache/") || rel == "trace" || strings.HasPrefix(rel, "trace.")
		}
	}
	files, _, err := previewTreeFilesFiltered(dir, false, ignore)
	if err != nil {
		return "", err
	}
	digest, _, err := previewDigestFiles(files, previewBundleMaxBytes)
	return digest, err
}

func previewBundleDigest(dir string) (string, error) {
	// A crash can occur after the seal directory is atomically renamed but
	// before its ownership marker is removed. The marker is control metadata,
	// not served output, so ignore it while verifying the accepted digest. A
	// later successful open removes it opportunistically.
	files, _, err := previewTreeFilesFiltered(dir, true, func(rel string, _ os.DirEntry) bool {
		return rel == previewSealMarkerName
	})
	if err != nil {
		return "", err
	}
	digest, _, err := previewDigestFiles(files, previewBundleMaxBytes)
	return digest, err
}

func previewVerifyBundle(dir, digest string) error {
	if !previewDigestRE.MatchString(digest) {
		return errors.New("invalid accepted preview digest")
	}
	actual, err := previewBundleDigest(dir)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(actual), []byte(digest)) != 1 {
		return fmt.Errorf("sealed preview digest mismatch: accepted %s, found %s", digest, actual)
	}
	return nil
}

func previewCacheBytes(cacheRoot string) (int64, error) {
	return previewCacheBytesContext(context.Background(), cacheRoot)
}

func previewCacheBytesContext(ctx context.Context, cacheRoot string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(cacheRoot)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !entry.IsDir() || !previewDigestRE.MatchString("sha256:"+entry.Name()) {
			continue
		}
		files, size, walkErr := previewTreeFilesContext(ctx, filepath.Join(cacheRoot, entry.Name()), false)
		if walkErr != nil {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			continue
		}
		_ = files
		total += size
	}
	return total, ctx.Err()
}

func previewSealBundle(srcDir, cacheRoot string) (string, string, error) {
	return previewSealBundleWithCreator(srcDir, cacheRoot, previewCreateOwnedSeal)
}

func previewSealBundleWithCreator(srcDir, cacheRoot string, createSeal func(string) (string, previewSealMarker, func(), error)) (string, string, error) {
	return previewSealBundleWithCreatorContext(context.Background(), srcDir, cacheRoot, createSeal)
}

func previewSealBundleWithCreatorContext(ctx context.Context, srcDir, cacheRoot string, createSeal func(string) (string, previewSealMarker, func(), error)) (string, string, error) {
	files, _, err := previewTreeFiles(srcDir, false)
	if err != nil {
		return "", "", err
	}
	for _, file := range files {
		if file.rel == previewBundleManifestName {
			return "", "", fmt.Errorf("preview output reserves %s", previewBundleManifestName)
		}
	}
	digest, size, err := previewDigestFiles(files, previewBundleMaxBytes)
	if err != nil {
		return "", "", err
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", "", err
	}
	dest := filepath.Join(cacheRoot, hexDigest)
	if info, statErr := os.Stat(dest); statErr == nil && info.IsDir() {
		if err := previewVerifyBundle(dest, digest); err != nil {
			return "", "", err
		}
		if _, release, err := retirementCleanupContext(ctx, nil, "", dest); err == nil {
			_ = os.Remove(filepath.Join(dest, previewSealMarkerName))
			_ = previewTouchBundle(dest)
			release()
		}
		return digest, dest, nil
	}
	// Make room before refusing the seal. The durable registry is the cache's
	// pin authority, so an active/idle static preview can never be evicted while
	// old unreferenced accepted bundles are reclaimed least-recently-used.
	reserve := size + 4<<10 // manifest plus atomic-write bookkeeping
	limit := previewCacheMaxBytes - reserve
	if limit < 0 {
		return "", "", fmt.Errorf("preview cache limit (%d bytes) would be exceeded", previewCacheMaxBytes)
	}
	if err := previewCacheSweepToLimitContext(ctx, cacheRoot, previewPinnedBundlesFromRegistry(cacheRoot), limit); err != nil {
		return "", "", err
	}
	used, err := previewCacheBytes(cacheRoot)
	if err != nil {
		return "", "", err
	}
	if used+size > previewCacheMaxBytes {
		return "", "", fmt.Errorf("preview cache limit (%d bytes) would be exceeded", previewCacheMaxBytes)
	}
	tmp, creationMarker, releaseSeal, err := createSeal(cacheRoot)
	if err != nil {
		return "", "", err
	}
	defer releaseSeal()
	committed := false
	defer func() {
		if !committed {
			// Never adopt a marker replaced after this producer created the
			// seal: cleanup is authorized only by our original ownership proof.
			_ = previewRemoveSeal(ctx, tmp, creationMarker)
		}
	}()
	for _, file := range files {
		outPath := filepath.Join(tmp, filepath.FromSlash(file.rel))
		if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
			return "", "", err
		}
		in, err := os.Open(file.abs)
		if err != nil {
			return "", "", err
		}
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = in.Close()
			return "", "", err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		closeOutErr := out.Close()
		closeInErr := in.Close()
		if copyErr != nil || syncErr != nil || closeOutErr != nil || closeInErr != nil {
			return "", "", errors.Join(copyErr, syncErr, closeOutErr, closeInErr)
		}
	}
	now := time.Now().UTC()
	manifest, _ := json.MarshalIndent(previewBundleManifest{SchemaVersion: 1, Digest: digest, Size: size, CreatedAt: now, LastUsedAt: now}, "", "  ")
	if err := writeFileAtomicSynced(filepath.Join(tmp, previewBundleManifestName), manifest, 0o600); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if info, statErr := os.Stat(dest); statErr == nil && info.IsDir() && previewVerifyBundle(dest, digest) == nil {
			return digest, dest, nil
		}
		return "", "", err
	}
	committed = true
	// Keep the marker through the atomic rename. Before this point every
	// interrupted seal remains ownership-proven and restart-recoverable.
	_, releaseMarker, markerGuardErr := retirementCleanupContext(ctx, nil, "", dest)
	if markerGuardErr != nil {
		return digest, dest, nil
	}
	defer releaseMarker()
	if err := os.Remove(filepath.Join(dest, previewSealMarkerName)); err != nil {
		// The accepted bundle remains valid because verification excludes this
		// control file; the next use removes it opportunistically.
		log.Printf("preview cache: remove committed seal marker %s: %v", dest, err)
	}
	if dir, err := os.Open(cacheRoot); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return digest, dest, nil
}

func previewTouchBundle(dir string) error {
	path := filepath.Join(dir, previewBundleManifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var manifest previewBundleManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	manifest.LastUsedAt = time.Now().UTC()
	raw, _ = json.MarshalIndent(manifest, "", "  ")
	return writeFileAtomicSynced(path, raw, 0o600)
}

func previewPinnedBundlesFromRegistry(cacheRoot string) map[string]bool {
	pinned, _ := previewPinnedBundlesFromRegistryContext(context.Background(), cacheRoot)
	return pinned
}

func previewPinnedBundlesFromRegistryContext(ctx context.Context, cacheRoot string) (map[string]bool, error) {
	pinned, err := previewInFlightBundlePinsContext(ctx, cacheRoot)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(cacheRoot), "previews.json"))
	if err != nil {
		return pinned, ctx.Err()
	}
	var targets []*PreviewTarget
	if json.Unmarshal(raw, &targets) != nil {
		return pinned, ctx.Err()
	}
	now := time.Now().UTC()
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if target == nil || target.Mode != previewModeStatic || target.State == previewStateExpired || !target.ExpiresAt.After(now) || target.BundleDir == "" {
			continue
		}
		bundleDir := filepath.Clean(target.BundleDir)
		if filepath.IsAbs(bundleDir) && previewPathWithin(cacheRoot, bundleDir) {
			pinned[bundleDir] = true
		}
	}
	return pinned, ctx.Err()
}

func (m *previewManager) pinnedBundlesContext(ctx context.Context) (map[string]bool, error) {
	if err := previewLockContext(ctx, &m.mu); err != nil {
		return nil, err
	}
	defer m.mu.Unlock()
	return m.pinnedBundlesLockedContext(ctx)
}

func (m *previewManager) pinnedBundlesLockedContext(ctx context.Context) (map[string]bool, error) {
	pinned := map[string]bool{}
	now := m.now().UTC()
	for _, target := range m.targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if target != nil && target.BundleDir != "" && target.ExpiresAt.After(now) && target.State != previewStateExpired {
			pinned[filepath.Clean(target.BundleDir)] = true
		}
	}
	return pinned, ctx.Err()
}

// Context-aware maintenance counterpart to the legacy manager prune method.
// Hold the same target lock through inventory/removal to preserve live pins.
func (m *previewManager) pruneCacheContext(ctx context.Context, capBytes int64) (int64, error) {
	if m == nil || capBytes < 0 {
		return 0, ctx.Err()
	}
	ctx, release, guardErr := retirementCleanupContext(ctx, m.retirement, "", "")
	if guardErr != nil {
		return 0, guardErr
	}
	defer release()
	if err := previewLockContext(ctx, &m.mu); err != nil {
		return 0, err
	}
	defer m.mu.Unlock()
	pinned, err := m.pinnedBundlesLockedContext(ctx)
	if err != nil {
		return 0, err
	}
	before, err := previewCacheBytesContext(ctx, m.cacheRoot)
	if err != nil {
		return 0, err
	}
	if err := previewCacheSweepToLimitContext(ctx, m.cacheRoot, pinned, capBytes); err != nil {
		return 0, err
	}
	after, err := previewCacheBytesContext(ctx, m.cacheRoot)
	if err != nil {
		return 0, err
	}
	return max(int64(0), before-after), nil
}

func previewCacheSweep(cacheRoot string, pinned map[string]bool) error {
	return previewCacheSweepToLimit(cacheRoot, pinned, previewCacheMaxBytes)
}

func previewCacheSweepToLimit(cacheRoot string, pinned map[string]bool, limit int64) error {
	return previewCacheSweepToLimitContext(context.Background(), cacheRoot, pinned, limit)
}

func previewCacheSweepToLimitContext(ctx context.Context, cacheRoot string, pinned map[string]bool, limit int64) error {
	ctx, release, guardErr := retirementCleanupContext(ctx, nil, "", "")
	if guardErr != nil {
		return guardErr
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	if pinned == nil {
		pinned = map[string]bool{}
	}
	inFlight, err := previewInFlightBundlePinsContext(ctx, cacheRoot)
	if err != nil {
		return err
	}
	for dir := range inFlight {
		pinned[dir] = true
	}
	entries, err := os.ReadDir(cacheRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		path     string
		size     int64
		used     time.Time
		identity generatedFSIdentity
	}
	var total int64
	var candidates []candidate
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".seal-") {
			path := filepath.Join(cacheRoot, entry.Name())
			marker, markerErr := previewReadSealMarker(path)
			active, err := previewSealActiveContext(ctx, path)
			if err != nil {
				return err
			}
			if markerErr != nil || active || time.Since(marker.CreatedAt) < taskTmpGrace {
				continue
			}
			_ = previewRemoveInactiveSeal(ctx, path, marker)
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		if !entry.IsDir() || !previewDigestRE.MatchString("sha256:"+entry.Name()) {
			continue
		}
		path := filepath.Join(cacheRoot, entry.Name())
		if token, ok := ctx.Value(retirementCleanupContextKey{}).(*retirementCleanupToken); ok && token.check("", path) != nil {
			continue
		}
		identity, identityErr := generatedIdentity(path)
		if identityErr != nil || !identity.Valid {
			continue
		}
		if _, _, complete, _ := generatedMeasure(ctx, path, identity); !complete {
			// Accepted preview bundles are engine-owned, but a symlink, mount
			// boundary, or inspection failure makes this candidate immutable.
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		_, size, walkErr := previewTreeFilesContext(ctx, path, false)
		if walkErr != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		total += size
		if pinned != nil && pinned[path] {
			continue
		}
		used := time.Time{}
		if info, infoErr := entry.Info(); infoErr == nil {
			used = info.ModTime()
		}
		if raw, readErr := os.ReadFile(filepath.Join(path, previewBundleManifestName)); readErr == nil {
			var manifest previewBundleManifest
			if json.Unmarshal(raw, &manifest) == nil {
				used = manifest.LastUsedAt
			}
		}
		candidates = append(candidates, candidate{path: path, size: size, used: used, identity: identity})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used.Before(candidates[j].used) })
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if total <= limit {
			break
		}
		if !previewPathWithin(cacheRoot, candidate.path) {
			continue
		}
		if err := generatedRemoveTree(ctx, candidate.path, candidate.identity); err != nil {
			return err
		}
		total -= candidate.size
	}
	return ctx.Err()
}

type previewStaticServer struct {
	mu        sync.Mutex
	root      string
	spa       bool
	integrity map[string]previewStaticFileIntegrity
	listener  net.Listener
	server    *http.Server
}

type previewStaticFileIntegrity struct {
	size   int64
	digest [sha256.Size]byte
}

func previewStaticIntegrity(root, acceptedDigest string) (map[string]previewStaticFileIntegrity, error) {
	if err := previewVerifyBundle(root, acceptedDigest); err != nil {
		return nil, err
	}
	files, _, err := previewTreeFiles(root, true)
	if err != nil {
		return nil, err
	}
	integrity := make(map[string]previewStaticFileIntegrity, len(files))
	for _, item := range files {
		file, err := os.Open(item.abs)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		integrity[item.rel] = previewStaticFileIntegrity{size: item.size, digest: digest}
	}
	return integrity, nil
}

func newPreviewStaticServer(root string, spaFallback bool, expectedDigests ...string) (*previewStaticServer, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	acceptedDigest := ""
	if len(expectedDigests) > 0 {
		acceptedDigest = expectedDigests[0]
	} else {
		raw, readErr := os.ReadFile(filepath.Join(resolved, previewBundleManifestName))
		if readErr != nil {
			return nil, readErr
		}
		var manifest previewBundleManifest
		if json.Unmarshal(raw, &manifest) != nil {
			return nil, errors.New("invalid sealed preview manifest")
		}
		acceptedDigest = manifest.Digest
	}
	integrity, err := previewStaticIntegrity(resolved, acceptedDigest)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	static := &previewStaticServer{root: resolved, spa: spaFallback, integrity: integrity, listener: listener}
	server := &http.Server{Handler: http.HandlerFunc(static.serveHTTP), ReadHeaderTimeout: 10 * time.Second}
	static.server = server
	// Serve on the local, not static.server: Close() nils the field under s.mu,
	// and this goroutine reads it without the lock.
	go func() { _ = server.Serve(listener) }()
	return static, nil
}

func (s *previewStaticServer) Port() uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return 0
	}
	return uint16(s.listener.Addr().(*net.TCPAddr).Port)
}

func previewHasDotSegment(path string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.HasPrefix(segment, ".") && segment != "" {
			return true
		}
	}
	return false
}

// previewIsBackendPath reports whether a mounted request targets a daemon API
// route rather than a file the static bundle could ever contain.
func previewIsBackendPath(cleanURL string) bool {
	if cleanURL == "/api" || strings.HasPrefix(cleanURL, "/api/") {
		return true
	}
	// A basePath-mounted preview is served under /preview/<id>/, so its API
	// calls arrive as /preview/<id>/api/... . Treat any /api segment as a
	// backend path; a real bundle file at that path still wins in serveHTTP.
	if i := strings.Index(cleanURL, "/api/"); i >= 0 {
		return true
	}
	return strings.HasSuffix(cleanURL, "/api")
}

// previewWriteBackendUnavailable answers an API request with the same honest
// JSON 404 the in-page preview fetch shim returns, so both boundaries describe
// a mounted preview the same way.
func previewWriteBackendUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, `{"error":"preview backend unavailable"}`+"\n")
}

func (s *previewStaticServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	cleanURL := filepath.ToSlash(filepath.Clean("/" + r.URL.Path))
	// Missing API paths stay unavailable. App acceptance that needs a backend
	// uses its own isolated daemon and home, never the host's running daemon.
	// A bundle may legitimately ship files under /api/, so existence wins.
	backendPath := previewIsBackendPath(cleanURL)
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !backendPath {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(cleanURL, "/")
	spaCandidate := !backendPath && filepath.Ext(strings.TrimSuffix(rel, "/")) == ""
	if rel == "" || strings.HasSuffix(r.URL.Path, "/") {
		rel = filepath.ToSlash(filepath.Join(rel, "index.html"))
	}
	if rel == previewBundleManifestName || previewHasDotSegment(rel) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.root, filepath.FromSlash(rel))
	if !previewPathWithin(s.root, path) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		if s.spa && spaCandidate {
			path = filepath.Join(s.root, "index.html")
			info, err = os.Stat(path)
		}
	}
	if err != nil || !info.Mode().IsRegular() {
		// A sealed preview bundle may omit webdist.json because the preview
		// build does not run the provenance emitter, yet the shell still probes
		// it. Serve the daemon's embedded provenance instead of a 404 that a
		// mounted acceptance run records as a failed request.
		if rel == "webdist.json" {
			provenance := CurrentWebProvenance()
			if provenance.WebVersion == "" {
				provenance.WebVersion = version
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			body, _ := json.Marshal(provenance)
			_, _ = w.Write(append(body, '\n'))
			return
		}
		if backendPath {
			previewWriteBackendUnavailable(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	if s.integrity != nil {
		finalRel, relErr := filepath.Rel(s.root, path)
		expected, ok := s.integrity[filepath.ToSlash(finalRel)]
		if relErr != nil || !ok || info.Size() != expected.size {
			http.Error(w, "sealed preview integrity check failed", http.StatusInternalServerError)
			return
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil || subtle.ConstantTimeCompare(hash.Sum(nil), expected.digest[:]) != 1 {
			http.Error(w, "sealed preview integrity check failed", http.StatusInternalServerError)
			return
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "sealed preview integrity check failed", http.StatusInternalServerError)
			return
		}
	}
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
}

func (s *previewStaticServer) Close() error {
	s.mu.Lock()
	server := s.server
	listener := s.listener
	s.server = nil
	s.listener = nil
	s.mu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	}
	if listener != nil {
		return listener.Close()
	}
	return nil
}
