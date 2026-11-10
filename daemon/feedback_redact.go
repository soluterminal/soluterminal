package main

import (
	"soluterminal-shared/feedbackredact"
	"time"
)

// redactFeedback scrubs text that leaves this computer in a feedback report:
// title, body, pane text, logs and replies. Identity discovery is cached; the
// shared rules and marker preservation make repeated preview/submit passes
// idempotent. Log capture and terminal-tail redaction keep their own contracts.
func redactFeedback(s string) string {
	return redactFeedbackWith(s, cachedFeedbackRedactor(), feedbackPairingCodes.recent(time.Now()))
}

// Keep the adapter pure and injectable so parity tests never inspect the test
// host's real identity or invoke Windows interop.
func redactFeedbackWith(s string, device *feedbackredact.Redactor, codes []string) string {
	text, _ := device.WithCodes(codes).Redact(s)
	return text
}
