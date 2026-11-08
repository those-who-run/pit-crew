// Package poller reads GitHub, decides what the ladder owes each pull request and
// issue, and starts at most one Job per run within its limits. It also carries the
// no-model `ready` pass, which sets the ladder's statuses, since both read the
// same things.
package poller

import (
	"context"
	"log/slog"

	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/ladder"
)

// Source is what the poller reads from GitHub. *github.Client is one.
type Source interface {
	ListInstallationRepos(ctx context.Context) ([]string, error)
	ListOpenPulls(ctx context.Context, repo string) ([]github.PullRequest, error)
	ListPullCommits(ctx context.Context, repo string, number int) ([]github.Commit, error)
	ListComments(ctx context.Context, repo string, number int) ([]github.Comment, error)
	ListStatuses(ctx context.Context, repo, sha string) ([]github.Status, error)
	ListChecks(ctx context.Context, repo, sha string) (map[string]string, error)
	ListRequiredChecks(ctx context.Context, repo, branch string) ([]string, error)
	ListOpenIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error)
	ListTimeline(ctx context.Context, repo string, number int) ([]github.TimelineEvent, error)
	ListBranches(ctx context.Context, repo, prefix string) ([]string, error)
}

// Policies is the ladder's configuration for pull requests and issues.
type Policies struct {
	Pull  ladder.Policy
	Build ladder.BuildPolicy
	// Repos are the repositories to poll for pull requests; empty means the
	// installation's.
	Repos []string
	// IssueRepo is where issues live, as owner/name.
	IssueRepo string
}

// Candidate is one pull request with its decision.
type Candidate struct {
	Pull   github.PullRequest
	Rounds int
	// ReReviews is how often the maintainer has asked for a review again; it names
	// the Job when the decision is one.
	ReReviews int
	Decision  ladder.Decision
}

// BuildCandidate is one issue with its decision.
type BuildCandidate struct {
	Issue    github.Issue
	Decision ladder.BuildDecision
}

