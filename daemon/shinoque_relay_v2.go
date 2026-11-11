package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Relay v2 signs every relayed request with its own MAC, so no reusable
// secret crosses a connection: the raw key and the per-device proof never do.
// The format is pinned by Shinoque's @security (ST-157, 2026-11-10):
//
//	X-Shinoque-Relay-V2: v2.<ts>.<nonce>.<mac>
//
// ts is Unix seconds; nonce is 16 random bytes and mac is HMAC-SHA256 under
// the literal key string, both base64url without padding. The MAC covers the
// LF-joined fields of shinoqueRelayV2Message. The core accepts |now-ts| <= 60 s
// and refuses a nonce it has seen.
const shinoqueRelayV2Header = "X-Shinoque-Relay-V2"

// The core answers every v2-verified request with an X-Shinoque-Relay-Proof
// header, an HMAC keyed as v2 is, over the LF-joined proof label, ts, nonce
// and decimal status (shinoqueRelayV2Proof). solud then knows the reply came
// from the core that verified the request (Shinoque @security, 2026-11-10).
const shinoqueRelayProofHeader = "X-Shinoque-Relay-Proof"

// Seams for tests; nil uses the clock and the OS CSPRNG.
var (
	shinoqueRelayV2Now   func() time.Time
	shinoqueRelayV2Nonce func() []byte
)

// shinoqueRelayV2Message is the signed bytes: method exactly as on the request
// line, target the raw request-target (path and query, as sent), prefix the
// X-Forwarded-Prefix value and device the private device id or "".
func shinoqueRelayV2Message(ts, nonce, method, target, prefix, device string) string {
	return strings.Join([]string{"shinoque-relay-v2", ts, nonce, method, target, prefix, device}, "\n")
}

func shinoqueRelayV2Value(key string, ts int64, nonce []byte, method, target, prefix, device string) string {
	stamp := strconv.FormatInt(ts, 10)
	encodedNonce := base64.RawURLEncoding.EncodeToString(nonce)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(shinoqueRelayV2Message(stamp, encodedNonce, method, target, prefix, device)))
	return "v2." + stamp + "." + encodedNonce + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// shinoqueRelayV2Sign returns a fresh header value for one request.
func shinoqueRelayV2Sign(key, method, target, prefix, device string) string {
	now := time.Now
	if shinoqueRelayV2Now != nil {
		now = shinoqueRelayV2Now
	}
	var nonce []byte
	if shinoqueRelayV2Nonce != nil {
		nonce = shinoqueRelayV2Nonce()
	} else {
		nonce = make([]byte, 16)
		_, _ = rand.Read(nonce)
	}
	return shinoqueRelayV2Value(key, now().Unix(), nonce, method, target, prefix, device)
}

// shinoqueRelayV2Parts returns the ts and nonce of a v2 value solud made.
func shinoqueRelayV2Parts(value string) (ts, nonce string, ok bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != "v2" || parts[1] == "" || len(parts[2]) != 22 {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func shinoqueRelayV2Proof(key, ts, nonce string, status int) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(strings.Join([]string{"shinoque-relay-v2-proof", ts, nonce, strconv.Itoa(status)}, "\n")))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// shinoqueRelayProofValid checks a reply against the v2 value that requested it.
func shinoqueRelayProofValid(key, requestV2 string, status int, header http.Header) bool {
	ts, nonce, ok := shinoqueRelayV2Parts(requestV2)
	proofs := header.Values(shinoqueRelayProofHeader)
	if !ok || len(proofs) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(proofs[0]), []byte(shinoqueRelayV2Proof(key, ts, nonce, status))) == 1
}
