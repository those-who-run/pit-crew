// Package review is `pit-crew publish-review`: the step after the model that
// checks what the reviewer wrote and posts it, or holds it for the maintainer.
// It holds the reviewer App's token and no model.
package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/ladder"
	"github.com/those-who-run/pit-crew/internal/lanes"
	"github.com/those-who-run/pit-crew/internal/publish"
	"github.com/those-who-run/pit-crew/internal/steps"
)

// Config is the step's configuration.
type Config struct {
	Repo    string
	Number  int
	HeadSHA string
	// Reviewer is the login the token acts as; an existing comment of its own for
	// this head is updated rather than repeated.
	Reviewer string
	// ReReviewLabel is the label the maintainer asks for another review with; it
	// comes off once this review has posted or held, so one label is one run,
	// unless the maintainer applied it after the fetch step read the pull request.
	ReReviewLabel string
	// Maintainer is the login whose re-review label counts.
	Maintainer   string
	Dir          string
	HoldDir      string
	AllowFile    string
	PatternsFile string
}

// ConfigFromEnv reads the configuration.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Repo: getenv("PUBLISH_REPO"), HeadSHA: getenv("PUBLISH_HEAD_SHA"), Reviewer: getenv("LADDER_REVIEWER"), ReReviewLabel: getenv("LADDER_REREVIEW_LABEL"),
		Maintainer: getenv("LADDER_MAINTAINER"),
		Dir:        getenv("PUBLISH_DIR"), HoldDir: getenv("PUBLISH_HOLD_DIR"), AllowFile: getenv("PUBLISH_ALLOW_FILE"), PatternsFile: getenv("PUBLISH_PATTERNS_FILE"),
	}
	if cfg.Dir == "" {
		cfg.Dir = "/work"
	}
	if cfg.Reviewer == "" {
		cfg.Reviewer = "twr-reviewer[bot]"
	}
	if cfg.ReReviewLabel == "" {
		cfg.ReReviewLabel = "re-review"
	}
	var err error
	if cfg.Number, err = strconv.Atoi(getenv("PUBLISH_NUMBER")); err != nil || cfg.Number < 1 {
		return cfg, errors.New("PUBLISH_NUMBER: not a pull request number")
	}
	if cfg.Repo == "" || !ladder.IsSHA(cfg.HeadSHA) {
		return cfg, errors.New("PUBLISH_REPO and PUBLISH_HEAD_SHA are needed")
	}
	if cfg.HoldDir == "" {
		return cfg, errors.New("PUBLISH_HOLD_DIR is needed: held comments and private.md must have somewhere to go")
	}
	return cfg, nil
}

// GitHub is what the step does on GitHub. *github.Client is one.
type GitHub interface {
	GetPull(ctx context.Context, repo string, number int) (github.PullRequest, error)
	ListComments(ctx context.Context, repo string, number int) ([]github.Comment, error)
	CreateComment(ctx context.Context, repo string, number int, body string) (int64, error)
	UpdateComment(ctx context.Context, repo string, id int64, body string) error
	SetStatus(ctx context.Context, repo, sha, context, state, description string) error
	RemoveLabel(ctx context.Context, repo string, number int, name string) error
	AddLabel(ctx context.Context, repo string, number int, name string) error
	ListTimeline(ctx context.Context, repo string, number int) ([]github.TimelineEvent, error)
}

// Run does the step and returns its exit code.
func Run(ctx context.Context, cfg Config, gh GitHub, logger *slog.Logger) int {
	code, err := run(ctx, cfg, gh, logger)
	if err != nil {
		logger.ErrorContext(ctx, "publish-review failed", "repo", cfg.Repo, "number", cfg.Number, "err", err.Error())
	}
	return code
}

