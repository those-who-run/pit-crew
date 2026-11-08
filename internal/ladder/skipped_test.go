package ladder

import "testing"

// skippedStatus is the record of pit-crew#10, as the review poster writes it: a green
// status whose description, not its state, says what the review concluded.
func skippedStatus(minutes int) Status {
	return Status{Context: StatusVerdict, State: "success", Description: "skipped: kind B, lane standard", Creator: reviewer, CreatedAt: at(minutes)}
}

func TestSkippedVerdictIsNotReady(t *testing.T) {
	release := func(change func(*PullRequest)) PullRequest {
		return builderPR(func(pr *PullRequest) {
			pr.Author = "twr-release[bot]"
			pr.Commits = []Commit{{SHA: head, Author: "twr-release[bot]", Committer: "twr-release[bot]"}}
			pr.Statuses = []Status{skippedStatus(5)}
			if change != nil {
				change(pr)
			}
		})
	}
	skipped := Decision{Work: WorkNone, Reason: "verdict-skipped", Verdict: VerdictSkipped}
	cases := []struct {
		name string
		pr   PullRequest
		want Decision
	}{
		{"a release skipped at its head, every check green", release(nil), skipped},
		{"a builder's pull request skipped: no fix either", builderPR(func(pr *PullRequest) {
			pr.Statuses = []Status{skippedStatus(5)}
		}), skipped},
		{"no required checks known, so every reported check counts", release(func(pr *PullRequest) {
			pr.RequiredChecks = nil
			pr.Checks[StatusVerdict] = "success"
		}), skipped},
		{"a ruleset that requires the verdict status", release(func(pr *PullRequest) {
			pr.RequiredChecks = append(pr.RequiredChecks, StatusVerdict)
			pr.Checks[StatusVerdict] = "success"
		}), skipped},
		{"a skip newer than a ready at the same head", release(func(pr *PullRequest) {
			pr.Comments = []Comment{reviewComment(head, VerdictReady, 1)}
		}), skipped},
		{"a marker that says skipped", release(func(pr *PullRequest) {
			pr.Statuses = nil
			pr.Comments = []Comment{reviewComment(head, VerdictSkipped, 5)}
		}), skipped},
		{"a stranger's ready status after the skip", release(func(pr *PullRequest) {
			pr.Statuses = append(pr.Statuses, Status{Context: StatusVerdict, State: "success", Description: "ready", Creator: "someone-else", CreatedAt: at(9)})
		}), skipped},
		{"control: a ready status at the same head does reach ready", release(func(pr *PullRequest) {
			pr.Statuses = []Status{verdictStatus(VerdictReady, 5)}
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
