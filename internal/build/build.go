// Package build is `pit-crew publish-build`: the step after the model that checks
// the builder's commits and its pull request text, then pushes the branch and
// opens the pull request (a build) or posts the answer to a review (a fix). It
// holds the builder App's token and no model.
//
// It never runs git inside the model's repository. The model can write that
// repository's .git/config and .git/hooks, which git would honour in the step
// that holds the token. The step makes a repository of its own, reads the model's
// objects through GIT_ALTERNATE_OBJECT_DIRECTORIES, reads the branch's head from
// its ref file, and does every check and the push from there.
package build

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/those-who-run/pit-crew/internal/fetch"
	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/ladder"
	"github.com/those-who-run/pit-crew/internal/lanes"
	"github.com/those-who-run/pit-crew/internal/publish"
	"github.com/those-who-run/pit-crew/internal/steps"
)

// Config is the step's configuration.
type Config struct {
	Mode      string
	Repo      string
	IssueRepo string
	Issue     int
	// Number, HeadSHA and Round are a fix's: the pull request, the head the review
	// was of, the round.
	Number  int
	HeadSHA string
	Round   int
	// BaseRef is the base branch, main by default.
	BaseRef string
	Builder publish.Identity
	// Protected are paths a build may not touch, in lanes.json's syntax.
	Protected    []string
	Dir          string
	HoldDir      string
	AllowFile    string
	PatternsFile string
	// GitBase and Token are the push's; the tests point GitBase at a directory.
	GitBase string
	Token   string
	// Scratch is where the step's own repository goes; empty means a temp dir.
	Scratch string
}

// ConfigFromEnv reads the configuration.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Mode: getenv("PUBLISH_MODE"), Repo: getenv("PUBLISH_REPO"), IssueRepo: getenv("PUBLISH_ISSUE_REPO"), HeadSHA: getenv("PUBLISH_HEAD_SHA"),
		BaseRef: or(getenv("PUBLISH_BASE_REF"), "main"),
		Builder: publish.Identity{Name: getenv("PUBLISH_BUILDER_NAME"), Email: getenv("PUBLISH_BUILDER_EMAIL")},
		Dir:     or(getenv("PUBLISH_DIR"), "/work"), HoldDir: getenv("PUBLISH_HOLD_DIR"),
		AllowFile: getenv("PUBLISH_ALLOW_FILE"), PatternsFile: getenv("PUBLISH_PATTERNS_FILE"),
		GitBase: or(getenv("PUBLISH_GIT_BASE"), "https://github.com/"),
	}
	cfg.Protected = publish.DefaultProtected
	if v := getenv("PUBLISH_PROTECTED"); v != "" {
		cfg.Protected = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.Protected = append(cfg.Protected, p)
			}
		}
	}
	var err error
	for _, n := range []struct {
		name string
		into *int
	}{{"PUBLISH_ISSUE", &cfg.Issue}, {"PUBLISH_NUMBER", &cfg.Number}, {"PUBLISH_ROUND", &cfg.Round}} {
		if v := getenv(n.name); v != "" {
			if *n.into, err = strconv.Atoi(v); err != nil || *n.into < 0 {
				return cfg, fmt.Errorf("%s: %q is not a number", n.name, v)
			}
		}
	}
	switch cfg.Mode {
	case publish.ModeBuild:
	case publish.ModeFix:
		if cfg.Number == 0 || !ladder.IsSHA(cfg.HeadSHA) || cfg.Round == 0 {
			return cfg, errors.New("PUBLISH_NUMBER, PUBLISH_HEAD_SHA and PUBLISH_ROUND are needed for a fix")
		}
	default:
		return cfg, fmt.Errorf("PUBLISH_MODE: %q is not build or fix", cfg.Mode)
	}
	if cfg.Repo == "" || cfg.IssueRepo == "" || cfg.Issue == 0 {
		return cfg, errors.New("PUBLISH_REPO, PUBLISH_ISSUE_REPO and PUBLISH_ISSUE are needed")
	}
	if cfg.Builder.Name == "" || cfg.Builder.Email == "" {
		return cfg, errors.New("PUBLISH_BUILDER_NAME and PUBLISH_BUILDER_EMAIL are needed")
	}
	if cfg.HoldDir == "" {
		return cfg, errors.New("PUBLISH_HOLD_DIR is needed")
	}
	return cfg, nil
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// GitHub is what the step does on GitHub. *github.Client is one.
type GitHub interface {
	GetPull(ctx context.Context, repo string, number int) (github.PullRequest, error)
	ListComments(ctx context.Context, repo string, number int) ([]github.Comment, error)
	CreateComment(ctx context.Context, repo string, number int, body string) (int64, error)
	CreatePull(ctx context.Context, repo, title, body, head, base string) (int, string, error)
	UpdatePullBody(ctx context.Context, repo string, number int, body string) error
}

