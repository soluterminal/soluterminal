package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

// The compiled key is the only one a release build uses; a dev build may take
// SOLU_UPDATE_MANIFEST_PUBKEY instead.
func TestUpdateManifestPublicKeySource(t *testing.T) {
	compiled, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, oldVersion := defaultUpdateManifestPubKey, version
	t.Cleanup(func() { defaultUpdateManifestPubKey, version = oldKey, oldVersion })
	defaultUpdateManifestPubKey = base64.StdEncoding.EncodeToString(compiled)
	t.Setenv("SOLU_UPDATE_MANIFEST_PUBKEY", base64.StdEncoding.EncodeToString(other))

	for _, tc := range []struct {
		version string
		want    ed25519.PublicKey
	}{
		{"0.0.1243", compiled},
		{"0.0.1243.7", compiled},
		{"", compiled},
		{"dev", other},
	} {
		version = tc.version
		got, err := updateManifestPublicKey()
		if err != nil {
			t.Fatalf("version %q: %v", tc.version, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("version %q: got the %s key", tc.version, map[bool]string{true: "environment's", false: "compiled"}[bytes.Equal(got, other)])
		}
	}

	// A release build with nothing compiled in stays without a key: the
	// environment can't supply one either.
	version = "0.0.1243"
	defaultUpdateManifestPubKey = ""
	if got, err := updateManifestPublicKey(); err != nil || got != nil {
		t.Fatalf("release build without a compiled key = %x, %v", got, err)
	}
	// A malformed environment key is ignored in a release build.
	defaultUpdateManifestPubKey = base64.StdEncoding.EncodeToString(compiled)
	t.Setenv("SOLU_UPDATE_MANIFEST_PUBKEY", "not-a-key")
	if got, err := updateManifestPublicKey(); err != nil || !bytes.Equal(got, compiled) {
		t.Fatalf("release build with a malformed environment key = %x, %v", got, err)
	}
}
