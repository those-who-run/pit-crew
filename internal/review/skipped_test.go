package review

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/those-who-run/pit-crew/internal/github"
)

// skippedVolumes is a review the model skipped, giving reason as raw JSON.
func skippedVolumes(t *testing.T, reason string) Config {
	t.Helper()
	cfg := volumes(t, "skipped", "", "")
	write(t, cfg.Dir, "out/verdict.json", `{"repo": "those-who-run/ecu", "pr": 31, "sha": "`+head+`", "kind": "B", "lane": "standard", "verdict": "skipped", "reason": `+reason+`}`)
	return cfg
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Each case runs under both log formats the command offers. The status and the
// conversation stay the publisher's own words, whatever the reason says.
func TestTheSkipReasonIsLoggedAndGoesNowhereElse(t *testing.T) {
	const forged = `time=2026-11-08T00:00:00Z level=ERROR msg="forged line" verdict=ready`
	cases := []struct {
		name, reason, wantIn string
	}{
		{"a plain reason", "committer web-flow is outside the list", "committer web-flow is outside the list"},
		{"a reason that tries to start a log line", "ok\n" + forged + "\r\n{\"level\":\"ERROR\",\"msg\":\"forged\"}", "ok"},
		{"terminal escapes", "a\x1b[2Jb\u009bc d", "a [2Jb c d"},
		{"a huge reason", strings.Repeat("é", 200000), "é"},
	}
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	}
	for _, c := range cases {
		for format, handler := range handlers {
			t.Run(c.name+", "+format, func(t *testing.T) {
				cfg := skippedVolumes(t, jsonString(t, c.reason))
				g := &gh{pull: github.PullRequest{HeadSHA: head}}
				var out bytes.Buffer
				if code := Run(t.Context(), cfg, g, slog.New(handler(&out))); code != 0 {
					t.Fatalf("exit %d; log %s", code, out.String())
				}
				want := []string{"1f0c5d2e review / verdict success: skipped: kind B, lane standard"}
				if !reflect.DeepEqual(g.statuses, want) || len(g.created) != 0 || len(g.updated) != 0 {
					t.Errorf("statuses %q, created %q, updated %v", g.statuses, g.created, g.updated)
				}
				lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
				if len(lines) != 2 {
					t.Fatalf("%d log lines, want the reason's and the publish line: %q", len(lines), out.String())
				}
				line := lines[0]
				if !strings.Contains(line, "reason") || !strings.Contains(line, c.wantIn) {
					t.Errorf("reason line %q lacks %q", line, c.wantIn)
				}
				if len(line) > 3000 {
					t.Errorf("reason line is %d bytes", len(line))
				}
				for _, l := range lines {
					if strings.HasPrefix(l, "time=2026-11-08T00") || strings.Contains(l, "\x1b") {
						t.Errorf("forged or raw output: %q", l)
					}
				}
			})
		}
	}
}

// A reason is for the log alone: one the model wrote oddly costs nothing, and one
// it left out adds no line.
func TestAnOddSkipReasonDoesNotHoldTheReview(t *testing.T) {
	for _, reason := range []string{`42`, `["author", "committer"]`, `{"why": "x"}`, `null`, `""`} {
		g := &gh{pull: github.PullRequest{HeadSHA: head}}
		var out bytes.Buffer
		cfg := skippedVolumes(t, reason)
		if code := Run(t.Context(), cfg, g, slog.New(slog.NewJSONHandler(&out, nil))); code != 0 || len(g.statuses) != 1 {
			t.Errorf("%s: exit %d, statuses %q", reason, code, g.statuses)
		}
		hasLine := strings.Contains(out.String(), `"reason"`)
		if wantLine := reason != `null` && reason != `""`; hasLine != wantLine {
			t.Errorf("%s: reason line %v, want %v: %s", reason, hasLine, wantLine, out.String())
		}
	}
}
