package main

import (
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"soluterminal-shared/feedbackredact"
)

// The sidebar shows each server's url, command and args in a short display
// form. The config values themselves stay inside the daemon: every caller
// that serialises a server list goes through mcpServersForDisplay.

const mcpMasked = "[redacted: token]"

// mcpServersForDisplay returns a copy of list whose url, command and args
// are the short display form. The input is left untouched, so the liveness
// probe and capability checks keep working on the configured values.
func mcpServersForDisplay(list []McpServerInfo) []McpServerInfo {
	out := make([]McpServerInfo, len(list))
	for i, s := range list {
		out[i] = s
		if s.URL != "" {
			out[i].URL = mcpDisplayURL(s.URL)
		}
		// One scanner for command and args: a flag at the end of the command
		// string still covers the first argument.
		var scan mcpArgScanner
		if s.Command != "" {
			out[i].Command = scan.arg(s.Command, false)
		}
		if s.Args != nil {
			out[i].Args = make([]string, len(s.Args))
			for j, a := range s.Args {
				out[i].Args[j] = scan.arg(a, true)
			}
		}
	}
	return out
}

var mcpSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// mcpDisplayLooseURL handles a value that does not parse as scheme://host.
// It keeps only what precedes the query and fragment, and nothing up to the
// last "@": a malformed sign-in part must not survive because it was
// malformed.
func mcpDisplayLooseURL(raw string) string {
	head := raw
	if i := strings.IndexAny(head, "?#"); i >= 0 {
		head = head[:i]
	}
	scheme := ""
	if i := strings.Index(head, "://"); i >= 0 && mcpSchemeRe.MatchString(head[:i]) {
		scheme, head = head[:i+3], head[i+3:]
	}
	if at := strings.LastIndex(head, "@"); at >= 0 {
		head = head[at+1:]
	}
	return scheme + feedbackredact.Redact(head)
}

// mcpDisplayURL keeps scheme://host/path. Sign-in details before the host
// and the fragment are dropped, and every query value is replaced; query
// names stay so the row is still recognisable.
func mcpDisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return mcpDisplayLooseURL(raw)
	}
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	b.WriteString(u.Host)
	segments := strings.Split(u.EscapedPath(), "/")
	for i, seg := range segments {
		if seg != "" {
			segments[i] = mcpPathSegment(seg)
		}
	}
	b.WriteString(strings.Join(segments, "/"))
	if u.RawQuery != "" || u.ForceQuery {
		b.WriteByte('?')
		parts := strings.FieldsFunc(u.RawQuery, func(r rune) bool { return r == '&' || r == ';' })
		for i, part := range parts {
			if i > 0 {
				b.WriteByte('&')
			}
			name, _, hasValue := strings.Cut(part, "=")
			if !hasValue {
				// A bare query string can be the value itself.
				b.WriteString(mcpMasked)
				continue
			}
			b.WriteString(feedbackredact.Redact(name))
			b.WriteByte('=')
			b.WriteString(mcpMasked)
		}
	}
	return b.String()
}

// mcpArgScanner walks a command line one piece at a time and remembers when
// the previous piece was a flag whose value must not be shown, so both
// "--flag value" (two pieces) and "--flag=value" (one piece) are covered.
//
// Pieces are split on spaces only. Quotes are not trusted to nest cleanly
// (shell lines inside shell lines escape them in many ways), so a flag is
// recognised with any quote marks around it, and a hidden value that opens
// a quote stays hidden until that quote closes or the string ends. A hidden
// value that ends in a backslash continues into the next piece, because a
// shell reads "two\ words" as one value.
type mcpArgScanner struct {
	maskNext   bool // previous piece was a bare value-hiding flag
	headerNext bool // previous piece was a bare header flag
	closer     byte // inside a hidden quoted value: the quote mark that ends it
	closerEsc  bool // that quote mark was written with a backslash
	spaceNext  bool // the hidden value ended in a backslash: the next piece is still the value
	shortNext  byte // previous piece was -p, -k or -u: its letter
	// runner is set while reading the own options of a program that uses -p
	// for something else (a package, a port, a project). It ends at that
	// program's first plain word, so the -p of whatever it starts counts again.
	runner    bool
	runnerArg bool // previous piece was the runner's own -p: the next piece is its value
	atStart   bool // the piece being read is where a program name stands
	prevC     bool // previous piece was -c or /c: a program name follows
	// Text that is already in its display form (a shortened address, a
	// package name with its version, an IP address). The shared rules would
	// read "name@version" as a mail address and mask the host of an address
	// the url field shows, so these pieces sit out that pass.
	kept []string
}

