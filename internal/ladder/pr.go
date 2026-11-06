package ladder

import (
	"sort"
	"time"
)

// Policy is who may do what on a pull request. All of it is configuration: the zero
// value allows nobody, so a poller with no configuration starts nothing.
type Policy struct {
	// Reviewer is the login the review poster acts as, such as `twr-reviewer[bot]`.
	// Review markers and the verdict status count only when it wrote them.
	Reviewer string
	// Builder is the login the build publisher acts as, such as `twr-builder[bot]`.
	// Fix markers count only when it wrote them, and only its pull requests enter the
	// fix loop.
	Builder string
	// Authors are the logins whose pull requests are reviewed, and who may author a
	// commit in one.
	Authors []string
	// Committers are the logins that may be a commit's committer. Empty means the
	// same as Authors.
	Committers []string
	// HoldLabel pauses a pull request or an issue.
	HoldLabel string
}

// Commit is one commit of a pull request, with the logins GitHub maps its author
// and committer to. A login is empty when GitHub maps the address to no account.
type Commit struct {
	SHA       string
	Author    string
	Committer string
}

// Comment is one comment on a pull request's conversation.
type Comment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Status is one commit status on the pull request's head commit.
type Status struct {
	Context     string
	State       string
	Description string
	// Creator is the login that set the status.
	Creator   string
	CreatedAt time.Time
}

// PullRequest is what the ladder reads about one open pull request.
type PullRequest struct {
	// Repo is the base repository, as owner/name.
	Repo   string
	Number int
	Author string
	// HeadRepo is the repository the head branch lives in; it differs from Repo for
	// a fork and is empty when the fork has been deleted.
	HeadRepo string
	HeadRef  string
	HeadSHA  string
	Draft    bool
	Labels   []string
	Commits  []Commit
	Comments []Comment
	// Statuses are the commit statuses on HeadSHA.
	Statuses []Status
	// Checks is the state of each check on HeadSHA by name, check runs and commit
	// statuses together: `success`, `failure`, `pending` and so on.
	Checks map[string]string
	// RequiredChecks are the checks the base branch's ruleset requires.
	RequiredChecks []string
}

// Work is what is due on a pull request.
type Work string

// The kinds of work. None means nothing is due; the decision's reason says why.
const (
	WorkNone   Work = "none"
	WorkReview Work = "review"
	WorkFix    Work = "fix"
	WorkReady  Work = "ready"
	WorkStuck  Work = "stuck"
)

// Decision is what is due on a pull request, and why.
type Decision struct {
	Work Work
	// Reason is a short, stable word for logs and the heartbeat's counts. It never
	// holds text from the pull request.
	Reason string
	// Round is the fix round to start, for WorkFix; the rounds used, for WorkStuck.
	Round int
	// Verdict is the verdict at the head, when there is one.
	Verdict Verdict
}

// Skipped reports why a pull request is outside the ladder altogether, or "" when it
// is in. A skipped pull request gets no comment and no status, so a stranger cannot
// use the reviewer as a reply channel.
func (p Policy) Skipped(pr PullRequest) string {
	committers := p.Committers
	if len(committers) == 0 {
		committers = p.Authors
	}
	switch {
	case !hasLogin(p.Authors, pr.Author):
		return "author-not-allowlisted"
	case !sameRepo(pr.HeadRepo, pr.Repo):
		return "fork"
	case pr.Draft:
		return "draft"
	case p.HoldLabel != "" && hasLabel(pr.Labels, p.HoldLabel):
		return "hold"
	case !IsSHA(pr.HeadSHA):
		return "no-head"
	case len(pr.Commits) == 0:
		return "no-commits"
	}
	for _, commit := range pr.Commits {
		if !hasLogin(p.Authors, commit.Author) || !hasLogin(committers, commit.Committer) {
			return "commit-not-allowlisted"
		}
	}
	return ""
}

func sameRepo(a, b string) bool { return sameLogin(a, b) }

// review is the newest record of a review at the head.
type review struct {
	at      time.Time
	verdict Verdict
}