// Run does the step and returns its exit code.
func Run(ctx context.Context, cfg Config, gh GitHub, logger *slog.Logger) int {
	code, err := run(ctx, cfg, gh, logger)
	if err != nil {
		logger.ErrorContext(ctx, "publish-build failed", "mode", cfg.Mode, "repo", cfg.Repo, "issue", cfg.Issue, "err", err.Error())
	}
	return code
}

func run(ctx context.Context, cfg Config, gh GitHub, logger *slog.Logger) (int, error) {
	task, err := steps.ReadTask(cfg.Dir)
	if err != nil {
		return steps.ExitConfig, err
	}
	if task.Kind != cfg.Mode || !strings.EqualFold(task.Repo, cfg.Repo) || task.Issue != cfg.Issue || !strings.EqualFold(task.IssueRepo, cfg.IssueRepo) ||
		(cfg.Mode == publish.ModeFix && (task.Number != cfg.Number || task.HeadSHA != cfg.HeadSHA || task.Round != cfg.Round)) {
		return steps.ExitConfig, fmt.Errorf("task.json (%s %s, issue %s#%d, pull request %d at %s, round %d) is not this step's (%s %s, issue %s#%d, pull request %d at %s, round %d)",
			task.Kind, task.Repo, task.IssueRepo, task.Issue, task.Number, task.HeadSHA, task.Round, cfg.Mode, cfg.Repo, cfg.IssueRepo, cfg.Issue, cfg.Number, cfg.HeadSHA, cfg.Round)
	}
	scanner, err := steps.Scanner(cfg.AllowFile, cfg.PatternsFile)
	if err != nil {
		return steps.ExitConfig, err
	}
	laneConfig, err := lanes.Load(filepath.Join(cfg.Dir, fetch.AgentDir, "agent", "lanes.json"))
	if err != nil {
		return steps.ExitConfig, err
	}
	buildTask := publish.BuildTask{Mode: cfg.Mode, Repo: cfg.Repo, IssueRepo: cfg.IssueRepo, Issue: cfg.Issue, Branch: task.Branch,
		BaseSHA: task.BaseSHA, PrevHead: task.HeadSHA, Round: cfg.Round}
	facts := publish.BuildFacts{}
	for name, into := range map[string]*string{"pr-title.txt": &facts.Title, "pr-body.md": &facts.Body, "answer.md": &facts.Answer} {
		data, err := steps.ReadOutput(cfg.Dir, name)
		if err != nil {
			return steps.ExitFailed, err
		}
		*into = string(data)
	}
	var pr github.PullRequest
	rewrote := false
	if cfg.Mode == publish.ModeFix {
		if pr, err = gh.GetPull(ctx, cfg.Repo, cfg.Number); err != nil {
			return steps.ExitFailed, err
		}
		if pr.HeadSHA != cfg.HeadSHA {
			logger.InfoContext(ctx, "head moved", "repo", cfg.Repo, "number", cfg.Number, "was", cfg.HeadSHA, "now", pr.HeadSHA)
			return steps.ExitHeadMoved, nil
		}
		// The pull request's own title and body stand unless the builder rewrote
		// the body (the evidence table changed); a rewritten body is checked like a
		// new one and applied after the push.
		facts.Title = pr.Title
		rewrote = strings.TrimSpace(facts.Body) != ""
		if !rewrote {
			facts.Body = pr.Body
		}
		comments, err := gh.ListComments(ctx, cfg.Repo, cfg.Number)
		if err != nil {
			return steps.ExitFailed, err
		}
		var data ladder.PullRequest
		for _, c := range comments {
			data.Comments = append(data.Comments, ladder.Comment{Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
		}
		buildTask.RoundsUsed, _ = ladder.Policy{Builder: cfg.Builder.Name}.FixRounds(data)
	}
	work, err := newWorkspace(ctx, cfg, filepath.Join(cfg.Dir, fetch.RepoDir), task.Branch)
	if err != nil {
		return steps.ExitFailed, err
	}
	defer work.close()
	if err := work.facts(ctx, &facts, buildTask); err != nil {
		return steps.ExitFailed, err
	}
	refusals := publish.CheckBuild(buildTask, facts, publish.BuildRules{Builder: cfg.Builder, Protected: cfg.Protected, Lanes: laneConfig, Scanner: scanner})
	if len(refusals) > 0 {
		return hold(ctx, cfg, task, facts, refusals, logger)
	}
	if _, err := work.git(ctx, "push", "--quiet", cfg.url(), facts.HeadSHA+":refs/heads/"+task.Branch); err != nil {
		return steps.ExitFailed, err
	}
	switch cfg.Mode {
	case publish.ModeBuild:
		number, url, err := gh.CreatePull(ctx, cfg.Repo, strings.TrimSpace(facts.Title), facts.Body, task.Branch, cfg.BaseRef)
		if err != nil {
			return steps.ExitFailed, err
		}
		logger.InfoContext(ctx, "opened", "repo", cfg.Repo, "number", number, "url", url, "branch", task.Branch, "commits", len(facts.Commits))
	case publish.ModeFix:
		if rewrote {
			if err := gh.UpdatePullBody(ctx, cfg.Repo, cfg.Number, facts.Body); err != nil {
				return steps.ExitFailed, err
			}
		}
		if _, err := gh.CreateComment(ctx, cfg.Repo, cfg.Number, publish.AnswerBody(facts.Answer, cfg.Round)); err != nil {
			return steps.ExitFailed, err
		}
		logger.InfoContext(ctx, "answered", "repo", cfg.Repo, "number", cfg.Number, "round", cfg.Round, "head", facts.HeadSHA, "commits", len(facts.Commits))
	}
	return steps.ExitOK, nil
}

func (c Config) url() string { return c.GitBase + c.Repo + ".git" }

func hold(ctx context.Context, cfg Config, task fetch.Task, facts publish.BuildFacts, refusals []publish.Refusal, logger *slog.Logger) (int, error) {
	dir, err := steps.HoldDir(cfg.HoldDir, cfg.Repo, cfg.Issue, facts.HeadSHA)
	if err != nil {
		return steps.ExitFailed, err
	}
	var reasons strings.Builder
	fmt.Fprintf(&reasons, "%s for %s#%d in %s, branch %s at %s, held.\n\n", cfg.Mode, cfg.IssueRepo, cfg.Issue, cfg.Repo, task.Branch, facts.HeadSHA)
	for _, r := range refusals {
		fmt.Fprintf(&reasons, "- %s: %s\n", r.Code, r.Detail)
	}
	reasons.WriteString("\nCommits:\n")
	for _, c := range facts.Commits {
		fmt.Fprintf(&reasons, "- %s %s <%s>: %s\n", c.SHA, c.Author.Name, c.Author.Email, strings.SplitN(c.Message, "\n", 2)[0])
	}
	files := map[string][]byte{"reasons.md": []byte(reasons.String()), "pr-title.txt": []byte(facts.Title), "pr-body.md": []byte(facts.Body), "patch.diff": []byte(facts.Patch)}
	if facts.Answer != "" {
		files["answer.md"] = []byte(facts.Answer)
	}
	if err := steps.Hold(dir, files); err != nil {
		return steps.ExitFailed, err
	}
	logger.InfoContext(ctx, "held", "mode", cfg.Mode, "repo", cfg.Repo, "issue", cfg.Issue, "dir", dir, "codes", strings.Join(publish.Codes(refusals), ","))
	return steps.ExitHeld, nil
}

// workspace is the step's own bare repository, reading the model's objects
// through an alternates path.
type workspace struct {
	cfg     Config
	dir     string
	objects string
	cleanup func()
}

// newWorkspace checks the model's repository is a plain one (no symbolic links
// where git would follow them into the step's own files) and makes the step's.
func newWorkspace(ctx context.Context, cfg Config, untrusted, branch string) (*workspace, error) {
	gitDir := filepath.Join(untrusted, ".git")
	for _, p := range []string{gitDir, filepath.Join(gitDir, "objects")} {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("the checkout: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("the checkout: %s is not a directory", p)
		}
	}
	if branch == "" {
		return nil, errors.New("task.json names no branch")
	}
	scratch := cfg.Scratch
	cleanup := func() {}
	if scratch == "" {
		var err error
		if scratch, err = os.MkdirTemp("", "pit-crew-publish-"); err != nil {
			return nil, err
		}
		cleanup = func() { _ = os.RemoveAll(scratch) }
	}
	w := &workspace{cfg: cfg, dir: filepath.Join(scratch, "publish.git"), objects: filepath.Join(gitDir, "objects"), cleanup: cleanup}
	if _, err := w.git(ctx, "init", "--quiet", "--bare", w.dir); err != nil {
		cleanup()
		return nil, err
	}
	return w, nil
}

func (w *workspace) close() { w.cleanup() }

// git runs one git command in the step's repository, the token in its
// environment only.
func (w *workspace) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // the arguments are the step's own: refs it checked and paths it made
	if args[0] != "init" {
		cmd.Dir = w.dir
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALTERNATE_OBJECT_DIRECTORIES="+w.objects, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if w.cfg.Token != "" {
		header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+w.cfg.Token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+w.cfg.GitBase+".extraheader", "GIT_CONFIG_VALUE_0="+header)
	}
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// HeadOf reads where the untrusted repository's HEAD points and the commit that
// ref names, from the files alone: HEAD, a loose ref, or packed-refs.
func HeadOf(gitDir string) (ref, sha string, err error) {
	head, err := readPlain(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", "", err
	}
	line := strings.TrimSpace(head)
	target, ok := strings.CutPrefix(line, "ref: ")
	if !ok {
		if ladder.IsSHA(line) {
			return line, line, nil // detached
		}
		return "", "", fmt.Errorf("HEAD is %q", line)
	}
	if strings.Contains(target, "..") || !strings.HasPrefix(target, "refs/") {
		return "", "", fmt.Errorf("HEAD names %q", target)
	}
	if loose, err := readPlain(filepath.Join(gitDir, filepath.FromSlash(target))); err == nil {
		sha = strings.TrimSpace(loose)
	} else if packed, err := readPlain(filepath.Join(gitDir, "packed-refs")); err == nil {
		scanner := bufio.NewScanner(strings.NewReader(packed))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 2 && fields[1] == target {
				sha = fields[0]
			}
		}
	}
	if !ladder.IsSHA(sha) {
		return target, "", fmt.Errorf("%s names no commit", target)
	}
	return target, sha, nil
}