// keep returns a stand-in for text the shared rules must not rewrite.
func (s *mcpArgScanner) keep(text string) string {
	s.kept = append(s.kept, text)
	return "\x00" + strconv.Itoa(len(s.kept)-1) + "\x00"
}

var mcpKeptRe = regexp.MustCompile("\x00([0-9]+)\x00")

// finish runs the shared rules over a scanned string and puts the kept
// pieces back. A stand-in the rules removed stays removed.
func (s *mcpArgScanner) finish(text string) string {
	out := mcpKeptRe.ReplaceAllStringFunc(feedbackredact.Redact(text), func(m string) string {
		if i, err := strconv.Atoi(m[1 : len(m)-1]); err == nil && i < len(s.kept) {
			return s.kept[i]
		}
		return ""
	})
	s.kept = s.kept[:0]
	return strings.ReplaceAll(out, "\x00", "")
}

// arg returns the display form of one string. whole says the string is a
// single argv element: when a flag is waiting for its value, the complete
// element is that value, spaces included.
func (s *mcpArgScanner) arg(text string, whole bool) string {
	s.closer, s.spaceNext = 0, false // a value never runs from one string into the next
	// Stand-ins are built from NUL bytes; none may come from the config.
	text = strings.ReplaceAll(text, "\x00", "")
	afterC := s.prevC
	s.prevC = false
	if whole && (s.maskNext || s.headerNext) {
		return s.finish(s.value(text))
	}
	if whole && s.shortNext != 0 {
		flag := s.shortNext
		s.shortNext = 0
		if hidden, ok := mcpShortValue(flag, text); ok {
			return s.finish(hidden)
		}
	}
	if whole {
		// "--flag=two words" as one argv element: everything after the
		// separator is the value, so decide before splitting on spaces.
		if name, sep, value, ok := mcpAssignment(text); ok {
			switch {
			case mcpHiddenName(name):
				return s.finish(name + sep + mcpMasked)
			case mcpHeaderFlag(name):
				return s.finish(name + sep + mcpHeaderValue(value))
			}
			if flag := s.shortFlag(name); flag != 0 {
				if hidden, ok := mcpShortValue(flag, value); ok {
					return s.finish(name + sep + hidden)
				}
			}
		}
	}
	// A program name starts the command, a shell line given as one argument,
	// or the argument after -c.
	start := !whole || afterC || strings.ContainsAny(strings.TrimSpace(text), " \t")
	return s.finish(s.line(text, start))
}

// value consumes a pending flag for one complete argv element.
func (s *mcpArgScanner) value(text string) string {
	if s.maskNext {
		s.chain(text)
		if _, core, _ := mcpSplitQuotes(text); mcpSchemeWord(core) {
			s.maskNext = true
		}
		return mcpMasked
	}
	s.headerNext = false
	if _, core, _ := mcpSplitQuotes(text); len(core) > 1 && strings.IndexByte(core, ':') == len(core)-1 && !strings.ContainsAny(core, " \t") {
		// A header name on its own (its only colon is the last character):
		// the value follows as the next piece.
		s.maskNext = true
		return text
	}
	return mcpHeaderValue(text)
}

// chain is called when a hidden value itself looks like a flag: the flag
// before it may take no value ("--no-auth --header ..."), so the piece is
// hidden and still counts as the flag it looks like. "--token --secret x"
// never leaves x visible.
func (s *mcpArgScanner) chain(core string) {
	bare := mcpBareFlag(core)
	s.headerNext = bare && mcpHeaderFlag(core)
	s.maskNext = bare && !s.headerNext && mcpHiddenName(core)
	s.shortNext = 0
	if bare && !s.headerNext && !s.maskNext {
		s.shortNext = s.shortFlag(core)
	}
}

