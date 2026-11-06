package ladder

import (
	"testing"
	"time"
)

var policy = Policy{
	Reviewer:  reviewer,
	Builder:   builder,
	Authors:   []string{builder, owner, "twr-release[bot]", "renovate[bot]"},
	HoldLabel: "hold",
}

// builderPR is a pull request a build Job opened: one commit by the builder, on a
// branch in the same repository, with the repository's required checks green.
func builderPR(change func(*PullRequest)) PullRequest {
	pr := PullRequest{
		Repo:           "those-who-run/ecu",
		Number:         31,
		Author:         builder,
		HeadRepo:       "those-who-run/ecu",
		HeadRef:        "agent/232-fuel-callouts",
		HeadSHA:        head,
		Commits:        []Commit{{SHA: head, Author: builder, Committer: builder}},
		Checks:         map[string]string{"test": "success", "checks / checks": "success", "go / check": "success"},
		RequiredChecks: []string{"test", "checks / checks", "go / check"},
	}
	if change != nil {
		change(&pr)
	}
	return pr
}

func reviewComment(sha string, verdict Verdict, minutes int) Comment {
	marker := ReviewMarker{SHA: sha, Kind: "A", Lane: "standard", Verdict: verdict}
	return Comment{
		Author:    reviewer,
		Body:      "Review at head `" + sha[:8] + "`: **" + string(verdict) + "**.\n\n**Checked:** I ran `make check`.\n\n" + marker.String(),
		CreatedAt: at(minutes),
		UpdatedAt: at(minutes),
	}
}

func verdictStatus(verdict Verdict, minutes int) Status {
	return Status{
		Context: StatusVerdict, State: verdict.StatusState(), Description: verdict.StatusDescription(""),
		Creator: reviewer, CreatedAt: at(minutes),
	}
}

