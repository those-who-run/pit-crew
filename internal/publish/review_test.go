package publish

import (
	"reflect"
	"strings"
	"testing"

	"github.com/those-who-run/pit-crew/internal/ladder"
	"github.com/those-who-run/pit-crew/internal/lanes"
	"github.com/those-who-run/pit-crew/internal/privatescan"
)

const (
	head  = "1f0c5d2e9a7b4c3d8e6f1a2b3c4d5e6f7a8b9c0d"
	other = "0000000000000000000000000000000000000001"
)

var task = ReviewTask{Repo: "those-who-run/ecu", PR: 31, HeadSHA: head, Lane: lanes.Standard}

var privateHost = strings.Join([]string{"nuc", "home", "arpa"}, ".")

func scanner() privatescan.Scanner { return privatescan.Scanner{Allow: privatescan.DefaultAllowlist()} }

// verdictJSON is verdict.json for a change review, as the brief defines it.
func verdictJSON(verdict ladder.Verdict, blocking ...string) []byte {
	items := "[]"
	if len(blocking) > 0 {
		items = `["` + strings.Join(blocking, `", "`) + `"]`
	}
	return []byte(`{"repo": "those-who-run/ecu", "pr": 31, "sha": "` + head + `", "kind": "A", "lane": "standard",
	  "verdict": "` + string(verdict) + `", "blocking": ` + items + `, "ran": ["make check"], "not_run": []}`)
}

// comment is a change review's comment.md, as the brief asks for it.
func comment(verdict ladder.Verdict, middle string) []byte {
	return []byte("Review at head `1f0c5d2e`: **" + string(verdict) + "**.\n\n" +
		"**Criteria:**\n- AC1 fuel callout: proven, by `race/fuel_test.go`, `TestBoxThisLap`.\n\n" +
		"**Checked:** I ran `make check` (green, 41.2 s).\n\n" + middle +
		"<!-- twr-review sha=" + head + " kind=A lane=standard -->\n")
}

func TestCheckReviewPosts(t *testing.T) {
	cases := []struct {
		name  string
		files ReviewFiles
		want  ReviewResult
	}{
		{"ready", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady), Comment: comment(ladder.VerdictReady, "")},
			ReviewResult{Outcome: ReviewPost, Verdict: ladder.VerdictReady, Kind: "A", Lane: "standard",
				StatusState: "success", StatusDescription: "ready: kind A, lane standard"}},
		{"changes, two asked for", ReviewFiles{Verdict: verdictJSON(ladder.VerdictChanges, "a", "b"),
			Comment: comment(ladder.VerdictChanges, "**Asked for:**\n1. `race/fuel.go:88 (head 1f0c5d2e)`: wrong unit.\n2. Same at line 97.\n\n")},
			ReviewResult{Outcome: ReviewPost, Verdict: ladder.VerdictChanges, Kind: "A", Lane: "standard",
				StatusState: "failure", StatusDescription: "changes: 2 asked for, kind A, lane standard"}},
		{"a clean dependency review has no comment", ReviewFiles{Verdict: []byte(`{"repo": "those-who-run/ecu", "pr": 31, "sha": "` + head +
			`", "kind": "D", "lane": "standard", "verdict": "ready", "blocking": [], "ran": [], "not_run": []}`)},
			ReviewResult{Outcome: ReviewPost, Verdict: ladder.VerdictReady, Kind: "D", Lane: "standard",
				StatusState: "success", StatusDescription: "ready: kind D, lane standard"}},
		{"skipped, with the reason the brief asks for", ReviewFiles{Verdict: []byte(`{"repo": "those-who-run/ecu", "pr": 31, "sha": "` + head +
			`", "verdict": "skipped", "reason": "author outside the list"}`)},
			ReviewResult{Outcome: ReviewPost, Verdict: ladder.VerdictSkipped, StatusState: "error", StatusDescription: "skipped"}},
		{"blocked before the kind was known", ReviewFiles{Verdict: []byte(`{"repo": "those-who-run/ecu", "pr": 31, "sha": "` + head +
			`", "kind": "", "lane": "", "verdict": "blocked", "blocking": [], "ran": [], "not_run": ["the head kept moving"]}`)},
			ReviewResult{Outcome: ReviewPost, Verdict: ladder.VerdictBlocked, StatusState: "error", StatusDescription: "blocked"}},
		{"the marker may already carry the verdict", ReviewFiles{Verdict: verdictJSON(ladder.VerdictNits),
			Comment: []byte(strings.Replace(string(comment(ladder.VerdictNits, "")), "lane=standard -->", "lane=standard verdict=nits -->", 1))},
			ReviewResult{Outcome: ReviewPost, Verdict: ladder.VerdictNits, Kind: "A", Lane: "standard",
				StatusState: "success", StatusDescription: "nits: kind A, lane standard"}},
		{"private.md beside a clean comment changes nothing public", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: comment(ladder.VerdictReady, ""), Private: []byte("The deploy doc names " + privateHost + ".\n")},
			ReviewResult{Outcome: ReviewPost, Verdict: ladder.VerdictReady, Kind: "A", Lane: "standard",
				StatusState: "success", StatusDescription: "ready: kind A, lane standard"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CheckReview(task, head, c.files, scanner())
			body := got.Body
			got.Body = ""
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v\nwant %+v", got, c.want)
			}
			if c.files.Comment == nil {
				if body != "" {
					t.Errorf("a body %q with no comment", body)
				}
				return
			}
			want := ladder.ReviewMarker{SHA: head, Kind: "A", Lane: "standard", Verdict: c.want.Verdict}.String() + "\n"
			if !strings.HasSuffix(body, "\n\n"+want) {
				t.Errorf("body ends %q, want the marker with the verdict", body[len(body)-80:])
			}
			if !strings.HasPrefix(body, "Review at head `1f0c5d2e`: **") || len(ladder.AnyMarkerRE.FindAllString(body, -1)) != 1 {
				t.Errorf("body %q", body)
			}
		})
	}
}

