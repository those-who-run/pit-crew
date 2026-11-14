package publish

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/those-who-run/pit-crew/internal/lanes"
)

var builder = Identity{Name: "twr-builder[bot]", Email: "123456+twr-builder[bot]@users.noreply.github.com"}

var maintainer = Identity{Name: "Octo Maintainer", Email: "1+octo-maintainer@users.noreply.github.com"}

func rules(t *testing.T) BuildRules {
	t.Helper()
	cfg, err := lanes.Load(filepath.Join("..", "lanes", "testdata", "lanes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return BuildRules{Builder: builder, Protected: DefaultProtected, Lanes: cfg, Scanner: scanner()}
}

const (
	baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// body is a pull request body as twr-pr-ready writes it.
const body = `## What changed

The fuel callout fires when the laps left reach the margin.

AI assistance: Claude Code wrote the change and its test.

New direct dependencies: none

## How it was checked

| Criterion | Proof | Where |
|---|---|---|
| AC1 box this lap | recording | ` + "`race/fuel_test.go`, `TestBoxThisLap`" + ` |

- [x] The command that matches CI (` + "`make check`" + `) is green
- [x] Docs updated, or none needed because: the README already describes the callout

Closes those-who-run/paddock#232.
🤖 Generated with [Claude Code](https://claude.com/claude-code)
`

const patch = `diff --git a/race/fuel.go b/race/fuel.go
index 1111111..2222222 100644
--- a/race/fuel.go
+++ b/race/fuel.go
@@ -88,0 +89,2 @@ func (f *Fuel) Check(lapsLeft float64) bool {
+	// The margin is in laps; the comparison was in litres.
+	return lapsLeft <= f.marginLaps
diff --git a/race/fuel_test.go b/race/fuel_test.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/race/fuel_test.go
@@ -0,0 +1,3 @@
+package race
+
+func TestBoxThisLap(t *testing.T) {}
`

func buildTask() BuildTask {
	return BuildTask{Mode: ModeBuild, Repo: "those-who-run/ecu", IssueRepo: "those-who-run/paddock", Issue: 232,
		Branch: "agent/232-fuel-callouts", BaseSHA: baseSHA}
}

func buildFacts(change func(*BuildFacts)) BuildFacts {
	facts := BuildFacts{
		HeadRef:        "refs/heads/agent/232-fuel-callouts",
		HeadSHA:        headSHA,
		BaseIsAncestor: true,
		Commits: []GitCommit{{SHA: headSHA, Author: builder, Committer: builder,
			Message: "feat(race): box-this-lap callout\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n"}},
		Changes: []lanes.Change{{Status: "M", Path: "race/fuel.go"}, {Status: "A", Path: "race/fuel_test.go"}},
		Patch:   patch,
		Title:   "feat(race): box-this-lap callout",
		Body:    body,
	}
	if change != nil {
		change(&facts)
	}
	return facts
}

func fixTask() BuildTask {
	task := buildTask()
	task.Mode, task.PrevHead, task.Round, task.RoundsUsed = ModeFix, baseSHA, 1, 0
	return task
}

const answer = "Answer to the review at `aaaaaaaa`:\n\n1. Fixed in `bbbbbbbb`: the margin is now compared in laps; `TestBoxThisLap` covers it.\n2. Contested: the loop is bounded by the grid size, see `race/grid.go`.\n\nNot blocking: fixed 1.\n"

func fixFacts(change func(*BuildFacts)) BuildFacts {
	return buildFacts(func(f *BuildFacts) {
		f.PrevIsAncestor = true
		f.NewCommits = f.Commits
		f.Answer = answer
		if change != nil {
			change(f)
		}
	})
}

func TestCheckBuildPasses(t *testing.T) {
	cases := []struct {
		name  string
		task  BuildTask
		facts BuildFacts
	}{
		{"a build", buildTask(), buildFacts(nil)},
		{"a build whose boxes are not ticked", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.ReplaceAll(body, "- [x]", "- [ ]")
		})},
		{"a build with no tool trailer", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, ToolTrailer+"\n", "", 1)
		})},
		{"part of, with what is left", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "Closes those-who-run/paddock#232.", "Part of those-who-run/paddock#232 (AC2, checked on the car, stays open).", 1)
		})},
		{"a deleted test, said so", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "D", Path: "race/old_test.go"})
			f.Body = strings.Replace(body, "\nCloses", "\nTests weakened: race/old_test.go covered the litres path, which is gone.\n\nCloses", 1)
		})},
		{"a skip that only moved", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Patch = patch + "diff --git a/race/grid_test.go b/race/grid_test.go\n--- a/race/grid_test.go\n+++ b/race/grid_test.go\n" +
				"@@ -5 +5,0 @@\n-\tt.Skip(\"needs a server\")\n@@ -9,0 +9 @@\n+\tt.Skip(\"needs a server\")\n"
		})},
		{"a skip outside a test file", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Patch = patch + "diff --git a/race/x.go b/race/x.go\n--- a/race/x.go\n+++ b/race/x.go\n@@ -1,0 +2 @@\n+\tt.Skip()\n"
		})},
		{"a fix with new commits", fixTask(), fixFacts(nil)},
		{"a fix that contests everything adds no commit", fixTask(), fixFacts(func(f *BuildFacts) {
			f.Commits, f.NewCommits = nil, nil
			f.Changes = nil
			f.Patch = ""
		})},
		{"a fix's second round", func() BuildTask { t := fixTask(); t.Round, t.RoundsUsed = 2, 1; return t }(), fixFacts(nil)},
		{"a fix whose branch merged main in", fixTask(), fixFacts(func(f *BuildFacts) {
			f.NewCommits = append(f.NewCommits, GitCommit{SHA: "c", Author: builder, Committer: builder, Message: "Merge branch 'main' into agent/232-fuel-callouts"})
		})},
		{"a fix after someone else's earlier commit", fixTask(), fixFacts(func(f *BuildFacts) {
			earlier := GitCommit{SHA: "dddddddddddddddddddddddddddddddddddddddd", Author: maintainer, Committer: maintainer, Message: "Rebase onto main"}
			f.Commits = append([]GitCommit{earlier}, f.Commits...)
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CheckBuild(c.task, c.facts, rules(t)); len(got) != 0 {
				t.Errorf("refused: %+v", got)
			}
		})
	}
}