func fixComment(author string, round, minutes int) Comment {
	return Comment{
		Author:    author,
		Body:      "Answer to the review at `1f0c5d2e`:\n\n1. Fixed in `2b3c4d5e`: the guard now covers it.\n\n" + FixMarker(round),
		CreatedAt: at(minutes),
		UpdatedAt: at(minutes),
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		pr   PullRequest
		want Decision
	}{
		// Outside the ladder: nothing is posted, whatever else is true.
		{"a stranger's pull request", builderPR(func(pr *PullRequest) { pr.Author = "someone-else" }),
			Decision{Work: WorkNone, Reason: "author-not-allowlisted"}},
		{"an author with no login", builderPR(func(pr *PullRequest) { pr.Author = "" }),
			Decision{Work: WorkNone, Reason: "author-not-allowlisted"}},
		{"a fork", builderPR(func(pr *PullRequest) { pr.Author = owner; pr.HeadRepo = "octo-maintainer/ecu" }),
			Decision{Work: WorkNone, Reason: "fork"}},
		{"a deleted fork", builderPR(func(pr *PullRequest) { pr.HeadRepo = "" }),
			Decision{Work: WorkNone, Reason: "fork"}},
		{"a draft", builderPR(func(pr *PullRequest) { pr.Draft = true }),
			Decision{Work: WorkNone, Reason: "draft"}},
		{"held", builderPR(func(pr *PullRequest) { pr.Labels = []string{"parity", "Hold"} }),
			Decision{Work: WorkNone, Reason: "hold"}},
		{"a commit by someone else", builderPR(func(pr *PullRequest) {
			pr.Commits = append(pr.Commits, Commit{SHA: oldHead, Author: "someone-else", Committer: builder})
		}), Decision{Work: WorkNone, Reason: "commit-not-allowlisted"}},
		{"a commit committed by someone else", builderPR(func(pr *PullRequest) {
			pr.Commits = []Commit{{SHA: head, Author: builder, Committer: "web-flow"}}
		}), Decision{Work: WorkNone, Reason: "commit-not-allowlisted"}},
		{"a commit whose author GitHub maps to nobody", builderPR(func(pr *PullRequest) {
			pr.Commits = []Commit{{SHA: head, Author: "", Committer: builder}}
		}), Decision{Work: WorkNone, Reason: "commit-not-allowlisted"}},
		{"no commits read", builderPR(func(pr *PullRequest) { pr.Commits = nil }),
			Decision{Work: WorkNone, Reason: "no-commits"}},
		{"no head SHA", builderPR(func(pr *PullRequest) { pr.HeadSHA = "main" }),
			Decision{Work: WorkNone, Reason: "no-head"}},

		// Review.
		{"a new pull request", builderPR(nil), Decision{Work: WorkReview, Reason: "no-review-at-head"}},
		{"the maintainer's own pull request", builderPR(func(pr *PullRequest) {
			pr.Author = owner
			pr.Commits = []Commit{{SHA: head, Author: owner, Committer: owner}}
		}), Decision{Work: WorkReview, Reason: "no-review-at-head"}},
		{"logins ignore case", builderPR(func(pr *PullRequest) {
			pr.Author = "Octo-Maintainer"
			pr.HeadRepo = "Those-Who-Run/ecu"
			pr.Commits = []Commit{{SHA: head, Author: "OCTO-MAINTAINER", Committer: "octo-maintainer"}}
		}), Decision{Work: WorkReview, Reason: "no-review-at-head"}},
		{"reviewed at an older head only", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(oldHead, VerdictReady, 1)}
		}), Decision{Work: WorkReview, Reason: "no-review-at-head"}},
		{"a stranger pasted the marker for this head", builderPR(func(pr *PullRequest) {
			c := reviewComment(head, VerdictReady, 1)
			c.Author = "someone-else"
			pr.Comments = []Comment{c}
		}), Decision{Work: WorkReview, Reason: "no-review-at-head"}},
		{"the builder pasted the reviewer's marker", builderPR(func(pr *PullRequest) {
			c := reviewComment(head, VerdictReady, 1)
			c.Author = builder
			pr.Comments = []Comment{c}
		}), Decision{Work: WorkReview, Reason: "no-review-at-head"}},
		{"a verdict status set by someone else", builderPR(func(pr *PullRequest) {
			s := verdictStatus(VerdictReady, 1)
			s.Creator = "someone-else"
			pr.Statuses = []Status{s}
		}), Decision{Work: WorkReview, Reason: "no-review-at-head"}},
		{"the builder answered after the review, without a new head", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictChanges, 1), fixComment(builder, 1, 5)}
		}), Decision{Work: WorkReview, Reason: "answered-since-review", Round: 1}},

		// Fix and stuck.
		{"changes asked, no round yet", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictChanges, 1)}
		}), Decision{Work: WorkFix, Reason: "changes-asked", Round: 1, Verdict: VerdictChanges}},
		{"changes asked again after round one", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(oldHead, VerdictChanges, 1), fixComment(builder, 1, 5), reviewComment(head, VerdictChanges, 9)}
		}), Decision{Work: WorkFix, Reason: "changes-asked", Round: 2, Verdict: VerdictChanges}},
		{"changes asked after two rounds", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{
				reviewComment(oldHead, VerdictChanges, 1), fixComment(builder, 1, 5),
				reviewComment(oldHead, VerdictChanges, 9), fixComment(builder, 2, 12),
				reviewComment(head, VerdictChanges, 20),
			}
		}), Decision{Work: WorkStuck, Reason: "fix-rounds-used", Round: 2, Verdict: VerdictChanges}},
		{"the same round posted twice counts once", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{fixComment(builder, 1, 2), fixComment(builder, 1, 3), reviewComment(head, VerdictChanges, 9)}
		}), Decision{Work: WorkFix, Reason: "changes-asked", Round: 2, Verdict: VerdictChanges}},
		{"fix markers pasted by a stranger do not use up rounds", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{fixComment("someone-else", 1, 2), fixComment(owner, 2, 3), reviewComment(head, VerdictChanges, 9)}
		}), Decision{Work: WorkFix, Reason: "changes-asked", Round: 1, Verdict: VerdictChanges}},
		{"changes on the maintainer's pull request are left to its session", builderPR(func(pr *PullRequest) {
			pr.Author = owner
			pr.Commits = []Commit{{SHA: head, Author: owner, Committer: owner}}
			pr.Comments = []Comment{reviewComment(head, VerdictChanges, 1)}
		}), Decision{Work: WorkNone, Reason: "changes-left-to-author", Verdict: VerdictChanges}},

		// Ready.
		{"ready and green", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictReady, 1)}
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictReady}},
		{"nits and green", builderPR(func(pr *PullRequest) {
			pr.Statuses = []Status{verdictStatus(VerdictNits, 1)}
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictNits}},
		{"a clean release review leaves only a status", builderPR(func(pr *PullRequest) {
			pr.Author = "twr-release[bot]"
			pr.Commits = []Commit{{SHA: head, Author: "twr-release[bot]", Committer: "twr-release[bot]"}}
			pr.Statuses = []Status{verdictStatus(VerdictReady, 1)}
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictReady}},
		{"ready, a required check still running", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictReady, 1)}
			pr.Checks["go / check"] = "in_progress"
		}), Decision{Work: WorkNone, Reason: "checks-pending", Verdict: VerdictReady}},
		{"ready, a required check not reported", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictReady, 1)}
			delete(pr.Checks, "test")
		}), Decision{Work: WorkNone, Reason: "checks-pending", Verdict: VerdictReady}},
		{"ready, a required check red", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictReady, 1)}
			pr.Checks["test"] = "failure"
			pr.Checks["go / check"] = "pending"
		}), Decision{Work: WorkNone, Reason: "checks-failing", Verdict: VerdictReady}},
		{"ready, a check that is not required is red", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictReady, 1)}
			pr.Checks["scorecard"] = "failure"
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictReady}},
		{"a skipped required check is green, as GitHub reads it", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictReady, 1)}
			pr.Checks["go / check"] = "skipped"
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictReady}},

		// A review that reached no usable verdict.
		{"blocked", builderPR(func(pr *PullRequest) { pr.Statuses = []Status{verdictStatus(VerdictBlocked, 1)} }),
			Decision{Work: WorkNone, Reason: "verdict-blocked", Verdict: VerdictBlocked}},
		{"held for the maintainer", builderPR(func(pr *PullRequest) { pr.Statuses = []Status{verdictStatus(VerdictHeld, 1)} }),
			Decision{Work: WorkNone, Reason: "verdict-held", Verdict: VerdictHeld}},
		{"a marker with no verdict and no status", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, "", 1)}
		}), Decision{Work: WorkNone, Reason: "reviewed-no-verdict"}},

		// Which record wins.
		{"the status is newer than the comment", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictChanges, 1)}
			pr.Statuses = []Status{verdictStatus(VerdictReady, 4)}
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictReady}},
		{"the comment was updated after the status", builderPR(func(pr *PullRequest) {
			c := reviewComment(head, VerdictChanges, 1)
			c.UpdatedAt = at(8)
			pr.Comments = []Comment{c}
			pr.Statuses = []Status{verdictStatus(VerdictReady, 4)}
		}), Decision{Work: WorkFix, Reason: "changes-asked", Round: 1, Verdict: VerdictChanges}},
		{"a newer marker without a verdict takes the status's", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, "", 6)}
			pr.Statuses = []Status{verdictStatus(VerdictNits, 4)}
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictNits}},
		{"written in the same second, the record with a verdict wins", builderPR(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, "", 4)}
			pr.Statuses = []Status{verdictStatus(VerdictReady, 4)}
		}), Decision{Work: WorkReady, Reason: "verdict-and-checks", Verdict: VerdictReady}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := policy.Decide(c.pr); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestTheZeroPolicyAllowsNobody(t *testing.T) {
	if got := (Policy{}).Decide(builderPR(nil)); got.Work != WorkNone {
		t.Errorf("an unconfigured policy decided %+v", got)
	}
}

