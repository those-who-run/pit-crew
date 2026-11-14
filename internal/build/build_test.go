package build

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/those-who-run/pit-crew/internal/fetch"
	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/publish"
)

var builder = publish.Identity{Name: "twr-builder[bot]", Email: "1+twr-builder[bot]@users.noreply.github.com"}

// git runs git for a fixture as the identity given.
func git(t *testing.T, dir string, who publish.Identity, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // the test's own fixture commands
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME="+who.Name, "GIT_AUTHOR_EMAIL="+who.Email, "GIT_COMMITTER_NAME="+who.Name,
		"GIT_COMMITTER_EMAIL="+who.Email, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil { //nolint:gosec // the test's temp dir
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p) //nolint:gosec // the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const body = "## What changed\n\nThe callout.\n\nAI assistance: Claude Code wrote it.\n\nNew direct dependencies: none\n\n## How it was checked\n\n" +
	"| Criterion | Proof | Where |\n|---|---|---|\n| AC1 | unit | `race/fuel_test.go` |\n\n" +
	"- [x] The command that matches CI (`make check`) is green\n- [x] Docs updated, or none needed because: none needed\n\nCloses those-who-run/paddock#232.\n"

// fixture is an origin with main, and /work as fetch left it for a build: the
// clone on agent/232-fuel at main, with the builder's identity set.
type fixture struct {
	origin  string // the directory of bare repositories, as a git base
	work    string
	repo    string
	mainSHA string
	gh      *gh
	cfg     Config
}

type gh struct {
	pull    github.PullRequest
	pulls   []string
	answers []string
	bodies  []string
}

func (g *gh) UpdatePullBody(_ context.Context, _ string, _ int, body string) error {
	g.bodies = append(g.bodies, body)
	return nil
}

func (g *gh) GetPull(context.Context, string, int) (github.PullRequest, error)    { return g.pull, nil }
func (g *gh) ListComments(context.Context, string, int) ([]github.Comment, error) { return nil, nil }
func (g *gh) CreateComment(_ context.Context, _ string, _ int, body string) (int64, error) {
	g.answers = append(g.answers, body)
	return 1, nil
}
func (g *gh) CreatePull(_ context.Context, repo, title, body, head, base string) (int, string, error) {
	g.pulls = append(g.pulls, strings.Join([]string{repo, title, head, base, body}, "\n"))
	return 33, "https://github.com/" + repo + "/pull/33", nil
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "those-who-run")
	seed := filepath.Join(root, "seed")
	git(t, root, builder, "init", "-q", "-b", "main", seed)
	write(t, seed, "README.md", "# ecu\n")
	write(t, seed, "race/race.go", "package race\n")
	write(t, seed, ".claude/settings.json", "{}\n")
	git(t, seed, builder, "add", ".")
	git(t, seed, builder, "commit", "-q", "-m", "feat: start")
	mainSHA := git(t, seed, builder, "rev-parse", "HEAD")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatal(err)
	}
	git(t, root, builder, "clone", "-q", "--bare", seed, filepath.Join(base, "ecu.git"))
	work := filepath.Join(root, "work")
	repo := filepath.Join(work, fetch.RepoDir)
	git(t, root, builder, "clone", "-q", filepath.Join(base, "ecu.git"), repo)
	git(t, repo, builder, "config", "user.name", builder.Name)
	git(t, repo, builder, "config", "user.email", builder.Email)
	git(t, repo, builder, "checkout", "-q", "-b", "agent/232-fuel")
	lanesJSON := read(t, filepath.Join("..", "lanes", "testdata", "lanes.json"))
	write(t, work, "agent/agent/lanes.json", lanesJSON)
	task := fetch.Task{Kind: mode, Repo: "those-who-run/ecu", BaseRef: "main", BaseSHA: mainSHA, IssueRepo: "those-who-run/paddock", Issue: 232, Branch: "agent/232-fuel"}
	f := &fixture{origin: root + "/", work: work, repo: repo, mainSHA: mainSHA, gh: &gh{}}
	f.cfg = Config{Mode: mode, Repo: "those-who-run/ecu", IssueRepo: "those-who-run/paddock", Issue: 232, BaseRef: "main", Builder: builder,
		Protected: publish.DefaultProtected, Dir: work, HoldDir: filepath.Join(work, "hold"), GitBase: f.origin, Token: "ghs_" + strings.Repeat("x", 36)}
	if mode == publish.ModeFix {
		// The pull request's branch exists on the origin with one builder commit.
		write(t, repo, "race/fuel.go", "package race\n\nfunc Fuel() {}\n")
		git(t, repo, builder, "add", ".")
		git(t, repo, builder, "commit", "-q", "-m", "feat(race): fuel")
		git(t, repo, builder, "push", "-q", "origin", "agent/232-fuel")
		head := git(t, repo, builder, "rev-parse", "HEAD")
		task.Number, task.HeadSHA, task.Round = 31, head, 1
		f.cfg.Number, f.cfg.HeadSHA, f.cfg.Round = 31, head, 1
		f.gh.pull = github.PullRequest{Repo: "those-who-run/ecu", Number: 31, Title: "feat(race): fuel", Body: body, HeadSHA: head, HeadRef: "agent/232-fuel"}
	}
	taskJSON, _ := json.Marshal(task)
	write(t, work, "data/task.json", string(taskJSON))
	return f
}