// reviewAtHead finds the reviewer's record for the head commit: its marker comment
// for that SHA, or its verdict status on it, whichever is newer. A clean release or
// dependency review posts no comment, so the status alone is a record.
func (p Policy) reviewAtHead(pr PullRequest) (review, bool) {
	var records []review
	for _, comment := range pr.Comments {
		if !sameLogin(comment.Author, p.Reviewer) {
			continue
		}
		for _, marker := range ParseReviewMarkers(comment.Body) {
			if marker.SHA == pr.HeadSHA {
				records = append(records, review{at: latest(comment.CreatedAt, comment.UpdatedAt), verdict: marker.Verdict})
			}
		}
	}
	for _, status := range pr.Statuses {
		if status.Context != StatusVerdict || !sameLogin(status.Creator, p.Reviewer) {
			continue
		}
		verdict, _ := VerdictFromStatus(status.Description)
		records = append(records, review{at: status.CreatedAt, verdict: verdict})
	}
	if len(records) == 0 {
		return review{}, false
	}
	// Newest first; of two records at the same instant, the one with a verdict.
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].at.Equal(records[j].at) {
			return records[i].at.After(records[j].at)
		}
		return records[i].verdict != "" && records[j].verdict == ""
	})
	newest := records[0]
	if newest.verdict == "" {
		// A marker written before the poster added verdicts, or a status it could not
		// read: take the newest record that does say.
		for _, record := range records[1:] {
			if record.verdict != "" {
				newest.verdict = record.verdict
				break
			}
		}
	}
	return newest, true
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// FixRounds returns the fix rounds the builder has answered on a pull request, and
// when it last did. Rounds are counted from the builder's own marker comments; the
// same round twice counts once.
func (p Policy) FixRounds(pr PullRequest) (rounds int, last time.Time) {
	seen := map[int]bool{}
	for _, comment := range pr.Comments {
		if !sameLogin(comment.Author, p.Builder) {
			continue
		}
		for _, round := range ParseFixRounds(comment.Body) {
			seen[round] = true
			last = latest(last, latest(comment.CreatedAt, comment.UpdatedAt))
		}
	}
	return len(seen), last
}

// ChecksGreen reports whether every required check on the head is green, and if not
// a word for why. With no required checks known, every check reported must be green
// and there must be at least one: an empty list never passes by default. The
// ladder's own statuses are left out, since ready cannot wait for itself.
func ChecksGreen(pr PullRequest) (bool, string) {
	required := pr.RequiredChecks
	if len(required) == 0 {
		for name := range pr.Checks {
			if name != StatusReady && name != StatusStuck && name != StatusVerdict {
				required = append(required, name)
			}
		}
		if len(required) == 0 {
			return false, "no-checks"
		}
	}
	pending := false
	for _, name := range required {
		switch pr.Checks[name] {
		case "success", "neutral", "skipped":
		case "", "pending", "queued", "in_progress", "expected", "waiting", "requested":
			pending = true
		default:
			return false, "checks-failing"
		}
	}
	if pending {
		return false, "checks-pending"
	}
	return true, ""
}

// Decide says what is due on one open pull request.
//
//   - review: the pull request is in the ladder and has no review record at its head,
//     or the builder has answered since the last one.
//   - fix: a builder's pull request whose verdict at the head is changes, with fewer
//     than MaxFixRounds rounds answered.
//   - stuck: the same, with the rounds used up.
//   - ready: the verdict at the head is ready or nits and every required check is
//     green.
func (p Policy) Decide(pr PullRequest) Decision {
	if reason := p.Skipped(pr); reason != "" {
		return Decision{Work: WorkNone, Reason: reason}
	}
	rounds, lastFix := p.FixRounds(pr)
	record, reviewed := p.reviewAtHead(pr)
	if !reviewed {
		return Decision{Work: WorkReview, Reason: "no-review-at-head", Round: rounds}
	}
	if lastFix.After(record.at) {
		// An answer that contests every item pushes nothing, so the head stays where
		// it was. The reviewer still owes it a read.
		return Decision{Work: WorkReview, Reason: "answered-since-review", Round: rounds}
	}
	switch record.verdict {
	case VerdictChanges:
		if !sameLogin(pr.Author, p.Builder) {
			// A pull request from a laptop session is fixed by its session.
			return Decision{Work: WorkNone, Reason: "changes-left-to-author", Verdict: record.verdict}
		}
		if rounds >= MaxFixRounds {
			return Decision{Work: WorkStuck, Reason: "fix-rounds-used", Round: rounds, Verdict: record.verdict}
		}
		return Decision{Work: WorkFix, Reason: "changes-asked", Round: rounds + 1, Verdict: record.verdict}
	case VerdictReady, VerdictNits:
		if green, why := ChecksGreen(pr); !green {
			return Decision{Work: WorkNone, Reason: why, Verdict: record.verdict}
		}
		return Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: record.verdict}
	case "":
		return Decision{Work: WorkNone, Reason: "reviewed-no-verdict"}
	default:
		return Decision{Work: WorkNone, Reason: "verdict-" + string(record.verdict), Verdict: record.verdict}
	}
}
