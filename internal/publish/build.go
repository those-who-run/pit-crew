package publish

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/those-who-run/pit-crew/internal/ladder"
	"github.com/those-who-run/pit-crew/internal/lanes"
	"github.com/those-who-run/pit-crew/internal/privatescan"
)

// The two things a build publisher publishes.
const (
	// ModeBuild opens a pull request from a new branch.
	ModeBuild = "build"
	// ModeFix pushes to a builder's open pull request and posts its answer to a
	// review.
	ModeFix = "fix"
)

// Identity is a git author: the builder's, as configured.
type Identity struct {
	Name  string
	Email string
}

// GitCommit is one commit the agent made, as git recorded it.
type GitCommit struct {
	SHA       string
	Author    Identity
	Committer Identity
	// Message is the whole commit message.
	Message string
}

// BuildTask is what the fetch step recorded for a build or a fix. It comes from the
// Job's environment and the read-only data volume.
type BuildTask struct {
	Mode string
	// Repo is where the pull request lands; IssueRepo and Issue are the issue it is
	// built from.
	Repo      string
	IssueRepo string
	Issue     int
	// Branch is the branch the fetch step made, `agent/<issue>-<slug>`.
	Branch string
	// BaseSHA is the tip of the base branch when the work started.
	BaseSHA string
	// PrevHead is the pull request's head when a fix started; empty for a build.
	PrevHead string
	// Round is the fix round the poller started; RoundsUsed is how many the builder
	// has answered on the pull request so far.
	Round      int
	RoundsUsed int
}

// BuildFacts are what the publisher read from the agent's work: its commits, by
// reading the repository's objects from a repository of its own, and its output
// files.
type BuildFacts struct {
	// HeadRef is what the working repository's HEAD names, such as
	// `refs/heads/agent/232-fuel-callouts`.
	HeadRef string
	HeadSHA string
	// BaseIsAncestor says BaseSHA is an ancestor of HeadSHA; PrevIsAncestor says the
	// same of PrevHead.
	BaseIsAncestor bool
	PrevIsAncestor bool
	// Commits are the commits on the branch that the base branch does not have.
	Commits []GitCommit
	// Changes is `git diff --name-status` from the merge base with the base branch.
	Changes []lanes.Change
	// Patch is that diff with no context lines (`--unified=0`).
	Patch string
	// Title and Body are the pull request's: the agent's files for a build, and for
	// a fix the open pull request's own, or the agent's when it rewrote the body.
	Title string
	Body  string
	// Answer is the builder's answer to the review, for a fix.
	Answer string
}

// BuildRules is what a build is checked against.
type BuildRules struct {
	Builder Identity
	// Protected are path patterns, in lanes.json's syntax, that no build may touch.
	Protected []string
	Lanes     *lanes.Config
	Scanner   privatescan.Scanner
}

// DefaultProtected are the paths the plan says a build Job never touches: the
// agent's own guard, settings and skills, and who owns what.
var DefaultProtected = []string{".claude/**", "**/CODEOWNERS"}

// ToolTrailer is the one line that may follow a pull request body's last line.
const ToolTrailer = "🤖 Generated with [Claude Code](https://claude.com/claude-code)"

var (
	branchRE = regexp.MustCompile(`^agent/([1-9][0-9]*)-[a-z0-9]+(?:-[a-z0-9]+)*$`)
	titleRE  = regexp.MustCompile(`^(?:feat|fix|perf|revert|deps|docs|refactor|test|build|ci|chore|style)(?:\([a-z0-9._/-]+\))?!?: \S.*$`)
	// Skips, per language: Go's t.Skip, Python's unittest and pytest, and the
	// JavaScript runners'. The Go pattern is the one go_test_guard.py in
	// those-who-run/.github uses.
	skipRE = regexp.MustCompile(`\.Skip(?:f|Now)?\(|@(?:unittest\.)?skip|pytest\.(?:mark\.)?skip|\.skipTest\(|\bSkipTest\b|\b(?:it|test|describe)\.(?:skip|todo)\(|\bx(?:it|describe|test)\(`)
	// The reason line, as tests_weakened_reason.py in those-who-run/.github reads it.
	weakenedRE = regexp.MustCompile(`(?m)^[ \t]*Tests weakened:(.*)$`)
)

// minWeakenedReason is the shortest reason that counts, as in CI's check.
const minWeakenedReason = 10

