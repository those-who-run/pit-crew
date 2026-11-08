package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/kube"
	"github.com/those-who-run/pit-crew/internal/ladder"
)

const (
	head     = "1f0c5d2e9a7b4c3d8e6f1a2b3c4d5e6f7a8b9c0d"
	reviewer = "twr-reviewer[bot]"
	builder  = "twr-builder[bot]"
	owner    = "octo-maintainer"
)

func at(minutes int) time.Time {
	return time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
}

// source is GitHub as the poller sees it, held in memory. It is data, not
// expectations: the tests check the decisions, not the calls.
type source struct {
	repos    []string
	pulls    map[string][]github.PullRequest
	commits  map[string][]github.Commit
	comments map[string][]github.Comment
	statuses map[string][]github.Status
	checks   map[string]map[string]string
	required map[string][]string
	issues   []github.Issue
	// timeline holds label events by repo#number, for issues and pull requests
	// alike, as GitHub's issue timeline does.
	timeline map[string][]github.TimelineEvent
	branches map[string][]string
	// broken makes one repository's pull list fail.
	broken string
	// reposErr is the answer to the first call, the installation's repositories.
	reposErr error
}

func key(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func (s *source) ListInstallationRepos(context.Context) ([]string, error) {
	return s.repos, s.reposErr
}
func (s *source) ListOpenPulls(_ context.Context, repo string) ([]github.PullRequest, error) {
	if repo == s.broken {
		return nil, errors.New("boom")
	}
	return s.pulls[repo], nil
}
func (s *source) ListPullCommits(_ context.Context, repo string, n int) ([]github.Commit, error) {
	return s.commits[key(repo, n)], nil
}
func (s *source) ListComments(_ context.Context, repo string, n int) ([]github.Comment, error) {
	return s.comments[key(repo, n)], nil
}
func (s *source) ListStatuses(_ context.Context, _, sha string) ([]github.Status, error) {
	return s.statuses[sha], nil
}
func (s *source) ListChecks(_ context.Context, _, sha string) (map[string]string, error) {
	return s.checks[sha], nil
}
func (s *source) ListRequiredChecks(_ context.Context, repo, _ string) ([]string, error) {
	if r, ok := s.required[repo]; ok {
		return r, nil
	}
	return nil, &github.Error{Status: 404, Message: "Not Found"}
}
func (s *source) ListOpenIssues(context.Context, string, []string) ([]github.Issue, error) {
	return s.issues, nil
}
func (s *source) ListTimeline(_ context.Context, repo string, n int) ([]github.TimelineEvent, error) {
	return s.timeline[key(repo, n)], nil
}
func (s *source) ListBranches(_ context.Context, repo, prefix string) ([]string, error) {
	var out []string
	for _, b := range s.branches[repo] {
		if strings.HasPrefix(b, prefix) {
			out = append(out, b)
		}
	}
	return out, nil
}

// jobs is the cluster's Job list, held in memory.
type jobs struct {
	list    []kube.Job
	exits   map[string]int
	created [][]byte
	refuse  error
}

func (j *jobs) ListJobs(context.Context, string) ([]kube.Job, error) {
	if j.refuse != nil {
		return nil, j.refuse
	}
	return j.list, nil
}

func (j *jobs) CreateJob(_ context.Context, job []byte) error {
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(job, &meta)
	for _, have := range j.list {
		if have.Name == meta.Metadata.Name {
			return &kube.Error{Status: 409, Reason: "AlreadyExists"}
		}
	}
	j.created = append(j.created, job)
	return nil
}

func (j *jobs) ContainerExit(_ context.Context, name, _ string) (int, bool, error) {
	code, ok := j.exits[name]
	return code, ok, nil
}

func pull(repo string, n int, author, sha string) github.PullRequest {
	return github.PullRequest{Repo: repo, Number: n, Title: "feat: " + key(repo, n), Author: author, HeadRepo: repo,
		HeadRef: "agent/232-x", HeadSHA: sha, BaseRef: "main", URL: "https://github.com/" + repo + "/pull/" + fmt.Sprint(n),
		Body: "x\n\nCloses those-who-run/paddock#232.\n"}
}

func reviewComment(sha string, verdict ladder.Verdict, minutes int) github.Comment {
	m := ladder.ReviewMarker{SHA: sha, Kind: "A", Lane: "standard", Verdict: verdict}
	return github.Comment{ID: int64(minutes), Author: reviewer, Body: "Review at head\n\n" + m.String(), CreatedAt: at(minutes), UpdatedAt: at(minutes)}
}

// world is a small organisation: one builder pull request waiting for review,
// one of the maintainer's that is ready, one of a stranger's, one builder pull
// request with changes asked, and an issue the maintainer labelled.
func world() *source {
	ecu, logbook := "those-who-run/ecu", "those-who-run/logbook"
	sha2 := "2222222222222222222222222222222222222222"
	sha3 := "3333333333333333333333333333333333333333"
	sha4 := "4444444444444444444444444444444444444444"
	return &source{
		repos: []string{ecu, logbook},
		pulls: map[string][]github.PullRequest{
			ecu:     {pull(ecu, 31, builder, head), pull(ecu, 30, owner, sha2), pull(ecu, 29, "someone-else", sha3)},
			logbook: {pull(logbook, 7, builder, sha4)},
		},
		commits: map[string][]github.Commit{
			key(ecu, 31):    {{SHA: head, Author: builder, Committer: builder}},
			key(ecu, 30):    {{SHA: sha2, Author: owner, Committer: owner}},
			key(ecu, 29):    {{SHA: sha3, Author: "someone-else", Committer: "someone-else"}},
			key(logbook, 7): {{SHA: sha4, Author: builder, Committer: builder}},
		},
		comments: map[string][]github.Comment{
			key(ecu, 30):    {reviewComment(sha2, ladder.VerdictReady, 1)},
			key(logbook, 7): {reviewComment(sha4, ladder.VerdictChanges, 1)},
		},
		statuses: map[string][]github.Status{},
		checks:   map[string]map[string]string{sha2: {"test": "success", "go / check": "success"}},
		required: map[string][]string{ecu: {"test", "go / check"}},
		issues:   []github.Issue{{Repo: "those-who-run/paddock", Number: 232, Title: "Fuel callout", Labels: []string{"component: ecu", "agent-ready"}}},
		timeline: map[string][]github.TimelineEvent{key("those-who-run/paddock", 232): {
			{Event: "labeled", Actor: owner, Label: "agent-ready", At: at(-60)},
		}},
	}
}

func policies() Policies {
	return Policies{
		Pull: ladder.Policy{Reviewer: reviewer, Builder: builder, Authors: []string{builder, owner}, HoldLabel: "hold",
			Maintainer: owner, ReReviewLabel: "re-review"},
		Build: ladder.BuildPolicy{Maintainer: owner, Builder: builder, Authors: []string{builder, owner}, ReadyLabel: "agent-ready",
			TriageLabel: "needs triage", HoldLabel: "hold", ComponentPrefix: "component: ", Owner: "those-who-run",
			Repos: []string{"those-who-run/ecu"}},
		IssueRepo: "those-who-run/paddock",
	}
}

func templates(t *testing.T, kinds ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, kind := range kinds {
		text := `{"apiVersion": "batch/v1", "kind": "Job", "metadata": {"labels": {"team": "twr"}},
		  "spec": {"template": {"spec": {"initContainers": [{"name": "fetch", "env": [
		    {"name": "FETCH_KIND", "value": "{{kind}}"}, {"name": "FETCH_REPO", "value": "{{repo}}"},
		    {"name": "FETCH_NUMBER", "value": "{{number}}"}, {"name": "FETCH_HEAD_SHA", "value": "{{head_sha}}"},
		    {"name": "FETCH_ISSUE", "value": "{{issue}}"}, {"name": "FETCH_ROUND", "value": "{{round}}"},
		    {"name": "JOB", "value": "{{job_name}}"}]}]}}}}`
		if err := os.WriteFile(filepath.Join(dir, kind+".json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func logger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newPoller(t *testing.T, src *source, j *jobs, limits Limits) *Poller {
	t.Helper()
	return &Poller{Source: src, Jobs: j, Policies: policies(), Limits: limits, Logger: logger(), Now: func() time.Time { return at(0) }}
}

func TestRunDecidesAndStartsOneJob(t *testing.T) {
	j := &jobs{}
	p := newPoller(t, world(), j, Limits{StartJobs: true, TemplateDir: templates(t, "review", "fix", "build"), MaxActive: 1, DailyCap: 12})
	hb, err := p.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantDecisions := map[string]int{"review": 1, "ready": 1, "none": 1, "fix": 1, "build": 1}
	if !reflect.DeepEqual(hb.Decisions, wantDecisions) || hb.Pulls != 4 || hb.Issues != 1 || hb.Errors != 0 {
		t.Errorf("heartbeat %+v", hb)
	}
	if hb.Reasons["author-not-allowlisted"] != 1 || hb.Reasons["labelled-by-maintainer"] != 1 {
		t.Errorf("reasons %v", hb.Reasons)
	}
	// A fix comes before a review and a build.
	if hb.Started != "fix-logbook-7-r1" || len(j.created) != 1 {
		t.Fatalf("started %q, created %d", hb.Started, len(j.created))
	}
	var job map[string]any
	if err := json.Unmarshal(j.created[0], &job); err != nil {
		t.Fatal(err)
	}
	meta := job["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if meta["name"] != "fix-logbook-7-r1" || labels[LabelManagedBy] != ManagedBy || labels[LabelComponent] != "fix" || labels["team"] != "twr" {
		t.Errorf("metadata %v", meta)
	}
	annotations := meta["annotations"].(map[string]any)
	if annotations["pit-crew/repo"] != "those-who-run/logbook" || annotations["pit-crew/number"] != "7" || annotations["pit-crew/issue"] != "232" {
		t.Errorf("annotations %v", annotations)
	}
	text := string(j.created[0])
	for _, want := range []string{`"FETCH_KIND","value":"fix"`, `"FETCH_NUMBER","value":"7"`, `"FETCH_ROUND","value":"1"`, `"FETCH_ISSUE","value":"232"`, `"JOB","value":"fix-logbook-7-r1"`} {
		if !strings.Contains(text, want) {
			t.Errorf("job lacks %s: %s", want, text)
		}
	}
}

func TestRunStartsTheNextUnitWhenTheFirstExists(t *testing.T) {
	j := &jobs{list: []kube.Job{{Name: "fix-logbook-7-r1", Active: false, CreatedAt: at(-30), Labels: map[string]string{LabelComponent: "fix"}}}}
	p := newPoller(t, world(), j, Limits{StartJobs: true, TemplateDir: templates(t, "review", "fix", "build"), MaxActive: 1})
	hb, err := p.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if hb.Started != "review-ecu-31-1f0c5d2e-f0" || !reflect.DeepEqual(hb.Skipped, []string{"fix-logbook-7-r1"}) {
		t.Errorf("started %q, skipped %v", hb.Started, hb.Skipped)
	}
}

// heldWorld is one builder pull request whose review was held at its head two
// hours ago; its Job is still there, as a finished Job is for its TTL.
func heldWorld() (*source, *jobs) {
	ecu := "those-who-run/ecu"
	sha5 := "5555555555555555555555555555555555555555"
	pr := pull(ecu, 28, builder, sha5)
	pr.Labels = []string{"parity", "re-review"}
	src := &source{
		repos:    []string{ecu},
		pulls:    map[string][]github.PullRequest{ecu: {pr}},
		commits:  map[string][]github.Commit{key(ecu, 28): {{SHA: sha5, Author: builder, Committer: builder}}},
		statuses: map[string][]github.Status{sha5: {{Context: ladder.StatusVerdict, State: "error", Description: "held: verdict-missing", Creator: reviewer, CreatedAt: at(-120)}}},
		checks:   map[string]map[string]string{sha5: {"test": "success", "go / check": "success"}},
		required: map[string][]string{ecu: {"test", "go / check"}},
		timeline: map[string][]github.TimelineEvent{key(ecu, 28): {
			{Event: "labeled", Actor: "someone-else", Label: "parity", At: at(-200)},
			{Event: "labeled", Actor: owner, Label: "re-review", At: at(-30)},
		}},
	}
	j := &jobs{list: []kube.Job{{Name: "review-ecu-28-55555555-f0", CreatedAt: at(-125), FinishedAt: at(-110), Labels: map[string]string{LabelComponent: "review"}}}}
	return src, j
}

func TestTheMaintainersReReviewLabelStartsAReviewAgain(t *testing.T) {
	src, j := heldWorld()
	p := newPoller(t, src, j, Limits{StartJobs: true, TemplateDir: templates(t, "review"), MaxActive: 1})
	hb, err := p.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if hb.Reasons["re-review"] != 1 || hb.Decisions["review"] != 1 {
		t.Errorf("heartbeat %+v", hb)
	}
	// The held review's Job is still there under the plain name, so the ask has
	// a name of its own; the same ask on the next poll finds it and starts nothing.
	if hb.Started != "review-ecu-28-55555555-f0-a1" || len(j.created) != 1 || len(hb.Skipped) != 0 {
		t.Fatalf("started %q, created %d, skipped %v", hb.Started, len(j.created), hb.Skipped)
	}
	j.list = append(j.list, kube.Job{Name: hb.Started, Active: false, CreatedAt: at(0), Labels: map[string]string{LabelComponent: "review"}})
	hb, _ = p.Run(t.Context())
	if hb.Started != "" || !reflect.DeepEqual(hb.Skipped, []string{"review-ecu-28-55555555-f0-a1"}) {
		t.Errorf("second poll started %q, skipped %v", hb.Started, hb.Skipped)
	}
}

func TestAStrangersReReviewLabelChangesNothing(t *testing.T) {
	for name, change := range map[string]func(*source){
		"a stranger applied it": func(s *source) {
			s.timeline[key("those-who-run/ecu", 28)][1].Actor = "someone-else"
		},
		"the maintainer applied it before the hold": func(s *source) {
			s.timeline[key("those-who-run/ecu", 28)][1].At = at(-150)
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, j := heldWorld()
			change(src)
			p := newPoller(t, src, j, Limits{StartJobs: true, TemplateDir: templates(t, "review"), MaxActive: 1})
			hb, err := p.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if hb.Reasons["verdict-held"] != 1 || hb.Started != "" || len(j.created) != 0 {
				t.Errorf("heartbeat %+v", hb)
			}
		})
	}
}

func TestAKindWithoutATemplateIsNeverStarted(t *testing.T) {
	j := &jobs{}
	p := newPoller(t, world(), j, Limits{StartJobs: true, TemplateDir: templates(t, "build"), MaxActive: 1})
	hb, _ := p.Run(t.Context())
	if hb.Started != "build-ecu-232" || len(j.created) != 1 {
		t.Errorf("started %q", hb.Started)
	}
	p.Limits.TemplateDir = ""
	hb, _ = p.Run(t.Context())
	if hb.Started != "" || hb.Gate != "" {
		t.Errorf("with no templates: started %q, gate %q", hb.Started, hb.Gate)
	}
}

func TestGates(t *testing.T) {
	active := kube.Job{Name: "review-ecu-1-abcdef01-f0", Active: true, CreatedAt: at(-10), Labels: map[string]string{LabelComponent: "review"}}
	done := func(name string, minutesAgo int) kube.Job {
		return kube.Job{Name: name, CreatedAt: at(-minutesAgo - 20), FinishedAt: at(-minutesAgo), Labels: map[string]string{LabelComponent: "review"}}
	}
	failed := func(name string, minutesAgo int) kube.Job {
		j := done(name, minutesAgo)
		j.Failed = true
		return j
	}
	cases := []struct {
		name   string
		limits Limits
		jobs   *jobs
		want   string
		check  func(t *testing.T, hb Heartbeat)
	}{
		{"dry run", Limits{}, &jobs{}, "dry-run", nil},
		{"quiet window", Limits{StartJobs: true, Quiet: []Window{{Start: 8 * 60, End: 10 * 60}}}, &jobs{}, "quiet", nil},
		{"quiet window across midnight, outside it", Limits{StartJobs: true, Quiet: []Window{{Start: 22 * 60, End: 7 * 60}}}, &jobs{}, "", nil},
		// 09:00 UTC is 10:00 an hour east, past the window's end.
		{"quiet window in another zone", Limits{StartJobs: true, Quiet: []Window{{Start: 9 * 60, End: 10 * 60}},
			Location: time.FixedZone("east", 3600)}, &jobs{}, "", nil},
		{"quiet window in another zone, inside it", Limits{StartJobs: true, Quiet: []Window{{Start: 9 * 60, End: 10 * 60}},
			Location: time.FixedZone("half", 1800)}, &jobs{}, "quiet", nil},
		{"a Job is active", Limits{StartJobs: true, MaxActive: 1}, &jobs{list: []kube.Job{active}}, "active",
			func(t *testing.T, hb Heartbeat) {
				if hb.Active != 1 {
					t.Errorf("active %d", hb.Active)
				}
			}},
		{"two may be active", Limits{StartJobs: true, MaxActive: 2}, &jobs{list: []kube.Job{active}}, "", nil},
		{"the daily cap", Limits{StartJobs: true, DailyCap: 2}, &jobs{list: []kube.Job{done("a", 60), done("b", 120), done("c", 25*60)}}, "cap",
			func(t *testing.T, hb Heartbeat) {
				if hb.Today != 2 {
					t.Errorf("today %d", hb.Today)
				}
			}},
		{"the plan limit was reached an hour ago", Limits{StartJobs: true, BackoffPlanLimit: 3 * time.Hour},
			&jobs{list: []kube.Job{failed("x", 60)}, exits: map[string]int{"x": ExitPlanLimit}}, "backoff",
			func(t *testing.T, hb Heartbeat) {
				if hb.PlanLimitUntil != at(120).Format(time.RFC3339) {
					t.Errorf("plan limit until %q", hb.PlanLimitUntil)
				}
			}},
		{"the plan limit was reached four hours ago", Limits{StartJobs: true, BackoffPlanLimit: 3 * time.Hour},
			&jobs{list: []kube.Job{failed("x", 240)}, exits: map[string]int{"x": ExitPlanLimit}}, "", nil},
		{"the login expired", Limits{StartJobs: true, BackoffPlanLimit: 3 * time.Hour, BackoffLoginExpired: 6 * time.Hour},
			&jobs{list: []kube.Job{failed("x", 200)}, exits: map[string]int{"x": ExitLoginExpired}}, "backoff",
			func(t *testing.T, hb Heartbeat) {
				if hb.LoginExpiredUntil == "" || hb.PlanLimitUntil != "" {
					t.Errorf("heartbeat %+v", hb)
				}
			}},
		{"a Job that failed for another reason", Limits{StartJobs: true, BackoffPlanLimit: 3 * time.Hour},
			&jobs{list: []kube.Job{failed("x", 60)}, exits: map[string]int{"x": 1}}, "", nil},
		{"a failed Job whose pod is gone", Limits{StartJobs: true, BackoffPlanLimit: 3 * time.Hour}, &jobs{list: []kube.Job{failed("x", 60)}}, "", nil},
		{"the Jobs cannot be listed", Limits{StartJobs: true}, &jobs{refuse: errors.New("forbidden")}, "jobs-unreadable",
			func(t *testing.T, hb Heartbeat) {
				if hb.Errors != 1 {
					t.Errorf("errors %d", hb.Errors)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.limits.TemplateDir = templates(t, "review", "fix", "build")
			p := newPoller(t, world(), c.jobs, c.limits)
			hb, err := p.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if hb.Gate != c.want {
				t.Errorf("gate %q, want %q", hb.Gate, c.want)
			}
			if (hb.Started != "") != (c.want == "") {
				t.Errorf("gate %q and started %q", hb.Gate, hb.Started)
			}
			if c.check != nil {
				c.check(t, hb)
			}
		})
	}
}

func TestIdleGate(t *testing.T) {
	status := http.StatusOK
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer gate.Close()
	run := func(limits Limits) Heartbeat {
		limits.StartJobs, limits.TemplateDir = true, templates(t, "review", "fix", "build")
		hb, err := newPoller(t, world(), &jobs{}, limits).Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return hb
	}
	if hb := run(Limits{IdleGateURL: gate.URL}); hb.Gate != "" || hb.Started == "" {
		t.Errorf("an idle gate stopped a start: %+v", hb)
	}
	status = http.StatusServiceUnavailable
	if hb := run(Limits{IdleGateURL: gate.URL}); hb.Gate != "idle" || hb.Started != "" {
		t.Errorf("a live session did not stop a start: %+v", hb)
	}
	// With a field named, a 200 alone is not idle: ecu's /healthz answers 200
	// during a race too, and says so in its body.
	ecuHealth := `{"idle": %s, "idle_secs": %s, "activity": %q, "telemetry_connected": true, "telemetry_listening": true,
	  "last_packet_age_secs": 0.2, "packets": 10, "races_started": 1, "tasks": {"drive": true}, "nats_connected": true, "ok": true}`
	body := fmt.Sprintf(ecuHealth, "true", "612.5", "idle")
	field := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	defer field.Close()
	if hb := run(Limits{IdleGateURL: field.URL, IdleGateField: "idle"}); hb.Gate != "" || hb.Started == "" {
		t.Errorf("an idle ecu stopped a start: %+v", hb)
	}
	for name, text := range map[string]string{
		"driving":          fmt.Sprintf(ecuHealth, "false", "0", "driving"),
		"a missing field":  `{"ok": true}`,
		"a string":         `{"idle": "true"}`,
		"a number":         `{"idle": 1}`,
		"null":             `{"idle": null}`,
		"not an object":    `[true]`,
		"not JSON":         `idle`,
		"an empty body":    ``,
		"the field nested": `{"status": {"idle": true}}`,
	} {
		body = text
		if hb := run(Limits{IdleGateURL: field.URL, IdleGateField: "idle"}); hb.Gate != "idle" || hb.Started != "" || hb.Errors != 0 {
			t.Errorf("%s did not close the gate: %+v", name, hb)
		}
	}
	gate.Close()
	if hb := run(Limits{IdleGateURL: gate.URL}); hb.Gate != "idle" || hb.Errors != 1 {
		t.Errorf("an unreachable gate did not close: %+v", hb)
	}
	if hb := run(Limits{IdleGateCmd: []string{"true"}}); hb.Gate != "" {
		t.Errorf("a command gate that exits 0: %+v", hb)
	}
	if hb := run(Limits{IdleGateCmd: []string{"false"}}); hb.Gate != "idle" || hb.Errors != 0 {
		t.Errorf("a command gate that exits 1: %+v", hb)
	}
	if hb := run(Limits{IdleGateCmd: []string{filepath.Join(t.TempDir(), "missing")}}); hb.Gate != "idle" || hb.Errors != 1 {
		t.Errorf("a command gate that is missing: %+v", hb)
	}
}

func TestHeartbeatIsSentAndWritten(t *testing.T) {
	var got string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.Header.Get("Content-Type") + " " + string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	file := filepath.Join(t.TempDir(), "heartbeat.json")
	p := newPoller(t, world(), &jobs{}, Limits{HeartbeatURL: sink.URL, HeartbeatFile: file})
	if _, err := p.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "POST application/json {\"at\":\"2026-11-06T09:00:00Z\"") || !strings.Contains(got, `"gate":"dry-run"`) {
		t.Errorf("sent %q", got)
	}
	data, err := os.ReadFile(file) //nolint:gosec // a file in the test's temp dir
	if err != nil || !strings.Contains(string(data), `"pulls":4`) {
		t.Errorf("file %q, %v", data, err)
	}
	// A sink that is gone or unhappy never fails the run.
	sink.Close()
	p.Limits.HeartbeatFile = filepath.Join(t.TempDir(), "no", "such", "dir")
	if _, err := p.Run(t.Context()); err != nil {
		t.Errorf("run failed on the heartbeat: %v", err)
	}
}

// GitHub answering 5xx, or timing out, after the client's retries is GitHub's
// outage: the run starts nothing, says so in the heartbeat and returns no
// error, so the CronJob's Job does not fail. A 4xx is the poller's to fix, and
// still fails the run.
func TestAGitHubOutageIsNotAFailedRun(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"504 on the first call", &github.Error{Method: "GET", Path: "/installation/repositories", Status: 504, Message: "Gateway Timeout"}},
		{"502, wrapped", fmt.Errorf("listing: %w", &github.Error{Status: 502})},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := world()
			src.reposErr = c.err
			j := &jobs{}
			file := filepath.Join(t.TempDir(), "heartbeat.json")
			p := newPoller(t, src, j, Limits{StartJobs: true, TemplateDir: templates(t, "review", "fix", "build"), MaxActive: 1, DailyCap: 12, HeartbeatFile: file})
			hb, err := p.Run(t.Context())
			if err != nil || !hb.GitHubUnavailable || hb.Errors != 1 || len(j.created) != 0 {
				t.Errorf("heartbeat %+v, err %v, created %d", hb, err, len(j.created))
			}
			data, _ := os.ReadFile(file) //nolint:gosec // a file in the test's temp dir
			if !strings.Contains(string(data), `"github_unavailable":true`) || !strings.Contains(string(data), `"errors":1`) || !strings.Contains(string(data), `"at":"2026-11-06T09:00:00Z"`) {
				t.Errorf("heartbeat file %s", data)
			}
		})
	}
	for _, status := range []int{401, 403, 404} {
		src := world()
		src.reposErr = &github.Error{Method: "GET", Path: "/installation/repositories", Status: status}
		file := filepath.Join(t.TempDir(), "heartbeat.json")
		if _, err := newPoller(t, src, &jobs{}, Limits{HeartbeatFile: file}).Run(t.Context()); !github.IsStatus(err, status) {
			t.Errorf("%d: err %v, want it returned", status, err)
		}
	}
	// A run that read GitHub says it was available.
	file := filepath.Join(t.TempDir(), "heartbeat.json")
	if hb, err := newPoller(t, world(), &jobs{}, Limits{HeartbeatFile: file}).Run(t.Context()); err != nil || hb.GitHubUnavailable {
		t.Errorf("heartbeat %+v, %v", hb, err)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), `"github_unavailable":false`) { //nolint:gosec // a file in the test's temp dir
		t.Errorf("heartbeat file %s", data)
	}
}