// commit makes the model's commit in the checkout.
func (f *fixture) commit(t *testing.T, who publish.Identity, message string, files map[string]string) string {
	t.Helper()
	for name, text := range files {
		write(t, f.repo, name, text)
	}
	git(t, f.repo, who, "add", "-A")
	git(t, f.repo, who, "commit", "-q", "-m", message)
	return git(t, f.repo, who, "rev-parse", "HEAD")
}

func (f *fixture) originRef(t *testing.T, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", filepath.Join(f.origin, "those-who-run", "ecu.git"), "rev-parse", "--verify", "-q", ref) //nolint:gosec // the fixture
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func logger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestBuildPushesAndOpensThePullRequest(t *testing.T) {
	f := newFixture(t, publish.ModeBuild)
	head := f.commit(t, builder, "feat(race): fuel callout\n\nCo-Authored-By: Claude <noreply@anthropic.com>", map[string]string{"race/fuel.go": "package race\n\nfunc Fuel() {}\n", "race/fuel_test.go": "package race\n"})
	write(t, f.work, "out/pr-title.txt", "feat(race): fuel callout\n")
	write(t, f.work, "out/pr-body.md", body)
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := f.originRef(t, "refs/heads/agent/232-fuel"); got != head {
		t.Errorf("origin's branch is at %q, want %s", got, head)
	}
	if len(f.gh.pulls) != 1 || !strings.HasPrefix(f.gh.pulls[0], "those-who-run/ecu\nfeat(race): fuel callout\nagent/232-fuel\nmain\n## What changed") {
		t.Errorf("pulls %q", f.gh.pulls)
	}
	if _, err := os.Stat(f.cfg.HoldDir); !os.IsNotExist(err) {
		t.Error("a hold directory was made for a clean build")
	}
}

func TestTheModelsGitConfigurationAndHooksAreNeverRun(t *testing.T) {
	f := newFixture(t, publish.ModeBuild)
	f.commit(t, builder, "feat(race): fuel", map[string]string{"race/fuel.go": "package race\n"})
	write(t, f.work, "out/pr-title.txt", "feat(race): fuel")
	write(t, f.work, "out/pr-body.md", body)
	// A hook that would run in the step holding the token, and a configuration
	// that would send the push elsewhere.
	marker := filepath.Join(f.work, "hook-ran")
	write(t, f.repo, ".git/hooks/pre-push", "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(filepath.Join(f.repo, ".git", "hooks", "pre-push"), 0o700); err != nil { //nolint:gosec // an executable hook, on purpose
		t.Fatal(err)
	}
	elsewhere := filepath.Join(f.work, "elsewhere.git")
	git(t, f.work, builder, "init", "-q", "--bare", elsewhere)
	git(t, f.repo, builder, "config", "core.hooksPath", filepath.Join(f.repo, ".git", "hooks"))
	git(t, f.repo, builder, "config", "url."+elsewhere+".insteadOf", f.origin+"those-who-run/ecu.git")
	git(t, f.repo, builder, "config", "alias.push", "!touch "+marker)
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the model's hook or alias ran")
	}
	if got := f.originRef(t, "refs/heads/agent/232-fuel"); got == "" {
		t.Error("the push did not reach the origin")
	}
	cmd := exec.Command("git", "-C", elsewhere, "rev-parse", "--verify", "-q", "refs/heads/agent/232-fuel") //nolint:gosec // the fixture
	if out, _ := cmd.Output(); len(out) > 0 {
		t.Error("the push went where the model's configuration pointed")
	}
}

