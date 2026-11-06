package ladder

import (
	"regexp"
	"strconv"
	"strings"
)

// IssueRef is the issue a pull request body names in its `Closes` or `Part of` line.
type IssueRef struct {
	// Repo is the issue's repository, as owner/name.
	Repo   string
	Number int
	// Closes is true for `Closes`, false for `Part of`.
	Closes bool
}

// A closing line as paddock's AGENTS.md defines it: the full owner/name#N form, at
// the start of a line. `Closes #12` names no repository and is not one.
var issueRefRE = regexp.MustCompile(`(?m)^(Closes|Part of) ([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)#([1-9][0-9]{0,8})\b`)

// IssueRefs returns the issues a pull request body names in `Closes` or `Part of`
// lines, in order, without repeats.
func IssueRefs(body string) []IssueRef {
	var refs []IssueRef
	seen := map[string]bool{}
	for _, match := range issueRefRE.FindAllStringSubmatch(strings.ReplaceAll(body, "\r\n", "\n"), -1) {
		number, _ := strconv.Atoi(match[3]) // at most nine digits, by the pattern
		key := strings.ToLower(match[2]) + "#" + match[3]
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, IssueRef{Repo: match[2], Number: number, Closes: match[1] == "Closes"})
	}
	return refs
}

// RefersTo reports whether body has a `Closes` or `Part of` line for issue number in
// repo.
func RefersTo(body, repo string, number int) bool {
	for _, ref := range IssueRefs(body) {
		if ref.Number == number && strings.EqualFold(ref.Repo, repo) {
			return true
		}
	}
	return false
}