// shortFlag returns the letter of a one-letter flag whose value is usually a
// sign-in detail (-p, -k, -u), or 0. Among a runner's own options -p means
// something else and is left alone.
func (s *mcpArgScanner) shortFlag(core string) byte {
	switch core {
	case "-k", "-u":
		return core[1]
	case "-p":
		if !s.runner {
			return 'p'
		}
	}
	return 0
}

var mcpRunners = map[string]bool{
	"npx": true, "npm": true, "pnpm": true, "bunx": true, "bun": true, "yarn": true,
	"docker": true, "podman": true, "tsc": true, "uv": true, "uvx": true, "node": true, "ssh": true,
}

func mcpRunnerWord(core string) bool {
	if i := strings.LastIndexAny(core, `/\`); i >= 0 {
		core = core[i+1:]
	}
	core = strings.ToLower(core)
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		core = strings.TrimSuffix(core, ext)
	}
	return mcpRunners[core]
}

// A port, a port pair or user and group numbers: 8080, 8080:80, 1000:1000.
var mcpPortRe = regexp.MustCompile(`^[0-9]{1,5}(?::[0-9]{1,5})?(?:/(?:tcp|udp))?$`)

// A file name by its ending, for values written without a folder.
var mcpFileRe = regexp.MustCompile(`(?i)\.(?:json|jsonc|js|mjs|cjs|ts|py|toml|ya?ml|pem|key|crt|cer|p12|pfx|txt|conf|cfg|ini|env|sock|db|sqlite)$`)

// mcpShortPlain reports whether the value after a one-letter flag is plainly
// not a sign-in detail: nothing, another flag, an address or package, a
// port, or a path.
func mcpShortPlain(flag byte, v string) bool {
	if v == "" || v == "." || v == ".." || v[0] == '-' || v[0] == '/' || v[0] == '~' {
		return true
	}
	if strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../") || mcpWindowsPathRe.MatchString(v) {
		return true
	}
	if _, ok := mcpDisplayWord(v); ok {
		return true
	}
	if mcpPortRe.MatchString(v) || mcpFileRe.MatchString(v) {
		return true
	}
	// -u is a sign-in only in the form name:password.
	return flag == 'u' && !strings.Contains(v, ":")
}

var mcpWindowsPathRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// mcpShortValue returns the hidden form of one complete value after a
// one-letter flag, or false when the value is shown as usual.
func mcpShortValue(flag byte, v string) (string, bool) {
	lead, core, trail := mcpSplitQuotes(v)
	if mcpShortPlain(flag, core) {
		return "", false
	}
	return lead + mcpShortKeep(flag, core) + mcpMasked + trail, true
}

// mcpShortKeep is the part of a -u value that stays: the name and its colon.
func mcpShortKeep(flag byte, core string) string {
	if name, _, ok := strings.Cut(core, ":"); ok && flag == 'u' && name != "" {
		return name + ":"
	}
	return ""
}

// The word before a sign-in value, as in "Bearer <value>": the piece after
// it is hidden as well.
func mcpSchemeWord(core string) bool {
	switch strings.ToLower(core) {
	case "bearer", "basic", "token", "digest", "apikey", "key":
		return true
	}
	return false
}

// line handles a string that may hold several space-separated pieces, such
// as a command with inline arguments or a shell line.
func (s *mcpArgScanner) line(text string, start bool) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		j := i
		for j < len(text) && isMcpSpace(text[j]) {
			j++
		}
		end := j
		for end < len(text) && !isMcpSpace(text[end]) {
			end++
		}
		if s.spaceNext && s.closer == 0 {
			// The rest of a hidden value after a backslash and a space.
			if s.spaceNext = mcpEndsEscaped(text[j:end]); !s.spaceNext {
				_, _, trail := mcpSplitQuotes(text[j:end])
				b.WriteString(trail)
			}
		} else if s.closer != 0 {
			// Still inside a hidden quoted value: show nothing of it, and
			// only the closing quote marks of the piece that ends it.
			if _, open := mcpQuoteState(text[j:end], s.closer, s.closerEsc); !open {
				s.closer = 0
				_, _, trail := mcpSplitQuotes(text[j:end])
				b.WriteString(trail)
			}
		} else {
			b.WriteString(text[i:j])
			if j < end {
				s.atStart = start || s.prevC
				start = false
				b.WriteString(s.piece(text[j:end]))
				_, core, _ := mcpSplitQuotes(text[j:end])
				s.prevC = core == "-c" || core == "/c" || core == "/C"
			}
		}
		i = end
	}
	return b.String()
}

func (s *mcpArgScanner) piece(p string) string {
	lead, core, trail := mcpSplitQuotes(p)
	if s.maskNext {
		s.chain(core)
		if mcpSchemeWord(core) {
			s.maskNext = true
		}
		return s.hide(p, "")
	}
	if s.headerNext {
		s.headerNext = false
		return s.hideHeader(p)
	}
	if s.shortNext != 0 {
		flag := s.shortNext
		s.shortNext = 0
		if !mcpShortPlain(flag, core) {
			return s.hide(p, mcpShortKeep(flag, core))
		}
	}
	switch {
	case s.atStart && mcpRunnerWord(core):
		s.runner, s.runnerArg = true, false
	case s.runnerArg:
		s.runnerArg = false // the value of the runner's own -p
	case core != "" && core[0] != '-':
		s.runner = false // a plain word: the runner's own options are over
	}
	if shown, ok := mcpDisplayWord(core); ok {
		return lead + s.keep(shown) + trail
	}
	if name, sep, value, ok := mcpAssignment(core); ok {
		value += trail
		switch {
		case mcpHiddenName(name):
			return lead + name + sep + s.hide(value, "")
		case mcpHeaderFlag(name):
			return lead + name + sep + s.hideHeader(value)
		}
		valueLead, valueCore, valueTrail := mcpSplitQuotes(value)
		// "-p=value": the one-letter flags written with their value attached.
		if flag := s.shortFlag(name); flag != 0 && !mcpShortPlain(flag, valueCore) {
			return lead + name + sep + s.hide(value, mcpShortKeep(flag, valueCore))
		}
		if shown, ok := mcpDisplayWord(valueCore); ok {
			return lead + name + sep + valueLead + s.keep(shown) + valueTrail
		}
	}
	if mcpBareFlag(core) {
		switch {
		case mcpHeaderFlag(core):
			s.headerNext = true
		case mcpHiddenName(core):
			s.maskNext = true
		default:
			s.shortNext = s.shortFlag(core)
			s.runnerArg = s.runner && (core == "-p" || core == "--package" || core == "--python")
		}
		return p
	}
	if strings.Contains(p, "?") {
		// An address written without its scheme still has query values.
		return mcpQueryValueRe.ReplaceAllString(p, "${1}"+mcpMasked)
	}
	return p
}

// hide replaces a value piece, keeping its surrounding quote marks and the
// keep text (a header name). When the piece opens a quote it does not close,
// the following pieces are hidden too, up to the closing quote.
func (s *mcpArgScanner) hide(p, keep string) string {
	lead, _, trail := mcpSplitQuotes(p)
	if q, esc := mcpOpenQuote(p); q != 0 {
		s.closer, s.closerEsc = q, esc
		return lead + keep + mcpMasked
	}
	if mcpEndsEscaped(p) {
		s.spaceNext = true
		return lead + keep + mcpMasked
	}
	return lead + keep + mcpMasked + trail
}

// mcpEndsEscaped reports whether a piece ends in an odd run of backslashes,
// which makes the space after it part of the same shell word.
func mcpEndsEscaped(p string) bool {
	n := 0
	for n < len(p) && p[len(p)-1-n] == '\\' {
		n++
	}
	return n%2 == 1
}

// hideHeader hides a header's value and keeps its name: Name: value.
func (s *mcpArgScanner) hideHeader(p string) string {
	_, core, trail := mcpSplitQuotes(p)
	if name, rest, ok := strings.Cut(core, ":"); ok && name != "" {
		if q, _ := mcpOpenQuote(p); rest == "" && trail == "" && q == 0 {
			// "Name: value" written without quotes: the value is the next piece.
			s.maskNext = true
			return p
		}
		return s.hide(p, name+": ")
	}
	return s.hide(p, "")
}

// mcpDisplayWord recognises a piece that has a display form of its own: an
// address (shortened like the url field), a package name with its version,
// or an IP address with or without a port.
func mcpDisplayWord(word string) (string, bool) {
	switch {
	case mcpURLShaped.MatchString(word):
		return mcpDisplayURL(word), true
	case mcpPackageWord(word):
		return word, true
	}
	if _, err := netip.ParseAddr(word); err == nil {
		return word, true
	}
	if _, err := netip.ParseAddrPort(word); err == nil {
		return word, true
	}
	return "", false
}

// A package and its version as npx and similar runners take it:
// name@1.2.3, @scope/name@latest. The parts are short and lower-case, and
// the version is a number or one of the common tags, so nothing else that
// has an "@" in it qualifies.
var mcpPackageRe = regexp.MustCompile(`^((?:@[a-z0-9][a-z0-9._-]{0,30}/)?[a-z0-9][a-z0-9._-]{0,30})@(?:[~^]?v?[0-9]+(?:\.[0-9]+){0,2}(?:[-+][0-9A-Za-z.-]{1,24})?|latest|next|beta|alpha|canary|rc)$`)

// mcpPackageWord also requires that the shared rules find nothing in the
// package name itself.
func mcpPackageWord(word string) bool {
	m := mcpPackageRe.FindStringSubmatch(word)
	return m != nil && feedbackredact.Redact(m[1]) == m[1]
}

// mcpPathSegment is the display form of one path segment of an address. A
// package name with its version stays; a segment that reads as a generated
// value rather than a word is replaced; the shared rules cover the rest.
func mcpPathSegment(seg string) string {
	switch {
	case mcpPackageWord(seg):
		return seg
	case mcpGeneratedSegment(seg):
		return mcpMasked
	}
	return feedbackredact.Redact(seg)
}

var mcpUUIDRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// mcpGeneratedSegment looks at each run of letters and digits in a path
// segment. Words, version numbers and dates do not qualify: a run has to be
// long and mix letters with several digits, or switch case far more often
// than a camelCase name does.
func mcpGeneratedSegment(seg string) bool {
	if mcpUUIDRe.MatchString(seg) {
		return true
	}
	n, digits, letters, flips := 0, 0, 0, 0
	var last byte // 'u' or 'l': case of the previous letter in the run
	generated := func() bool {
		return n >= 16 && digits >= 2 && letters >= 2 ||
			n >= 12 && digits >= 3 && letters >= 3 ||
			n >= 20 && flips >= 6
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		var kind byte
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c >= 'a' && c <= 'z':
			kind = 'l'
		case c >= 'A' && c <= 'Z':
			kind = 'u'
		default:
			if generated() {
				return true
			}
			n, digits, letters, flips, last = 0, 0, 0, 0, 0
			continue
		}
		n++
		if kind != 0 {
			letters++
			if last != 0 && last != kind {
				flips++
			}
			last = kind
		}
	}
	return generated()
}

var mcpURLShaped = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
var mcpNameRe = regexp.MustCompile(`^-{0,2}[A-Za-z_][A-Za-z0-9_.-]*$`)
var mcpQueryValueRe = regexp.MustCompile(`([?&][^=&\s#"']*=)[^&\s#"']*`)

// mcpAssignment splits "name=value". A flag (leading dash) may also use a
// colon, as in "--flag:value". The name must look like a flag or variable
// name, so ordinary text containing "=" is left to the other rules.
func mcpAssignment(p string) (name, sep, value string, ok bool) {
	seps := "="
	if strings.HasPrefix(p, "-") {
		seps = "=:"
	}
	i := strings.IndexAny(p, seps)
	if i <= 0 || !mcpNameRe.MatchString(p[:i]) {
		return "", "", "", false
	}
	return p[:i], p[i : i+1], p[i+1:], true
}

// mcpHeaderValue keeps a header's name and hides what follows the colon.
// The argument is one complete value.
func mcpHeaderValue(v string) string {
	lead, core, trail := mcpSplitQuotes(v)
	if name, _, ok := strings.Cut(core, ":"); ok && name != "" && !strings.ContainsAny(name, " \t") {
		return lead + name + ": " + mcpMasked + trail
	}
	return mcpMasked
}

func isMcpSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isMcpQuoteMark(c byte) bool { return c == '"' || c == '\'' || c == '\\' }

// mcpSplitQuotes separates the quote marks and backslashes around a piece
// from its content.
func mcpSplitQuotes(p string) (lead, core, trail string) {
	i := 0
	for i < len(p) && isMcpQuoteMark(p[i]) {
		i++
	}
	j := len(p)
	for j > i && isMcpQuoteMark(p[j-1]) {
		j--
	}
	return p[:i], p[i:j], p[j:]
}

// mcpQuoteState reads p with a quote already open (q, or 0 for none) and
// reports the quote left open at its end. A quote opened plainly closes on
// the same plain mark. One opened with a backslash closes on the same mark
// written either way: a plain one means the enclosing quote ended first.
func mcpQuoteState(p string, q byte, esc bool) (byte, bool) {
	slashes := 0
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' {
			slashes++
			continue
		}
		escaped := slashes%2 == 1
		slashes = 0
		if c != '"' && c != '\'' {
			continue
		}
		switch {
		case q == 0:
			q, esc = c, escaped
		case c == q && (esc || !escaped):
			q, esc = 0, false
		}
	}
	return q, q != 0
}

// mcpOpenQuote returns the quote a value piece leaves open. Closing marks at
// the very end that have no partner inside the piece belong to an enclosing
// quote and do not open anything.
func mcpOpenQuote(p string) (byte, bool) {
	q, open := mcpQuoteState(p, 0, false)
	if !open {
		return 0, false
	}
	lead, core, _ := mcpSplitQuotes(p)
	q, open = mcpQuoteState(lead+core, 0, false)
	if !open {
		return 0, false
	}
	// Report how the opening mark was written.
	_, esc := mcpFirstQuote(lead + core)
	return q, esc
}

// mcpFirstQuote finds the first quote mark and whether a backslash precedes it.
func mcpFirstQuote(p string) (byte, bool) {
	slashes := 0
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\\':
			slashes++
		case '"', '\'':
			return c, slashes%2 == 1
		default:
			slashes = 0
		}
	}
	return 0, false
}