// Gather reads everything the ladder needs about one open pull request. The
// skipped checks come first, so a stranger's pull request costs one call, not six.
func Gather(ctx context.Context, src Source, policy ladder.Policy, pr github.PullRequest) (ladder.PullRequest, error) {
	out := ladder.PullRequest{
		Repo: pr.Repo, Number: pr.Number, Author: pr.Author, HeadRepo: pr.HeadRepo, HeadRef: pr.HeadRef,
		HeadSHA: pr.HeadSHA, Draft: pr.Draft, Labels: pr.Labels,
	}
	if reason := policy.Skipped(out); reason != "" && reason != "no-commits" {
		return out, nil
	}
	commits, err := src.ListPullCommits(ctx, pr.Repo, pr.Number)
	if err != nil {
		return out, err
	}
	for _, c := range commits {
		out.Commits = append(out.Commits, ladder.Commit{SHA: c.SHA, Author: c.Author, Committer: c.Committer})
	}
	if reason := policy.Skipped(out); reason != "" {
		return out, nil
	}
	comments, err := src.ListComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		return out, err
	}
	for _, c := range comments {
		out.Comments = append(out.Comments, ladder.Comment{ID: c.ID, Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	statuses, err := src.ListStatuses(ctx, pr.Repo, pr.HeadSHA)
	if err != nil {
		return out, err
	}
	for _, s := range statuses {
		out.Statuses = append(out.Statuses, ladder.Status{Context: s.Context, State: s.State, Description: s.Description, Creator: s.Creator, CreatedAt: s.CreatedAt})
	}
	if out.Checks, err = src.ListChecks(ctx, pr.Repo, pr.HeadSHA); err != nil {
		return out, err
	}
	if out.RequiredChecks, err = src.ListRequiredChecks(ctx, pr.Repo, pr.BaseRef); err != nil {
		// A repository without a ruleset answers 404; the decision then wants every
		// check green.
		if !github.IsStatus(err, 404) {
			return out, err
		}
	}
	if policy.ReReviewLabel != "" && ladder.HasLabel(pr.Labels, policy.ReReviewLabel) {
		// Who put the label there, and when, is on the timeline; it is read only
		// when the label is on, as the issue's is for agent-ready.
		events, err := src.ListTimeline(ctx, pr.Repo, pr.Number)
		if err != nil {
			return out, err
		}
		out.LabelEvents = labelEvents(events)
	}
	return out, nil
}

// labelEvents keeps the labeled and unlabeled events of a timeline.
func labelEvents(events []github.TimelineEvent) []ladder.LabelEvent {
	var out []ladder.LabelEvent
	for _, e := range events {
		if e.Event == "labeled" || e.Event == "unlabeled" {
			out = append(out, ladder.LabelEvent{Label: e.Label, Actor: e.Actor, Added: e.Event == "labeled", At: e.At})
		}
	}
	return out
}

// Pulls returns every open pull request in the repositories polled, with its
// decision. A repository that cannot be read is logged and skipped, so one broken
// repository does not stop the rest.
func Pulls(ctx context.Context, src Source, policies Policies, logger *slog.Logger) ([]Candidate, int, error) {
	repos := policies.Repos
	if len(repos) == 0 {
		var err error
		if repos, err = src.ListInstallationRepos(ctx); err != nil {
			return nil, 0, err
		}
	}
	var candidates []Candidate
	errorCount := 0
	for _, repo := range repos {
		pulls, err := src.ListOpenPulls(ctx, repo)
		if err != nil {
			logger.WarnContext(ctx, "list pulls", "repo", repo, "err", err.Error())
			errorCount++
			continue
		}
		for _, pr := range pulls {
			data, err := Gather(ctx, src, policies.Pull, pr)
			if err != nil {
				logger.WarnContext(ctx, "read pull", "repo", repo, "number", pr.Number, "err", err.Error())
				errorCount++
				continue
			}
			rounds, _ := policies.Pull.FixRounds(data)
			candidates = append(candidates, Candidate{Pull: pr, Rounds: rounds, ReReviews: policies.Pull.ReReviews(data), Decision: policies.Pull.Decide(data)})
		}
	}
	return candidates, errorCount, nil
}

// GatherIssue reads what the build decision needs about one open issue: who put
// the ready label on it, which pull requests name it, and which branches exist for
// it in the target repository.
func GatherIssue(ctx context.Context, src Source, policy ladder.BuildPolicy, issue github.Issue) (ladder.Issue, error) {
	out := ladder.Issue{Repo: issue.Repo, Number: issue.Number, Labels: issue.Labels}
	events, err := src.ListTimeline(ctx, issue.Repo, issue.Number)
	if err != nil {
		return out, err
	}
	out.LabelEvents = labelEvents(events)
	for _, e := range events {
		if e.Event == "cross-referenced" && e.Source != nil && ladder.RefersTo(e.Source.Body, issue.Repo, issue.Number) {
			out.LinkedPRs = append(out.LinkedPRs, ladder.LinkedPR{Repo: e.Source.Repo, Number: e.Source.Number, Author: e.Source.Author,
				Open: e.Source.State == "open", CreatedAt: e.Source.CreatedAt})
		}
	}
	if repo, _ := policy.Target(issue.Labels); repo != "" {
		if out.Branches, err = src.ListBranches(ctx, repo, ladder.BranchPrefix(issue.Number)); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Issues returns every open issue carrying the ready label, with its decision.
// With no maintainer, no ready label, no build repositories or no issue repository
// configured there are none: builds are off.
func Issues(ctx context.Context, src Source, policies Policies, logger *slog.Logger) ([]BuildCandidate, int, error) {
	policy := policies.Build
	if policy.ReadyLabel == "" || policy.Maintainer == "" || len(policy.Repos) == 0 || policies.IssueRepo == "" {
		return nil, 0, nil
	}
	issues, err := src.ListOpenIssues(ctx, policies.IssueRepo, []string{policy.ReadyLabel})
	if err != nil {
		return nil, 0, err
	}
	var candidates []BuildCandidate
	errorCount := 0
	for _, issue := range issues {
		data, err := GatherIssue(ctx, src, policy, issue)
		if err != nil {
			logger.WarnContext(ctx, "read issue", "repo", issue.Repo, "number", issue.Number, "err", err.Error())
			errorCount++
			continue
		}
		candidates = append(candidates, BuildCandidate{Issue: issue, Decision: policy.Decide(data)})
	}
	return candidates, errorCount, nil
}
