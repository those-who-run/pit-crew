package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/kube"
	"github.com/those-who-run/pit-crew/internal/ladder"
)

// Jobs is what the poller does with the cluster. *kube.Client is one.
type Jobs interface {
	ListJobs(ctx context.Context, selector string) ([]kube.Job, error)
	CreateJob(ctx context.Context, job []byte) error
	ContainerExit(ctx context.Context, jobName, container string) (int, bool, error)
}

// The labels and annotations the poller puts on a Job and reads back.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	ManagedBy      = "pit-crew"
	Selector       = LabelManagedBy + "=" + ManagedBy
	// AgentContainer is the name the template must give the model's container, so
	// its exit code can be read.
	AgentContainer = "agent"
)

// The agent step's exit codes the poller acts on, as twr-claude run defines them.
const (
	ExitLoginExpired = 70
	ExitPlanLimit    = 75
)

// Limits are the poller's own brakes.
type Limits struct {
	// StartJobs is false for a dry run: decide, log, create nothing.
	StartJobs bool
	// TemplateDir holds review.json, build.json and fix.json.
	TemplateDir string
	MaxActive   int
	DailyCap    int
	// Quiet are windows in which nothing starts, in Location.
	Quiet    []Window
	Location *time.Location
	// BackoffPlanLimit and BackoffLoginExpired are how long to start nothing after
	// an agent exited with that code.
	BackoffPlanLimit    time.Duration
	BackoffLoginExpired time.Duration
	// IdleGateURL answers 200 when no session is live; IdleGateCmd exits 0 then.
	IdleGateURL string
	// IdleGateField, when set, names a top-level field of the URL's JSON body
	// that must be boolean true as well: a health endpoint answers 200 during a
	// race too, and says `"idle": false`.
	IdleGateField string
	IdleGateCmd   []string
	// HeartbeatURL gets the summary by POST; HeartbeatFile gets it written.
	HeartbeatURL  string
	HeartbeatFile string
}

// Window is a daily quiet window, from Start to End in minutes after midnight; a
// window with End before Start crosses midnight.
type Window struct {
	Start, End int
}

var windowRE = regexp.MustCompile(`^(\d{2}):(\d{2})-(\d{2}):(\d{2})$`)

// ParseWindows reads `HH:MM-HH:MM` windows, comma-separated.
func ParseWindows(text string) ([]Window, error) {
	var windows []Window
	for _, item := range strings.Split(text, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		m := windowRE.FindStringSubmatch(item)
		if m == nil {
			return nil, fmt.Errorf("quiet window %q is not HH:MM-HH:MM", item)
		}
		var n [4]int
		for i := range n {
			n[i], _ = strconv.Atoi(m[i+1])
		}
		if n[0] > 23 || n[2] > 23 || n[1] > 59 || n[3] > 59 {
			return nil, fmt.Errorf("quiet window %q has an hour or minute out of range", item)
		}
		windows = append(windows, Window{Start: n[0]*60 + n[1], End: n[2]*60 + n[3]})
	}
	return windows, nil
}

// Contains reports whether the time of day t falls in the window.
func (w Window) Contains(t time.Time) bool {
	minute := t.Hour()*60 + t.Minute()
	if w.Start <= w.End {
		return minute >= w.Start && minute < w.End
	}
	return minute >= w.Start || minute < w.End
}

// Heartbeat is one run's summary, for the maintainer's alerting.
type Heartbeat struct {
	At        time.Time      `json:"at"`
	Pulls     int            `json:"pulls"`
	Issues    int            `json:"issues"`
	Decisions map[string]int `json:"decisions"`
	Reasons   map[string]int `json:"reasons"`
	// Started is the Job created this run, if any.
	Started string `json:"started"`
	// Gate names what stopped a start: quiet, backoff, cap, active, idle, dry-run.
	Gate   string `json:"gate"`
	Active int    `json:"active"`
	Today  int    `json:"today"`
	// Skipped names units that were due but already have a Job.
	Skipped           []string `json:"skipped,omitempty"`
	PlanLimitUntil    string   `json:"plan_limit_until"`
	LoginExpiredUntil string   `json:"login_expired_until"`
	Errors            int      `json:"errors"`
}

