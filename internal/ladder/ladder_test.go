package ladder

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	head     = "1f0c5d2e9a7b4c3d8e6f1a2b3c4d5e6f7a8b9c0d"
	oldHead  = "0000000000000000000000000000000000000001"
	reviewer = "twr-reviewer[bot]"
	builder  = "twr-builder[bot]"
	owner    = "octo-maintainer"
)

// at is a time some minutes into the fixture's day.
func at(minutes int) time.Time {
	return time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
}

func TestReviewMarkerRoundTrip(t *testing.T) {
	cases := []ReviewMarker{
		{SHA: head, Kind: "A", Lane: "standard"},
		{SHA: head, Kind: "D", Lane: "sensitive", Verdict: VerdictChanges},
	}
	for _, marker := range cases {
		got := ParseReviewMarkers("Review at head `1f0c5d2e`: **ready**.\n\n" + marker.String() + "\n")
		if !reflect.DeepEqual(got, []ReviewMarker{marker}) {
			t.Errorf("%s parsed as %v", marker, got)
		}
	}
}

func TestParseReviewMarkers(t *testing.T) {
	cases := []struct {
		name, body string
		want       []ReviewMarker
	}{
		{"the brief's form", "<!-- twr-review sha=" + head + " kind=A lane=light -->",
			[]ReviewMarker{{SHA: head, Kind: "A", Lane: "light"}}},
		{"the plan's short form", "<!-- twr-review sha=" + head + " -->", []ReviewMarker{{SHA: head}}},
		{"no spaces inside the comment", "<!--twr-review sha=" + head + "-->", []ReviewMarker{{SHA: head}}},
		{"an unknown field is ignored", "<!-- twr-review sha=" + head + " model=x -->", []ReviewMarker{{SHA: head}}},
		{"an unknown verdict is dropped", "<!-- twr-review sha=" + head + " verdict=lgtm -->", []ReviewMarker{{SHA: head}}},
		{"two markers", "<!-- twr-review sha=" + head + " -->\n<!-- twr-review sha=" + oldHead + " -->",
			[]ReviewMarker{{SHA: head}, {SHA: oldHead}}},
		{"a short SHA is not a marker", "<!-- twr-review sha=1f0c5d2e -->", nil},
		{"an upper-case SHA is not a marker", "<!-- twr-review sha=" + strings.ToUpper(head) + " -->", nil},
		{"no SHA", "<!-- twr-review kind=A -->", nil},
		{"another marker", "<!-- twr-fix round=1 -->", nil},
		{"prose about a marker", "the marker is twr-review sha=" + head, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseReviewMarkers(c.body); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestFixMarkers(t *testing.T) {
	if got := FixMarker(2); got != "<!-- twr-fix round=2 -->" {
		t.Errorf("marker %q", got)
	}
	cases := []struct {
		body string
		want []int
	}{
		{"Answer to the review at `1f0c5d2e`:\n\n1. Fixed.\n\n" + FixMarker(1), []int{1}},
		{FixMarker(1) + FixMarker(2), []int{1, 2}},
		{"<!-- twr-fix round=0 -->", nil},
		{"<!-- twr-fix round=12 -->", nil},
		{"<!-- twr-fix -->", nil},
		{"twr-fix round=1", nil},
	}
	for _, c := range cases {
		if got := ParseFixRounds(c.body); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: rounds %v, want %v", c.body, got, c.want)
		}
	}
}

func TestAnyMarker(t *testing.T) {
	for body, want := range map[string]bool{
		FixMarker(1):                       true,
		"<!-- twr-review sha=x -->":        true,
		"<!--twr-ready-->":                 true,
		"<!-- a note about twr-review -->": false,
		"twr-review":                       false,
	} {
		if got := AnyMarkerRE.MatchString(body); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
}

func TestVerdictStatus(t *testing.T) {
	cases := []struct {
		verdict Verdict
		state   string
	}{
		{VerdictReady, "success"}, {VerdictNits, "success"}, {VerdictChanges, "failure"},
		{VerdictBlocked, "error"}, {VerdictSkipped, "error"}, {VerdictHeld, "error"},
	}
	for _, c := range cases {
		if got := c.verdict.StatusState(); got != c.state {
			t.Errorf("%s: state %q, want %q", c.verdict, got, c.state)
		}
		for _, detail := range []string{"", "2 asked for"} {
			back, ok := VerdictFromStatus(c.verdict.StatusDescription(detail))
			if !ok || back != c.verdict {
				t.Errorf("%s with %q read back as %q, %v", c.verdict, detail, back, ok)
			}
		}
	}
	for _, description := range []string{"", "looks good", "ready to go", "approved: yes"} {
		if v, ok := VerdictFromStatus(description); ok {
			t.Errorf("%q read as verdict %q", description, v)
		}
	}
}

func TestIssueRefs(t *testing.T) {
	cases := []struct {
		name, body string
		want       []IssueRef
	}{
		{"closes, as the last line", "## What changed\n\nx\n\nCloses those-who-run/paddock#232.\n",
			[]IssueRef{{"those-who-run/paddock", 232, true}}},
		{"part of, with what is left", "Part of those-who-run/paddock#41 (AC3, checked on the car, stays open).",
			[]IssueRef{{"those-who-run/paddock", 41, false}}},
		{"CRLF", "x\r\nCloses those-who-run/paddock#7.\r\n", []IssueRef{{"those-who-run/paddock", 7, true}}},
		{"two issues, one twice", "Closes a/b#1.\nPart of a/c#2\nCloses A/B#1.",
			[]IssueRef{{"a/b", 1, true}, {"a/c", 2, false}}},
		{"a short reference names no repository", "Closes #12.", nil},
		{"mid-line is prose, not the line", "This closes nothing. See Closes those-who-run/paddock#9.", nil},
		{"quoted in a block quote is not the line", "> Closes those-who-run/paddock#9.", nil},
		{"issue zero", "Closes those-who-run/paddock#0.", nil},
		{"no issue", "No issue: planned in the maintainer's notes.", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IssueRefs(c.body); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	if !RefersTo("Closes Those-Who-Run/Paddock#232.", "those-who-run/paddock", 232) {
		t.Error("RefersTo is case-sensitive on the repository")
	}
	if RefersTo("Closes those-who-run/paddock#2321.", "those-who-run/paddock", 232) {
		t.Error("RefersTo matched a longer number")
	}
}

func TestSlug(t *testing.T) {
	for title, want := range map[string]string{
		"Fuel callouts: say \"box this lap\" once": "fuel-callouts-say-box-this-lap-once",
		"  --Weird__title!!  ":                     "weird-title",
		"Ünïcödé only ✓":                           "n-c-d-only",
		"":                                         "work",
		"!!!":                                      "work",
		"a title that is much longer than forty characters in total length": "a-title-that-is-much-longer-than-forty-c",
	} {
		got := Slug(title)
		if got != want {
			t.Errorf("Slug(%q) = %q, want %q", title, got, want)
		}
		if len(got) > 40 || strings.HasSuffix(got, "-") || strings.HasPrefix(got, "-") {
			t.Errorf("Slug(%q) = %q is not a branch tail", title, got)
		}
	}
}