func mcpBareFlag(p string) bool {
	return len(p) > 1 && p[0] == '-' && !strings.ContainsAny(p, "=:")
}

func mcpHeaderFlag(name string) bool {
	switch strings.ToLower(name) {
	case "-h", "--header", "--headers", "-header", "--http-header":
		// "-h" is help for most tools and then takes no value; hiding the
		// piece after it costs nothing.
		return true
	}
	return false
}

var mcpHiddenParts = []string{
	"key", "token", "secret", "password", "passwd", "passphrase", "pwd",
	"auth", "credential", "bearer", "cookie", "session", "signature", "jwt",
}

var mcpHiddenWords = map[string]bool{
	"pat": true, "pw": true, "pass": true, "sig": true, "sid": true,
	"dsn": true, "cred": true, "creds": true,
}

// mcpHiddenName reports whether a flag or variable name is the kind whose
// value is never shown. It errs towards hiding: a harmless "--keyboard us"
// loses its value, which only shortens the row.
func mcpHiddenName(name string) bool {
	n := strings.ToLower(strings.TrimLeft(name, "-"))
	if n == "" {
		return false
	}
	for _, part := range mcpHiddenParts {
		if strings.Contains(n, part) {
			return true
		}
	}
	for _, word := range strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if mcpHiddenWords[word] {
			return true
		}
	}
	return false
}