// CheckBuild checks an agent's commits and its pull request text, and returns every
// reason not to publish them. No refusals means push and post.
//
// It refuses when the work is not on the branch the fetch step made, when the
// branch is not cut from the base branch (or, for a fix, does not continue the pull
// request's head), when a commit is not authored and committed as the builder, when
// the diff touches a protected path, when a test is deleted or skipped and the body
// has no `Tests weakened:` line, when the title is not a Conventional Commit, when
// the body lacks the template's sections or does not end with this issue's `Closes`
// or `Part of` line, when any text names a person or carries a marker, and when the
// private-value scan finds something in the text or in the lines the diff adds.
func CheckBuild(task BuildTask, facts BuildFacts, rules BuildRules) []Refusal {
	var refusals []Refusal
	refusals = append(refusals, checkBranch(task, facts)...)
	refusals = append(refusals, checkCommits(facts.Commits, rules)...)
	added, removed := ParsePatch(facts.Patch)
	refusals = append(refusals, checkPaths(facts.Changes, rules.Protected)...)
	refusals = append(refusals, checkTests(facts, added, removed, rules.Lanes)...)
	refusals = append(refusals, checkTitle(facts.Title, rules.Scanner)...)
	refusals = append(refusals, checkBody(task, facts.Body, rules.Scanner)...)
	if task.Mode == ModeFix {
		refusals = append(refusals, checkAnswer(task, facts.Answer, rules.Scanner)...)
	}
	code := rules.Scanner
	code.Code = true
	for _, path := range sortedKeys(added) {
		refusals = append(refusals, scanText(code, "diff of "+path, strings.Join(added[path], "\n"))...)
	}
	return refusals
}

func checkBranch(task BuildTask, facts BuildFacts) []Refusal {
	var refusals []Refusal
	m := branchRE.FindStringSubmatch(task.Branch)
	if m == nil || m[1] != fmt.Sprint(task.Issue) {
		refusals = append(refusals, refuse("branch", "the task's branch %q is not agent/%d-<slug>", task.Branch, task.Issue))
	}
	if facts.HeadRef != "refs/heads/"+task.Branch {
		refusals = append(refusals, refuse("branch", "HEAD is %q, and the work belongs on %s", facts.HeadRef, task.Branch))
	}
	if !ladder.IsSHA(facts.HeadSHA) {
		return append(refusals, refuse("branch", "the branch has no commit"))
	}
	switch task.Mode {
	case ModeBuild:
		if !facts.BaseIsAncestor {
			refusals = append(refusals, refuse("not-cut-from-main", "the branch does not contain %s, the base it was cut from", task.BaseSHA))
		}
		if len(facts.Commits) == 0 {
			refusals = append(refusals, refuse("no-commits", "the branch has no commit of its own"))
		}
	case ModeFix:
		// A fix adds commits to the pull request's head; it never rewrites it. An
		// answer that contests every item adds none, and that is allowed.
		if !facts.PrevIsAncestor {
			refusals = append(refusals, refuse("history-rewritten", "the branch does not contain %s, the head the review was of", task.PrevHead))
		}
	default:
		refusals = append(refusals, refuse("mode", "mode %q is neither build nor fix", task.Mode))
	}
	return refusals
}