func TestBuildIsHeld(t *testing.T) {
	other := publish.Identity{Name: "Octo Maintainer", Email: "1+octo-maintainer@users.noreply.github.com"}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
		code  string
	}{
		{"a commit by someone else", func(t *testing.T, f *fixture) {
			f.commit(t, other, "feat: x", map[string]string{"race/fuel.go": "package race\n"})
		}, "commit-author"},
		{"the guard touched", func(t *testing.T, f *fixture) {
			f.commit(t, builder, "feat: x", map[string]string{".claude/settings.json": "{\"steer\": true}\n"})
		}, "protected-path"},
		{"no commit", func(_ *testing.T, _ *fixture) {}, "no-commits"},
		{"work on another branch", func(t *testing.T, f *fixture) {
			git(t, f.repo, builder, "checkout", "-q", "-b", "agent/232-other")
			f.commit(t, builder, "feat: x", map[string]string{"race/fuel.go": "package race\n"})
		}, "branch"},
		{"a detached head", func(t *testing.T, f *fixture) {
			sha := f.commit(t, builder, "feat: x", map[string]string{"race/fuel.go": "package race\n"})
			git(t, f.repo, builder, "checkout", "-q", "--detach", sha)
		}, "branch"},
		{"a body that is not the template's", func(t *testing.T, f *fixture) {
			f.commit(t, builder, "feat: x", map[string]string{"race/fuel.go": "package race\n"})
			write(t, f.work, "out/pr-body.md", "Done.\n")
		}, "body"},
		{"a private value in the diff", func(t *testing.T, f *fixture) {
			f.commit(t, builder, "feat: x", map[string]string{"race/fuel.go": "package race\n\nconst url = \"http://" + strings.Join([]string{"nuc", "home", "arpa"}, ".") + "\"\n"})
		}, "private-scan"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, publish.ModeBuild)
			write(t, f.work, "out/pr-title.txt", "feat(race): fuel")
			write(t, f.work, "out/pr-body.md", body)
			c.setup(t, f)
			if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 10 {
				t.Fatalf("exit %d", code)
			}
			if len(f.gh.pulls) != 0 || f.originRef(t, "refs/heads/agent/232-fuel") != "" || f.originRef(t, "refs/heads/agent/232-other") != "" {
				t.Error("something was published")
			}
			entries, err := os.ReadDir(f.cfg.HoldDir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("hold dir %v %v", entries, err)
			}
			reasons := read(t, filepath.Join(f.cfg.HoldDir, entries[0].Name(), "reasons.md"))
			if !strings.Contains(reasons, "- "+c.code+":") {
				t.Errorf("reasons %q lack %s", reasons, c.code)
			}
		})
	}
}

