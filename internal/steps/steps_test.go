package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil { //nolint:gosec // the test's temp dir
		t.Fatal(err)
	}
	return p
}

func TestScanner(t *testing.T) {
	dir := t.TempDir()
	if _, err := Scanner("", ""); err != nil {
		t.Error(err)
	}
	allow := write(t, dir, "extra.allow", "status.example-vendor.io A vendor's status page.\n")
	patterns := write(t, dir, "patterns.txt", "lab-host garage-[a-z]+\n")
	s, err := Scanner(allow, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Scan("https://status.example-vendor.io/x and garage-pi\n"); len(got) != 1 || got[0].Rule != "lab-host" {
		t.Errorf("findings %+v", got)
	}
	for _, bad := range [][2]string{
		{filepath.Join(dir, "none"), ""},
		{"", filepath.Join(dir, "none")},
		{write(t, dir, "bad.allow", "nuc.lan why\n"), ""},
		{"", write(t, dir, "bad.txt", "x [\n")},
	} {
		if _, err := Scanner(bad[0], bad[1]); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

func TestTaskAndOutput(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadTask(dir); err == nil {
		t.Error("no error without task.json")
	}
	write(t, dir, "data/task.json", "nope")
	if _, err := ReadTask(dir); err == nil {
		t.Error("no error for a task.json that is not JSON")
	}
	write(t, dir, "data/task.json", `{"kind": "review", "repo": "those-who-run/ecu", "number": 31}`)
	task, err := ReadTask(dir)
	if err != nil || task.Kind != "review" || task.Number != 31 {
		t.Errorf("%+v %v", task, err)
	}
	if data, err := ReadOutput(dir, "comment.md"); err != nil || data != nil {
		t.Errorf("missing output: %q %v", data, err)
	}
	write(t, dir, "out/comment.md", "Review")
	if data, err := ReadOutput(dir, "comment.md"); err != nil || string(data) != "Review" {
		t.Errorf("%q %v", data, err)
	}
}

func TestHold(t *testing.T) {
	root := t.TempDir()
	if _, err := HoldDir("", "those-who-run/ecu", 31, "abc"); err == nil {
		t.Error("no error without a hold root")
	}
	dir, err := HoldDir(root, "those-who-run/.github", 31, strings.Repeat("a", 40))
	if err != nil || filepath.Base(dir) != "github-31-aaaaaaaa" {
		t.Errorf("%q %v", dir, err)
	}
	if err := Hold(dir, map[string][]byte{"reasons.md": []byte("why"), "private.md": nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "reasons.md")); err != nil {
		t.Error("reasons.md not written")
	}
	if _, err := os.Stat(filepath.Join(dir, "private.md")); err == nil {
		t.Error("a nil file was written")
	}
	if err := Hold(filepath.Join(root, "none"), map[string][]byte{"x": []byte("y")}); err == nil {
		t.Error("no error for a missing directory")
	}
}
