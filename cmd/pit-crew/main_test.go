package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

const lanesJSON = "../../internal/lanes/testdata/lanes.json"

func TestVersionAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-version"}, env(nil), strings.NewReader(""), &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != version {
		t.Errorf("exit %d, stdout %q", code, stdout.String())
	}
	for _, arg := range []string{"-h", "-help"} {
		stderr.Reset()
		if code := run(t.Context(), []string{arg}, env(nil), strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "publish-review") {
			t.Errorf("%s: exit %d, stderr %q", arg, code, stderr.String())
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"-nope"}, {"dance"}, {"poll", "extra"}} {
		var stderr bytes.Buffer
		if code := run(t.Context(), args, env(nil), strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	for _, c := range [][2]string{{"LOG_LEVEL", "loud"}, {"LOG_FORMAT", "yaml"}} {
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"lane"}, env(map[string]string{c[0]: c[1]}), strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), c[0]) {
			t.Errorf("%s=%s: exit %d, stderr %q", c[0], c[1], code, stderr.String())
		}
	}
}

// LOG_FORMAT picks the handler and nothing else: the same record, at the same
// level, comes out as a JSON object or as slog's text line.
func TestLogFormat(t *testing.T) {
	for _, c := range []struct {
		format string
		want   string
	}{
		{"", `{"time":`},
		{"json", `{"time":`},
		{"text", "time="},
	} {
		var out bytes.Buffer
		logger, err := newLogger(&out, "warn", c.format)
		if err != nil {
			t.Fatalf("%q: %v", c.format, err)
		}
		logger.Info("quiet", "job", "x")
		logger.Warn("loud", "job", "x")
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 1 || !strings.HasPrefix(lines[0], c.want) || !strings.Contains(lines[0], "loud") || !strings.Contains(lines[0], "WARN") {
			t.Errorf("%q: %q", c.format, out.String())
		}
		if c.format == "text" && !strings.Contains(lines[0], ` msg=loud job=x`) {
			t.Errorf("text: %q", out.String())
		}
		if c.format != "text" && !strings.Contains(lines[0], `"msg":"loud","job":"x"}`) {
			t.Errorf("%q: %q", c.format, out.String())
		}
	}
}

// The cases of test_lanes.py's CommandLine class.
func TestLane(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"lane", "--repo", "those-who-run/ecu", "--config", lanesJSON}, env(nil), strings.NewReader("M\tREADME.md\n"), &stdout, &stderr)
	if code != 0 || stdout.String() != "light\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run(t.Context(), []string{"lane", "--repo", "those-who-run/ecu", "--config", lanesJSON}, env(nil), strings.NewReader("nonsense\n"), &stdout, &stderr)
	if code != 2 || stdout.String() != "" || !strings.Contains(stderr.String(), "name-status") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{{"lane"}, {"lane", "--repo", "x"}, {"lane", "--repo", "x", "--config", filepath.Join(t.TempDir(), "none.json")}, {"lane", "--nope"}} {
		if code := run(t.Context(), args, env(nil), strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// Every step stops on its configuration before it reaches the network.
func TestStepsStopOnConfiguration(t *testing.T) {
	cases := []struct {
		command string
		vars    map[string]string
		want    string
	}{
		{"poll", map[string]string{"POLL_MAX_ACTIVE": "x"}, "POLL_MAX_ACTIVE"},
		{"poll", map[string]string{"LADDER_COMPONENTS": "x"}, "LADDER_COMPONENTS"},
		{"poll", map[string]string{}, "APP_CLIENT_ID"},
		{"ready", map[string]string{"LADDER_COMPONENTS": "x"}, "LADDER_COMPONENTS"},
		{"ready", map[string]string{}, "APP_CLIENT_ID"},
		{"fetch", map[string]string{}, "FETCH_KIND"},
		{"fetch", map[string]string{"FETCH_KIND": "build", "FETCH_REPO": "o/r", "FETCH_ISSUE": "1", "FETCH_ISSUE_REPO": "o/p", "FETCH_AGENT_REF": "v1",
			"FETCH_BUILDER_NAME": "b", "FETCH_BUILDER_EMAIL": "b@example.com"}, "APP_CLIENT_ID"},
		{"publish-review", map[string]string{}, "PUBLISH_NUMBER"},
		{"publish-review", map[string]string{"PUBLISH_REPO": "o/r", "PUBLISH_NUMBER": "1", "PUBLISH_HEAD_SHA": strings.Repeat("a", 40), "PUBLISH_HOLD_DIR": "/hold"}, "APP_CLIENT_ID"},
		{"publish-build", map[string]string{}, "PUBLISH_MODE"},
		{"publish-build", map[string]string{"PUBLISH_MODE": "build", "PUBLISH_REPO": "o/r", "PUBLISH_ISSUE_REPO": "o/p", "PUBLISH_ISSUE": "1",
			"PUBLISH_BUILDER_NAME": "b", "PUBLISH_BUILDER_EMAIL": "b@example.com", "PUBLISH_HOLD_DIR": "/hold"}, "APP_CLIENT_ID"},
	}
	for _, c := range cases {
		t.Run(c.command+" "+c.want, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), []string{c.command}, env(c.vars), strings.NewReader(""), &stdout, &stderr); code != 2 {
				t.Errorf("exit %d, stderr %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.want) {
				t.Errorf("stderr %q does not name %s", stderr.String(), c.want)
			}
		})
	}
}

// With an App configured but no key file, the token cannot be minted and the
// step still stops before any step-specific work.
func TestStepsStopWhenTheKeyIsMissing(t *testing.T) {
	keyless := map[string]string{"APP_CLIENT_ID": "Iv1.x", "APP_KEY_FILE": filepath.Join(t.TempDir(), "none.pem"),
		"GITHUB_API_URL": "http://127.0.0.1:1", "FETCH_KIND": "build", "FETCH_REPO": "o/r", "FETCH_ISSUE": "1", "FETCH_ISSUE_REPO": "o/p",
		"FETCH_AGENT_REF": "v1", "FETCH_BUILDER_NAME": "b", "FETCH_BUILDER_EMAIL": "b@example.com"}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"fetch"}, env(keyless), strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Stat("/work"); err == nil {
		t.Error("the step wrote to /work")
	}
}
