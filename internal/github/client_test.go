package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is a GitHub API at the HTTP boundary: routes to canned JSON, a record of
// every request, and knobs for the failure modes the client must survive.
type fake struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	// requests records "METHOD path?query" and the bodies of writes.
	requests []string
	bodies   []string
	// failFirst makes the first n requests answer with status, or, with status 0,
	// never reach a handler (the test's transport drops them).
	failFirst  int
	failStatus int
	failHeader map[string]string
}

func newFake(t *testing.T) (*fake, *Client) {
	t.Helper()
	f := &fake{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	var sleeps []time.Duration
	client := &Client{
		BaseURL: server.URL,
		Token:   "ghs_test",
		HTTP:    &http.Client{Transport: &dropping{f: f, next: server.Client().Transport}},
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	}
	t.Cleanup(func() { f.t.Logf("sleeps: %v", sleeps) })
	return f, client
}

// dropping is the transport of a pod whose network is not up yet: the first
// requests fail with a connection error.
type dropping struct {
	f    *fake
	next http.RoundTripper
}

func (d *dropping) RoundTrip(r *http.Request) (*http.Response, error) {
	d.f.mu.Lock()
	drop := d.f.failFirst > 0 && d.f.failStatus == 0
	if drop {
		d.f.failFirst--
	}
	d.f.mu.Unlock()
	if drop {
		return nil, &net.OpError{Op: "dial", Err: errors.New("connect: network is unreachable")}
	}
	return d.next.RoundTrip(r)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if len(body) > 0 {
		f.bodies = append(f.bodies, string(body))
	}
	fail := f.failFirst > 0 && f.failStatus != 0
	if fail {
		f.failFirst--
	}
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer ghs_test" || r.Header.Get("Accept") != "application/vnd.github+json" {
		f.t.Errorf("headers %v", r.Header)
	}
	if fail {
		for k, v := range f.failHeader {
			w.Header().Set(k, v)
		}
		w.WriteHeader(f.failStatus)
		_, _ = fmt.Fprint(w, `{"message": "no"}`)
		return
	}
	handler, ok := f.routes[r.Method+" "+r.URL.Path]
	if !ok {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message": "Not Found"}`)
		return
	}
	handler(w, r)
}

