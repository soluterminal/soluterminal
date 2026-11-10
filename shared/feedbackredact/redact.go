// Package feedbackredact owns the device and cloud feedback redaction rules.
// It uses RE2 expressions and linear merges of ordered matches. Inputs are never
// logged. Redactor is immutable and safe for concurrent use.
package feedbackredact

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Identity contains only local identity hints, never credentials. The daemon
// supplies its cached host/user names; the cloud uses the same rules without
// collecting the cloud machine's identity.
type Identity struct {
	Hostnames []string
	Usernames []string
	Codes     []string // known local pairing codes, regardless of shape
}

type rule struct {
	re     *regexp.Regexp
	kind   string
	groups []int    // value groups; empty means the whole match
	hints  []string // case-insensitive literals; any one is enough
}

func pattern(kind, expr string, groups []int, hints ...string) rule {
	return rule{regexp.MustCompile(expr), kind, groups, hints}
}

const secretName = `(?:password|passphrase|passwd|secret|token|api[_-]?key|access[_-]?key|auth[_-]?token|access_token|refresh_token|id_token|account_id|openai_api_key|codex_access_token|codex_api_key|expires_at|[A-Z][A-Z0-9_]*_KEY)`
const codeName = `(?:pairing[ _-]?code|pair[ _-]?code|device[ _-]?code|verification[ _-]?code|user[ _-]?code|claim[ _-]?code)`

// This is the only pattern list. Keep provider-specific forms ahead of the
// opaque fallback. Context rules scrub short values as well as long tokens.
var rules = []rule{
	pattern("private key", `(?s)-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----.*?(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----|\z)`, nil, "private key-----"),
	pattern("password", `(?im)(?:^|\n)[^\r\n]*(?:password|passphrase)[^:\r\n]*:[ \t]*\r?\n([^\r\n]+)`, []int{1}, "password", "passphrase"),
	pattern("header", `(?i)\b(?:authorization|proxy-authorization|(?:set-)?cookie)[ \t]*:[ \t]*([^\s"'][^\r\n"']*)`, []int{1}, "authorization", "cookie"),
	pattern("code", `(?i)"`+codeName+`"[ \t]*:[ \t]*"([^"\r\n]+)"`, []int{1}, "code"),
	pattern("code", `(?i)\b`+codeName+`(?:[ \t]+is)?[ \t]*(?:[=:][ \t]*|[ \t]+)(?:"([^"\r\n]+)"|'([^'\r\n]+)'|([^\s"'&,;<>]+))`, []int{1, 2, 3}, "code"),
	pattern("claim", `\b[ABCDEFGHJKMNPQRSTUVWXYZ23456789]{6}\b`, nil),
	pattern("code", `\b[0-9A-HJKMNP-TV-Z]{4}(?:-[0-9A-HJKMNP-TV-Z]{4}){3}\b`, nil, "-"),
	pattern("token", `(?i)(?:[?&])(?:code|state|sid|session|key|sig|signature|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature|token|access_token|id_token|refresh_token|client_secret|password|auth|[A-Z0-9_-]*`+secretName+`)=([^&\s#"'<>]+)`, []int{1}, "?", "&"),
	pattern("token", `(?i)"`+secretName+`"[ \t]*:[ \t]*"([^"\r\n]+)"`, []int{1}, "\""),
	pattern("assignment", `(?i)`+secretName+`\b[ \t]*[=:][ \t]*(?:"([^"\r\n]+)"|'([^'\r\n]+)'|([^\s]+))`, []int{1, 2, 3}, "=", ":"),
	pattern("token", `(?i)\bBearer[ \t]+([A-Za-z0-9._~+/-]+=*)`, []int{1}, "bearer"),
	pattern("token", `\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{8,}|xapp-[A-Za-z0-9-]{8,}|xoxe-[A-Za-z0-9-]{8,}|AIza[A-Za-z0-9_-]{35}|[sr]k_live_[A-Za-z0-9]{8,}|npm_[A-Za-z0-9]{8,}|glpat-[A-Za-z0-9_-]{8,}|hf_[A-Za-z]{30,})\b`, nil, "sk-", "gh", "xox", "xapp-", "AIza", "k_live_", "npm_", "glpat-", "hf_"),
	pattern("token", `\b(?:AKIA|ASIA|AROA|AIDA|AGPA|ANPA|AIPA|ABIA|ACCA|ANVA)[0-9A-Z]{16}\b`, nil, "akia", "asia", "aroa", "aida", "agpa", "anpa", "aipa", "abia", "acca", "anva"),
	pattern("token", `\b[0-9]{8,10}:[A-Za-z0-9_-]{35}\b`, nil, ":"),
	pattern("token", `\b(?:mfa\.[A-Za-z0-9_-]{20,}|[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{6}\.[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`, nil, "."),
	pattern("token", `(?i)\b[a-z][a-z0-9+.-]*://([^/\s:@]+:[^/\s@]+@)`, []int{1}, "://"),
	pattern("email", `(?i)\b[A-Z0-9.!#$%&'*+/=?^_`+"`"+`{|}~-]+@[A-Z0-9](?:[A-Z0-9-]*[A-Z0-9])?(?:\.[A-Z0-9](?:[A-Z0-9-]*[A-Z0-9])?)+`, nil, "@"),
	pattern("host", `\b[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+`, nil, "@"),
	pattern("home", `(?i)(?:/mnt/[a-z]/Users|/home|/Users|[A-Z]:[\\/]Users)[\\/][^/\\\s"'<>:]+`, nil, "/users", "/home", "\\users"),
	pattern("home", `(?i)\\\\wsl(?:\.localhost|\$)\\[^\\\s]+\\home\\[^\\\s"'<>:]+`, nil, "\\wsl"),
	pattern("home", `/root(?:/|\b)`, nil, "/root"),
	pattern("token", `\b[A-Za-z0-9_-]{32,}\b`, nil),
}