func TestCheckBuildRefuses(t *testing.T) {
	cases := []struct {
		name   string
		task   BuildTask
		facts  BuildFacts
		codes  []string
		detail string
	}{
		{"work on another branch", buildTask(), buildFacts(func(f *BuildFacts) { f.HeadRef = "refs/heads/agent/232-other" }),
			[]string{"branch"}, "HEAD is"},
		{"a detached HEAD", buildTask(), buildFacts(func(f *BuildFacts) { f.HeadRef = headSHA }), []string{"branch"}, "HEAD is"},
		{"a task branch for another issue", func() BuildTask { t := buildTask(); t.Branch = "agent/231-fuel-callouts"; return t }(),
			buildFacts(func(f *BuildFacts) { f.HeadRef = "refs/heads/agent/231-fuel-callouts" }), []string{"branch"}, "agent/232-"},
		{"a task branch that is not agent/", func() BuildTask { t := buildTask(); t.Branch = "feat/232"; return t }(),
			buildFacts(func(f *BuildFacts) { f.HeadRef = "refs/heads/feat/232" }), []string{"branch"}, ""},
		{"no commit at all", buildTask(), buildFacts(func(f *BuildFacts) { f.HeadSHA = ""; f.Commits = nil }), []string{"branch"}, "no commit"},
		{"not cut from main", buildTask(), buildFacts(func(f *BuildFacts) { f.BaseIsAncestor = false }), []string{"not-cut-from-main"}, baseSHA},
		{"a build with nothing on the branch", buildTask(), buildFacts(func(f *BuildFacts) { f.Commits = nil }), []string{"no-commits"}, ""},
		{"a fix that rewrote the head", fixTask(), fixFacts(func(f *BuildFacts) { f.PrevIsAncestor = false }), []string{"history-rewritten"}, baseSHA},
		{"an unknown mode", func() BuildTask { t := buildTask(); t.Mode = "review"; return t }(), buildFacts(nil), []string{"mode"}, ""},
		{"a commit authored as the maintainer", buildTask(), buildFacts(func(f *BuildFacts) { f.Commits[0].Author = maintainer }),
			[]string{"commit-author"}, "Octo Maintainer"},
		{"a fix commit after the reviewed head that is not the builder's", fixTask(), fixFacts(func(f *BuildFacts) {
			f.NewCommits = append(f.NewCommits, GitCommit{SHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Author: maintainer, Committer: builder, Message: "tweak"})
		}), []string{"commit-author"}, "eeeeeeee"},
		{"a build refuses a commit that is not the builder's, even one a fix would skip", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Commits = append(f.Commits, GitCommit{SHA: "dddddddddddddddddddddddddddddddddddddddd", Author: maintainer, Committer: maintainer, Message: "Rebase"})
		}), []string{"commit-author"}, "dddddddd"},
		{"a commit committed as someone else", buildTask(), buildFacts(func(f *BuildFacts) { f.Commits[0].Committer = maintainer }),
			[]string{"commit-author"}, ""},
		{"a commit with the builder's name and another address", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Commits[0].Author.Email = "builder@" + "example.net"
		}), []string{"commit-author"}, ""},
		{"a commit message with a private value", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Commits[0].Message += "\nTested against " + privateHost + "\n"
		}), []string{"private-scan"}, privateHost},
		{"a commit message with a marker", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Commits[0].Message += "\n<!-- twr-fix round=1 -->\n"
		}), []string{"marker"}, "commit"},
		{"the guard touched", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "M", Path: ".claude/hooks/guard.py"})
		}), []string{"protected-path"}, ".claude/hooks/guard.py"},
		{"the settings touched", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "A", Path: ".claude/settings.json"})
		}), []string{"protected-path"}, ""},
		{"CODEOWNERS touched", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "M", Path: ".github/CODEOWNERS"})
		}), []string{"protected-path"}, "CODEOWNERS"},
		{"a test deleted without a word", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "D", Path: "race/old_test.go"})
		}), []string{"tests-weakened"}, "deleted race/old_test.go"},
		{"a test renamed away", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "R", Path: "race/old_test.go"}, lanes.Change{Status: "R", Path: "race/old.go"})
		}), []string{"tests-weakened"}, "renamed race/old_test.go"},
		{"a fixture deleted", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "D", Path: "race/testdata/grid.jsonl"})
		}), []string{"tests-weakened"}, "testdata"},
		{"a skip added", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Patch = strings.Replace(patch, "+func TestBoxThisLap(t *testing.T) {}", "+func TestBoxThisLap(t *testing.T) { t.Skip(\"later\") }", 1)
		}), []string{"tests-weakened"}, "skip added in race/fuel_test.go"},
		{"a Python skip added", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Patch = patch + "diff --git a/tests/test_x.py b/tests/test_x.py\n--- a/tests/test_x.py\n+++ b/tests/test_x.py\n@@ -1,0 +2 @@\n+@unittest.skip(\"later\")\n"
		}), []string{"tests-weakened"}, "tests/test_x.py"},
		{"a reason too short to count", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Changes = append(f.Changes, lanes.Change{Status: "D", Path: "race/old_test.go"})
			f.Body = strings.Replace(body, "\nCloses", "\nTests weakened: gone\n\nCloses", 1)
		}), []string{"tests-weakened"}, ""},
		{"a title that is not a Conventional Commit", buildTask(), buildFacts(func(f *BuildFacts) { f.Title = "Add fuel callout" }),
			[]string{"title"}, ""},
		{"a title with a type the hooks refuse", buildTask(), buildFacts(func(f *BuildFacts) { f.Title = "feature: fuel callout" }),
			[]string{"title"}, ""},
		{"a title over two lines", buildTask(), buildFacts(func(f *BuildFacts) { f.Title = "feat: fuel\ncallout" }), []string{"title"}, ""},
		{"a title with a private value", buildTask(), buildFacts(func(f *BuildFacts) { f.Title = "feat: talk to " + privateHost }),
			[]string{"private-scan"}, privateHost},
		{"no What changed", buildTask(), buildFacts(func(f *BuildFacts) { f.Body = strings.Replace(body, "## What changed", "## Changes", 1) }),
			[]string{"body"}, "What changed"},
		{"the sections the other way round", buildTask(), buildFacts(func(f *BuildFacts) {
			i := strings.Index(body, "## How it was checked")
			f.Body = body[i:] + body[:i]
		}), []string{"body"}, ""},
		{"no AI assistance line", buildTask(), buildFacts(func(f *BuildFacts) { f.Body = strings.Replace(body, "AI assistance: Claude", "Assistance: Claude", 1) }),
			[]string{"body"}, "AI assistance"},
		{"an AI assistance line left as the template", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "AI assistance: Claude Code wrote the change and its test.", "AI assistance: <!-- the tool, and what it did -->", 1)
		}), []string{"body"}, "AI assistance"},
		{"no dependencies line", buildTask(), buildFacts(func(f *BuildFacts) { f.Body = strings.Replace(body, "New direct dependencies: none\n", "", 1) }),
			[]string{"body"}, "New direct dependencies"},
		{"no evidence table", buildTask(), buildFacts(func(f *BuildFacts) { f.Body = strings.Replace(body, "| Criterion | Proof | Where |", "", 1) }),
			[]string{"body"}, "evidence table"},
		{"an evidence table with no row", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "| AC1 box this lap | recording | `race/fuel_test.go`, `TestBoxThisLap` |\n", "", 1)
		}), []string{"body"}, "evidence table"},
		{"no CI checkbox", buildTask(), buildFacts(func(f *BuildFacts) { f.Body = strings.Replace(body, "- [x] The command", "The command", 1) }),
			[]string{"body"}, "command that matches CI"},
		{"no docs checkbox", buildTask(), buildFacts(func(f *BuildFacts) { f.Body = strings.Replace(body, "- [x] Docs updated", "- Docs updated", 1) }),
			[]string{"body"}, "docs"},
		{"the issue line is not last", buildTask(), buildFacts(func(f *BuildFacts) { f.Body = body + "\nOne more thing.\n" }),
			[]string{"body"}, "last line"},
		{"the issue line names another issue", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "paddock#232", "paddock#233", 1)
		}), []string{"body"}, "last line"},
		{"the issue line names no repository", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "Closes those-who-run/paddock#232.", "Closes #232.", 1)
		}), []string{"body"}, "last line"},
		{"no issue, which a build always has", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "Closes those-who-run/paddock#232.", "No issue: planned in the maintainer's notes.", 1)
		}), []string{"body"}, "last line"},
		{"a body with a marker", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "\nCloses", "\n<!-- twr-review sha="+headSHA+" -->\nCloses", 1)
		}), []string{"marker"}, "body"},
		{"a body that names someone", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "fires when", "fires (asked by @octo-maintainer) when", 1)
		}), []string{"mention"}, "@octo-maintainer"},
		{"a body with a private value", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Body = strings.Replace(body, "fires when", "fires on "+privateHost+" when", 1)
		}), []string{"private-scan"}, privateHost},
		{"a diff that adds a private address", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Patch = strings.Replace(patch, "+\treturn lapsLeft <= f.marginLaps", "+\tconst nats = \"nats://10.20.30."+"40:4222\"", 1)
		}), []string{"private-scan"}, "diff of race/fuel.go"},
		{"a diff that adds a credential", buildTask(), buildFacts(func(f *BuildFacts) {
			f.Patch = strings.Replace(patch, "+\treturn lapsLeft <= f.marginLaps", "+\ttoken := \"ghp_"+strings.Repeat("a1B2", 9)+"\"", 1)
		}), []string{"private-scan"}, "secret"},
		{"a fix with no answer", fixTask(), fixFacts(func(f *BuildFacts) { f.Answer = "" }), []string{"answer-missing"}, ""},
		{"a fix whose answer starts otherwise", fixTask(), fixFacts(func(f *BuildFacts) { f.Answer = "Fixed everything.\n" }),
			[]string{"answer-invalid"}, "aaaaaaaa"},
		{"a fix whose answer names another head", fixTask(), fixFacts(func(f *BuildFacts) {
			f.Answer = strings.Replace(answer, "aaaaaaaa", "cccccccc", 1)
		}), []string{"answer-invalid"}, ""},
		{"a fix whose answer carries its own marker", fixTask(), fixFacts(func(f *BuildFacts) { f.Answer = answer + "<!-- twr-fix round=1 -->\n" }),
			[]string{"marker"}, "answer"},
		{"a fix whose answer names someone", fixTask(), fixFacts(func(f *BuildFacts) { f.Answer = answer + "\n@octo-maintainer please decide.\n" }),
			[]string{"mention"}, ""},
		{"a third round", func() BuildTask { t := fixTask(); t.Round, t.RoundsUsed = 3, 2; return t }(), fixFacts(nil), []string{"round"}, "at most 2"},
		{"a round that skips one", func() BuildTask { t := fixTask(); t.Round, t.RoundsUsed = 2, 0; return t }(), fixFacts(nil), []string{"round"}, ""},
		{"a round already answered", func() BuildTask { t := fixTask(); t.Round, t.RoundsUsed = 1, 1; return t }(), fixFacts(nil), []string{"round"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CheckBuild(c.task, c.facts, rules(t))
			if codes := Codes(got); !reflect.DeepEqual(codes, c.codes) {
				t.Fatalf("codes %v, want %v: %+v", codes, c.codes, got)
			}
			var details []string
			for _, r := range got {
				details = append(details, r.Detail)
			}
			if c.detail != "" && !strings.Contains(strings.Join(details, "\n"), c.detail) {
				t.Errorf("details %q do not say %q", details, c.detail)
			}
		})
	}
}