func (f *fake) file(method, path, name string) {
	f.routes[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // a fixture named by the test
		if err != nil {
			f.t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
}

func (f *fake) json(method, path, body string) {
	f.routes[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}
}

func TestListOpenPulls(t *testing.T) {
	f, c := newFake(t)
	f.file("GET", "/repos/those-who-run/ecu/pulls", "pulls.json")
	pulls, err := c.ListOpenPulls(t.Context(), "those-who-run/ecu")
	if err != nil {
		t.Fatal(err)
	}
	want := []PullRequest{
		{Repo: "those-who-run/ecu", Number: 31, Title: "feat(race): box-this-lap callout", Body: "## What changed\n\nCloses those-who-run/paddock#232.\n",
			Author: "twr-builder[bot]", HeadRepo: "those-who-run/ecu", HeadRef: "agent/232-fuel-callouts", HeadSHA: "1f0c5d2e9a7b4c3d8e6f1a2b3c4d5e6f7a8b9c0d",
			BaseRef: "main", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Labels: []string{"parity"},
			URL: "https://github.com/those-who-run/ecu/pull/31", State: "open", CreatedAt: time.Date(2026, 11, 6, 9, 10, 0, 0, time.UTC)},
		{Repo: "those-who-run/ecu", Number: 30, Title: "docs: tidy", Author: "someone-else", HeadRef: "patch-1",
			HeadSHA: "0000000000000000000000000000000000000001", BaseRef: "main", BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Draft: true, Labels: []string{}, URL: "https://github.com/those-who-run/ecu/pull/30", State: "open",
			CreatedAt: time.Date(2026, 11, 5, 9, 10, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(pulls, want) {
		t.Errorf("got %+v\nwant %+v", pulls, want)
	}
	if got := f.requests; !reflect.DeepEqual(got, []string{"GET /repos/those-who-run/ecu/pulls?state=open&per_page=100"}) {
		t.Errorf("requests %v", got)
	}
}

func TestPagination(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /repos/those-who-run/ecu/issues/31/comments"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprint(w, `[{"id": 2, "user": {"login": "twr-reviewer[bot]"}, "body": "second", "created_at": "2026-11-06T09:00:00Z", "updated_at": "2026-11-06T09:30:00Z"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/those-who-run/ecu/issues/31/comments?per_page=100&page=2>; rel="next", <x>; rel="last"`, r.Host))
		_, _ = fmt.Fprint(w, `[{"id": 1, "user": null, "body": "first", "created_at": "2026-11-06T08:00:00Z", "updated_at": "2026-11-06T08:00:00Z"}]`)
	}
	comments, err := c.ListComments(t.Context(), "those-who-run/ecu", 31)
	if err != nil {
		t.Fatal(err)
	}
	want := []Comment{
		{ID: 1, Body: "first", CreatedAt: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)},
		{ID: 2, Author: "twr-reviewer[bot]", Body: "second", CreatedAt: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(comments, want) {
		t.Errorf("got %+v, want %+v", comments, want)
	}
	if len(f.requests) != 2 {
		t.Errorf("requests %v", f.requests)
	}
}

func TestRetriesWhileTheNetworkComesUp(t *testing.T) {
	f, c := newFake(t)
	f.failFirst = 3
	f.json("GET", "/rate_limit", `{"resources": {"core": {"limit": 5000}}}`)
	if err := c.WaitForNetwork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 1 {
		t.Errorf("the server saw %v", f.requests)
	}
	f.failFirst = 10
	c.Retries = 2
	if err := c.WaitForNetwork(t.Context()); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error %v, want the transport's after the retries", err)
	}
}

func TestAWriteIsNotRetriedOnATransportError(t *testing.T) {
	f, c := newFake(t)
	f.failFirst = 1
	f.json("POST", "/repos/those-who-run/ecu/issues/31/comments", `{"id": 9}`)
	if _, err := c.CreateComment(t.Context(), "those-who-run/ecu", 31, "x"); err == nil {
		t.Error("the comment was retried, and might have been posted twice")
	}
	if len(f.requests) != 0 {
		t.Errorf("the server saw %v", f.requests)
	}
}

func TestRateLimitsAreWaitedOut(t *testing.T) {
	f, c := newFake(t)
	var waited []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	}
	f.failFirst, f.failStatus = 1, http.StatusForbidden
	f.failHeader = map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(time.Now().Add(30 * time.Second).Unix())}
	f.json("POST", "/repos/those-who-run/ecu/statuses/abc", `{}`)
	if err := c.SetStatus(t.Context(), "those-who-run/ecu", "abc", "review / verdict", "success", "ready"); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] < 25*time.Second || waited[0] > 35*time.Second {
		t.Errorf("waited %v, want about 31 s", waited)
	}
	// A secondary limit says Retry-After.
	waited = nil
	f.failFirst, f.failStatus = 1, http.StatusTooManyRequests
	f.failHeader = map[string]string{"Retry-After": "7"}
	if err := c.SetStatus(t.Context(), "those-who-run/ecu", "abc", "review / verdict", "success", "ready"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waited, []time.Duration{7 * time.Second}) {
		t.Errorf("waited %v", waited)
	}
	// A 403 that is not a rate limit is an answer.
	f.failFirst, f.failStatus, f.failHeader = 1, http.StatusForbidden, nil
	err := c.SetStatus(t.Context(), "those-who-run/ecu", "abc", "review / verdict", "success", "ready")
	if !IsStatus(err, http.StatusForbidden) {
		t.Errorf("error %v, want a 403", err)
	}
}

func TestRateLimitWait(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		header map[string]string
		want   time.Duration
	}{
		{map[string]string{"Retry-After": "12"}, 12 * time.Second},
		{map[string]string{"Retry-After": "9999"}, maxRateLimitWait},
		{map[string]string{"X-RateLimit-Reset": "1000060"}, 61 * time.Second},
		{map[string]string{"X-RateLimit-Reset": "999000"}, 5 * time.Second},
		{map[string]string{}, 5 * time.Second},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.header {
			h.Set(k, v)
		}
		if got := rateLimitWait(h, now); got != c.want {
			t.Errorf("%v: %v, want %v", c.header, got, c.want)
		}
	}
}

