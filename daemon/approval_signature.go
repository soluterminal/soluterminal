package main

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// approval_signature.go holds the signing scheme for what a phone shows for an
// approval, so a compromised relay cannot make a dangerous request look
// harmless (ST-193, phone public-release criterion C10b; format agreed with
// @secure 14 Nov).
//
// Since the Focus (native) panes were removed, the only signed approval is a
// terminal Claude Code permission menu (ST-265, terminal_one_tap.go): it is
// signed with signApproval (approval_signer.go), with promptId in the
// requestId slot, and answered through handlePaneChoicePromptResolve, which
// binds the digest to the live prompt. Nothing sends a requestId.
//
// Key and algorithm: this computer's identity key (daemon_identity.go, ECDSA
// P-256), which approved devices pin at enrolment (device code V2).
// ECDSA-SHA256, signature in P1363 form (r||s, 64 bytes), base64url without
// padding: the same form as the private-relay reply signature.
//
// Signed bytes: UTF-8, fields joined with "\n", no trailing newline:
//
//	SOLU-APPROVAL-V1
//	<machineId>
//	<paneId>
//	<requestId>
//	<toolName>
//	<widens: "1" or "0">
//	<issuedAtMs, decimal>
//	<base64url(sha256(summary))>
//
// The summary is hashed so a newline in a command cannot shift fields; if any
// other field is empty or contains "\n" the daemon does not sign, and the phone
// offers no one-tap. The phone rebuilds these bytes from what it DISPLAYS (the
// summary it shows, the tool name it shows, the machine id it looked the key up
// by) and never takes a hash from the push.
//
// Domain separation: the identity key's only other signature is the private
// relay reply, whose signed bytes always begin "SOLU-PRIVATE-V1-D\n"
// (privateReplyMessage), so no relay signature can be read as an approval.
//
// Binding: the phone's Approve sends approvalDigest =
// base64url(sha256(signed bytes)). handlePaneApprovalResolve recomputes it for
// the LIVE pending request and refuses a mismatch (409), which closes requestId
// reuse (Codex JSON-RPC ids restart with the app-server).

const (
	approvalSigDomain  = "SOLU-APPROVAL-V1"
	approvalSigVersion = "1"
)

// approvalSigner supplies the identity key and this computer's machine id.
// main wires it once at startup; nil (tests, an unclaimed daemon) means no
// signatures and so no phone one-tap.
type approvalSigner struct {
	key       func() *daemonIdentity
	machineID func() string
}

var activeApprovalSigner atomic.Pointer[approvalSigner]

func setApprovalSigner(s *approvalSigner) {
	approvalMachineIDCache.Store(nil)
	activeApprovalSigner.Store(s)
}

// approvalMachineIDCache keeps machineID() (a claim-file read) off the push
// and pane-list paths; refreshed every approvalMachineIDTTL so a re-claim is
// picked up.
type cachedMachineID struct {
	signer *approvalSigner
	id     string
	at     time.Time
}

const approvalMachineIDTTL = 30 * time.Second

var approvalMachineIDCache atomic.Pointer[cachedMachineID]

func (s *approvalSigner) currentMachineID() string {
	if c := approvalMachineIDCache.Load(); c != nil && c.signer == s && time.Since(c.at) < approvalMachineIDTTL {
		return c.id
	}
	id := s.machineID()
	approvalMachineIDCache.Store(&cachedMachineID{signer: s, id: id, at: time.Now()})
	return id
}

// signedApproval is the signature for one pending approval, made once and
// reused by every push and pane-list hint for it, so the digest the phone
// sends back matches what the daemon recomputes.
type signedApproval struct {
	machineID string
	issuedAt  int64
	sig       string
	digest    string
	// what was signed, to detect a stale cache entry
	tool, summary string
	widens        bool
}

// approvalSigMu guards a signedApproval cache (kept for the terminal-pane
// one-tap, which signs again).
var approvalSigMu sync.Mutex

// approvalSignedMessage builds the canonical bytes, or ok=false when a field
// is empty or contains a newline.
func approvalSignedMessage(machineID, paneID, requestID, toolName string, widens bool, issuedAtMs int64, summary string) ([]byte, bool) {
	for _, f := range []string{machineID, paneID, requestID, toolName} {
		if f == "" || strings.ContainsAny(f, "\r\n") {
			return nil, false
		}
	}
	w := "0"
	if widens {
		w = "1"
	}
	sum := sha256.Sum256([]byte(summary))
	return []byte(strings.Join([]string{
		approvalSigDomain, machineID, paneID, requestID, toolName, w,
		strconv.FormatInt(issuedAtMs, 10),
		base64.RawURLEncoding.EncodeToString(sum[:]),
	}, "\n")), true
}

func approvalDigest(msg []byte) string {
	sum := sha256.Sum256(msg)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
