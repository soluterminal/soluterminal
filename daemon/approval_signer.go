package main

import "time"

// signApproval signs (paneID, requestID, tool, summary, widens) under the
// SOLU-APPROVAL-V1 format (approval_signature.go) with this computer's
// identity key. It keeps no cache: callers that show the same request more
// than once keep the result so the issuedAt, and so the digest, stay stable
// (signTerminalOneTap). nil when signing is off, the computer is unclaimed,
// or a field can't be signed.
func signApproval(paneID, requestID, tool, summary string, widens bool) *signedApproval {
	s := activeApprovalSigner.Load()
	if s == nil || s.key == nil || s.machineID == nil {
		return nil
	}
	machineID := s.currentMachineID()
	if machineID == "" {
		return nil // unclaimed: nothing to sign for, and no key read
	}
	key := s.key() // may read the key from disk: outside any lock
	if key == nil {
		return nil
	}
	issuedAt := time.Now().UnixMilli()
	msg, ok := approvalSignedMessage(machineID, paneID, requestID, tool, widens, issuedAt, summary)
	if !ok {
		return nil
	}
	sig, err := key.sign(msg)
	if err != nil {
		return nil
	}
	return &signedApproval{machineID: machineID, issuedAt: issuedAt, sig: sig, digest: approvalDigest(msg), tool: tool, summary: summary, widens: widens}
}