func TestCheckBuildReportsEverything(t *testing.T) {
	facts := buildFacts(func(f *BuildFacts) {
		f.BaseIsAncestor = false
		f.Title = "fuel"
		f.Body = "nothing"
		f.Changes = append(f.Changes, lanes.Change{Status: "M", Path: "CODEOWNERS"})
	})
	got := Codes(CheckBuild(buildTask(), facts, rules(t)))
	want := []string{"not-cut-from-main", "protected-path", "title", "body"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codes %v, want %v", got, want)
	}
}

func TestCheckBuildNeedsItsConfiguration(t *testing.T) {
	r := rules(t)
	r.Builder = Identity{}
	r.Lanes = nil
	r.Protected = []string{"/abs"}
	got := Codes(CheckBuild(buildTask(), buildFacts(nil), r))
	want := []string{"commit-author", "configuration"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codes %v, want %v", got, want)
	}
}

func TestAnswerBody(t *testing.T) {
	got := AnswerBody("Answer to the review at `aaaaaaaa`:\n\n1. Fixed.\n\n\n", 2)
	want := "Answer to the review at `aaaaaaaa`:\n\n1. Fixed.\n\n<!-- twr-fix round=2 -->\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParsePatch(t *testing.T) {
	added, removed := ParsePatch(patch + "diff --git a/old.go b/old.go\ndeleted file mode 100644\n--- a/old.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-package old\n-// gone\n" +
		"diff --git a/x.md b/x.md\n--- a/x.md\n+++ b/x.md\n@@ -1 +1 @@\n--- a heading that starts with dashes\n+++ one that starts with pluses\n")
	wantAdded := map[string][]string{
		"race/fuel.go":      {"\t// The margin is in laps; the comparison was in litres.", "\treturn lapsLeft <= f.marginLaps"},
		"race/fuel_test.go": {"package race", "", "func TestBoxThisLap(t *testing.T) {}"},
		"x.md":              {"++ one that starts with pluses"},
	}
	wantRemoved := map[string][]string{"old.go": {"package old", "// gone"}, "x.md": {"-- a heading that starts with dashes"}}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Errorf("added %q, want %q", added, wantAdded)
	}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Errorf("removed %q, want %q", removed, wantRemoved)
	}
}
