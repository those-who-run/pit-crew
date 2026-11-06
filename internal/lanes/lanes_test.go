package lanes

// These are the cases of agent/tests/test_lanes.py in those-who-run/.github, ported
// one for one: the reference says the same input must give the same lane in any port.
// testdata/lanes.json is a copy of agent/lanes.json there, which is the canonical
// file; the jobs read the real one from .github at a pinned release.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const ecu = "those-who-run/ecu"

func committed(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load(filepath.Join("testdata", "lanes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func m(path string) []Change { return []Change{{"M", path}} }

func TestPatterns(t *testing.T) {
	cases := []struct {
		name, pattern, path string
		want                bool
	}{
		{"a star stays in one directory", "docs/*.md", "docs/board.md", true},
		{"a star does not cross a directory", "docs/*.md", "docs/adr/0001.md", false},
		{"a double star crosses directories", "deploy/**", "deploy/base/nats/kustomization.yaml", true},
		{"a double star is anchored at the root", "deploy/**", "docs/deploy/readme.txt", false},
		{"a leading double star matches at the root", "**/go.mod", "go.mod", true},
		{"a leading double star matches below", "**/go.mod", "tools/go.mod", true},
		{"a leading double star still ends the path", "**/go.mod", "tools/go.mod.bak", false},
		{"the whole path must match", "AGENTS.md", "docs/AGENTS.md.txt", false},
		{"a name that only starts the same", "**/Dockerfile", "docs/Dockerfile-notes.md", false},
		{"a dot is a dot", ".github/**", "xgithub/workflows/ci.yml", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re, err := CompilePattern(c.pattern)
			if err != nil {
				t.Fatal(err)
			}
			if got := re.MatchString(c.path); got != c.want {
				t.Errorf("%q against %q: %v, want %v", c.pattern, c.path, got, c.want)
			}
		})
	}
}

func TestBadPatternsAreRefused(t *testing.T) {
	for _, pattern := range []string{"", "/etc/passwd", `a\b`} {
		if _, err := CompilePattern(pattern); err == nil {
			t.Errorf("%q compiled, want an error", pattern)
		}
	}
}

func TestClassify(t *testing.T) {
	cfg := committed(t)
	cases := []struct {
		name    string
		repo    string
		changes []Change
		want    Lane
	}{
		// Sensitive: each kind.
		{"a workflow", ecu, m(".github/workflows/ci.yml"), Sensitive},
		{"the guard", ecu, m(".claude/hooks/guard.py"), Sensitive},
		{"AGENTS.md", ecu, m("AGENTS.md"), Sensitive},
		{"CLAUDE.md", ecu, m("CLAUDE.md"), Sensitive},
		{"CODEOWNERS", ecu, m(".github/CODEOWNERS"), Sensitive},
		{"an ADR", ecu, m("docs/adr/0015-something.md"), Sensitive},
		{"a proto file", ecu, m("proto/twr/v1/frame.proto"), Sensitive},
		{"the subjects catalogue", ecu, m("subjects.yaml"), Sensitive},
		{"a manifest", ecu, m("deploy/base/ecu/deployment.yaml"), Sensitive},
		{"a Dockerfile", ecu, m("Dockerfile"), Sensitive},
		{"go.mod", ecu, m("go.mod"), Sensitive},
		{"the tools go.mod", ecu, m("tools/go.mod"), Sensitive},
		{"pyproject.toml", ecu, m("pyproject.toml"), Sensitive},
		{"a package.json", ecu, m("web/package.json"), Sensitive},
		{"a migration", ecu, m("internal/store/migrate.go"), Sensitive},
		{"renovate.json", ecu, m("renovate.json"), Sensitive},
		{"one sensitive file makes the change sensitive", ecu,
			[]Change{{"M", "README.md"}, {"M", "race/race.go"}, {"M", "go.mod"}}, Sensitive},
		// AGENTS.md and an ADR are Markdown, and still the maintainer's to read.
		{"sensitive wins over docs", ecu, []Change{{"A", "docs/adr/0015-something.md"}}, Sensitive},
		{"a lock file alone is not sensitive: go.sum", ecu, m("go.sum"), Standard},
		{"a lock file alone is not sensitive: uv.lock", ecu, m("uv.lock"), Standard},
		{"near miss: deploy inside internal", ecu, m("internal/deploy/render.go"), Standard},
		{"near miss: proto inside internal", ecu, m("internal/proto/decode.go"), Standard},
		{"a repository adds its own: .github", "those-who-run/.github", m("README.md"), Sensitive},
		{"a repository adds its own: chassis templates", "those-who-run/chassis", m("README.md.jinja"), Sensitive},
		{"a repository's additions stop at its patterns", "those-who-run/chassis", m("README.md"), Light},
		{"the repository name ignores case", "Those-Who-Run/.GitHub", m("README.md"), Sensitive},
		// These two are what the ladder itself stands on.
		{"the settings are sensitive everywhere", "those-who-run/anything", m(".claude/settings.json"), Sensitive},
		{"the workflows are sensitive everywhere", "those-who-run/anything", m(".github/workflows/release.yml"), Sensitive},

		// Light.
		{"docs only", ecu, []Change{{"M", "README.md"}, {"A", "docs/board.md"}, {"D", "docs/old.md"}}, Light},
		{"tests added only: Go", ecu, []Change{{"A", "race/grid_test.go"}, {"A", "race/testdata/grid.jsonl.gz"}}, Light},
		{"tests added only: Python and TypeScript", ecu,
			[]Change{{"A", "tests/test_links.py"}, {"A", "web/src/laps.test.tsx"}}, Light},
		{"docs and added tests together", ecu, []Change{{"M", "README.md"}, {"A", "race/grid_test.go"}}, Light},
		{"an edited test is not light", ecu, m("race/grid_test.go"), Standard},
		{"a deleted test is not light", ecu, []Change{{"D", "race/grid_test.go"}}, Standard},
		{"code beside docs is standard", ecu, []Change{{"M", "README.md"}, {"M", "race/race.go"}}, Standard},

		// Standard.
		{"component code", ecu, []Change{{"M", "race/race.go"}, {"A", "race/grid_test.go"}}, Standard},
		{"an empty change is not light", ecu, nil, Standard},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cfg.Classify(c.repo, c.changes); got != c.want {
				t.Errorf("lane %q, want %q", got, c.want)
			}
		})
	}
}