var marker = regexp.MustCompile(`\[(?:REDACTED|redacted: (?:token|email|ip|host|user|code|private key|password|header))\]`)

// Redactor holds an optional device identity expression. Names are literals,
// case-insensitive, and bounded by identifier characters to avoid changing a
// name inside an unrelated word. Even short local names are covered.
type Redactor struct{ identities []rule }

func New(id Identity) *Redactor {
	r := &Redactor{}
	for _, set := range []struct {
		kind  string
		names []string
	}{{"host", id.Hostnames}, {"user", id.Usernames}, {"code", id.Codes}} {
		seen := map[string]bool{}
		var names []string
		for _, name := range set.names {
			name = strings.TrimSpace(name)
			key := strings.ToLower(name)
			if name != "" && !seen[key] {
				seen[key] = true
				names = append(names, regexp.QuoteMeta(name))
			}
		}
		sort.SliceStable(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		if len(names) > 0 {
			r.identities = append(r.identities, pattern(set.kind, `(?i)(?:`+strings.Join(names, "|")+`)`, nil))
		}
	}
	return r
}

// WithCodes adds literal local codes to the immutable cached identity hints.
// The daemon supplies only active/recent codes, never its pairing secret.
func (r *Redactor) WithCodes(codes []string) *Redactor {
	if len(codes) == 0 {
		return r
	}
	extra := New(Identity{Codes: codes})
	identities := append([]rule(nil), r.identities...)
	return &Redactor{identities: append(identities, extra.identities...)}
}

var standard = New(Identity{})

// Redact applies the same pattern set used by the daemon and cloud.
func Redact(s string) string { text, _ := standard.Redact(s); return text }

// Redact returns scrubbed text and the number of new replacements. Existing
// markers are opaque, so a cloud backstop can add markers without rewriting
// device markers. No input or identity hint is ever included in a diagnostic.
func (r *Redactor) Redact(s string) (string, int) {
	spans := r.findSpans(s)
	// Match on the complete input: a marker inside a PEM block or header must
	// not hide its remaining context from the backstop. Then subtract marker
	// bytes from the removal spans so existing preview markers stay verbatim.
	spans = preserveMarkers(spans, marker.FindAllStringIndex(s, -1))
	if len(spans) == 0 {
		return s, 0
	}
	var out strings.Builder
	start := 0
	for _, loc := range spans {
		out.WriteString(s[start:loc.start])
		if loc.kind == "home" {
			out.WriteByte('~')
			if strings.HasSuffix(s[loc.start:loc.end], "/") {
				out.WriteByte('/')
			}
		} else {
			out.WriteString("[redacted: " + loc.kind + "]")
		}
		start = loc.end
	}
	out.WriteString(s[start:])
	return out.String(), len(spans)
}

// Both streams are ordered and disjoint. A removal spanning multiple markers
// becomes separate removals around them, without deleting or changing markers.
func preserveMarkers(spans []span, markers [][]int) []span {
	if len(markers) == 0 {
		return spans
	}
	var out []span
	j := 0
	for _, loc := range spans {
		start := loc.start
		for j < len(markers) && markers[j][1] <= start {
			j++
		}
		for j < len(markers) && markers[j][0] < loc.end {
			if start < markers[j][0] {
				out = append(out, span{start, markers[j][0], loc.kind})
			}
			start = max(start, markers[j][1])
			if markers[j][1] > loc.end {
				break
			}
			j++
		}
		if start < loc.end {
			out = append(out, span{start, loc.end, loc.kind})
		}
	}
	return out
}

type span struct {
	start, end int
	kind       string
}

// merge combines ordered streams in linear time; no input-sized sort and no
// repeated replacement passes. Overlap is unioned so one match cannot expose
// the tail of another credential. Earlier rules decide the marker kind on ties.
func merge(a, b []span) []span {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]span, 0, len(a)+len(b))
	for len(a) > 0 || len(b) > 0 {
		var next span
		if len(b) == 0 || (len(a) > 0 && a[0].start <= b[0].start) {
			next = a[0]
			a = a[1:]
		} else {
			next = b[0]
			b = b[1:]
		}
		if len(out) > 0 && next.start < out[len(out)-1].end {
			if next.end > out[len(out)-1].end {
				out[len(out)-1].end = next.end
			}
		} else {
			out = append(out, next)
		}
	}
	return out
}

