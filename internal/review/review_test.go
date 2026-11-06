package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/those-who-run/pit-crew/internal/fetch"
	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/ladder"
)

const (
	head  = "1f0c5d2e9a7b4c3d8e6f1a2b3c4d5e6f7a8b9c0d"
	other = "0000000000000000000000000000000000000001"
)

// gh is GitHub as the step sees it, recording what it writes.
type gh struct {
	pull     github.PullRequest
	comments []github.Comment
	created  []string
	updated  map[int64]string
	statuses []string
}

func (g *gh) GetPull(context.Context, string, int) (github.PullRequest, error) { return g.pull, nil }
func (g *gh) ListComments(context.Context, string, int) ([]github.Comment, error) {
	return g.comments, nil
}
func (g *gh) CreateComment(_ context.Context, _ string, _ int, body string) (int64, error) {
	g.created = append(g.created, body)
	return 42, nil
}
func (g *gh) UpdateComment(_ context.Context, _ string, id int64, body string) error {
	if g.updated == nil {
		g.updated = map[int64]string{}
	}
	g.updated[id] = body
	return nil
}
func (g *gh) SetStatus(_ context.Context, _, sha, context, state, description string) error {
	g.statuses = append(g.statuses, fmt.Sprintf("%s %s %s: %s", sha[:8], context, state, description))
	return nil
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

// volumes lays out /work as fetch left it and the agent wrote it.
func volumes(t *testing.T, verdict ladder.Verdict, comment, private string) Config {
	t.Helper()
	dir := t.TempDir()
	task, _ := json.Marshal(fetch.Task{Kind: "review", Repo: "those-who-run/ecu", Number: 31, HeadSHA: head, BaseRef: "main", BaseSHA: other, Lane: "standard"})
	write(t, dir, "data/task.json", string(task))
	v := `{"repo": "those-who-run/ecu", "pr": 31, "sha": "` + head + `", "kind": "A", "lane": "standard", "verdict": "` + string(verdict) + `", "blocking": [], "ran": [], "not_run": []}`
	if verdict == ladder.VerdictChanges {
		v = strings.Replace(v, `"blocking": []`, `"blocking": ["a"]`, 1)
	}
	write(t, dir, "out/verdict.json", v)
	if comment != "" {
		write(t, dir, "out/comment.md", comment)
	}
	if private != "" {
		write(t, dir, "out/private.md", private)
	}
	return Config{Repo: "those-who-run/ecu", Number: 31, HeadSHA: head, Reviewer: "twr-reviewer[bot]", Dir: dir, HoldDir: filepath.Join(dir, "hold")}
}

func comment(verdict ladder.Verdict, extra string) string {
	return "Review at head `1f0c5d2e`: **" + string(verdict) + "**.\n\n**Checked:** I ran `make check`.\n\n" + extra +
		"<!-- twr-review sha=" + head + " kind=A lane=standard -->\n"
}

func logger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestPostsAComment(t *testing.T) {
	cfg := volumes(t, ladder.VerdictReady, comment(ladder.VerdictReady, ""), "")
	g := &gh{pull: github.PullRequest{HeadSHA: head}}
	if code := Run(t.Context(), cfg, g, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(g.created) != 1 || !strings.HasSuffix(g.created[0], "<!-- twr-review sha="+head+" kind=A lane=standard verdict=ready -->\n") {
		t.Errorf("created %q", g.created)
	}
	if len(g.statuses) != 1 || g.statuses[0] != "1f0c5d2e review / verdict success: ready: kind A, lane standard" {
		t.Errorf("statuses %q", g.statuses)
	}
	if _, err := os.Stat(cfg.HoldDir); !os.IsNotExist(err) {
		t.Error("a hold directory was made for a clean review")
	}
}

func TestUpdatesItsOwnCommentForTheHead(t *testing.T) {
	cfg := volumes(t, ladder.VerdictNits, comment(ladder.VerdictNits, ""), "")
	g := &gh{pull: github.PullRequest{HeadSHA: head}, comments: []github.Comment{
		{ID: 7, Author: "someone-else", Body: "<!-- twr-review sha=" + head + " kind=A lane=standard -->"},
		{ID: 8, Author: "twr-reviewer[bot]", Body: "old\n<!-- twr-review sha=" + other + " kind=A lane=standard -->"},
		{ID: 9, Author: "TWR-Reviewer[bot]", Body: "old\n<!-- twr-review sha=" + head + " kind=A lane=standard verdict=changes -->"},
	}}
	if code := Run(t.Context(), cfg, g, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(g.created) != 0 || len(g.updated) != 1 || !strings.Contains(g.updated[9], "verdict=nits") {
		t.Errorf("created %q, updated %v", g.created, g.updated)
	}
}

func TestHeadMoved(t *testing.T) {
	cfg := volumes(t, ladder.VerdictReady, comment(ladder.VerdictReady, ""), "")
	g := &gh{pull: github.PullRequest{HeadSHA: other}}
	if code := Run(t.Context(), cfg, g, logger()); code != 11 {
		t.Errorf("exit %d", code)
	}
	if len(g.created) != 0 || len(g.statuses) != 0 {
		t.Errorf("posted %q %q", g.created, g.statuses)
	}
}

func TestHoldsAndSaysWhy(t *testing.T) {
	private := "The deploy doc names " + strings.Join([]string{"nuc", "home", "arpa"}, ".") + ".\n"
	cfg := volumes(t, ladder.VerdictReady, comment(ladder.VerdictReady, "cc @someone\n\n"), private)
	g := &gh{pull: github.PullRequest{HeadSHA: head}}
	if code := Run(t.Context(), cfg, g, logger()); code != 10 {
		t.Fatalf("exit %d", code)
	}
	if len(g.created) != 0 {
		t.Errorf("posted %q", g.created)
	}
	if len(g.statuses) != 1 || g.statuses[0] != "1f0c5d2e review / verdict error: held: mention" {
		t.Errorf("statuses %q", g.statuses)
	}
	hold := filepath.Join(cfg.HoldDir, "ecu-31-1f0c5d2e")
	if got := read(t, filepath.Join(hold, "reasons.md")); !strings.Contains(got, "- mention: comment names @someone") {
		t.Errorf("reasons %q", got)
	}
	if got := read(t, filepath.Join(hold, "private.md")); got != private {
		t.Errorf("private.md %q", got)
	}
	for _, name := range []string{"comment.md", "verdict.json"} {
		if _, err := os.Stat(filepath.Join(hold, name)); err != nil {
			t.Errorf("%s not held", name)
		}
	}
}

func TestPrivateBesideACleanCommentIsHeldAlone(t *testing.T) {
	cfg := volumes(t, ladder.VerdictChanges, comment(ladder.VerdictChanges, "**Asked for:**\n1. x\n\n"), "one item sent privately\n")
	g := &gh{pull: github.PullRequest{HeadSHA: head}}
	if code := Run(t.Context(), cfg, g, logger()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(g.created) != 1 || !strings.Contains(g.statuses[0], "failure: changes: 1 asked for") {
		t.Errorf("created %d, statuses %q", len(g.created), g.statuses)
	}
	hold := filepath.Join(cfg.HoldDir, "ecu-31-1f0c5d2e")
	if _, err := os.Stat(filepath.Join(hold, "private.md")); err != nil {
		t.Error("private.md not held")
	}
	if _, err := os.Stat(filepath.Join(hold, "reasons.md")); err == nil {
		t.Error("reasons written for a clean review")
	}
}

func TestStatusOnlyReview(t *testing.T) {
	cfg := volumes(t, ladder.VerdictReady, "", "")
	write(t, cfg.Dir, "out/verdict.json", `{"repo": "those-who-run/ecu", "pr": 31, "sha": "`+head+`", "kind": "D", "lane": "standard", "verdict": "ready", "blocking": [], "ran": [], "not_run": []}`)
	g := &gh{pull: github.PullRequest{HeadSHA: head}}
	if code := Run(t.Context(), cfg, g, logger()); code != 0 || len(g.created) != 0 || len(g.statuses) != 1 {
		t.Errorf("exit %d, created %q, statuses %q", code, g.created, g.statuses)
	}
}

func TestConfigurationErrors(t *testing.T) {
	t.Run("task.json is for another pull request", func(t *testing.T) {
		cfg := volumes(t, ladder.VerdictReady, comment(ladder.VerdictReady, ""), "")
		cfg.Number = 32
		if code := Run(t.Context(), cfg, &gh{pull: github.PullRequest{HeadSHA: head}}, logger()); code != 2 {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("no task.json", func(t *testing.T) {
		cfg := volumes(t, ladder.VerdictReady, comment(ladder.VerdictReady, ""), "")
		_ = os.Remove(filepath.Join(cfg.Dir, "data", "task.json"))
		if code := Run(t.Context(), cfg, &gh{}, logger()); code != 2 {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("a patterns file that does not compile", func(t *testing.T) {
		cfg := volumes(t, ladder.VerdictReady, comment(ladder.VerdictReady, ""), "")
		write(t, cfg.Dir, "patterns.txt", "lab [\n")
		cfg.PatternsFile = filepath.Join(cfg.Dir, "patterns.txt")
		if code := Run(t.Context(), cfg, &gh{}, logger()); code != 2 {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("the lab's patterns hold a comment", func(t *testing.T) {
		cfg := volumes(t, ladder.VerdictReady, comment(ladder.VerdictReady, "I checked the garage-pi too.\n\n"), "")
		write(t, cfg.Dir, "patterns.txt", "lab-host garage-[a-z]+\n")
		write(t, cfg.Dir, "extra.allow", "status.example-vendor.io A vendor.\n")
		cfg.PatternsFile, cfg.AllowFile = filepath.Join(cfg.Dir, "patterns.txt"), filepath.Join(cfg.Dir, "extra.allow")
		g := &gh{pull: github.PullRequest{HeadSHA: head}}
		if code := Run(t.Context(), cfg, g, logger()); code != 10 || !strings.Contains(g.statuses[0], "held: private-scan") {
			t.Errorf("exit %d, statuses %q", code, g.statuses)
		}
	})
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	good := map[string]string{"PUBLISH_REPO": "those-who-run/ecu", "PUBLISH_NUMBER": "31", "PUBLISH_HEAD_SHA": head, "PUBLISH_HOLD_DIR": "/hold"}
	cfg, err := ConfigFromEnv(env(good))
	if err != nil || cfg.Dir != "/work" || cfg.Reviewer != "twr-reviewer[bot]" || cfg.Number != 31 {
		t.Errorf("%+v %v", cfg, err)
	}
	for _, drop := range []string{"PUBLISH_REPO", "PUBLISH_NUMBER", "PUBLISH_HEAD_SHA", "PUBLISH_HOLD_DIR"} {
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
}