func TestARenamedTestIsNotLight(t *testing.T) {
	changes, err := ParseNameStatus("R100\trace/grid_test.go\trace/start_test.go\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := committed(t).Classify(ecu, changes); got != Standard {
		t.Errorf("lane %q, want standard", got)
	}
}

func TestARenameIntoASensitivePathIsSensitive(t *testing.T) {
	changes, err := ParseNameStatus("R087\tdocs/a.md\tproto/a.md\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := committed(t).Classify(ecu, changes); got != Sensitive {
		t.Errorf("lane %q, want sensitive", got)
	}
}

func TestParseNameStatus(t *testing.T) {
	cases := []struct {
		name, text string
		want       []Change
	}{
		{"plain lines", "M\tREADME.md\nA\trace/grid_test.go\n\n", []Change{{"M", "README.md"}, {"A", "race/grid_test.go"}}},
		{"a rename gives both paths", "R087\tdocs/a.md\tproto/a.md\n", []Change{{"R", "docs/a.md"}, {"R", "proto/a.md"}}},
		{"CRLF line endings", "M\tREADME.md\r\n", []Change{{"M", "README.md"}}},
		{"nothing", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseNameStatus(c.text)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestOtherTextIsRefused(t *testing.T) {
	for _, text := range []string{"README.md\n", "M\n", "X\tREADME.md\n", "R100\tonly-one\n", "M\ta\tb\n", "\tREADME.md\n", "M\t\n"} {
		if _, err := ParseNameStatus(text); err == nil || !strings.Contains(err.Error(), "name-status") {
			t.Errorf("%q: error %v, want one naming name-status", text, err)
		}
	}
}

func TestConfig(t *testing.T) {
	cases := []struct {
		name, config, want string
	}{
		{"a missing list is refused", `{"sensitive": [".github/**"], "docs": ["**/*.md"]}`, "tests: a list"},
		{"an empty list is refused", `{"sensitive": [], "docs": ["**/*.md"], "tests": ["tests/**"]}`, "sensitive: a list"},
		{"a list that is not one is refused", `{"sensitive": "x", "docs": ["**/*.md"], "tests": ["tests/**"]}`, "sensitive: a list"},
		{"a bad pattern is refused", `{"sensitive": ["/abs"], "docs": ["**/*.md"], "tests": ["tests/**"]}`, "bad pattern"},
		{"a pattern that is not a string is refused", `{"sensitive": [7], "docs": ["**/*.md"], "tests": ["tests/**"]}`, "7 is not a string"},
		// A repository can't declare its own code light.
		{"a repository may only add sensitive patterns",
			`{"sensitive": [".github/**"], "docs": ["**/*.md"], "tests": ["tests/**"],
			  "repositories": {"those-who-run/ecu": {"docs": ["**"]}}}`, "only `sensitive` may be added"},
		{"a repository's entry must be an object",
			`{"sensitive": [".github/**"], "docs": ["**/*.md"], "tests": ["tests/**"],
			  "repositories": {"those-who-run/ecu": ["**"]}}`, "only `sensitive` may be added"},
		{"repositories must be an object",
			`{"sensitive": [".github/**"], "docs": ["**/*.md"], "tests": ["tests/**"], "repositories": []}`,
			"repositories: an object"},
		{"a repository's bad pattern is refused",
			`{"sensitive": [".github/**"], "docs": ["**/*.md"], "tests": ["tests/**"],
			  "repositories": {"those-who-run/ecu": {"sensitive": ["a\\b"]}}}`, "repositories.those-who-run/ecu.sensitive: bad pattern"},
		{"not JSON", `nope`, "lanes.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.config))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestEveryProblemIsReported(t *testing.T) {
	_, err := Parse([]byte(`{"sensitive": ["/abs"], "docs": []}`))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"bad pattern", "docs: a list", "tests: a list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestARepositoryWithoutSensitivePatternsAddsNothing(t *testing.T) {
	cfg, err := Parse([]byte(`{"sensitive": [".github/**"], "docs": ["**/*.md"], "tests": ["tests/**"],
		"repositories": {"those-who-run/ecu": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Classify(ecu, m("README.md")); got != Light {
		t.Errorf("lane %q, want light", got)
	}
}

func TestIsTest(t *testing.T) {
	cfg := committed(t)
	for path, want := range map[string]bool{
		"race/grid_test.go": true, "tests/test_x.py": true, "web/src/a.test.ts": true, "race/race.go": false,
	} {
		if got := cfg.IsTest(path); got != want {
			t.Errorf("IsTest(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestLoadAMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); err == nil || !os.IsNotExist(unwrapAll(err)) {
		t.Errorf("error %v, want a missing file", err)
	}
}

func unwrapAll(err error) error {
	for {
		inner, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = inner.Unwrap()
	}
}
