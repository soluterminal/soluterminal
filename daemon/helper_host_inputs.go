package main

import (
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// What a Windows host needs to place the WSL helper (helper-protocol-v1,
// Files; ruling 8.1; @one-solud's M1-5 answer, 20 Nov):
//   - the binary: solud-linux-amd64.gz, laid by the installer beside the
//     canonical solud.exe, gunzipped with a size cap;
//   - its expected sha256: platforms["linux-amd64"].sha256 from the cached
//     signed manifest <LocalAppData>\SoluTerminalHost\manifest\update.json
//     (+ .sig), verified with the pinned key, for this host's version and
//     flavor. The hash is of the RAW binary, so the decompressed bytes are
//     what is checked. No verified entry, no Linux pane: never an unsigned
//     fallback.

const (
	helperSourceName      = "solud-linux-amd64.gz"
	helperManifestPlatKey = "linux-amd64"
	helperManifestMax     = 4 << 20
	helperManifestSigMax  = 4096
)

// helperSourceMaxBytes caps the decompressed helper, so a hostile .gz can't
// balloon (a var for tests).
var helperSourceMaxBytes int64 = 256 << 20

// helperManifestPublicKey is the key the cached manifest must be signed
// with: the compiled-in one (a seam for tests).
var helperManifestPublicKey = updateManifestPublicKey

var errHelperManifestUnverified = errors.New("Linux panes need this release's signed manifest, and it couldn't be verified")

// helperExpectedSHA returns the signed sha256 of solud-linux for this host.
func helperExpectedSHA(manifestDir, hostVersion, flavor string) (string, error) {
	fail := func(why string, args ...any) (string, error) {
		return "", fmt.Errorf("%w: %s", errHelperManifestUnverified, fmt.Sprintf(why, args...))
	}
	pub, err := helperManifestPublicKey()
	if err != nil || pub == nil {
		return fail("this build has no manifest key")
	}
	body, err := readCappedNoFollow(filepath.Join(manifestDir, "update.json"), helperManifestMax)
	if err != nil {
		return fail("read update.json: %v", err)
	}
	rawSig, err := readCappedNoFollow(filepath.Join(manifestDir, "update.json.sig"), helperManifestSigMax)
	if err != nil {
		return fail("read update.json.sig: %v", err)
	}
	sig, err := decodeBase64Flexible(strings.TrimSpace(string(rawSig)))
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, body, sig) {
		return fail("the signature doesn't verify")
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return fail("update.json: %v", err)
	}
	if m.Version != hostVersion {
		return fail("the cached manifest is for %q, this host is %q", m.Version, hostVersion)
	}
	if err := checkManifestFlavor(&m, flavor, hostVersion); err != nil {
		return fail("%v", err)
	}
	sha := strings.ToLower(strings.TrimSpace(m.Platforms[helperManifestPlatKey].SHA256))
	if !sha256HexRe.MatchString(sha) {
		return fail("no %s entry", helperManifestPlatKey)
	}
	return sha, nil
}

// helperGzSource opens the bundled helper for placement. It gunzips into a
// private temp file while hashing, and hands the bytes out only if they
// match wantSHA, so a changed .gz is never placed.
func helperGzSource(gzPath, wantSHA string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		f, err := openNoFollow(gzPath)
		if err != nil {
			return nil, fmt.Errorf("the Linux helper isn't installed beside solud: %w", err)
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(gzPath), err)
		}
		defer zr.Close()
		tmp, err := os.CreateTemp("", "solud-linux-*")
		if err != nil {
			return nil, err
		}
		cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(zr, helperSourceMaxBytes+1))
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("%s: %w", filepath.Base(gzPath), err)
		}
		if n > helperSourceMaxBytes {
			cleanup()
			return nil, fmt.Errorf("%s decompresses past %d bytes", filepath.Base(gzPath), helperSourceMaxBytes)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
			cleanup()
			return nil, errors.New("the bundled Linux helper doesn't match the signed manifest")
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			cleanup()
			return nil, err
		}
		return &removeOnClose{File: tmp}, nil
	}
}

type removeOnClose struct{ *os.File }

func (r *removeOnClose) Close() error {
	err := r.File.Close()
	_ = os.Remove(r.File.Name())
	return err
}

// openNoFollow opens a regular file for reading and refuses a symlink:
// O_NOFOLLOW where the platform has it, and an Lstat check everywhere
// (Windows has no O_NOFOLLOW).
func openNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	return os.OpenFile(path, os.O_RDONLY|noFollowFlag, 0)
}

func readCappedNoFollow(path string, max int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", filepath.Base(path), max)
	}
	return b, nil
}
