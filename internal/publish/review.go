package publish

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/those-who-run/pit-crew/internal/ladder"
	"github.com/those-who-run/pit-crew/internal/lanes"
	"github.com/those-who-run/pit-crew/internal/privatescan"
)

// ReviewTask is what the fetch step recorded about the pull request under review.
// It comes from the Job's own environment and the read-only data volume, never from
// a file the model could write.
type ReviewTask struct {
	// Repo is the repository, as owner/name.
	Repo string
	PR   int
	// HeadSHA is the commit that was checked out and reviewed.
	HeadSHA string
	Lane    lanes.Lane
}

// ReviewFiles are the files a reviewer may leave in its output folder. A nil slice
// means the file is not there.
type ReviewFiles struct {
	Verdict []byte
	Comment []byte
	Private []byte
}

// VerdictFile is verdict.json as the reviewer's brief defines it.
type VerdictFile struct {
	Repo     string         `json:"repo"`
	PR       int            `json:"pr"`
	SHA      string         `json:"sha"`
	Kind     string         `json:"kind"`
	Lane     string         `json:"lane"`
	Verdict  ladder.Verdict `json:"verdict"`
	Blocking []string       `json:"blocking"`
	Ran      []string       `json:"ran"`
	NotRun   []string       `json:"not_run"`
}

// ReviewOutcome is what the review poster does with a review.
type ReviewOutcome string

// The outcomes of CheckReview.
const (
	// ReviewPost: record the verdict, and post the comment when there is one.
	ReviewPost ReviewOutcome = "post"
	// ReviewHold: post nothing of the model's; keep its files for the maintainer and
	// record that the review was held.
	ReviewHold ReviewOutcome = "hold"
	// ReviewHeadMoved: the pull request has a new head, so this review is of the
	// past. Post nothing; the poller queues the new head.
	ReviewHeadMoved ReviewOutcome = "head-moved"
)

// ReviewResult is CheckReview's answer.
type ReviewResult struct {
	Outcome ReviewOutcome
	// Verdict is the verdict to record: the reviewer's, or held.
	Verdict ladder.Verdict
	// Kind and Lane are the reviewer's, once checked.
	Kind string
	Lane string
	// Body is the comment to post, ending in the marker with the verdict added;
	// empty when there is no comment to post.
	Body string
	// StatusState and StatusDescription are the `review / verdict` status to set.
	StatusState       string
	StatusDescription string
	// Refusals say why a review is held.
	Refusals []Refusal
}

var (
	kindRE = regexp.MustCompile(`^[A-D]$`)
	// The first line the brief asks for, with the short SHA in or out of backticks.
	reviewFirstLineRE = regexp.MustCompile("^Review at head `?([0-9a-f]{8})`?: \\*\\*[^*]+\\*\\*\\.$")
)

// CheckReview checks a reviewer's output against the task and the brief, and says
// what to do with it. currentHead is the pull request's head now.
//
// It refuses (holds) when verdict.json is missing, unreadable or does not describe
// this task; when a comment is missing where the brief requires one or present
// where it forbids one; when the comment is over 15 KB, does not start and end as
// the brief says, carries a marker it should not, names a person, or fails the
// private-value scan. A moved head is not a refusal: nothing is wrong with the
// review, it is only late.
func CheckReview(task ReviewTask, currentHead string, files ReviewFiles, scanner privatescan.Scanner) ReviewResult {
	if currentHead != task.HeadSHA {
		return ReviewResult{Outcome: ReviewHeadMoved}
	}
	verdict, refusals := checkVerdict(task, files.Verdict)
	if len(refusals) == 0 {
		refusals = append(refusals, checkComment(task, verdict, files.Comment, scanner)...)
	}
	if len(refusals) > 0 {
		return ReviewResult{
			Outcome:           ReviewHold,
			Verdict:           ladder.VerdictHeld,
			Refusals:          refusals,
			StatusState:       ladder.VerdictHeld.StatusState(),
			StatusDescription: ladder.VerdictHeld.StatusDescription(strings.Join(Codes(refusals), ", ")),
		}
	}
	result := ReviewResult{
		Outcome:           ReviewPost,
		Verdict:           verdict.Verdict,
		Kind:              verdict.Kind,
		Lane:              verdict.Lane,
		StatusState:       verdict.Verdict.StatusState(),
		StatusDescription: verdict.Verdict.StatusDescription(statusDetail(verdict)),
	}
	if files.Comment != nil {
		// The reviewer's marker is replaced by the same marker with the verdict, so
		// the comment alone says what the review concluded.
		_, rest := lastLine(string(files.Comment))
		marker := ladder.ReviewMarker{SHA: task.HeadSHA, Kind: verdict.Kind, Lane: verdict.Lane, Verdict: verdict.Verdict}
		result.Body = rest + marker.String() + "\n"
	}
	return result
}

