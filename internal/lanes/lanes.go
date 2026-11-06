// Package lanes says how closely a pull request is read, from the paths it changes.
//
// It is a port of the reference classifier, agent/lanes.py in those-who-run/.github,
// and reads the same lanes.json, which lives beside that file. The reference and its
// tests are the specification: the same input must give the same lane here, and
// lanes_test.go ports every case. No model reads the diff to decide the lane, so a
// pull request cannot talk its way into a lighter one.
//
//   - sensitive: any path matches a sensitive pattern, the repository's own included.
//     One sensitive file makes the whole change sensitive.
//   - light: every path is documentation, or a test file the change adds. A test that
//     is edited, renamed or deleted is not light: that is where a weakened test hides.
//   - standard: everything else, and an empty change (nothing to read is not a reason
//     to read less).
//
// A pattern is matched against the whole path from the repository's root. `*` matches
// within one directory, `**` matches across directories, and `**/` at the start also
// matches a file at the root. A rename or a copy counts as both of its paths, each
// with the rename's status, so neither is an added file.
package lanes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Lane is how closely a change is read.
type Lane string

// The three lanes, lightest first.
const (
	Light     Lane = "light"
	Standard  Lane = "standard"
	Sensitive Lane = "sensitive"
)

// statusAdded is git's status letter for a file the change adds.
const statusAdded = "A"

// Change is one path of a change with its `git diff --name-status` letter.
type Change struct {
	Status string
	Path   string
}

// Config is lanes.json, checked and compiled by Parse.
type Config struct {
	sensitive []*regexp.Regexp
	docs      []*regexp.Regexp
	tests     []*regexp.Regexp
	// repositories holds each repository's extra sensitive patterns, by its
	// lower-cased owner/name.
	repositories map[string][]*regexp.Regexp
}

// CompilePattern returns a lanes.json pattern as a regular expression over a whole
// path. An empty pattern, an absolute one or one with a backslash is refused.
func CompilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, `\`) {
		return nil, fmt.Errorf("bad pattern %q: a relative path with forward slashes", pattern)
	}
	var out strings.Builder
	out.WriteString(`^(?:`)
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			out.WriteString(`(?:[^/]+/)*`)
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			out.WriteString(`.*`)
			i += 2
		case pattern[i] == '*':
			out.WriteString(`[^/]*`)
			i++
		default:
			out.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		}
	}
	out.WriteString(`)$`)
	// QuoteMeta output and the fixed fragments above always compile.
	return regexp.MustCompile(out.String()), nil
}

// Load reads and checks the lanes.json at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's configuration, not input from a pull request
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse checks lanes.json and compiles its patterns, so a wrong key or pattern fails
// here and not at the first pull request. Every problem is reported, not only the
// first.
func Parse(data []byte) (*Config, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("lanes.json: %w", err)
	}
	var problems []string
	cfg := &Config{repositories: map[string][]*regexp.Regexp{}}
	for _, list := range []struct {
		key  string
		into *[]*regexp.Regexp
	}{{"sensitive", &cfg.sensitive}, {"docs", &cfg.docs}, {"tests", &cfg.tests}} {
		items, ok := rawList(raw[list.key])
		if !ok || len(items) == 0 {
			problems = append(problems, list.key+": a list of patterns, not empty")
			continue
		}
		compiled, bad := compileAll(list.key, items)
		problems = append(problems, bad...)
		*list.into = compiled
	}
	problems = append(problems, cfg.parseRepositories(raw["repositories"])...)
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return cfg, nil
}

// parseRepositories reads the per-repository additions. A repository may only add
// sensitive patterns: it cannot declare its own code light.
func (c *Config) parseRepositories(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var repositories map[string]json.RawMessage
	if err := json.Unmarshal(raw, &repositories); err != nil || repositories == nil {
		return []string{"repositories: an object, repository name to its patterns"}
	}
	names := make([]string, 0, len(repositories))
	for name := range repositories {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, name := range names {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(repositories[name], &extra); err != nil || extra == nil || hasOtherKey(extra, "sensitive") {
			problems = append(problems, fmt.Sprintf("repositories.%s: only `sensitive` may be added per repository", name))
			continue
		}
		items, ok := rawList(extra["sensitive"])
		if !ok {
			continue
		}
		key := "repositories." + name + ".sensitive"
		compiled, bad := compileAll(key, items)
		problems = append(problems, bad...)
		lower := strings.ToLower(name)
		c.repositories[lower] = append(c.repositories[lower], compiled...)
	}
	return problems
}

func hasOtherKey(m map[string]json.RawMessage, allowed string) bool {
	for key := range m {
		if key != allowed {
			return true
		}
	}
	return false
}

// rawList decodes a JSON array, leaving each item raw so a non-string is reported by
// name rather than failing the whole list.
func rawList(raw json.RawMessage) ([]json.RawMessage, bool) {
	if raw == nil {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, false
	}
	return items, true
}

func compileAll(key string, items []json.RawMessage) ([]*regexp.Regexp, []string) {
	var compiled []*regexp.Regexp
	var problems []string
	for _, item := range items {
		var pattern string
		if err := json.Unmarshal(item, &pattern); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s is not a string", key, item))
			continue
		}
		re, err := CompilePattern(pattern)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		compiled = append(compiled, re)
	}
	return compiled, problems
}

func matches(path string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// IsTest reports whether path matches one of the config's test patterns. The build
// publisher uses it to see a deleted test.
func (c *Config) IsTest(path string) bool { return matches(path, c.tests) }

// Classify returns the lane for changes in repo (owner/name, compared without case).
func (c *Config) Classify(repo string, changes []Change) Lane {
	if len(changes) == 0 {
		return Standard
	}
	sensitive := append(append([]*regexp.Regexp{}, c.sensitive...), c.repositories[strings.ToLower(repo)]...)
	for _, change := range changes {
		if matches(change.Path, sensitive) {
			return Sensitive
		}
	}
	for _, change := range changes {
		if matches(change.Path, c.docs) {
			continue
		}
		if change.Status == statusAdded && matches(change.Path, c.tests) {
			continue
		}
		return Standard
	}
	return Light
}

// ParseNameStatus reads `git diff --name-status` output. A rename or a copy gives
// two changes, one for each path, both with the rename's letter. Anything that is
// not that output is an error, so a caller never classifies half a diff.
func ParseNameStatus(text string) ([]Change, error) {
	var changes []Change
	for number, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		status := ""
		if fields[0] != "" {
			status = fields[0][:1]
		}
		paths := fields[1:]
		wanted := 1
		if status == "R" || status == "C" {
			wanted = 2
		}
		if status == "" || !strings.Contains("ACDMRT", status) || len(paths) != wanted || hasEmpty(paths) {
			return nil, fmt.Errorf("line %d: not `git diff --name-status` output: %q", number+1, line)
		}
		for _, path := range paths {
			changes = append(changes, Change{Status: status, Path: path})
		}
	}
	return changes, nil
}

func hasEmpty(items []string) bool {
	for _, item := range items {
		if item == "" {
			return true
		}
	}
	return false
}
