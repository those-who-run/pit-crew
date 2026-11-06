// Package privatescan finds values that must not be posted in public: an address, a
// host name or a cluster context that belongs to one deployment, a home path, a
// credential. The publishers run it over everything a model wrote before any of it
// reaches GitHub, and hold what fails for the maintainer.
//
// The address, host and context rules are a port of scripts/private_scan.py in
// those-who-run/paddock, which scans that repository's files; the cases of its tests
// are ported in privatescan_test.go. Its fourth rule, `bootstrap`, is about NATS
// stream definitions in paddock's own files and has no meaning for a comment, so it
// is left out. Two rules are added for text a model writes: `home-path` and
// `secret`.
//
// What is private to one deployment cannot be listed in a public repository, so a
// job supplies its own patterns at run time (ParseExtra): each has a public name,
// which is all a log or a status ever shows of it.
package privatescan

import (
	_ "embed" // the default allowlist
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

//go:embed default.allow
var defaultAllow string

// Finding is one private value found.
type Finding struct {
	// Rule is `address`, `host`, `context`, `home-path`, `secret`, or the name of an
	// extra pattern.
	Rule string
	// Line is the line it is on, from 1.
	Line int
	// Value is what was found. It is private: write it only where the maintainer
	// alone reads, never to a log, a status or a comment.
	Value string
}

// String names the rule and the line, and not the value, so it is safe to log.
func (f Finding) String() string { return fmt.Sprintf("%s: line %d", f.Rule, f.Line) }

// Allowlist is the public hosts the scan accepts.
type Allowlist struct {
	hosts []string
}

var (
	hostLabel   = `[a-z0-9](?:[a-z0-9-]*[a-z0-9])?`
	allowHostRE = regexp.MustCompile(`^(?:` + hostLabel + `\.)+` + hostLabel + `$`)
)

// DefaultAllowlist returns the hosts in default.allow.
func DefaultAllowlist() *Allowlist {
	allow, err := (&Allowlist{}).Add(defaultAllow, "default.allow")
	if err != nil {
		panic(err) // the embedded file is checked by the tests
	}
	return allow
}

// Add returns the allowlist with the entries of text added, in paddock's format: a
// host, then the reason it is public. An entry with no reason, one that is not a
// host name, and a home network's name are refused, since a host only one network
// knows is never public.
func (a *Allowlist) Add(text, where string) (*Allowlist, error) {
	out := &Allowlist{hosts: append([]string(nil), a.hosts...)}
	var problems []string
	for n, line := range splitLines(text) {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		parts := strings.Fields(line)
		host := strings.ToLower(parts[0])
		switch {
		case len(parts) < 2:
			problems = append(problems, fmt.Sprintf("%s:%d: %s has no reason", where, n+1, host))
		case !allowHostRE.MatchString(host):
			problems = append(problems, fmt.Sprintf("%s:%d: %s is not a host name", where, n+1, host))
		case isHomeTLD(host[strings.LastIndex(host, ".")+1:]):
			problems = append(problems, fmt.Sprintf("%s:%d: %s is a home network's name, never a public host", where, n+1, host))
		default:
			out.hosts = append(out.hosts, host)
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("allowlist: %s", strings.Join(problems, "; "))
	}
	return out, nil
}

func (a *Allowlist) allows(host string) bool {
	if a == nil {
		return false
	}
	for _, entry := range a.hosts {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// Extra is one pattern a job supplies at run time.
type Extra struct {
	// Name is public: it is what a log or a status shows when the pattern matches.
	Name string
	re   *regexp.Regexp
}

var extraNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ParseExtra reads a job's own patterns: one per line, a name (lower-case letters,
// digits and hyphens), then a regular expression in Go's syntax, matched without
// case. Blank lines and lines starting with # are skipped. A line that cannot be
// read is an error, so a typo stops the job rather than letting a value through.
func ParseExtra(text, where string) ([]Extra, error) {
	var extras []Extra
	for n, line := range splitLines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, pattern, ok := strings.Cut(line, " ")
		pattern = strings.TrimSpace(pattern)
		if !ok || pattern == "" || !extraNameRE.MatchString(name) {
			return nil, fmt.Errorf("%s:%d: want a name, a space, then a pattern", where, n+1)
		}
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			// The pattern is private, so the error names the line and not the text.
			return nil, fmt.Errorf("%s:%d: pattern %s does not compile", where, n+1, name)
		}
		extras = append(extras, Extra{Name: name, re: re})
	}
	return extras, nil
}

// Scanner holds what a scan is checked against.
type Scanner struct {
	// Allow is the public hosts accepted; nil accepts none.
	Allow *Allowlist
	// Extra is the job's own patterns.
	Extra []Extra
	// Code is set when the text is source code and not prose. A dotted name in code
	// is a selector (`time.Local`, `cfg.Internal`) far more often than a host, so
	// only the host of a URL and the host after `user@` are read as hosts there.
	Code bool
}

// Scan returns every finding in text, in line order.
func (s Scanner) Scan(text string) []Finding {
	var findings []Finding
	for n, line := range splitLines(text) {
		// A \n or \t escape in a string is a separator, not the start of a name.
		line = escapeRE.ReplaceAllString(line, " ")
		add := func(rule string, values []string) {
			for _, value := range values {
				findings = append(findings, Finding{Rule: rule, Line: n + 1, Value: value})
			}
		}
		add("address", addresses(line))
		add("host", hosts(line, s.Allow, !s.Code))
		add("context", contexts(line))
		add("home-path", homePaths(line))
		add("secret", secrets(line))
		for _, extra := range s.Extra {
			add(extra.Name, extra.re.FindAllString(line, -1))
		}
	}
	return findings
}

var escapeRE = regexp.MustCompile(`\\[nrt]`)

// splitLines splits as Python's str.splitlines does, which is what the reference
// numbers its lines by.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '\r':
			lines = append(lines, text[start:i])
			if strings.HasPrefix(text[i+size:], "\n") {
				size++
			}
			start = i + size
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, text[start:i])
			start = i + size
		}
		i += size
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// isWord is Python's \w for text: a letter, a digit or an underscore, in any script.
func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func isASCIIAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// before returns the rune that ends at i, or false at the start of the line.
func before(line string, i int) (rune, bool) {
	if i <= 0 {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(line[:i])
	return r, true
}

// after returns the rune that starts at i, or false at the end of the line.
func after(line string, i int) (rune, bool) {
	if i >= len(line) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(line[i:])
	return r, true
}

// The documentation ranges (RFC 5737) and loopback name no real machine; 0.0.0.0 is
// "every interface" in a bind address.
var allowedNetworks = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("0.0.0.0/32"),
}