func matches(p rule, s, lower string, identity bool) []span {
	if len(p.hints) > 0 {
		found := false
		for _, hint := range p.hints {
			if strings.Contains(lower, strings.ToLower(hint)) {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	var out []span
	for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
		// Query parameters have their own delimiter-aware rule. Everywhere
		// else, an unquoted password may contain punctuation and must be
		// removed through the next whitespace, preserving the legacy safety.
		if p.kind == "assignment" && queryKey(s, m[0]) {
			continue
		}
		a, b := m[0], m[1]
		for _, group := range p.groups {
			if m[2*group] >= 0 {
				a, b = m[2*group], m[2*group+1]
				break
			}
		}
		if a == b {
			continue
		}
		kind := p.kind
		if kind == "assignment" {
			kind = "token"
		}
		if kind == "claim" {
			if !strings.ContainsAny(s[a:b], "23456789") {
				continue
			}
			kind = "code"
		}
		if identity && (!nameBoundary(s[:a], true) || !nameBoundary(s[b:], false)) {
			continue
		}
		out = append(out, span{a, b, kind})
	}
	return out
}

// queryKey recognizes even a suffix match inside a prefixed credential key.
// It scans only that key, so all matches together remain linear in input size.
func queryKey(s string, start int) bool {
	for start > 0 {
		c := s[start-1]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			start--
			continue
		}
		return c == '?' || c == '&'
	}
	return false
}

func nameBoundary(s string, last bool) bool {
	if s == "" {
		return true
	}
	var r rune
	if last {
		r, _ = utf8.DecodeLastRuneInString(s)
	} else {
		r, _ = utf8.DecodeRuneInString(s)
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
}

func (r *Redactor) findSpans(s string) []span {
	lower := strings.ToLower(s)
	var spans []span
	for _, p := range rules {
		spans = merge(spans, matches(p, s, lower, false))
	}
	spans = merge(spans, ipSpans(s))
	for _, p := range r.identities {
		spans = merge(spans, matches(p, s, lower, true))
	}
	return spans
}

// Parsing bounded address candidates keeps versions and stack locations intact,
// covers compressed/mapped IPv6 and zones, and avoids a backtracking IP regex.
var ipv4Candidate = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)

func ipSpans(s string) []span {
	var ipv4 []span
	for _, loc := range ipv4Candidate.FindAllStringIndex(s, -1) {
		if _, err := netip.ParseAddr(s[loc[0]:loc[1]]); err == nil {
			ipv4 = append(ipv4, span{loc[0], loc[1], "ip"})
		}
	}
	var ipv6 []span
	for i := 0; i < len(s); {
		if !ipBodyByte(s[i]) {
			i++
			continue
		}
		start := i
		for i < len(s) && ipBodyByte(s[i]) {
			i++
		}
		if i < len(s) && s[i] == '%' {
			i++
			for i < len(s) && zoneByte(s[i]) {
				i++
			}
		}
		candidate := strings.TrimRight(s[start:i], ".")
		end := start + len(candidate)
		if len(candidate) > 128 || !strings.Contains(candidate, ":") {
			continue
		}
		if _, err := netip.ParseAddr(candidate); err == nil {
			ipv6 = append(ipv6, span{start, end, "ip"})
			continue
		}
		// Log prefixes/suffixes can abut addresses with a colon. A single leading
		// colon is punctuation; a double colon is a real IPv6 compression prefix.
		if strings.HasPrefix(candidate, ":") && !strings.HasPrefix(candidate, "::") {
			start++
			candidate = candidate[1:]
		}
		candidate = strings.TrimRight(candidate, ":")
		end = start + len(candidate)
		if _, err := netip.ParseAddr(candidate); err == nil {
			ipv6 = append(ipv6, span{start, end, "ip"})
		}
	}
	return merge(ipv4, ipv6)
}

func ipBodyByte(c byte) bool {
	return c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c >= '0' && c <= '9' || c == ':' || c == '.'
}

func zoneByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}