func TestFixPushesAndAnswers(t *testing.T) {
	f := newFixture(t, publish.ModeFix)
	head := f.commit(t, builder, "fix(race): compare in laps", map[string]string{"race/fuel.go": "package race\n\nfunc Fuel() bool { return true }\n"})
	write(t, f.work, "out/answer.md", "Answer to the review at `"+f.cfg.HeadSHA[:8]+"`:\n\n1. Fixed in `"+head[:8]+"`: laps.\n")
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := f.originRef(t, "refs/heads/agent/232-fuel"); got != head {
		t.Errorf("origin's branch is at %q, want %s", got, head)
	}
	if len(f.gh.answers) != 1 || !strings.HasSuffix(f.gh.answers[0], "\n\n<!-- twr-fix round=1 -->\n") || len(f.gh.pulls) != 0 {
		t.Errorf("answers %q, pulls %q", f.gh.answers, f.gh.pulls)
	}
}

// moveHead makes the pull request's head, as the fix started from it, newHead.
func (f *fixture) moveHead(t *testing.T, newHead string) {
	t.Helper()
	git(t, f.repo, builder, "push", "-q", "--force", "origin", "agent/232-fuel")
	f.cfg.HeadSHA = newHead
	f.gh.pull.HeadSHA = newHead
	var task fetch.Task
	path := filepath.Join(f.work, "data", "task.json")
	if err := json.Unmarshal([]byte(read(t, path)), &task); err != nil {
		t.Fatal(err)
	}
	task.HeadSHA = newHead
	data, _ := json.Marshal(task)
	write(t, f.work, "data/task.json", string(data))
}

func TestFixIgnoresCommitsBeforeTheReviewedHead(t *testing.T) {
	other := publish.Identity{Name: "Octo Maintainer", Email: "1+octo-maintainer@users.noreply.github.com"}
	f := newFixture(t, publish.ModeFix)
	// The maintainer rebased: an earlier commit is theirs, and the review is of the head after it.
	git(t, f.repo, builder, "reset", "-q", "--hard", f.mainSHA)
	f.commit(t, other, "chore: rebase", map[string]string{"race/other.go": "package race\n"})
	reviewed := f.commit(t, builder, "feat(race): fuel", map[string]string{"race/fuel.go": "package race\n\nfunc Fuel() {}\n"})
	f.moveHead(t, reviewed)
	head := f.commit(t, builder, "fix(race): compare in laps", map[string]string{"race/fuel.go": "package race\n\nfunc Fuel() bool { return true }\n"})
	write(t, f.work, "out/answer.md", "Answer to the review at `"+reviewed[:8]+"`:\n\n1. Fixed in `"+head[:8]+"`: laps.\n")
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := f.originRef(t, "refs/heads/agent/232-fuel"); got != head {
		t.Errorf("origin's branch is at %q, want %s", got, head)
	}
}

func TestFixThatMergesAMainWithOthersCommits(t *testing.T) {
	other := publish.Identity{Name: "Octo Maintainer", Email: "1+octo-maintainer@users.noreply.github.com"}
	f := newFixture(t, publish.ModeFix)
	// main advances with a commit that is not the builder's, after the reviewed head.
	git(t, f.repo, builder, "checkout", "-q", "main")
	f.commit(t, other, "chore: main moves", map[string]string{"docs.md": "# docs\n"})
	git(t, f.repo, builder, "push", "-q", "origin", "main")
	git(t, f.repo, builder, "checkout", "-q", "agent/232-fuel")
	git(t, f.repo, builder, "merge", "-q", "--no-ff", "-m", "merge main", "main")
	head := f.commit(t, builder, "fix(race): compare in laps", map[string]string{"race/fuel.go": "package race\n\nfunc Fuel() bool { return true }\n"})
	write(t, f.work, "out/answer.md", "Answer to the review at `"+f.cfg.HeadSHA[:8]+"`:\n\n1. Fixed in `"+head[:8]+"`: laps.\n")
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := f.originRef(t, "refs/heads/agent/232-fuel"); got != head {
		t.Errorf("origin's branch is at %q, want %s", got, head)
	}
}

