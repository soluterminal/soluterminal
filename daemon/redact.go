package main

import (
	"fmt"
	"regexp"
	"strings"
)

// This module scrubs secrets and PII out of strings before they are shipped to
// the cloud (crash reports) or stored in the in-memory log ring. It is
// security-critical: a bug here leaks a secret, so the test suite is
// adversarial by design.
//
// All functions are pure (no I/O, no mutable globals beyond the compiled
// regexes), allocation-light, and safe to call on every log line.

var (
	// reHomePath rewrites a home-directory root + username down to "~",
	// preserving everything after the username segment.
	//
	// Roots recognised (the username is the FIRST segment after the root):
	//   /home/<user>/...
	//   /Users/<user>/...
	//   /mnt/c/Users/<user>/...   (WSL view of a Windows home)
	//   C:\Users\<user>\...  and  C:/Users/<user>/...
	//
	// The username run is [^/\\]+ so it stops at the first separator of either
	// flavour. The trailing separator is captured ($2) and re-emitted so the
	// rest of the path is kept verbatim (forward- or back-slash preserved).
	reHomePath = regexp.MustCompile(`(?:/mnt/c/Users|/home|/Users|[A-Za-z]:[\\/]Users)[\\/][^/\\]+([\\/])`)

	// reBearer collapses a "Bearer <token>" pair (any case for the keyword)
	// into "Bearer [REDACTED]". The token must be a 16+ char run from the
	// base64url / JWT-ish alphabet (which includes '.' so it spans whole JWTs).
	reBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._-]{16,}`)

	// reSkToken catches OpenAI / Anthropic style keys: "sk-" followed by 16+
	// chars of [A-Za-z0-9_-]. Anchored at a word boundary so it does not fire
	// mid-word.
	reSkToken = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)

	// reAWSKey catches AWS access-key ids: a documented prefix + 16 chars, 20
	// total — BELOW reLongOpaque's 32-char floor, so the catch-all misses them.
	reAWSKey = regexp.MustCompile(`\b(?:AKIA|ASIA|AROA|AIDA|AGPA|ANPA|AIPA|ABIA|ACCA|ANVA)[0-9A-Z]{16}\b`)

	// rePEMBlock catches a PEM private-key block including the body newlines
	// (?s makes '.' span lines). Matches "BEGIN [RSA|EC|OPENSSH ...] PRIVATE KEY".
	rePEMBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

	// reKVSecret catches key=value / key: value secrets whose VALUE may be
	// SHORT (below the opaque floor) or contain '/' '+' (which break the opaque
	// run) — e.g. "PASSWORD=hunter2", "AWS_SECRET_ACCESS_KEY=wJalr.../bPx...".
	// Only the value is scrubbed; the keyword is kept so the log stays
	// diagnosable.
	//
	// NO leading \b: the keyword is frequently a SUFFIX joined by '_' to a
	// prefix (DB_PASSWORD, AWS_SECRET_ACCESS_KEY, SESSION_TOKEN). RE2 counts '_'
	// as a word char, so a leading \b would never fire on those — the dominant
	// real-world shape in an env/.env snapshot. The trailing \b plus the
	// required =/: delimiter keep it from matching inside words like "tokens=5".
	// reURLUserinfo: scheme://user:password@host.
	reURLUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
	// reURLSecretParam: OAuth codes, session ids, signed-URL signatures and
	// token-bearing query parameters.
	reURLSecretParam = regexp.MustCompile(`(?i)([?&](?:code|state|sid|session|key|sig|signature|x-amz-signature|x-amz-credential|x-amz-security-token|token|access_token|id_token|refresh_token|client_secret|password|auth)=)[^&\s#"'<>]+`)
	// reCookieHeader: Cookie / Set-Cookie header values.
	reCookieHeader = regexp.MustCompile(`(?i)\b((?:set-)?cookie\s*:\s*)[^\r\n]+`)
	// reEnvKeyAssign: FOO_KEY=value style assignments (signing/API keys).
	reEnvKeyAssign = regexp.MustCompile(`\b([A-Z][A-Z0-9_]*_KEY)(\s*=\s*)[^\s"']+`)
	// reHFToken: Hugging Face tokens (hf_ + 34 letters) are letters-only, so
	// the opaque heuristics alone can miss them.
	reHFToken  = regexp.MustCompile(`\bhf_[A-Za-z]{30,}\b`)
	reKVSecret = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|access[_-]?key|auth[_-]?token)\b(\s*[=:]\s*)\S+`)

	// reWorkerArtifactKVSecret is the persisted-artifact variant of
	// reKVSecret. Worker streams and argv footers contain compact JSON, so a
	// whitespace-only value boundary would consume the closing quote and every
	// field after it. Consume a shell/TOML-style quoted value as one unit; for
	// unquoted values, stop before JSON string/field delimiters (including the
	// backslash before an escaped quote) while treating commas as value data.
	// This keeps boot-adoption envelopes and argv footers parseable without
	// leaking quoted or comma-delimited credentials.
	reWorkerArtifactKVSecret = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|access[_-]?key|auth[_-]?token)\b(\s*[=:]\s*)(?:"[^"\\]*"|[^[:space:]\\"}]+)`)

	// reJSONSecret catches quoted JSON/TOML-ish credential fields that the
	// key=value rule intentionally misses because of the quote between the key
	// and the colon. Codex auth.json uses these names; muse's
	// ~/.config/muse/auth.json adds `api_key` and `expires_at` under
	// providers.<name> (docs/DESIGN-multimodel-crew.md §4.3). Keep the key and
	// quotes so malformed-auth diagnostics stay useful while the value is
	// scrubbed.
	reJSONSecret = regexp.MustCompile(`(?i)("(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|auth[_-]?token|access_token|refresh_token|id_token|account_id|openai_api_key|codex_access_token|codex_api_key|expires_at)"\s*:\s*")[^"]*(")`)

	// reGHToken catches GitHub tokens: classic prefixed (ghp_/gho_/ghu_/
	// ghs_/ghr_) and fine-grained (github_pat_). `gh auth token` prints
	// these BARE in terminal scrollback, so the tail scrub needs a keyed
	// rule for them — they carry no "Bearer"/"key=" context to anchor on.
	reGHToken = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)

	// reLongOpaque catches a standalone run of 32+ chars from the
	// base64url / hex alphabet ([A-Za-z0-9_-]). This is the catch-all for the
	// 256-bit machine token (hex), OAuth tokens, etc.
	//
	// It is deliberately conservative to avoid false positives on ordinary
	// file paths and Go function names:
	//   - The run is word-bounded (\b ... \b) so it will not eat part of a
	//     longer token-with-punctuation.
	//   - A negative lookbehind-style guard is impossible in RE2, so the bounds
	//     plus the alphabet (no '/', '.', or '(') keep it from matching across
	//     path separators or qualified identifiers. A run like
	//     "main.(*Updater).Check" or "/app/daemon/updater.go" contains '.',
	//     '/', and '(' which are NOT in the alphabet, so no single 32-char
	//     match exists inside them.
	reLongOpaque = regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`)
)

// Feedback preview/submit uses redactFeedback and the shared feedback rule set.
// This legacy scrubber retains the log, crash and support-bundle contract.
//
// redactSensitive scrubs home-directory paths and token-shaped strings out of an
// arbitrary string (a log line, a panic message, or a stack trace). Pure,
// allocation-light, safe to call on every log line.
func redactSensitive(s string) string {
	if s == "" {
		return s
	}
	// Order: redact tokens FIRST, then rewrite home paths.
	//
	// Tokens first because the home-path rewrite shortens a path and could in
	// principle splice two runs together; doing tokens on the original text
	// avoids any cross-talk. The token rules never touch the path roots
	// (/home, /Users, ...) because those contain '/' which is not in any token
	// alphabet, so running them first is safe for the path rewrite too.
	s = reBearer.ReplaceAllString(s, "Bearer [REDACTED]")
	s = reSkToken.ReplaceAllString(s, "[REDACTED]")
	s = reAWSKey.ReplaceAllString(s, "[REDACTED]")
	s = reHFToken.ReplaceAllString(s, "[REDACTED]")
	s = rePEMBlock.ReplaceAllString(s, "[REDACTED]")
	// Keyword + delimiter kept ($1$2), only the value scrubbed. Runs BEFORE the
	// opaque catch-all so short values (below 32 chars) are still caught.
	s = reKVSecret.ReplaceAllString(s, "${1}${2}[REDACTED]")
	s = reJSONSecret.ReplaceAllString(s, "${1}[REDACTED]${2}")
	// URL- and header-borne credentials (bundle/feedback surfaces only; the
	// terminal-tail and auto-approval checks keep their narrower rule set).
	s = reURLUserinfo.ReplaceAllString(s, "${1}[REDACTED]@")
	s = reURLSecretParam.ReplaceAllString(s, "${1}[REDACTED]")
	s = reCookieHeader.ReplaceAllString(s, "${1}[REDACTED]")
	s = reEnvKeyAssign.ReplaceAllString(s, "${1}${2}[REDACTED]")
	s = reLongOpaque.ReplaceAllString(s, "[REDACTED]")

	// Rewrite home roots last. The replacement keeps the captured separator so
	// the remainder of the path (and its slash flavour) survives.
	s = reHomePath.ReplaceAllString(s, "~$1")
	return s
}

// redactTerminalTail scrubs credential-shaped content out of terminal
// scrollback before it leaves the daemon over MCP (read_pane_tail). It runs
// the KEYED rules only — deliberately NOT reLongOpaque and NOT the home-path
// rewrite: commit SHAs (40 hex), pane ids, and absolute paths are legitimate,
// load-bearing tail content for an orchestrating agent, and reLongOpaque
// would eat them wholesale. The keyed rules cover the credential shapes that
// actually appear in terminals (auth headers, api keys, env dumps, PEM,
// gh/JSON creds); the trade is explicit: an unprefixed random blob that a
// program prints bare is NOT caught here — that is what the full
// redactSensitive remains for on cloud-bound surfaces.
func redactTerminalTail(s string) string {
	if s == "" {
		return s
	}
	s = reBearer.ReplaceAllString(s, "Bearer [REDACTED]")
	s = reSkToken.ReplaceAllString(s, "[REDACTED]")
	s = reAWSKey.ReplaceAllString(s, "[REDACTED]")
	s = reGHToken.ReplaceAllString(s, "[REDACTED]")
	s = reHFToken.ReplaceAllString(s, "[REDACTED]")
	s = rePEMBlock.ReplaceAllString(s, "[REDACTED]")
	s = reKVSecret.ReplaceAllString(s, "${1}${2}[REDACTED]")
	s = reJSONSecret.ReplaceAllString(s, "${1}[REDACTED]${2}")
	return s
}

// redactWorkerArtifact scrubs a persisted workflow worker log (bounded stream
// plus its # argv: footer) before writeFileAtomicSynced. It uses the KEYED rule
// set only (reBearer, reSkToken, reAWSKey, reGHToken, rePEMBlock, the
// compact-JSON-safe reWorkerArtifactKVSecret variant of reKVSecret, and
// reJSONSecret), deliberately NOT reLongOpaque or the reHomePath rewrite,
// for the same reason as redactTerminalTail: absolute paths in the header dir=
// field, 40-hex commit SHAs in tool output, muse session=<uuid> ids, and
// <json-schema N bytes> markers are load-bearing diagnostics. Consequently,
// an unkeyed provider-specific value such as a bare xai-/gsk_/ds- token is not
// covered here; onboarding a provider with such a key shape must add an
// explicit bare-prefix rule rather than assuming artifact redaction catches it.
// The input may contain invalid UTF-8 because boundedBuf can cut mid-rune; Go
// regexp copies unmatched bytes verbatim, preserving every byte outside a
// matched secret. Keep this named function so artifact rules can diverge from
// the MCP tail scrub later without touching every runtime call site.
func redactWorkerArtifact(s string) string {
	if s == "" {
		return s
	}
	s = reBearer.ReplaceAllString(s, "Bearer [REDACTED]")
	s = reSkToken.ReplaceAllString(s, "[REDACTED]")
	s = reAWSKey.ReplaceAllString(s, "[REDACTED]")
	s = reGHToken.ReplaceAllString(s, "[REDACTED]")
	s = rePEMBlock.ReplaceAllString(s, "[REDACTED]")
	s = reWorkerArtifactKVSecret.ReplaceAllString(s, "${1}${2}[REDACTED]")
	s = reJSONSecret.ReplaceAllString(s, "${1}[REDACTED]${2}")
	return s
}

// redactStack scrubs a runtime stack trace (from runtime.Stack / debug.Stack).
// Equivalent to redactSensitive applied to the whole blob; provided as a named
// helper for call-site clarity.
func redactStack(b []byte) string {
	return redactSensitive(string(b))
}

// redactPanicValue renders a recovered panic value to a string and scrubs it.
func redactPanicValue(v any) string {
	if v == nil {
		return ""
	}
	var raw string
	switch t := v.(type) {
	case error:
		raw = t.Error()
	case string:
		raw = t
	default:
		raw = fmt.Sprintf("%v", v)
	}
	// Trim incidental whitespace so a panic value padded by the runtime renders
	// cleanly; the scrub itself is whitespace-agnostic.
	return redactSensitive(strings.TrimSpace(raw))
}

// opaqueSecretLike reports whether s contains a token that plausibly IS a
// secret — used to keep secret-looking prompt text away from the Jev model.
// reLongOpaque alone (any 32+ run of [A-Za-z0-9_-]) is right for redacting
// logs, but as an approval gate it treated ordinary descriptive names as
// secrets: agents name files and worktrees like
// "shinoque-files-850f559d-move-check-attempt2" or
// "solu-four-role-repaired-rehearsal-20261002", and every prompt mentioning
// one was declined as a "sensitive permission action". A candidate counts
// only when it has a long unbroken random-looking stretch: a segment (between
// '-' / '_') of 20+ characters mixing letters with digits, or mixed case.
// UUIDs and word-style names have only short segments. Keyed secrets
// (sk-ant-…, ghp_…, JWTs, key=value) are still caught by redactTerminalTail.
func opaqueSecretLike(s string) bool {
	passwordContext := rePasswordContext.MatchString(s)
	for _, tok := range reLongOpaque.FindAllString(s, -1) {
		for _, seg := range strings.FieldsFunc(tok, func(r rune) bool { return r == '-' || r == '_' }) {
			if len(seg) < 20 {
				continue
			}
			if randomLooking(seg) || (passwordContext && mixedCaseLetters(seg)) {
				return true
			}
		}
	}
	return false
}

// randomLooking: letters mixed with digits, or both cases — the shape of a
// key or digest, unlike a long plain word.
func randomLooking(seg string) bool {
	var lower, upper, digit bool
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	if (lower || upper) && digit {
		return true
	}
	return lower && upper && !camelCaseWords(seg)
}

// mixedCaseLetters reports a segment with both letter cases.
func mixedCaseLetters(seg string) bool {
	return strings.IndexFunc(seg, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0 &&
		strings.IndexFunc(seg, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0
}

// rePasswordContext marks text where a word-shaped value is likely a
// passphrase (sshpass, -p, --password, user:pass@/-u user:, login); the
// CamelCase exemption is not applied there.
var rePasswordContext = regexp.MustCompile(`(?i)(\bsshpass\b|(^|\s)-p\s|--password|\blogin\b|(^|\s)-u\s+\S+:|\b(passphrase|passwd|password)\b)`)

// camelCaseWords reports whether a letters-only segment reads as words
// joined in CamelCase/camelCase (TestWorkflowBootHealsWrittenDepCycle,
// useAgentsStatusPolling, HTTPServerConfig): each word is a capital plus a
// lowercase tail that contains a vowel, two-letter words come from a short
// list of real ones ("On", "To", "Is"), and all-caps runs are short
// acronyms. A random mixed-case key flips case and drops vowels far too
// often to parse this way.
func camelCaseWords(seg string) bool {
	i, n := 0, len(seg)
	isLower := func(b byte) bool { return b >= 'a' && b <= 'z' }
	isUpper := func(b byte) bool { return b >= 'A' && b <= 'Z' }
	hasVowel := func(w string) bool { return strings.ContainsAny(w, "aeiouy") }
	// Optional leading lowercase word (camelCase).
	for i < n && isLower(seg[i]) {
		i++
	}
	if i > 0 && (i < 3 || !hasVowel(seg[:i])) {
		return false
	}
	for i < n {
		u := i
		for i < n && isUpper(seg[i]) {
			i++
		}
		ups := i - u
		if ups == 0 || ups > 6 {
			return false // non-letter (digits are handled by the caller) or a long caps run
		}
		l := i
		for i < n && isLower(seg[i]) {
			i++
		}
		tail := seg[l:i]
		switch {
		case tail == "":
			// Trailing acronym (…ID, …HTTP).
		case len(tail) == 1:
			if !camelTwoLetterWords[strings.ToLower(seg[i-2:i])] {
				return false
			}
			// Include the initial capital in the word: Owns and Ends contain
		// their only vowel there. An acronym prefix is not part of the word.
		case !hasVowel(strings.ToLower(seg[l-1 : i])):
			return false
		}
	}
	return true
}

// camelTwoLetterWords are the two-letter words common in identifiers.
var camelTwoLetterWords = map[string]bool{
	"an": true, "as": true, "at": true, "be": true, "by": true, "do": true, "go": true,
	"id": true, "if": true, "in": true, "is": true, "it": true, "my": true, "no": true,
	"of": true, "ok": true, "on": true, "or": true, "so": true, "to": true, "up": true,
	"we": true, "ui": true, "os": true, "db": true, "io": true,
}
