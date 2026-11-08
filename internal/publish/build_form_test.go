package publish

import (
	"strings"
	"testing"
)

// The form the task hands the agent must be one the publisher accepts, as it
// stands and with the tool's trailer after it, or a build that follows it to the
// letter is refused.
func TestBuildBodyFormPassesTheChecks(t *testing.T) {
	task := buildTask()
	form := BuildBodyForm(task.IssueRepo, task.Issue)
	for name, body := range map[string]string{
		"as it stands":   form,
		"with a trailer": form + "\n" + ToolTrailer + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := checkBody(task, body, rules(t).Scanner); len(got) != 0 {
				t.Errorf("refused: %v", got)
			}
		})
	}
}

// Each thing the publisher looks for is what the form says: take one out and the
// form is refused, so the form can't carry a part the checks ignore.
func TestBuildBodyFormHasEveryPartTheChecksWant(t *testing.T) {
	task := buildTask()
	form := BuildBodyForm(task.IssueRepo, task.Issue)
	for _, part := range []string{
		"## What changed\n",
		"AI assistance:",
		"New direct dependencies:",
		"## How it was checked\n",
		"| Criterion | Proof | Where |\n",
		"- [x] The command that matches CI",
		"- [x] Docs updated, or none needed because:",
		"Closes " + task.IssueRepo,
	} {
		t.Run(part, func(t *testing.T) {
			if !strings.Contains(form, part) {
				t.Fatalf("the form has no %q", part)
			}
			if got := checkBody(task, strings.Replace(form, part, "", 1), rules(t).Scanner); len(got) == 0 {
				t.Errorf("accepted without %q", part)
			}
		})
	}
}
