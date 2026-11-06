package ladder

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// BuildPolicy is who may start a build, and where builds may land. The zero value
// builds nothing.
type BuildPolicy struct {
	// Maintainer is the login whose label starts a build. Nobody else's does.
	Maintainer string
	// Builder is the login build Jobs publish as.
	Builder string
	// Authors are the logins whose open pull request for an issue means the issue is
	// already being worked on.
	Authors []string
	// ReadyLabel starts a build; TriageLabel and HoldLabel stop one.
	ReadyLabel  string
	TriageLabel string
	HoldLabel   string
	// ComponentPrefix is the start of a label that names the repository the change
	// lands in, such as `component: `.
	ComponentPrefix string
	// Components maps a component label to its repository (owner/name) where the
	// name alone does not say. An empty value means the component is never built by
	// a Job.
	Components map[string]string
	// Owner is the organisation a component's repository belongs to.
	Owner string
	// Repos are the repositories a build may land in: the ones the builder is
	// installed on.
	Repos []string
}

// LabelEvent is one time a label was added to or removed from an issue.
type LabelEvent struct {
	Label string
	// Actor is the login that did it.
	Actor string
	Added bool
	At    time.Time
}

// LinkedPR is a pull request whose body names the issue in a `Closes` or `Part of`
// line.
type LinkedPR struct {
	Repo      string
	Number    int
	Author    string
	Open      bool
	CreatedAt time.Time
}

// Issue is what the ladder reads about one open issue.
type Issue struct {
	// Repo is the issue's repository, as owner/name.
	Repo        string
	Number      int
	Labels      []string
	LabelEvents []LabelEvent
	LinkedPRs   []LinkedPR
	// Branches are the branches in the target repository that start with
	// BranchPrefix(Number).
	Branches []string
}

// BranchPrefix is the start of every branch a build Job makes for an issue.
func BranchPrefix(issue int) string { return "agent/" + strconv.Itoa(issue) + "-" }

// BuildDecision is whether to start a build for an issue, and why not when not.
type BuildDecision struct {
	Build bool
	// Reason is a short, stable word; it never holds text from the issue.
	Reason string
	// Repo is the repository the build lands in, when the labels name exactly one.
	Repo string
}

// Target returns the one repository an issue's component labels name, or a reason
// there is none: no component label, more than one repository, a component that is
// never built, or a repository outside Repos.
func (p BuildPolicy) Target(labels []string) (repo, reason string) {
	targets := map[string]bool{}
	for _, label := range labels {
		if p.ComponentPrefix == "" || !strings.HasPrefix(label, p.ComponentPrefix) {
			continue
		}
		mapped, ok := p.Components[label]
		if !ok {
			mapped = p.Owner + "/" + strings.TrimSpace(strings.TrimPrefix(label, p.ComponentPrefix))
		}
		if mapped == "" {
			return "", "component-not-built"
		}
		targets[strings.ToLower(mapped)] = true
	}
	switch len(targets) {
	case 0:
		return "", "no-component"
	case 1:
	default:
		return "", "several-components"
	}
	for target := range targets {
		for _, allowed := range p.Repos {
			if strings.EqualFold(allowed, target) {
				return allowed, ""
			}
		}
	}
	return "", "repository-not-allowed"
}

// labelledAt finds the event that put the label on the issue: the last time it was
// added. The actor of that event is who asked for the build.
func labelledAt(events []LabelEvent, label string) (LabelEvent, bool) {
	sorted := append([]LabelEvent(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	for i := len(sorted) - 1; i >= 0; i-- {
		if sorted[i].Added && strings.EqualFold(sorted[i].Label, label) {
			return sorted[i], true
		}
	}
	return LabelEvent{}, false
}

// Decide says whether an open issue is due a build: it carries the ready label, the
// maintainer put it there, it is neither untriaged nor held, its labels name one
// repository the builder may work in, and nothing is being built for it already.
//
// One label event starts at most one build. A pull request the builder opened for
// the issue after the label was added means that event is spent, whatever became of
// the pull request; the maintainer adds the label again to ask for another.
func (p BuildPolicy) Decide(issue Issue) BuildDecision {
	if p.ReadyLabel == "" || !hasLabel(issue.Labels, p.ReadyLabel) {
		return BuildDecision{Reason: "not-ready"}
	}
	if p.TriageLabel != "" && hasLabel(issue.Labels, p.TriageLabel) {
		return BuildDecision{Reason: "needs-triage"}
	}
	if p.HoldLabel != "" && hasLabel(issue.Labels, p.HoldLabel) {
		return BuildDecision{Reason: "hold"}
	}
	event, ok := labelledAt(issue.LabelEvents, p.ReadyLabel)
	if !ok {
		return BuildDecision{Reason: "no-label-event"}
	}
	if !sameLogin(event.Actor, p.Maintainer) {
		return BuildDecision{Reason: "label-not-by-maintainer"}
	}
	repo, reason := p.Target(issue.Labels)
	if reason != "" {
		return BuildDecision{Reason: reason}
	}
	for _, pr := range issue.LinkedPRs {
		if pr.Open && (hasLogin(p.Authors, pr.Author) || sameLogin(pr.Author, p.Builder)) {
			return BuildDecision{Reason: "open-pull-request", Repo: repo}
		}
		if sameLogin(pr.Author, p.Builder) && pr.CreatedAt.After(event.At) {
			return BuildDecision{Reason: "already-built", Repo: repo}
		}
	}
	for _, branch := range issue.Branches {
		if strings.HasPrefix(branch, BranchPrefix(issue.Number)) {
			return BuildDecision{Reason: "branch-exists", Repo: repo}
		}
	}
	return BuildDecision{Build: true, Reason: "labelled-by-maintainer", Repo: repo}
}

// Slug turns an issue title into the tail of a branch name: lower-case letters,
// digits and hyphens, at most 40 characters. No model chooses a branch name.
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "work"
	}
	return slug
}