func run(ctx context.Context, cfg Config, gh GitHub, logger *slog.Logger) (int, error) {
	task, err := steps.ReadTask(cfg.Dir)
	if err != nil {
		return steps.ExitConfig, err
	}
	if task.Kind != "review" || !strings.EqualFold(task.Repo, cfg.Repo) || task.Number != cfg.Number || task.HeadSHA != cfg.HeadSHA {
		return steps.ExitConfig, fmt.Errorf("task.json is for %s %s#%d at %s; this step was given %s#%d at %s",
			task.Kind, task.Repo, task.Number, task.HeadSHA, cfg.Repo, cfg.Number, cfg.HeadSHA)
	}
	scanner, err := steps.Scanner(cfg.AllowFile, cfg.PatternsFile)
	if err != nil {
		return steps.ExitConfig, err
	}
	var files publish.ReviewFiles
	for name, into := range map[string]*[]byte{"verdict.json": &files.Verdict, "comment.md": &files.Comment, "private.md": &files.Private} {
		if *into, err = steps.ReadOutput(cfg.Dir, name); err != nil {
			return steps.ExitFailed, err
		}
	}
	pr, err := gh.GetPull(ctx, cfg.Repo, cfg.Number)
	if err != nil {
		return steps.ExitFailed, err
	}
	result := publish.CheckReview(publish.ReviewTask{Repo: cfg.Repo, PR: cfg.Number, HeadSHA: cfg.HeadSHA, Lane: lanes.Lane(task.Lane)}, pr.HeadSHA, files, scanner)
	if result.Outcome == publish.ReviewHeadMoved {
		logger.InfoContext(ctx, "head moved", "repo", cfg.Repo, "number", cfg.Number, "was", cfg.HeadSHA, "now", pr.HeadSHA)
		return steps.ExitHeadMoved, nil
	}
	// private.md never goes to GitHub; held output goes nowhere else.
	if files.Private != nil || result.Outcome == publish.ReviewHold {
		dir, err := steps.HoldDir(cfg.HoldDir, cfg.Repo, cfg.Number, cfg.HeadSHA)
		if err != nil {
			return steps.ExitFailed, err
		}
		held := map[string][]byte{"private.md": files.Private}
		if result.Outcome == publish.ReviewHold {
			// A hold keeps everything the model wrote, agent.json and agent.md
			// included, so a run that wrote nothing else still shows what it said.
			if held, err = steps.ReadOutputs(cfg.Dir); err != nil {
				return steps.ExitFailed, err
			}
			var reasons strings.Builder
			fmt.Fprintf(&reasons, "Review of %s#%d at %s held.\n\n", cfg.Repo, cfg.Number, cfg.HeadSHA)
			for _, r := range result.Refusals {
				fmt.Fprintf(&reasons, "- %s: %s\n", r.Code, r.Detail)
			}
			held["reasons.md"] = []byte(reasons.String())
		}
		if err := steps.Hold(dir, held); err != nil {
			return steps.ExitFailed, err
		}
		logger.InfoContext(ctx, "held", "repo", cfg.Repo, "number", cfg.Number, "dir", dir, "codes", strings.Join(publish.Codes(result.Refusals), ","))
	}
	if result.Reason != "" {
		// The model's words, as a value: the Job's log is the only place they go.
		logger.InfoContext(ctx, "review reason", "repo", cfg.Repo, "number", cfg.Number, "verdict", string(result.Verdict), "reason", result.Reason)
	}
	if result.Outcome == publish.ReviewPost && result.Body != "" {
		if err := post(ctx, cfg, gh, result.Body); err != nil {
			return steps.ExitFailed, err
		}
	}
	if err := gh.SetStatus(ctx, cfg.Repo, cfg.HeadSHA, ladder.StatusVerdict, result.StatusState, result.StatusDescription); err != nil {
		return steps.ExitFailed, err
	}
	logger.InfoContext(ctx, "published", "repo", cfg.Repo, "number", cfg.Number, "outcome", string(result.Outcome), "verdict", string(result.Verdict), "status", result.StatusDescription)
	if ladder.HasLabel(pr.Labels, cfg.ReReviewLabel) {
		reReviewLabel(ctx, cfg, gh, task.FetchedAt, logger)
	}
	if result.Outcome == publish.ReviewHold {
		return steps.ExitHeld, nil
	}
	return steps.ExitOK, nil
}

// reReviewLabel takes the re-review label off once the review is on GitHub, when
// the review answered it: the maintainer's last ask is no newer than the moment
// the fetch step read the pull request. An ask that came while the Job ran is
// still owed a review, and this one's record is newer than it, so the label is
// handed back: taken off and put on again as the reviewer, which the ladder
// counts as the maintainer's ask carried over. All of it is best-effort: the
// review is posted, and a call GitHub refuses is a warning, not a failed step.
func reReviewLabel(ctx context.Context, cfg Config, gh GitHub, fetchedAt time.Time, logger *slog.Logger) {
	attrs := []any{"repo", cfg.Repo, "number", cfg.Number, "label", cfg.ReReviewLabel}
	asked := false
	// A task.json from before fetch recorded the time says nothing about when the
	// review read the pull request; the label comes off, as it always did.
	if !fetchedAt.IsZero() {
		timeline, err := gh.ListTimeline(ctx, cfg.Repo, cfg.Number)
		if err != nil {
			// Without the events nothing says the ask was answered: leave it, and the
			// next review of the pull request takes it off.
			logger.WarnContext(ctx, "re-review label left on: its events could not be read", append(attrs, "err", err.Error())...)
			return
		}
		var events []ladder.LabelEvent
		for _, e := range timeline {
			if e.Event == "labeled" || e.Event == "unlabeled" {
				events = append(events, ladder.LabelEvent{Label: e.Label, Actor: e.Actor, Added: e.Event == "labeled", At: e.At})
			}
		}
		policy := ladder.Policy{Maintainer: cfg.Maintainer, ReReviewLabel: cfg.ReReviewLabel}
		asked = policy.ReReviewAskedAfter(events, fetchedAt)
	}
	if err := gh.RemoveLabel(ctx, cfg.Repo, cfg.Number, cfg.ReReviewLabel); err != nil {
		logger.WarnContext(ctx, "remove label", append(attrs, "err", err.Error())...)
		return
	}
	if !asked {
		logger.InfoContext(ctx, "removed label", attrs...)
		return
	}
	if err := gh.AddLabel(ctx, cfg.Repo, cfg.Number, cfg.ReReviewLabel); err != nil {
		logger.WarnContext(ctx, "re-review asked during the review, and the label could not be put back: apply it again", append(attrs, "err", err.Error())...)
		return
	}
	logger.InfoContext(ctx, "re-review asked during the review: label handed back", append(attrs, "fetched_at", fetchedAt)...)
}

// post creates the comment, or updates the reviewer's own comment for this head
// when there is one, so a head gets one comment however many times it is read.
func post(ctx context.Context, cfg Config, gh GitHub, body string) error {
	comments, err := gh.ListComments(ctx, cfg.Repo, cfg.Number)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if !strings.EqualFold(c.Author, cfg.Reviewer) {
			continue
		}
		for _, m := range ladder.ParseReviewMarkers(c.Body) {
			if m.SHA == cfg.HeadSHA {
				return gh.UpdateComment(ctx, cfg.Repo, c.ID, body)
			}
		}
	}
	_, err = gh.CreateComment(ctx, cfg.Repo, cfg.Number, body)
	return err
}
