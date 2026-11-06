// Package publish holds the checks a publish step makes on what an agent wrote,
// before any of it reaches GitHub. The functions here are pure: they are given the
// files and the facts, and return what to post or why not. The model that wrote the
// files had no GitHub token; the step that runs these checks has no model.
//
// A refusal has a code and a detail. The code is one stable word and is safe in
// public: it is what a commit status and a log say. The detail can quote what was
// found, a private value included, and goes only to the hold folder, which the
// maintainer alone reads.
package publish

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/those-who-run/pit-crew/internal/ladder"
	"github.com/those-who-run/pit-crew/internal/privatescan"
)

// MaxCommentBytes is the largest comment a publisher posts: 15 KB, as the
// reviewer's brief says.
const MaxCommentBytes = 15 * 1024

// MaxBodyBytes is the largest pull request body a publisher posts. GitHub's own
// limit is 65,536 characters.
const MaxBodyBytes = 60 * 1024

// Refusal is one reason a publisher will not post.
type Refusal struct {
	// Code is a stable, public word for the reason.
	Code string
	// Detail says what was found. It may hold a private value.
	Detail string
}

func refuse(code, format string, args ...any) Refusal {
	return Refusal{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Codes returns the distinct codes of refusals, in order.
func Codes(refusals []Refusal) []string {
	var codes []string
	seen := map[string]bool{}
	for _, r := range refusals {
		if !seen[r.Code] {
			seen[r.Code] = true
			codes = append(codes, r.Code)
		}
	}
	return codes
}

// scanText runs the private-value scan over one piece of text a model wrote.
func scanText(scanner privatescan.Scanner, what, text string) []Refusal {
	var refusals []Refusal
	for _, finding := range scanner.Scan(text) {
		refusals = append(refusals, refuse("private-scan", "%s: %s: %s", what, finding, finding.Value))
	}
	return refusals
}

var (
	fenceRE      = regexp.MustCompile("(?m)^\\s*(?:```|~~~)")
	inlineCodeRE = regexp.MustCompile("`[^`\n]*`")
	// An @name as GitHub reads one: at the start of a word, a login after it.
	mentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./@-])(@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:/[A-Za-z0-9._-]+)?)`)
)

// mentions returns the @names in Markdown that GitHub would notify: those outside
// code blocks and code spans. A publisher posts as a bot, and a bot that can be
// steered into naming people is a way to reach them.
func mentions(markdown string) []string {
	var prose strings.Builder
	fenced := false
	for _, line := range strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n") {
		if fenceRE.MatchString(line) {
			fenced = !fenced
			continue
		}
		if !fenced {
			prose.WriteString(inlineCodeRE.ReplaceAllString(line, " "))
			prose.WriteByte('\n')
		}
	}
	var found []string
	for _, match := range mentionRE.FindAllStringSubmatch(prose.String(), -1) {
		found = append(found, match[1])
	}
	return found
}

// checkProse makes the checks every piece of Markdown a publisher posts must pass:
// it is UTF-8, within its size, names nobody, and holds no private value.
func checkProse(scanner privatescan.Scanner, what, text string, limit int) []Refusal {
	var refusals []Refusal
	if !utf8.ValidString(text) {
		return []Refusal{refuse(what+"-invalid", "%s is not UTF-8", what)}
	}
	if len(text) > limit {
		refusals = append(refusals, refuse(what+"-too-large", "%s is %d bytes; the limit is %d", what, len(text), limit))
	}
	if found := mentions(text); len(found) > 0 {
		refusals = append(refusals, refuse("mention", "%s names %s; a publisher notifies nobody", what, strings.Join(found, ", ")))
	}
	return append(refusals, scanText(scanner, what, text)...)
}

// lastLine returns the last line of text that is not blank, and the text before it.
func lastLine(text string) (line, rest string) {
	trimmed := strings.TrimRight(text, " \t\r\n")
	if i := strings.LastIndexByte(trimmed, '\n'); i >= 0 {
		return strings.TrimRight(trimmed[i+1:], "\r"), trimmed[:i+1]
	}
	return trimmed, ""
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimLeft(text, "\r\n"), "\n")
	return strings.TrimRight(line, "\r")
}

func countMarkers(text string) int { return len(ladder.AnyMarkerRE.FindAllString(text, -1)) }