func TestFixHeldForANewCommitThatIsNotTheBuilders(t *testing.T) {
	other := publish.Identity{Name: "Octo Maintainer", Email: "1+octo-maintainer@users.noreply.github.com"}
	f := newFixture(t, publish.ModeFix)
	head := f.commit(t, other, "fix(race): laps", map[string]string{"race/fuel.go": "package race\n\nfunc Fuel() bool { return true }\n"})
	write(t, f.work, "out/answer.md", "Answer to the review at `"+f.cfg.HeadSHA[:8]+"`:\n\n1. Fixed in `"+head[:8]+"`.\n")
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 10 {
		t.Fatalf("exit %d", code)
	}
	if got := f.originRef(t, "refs/heads/agent/232-fuel"); got != f.cfg.HeadSHA {
		t.Errorf("origin's branch moved to %q", got)
	}
}

func TestFixWithARewrittenBody(t *testing.T) {
	f := newFixture(t, publish.ModeFix)
	head := f.commit(t, builder, "fix(race): laps", map[string]string{"race/fuel.go": "package race\n\nfunc Fuel() bool { return true }\n"})
	write(t, f.work, "out/answer.md", "Answer to the review at `"+f.cfg.HeadSHA[:8]+"`:\n\n1. Fixed in `"+head[:8]+"`.\n")
	write(t, f.work, "out/pr-body.md", strings.Replace(body, "| AC1 | unit |", "| AC1 | recording |", 1))
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(f.gh.bodies) != 1 || !strings.Contains(f.gh.bodies[0], "| AC1 | recording |") || len(f.gh.answers) != 1 {
		t.Errorf("bodies %q, answers %d", f.gh.bodies, len(f.gh.answers))
	}
	// A rewritten body off the template is held like a new one.
	write(t, f.work, "out/pr-body.md", "Done.\n")
	f.gh.bodies = nil
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 10 || len(f.gh.bodies) != 0 {
		t.Errorf("exit %d, bodies %q", code, f.gh.bodies)
	}
}

func TestFixThatRewroteTheHeadIsHeld(t *testing.T) {
	f := newFixture(t, publish.ModeFix)
	git(t, f.repo, builder, "reset", "-q", "--hard", f.mainSHA)
	f.commit(t, builder, "fix(race): rewritten", map[string]string{"race/fuel.go": "package race\n"})
	write(t, f.work, "out/answer.md", "Answer to the review at `"+f.cfg.HeadSHA[:8]+"`:\n\n1. Fixed.\n")
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 10 {
		t.Fatalf("exit %d", code)
	}
	if got := f.originRef(t, "refs/heads/agent/232-fuel"); got != f.cfg.HeadSHA {
		t.Errorf("origin's branch moved to %q", got)
	}
}

func TestFixWhoseHeadMoved(t *testing.T) {
	f := newFixture(t, publish.ModeFix)
	f.gh.pull.HeadSHA = f.mainSHA
	write(t, f.work, "out/answer.md", "Answer to the review at `x`:\n")
	if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 11 {
		t.Errorf("exit %d", code)
	}
}