// readPlain reads a file that must be a plain file, not a symbolic link.
func readPlain(p string) (string, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a plain file", p)
	}
	data, err := os.ReadFile(p) //nolint:gosec // a fixed name under the checkout's .git
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// facts reads what CheckBuild needs from the step's own repository.
func (w *workspace) facts(ctx context.Context, facts *publish.BuildFacts, task publish.BuildTask) error {
	ref, sha, err := HeadOf(filepath.Join(w.cfg.Dir, fetch.RepoDir, ".git"))
	if err != nil {
		return fmt.Errorf("the checkout's HEAD: %w", err)
	}
	facts.HeadRef, facts.HeadSHA = ref, sha
	if _, err := w.git(ctx, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return fmt.Errorf("the branch's head %s is not a commit the checkout holds: %w", sha, err)
	}
	// The base branch as it is now, from the remote, so a fix that merged a newer
	// main is compared against that main.
	if _, err := w.git(ctx, "fetch", "--quiet", w.cfg.url(), "refs/heads/"+w.cfg.BaseRef+":refs/heads/base"); err != nil {
		return err
	}
	facts.BaseIsAncestor = w.isAncestor(ctx, task.BaseSHA, sha)
	if task.PrevHead != "" {
		facts.PrevIsAncestor = w.isAncestor(ctx, task.PrevHead, sha)
	}
	if facts.Commits, err = w.commits(ctx, "base.."+sha); err != nil {
		return err
	}
	// A fix is judged on the commits it added after the reviewed head. When that head
	// is not an ancestor the fix is refused as rewritten, so there is nothing to read.
	if task.Mode == publish.ModeFix && facts.PrevIsAncestor {
		if facts.NewCommits, err = w.commits(ctx, task.PrevHead+".."+sha); err != nil {
			return err
		}
	}
	status, err := w.git(ctx, "diff", "--name-status", "-M", "base..."+sha)
	if err != nil {
		return err
	}
	if facts.Changes, err = lanes.ParseNameStatus(string(status)); err != nil {
		return err
	}
	patch, err := w.git(ctx, "diff", "--unified=0", "-M", "--no-color", "base..."+sha)
	if err != nil {
		return err
	}
	facts.Patch = string(patch)
	return nil
}

// commits lists the commits in a revision range, newest first.
func (w *workspace) commits(ctx context.Context, rng string) ([]publish.GitCommit, error) {
	log, err := w.git(ctx, "log", "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%B%x1e", rng)
	if err != nil {
		return nil, err
	}
	var commits []publish.GitCommit
	for _, record := range strings.Split(string(log), "\x1e") {
		fields := strings.SplitN(strings.TrimLeft(record, "\n"), "\x00", 6)
		if len(fields) < 6 {
			continue
		}
		commits = append(commits, publish.GitCommit{SHA: fields[0], Author: publish.Identity{Name: fields[1], Email: fields[2]},
			Committer: publish.Identity{Name: fields[3], Email: fields[4]}, Message: fields[5]})
	}
	return commits, nil
}

func (w *workspace) isAncestor(ctx context.Context, ancestor, sha string) bool {
	if !ladder.IsSHA(ancestor) {
		return false
	}
	_, err := w.git(ctx, "merge-base", "--is-ancestor", ancestor, sha)
	return err == nil
}