func TestCheckReviewHeadMoved(t *testing.T) {
	got := CheckReview(task, other, ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady), Comment: comment(ladder.VerdictReady, "")}, scanner())
	if got.Outcome != ReviewHeadMoved || got.Body != "" || got.StatusState != "" {
		t.Errorf("got %+v, want nothing but head-moved", got)
	}
}

func TestCheckReviewHolds(t *testing.T) {
	big := comment(ladder.VerdictReady, strings.Repeat("**Checked:** a line of detail about what was run and seen.\n", 300))
	cases := []struct {
		name  string
		files ReviewFiles
		codes []string
		// detail is text the hold folder's reason must carry.
		detail string
	}{
		{"no verdict.json", ReviewFiles{Comment: comment(ladder.VerdictReady, "")}, []string{"verdict-missing"}, ""},
		{"verdict.json is not JSON", ReviewFiles{Verdict: []byte("ready")}, []string{"verdict-invalid"}, ""},
		{"another pull request", ReviewFiles{Verdict: []byte(strings.Replace(string(verdictJSON(ladder.VerdictReady)), `"pr": 31`, `"pr": 32`, 1)),
			Comment: comment(ladder.VerdictReady, "")}, []string{"verdict-invalid"}, "ecu#32"},
		{"another head", ReviewFiles{Verdict: []byte(strings.Replace(string(verdictJSON(ladder.VerdictReady)), head, other, 1)),
			Comment: comment(ladder.VerdictReady, "")}, []string{"verdict-invalid"}, "sha is"},
		{"a verdict the brief does not have", ReviewFiles{Verdict: verdictJSON("lgtm")}, []string{"verdict-invalid"}, "lgtm"},
		{"a kind the brief does not have", ReviewFiles{Verdict: []byte(strings.Replace(string(verdictJSON(ladder.VerdictReady)), `"kind": "A"`, `"kind": "E"`, 1))},
			[]string{"verdict-invalid"}, "kind"},
		{"a lighter lane than the diff's", ReviewFiles{Verdict: []byte(strings.Replace(string(verdictJSON(ladder.VerdictReady)), `"lane": "standard"`, `"lane": "light"`, 1))},
			[]string{"verdict-invalid"}, "lane is"},
		{"changes with nothing blocking", ReviewFiles{Verdict: verdictJSON(ladder.VerdictChanges)}, []string{"verdict-invalid"}, "blocking"},
		{"ready with something blocking", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady, "a")}, []string{"verdict-invalid"}, "that is changes"},
		{"a change review with no comment", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady)}, []string{"comment-missing"}, ""},
		{"a skipped review with a comment", ReviewFiles{Verdict: verdictJSON(ladder.VerdictSkipped), Comment: comment(ladder.VerdictSkipped, "")},
			[]string{"comment-unexpected"}, ""},
		{"over 15 KB", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady), Comment: big}, []string{"comment-too-large"}, "bytes"},
		{"not UTF-8", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady), Comment: append([]byte("Review at head"), 0xff, 0xfe)},
			[]string{"comment-invalid"}, "UTF-8"},
		{"the wrong first line", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: []byte(strings.Replace(string(comment(ladder.VerdictReady, "")), "Review at head", "Reviewed", 1))},
			[]string{"comment-invalid"}, "first line"},
		{"the first line names another head", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: []byte(strings.Replace(string(comment(ladder.VerdictReady, "")), "1f0c5d2e", "00000000", 1))},
			[]string{"comment-invalid"}, "first line"},
		{"no marker", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: []byte(strings.Replace(string(comment(ladder.VerdictReady, "")), "<!-- twr-review", "(marker:", 1))},
			[]string{"comment-invalid"}, "last line"},
		{"the marker names another head", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: []byte(strings.Replace(string(comment(ladder.VerdictReady, "")), "sha="+head, "sha="+other, 1))},
			[]string{"comment-invalid"}, "marker is"},
		{"the marker disagrees with verdict.json", ReviewFiles{Verdict: verdictJSON(ladder.VerdictChanges, "a"),
			Comment: []byte(strings.Replace(string(comment(ladder.VerdictChanges, "")), "lane=standard -->", "lane=standard verdict=ready -->", 1))},
			[]string{"comment-invalid"}, "marker is"},
		{"text on the marker's line", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: []byte(strings.Replace(string(comment(ladder.VerdictReady, "")), "<!-- twr-review", "Done. <!-- twr-review", 1))},
			[]string{"comment-invalid"}, "last line"},
		{"a second marker", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: comment(ladder.VerdictReady, "<!-- twr-fix round=1 -->\n\n")}, []string{"marker"}, "2 markers"},
		{"a private value", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: comment(ladder.VerdictReady, "**Not run here:** the server at "+privateHost+".\n\n")},
			[]string{"private-scan"}, privateHost},
		{"a mention", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: comment(ladder.VerdictReady, "Please look, @octo-maintainer.\n\n")}, []string{"mention"}, "@octo-maintainer"},
		{"everything at once is all reported", ReviewFiles{Verdict: verdictJSON(ladder.VerdictReady),
			Comment: comment(ladder.VerdictReady, "cc @someone at "+privateHost+"\n<!-- twr-fix round=1 -->\n\n")},
			[]string{"mention", "private-scan", "marker"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CheckReview(task, head, c.files, scanner())
			if got.Outcome != ReviewHold || got.Verdict != ladder.VerdictHeld || got.Body != "" {
				t.Fatalf("got %+v, want a hold with no body", got)
			}
			if codes := Codes(got.Refusals); !reflect.DeepEqual(codes, c.codes) {
				t.Errorf("codes %v, want %v: %+v", codes, c.codes, got.Refusals)
			}
			if got.StatusState != "error" || got.StatusDescription != "held: "+strings.Join(c.codes, ", ") {
				t.Errorf("status %q %q", got.StatusState, got.StatusDescription)
			}
			var details []string
			for _, r := range got.Refusals {
				details = append(details, r.Detail)
			}
			if c.detail != "" && !strings.Contains(strings.Join(details, "\n"), c.detail) {
				t.Errorf("details %q do not say %q", details, c.detail)
			}
		})
	}
}

func TestMentions(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"Please look, @octo-maintainer.", []string{"@octo-maintainer"}},
		{"cc @org/team", []string{"@org/team"}},
		{"(@a-b)", []string{"@a-b"}},
		{"@someone", []string{"@someone"}},
		{"me@example.com and x@users.noreply.github.com", nil},
		{"the `@contextlib.contextmanager` decorator", nil},
		{"```\n@pytest.mark.skip\n```\nfine", nil},
		{"image@sha256:abc", nil},
		{"a/@b", nil},
		{"@-dash and @_u", nil},
	}
	for _, c := range cases {
		if got := mentions(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %v, want %v", c.text, got, c.want)
		}
	}
}