func TestABrokenRepositoryIsCountedAndSkipped(t *testing.T) {
	src := world()
	src.broken = "those-who-run/ecu"
	hb, err := newPoller(t, src, &jobs{}, Limits{}).Run(t.Context())
	if err != nil || hb.Errors != 1 || hb.Pulls != 1 {
		t.Errorf("heartbeat %+v, %v", hb, err)
	}
}

func TestParseWindows(t *testing.T) {
	got, err := ParseWindows(" 22:00-07:00, 12:30-13:00 ,")
	if err != nil || !reflect.DeepEqual(got, []Window{{22 * 60, 7 * 60}, {12*60 + 30, 13 * 60}}) {
		t.Errorf("%v %v", got, err)
	}
	for _, bad := range []string{"22-07", "25:00-07:00", "22:00-07:60", "x"} {
		if _, err := ParseWindows(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if got, err := ParseWindows(""); err != nil || got != nil {
		t.Errorf("empty: %v %v", got, err)
	}
	w := Window{Start: 22 * 60, End: 7 * 60}
	for hour, want := range map[int]bool{23: true, 3: true, 7: false, 12: false, 22: true} {
		if got := w.Contains(time.Date(2026, 1, 1, hour, 0, 0, 0, time.UTC)); got != want {
			t.Errorf("%d:00 in 22:00-07:00 = %v", hour, got)
		}
	}
}

func TestRenderJob(t *testing.T) {
	t.Run("an unknown placeholder is an error", func(t *testing.T) {
		_, err := RenderJob([]byte(`{"spec": {"x": "{{nope}}"}}`), "n", "review", map[string]string{})
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("not JSON", func(t *testing.T) {
		if _, err := RenderJob([]byte(`{`), "n", "review", nil); err == nil {
			t.Error("no error")
		}
	})
	t.Run("placeholders in arrays and nested maps, numbers untouched", func(t *testing.T) {
		out, err := RenderJob([]byte(`{"spec": {"args": ["{{repo}}", 7, true, {"k": "{{kind}}-{{job_name}}"}]}}`), "job-1", "build", map[string]string{"repo": "a/b"})
		if err != nil {
			t.Fatal(err)
		}
		var job map[string]any
		_ = json.Unmarshal(out, &job)
		args := job["spec"].(map[string]any)["args"].([]any)
		if args[0] != "a/b" || args[1] != float64(7) || args[2] != true || args[3].(map[string]any)["k"] != "build-job-1" {
			t.Errorf("args %v", args)
		}
		if job["metadata"].(map[string]any)["name"] != "job-1" {
			t.Errorf("metadata %v", job["metadata"])
		}
	})
}

func TestRepoSlug(t *testing.T) {
	for repo, want := range map[string]string{"those-who-run/.github": "github", "those-who-run/ECU": "ecu", "x/---": "repo", "x/shakedown-fangio": "shakedown-fangio"} {
		if got := repoSlug(repo); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", repo, got, want)
		}
	}
}

func TestBuildsAreOffWithoutTheirConfiguration(t *testing.T) {
	p := newPoller(t, world(), &jobs{}, Limits{})
	p.Policies.Build.Maintainer = ""
	hb, _ := p.Run(t.Context())
	if hb.Issues != 0 {
		t.Errorf("issues %d with no maintainer", hb.Issues)
	}
}

// statuses records every status set.
type statuses struct {
	set []string
}

func (s *statuses) SetStatus(_ context.Context, repo, sha, context, state, description string) error {
	s.set = append(s.set, fmt.Sprintf("%s@%s %s=%s (%s)", repo, sha[:4], context, state, description))
	return nil
}

func TestReady(t *testing.T) {
	src := world()
	// logbook#7 has used its rounds: two answers and a third changes verdict.
	sha4 := "4444444444444444444444444444444444444444"
	fix := func(round, minutes int) github.Comment {
		return github.Comment{ID: int64(100 + minutes), Author: builder, Body: "Answer\n\n" + ladder.FixMarker(round), CreatedAt: at(minutes), UpdatedAt: at(minutes)}
	}
	src.comments[key("those-who-run/logbook", 7)] = []github.Comment{
		reviewComment(sha4, ladder.VerdictChanges, 1), fix(1, 2), reviewComment(sha4, ladder.VerdictChanges, 3), fix(2, 4), reviewComment(sha4, ladder.VerdictChanges, 5),
	}
	// ecu#31 was marked ready at this head by an earlier pass, and the reviewer has
	// not reviewed it since: the light must go off.
	src.statuses[head] = []github.Status{{Context: ladder.StatusReady, State: "success", Description: "x", Creator: reviewer, CreatedAt: at(0)}}
	w := &statuses{}
	list, err := Ready(t.Context(), src, w, policies(), logger())
	if err != nil {
		t.Fatal(err)
	}
	want := []ReadyEntry{{Repo: "those-who-run/ecu", Number: 30, Title: "feat: those-who-run/ecu#30", Author: owner, Verdict: ladder.VerdictReady, URL: "https://github.com/those-who-run/ecu/pull/30"}}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("list %+v", list)
	}
	wantSet := []string{
		"those-who-run/ecu@1f0c ladder / ready=pending (no longer ready: no-review-at-head)",
		"those-who-run/ecu@2222 ladder / ready=success (verdict ready, every required check green)",
		"those-who-run/logbook@4444 ladder / stuck=failure (2 fix rounds used; over to the maintainer)",
	}
	if !reflect.DeepEqual(w.set, wantSet) {
		t.Errorf("set %q\nwant %q", w.set, wantSet)
	}
	// A second pass with the statuses in place writes nothing.
	src.statuses[head] = nil
	src.statuses["2222222222222222222222222222222222222222"] = []github.Status{{Context: ladder.StatusReady, State: "success", Description: "verdict ready, every required check green", Creator: reviewer}}
	src.statuses[sha4] = []github.Status{{Context: ladder.StatusStuck, State: "failure", Description: "2 fix rounds used; over to the maintainer", Creator: reviewer}}
	w.set = nil
	if _, err := Ready(t.Context(), src, w, policies(), logger()); err != nil || len(w.set) != 0 {
		t.Errorf("second pass set %q, %v", w.set, err)
	}
	// A stranger's green light is not the reviewer's: it is left alone, and so
	// is a stranger's pull request.
	src.statuses["3333333333333333333333333333333333333333"] = []github.Status{{Context: ladder.StatusReady, State: "success", Creator: "someone-else"}}
	if _, err := Ready(t.Context(), src, w, policies(), logger()); err != nil || len(w.set) != 0 {
		t.Errorf("third pass set %q, %v", w.set, err)
	}
	file := filepath.Join(t.TempDir(), "ready.md")
	if err := WriteReadyList(file, list); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file) //nolint:gosec // a file in the test's temp dir
	if !strings.Contains(string(data), "- [those-who-run/ecu#30](https://github.com/those-who-run/ecu/pull/30): feat: those-who-run/ecu#30 (octo-maintainer, verdict ready)") {
		t.Errorf("list file %q", data)
	}
	if err := WriteReadyList(file, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "Nothing is ready.") { //nolint:gosec // the same file
		t.Errorf("empty list file %q", data)
	}
}

func TestPoliciesFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	p, err := PoliciesFromEnv(env(map[string]string{
		"LADDER_MAINTAINER": owner, "LADDER_AUTHORS": builder + ", " + owner, "LADDER_COMMITTERS": "web-flow",
		"LADDER_BUILD_REPOS": "those-who-run/ecu", "LADDER_COMPONENTS": "component: org=, component: deploy=those-who-run/paddock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Pull.Reviewer != reviewer || p.Pull.Builder != builder || !reflect.DeepEqual(p.Pull.Authors, []string{builder, owner}) ||
		!reflect.DeepEqual(p.Pull.Committers, []string{"web-flow"}) || p.Pull.HoldLabel != "hold" || p.IssueRepo != "those-who-run/paddock" ||
		p.Pull.Maintainer != owner || p.Pull.ReReviewLabel != "re-review" {
		t.Errorf("pull %+v, issue repo %s", p.Pull, p.IssueRepo)
	}
	if p.Build.Maintainer != owner || p.Build.ReadyLabel != "agent-ready" || p.Build.TriageLabel != "needs triage" || p.Build.Owner != "those-who-run" ||
		!reflect.DeepEqual(p.Build.Components, map[string]string{"component: org": "", "component: deploy": "those-who-run/paddock"}) {
		t.Errorf("build %+v", p.Build)
	}
	empty, err := PoliciesFromEnv(env(nil))
	if err != nil || len(empty.Pull.Authors) != 0 || empty.Build.Maintainer != "" {
		t.Errorf("empty %+v %v", empty, err)
	}
	if _, err := PoliciesFromEnv(env(map[string]string{"LADDER_COMPONENTS": "nonsense"})); err == nil {
		t.Error("a bad component mapping")
	}
}

func TestLimitsFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	l, err := LimitsFromEnv(env(map[string]string{"POLL_START_JOBS": "true", "POLL_MAX_ACTIVE": "2", "POLL_DAILY_CAP": "20", "POLL_QUIET": "22:00-07:00",
		"POLL_TZ": "Europe/Dublin", "POLL_BACKOFF_PLAN_LIMIT": "1h", "POLL_IDLE_GATE_CMD": "check-idle --quiet", "POLL_IDLE_GATE_FIELD": "idle"}))
	if err != nil || !l.StartJobs || l.MaxActive != 2 || l.DailyCap != 20 || len(l.Quiet) != 1 || l.Location.String() != "Europe/Dublin" || l.IdleGateField != "idle" ||
		l.BackoffPlanLimit != time.Hour || l.BackoffLoginExpired != 6*time.Hour || !reflect.DeepEqual(l.IdleGateCmd, []string{"check-idle", "--quiet"}) {
		t.Errorf("%+v %v", l, err)
	}
	d, err := LimitsFromEnv(env(nil))
	if err != nil || d.StartJobs || d.MaxActive != 1 || d.DailyCap != 12 || d.Location != time.UTC {
		t.Errorf("defaults %+v %v", d, err)
	}
	for _, bad := range []map[string]string{{"POLL_MAX_ACTIVE": "x"}, {"POLL_BACKOFF_PLAN_LIMIT": "soon"}, {"POLL_QUIET": "x"}, {"POLL_TZ": "Mars/Olympus"}} {
		if _, err := LimitsFromEnv(env(bad)); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

// The example template in examples/ is what the manifests start from: it must
// render, and name the containers the contract names.
func TestTheExampleTemplateRenders(t *testing.T) {
	template, err := os.ReadFile(filepath.Join("..", "..", "examples", "review.json"))
	if err != nil {
		t.Fatal(err)
	}
	u := reviewUnit(Candidate{Pull: pull("those-who-run/ecu", 31, builder, head)})
	out, err := RenderJob(template, u.name, u.kind, u.values)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{`"name":"review-ecu-31-1f0c5d2e-f0"`, `"name":"fetch"`, `"name":"` + AgentContainer + `"`, `"name":"publish"`,
		`"FETCH_HEAD_SHA","value":"` + head + `"`, `"automountServiceAccountToken":false`} {
		if !strings.Contains(text, want) {
			t.Errorf("the rendered example lacks %s", want)
		}
	}
	if strings.Contains(text, "{{") {
		t.Errorf("a placeholder is left: %s", text)
	}
}