// statusDetail is the publisher's own summary for the status description: counts
// and the kind and lane, nothing the model worded.
func statusDetail(v VerdictFile) string {
	var parts []string
	if v.Verdict == ladder.VerdictChanges {
		parts = append(parts, fmt.Sprintf("%d asked for", len(v.Blocking)))
	}
	if v.Kind != "" {
		parts = append(parts, "kind "+v.Kind)
	}
	if v.Lane != "" {
		parts = append(parts, "lane "+v.Lane)
	}
	return strings.Join(parts, ", ")
}

func checkVerdict(task ReviewTask, raw []byte) (VerdictFile, []Refusal) {
	var v VerdictFile
	if raw == nil {
		return v, []Refusal{refuse("verdict-missing", "verdict.json is not in the output folder")}
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, []Refusal{refuse("verdict-invalid", "verdict.json: %v", err)}
	}
	var refusals []Refusal
	bad := func(format string, args ...any) {
		refusals = append(refusals, refuse("verdict-invalid", "verdict.json: "+format, args...))
	}
	if !strings.EqualFold(v.Repo, task.Repo) || v.PR != task.PR {
		bad("it is about %s#%d, and the task is %s#%d", v.Repo, v.PR, task.Repo, task.PR)
	}
	if v.SHA != task.HeadSHA {
		bad("sha is %q, and the head that was fetched is %s", v.SHA, task.HeadSHA)
	}
	known := false
	for _, verdict := range ladder.ModelVerdicts {
		known = known || v.Verdict == verdict
	}
	if !known {
		bad("verdict %q is not one of ready, nits, changes, blocked, skipped", v.Verdict)
		return v, refusals
	}
	// A review that could not start may not know its kind or lane; one that reached
	// a conclusion must, and the lane is the one the fetch step worked out.
	concluded := v.Verdict == ladder.VerdictReady || v.Verdict == ladder.VerdictNits || v.Verdict == ladder.VerdictChanges
	if (concluded || v.Kind != "") && !kindRE.MatchString(v.Kind) {
		bad("kind %q is not one of A, B, C, D", v.Kind)
	}
	if (concluded || v.Lane != "") && v.Lane != string(task.Lane) {
		bad("lane is %q, and the lane of this change is %s", v.Lane, task.Lane)
	}
	switch {
	case v.Verdict == ladder.VerdictChanges && len(v.Blocking) == 0:
		bad("verdict changes needs at least one blocking item")
	case (v.Verdict == ladder.VerdictReady || v.Verdict == ladder.VerdictNits) && len(v.Blocking) > 0:
		bad("verdict %s has %d blocking items; that is changes", v.Verdict, len(v.Blocking))
	}
	return v, refusals
}

func checkComment(task ReviewTask, v VerdictFile, raw []byte, scanner privatescan.Scanner) []Refusal {
	if raw == nil {
		// A change review always comments; a release, pin or dependency review only
		// when something is wrong.
		if v.Kind == "A" && v.Verdict != ladder.VerdictSkipped {
			return []Refusal{refuse("comment-missing", "a kind A review always writes comment.md")}
		}
		return nil
	}
	if v.Verdict == ladder.VerdictSkipped {
		return []Refusal{refuse("comment-unexpected", "a skipped review writes only verdict.json")}
	}
	comment := string(raw)
	refusals := checkProse(scanner, "comment", comment, MaxCommentBytes)
	if len(refusals) > 0 && refusals[0].Code == "comment-invalid" {
		return refusals
	}
	if m := reviewFirstLineRE.FindStringSubmatch(firstLine(comment)); m == nil || m[1] != task.HeadSHA[:8] {
		refusals = append(refusals, refuse("comment-invalid",
			"the first line is not \"Review at head `%s`: **<verdict>**.\"", task.HeadSHA[:8]))
	}
	want := ladder.ReviewMarker{SHA: task.HeadSHA, Kind: v.Kind, Lane: v.Lane}
	last, _ := lastLine(comment)
	markers := ladder.ParseReviewMarkers(last)
	switch {
	case len(markers) != 1 || strings.TrimSpace(ladder.AnyMarkerRE.ReplaceAllString(last, "")) != "":
		refusals = append(refusals, refuse("comment-invalid", "the last line is not the marker %s", want))
	case markers[0].SHA != want.SHA || markers[0].Kind != want.Kind || markers[0].Lane != want.Lane ||
		(markers[0].Verdict != "" && markers[0].Verdict != v.Verdict):
		refusals = append(refusals, refuse("comment-invalid", "the marker is %s, and verdict.json says %s", markers[0], want))
	}
	if n := countMarkers(comment); n > 1 {
		refusals = append(refusals, refuse("marker", "the comment has %d markers; only its last line may be one", n))
	}
	return refusals
}
