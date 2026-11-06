package poller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/those-who-run/pit-crew/internal/ladder"
)

// StatusWriter sets commit statuses. *github.Client is one.
type StatusWriter interface {
	SetStatus(ctx context.Context, repo, sha, context, state, description string) error
}

// ReadyEntry is one pull request on the ready list.
type ReadyEntry struct {
	Repo    string
	Number  int
	Title   string
	Author  string
	Verdict ladder.Verdict
	URL     string
}

// Ready makes the no-model pass over every open pull request: it sets
// `ladder / ready` where the verdict and the checks allow, takes it back to
// pending where they no longer do, sets `ladder / stuck` where the fix rounds are
// used up, and returns the ready list. It writes a status only when the one on the
// head differs.
func Ready(ctx context.Context, src Source, writer StatusWriter, policies Policies, logger *slog.Logger) ([]ReadyEntry, error) {
	repos := policies.Repos
	if len(repos) == 0 {
		var err error
		if repos, err = src.ListInstallationRepos(ctx); err != nil {
			return nil, err
		}
	}
	var list []ReadyEntry
	for _, repo := range repos {
		pulls, err := src.ListOpenPulls(ctx, repo)
		if err != nil {
			logger.WarnContext(ctx, "list pulls", "repo", repo, "err", err.Error())
			continue
		}
		for _, pr := range pulls {
			data, err := Gather(ctx, src, policies.Pull, pr)
			if err != nil {
				logger.WarnContext(ctx, "read pull", "repo", repo, "number", pr.Number, "err", err.Error())
				continue
			}
			decision := policies.Pull.Decide(data)
			if err := applyStatuses(ctx, writer, policies.Pull, data, decision); err != nil {
				logger.WarnContext(ctx, "set status", "repo", repo, "number", pr.Number, "err", err.Error())
				continue
			}
			if decision.Work == ladder.WorkReady {
				list = append(list, ReadyEntry{Repo: pr.Repo, Number: pr.Number, Title: pr.Title, Author: pr.Author, Verdict: decision.Verdict, URL: pr.URL})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Repo != list[j].Repo {
			return list[i].Repo < list[j].Repo
		}
		return list[i].Number < list[j].Number
	})
	return list, nil
}

// current returns the newest status of a context on the head set by the reviewer.
func current(policy ladder.Policy, pr ladder.PullRequest, context string) (ladder.Status, bool) {
	var found ladder.Status
	ok := false
	for _, s := range pr.Statuses {
		if s.Context == context && strings.EqualFold(s.Creator, policy.Reviewer) && (!ok || s.CreatedAt.After(found.CreatedAt)) {
			found, ok = s, true
		}
	}
	return found, ok
}

func applyStatuses(ctx context.Context, writer StatusWriter, policy ladder.Policy, pr ladder.PullRequest, decision ladder.Decision) error {
	set := func(context, state, description string) error {
		if have, ok := current(policy, pr, context); ok && have.State == state && have.Description == description {
			return nil
		}
		return writer.SetStatus(ctx, pr.Repo, pr.HeadSHA, context, state, description)
	}
	switch decision.Work {
	case ladder.WorkReady:
		return set(ladder.StatusReady, "success", fmt.Sprintf("verdict %s, every required check green", decision.Verdict))
	case ladder.WorkStuck:
		if err := set(ladder.StatusStuck, "failure", fmt.Sprintf("%d fix rounds used; over to the maintainer", decision.Round)); err != nil {
			return err
		}
	case ladder.WorkNone:
		if decision.Reason == "author-not-allowlisted" || decision.Reason == "fork" || decision.Reason == "draft" || decision.Reason == "hold" ||
			decision.Reason == "commit-not-allowlisted" || decision.Reason == "no-head" || decision.Reason == "no-commits" {
			return nil
		}
	}
	// Ready was set on this head and no longer holds: say so rather than leave a
	// green light on.
	if have, ok := current(policy, pr, ladder.StatusReady); ok && have.State == "success" {
		return set(ladder.StatusReady, "pending", "no longer ready: "+decision.Reason)
	}
	return nil
}

// WriteReadyList writes the ready list as Markdown, newest file wins.
func WriteReadyList(path string, list []ReadyEntry) error {
	var b strings.Builder
	b.WriteString("# Ready to merge\n\nPull requests with a ready or nits verdict at their head and every required check green.\n\n")
	if len(list) == 0 {
		b.WriteString("Nothing is ready.\n")
	}
	for _, e := range list {
		fmt.Fprintf(&b, "- [%s#%d](%s): %s (%s, verdict %s)\n", e.Repo, e.Number, e.URL, e.Title, e.Author, e.Verdict)
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
