//go:build !linux

package main

import (
	"context"
	"testing"
)

func TestGeneratedStorageUnsupportedPlatformRefusesMutation(t *testing.T) {
	if err := generatedRemove(context.Background(), t.TempDir(), generatedFSIdentity{}, nil); generatedReason(err) != "filesystem_identity_unsupported" {
		t.Fatalf("unsupported platform removal err=%v", err)
	}
}