var (
	ipv4AtRE   = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}`)
	ipv4FullRE = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}$`)
)

// addresses returns the IPv4 addresses on a line that are not documentation,
// loopback or 0.0.0.0. Four dot-separated numbers that are part of a longer dotted
// run (a version such as v0.11.0.1, or 1.2.3.4.5) are not an address.
func addresses(line string) []string {
	var found []string
	for i := 0; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			continue
		}
		if r, ok := before(line, i); ok && (isWord(r) || r == '.') {
			continue
		}
		match := ipv4AtRE.FindString(line[i:])
		if match == "" {
			continue
		}
		end := i + len(match)
		if r, ok := after(line, end); ok && (isWord(r) || r == '.' && end+1 < len(line) && line[end+1] >= '0' && line[end+1] <= '9') {
			continue
		}
		i = end - 1
		addr, err := netip.ParseAddr(match)
		if err != nil {
			continue // an octet over 255: a version or an OID, not an address
		}
		if !inAllowedNetwork(addr) {
			found = append(found, match)
		}
	}
	return found
}

func inAllowedNetwork(addr netip.Addr) bool {
	for _, network := range allowedNetworks {
		if network.Contains(addr) {
			return true
		}
	}
	return false
}

// Names that are reserved and can never be a real host: RFC 2606's example.* and
// .example, .invalid and .test; RFC 6761's localhost; and cluster.local, the
// in-cluster names. Not .local: mDNS gives a home network's machines their real
// names there.
var placeholderSuffixes = []string{
	"example.com", "example.net", "example.org", "example", "invalid", "test", "localhost", "cluster.local",
}

