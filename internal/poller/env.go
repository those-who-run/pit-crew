package poller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/those-who-run/pit-crew/internal/ladder"
)

// PoliciesFromEnv reads the ladder's configuration from LADDER_* variables. With
// nothing set, nobody is allowlisted and nothing is built.
func PoliciesFromEnv(getenv func(string) string) (Policies, error) {
	owner := or(getenv("LADDER_OWNER"), "those-who-run")
	p := Policies{
		Pull: ladder.Policy{
			Reviewer:   or(getenv("LADDER_REVIEWER"), "twr-reviewer[bot]"),
			Builder:    or(getenv("LADDER_BUILDER"), "twr-builder[bot]"),
			Authors:    list(getenv("LADDER_AUTHORS")),
			Committers: list(getenv("LADDER_COMMITTERS")),
			HoldLabel:  or(getenv("LADDER_HOLD_LABEL"), "hold"),
			// The same login that starts a build with agent-ready asks for a review
			// again with re-review.
			Maintainer:    getenv("LADDER_MAINTAINER"),
			ReReviewLabel: or(getenv("LADDER_REREVIEW_LABEL"), "re-review"),
		},
		Repos:     list(getenv("LADDER_REPOS")),
		IssueRepo: or(getenv("LADDER_ISSUE_REPO"), owner+"/paddock"),
	}
	p.Build = ladder.BuildPolicy{
		Maintainer:      p.Pull.Maintainer,
		Builder:         p.Pull.Builder,
		Authors:         p.Pull.Authors,
		ReadyLabel:      or(getenv("LADDER_READY_LABEL"), "agent-ready"),
		RetryLabel:      or(getenv("LADDER_RETRY_LABEL"), "retry"),
		TriageLabel:     or(getenv("LADDER_TRIAGE_LABEL"), "needs triage"),
		HoldLabel:       p.Pull.HoldLabel,
		ComponentPrefix: or(getenv("LADDER_COMPONENT_PREFIX"), "component: "),
		Components:      map[string]string{},
		Owner:           owner,
		Repos:           list(getenv("LADDER_BUILD_REPOS")),
	}
	for _, pair := range list(getenv("LADDER_COMPONENTS")) {
		label, repo, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(label) == "" {
			return p, fmt.Errorf("LADDER_COMPONENTS: %q is not label=owner/name", pair)
		}
		p.Build.Components[strings.TrimSpace(label)] = strings.TrimSpace(repo)
	}
	return p, nil
}

// LimitsFromEnv reads the poller's POLL_* configuration.
func LimitsFromEnv(getenv func(string) string) (Limits, error) {
	limits := Limits{
		StartJobs:           getenv("POLL_START_JOBS") == "true",
		TemplateDir:         getenv("POLL_JOB_TEMPLATES"),
		MaxActive:           1,
		DailyCap:            12,
		BackoffPlanLimit:    3 * time.Hour,
		BackoffLoginExpired: 6 * time.Hour,
		IdleGateURL:         getenv("POLL_IDLE_GATE_URL"),
		IdleGateField:       getenv("POLL_IDLE_GATE_FIELD"),
		IdleGateCmd:         strings.Fields(getenv("POLL_IDLE_GATE_CMD")),
		HeartbeatURL:        getenv("POLL_HEARTBEAT_URL"),
		HeartbeatFile:       getenv("POLL_HEARTBEAT_FILE"),
		Location:            time.UTC,
	}
	var err error
	for _, n := range []struct {
		name string
		into *int
	}{{"POLL_MAX_ACTIVE", &limits.MaxActive}, {"POLL_DAILY_CAP", &limits.DailyCap}} {
		if v := getenv(n.name); v != "" {
			if *n.into, err = strconv.Atoi(v); err != nil || *n.into < 0 {
				return limits, fmt.Errorf("%s: %q is not a number", n.name, v)
			}
		}
	}
	for _, d := range []struct {
		name string
		into *time.Duration
	}{{"POLL_BACKOFF_PLAN_LIMIT", &limits.BackoffPlanLimit}, {"POLL_BACKOFF_LOGIN_EXPIRED", &limits.BackoffLoginExpired}} {
		if v := getenv(d.name); v != "" {
			if *d.into, err = time.ParseDuration(v); err != nil || *d.into < 0 {
				return limits, fmt.Errorf("%s: %q is not a duration", d.name, v)
			}
		}
	}
	if limits.Quiet, err = ParseWindows(getenv("POLL_QUIET")); err != nil {
		return limits, fmt.Errorf("POLL_QUIET: %w", err)
	}
	if v := getenv("POLL_TZ"); v != "" {
		if limits.Location, err = time.LoadLocation(v); err != nil {
			return limits, fmt.Errorf("POLL_TZ: %w", err)
		}
	}
	return limits, nil
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func list(text string) []string {
	var items []string
	for _, item := range strings.Split(text, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