// Poller is one run's dependencies.
type Poller struct {
	Source   Source
	Jobs     Jobs
	Policies Policies
	Limits   Limits
	Logger   *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// HTTP reaches the idle gate and the heartbeat; nil means a short-timeout
	// client.
	HTTP *http.Client
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) httpClient() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// unit is one piece of work the poller could start.
type unit struct {
	kind   string
	name   string
	values map[string]string
}

// Run makes one pass: reads, decides, starts at most one Job, and writes the
// heartbeat. It returns an error only when nothing could be read at all.
func (p *Poller) Run(ctx context.Context) (Heartbeat, error) {
	now := p.now()
	hb := Heartbeat{At: now.UTC(), Decisions: map[string]int{}, Reasons: map[string]int{}}
	pulls, errs, err := Pulls(ctx, p.Source, p.Policies, p.Logger)
	if err != nil {
		return hb, err
	}
	hb.Errors += errs
	issues, errs, err := Issues(ctx, p.Source, p.Policies, p.Logger)
	if err != nil {
		return hb, err
	}
	hb.Errors += errs
	hb.Pulls, hb.Issues = len(pulls), len(issues)
	var fixes, reviews, builds []unit
	for _, c := range pulls {
		hb.Decisions[string(c.Decision.Work)]++
		hb.Reasons[c.Decision.Reason]++
		p.Logger.InfoContext(ctx, "pull", "repo", c.Pull.Repo, "number", c.Pull.Number, "work", string(c.Decision.Work), "reason", c.Decision.Reason)
		switch c.Decision.Work {
		case ladder.WorkReview:
			reviews = append(reviews, reviewUnit(c))
		case ladder.WorkFix:
			fixes = append(fixes, fixUnit(c))
		}
	}
	for _, c := range issues {
		if c.Decision.Build {
			hb.Decisions["build"]++
			builds = append(builds, buildUnit(c))
		}
		hb.Reasons[c.Decision.Reason]++
		p.Logger.InfoContext(ctx, "issue", "repo", c.Issue.Repo, "number", c.Issue.Number, "build", c.Decision.Build, "reason", c.Decision.Reason)
	}
	// A fix continues work the reviewer already read; a review unblocks a merge;
	// a build makes new work. In that order.
	due := append(append(fixes, reviews...), builds...)
	hb.Gate = p.gate(ctx, now, &hb)
	if hb.Gate == "" && len(due) > 0 {
		p.start(ctx, due, &hb)
	}
	p.heartbeat(ctx, hb)
	return hb, nil
}