// The top-level domains a bare dotted name is read as a host for: the common generic
// ones, the country codes most likely here, and the suffixes home networks use. Not
// .sh, .py, .md or .go, which are file names far more often than hosts, nor .app,
// .page, .site, .build, .me or .info, which end code as often. A URL's host is read
// whatever its top-level domain.
var (
	publicTLDs = strings.Fields("com net org io dev ai cloud co biz xyz tech online eu uk ie de fr nl es it us ca au nz")
	// The suffixes of names inside a home or company network: never a public host,
	// so never in an allowlist either.
	homeTLDs = strings.Fields("lan home homelab lab box private local internal intranet corp localdomain arpa")
)

func isHomeTLD(tld string) bool {
	for _, home := range homeTLDs {
		if tld == home {
			return true
		}
	}
	return false
}

var (
	// A URL's host: after the scheme and any user:password@, up to the port or path.
	urlHostRE = regexp.MustCompile("(?i)\\b[a-z][a-z0-9+.-]*://(?:[^\\s/@'\"`<>]*@)?([^\\s:/?#'\"`<>()\\[\\],;|\\\\]+)")
	// A whole bare host: labels, then one of the top-level domains above.
	bareHostRE = regexp.MustCompile(`(?i)^(?:` + hostLabel + `\.)+(?:` + strings.Join(append(append([]string{}, publicTLDs...), homeTLDs...), "|") + `)$`)
	// A whole host after `user@`: the last label is letters, so `image@sha256:` and
	// `tool@v1.2.3` are not hosts.
	userHostRE = regexp.MustCompile(`(?i)^(?:[a-z0-9-]+\.)+[a-z]{2,}$`)
)

// hostRun returns the end of the run of host characters (letters, digits, dots and
// hyphens) that starts at i.
func hostRun(line string, i int) int {
	end := i
	for end < len(line) && (isASCIIAlnum(line[end]) || line[end] == '.' || line[end] == '-') {
		end++
	}
	return end
}

// hostEnding finds the longest prefix of line[start:run] that is a whole host by
// full, and that ends where a host may: not before a word character or one of
// stops, and not before a dot that goes on into another label. It is how the
// reference's look-ahead reads, longest match first.
func hostEnding(line string, start, run int, full *regexp.Regexp, stops string) int {
	for end := run; end > start; end-- {
		if end < run {
			// Inside the run the next character is a letter, a digit, a hyphen or a
			// dot; only a dot that ends the name may follow a host.
			if line[end] != '.' || end+1 < len(line) && isASCIIAlnum(line[end+1]) {
				continue
			}
		} else if r, ok := after(line, end); ok && (isWord(r) || strings.ContainsRune(stops, r)) {
			continue
		}
		if full.MatchString(line[start:end]) {
			return end
		}
	}
	return -1
}

// bareHostStart reports whether a bare host may start at i: after a character that
// is not part of a name, or after the dot of `*.name` or of a leading `.name` (a
// NO_PROXY entry or a cookie domain).
func bareHostStart(line string, i int) bool {
	prev, ok := before(line, i)
	if !ok {
		return true
	}
	if prev != '.' {
		return !isWord(prev) && prev != '-'
	}
	if i == 1 {
		return true
	}
	lead, _ := before(line, i-1)
	return strings.ContainsRune("*\"'`=,([", lead) || unicode.IsSpace(lead)
}

