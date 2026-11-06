// Package ladder is the decision core of the merge ladder: given what GitHub says
// about a pull request or an issue, it says what work, if any, is due. Every function
// here is pure: no network, no clock, no files. The poller and the publishers gather
// the data and act on the answer.
//
// State lives on GitHub and nowhere else: marker comments, commit statuses and
// labels. Everything read from GitHub is data, and a marker, a status or a label
// event only counts when the right account wrote it. A stranger who pastes a marker
// into a comment changes nothing.
package ladder

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The commit status contexts the ladder writes. Rulesets and gh-dash filters name
// them, so they never change.
const (
	// StatusVerdict carries the reviewer's verdict on a head commit.
	StatusVerdict = "review / verdict"
	// StatusReady is green when every required check is green and the verdict at the
	// head is ready or nits.
	StatusReady = "ladder / ready"
	// StatusStuck is set when the fix loop has used its rounds and the verdict is
	// still changes.
	StatusStuck = "ladder / stuck"
)

// MaxFixRounds is how many times a builder answers a review before the pull request
// is handed to the maintainer.
const MaxFixRounds = 2

// Verdict is the reviewer's conclusion on one head commit.
type Verdict string

// The verdicts of the reviewer's brief, and Held, which only the review poster
// writes: the review ran, and its comment was kept back for the maintainer.
const (
	VerdictReady   Verdict = "ready"
	VerdictNits    Verdict = "nits"
	VerdictChanges Verdict = "changes"
	VerdictBlocked Verdict = "blocked"
	VerdictSkipped Verdict = "skipped"
	VerdictHeld    Verdict = "held"
)

// ModelVerdicts are the verdicts a reviewer may write in verdict.json.
var ModelVerdicts = []Verdict{VerdictReady, VerdictNits, VerdictChanges, VerdictBlocked, VerdictSkipped}

// ParseVerdict returns the verdict a word names, or false.
func ParseVerdict(word string) (Verdict, bool) {
	switch v := Verdict(word); v {
	case VerdictReady, VerdictNits, VerdictChanges, VerdictBlocked, VerdictSkipped, VerdictHeld:
		return v, true
	}
	return "", false
}

// StatusState is the commit status state that records a verdict: success lets the
// pull request go on to ready, failure asks for changes, and error means the review
// reached no conclusion.
func (v Verdict) StatusState() string {
	switch v {
	case VerdictReady, VerdictNits:
		return "success"
	case VerdictChanges:
		return "failure"
	default:
		return "error"
	}
}

// StatusDescription is the description of a verdict's commit status. Its first word
// is the verdict, which is what VerdictFromStatus reads back; detail is the
// publisher's own words, never the model's.
func (v Verdict) StatusDescription(detail string) string {
	if detail == "" {
		return string(v)
	}
	return string(v) + ": " + detail
}

// VerdictFromStatus reads a verdict back from a status description.
func VerdictFromStatus(description string) (Verdict, bool) {
	word, _, _ := strings.Cut(description, ":")
	return ParseVerdict(strings.TrimSpace(word))
}

var (
	shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// A marker is an HTML comment, so GitHub shows nothing of it. The fields are
	// key=value words; unknown keys are ignored when reading, so a later field does
	// not break an older poller.
	reviewMarkerRE = regexp.MustCompile(`<!--\s*twr-review((?:\s+[a-z_]+=[^\s>]+)+)\s*-->`)
	fixMarkerRE    = regexp.MustCompile(`<!--\s*twr-fix\s+round=([1-9])\s*-->`)
	// AnyMarkerRE finds anything that looks like one of the ladder's markers, for the
	// publishers, which refuse model output that carries a marker it should not.
	AnyMarkerRE = regexp.MustCompile(`<!--\s*twr-[a-z]+\b[^>]*-->`)
)

// IsSHA reports whether s is a full, lower-case commit SHA.
func IsSHA(s string) bool { return shaRE.MatchString(s) }

// ReviewMarker is the marker that ends a review comment: which head was reviewed,
// the kind of review (A to D), the lane, and the verdict. The reviewer writes the
// first three; the poster adds the verdict when it posts.
type ReviewMarker struct {
	SHA     string
	Kind    string
	Lane    string
	Verdict Verdict
}

// String is the marker as it is written in a comment.
func (m ReviewMarker) String() string {
	s := fmt.Sprintf("<!-- twr-review sha=%s kind=%s lane=%s", m.SHA, m.Kind, m.Lane)
	if m.Verdict != "" {
		s += " verdict=" + string(m.Verdict)
	}
	return s + " -->"
}

// ParseReviewMarkers returns every well-formed review marker in a comment body. A
// marker without a full SHA is not one.
func ParseReviewMarkers(body string) []ReviewMarker {
	var markers []ReviewMarker
	for _, match := range reviewMarkerRE.FindAllStringSubmatch(body, -1) {
		var m ReviewMarker
		for _, field := range strings.Fields(match[1]) {
			key, value, _ := strings.Cut(field, "=")
			switch key {
			case "sha":
				m.SHA = value
			case "kind":
				m.Kind = value
			case "lane":
				m.Lane = value
			case "verdict":
				if v, ok := ParseVerdict(value); ok {
					m.Verdict = v
				}
			}
		}
		if IsSHA(m.SHA) {
			markers = append(markers, m)
		}
	}
	return markers
}

// FixMarker is the marker that ends a builder's answer to a review.
func FixMarker(round int) string { return fmt.Sprintf("<!-- twr-fix round=%d -->", round) }

// ParseFixRounds returns the round of every fix marker in a comment body.
func ParseFixRounds(body string) []int {
	var rounds []int
	for _, match := range fixMarkerRE.FindAllStringSubmatch(body, -1) {
		round, _ := strconv.Atoi(match[1]) // one digit, by the pattern
		rounds = append(rounds, round)
	}
	return rounds
}

// sameLogin compares two GitHub logins, which ignore case.
func sameLogin(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

func hasLogin(list []string, login string) bool {
	for _, item := range list {
		if sameLogin(item, login) {
			return true
		}
	}
	return false
}

func hasLabel(labels []string, name string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, name) {
			return true
		}
	}
	return false
}