func TestCommittersMayBeWider(t *testing.T) {
	// GitHub commits a web edit or an update-branch merge as web-flow; the
	// maintainer's configuration may allow it as a committer without allowing it
	// as an author.
	wider := policy
	wider.Committers = append([]string{"web-flow"}, policy.Authors...)
	pr := builderPR(func(pr *PullRequest) {
		pr.Commits = []Commit{{SHA: head, Author: builder, Committer: "web-flow"}}
	})
	if got := wider.Decide(pr); got.Work != WorkReview {
		t.Errorf("got %+v, want a review", got)
	}
	pr.Commits = []Commit{{SHA: head, Author: "web-flow", Committer: "web-flow"}}
	if got := wider.Decide(pr); got.Reason != "commit-not-allowlisted" {
		t.Errorf("got %+v, want the commit refused", got)
	}
}

func TestChecksGreenWithoutARuleset(t *testing.T) {
	cases := []struct {
		name   string
		checks map[string]string
		green  bool
		why    string
	}{
		{"nothing reported", nil, false, "no-checks"},
		{"only the ladder's own statuses", map[string]string{StatusVerdict: "success", StatusReady: "pending", StatusStuck: "failure"}, false, "no-checks"},
		{"every check green", map[string]string{"test": "success", "lint": "neutral", StatusReady: "pending"}, true, ""},
		{"one red", map[string]string{"test": "success", "lint": "cancelled"}, false, "checks-failing"},
		{"one queued", map[string]string{"test": "success", "lint": "queued"}, false, "checks-pending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			green, why := ChecksGreen(PullRequest{Checks: c.checks})
			if green != c.green || why != c.why {
				t.Errorf("got %v %q, want %v %q", green, why, c.green, c.why)
			}
		})
	}
}

