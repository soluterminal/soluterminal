package main

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// withHelperManifestKey signs fixture manifests with a fresh test key.
func withHelperManifestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := helperManifestPublicKey
	helperManifestPublicKey = func() (ed25519.PublicKey, error) { return pub, nil }
	t.Cleanup(func() { helperManifestPublicKey = old })
	return priv
}

func writeHelperManifest(t *testing.T, dir string, priv ed25519.PrivateKey, m Manifest) {
	t.Helper()
	body, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "update.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body))
	if err := os.WriteFile(filepath.Join(dir, "update.json.sig"), []byte(sig+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHelperExpectedSHA(t *testing.T) {
	priv := withHelperManifestKey(t)
	sha := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	good := Manifest{Version: "0.0.1250", Flavor: currentBuildFlavor(), Platforms: map[string]ManifestPlatform{
		"linux-amd64": {SHA256: sha}, "windows-amd64": {SHA256: hex.EncodeToString(make([]byte, 32))},
	}}
	dir := t.TempDir()
	writeHelperManifest(t, dir, priv, good)
	if got, err := helperExpectedSHA(dir, "0.0.1250", currentBuildFlavor()); err != nil || got != sha {
		t.Fatalf("expected sha = %q, %v", got, err)
	}
	refused := func(name string, mutate func(dir string)) {
		t.Helper()
		d := t.TempDir()
		writeHelperManifest(t, d, priv, good)
		mutate(d)
		if _, err := helperExpectedSHA(d, "0.0.1250", currentBuildFlavor()); !errors.Is(err, errHelperManifestUnverified) {
			t.Errorf("%s: %v", name, err)
		}
	}
	refused("tampered body", func(d string) {
		b, _ := os.ReadFile(filepath.Join(d, "update.json"))
		_ = os.WriteFile(filepath.Join(d, "update.json"), bytes.Replace(b, []byte("ab"), []byte("cd"), 1), 0o600)
	})
	refused("missing signature", func(d string) { _ = os.Remove(filepath.Join(d, "update.json.sig")) })
	refused("other version", func(d string) {
		m := good
		m.Version = "0.0.1249"
		writeHelperManifest(t, d, priv, m)
	})
	refused("other flavor", func(d string) {
		m := good
		m.Flavor = "team"
		writeHelperManifest(t, d, priv, m)
	})
	refused("no linux entry", func(d string) {
		m := good
		m.Platforms = map[string]ManifestPlatform{"windows-amd64": {SHA256: sha}}
		writeHelperManifest(t, d, priv, m)
	})
	// A symlinked manifest is refused on every platform (Windows has no
	// O_NOFOLLOW: openNoFollow checks with Lstat).
	real := filepath.Join(t.TempDir(), "update.json")
	b, _ := os.ReadFile(filepath.Join(dir, "update.json"))
	if err := os.WriteFile(real, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(t.TempDir(), "probe")); err != nil {
		t.Logf("symlinks can't be made here (%v); the symlink case is skipped", err)
	} else {
		refused("symlinked manifest", func(d string) {
			if err := os.Remove(filepath.Join(d, "update.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(d, "update.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A build with no compiled-in key never trusts an unsigned manifest.
	helperManifestPublicKey = func() (ed25519.PublicKey, error) { return nil, nil }
	if _, err := helperExpectedSHA(dir, "0.0.1250", currentBuildFlavor()); !errors.Is(err, errHelperManifestUnverified) {
		t.Fatalf("no key: %v", err)
	}
}

func gzFile(t *testing.T, payload []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	path := filepath.Join(t.TempDir(), helperSourceName)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHelperGzSource(t *testing.T) {
	payload := []byte("\x7fELF fake solud-linux")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	path := gzFile(t, payload)

	rc, err := helperGzSource(path, want)()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	name := rc.(*removeOnClose).Name()
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the decompressed temp file outlived Close")
	}
	if _, err := helperGzSource(path, hex.EncodeToString(make([]byte, 32)))(); err == nil {
		t.Fatal("bytes that don't match the manifest were handed out")
	}
	if _, err := helperGzSource(filepath.Join(t.TempDir(), "missing.gz"), want)(); err == nil {
		t.Fatal("a missing bundle was accepted")
	}
	// A bundle that decompresses past the cap is refused.
	old := helperSourceMaxBytes
	helperSourceMaxBytes = 1 << 20
	defer func() { helperSourceMaxBytes = old }()
	big := make([]byte, 2<<20)
	bigSum := sha256.Sum256(big)
	if _, err := helperGzSource(gzFile(t, big), hex.EncodeToString(bigSum[:]))(); err == nil {
		t.Fatal("an oversized bundle was accepted")
	}
}