func checkCommits(commits []GitCommit, rules BuildRules) []Refusal {
	var refusals []Refusal
	for _, commit := range commits {
		if rules.Builder.Name == "" || rules.Builder.Email == "" || commit.Author != rules.Builder || commit.Committer != rules.Builder {
			refusals = append(refusals, refuse("commit-author", "commit %s is by %s <%s>, committed by %s <%s>; every commit is the builder's",
				short(commit.SHA), commit.Author.Name, commit.Author.Email, commit.Committer.Name, commit.Committer.Email))
		}
		refusals = append(refusals, scanText(rules.Scanner, "message of commit "+short(commit.SHA), commit.Message)...)
		if n := countMarkers(commit.Message); n > 0 {
			refusals = append(refusals, refuse("marker", "the message of commit %s carries a marker", short(commit.SHA)))
		}
	}
	return refusals
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func checkPaths(changes []lanes.Change, protected []string) []Refusal {
	var refusals []Refusal
	for _, pattern := range protected {
		re, err := lanes.CompilePattern(pattern)
		if err != nil {
			refusals = append(refusals, refuse("configuration", "protected path: %v", err))
			continue
		}
		for _, change := range changes {
			if re.MatchString(change.Path) {
				refusals = append(refusals, refuse("protected-path", "the diff touches %s (%s), which matches %s", change.Path, change.Status, pattern))
			}
		}
	}
	return refusals
}

// checkTests looks for the two plain ways a change weakens its tests: a test file
// that is deleted, or renamed to something that is not a test, and a skip added to
// a test file. CI's test guard knows more ways; this is the net before the push.
func checkTests(facts BuildFacts, added, removed map[string][]string, cfg *lanes.Config) []Refusal {
	if cfg == nil {
		return []Refusal{refuse("configuration", "no lanes.json to tell a test file by")}
	}
	var weakened []string
	// A rename gives two changes with the same letter, the old path first.
	for i := 0; i < len(facts.Changes); i++ {
		change := facts.Changes[i]
		switch change.Status {
		case "D":
			if cfg.IsTest(change.Path) {
				weakened = append(weakened, "deleted "+change.Path)
			}
		case "R":
			if i+1 < len(facts.Changes) {
				to := facts.Changes[i+1].Path
				if cfg.IsTest(change.Path) && !cfg.IsTest(to) {
					weakened = append(weakened, "renamed "+change.Path+" to "+to+", which is not a test")
				}
				i++
			}
		}
	}
	// A skip that only moved (removed in one place, added in another) is not new.
	moved := map[string]int{}
	for path, lines := range removed {
		if !cfg.IsTest(path) {
			continue
		}
		for _, line := range lines {
			if skipRE.MatchString(line) {
				moved[strings.TrimSpace(line)]++
			}
		}
	}
	for _, path := range sortedKeys(added) {
		if !cfg.IsTest(path) {
			continue
		}
		for _, line := range added[path] {
			if !skipRE.MatchString(line) {
				continue
			}
			if key := strings.TrimSpace(line); moved[key] > 0 {
				moved[key]--
				continue
			}
			weakened = append(weakened, "a skip added in "+path)
		}
	}
	if len(weakened) == 0 || hasWeakenedReason(facts.Body) {
		return nil
	}
	return []Refusal{refuse("tests-weakened", "%s, and the body has no `Tests weakened:` line saying which test and why",
		strings.Join(weakened, "; "))}
}

func hasWeakenedReason(body string) bool {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	for _, match := range weakenedRE.FindAllStringSubmatch(body, -1) {
		if len(strings.TrimSpace(match[1])) >= minWeakenedReason {
			return true
		}
	}
	return false
}

func checkTitle(title string, scanner privatescan.Scanner) []Refusal {
	title = strings.TrimRight(title, "\r\n")
	var refusals []Refusal
	if strings.ContainsAny(title, "\r\n") || len(title) > 200 || !titleRE.MatchString(title) {
		refusals = append(refusals, refuse("title", "the title is not one line in Conventional Commits form, such as `feat(scope): what changed`"))
	}
	if countMarkers(title) > 0 {
		refusals = append(refusals, refuse("marker", "the title carries a marker"))
	}
	return append(refusals, checkProse(scanner, "title", title, 200)...)
}

var (
	htmlCommentRE = regexp.MustCompile(`<!--.*?-->`)
	tableHeaderRE = regexp.MustCompile(`(?m)^\|\s*Criterion\s*\|\s*Proof\s*\|\s*Where\s*\|\s*$`)
	tableRuleRE   = regexp.MustCompile(`^\|(?:\s*:?-{3,}:?\s*\|){3}\s*$`)
	tableRowRE    = regexp.MustCompile(`^\|(?:[^|\n]*\|){3}\s*$`)
)

// checkBody checks a pull request body against the organisation's template, as the
// twr-pr-ready skill writes it out: "What changed" with its two lines, "How it was
// checked" with the evidence table and the two checkboxes, and the issue line last.
func checkBody(task BuildTask, body string, scanner privatescan.Scanner) []Refusal {
	refusals := checkProse(scanner, "body", body, MaxBodyBytes)
	if countMarkers(body) > 0 {
		refusals = append(refusals, refuse("marker", "the body carries a marker"))
	}
	missing := func(what string) {
		refusals = append(refusals, refuse("body", "the body has no %s", what))
	}
	text := strings.ReplaceAll(body, "\r\n", "\n")
	changed, checked := sectionStart(text, "## What changed"), sectionStart(text, "## How it was checked")
	switch {
	case changed < 0:
		missing("`## What changed` section")
	case checked < changed:
		missing("`## How it was checked` section after `## What changed`")
	default:
		what, how := text[changed:checked], text[checked:]
		if !hasFilledLine(what, "AI assistance:") {
			missing("`AI assistance:` line under What changed")
		}
		if !hasFilledLine(what, "New direct dependencies:") {
			missing("`New direct dependencies:` line under What changed")
		}
		if !hasEvidenceTable(how) {
			missing("evidence table (Criterion | Proof | Where) with a row under How it was checked")
		}
		if !hasCheckbox(how, "The command that matches CI") {
			missing("checkbox for the command that matches CI")
		}
		if !hasCheckbox(how, "Docs updated, or none needed because:") {
			missing("checkbox for the docs")
		}
	}
	// Only the tool's own trailer may follow the issue line.
	last, rest := lastLine(text)
	if last == ToolTrailer {
		last, _ = lastLine(rest)
	}
	refs := ladder.IssueRefs(last)
	if len(refs) != 1 || refs[0].Number != task.Issue || !strings.EqualFold(refs[0].Repo, task.IssueRepo) {
		refusals = append(refusals, refuse("body", "the last line is not `Closes %s#%d.` or `Part of %s#%d (...)`",
			task.IssueRepo, task.Issue, task.IssueRepo, task.Issue))
	}
	return refusals
}

// sectionStart finds a heading that is a whole line.
func sectionStart(text, heading string) int {
	for offset := 0; ; {
		i := strings.Index(text[offset:], heading)
		if i < 0 {
			return -1
		}
		i += offset
		end := i + len(heading)
		if (i == 0 || text[i-1] == '\n') && (end == len(text) || text[end] == '\n' || strings.TrimSpace(firstLine(text[end:])) == "") {
			return i
		}
		offset = end
	}
}

// hasFilledLine reports whether a line starts with label and says something after
// it that is not only the template's comment.
func hasFilledLine(section, label string) bool {
	for _, line := range strings.Split(section, "\n") {
		if rest, ok := strings.CutPrefix(line, label); ok {
			rest = htmlCommentRE.ReplaceAllString(rest, "")
			if strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), "/")) != "" {
				return true
			}
		}
	}
	return false
}