// hosts returns the host names on a line that are not placeholders, in-cluster or
// allowed. A host is found three ways: the host of any URL, the host after `user@`,
// and, when bare is set, any other dotted name that ends in one of the top-level
// domains above.
func hosts(line string, allow *Allowlist, bare bool) []string {
	names := findAtWordStart(urlHostRE, line, func(string) bool { return true })
	for i, last := 1, 0; i < len(line); i++ {
		if line[i] != '@' || i-1 < last {
			continue
		}
		if prev, _ := before(line, i); !isWord(prev) && !strings.ContainsRune(".+-", prev) {
			continue
		}
		if end := hostEnding(line, i+1, hostRun(line, i+1), userHostRE, "-"); end > 0 {
			names = append(names, line[i+1:end])
			last = end
		}
	}
	for i := 0; bare && i < len(line); i++ {
		if !isASCIIAlnum(line[i]) || !bareHostStart(line, i) {
			continue
		}
		if end := hostEnding(line, i, hostRun(line, i), bareHostRE, "(-"); end > 0 {
			names = append(names, line[i:end])
			i = end - 1
		}
	}
	var found []string
	for _, name := range names {
		host := strings.TrimRight(strings.ToLower(name), ".")
		// No dot is a Service or localhost; $ and { are a template.
		if contains(found, host) || !strings.Contains(host, ".") || strings.ContainsAny(host, "${") {
			continue
		}
		if ipv4FullRE.MatchString(host) || placeholder(host) || allow.allows(host) {
			continue
		}
		found = append(found, host)
	}
	return found
}

func contains(list []string, item string) bool {
	for _, have := range list {
		if have == item {
			return true
		}
	}
	return false
}

func placeholder(host string) bool {
	for _, suffix := range placeholderSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

var (
	// A cluster context: after `--context` or `--context=`, `use-context`,
	// `current-context:`.
	contextRE = regexp.MustCompile("(?:--context[= ]|\\buse-context\\s|\\bcurrent-context:\\s)\\s*[\"']?([^\\s\"'`]+)")
	// Context values that name no cluster: the docs' placeholders and templates.
	contextPlaceholderRE = regexp.MustCompile(`^(?:my-[\p{L}\p{N}_-]+|<[^>]*>?|[$%{].*|\\?\$.*|example[\p{L}\p{N}_-]*)$`)
)

// contexts returns the cluster contexts on a line that are not placeholders.
func contexts(line string) []string {
	var found []string
	// `--context` needs no word boundary before it; the other two forms do.
	bounded := func(match string) bool { return !strings.HasPrefix(match, "--") }
	for _, value := range findAtWordStart(contextRE, line, bounded) {
		if !contextPlaceholderRE.MatchString(value) {
			found = append(found, value)
		}
	}
	return found
}

// findAtWordStart returns the first group of each match of re in line, left to
// right without overlap. Go's \b knows only ASCII letters, and the reference's
// knows every script's, so a match that bounded says needs a word boundary is
// dropped when a letter of any script comes just before it, and the search goes on
// from the next character.
func findAtWordStart(re *regexp.Regexp, line string, bounded func(match string) bool) []string {
	var groups []string
	for pos := 0; pos < len(line); {
		loc := re.FindStringSubmatchIndex(line[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		if prev, ok := before(line, start); ok && isWord(prev) && bounded(line[start:end]) {
			pos = start + 1
			continue
		}
		groups = append(groups, line[pos+loc[2]:pos+loc[3]])
		pos = end
	}
	return groups
}

var (
	homePathRE = regexp.MustCompile(`(?:/home|/Users)/([A-Za-z0-9._-]+)`)
	// Home directories that name no person: a placeholder, or the user an image or a
	// CI runner is built with.
	homePlaceholders = []string{"user", "username", "you", "me", "nonroot", "runner", "agent"}
)

// homePaths returns the home directories on a line that name a person's account.
func homePaths(line string) []string {
	var found []string
	for _, match := range homePathRE.FindAllStringSubmatch(line, -1) {
		if !contains(homePlaceholders, strings.ToLower(match[1])) {
			found = append(found, match[0])
		}
	}
	return found
}

// The shapes of credentials a job could hold or a model could have read: GitHub's
// token prefixes, Anthropic's, Slack's, and a PEM private key. gitleaks in CI knows
// many more; this is the net before the push, not instead of it.
var secretRE = regexp.MustCompile(
	`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,})` +
		`|-----BEGIN [A-Z ]*PRIVATE KEY-----`)

func secrets(line string) []string { return secretRE.FindAllString(line, -1) }
