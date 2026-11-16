package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// The canonical bytes are the contract with the phone (ST-194): this golden
// value must only change together with approvalSigDomain's version.
func TestApprovalSignedMessageGolden(t *testing.T) {
	t.Parallel()
	msg, ok := approvalSignedMessage("mach_1", "pane_1", "req-1", "Bash", false, 1794641562152, "git status\nrm -rf x")
	if !ok {
		t.Fatal("refused a signable request")
	}
	sum := sha256.Sum256([]byte("git status\nrm -rf x"))
	want := "SOLU-APPROVAL-V1\nmach_1\npane_1\nreq-1\nBash\n0\n1794641562152\n" + base64.RawURLEncoding.EncodeToString(sum[:])
	if string(msg) != want {
		t.Fatalf("message =\n%q\nwant\n%q", msg, want)
	}
	if m, _ := approvalSignedMessage("m", "p", "r", "Bash", true, 1, ""); !strings.Contains(string(m), "\nBash\n1\n1\n") {
		t.Fatalf("widens not encoded as 1: %q", m)
	}
	for _, bad := range [][4]string{
		{"", "p", "r", "Bash"},
		{"m", "p\nx", "r", "Bash"},
		{"m", "p", "r\r", "Bash"},
		{"m", "p", "r", "Ba\nsh"},
		{"m", "p", "", "Bash"},
	} {
		if _, ok := approvalSignedMessage(bad[0], bad[1], bad[2], bad[3], false, 1, "s"); ok {
			t.Fatalf("signed fields %q", bad)
		}
	}
}

// @secure amendment 1: the identity key's other signature (the private-relay
// reply) can never be read as an approval.
func TestApprovalSignatureDomainSeparatedFromRelayReply(t *testing.T) {
	t.Parallel()
	reply := privateReplyMessage([]byte("hello"), privateReply{Eph: "SOLU-APPROVAL-V1", Nonce: "n", Identity: "i"})
	if !strings.HasPrefix(string(reply), "SOLU-PRIVATE-V1-D\n") || strings.HasPrefix(string(reply), approvalSigDomain+"\n") {
		t.Fatalf("relay reply bytes start %q", string(reply)[:24])
	}
}

func withTestApprovalSigner(t *testing.T, machineID string) *daemonIdentity {
	t.Helper()
	id, err := loadDaemonIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prev := activeApprovalSigner.Load()
	setApprovalSigner(&approvalSigner{key: func() *daemonIdentity { return id }, machineID: func() string { return machineID }})
	t.Cleanup(func() { activeApprovalSigner.Store(prev) })
	return id
}

// verifyApprovalSig is what the phone does: rebuild the bytes from what it
// shows and verify against the pinned SPKI.
func verifyApprovalSig(t *testing.T, spki, machineID, paneID, reqID, tool string, widens bool, issuedAt, summary, sig string) bool {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(spki)
	if err != nil {
		t.Fatal(err)
	}
	pubAny, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	pub := pubAny.(*ecdsa.PublicKey)
	ms, err := json.Number(issuedAt).Int64()
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := approvalSignedMessage(machineID, paneID, reqID, tool, widens, ms, summary)
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(raw) != 64 {
		return false
	}
	digest := sha256.Sum256(msg)
	return ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:]))
}