// gate returns the name of the first brake that is on, or "". It reads the Jobs
// list once for the active count, the daily count and the back-offs.
func (p *Poller) gate(ctx context.Context, now time.Time, hb *Heartbeat) string {
	local := now
	if p.Limits.Location != nil {
		local = now.In(p.Limits.Location)
	}
	for _, w := range p.Limits.Quiet {
		if w.Contains(local) {
			return "quiet"
		}
	}
	jobs, err := p.Jobs.ListJobs(ctx, Selector)
	if err != nil {
		p.Logger.WarnContext(ctx, "list jobs", "err", err.Error())
		hb.Errors++
		return "jobs-unreadable"
	}
	var planUntil, loginUntil time.Time
	for _, job := range jobs {
		if job.Active {
			hb.Active++
		}
		if now.Sub(job.CreatedAt) < 24*time.Hour {
			hb.Today++
		}
		if !job.Failed || job.Labels[LabelComponent] == "" {
			continue
		}
		longest := max(p.Limits.BackoffPlanLimit, p.Limits.BackoffLoginExpired)
		if now.Sub(job.FinishedAt) > longest {
			continue
		}
		code, ok, err := p.Jobs.ContainerExit(ctx, job.Name, AgentContainer)
		if err != nil {
			p.Logger.WarnContext(ctx, "read exit code", "job", job.Name, "err", err.Error())
			hb.Errors++
			continue
		}
		switch {
		case ok && code == ExitPlanLimit:
			planUntil = latest(planUntil, job.FinishedAt.Add(p.Limits.BackoffPlanLimit))
		case ok && code == ExitLoginExpired:
			loginUntil = latest(loginUntil, job.FinishedAt.Add(p.Limits.BackoffLoginExpired))
		}
	}
	if planUntil.After(now) {
		hb.PlanLimitUntil = planUntil.UTC().Format(time.RFC3339)
	}
	if loginUntil.After(now) {
		hb.LoginExpiredUntil = loginUntil.UTC().Format(time.RFC3339)
	}
	switch {
	case hb.PlanLimitUntil != "" || hb.LoginExpiredUntil != "":
		return "backoff"
	case p.Limits.DailyCap > 0 && hb.Today >= p.Limits.DailyCap:
		return "cap"
	case hb.Active >= max(p.Limits.MaxActive, 1):
		return "active"
	case !p.Limits.StartJobs:
		return "dry-run"
	}
	if idle, err := p.idle(ctx); err != nil {
		p.Logger.WarnContext(ctx, "idle gate", "err", err.Error())
		hb.Errors++
		return "idle"
	} else if !idle {
		return "idle"
	}
	return ""
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// idle consults the idle gate. No gate configured means idle; a gate that cannot
// be reached means not idle, since the gate exists to protect a live session.
func (p *Poller) idle(ctx context.Context) (bool, error) {
	if p.Limits.IdleGateURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Limits.IdleGateURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := p.httpClient().Do(req)
		if err != nil {
			return false, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return false, err
		}
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		if p.Limits.IdleGateField != "" && !idleField(body, p.Limits.IdleGateField) {
			return false, nil
		}
	}
	if len(p.Limits.IdleGateCmd) > 0 {
		cmd := exec.CommandContext(ctx, p.Limits.IdleGateCmd[0], p.Limits.IdleGateCmd[1:]...) //nolint:gosec // the command is the operator's configuration
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// idleField reports whether the JSON object in body has field set to boolean
// true. Anything else, a missing field, another type, text that is not an
// object, is not idle: the gate exists to protect a live session.
func idleField(body []byte, field string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return false
	}
	var value bool
	raw, ok := object[field]
	return ok && json.Unmarshal(raw, &value) == nil && value
}

// start creates the first due unit whose Job is not there yet.
func (p *Poller) start(ctx context.Context, due []unit, hb *Heartbeat) {
	for _, u := range due {
		job, err := p.render(u)
		if err != nil {
			if errors.Is(err, errNoTemplate) {
				continue
			}
			p.Logger.WarnContext(ctx, "render job", "job", u.name, "err", err.Error())
			hb.Errors++
			continue
		}
		err = p.Jobs.CreateJob(ctx, job)
		switch {
		case err == nil:
			p.Logger.InfoContext(ctx, "started", "job", u.name, "kind", u.kind, "repo", u.values["repo"])
			hb.Started = u.name
			return
		case kube.IsAlreadyExists(err):
			hb.Skipped = append(hb.Skipped, u.name)
		default:
			p.Logger.WarnContext(ctx, "create job", "job", u.name, "err", err.Error())
			hb.Errors++
			return
		}
	}
}

var errNoTemplate = errors.New("no template for this kind")

// render reads the kind's template and fills it in for the unit.
func (p *Poller) render(u unit) ([]byte, error) {
	if p.Limits.TemplateDir == "" {
		return nil, errNoTemplate
	}
	template, err := os.ReadFile(filepath.Join(p.Limits.TemplateDir, u.kind+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoTemplate
	}
	if err != nil {
		return nil, err
	}
	return RenderJob(template, u.name, u.kind, u.values)
}

var placeholderRE = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)

// RenderJob fills a Job template in: every `{{name}}` in a string value is
// replaced (an unknown name is an error, so a typo in a template is caught), and
// the name, labels and annotations are set. The template's own labels and
// annotations are kept.
func RenderJob(template []byte, name, kind string, values map[string]string) ([]byte, error) {
	var job map[string]any
	if err := json.Unmarshal(template, &job); err != nil {
		return nil, fmt.Errorf("template for %s: %w", kind, err)
	}
	var missing []string
	var fill func(v any) any
	fill = func(v any) any {
		switch v := v.(type) {
		case string:
			return placeholderRE.ReplaceAllStringFunc(v, func(m string) string {
				key := m[2 : len(m)-2]
				if key == "job_name" {
					return name
				}
				if key == "kind" {
					return kind
				}
				value, ok := values[key]
				if !ok {
					missing = append(missing, key)
				}
				return value
			})
		case map[string]any:
			for k, item := range v {
				v[k] = fill(item)
			}
			return v
		case []any:
			for i, item := range v {
				v[i] = fill(item)
			}
			return v
		default:
			return v
		}
	}
	filled, _ := fill(job).(map[string]any)
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("template for %s names placeholders that have no value: %s", kind, strings.Join(missing, ", "))
	}
	meta, _ := filled["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		filled["metadata"] = meta
	}
	meta["name"] = name
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	labels[LabelManagedBy] = ManagedBy
	labels[LabelComponent] = kind
	meta["labels"] = labels
	annotations, _ := meta["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	for _, key := range []string{"repo", "number", "head_sha", "issue"} {
		if values[key] != "" {
			annotations["pit-crew/"+strings.ReplaceAll(key, "_", "-")] = values[key]
		}
	}
	meta["annotations"] = annotations
	return json.Marshal(filled)
}