func TestConfigurationErrors(t *testing.T) {
	t.Run("task.json is for another mode", func(t *testing.T) {
		f := newFixture(t, publish.ModeBuild)
		f.cfg.Mode = publish.ModeFix
		f.cfg.Number, f.cfg.HeadSHA, f.cfg.Round = 1, f.mainSHA, 1
		if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 2 {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("no checkout", func(t *testing.T) {
		f := newFixture(t, publish.ModeBuild)
		_ = os.RemoveAll(f.repo)
		if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 1 {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("a .git that is a symbolic link", func(t *testing.T) {
		f := newFixture(t, publish.ModeBuild)
		real := filepath.Join(f.work, "real-git")
		if err := os.Rename(filepath.Join(f.repo, ".git"), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(f.repo, ".git")); err != nil {
			t.Fatal(err)
		}
		if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 1 {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("a ref that is a symbolic link", func(t *testing.T) {
		f := newFixture(t, publish.ModeBuild)
		f.commit(t, builder, "feat: x", map[string]string{"race/fuel.go": "package race\n"})
		ref := filepath.Join(f.repo, ".git", "refs", "heads", "agent", "232-fuel")
		if err := os.Remove(ref); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.repo, "README.md"), ref); err != nil {
			t.Fatal(err)
		}
		if code := Run(t.Context(), f.cfg, f.gh, logger()); code != 1 {
			t.Errorf("exit %d", code)
		}
	})
}

func TestHeadOf(t *testing.T) {
	dir := t.TempDir()
	sha := strings.Repeat("a", 40)
	write(t, dir, "HEAD", "ref: refs/heads/agent/1-x\n")
	if _, _, err := HeadOf(dir); err == nil {
		t.Error("no error with no ref")
	}
	write(t, dir, "packed-refs", "# pack-refs with: peeled\n"+sha+" refs/heads/agent/1-x\n")
	if ref, got, err := HeadOf(dir); err != nil || ref != "refs/heads/agent/1-x" || got != sha {
		t.Errorf("%q %q %v", ref, got, err)
	}
	write(t, dir, "refs/heads/agent/1-x", strings.Repeat("b", 40)+"\n")
	if _, got, _ := HeadOf(dir); got != strings.Repeat("b", 40) {
		t.Errorf("the loose ref did not win: %q", got)
	}
	write(t, dir, "HEAD", sha+"\n")
	if ref, got, err := HeadOf(dir); err != nil || ref != sha || got != sha {
		t.Errorf("detached: %q %q %v", ref, got, err)
	}
	for _, bad := range []string{"nonsense\n", "ref: ../../etc/passwd\n", "ref: heads/x\n"} {
		write(t, dir, "HEAD", bad)
		if _, _, err := HeadOf(dir); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	good := map[string]string{"PUBLISH_MODE": "build", "PUBLISH_REPO": "those-who-run/ecu", "PUBLISH_ISSUE_REPO": "those-who-run/paddock", "PUBLISH_ISSUE": "232",
		"PUBLISH_BUILDER_NAME": "b", "PUBLISH_BUILDER_EMAIL": "b@example.com", "PUBLISH_HOLD_DIR": "/hold", "PUBLISH_PROTECTED": ".claude/**, CODEOWNERS"}
	cfg, err := ConfigFromEnv(env(good))
	if err != nil || cfg.Dir != "/work" || cfg.BaseRef != "main" || len(cfg.Protected) != 2 || cfg.GitBase != "https://github.com/" {
		t.Errorf("%+v %v", cfg, err)
	}
	fix := map[string]string{}
	for k, v := range good {
		fix[k] = v
	}
	fix["PUBLISH_MODE"], fix["PUBLISH_NUMBER"], fix["PUBLISH_HEAD_SHA"], fix["PUBLISH_ROUND"] = "fix", "31", strings.Repeat("a", 40), "2"
	delete(fix, "PUBLISH_PROTECTED")
	if cfg, err := ConfigFromEnv(env(fix)); err != nil || cfg.Round != 2 || len(cfg.Protected) != 2 {
		t.Errorf("%+v %v", cfg, err)
	}
	delete(fix, "PUBLISH_ROUND")
	if _, err := ConfigFromEnv(env(fix)); err == nil {
		t.Error("a fix without a round")
	}
	for _, drop := range []string{"PUBLISH_MODE", "PUBLISH_REPO", "PUBLISH_ISSUE", "PUBLISH_BUILDER_EMAIL", "PUBLISH_HOLD_DIR"} {
		m := map[string]string{}
		for k, v := range good {
			if k != drop {
				m[k] = v
			}
		}
		if _, err := ConfigFromEnv(env(m)); err == nil {
			t.Errorf("without %s: no error", drop)
		}
	}
	good["PUBLISH_ISSUE"] = "x"
	if _, err := ConfigFromEnv(env(good)); err == nil {
		t.Error("a bad number")
	}
}