func hasEvidenceTable(section string) bool {
	loc := tableHeaderRE.FindStringIndex(section)
	if loc == nil {
		return false
	}
	lines := strings.Split(strings.TrimPrefix(section[loc[1]:], "\n"), "\n")
	return len(lines) >= 2 && tableRuleRE.MatchString(lines[0]) && tableRowRE.MatchString(lines[1]) &&
		strings.Trim(lines[1], "| \t") != ""
}

func hasCheckbox(section, text string) bool {
	for _, line := range strings.Split(section, "\n") {
		for _, box := range []string{"- [x] ", "- [X] ", "- [ ] "} {
			if strings.HasPrefix(line, box+text) {
				return true
			}
		}
	}
	return false
}

var answerFirstLineRE = regexp.MustCompile("^Answer to the review at `?([0-9a-f]{8})`?:$")

// checkAnswer checks a builder's answer to a review, as the twr-fix-review skill
// writes it, and that the round is the next one.
func checkAnswer(task BuildTask, answer string, scanner privatescan.Scanner) []Refusal {
	var refusals []Refusal
	if task.Round < 1 || task.Round > ladder.MaxFixRounds || task.RoundsUsed != task.Round-1 {
		refusals = append(refusals, refuse("round", "round %d does not follow the %d rounds already answered (at most %d)",
			task.Round, task.RoundsUsed, ladder.MaxFixRounds))
	}
	if strings.TrimSpace(answer) == "" {
		return append(refusals, refuse("answer-missing", "a fix writes answer.md"))
	}
	refusals = append(refusals, checkProse(scanner, "answer", answer, MaxCommentBytes)...)
	if m := answerFirstLineRE.FindStringSubmatch(firstLine(answer)); m == nil || !strings.HasPrefix(task.PrevHead, m[1]) {
		refusals = append(refusals, refuse("answer-invalid", "the first line is not \"Answer to the review at `%s`:\"", short(task.PrevHead)))
	}
	if countMarkers(answer) > 0 {
		refusals = append(refusals, refuse("marker", "the answer carries a marker; the publisher adds its own"))
	}
	return refusals
}

// AnswerBody is the comment a fix posts: the builder's answer, then the fix marker.
func AnswerBody(answer string, round int) string {
	return strings.TrimRight(answer, " \t\r\n") + "\n\n" + ladder.FixMarker(round) + "\n"
}

// ParsePatch reads a `git diff --unified=0` patch and returns the lines it adds and
// the lines it removes, by file. It is the parser of go_test_guard.py in
// those-who-run/.github: `--- ` and `+++ ` are file names only between `diff --git`
// and the first hunk; inside a hunk they are a removed `-- ...` or an added
// `++ ...` line.
func ParsePatch(patch string) (added, removed map[string][]string) {
	added, removed = map[string][]string{}, map[string][]string{}
	var oldPath, newPath string
	inHeader := false
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHeader, oldPath, newPath = true, "", ""
		case strings.HasPrefix(line, "@@"):
			inHeader = false
		case inHeader && strings.HasPrefix(line, "--- "):
			oldPath, _ = strings.CutPrefix(line, "--- a/") // "" for /dev/null
		case inHeader && strings.HasPrefix(line, "+++ "):
			newPath, _ = strings.CutPrefix(line, "+++ b/")
		case inHeader:
		case strings.HasPrefix(line, "+") && newPath != "":
			added[newPath] = append(added[newPath], line[1:])
		case strings.HasPrefix(line, "-") && oldPath != "":
			removed[oldPath] = append(removed[oldPath], line[1:])
		}
	}
	return added, removed
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