var notNameRE = regexp.MustCompile(`[^a-z0-9-]+`)

// repoSlug is a repository's name as a Job name can carry it.
func repoSlug(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	slug := strings.Trim(notNameRE.ReplaceAllString(strings.ToLower(name), ""), "-")
	if slug == "" {
		return "repo"
	}
	return slug
}

func pullValues(pr github.PullRequest) map[string]string {
	owner, name, _ := strings.Cut(pr.Repo, "/")
	return map[string]string{
		"repo": pr.Repo, "owner": owner, "name": name, "number": strconv.Itoa(pr.Number), "head_sha": pr.HeadSHA,
		"base_ref": pr.BaseRef, "issue_repo": "", "issue": "", "round": "",
	}
}

func reviewUnit(c Candidate) unit {
	short := c.Pull.HeadSHA
	if len(short) > 8 {
		short = short[:8]
	}
	name := fmt.Sprintf("review-%s-%d-%s-f%d", repoSlug(c.Pull.Repo), c.Pull.Number, short, c.Rounds)
	if c.Decision.Reason == "re-review" {
		// The first review's Job at this head is still there for its TTL, so the
		// ask gets a name of its own.
		name += fmt.Sprintf("-a%d", c.ReReviews)
	}
	return unit{kind: "review", name: name, values: pullValues(c.Pull)}
}

func fixUnit(c Candidate) unit {
	values := pullValues(c.Pull)
	values["round"] = strconv.Itoa(c.Decision.Round)
	for _, ref := range ladder.IssueRefs(c.Pull.Body) {
		values["issue_repo"], values["issue"] = ref.Repo, strconv.Itoa(ref.Number)
		break
	}
	return unit{kind: "fix", name: fmt.Sprintf("fix-%s-%d-r%d", repoSlug(c.Pull.Repo), c.Pull.Number, c.Decision.Round), values: values}
}

func buildUnit(c BuildCandidate) unit {
	owner, name, _ := strings.Cut(c.Decision.Repo, "/")
	return unit{kind: "build", name: fmt.Sprintf("build-%s-%d", repoSlug(c.Decision.Repo), c.Issue.Number), values: map[string]string{
		"repo": c.Decision.Repo, "owner": owner, "name": name, "number": "", "head_sha": "", "base_ref": "main",
		"issue_repo": c.Issue.Repo, "issue": strconv.Itoa(c.Issue.Number), "round": "",
	}}
}

// heartbeat logs the summary and sends it where the configuration says. A
// heartbeat that cannot be sent is logged; it never fails the run.
func (p *Poller) heartbeat(ctx context.Context, hb Heartbeat) {
	data, _ := json.Marshal(hb)
	p.Logger.InfoContext(ctx, "heartbeat", "summary", string(data))
	if p.Limits.HeartbeatFile != "" {
		if err := os.WriteFile(p.Limits.HeartbeatFile, append(data, '\n'), 0o600); err != nil {
			p.Logger.WarnContext(ctx, "write heartbeat", "err", err.Error())
		}
	}
	if p.Limits.HeartbeatURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Limits.HeartbeatURL, bytes.NewReader(data))
		if err != nil {
			p.Logger.WarnContext(ctx, "send heartbeat", "err", err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.httpClient().Do(req)
		if err != nil {
			p.Logger.WarnContext(ctx, "send heartbeat", "err", err.Error())
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			p.Logger.WarnContext(ctx, "send heartbeat", "status", resp.StatusCode)
		}
	}
}