var buildPolicy = BuildPolicy{
	Maintainer:      owner,
	Builder:         builder,
	Authors:         []string{builder, owner},
	ReadyLabel:      "agent-ready",
	TriageLabel:     "needs triage",
	HoldLabel:       "hold",
	ComponentPrefix: "component: ",
	Components:      map[string]string{"component: org": "", "component: deploy": "those-who-run/paddock"},
	Owner:           "those-who-run",
	Repos:           []string{"those-who-run/ecu", "those-who-run/paddock", "those-who-run/logbook"},
}

// readyIssue is a triaged issue the maintainer labelled agent-ready ten minutes in.
func readyIssue(change func(*Issue)) Issue {
	issue := Issue{
		Repo:   "those-who-run/paddock",
		Number: 232,
		Labels: []string{"parity", "component: ecu", "agent-ready"},
		LabelEvents: []LabelEvent{
			{Label: "needs triage", Actor: "someone-else", Added: true, At: at(0)},
			{Label: "needs triage", Actor: owner, Added: false, At: at(5)},
			{Label: "agent-ready", Actor: owner, Added: true, At: at(10)},
		},
	}
	if change != nil {
		change(&issue)
	}
	return issue
}

func TestDecideIssue(t *testing.T) {
	cases := []struct {
		name  string
		issue Issue
		want  BuildDecision
	}{
		{"labelled by the maintainer", readyIssue(nil),
			BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: "those-who-run/ecu"}},
		{"no ready label", readyIssue(func(i *Issue) { i.Labels = []string{"parity", "component: ecu"} }),
			BuildDecision{Reason: "not-ready"}},
		{"still needs triage", readyIssue(func(i *Issue) { i.Labels = append(i.Labels, "needs triage") }),
			BuildDecision{Reason: "needs-triage"}},
		{"held", readyIssue(func(i *Issue) { i.Labels = append(i.Labels, "hold") }),
			BuildDecision{Reason: "hold"}},
		{"labelled by someone else", readyIssue(func(i *Issue) { i.LabelEvents[2].Actor = "someone-else" }),
			BuildDecision{Reason: "label-not-by-maintainer"}},
		{"labelled by the builder itself", readyIssue(func(i *Issue) { i.LabelEvents[2].Actor = builder }),
			BuildDecision{Reason: "label-not-by-maintainer"}},
		{"the maintainer labelled it, then someone else re-added it", readyIssue(func(i *Issue) {
			i.LabelEvents = append(i.LabelEvents,
				LabelEvent{Label: "agent-ready", Actor: owner, Added: false, At: at(20)},
				LabelEvent{Label: "agent-ready", Actor: "someone-else", Added: true, At: at(30)})
		}), BuildDecision{Reason: "label-not-by-maintainer"}},
		{"events out of order still find the last one", readyIssue(func(i *Issue) {
			i.LabelEvents = []LabelEvent{
				{Label: "agent-ready", Actor: owner, Added: true, At: at(30)},
				{Label: "agent-ready", Actor: "someone-else", Added: true, At: at(10)},
			}
		}), BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: "those-who-run/ecu"}},
		{"the label is there but no event says who added it", readyIssue(func(i *Issue) { i.LabelEvents = i.LabelEvents[:2] }),
			BuildDecision{Reason: "no-label-event"}},
		{"no component label", readyIssue(func(i *Issue) { i.Labels = []string{"agent-ready"} }),
			BuildDecision{Reason: "no-component"}},
		{"two components", readyIssue(func(i *Issue) { i.Labels = append(i.Labels, "component: logbook") }),
			BuildDecision{Reason: "several-components"}},
		{"two labels, one repository", readyIssue(func(i *Issue) {
			i.Labels = []string{"agent-ready", "component: deploy", "component: paddock"}
		}), BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: "those-who-run/paddock"}},
		{"a component that is never built by a Job", readyIssue(func(i *Issue) { i.Labels = []string{"agent-ready", "component: org"} }),
			BuildDecision{Reason: "component-not-built"}},
		{"a repository the builder is not installed on", readyIssue(func(i *Issue) { i.Labels = []string{"agent-ready", "component: radio"} }),
			BuildDecision{Reason: "repository-not-allowed"}},
		{"an open pull request by the maintainer", readyIssue(func(i *Issue) {
			i.LinkedPRs = []LinkedPR{{Repo: "those-who-run/ecu", Number: 30, Author: owner, Open: true, CreatedAt: at(2)}}
		}), BuildDecision{Reason: "open-pull-request", Repo: "those-who-run/ecu"}},
		{"an open pull request by a stranger does not block", readyIssue(func(i *Issue) {
			i.LinkedPRs = []LinkedPR{{Repo: "someone-else/ecu", Number: 1, Author: "someone-else", Open: true, CreatedAt: at(20)}}
		}), BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: "those-who-run/ecu"}},
		{"the builder's pull request merged since the label", readyIssue(func(i *Issue) {
			i.LinkedPRs = []LinkedPR{{Repo: "those-who-run/ecu", Number: 31, Author: builder, CreatedAt: at(40)}}
		}), BuildDecision{Reason: "already-built", Repo: "those-who-run/ecu"}},
		{"the label was added again after the builder's last pull request", readyIssue(func(i *Issue) {
			i.LinkedPRs = []LinkedPR{{Repo: "those-who-run/ecu", Number: 31, Author: builder, CreatedAt: at(40)}}
			i.LabelEvents = append(i.LabelEvents,
				LabelEvent{Label: "agent-ready", Actor: owner, Added: false, At: at(50)},
				LabelEvent{Label: "agent-ready", Actor: owner, Added: true, At: at(60)})
		}), BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: "those-who-run/ecu"}},
		{"a closed pull request by the maintainer, before the label", readyIssue(func(i *Issue) {
			i.LinkedPRs = []LinkedPR{{Repo: "those-who-run/ecu", Number: 12, Author: owner, CreatedAt: at(1)}}
		}), BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: "those-who-run/ecu"}},
		{"a branch for the issue exists", readyIssue(func(i *Issue) { i.Branches = []string{"agent/232-fuel-callouts"} }),
			BuildDecision{Reason: "branch-exists", Repo: "those-who-run/ecu"}},
		{"a branch for another issue with the same digits first", readyIssue(func(i *Issue) { i.Branches = []string{"agent/2320-other"} }),
			BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: "those-who-run/ecu"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildPolicy.Decide(c.issue); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestTheZeroBuildPolicyBuildsNothing(t *testing.T) {
	if got := (BuildPolicy{}).Decide(readyIssue(nil)); got.Build {
		t.Errorf("an unconfigured policy decided %+v", got)
	}
	// With the labels set and nothing else, there is still no maintainer to match.
	labels := BuildPolicy{ReadyLabel: "agent-ready"}
	if got := labels.Decide(readyIssue(nil)); got.Build || got.Reason != "label-not-by-maintainer" {
		t.Errorf("a policy with no maintainer decided %+v", got)
	}
}

func TestBranchPrefix(t *testing.T) {
	if got := BranchPrefix(232); got != "agent/232-" {
		t.Errorf("prefix %q", got)
	}
}

func TestLatest(t *testing.T) {
	var zero time.Time
	if got := latest(zero, at(1)); !got.Equal(at(1)) {
		t.Errorf("latest(zero, t) = %v", got)
	}
	if got := latest(at(2), at(1)); !got.Equal(at(2)) {
		t.Errorf("latest(later, earlier) = %v", got)
	}
}
