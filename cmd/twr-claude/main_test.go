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

func TestVersionHelpAndUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-version"}, env(nil), nil, strings.NewReader(""), &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != version {
		t.Errorf("exit %d, stdout %q", code, stdout.String())
	}
	if code := run(t.Context(), []string{"-h"}, env(nil), nil, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "from-stdin") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
	for _, args := range [][]string{{}, {"-nope"}, {"dance"}, {"login", "--nope"}} {
		if code := run(t.Context(), args, env(nil), nil, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestRunStopsOnConfiguration(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"run"}, env(nil), nil, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "TWR_CLAUDE_BRIEF") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
	// With a brief but no token file, run stops before starting anything.
	vars := map[string]string{"TWR_CLAUDE_BRIEF": "x", "TWR_CLAUDE_TOKEN_FILE": filepath.Join(t.TempDir(), "none"), "TWR_CLAUDE_BIN": filepath.Join(t.TempDir(), "none")}
	if code := run(t.Context(), []string{"run"}, env(vars), []string{"HOME=" + t.TempDir()}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("exit %d", code)
	}
	// And refuses first of all with a GitHub token in its environment.
	if code := run(t.Context(), []string{"run"}, env(vars), []string{"GH_TOKEN=x"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 3 {
		t.Errorf("exit %d", code)
	}
}

func TestLoginAndStatusStopOnConfiguration(t *testing.T) {
	vars := map[string]string{"TWR_CLAUDE_WARN_AFTER": "never"}
	for _, command := range []string{"login", "status"} {
		if code := run(t.Context(), []string{command}, env(vars), nil, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Errorf("%s: exit %d", command, code)
		}
	}
	// With a kubectl that is not there, status fails and login from stdin fails,
	// and neither runs claude.
	missing := map[string]string{"TWR_CLAUDE_KUBECTL": filepath.Join(t.TempDir(), "none"), "TWR_CLAUDE_BIN": filepath.Join(t.TempDir(), "none")}
	if code := run(t.Context(), []string{"status"}, env(missing), nil, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("status: exit %d", code)
	}
	if code := run(t.Context(), []string{"login", "--from-stdin"}, env(missing), nil, strings.NewReader("sk-ant-oat01-"+strings.Repeat("x", 30)), &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("login: exit %d", code)
	}
	if _, err := os.Stat(missing["TWR_CLAUDE_BIN"]); err == nil {
		t.Error("claude appeared")
	}
}