func TestAServerErrorIsRetriedForReadsOnly(t *testing.T) {
	f, c := newFake(t)
	f.failFirst, f.failStatus = 2, http.StatusBadGateway
	f.json("GET", "/repos/those-who-run/ecu/pulls/31", `{"number": 31, "user": {"login": "x"}, "head": {"ref": "b", "sha": "s"}, "base": {"ref": "main"}}`)
	pr, err := c.GetPull(t.Context(), "those-who-run/ecu", 31)
	if err != nil || pr.Number != 31 || pr.Author != "x" {
		t.Fatalf("%+v, %v", pr, err)
	}
	c.Retries = 1
	f.failFirst, f.failStatus = 2, http.StatusBadGateway
	if _, err := c.GetPull(t.Context(), "those-who-run/ecu", 31); !IsStatus(err, http.StatusBadGateway) {
		t.Errorf("error %v, want 502 after one retry", err)
	}
	f.failFirst, f.failStatus = 1, http.StatusBadGateway
	f.json("PATCH", "/repos/those-who-run/ecu/issues/comments/9", `{}`)
	if err := c.UpdateComment(t.Context(), "those-who-run/ecu", 9, "x"); !IsStatus(err, http.StatusBadGateway) {
		t.Errorf("error %v, want 502 with no retry", err)
	}
}

// timeout is what net/http returns when a request runs out of time.
type timeout struct{}

func (timeout) Error() string   { return "i/o timeout" }
func (timeout) Timeout() bool   { return true }
func (timeout) Temporary() bool { return true }

// statusErr stands for the App token's error, which carries GitHub's status the
// same way.
type statusErr int

func (s statusErr) Error() string   { return fmt.Sprintf("apptoken: %d", int(s)) }
func (s statusErr) HTTPStatus() int { return int(s) }

func TestUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"504 from GitHub", &Error{Method: "GET", Path: "/rate_limit", Status: 504}, true},
		{"502, wrapped", fmt.Errorf("listing: %w", &Error{Status: 502}), true},
		{"500", &Error{Status: 500}, true},
		{"503 while minting the token", fmt.Errorf("mint: %w", statusErr(503)), true},
		{"a timeout", fmt.Errorf("github: GET /rate_limit: %w", &net.OpError{Op: "read", Err: timeout{}}), true},
		{"401", &Error{Status: 401}, false},
		{"403", &Error{Status: 403}, false},
		{"404", &Error{Status: 404}, false},
		{"422 while minting the token", statusErr(422), false},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}, false},
		{"a configuration error", errors.New("APP_KEY_FILE: no such file"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Unavailable(c.err); got != c.want {
				t.Errorf("Unavailable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// The incident's first call: GET /rate_limit answering 504 on every try. After
// the retries, the error says GitHub was unavailable.
func TestWaitForNetworkOnAGitHubOutage(t *testing.T) {
	f, c := newFake(t)
	f.failFirst, f.failStatus = 100, http.StatusGatewayTimeout
	err := c.WaitForNetwork(t.Context())
	if !IsStatus(err, http.StatusGatewayTimeout) || !Unavailable(err) || len(f.requests) != 6 {
		t.Errorf("error %v after %d requests", err, len(f.requests))
	}
}

func TestSleepHonoursTheContext(t *testing.T) {
	f, c := newFake(t)
	c.Sleep = nil
	f.failFirst = 5
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.WaitForNetwork(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v", err)
	}
}

func TestCommitsChecksAndStatuses(t *testing.T) {
	f, c := newFake(t)
	f.json("GET", "/repos/those-who-run/ecu/pulls/31/commits", `[
	  {"sha": "1f0c", "author": {"login": "twr-builder[bot]"}, "committer": {"login": "twr-builder[bot]"},
	   "commit": {"author": {"name": "twr-builder[bot]", "email": "1+twr-builder[bot]@users.noreply.github.com"},
	              "committer": {"name": "twr-builder[bot]", "email": "1+twr-builder[bot]@users.noreply.github.com"}, "message": "feat: x"}},
	  {"sha": "2a3b", "author": null, "committer": {"login": "web-flow"},
	   "commit": {"author": {"name": "Nobody", "email": "nobody@example.com"}, "committer": {"name": "GitHub", "email": "noreply@github.com"}, "message": "Merge"}}]`)
	commits, err := c.ListPullCommits(t.Context(), "those-who-run/ecu", 31)
	if err != nil {
		t.Fatal(err)
	}
	want := []Commit{
		{SHA: "1f0c", Author: "twr-builder[bot]", Committer: "twr-builder[bot]", AuthorName: "twr-builder[bot]", AuthorEmail: "1+twr-builder[bot]@users.noreply.github.com",
			CommitterName: "twr-builder[bot]", CommitterEmail: "1+twr-builder[bot]@users.noreply.github.com", Message: "feat: x"},
		{SHA: "2a3b", Committer: "web-flow", AuthorName: "Nobody", AuthorEmail: "nobody@example.com", CommitterName: "GitHub", CommitterEmail: "noreply@github.com", Message: "Merge"},
	}
	if !reflect.DeepEqual(commits, want) {
		t.Errorf("got %+v\nwant %+v", commits, want)
	}

	f.json("GET", "/repos/those-who-run/ecu/commits/1f0c/check-runs", `{"total_count": 3, "check_runs": [
	  {"name": "test", "status": "completed", "conclusion": "success"},
	  {"name": "go / check", "status": "in_progress", "conclusion": null},
	  {"name": "test", "status": "completed", "conclusion": "failure"}]}`)
	f.json("GET", "/repos/those-who-run/ecu/commits/1f0c/statuses", `[
	  {"context": "review / verdict", "state": "success", "description": "ready: kind A", "creator": {"login": "twr-reviewer[bot]"}, "created_at": "2026-11-06T09:30:00Z"},
	  {"context": "review / verdict", "state": "failure", "description": "changes", "creator": {"login": "twr-reviewer[bot]"}, "created_at": "2026-11-06T09:00:00Z"},
	  {"context": "test", "state": "pending", "description": "", "creator": null, "created_at": "2026-11-06T08:00:00Z"}]`)
	checks, err := c.ListChecks(t.Context(), "those-who-run/ecu", "1f0c")
	if err != nil {
		t.Fatal(err)
	}
	wantChecks := map[string]string{"test": "success", "go / check": "in_progress", "review / verdict": "success"}
	if !reflect.DeepEqual(checks, wantChecks) {
		t.Errorf("checks %v, want %v", checks, wantChecks)
	}
	statuses, err := c.ListStatuses(t.Context(), "those-who-run/ecu", "1f0c")
	if err != nil || len(statuses) != 3 || statuses[0].Creator != "twr-reviewer[bot]" || statuses[2].Creator != "" {
		t.Errorf("statuses %+v, %v", statuses, err)
	}

	f.json("GET", "/repos/those-who-run/ecu/rules/branches/main", `[
	  {"type": "pull_request", "parameters": {"required_approving_review_count": 1}},
	  {"type": "required_status_checks", "parameters": {"required_status_checks": [{"context": "test"}, {"context": "checks / checks"}]}}]`)
	required, err := c.ListRequiredChecks(t.Context(), "those-who-run/ecu", "main")
	if err != nil || !reflect.DeepEqual(required, []string{"test", "checks / checks"}) {
		t.Errorf("required %v, %v", required, err)
	}
}

func TestWrites(t *testing.T) {
	f, c := newFake(t)
	f.json("POST", "/repos/those-who-run/ecu/issues/31/comments", `{"id": 42}`)
	f.json("PATCH", "/repos/those-who-run/ecu/issues/comments/42", `{"id": 42}`)
	f.json("POST", "/repos/those-who-run/ecu/statuses/1f0c", `{"id": 1}`)
	f.json("POST", "/repos/those-who-run/ecu/pulls", `{"number": 32, "html_url": "https://github.com/those-who-run/ecu/pull/32"}`)
	f.json("PATCH", "/repos/those-who-run/ecu/pulls/32", `{"number": 32}`)
	id, err := c.CreateComment(t.Context(), "those-who-run/ecu", 31, "Review at head")
	if err != nil || id != 42 {
		t.Fatalf("%d, %v", id, err)
	}
	if err := c.UpdateComment(t.Context(), "those-who-run/ecu", 42, "Review again"); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", 150)
	if err := c.SetStatus(t.Context(), "those-who-run/ecu", "1f0c", "review / verdict", "failure", long); err != nil {
		t.Fatal(err)
	}
	number, url, err := c.CreatePull(t.Context(), "those-who-run/ecu", "feat: x", "body", "agent/232-x", "main")
	if err != nil || number != 32 || url == "" {
		t.Fatalf("%d %q %v", number, url, err)
	}
	if err := c.UpdatePullBody(t.Context(), "those-who-run/ecu", 32, "new body"); err != nil {
		t.Fatal(err)
	}
	if f.bodies[4] != `{"body":"new body"}` {
		t.Errorf("update body %q", f.bodies[4])
	}
	var status map[string]string
	if err := json.Unmarshal([]byte(f.bodies[2]), &status); err != nil {
		t.Fatal(err)
	}
	if len([]rune(status["description"])) != 140 || !strings.HasSuffix(status["description"], "…") || status["context"] != "review / verdict" {
		t.Errorf("status body %v", status)
	}
	var pull map[string]any
	_ = json.Unmarshal([]byte(f.bodies[3]), &pull)
	if pull["draft"] != false || pull["head"] != "agent/232-x" || pull["base"] != "main" {
		t.Errorf("pull body %v", pull)
	}
	if f.bodies[0] != `{"body":"Review at head"}` || f.bodies[1] != `{"body":"Review again"}` {
		t.Errorf("bodies %q", f.bodies[:2])
	}
}

func TestRemoveLabel(t *testing.T) {
	f, c := newFake(t)
	// GitHub answers the labels left on the pull request.
	f.json("DELETE", "/repos/those-who-run/ecu/issues/31/labels/re-review", `[{"name": "parity"}]`)
	if err := c.RemoveLabel(t.Context(), "those-who-run/ecu", 31, "re-review"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.requests, []string{"DELETE /repos/those-who-run/ecu/issues/31/labels/re-review"}) {
		t.Errorf("requests %v", f.requests)
	}
	// A label that is not on the pull request answers 404, which is the state
	// wanted; anything else is an answer to pass on.
	f.routes["DELETE /repos/those-who-run/ecu/issues/31/labels/needs triage"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message": "Label does not exist"}`)
	}
	if err := c.RemoveLabel(t.Context(), "those-who-run/ecu", 31, "needs triage"); err != nil {
		t.Errorf("a label already gone: %v", err)
	}
	if f.requests[1] != "DELETE /repos/those-who-run/ecu/issues/31/labels/needs%20triage" {
		t.Errorf("request %q", f.requests[1])
	}
	f.failFirst, f.failStatus = 1, http.StatusForbidden
	if err := c.RemoveLabel(t.Context(), "those-who-run/ecu", 31, "re-review"); !IsStatus(err, http.StatusForbidden) {
		t.Errorf("error %v, want the 403", err)
	}
}

func TestCompare(t *testing.T) {
	f, c := newFake(t)
	f.json("GET", "/repos/those-who-run/ecu/compare/main...agent/232-x", `{"status": "ahead", "ahead_by": 2, "behind_by": 0,
	  "merge_base_commit": {"sha": "aaaa"}, "files": [{"filename": "race/fuel.go", "status": "modified"}, {"filename": "race/fuel_test.go", "status": "added"}]}`)
	cmp, err := c.Compare(t.Context(), "those-who-run/ecu", "main", "agent/232-x")
	if err != nil {
		t.Fatal(err)
	}
	want := Comparison{Status: "ahead", AheadBy: 2, MergeBaseSHA: "aaaa",
		Files: []ChangedFile{{"race/fuel.go", "modified"}, {"race/fuel_test.go", "added"}}}
	if !reflect.DeepEqual(cmp, want) {
		t.Errorf("got %+v, want %+v", cmp, want)
	}
	if !strings.HasPrefix(f.requests[0], "GET /repos/those-who-run/ecu/compare/main...agent%2F232-x") {
		t.Errorf("request %q", f.requests[0])
	}
}

func TestIssuesAndTimeline(t *testing.T) {
	f, c := newFake(t)
	f.json("GET", "/repos/those-who-run/paddock/issues", `[
	  {"number": 232, "title": "Fuel callout", "body": "## Acceptance criteria", "user": {"login": "octo-maintainer"}, "state": "open",
	   "labels": [{"name": "parity"}, {"name": "component: ecu"}, {"name": "agent-ready"}], "html_url": "https://github.com/those-who-run/paddock/issues/232", "created_at": "2026-11-01T00:00:00Z"},
	  {"number": 233, "title": "A pull request in paddock", "body": null, "user": {"login": "octo-maintainer"}, "state": "open", "labels": [],
	   "html_url": "https://github.com/those-who-run/paddock/pull/233", "created_at": "2026-11-01T00:00:00Z", "pull_request": {"merged_at": null}}]`)
	issues, err := c.ListOpenIssues(t.Context(), "those-who-run/paddock", []string{"agent-ready"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Issue{{Repo: "those-who-run/paddock", Number: 232, Title: "Fuel callout", Body: "## Acceptance criteria", Author: "octo-maintainer", State: "open",
		Labels: []string{"parity", "component: ecu", "agent-ready"}, URL: "https://github.com/those-who-run/paddock/issues/232", CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}}
	if !reflect.DeepEqual(issues, want) {
		t.Errorf("got %+v\nwant %+v", issues, want)
	}
	if !strings.Contains(f.requests[0], "labels=agent-ready") || !strings.Contains(f.requests[0], "state=open") {
		t.Errorf("request %q", f.requests[0])
	}
	f.json("GET", "/repos/those-who-run/paddock/issues/232", `{"number": 232, "title": "Fuel callout", "body": null, "user": null, "state": "open", "labels": []}`)
	issue, err := c.GetIssue(t.Context(), "those-who-run/paddock", 232)
	if err != nil || issue.Number != 232 || issue.Body != "" || issue.Author != "" {
		t.Errorf("%+v, %v", issue, err)
	}

	f.file("GET", "/repos/those-who-run/paddock/issues/232/timeline", "timeline.json")
	events, err := c.ListTimeline(t.Context(), "those-who-run/paddock", 232)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatalf("%d events", len(events))
	}
	if e := events[2]; e.Event != "labeled" || e.Actor != "octo-maintainer" || e.Label != "agent-ready" || !e.At.Equal(time.Date(2026, 11, 6, 9, 10, 0, 0, time.UTC)) {
		t.Errorf("label event %+v", e)
	}
	if e := events[4]; e.Event != "cross-referenced" || e.Source == nil || e.Source.Repo != "those-who-run/ecu" || e.Source.Number != 31 ||
		e.Source.Author != "twr-builder[bot]" || e.Source.State != "open" || e.Source.Merged || !strings.Contains(e.Source.Body, "paddock#232") {
		t.Errorf("cross-reference %+v", e.Source)
	}
	if e := events[5]; e.Source != nil {
		t.Errorf("an issue referencing the issue is not a pull request: %+v", e.Source)
	}

	f.json("GET", "/repos/those-who-run/ecu/git/matching-refs/heads/agent/232-", `[{"ref": "refs/heads/agent/232-fuel-callouts"}, {"ref": "refs/heads/agent/232-x"}]`)
	branches, err := c.ListBranches(t.Context(), "those-who-run/ecu", "agent/232-")
	if err != nil || !reflect.DeepEqual(branches, []string{"agent/232-fuel-callouts", "agent/232-x"}) {
		t.Errorf("branches %v, %v", branches, err)
	}
}

func TestInstallationRepos(t *testing.T) {
	f, c := newFake(t)
	f.json("GET", "/installation/repositories", `{"total_count": 3, "repositories": [
	  {"full_name": "those-who-run/ecu", "archived": false}, {"full_name": "those-who-run/old", "archived": true}, {"full_name": "those-who-run/paddock", "archived": false}]}`)
	repos, err := c.ListInstallationRepos(t.Context())
	if err != nil || !reflect.DeepEqual(repos, []string{"those-who-run/ecu", "those-who-run/paddock"}) {
		t.Errorf("repos %v, %v", repos, err)
	}
}

func TestBadRepositoryNames(t *testing.T) {
	_, c := newFake(t)
	for _, repo := range []string{"ecu", "/ecu", "those-who-run/", "a/b/c", "a b/c"} {
		if _, err := c.ListOpenPulls(t.Context(), repo); err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("%q: error %v", repo, err)
		}
	}
}

func TestErrorsNameTheCall(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /repos/those-who-run/ecu/pulls/99"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message": "Not Found", "documentation_url": "x"}`)
	}
	_, err := c.GetPull(t.Context(), "those-who-run/ecu", 99)
	if !IsStatus(err, http.StatusNotFound) || !strings.Contains(err.Error(), "GET /repos/those-who-run/ecu/pulls/99: 404 Not Found") {
		t.Errorf("error %v", err)
	}
	if IsStatus(errors.New("other"), http.StatusNotFound) {
		t.Error("IsStatus matched a plain error")
	}
	f.json("GET", "/repos/those-who-run/ecu/pulls/98", `not json`)
	if _, err := c.GetPull(t.Context(), "those-who-run/ecu", 98); err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error %v", err)
	}
}

func TestDefaults(t *testing.T) {
	c := &Client{}
	if c.base() != DefaultBaseURL || c.httpClient() == nil || c.retries() != 5 {
		t.Errorf("defaults %q %v %d", c.base(), c.httpClient(), c.retries())
	}
	if err := c.sleep(t.Context(), time.Millisecond); err != nil {
		t.Error(err)
	}
	if got := nextLink(`<https://api.github.com/x?page=3>; rel="prev"`); got != "" {
		t.Errorf("next %q", got)
	}
	if got := escapePath("agent/232-a b"); got != "agent/232-a%20b" {
		t.Errorf("escaped %q", got)
	}
}
