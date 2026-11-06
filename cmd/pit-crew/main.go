// Command pit-crew is the no-model machinery of the merge ladder, one subcommand
// per step: poll, fetch, publish-review, publish-build, ready, and lane. Each reads
// its configuration from the environment (docs/jobs.md), logs JSON to standard
// output and exits with the code its step defines. `pit-crew -version` prints the
// version.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/those-who-run/pit-crew/internal/apptoken"
	"github.com/those-who-run/pit-crew/internal/build"
	"github.com/those-who-run/pit-crew/internal/fetch"
	"github.com/those-who-run/pit-crew/internal/github"
	"github.com/those-who-run/pit-crew/internal/kube"
	"github.com/those-who-run/pit-crew/internal/lanes"
	"github.com/those-who-run/pit-crew/internal/poller"
	"github.com/those-who-run/pit-crew/internal/review"
)

// version is what -version prints: "dev" unless the build sets it with
// -ldflags "-X main.version=X.Y.Z", as the Dockerfile does from its VERSION
// argument on a release tag.
var version = "dev"

const usage = `usage: pit-crew [-version] <command>

commands:
  poll            read GitHub, decide, start at most one Job (POLL_*, LADDER_*)
  ready           set ladder / ready and ladder / stuck, write the ready list
  fetch           prepare the checkout and the data files for the agent (FETCH_*)
  publish-review  check and post a reviewer's output (PUBLISH_*)
  publish-build   check and push a builder's work, open the pull request (PUBLISH_*)
  lane            print the lane of a git diff --name-status read from stdin

docs/jobs.md says what each one reads and writes.
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// run is main without the process around it: it returns the exit code, 2 for a
// usage or configuration error.
func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet("pit-crew", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	if flags.NArg() < 1 || (flags.NArg() > 1 && flags.Arg(0) != "lane") {
		flags.Usage()
		return 2
	}
	logger, err := newLogger(stdout, getenv("LOG_LEVEL"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	var code int
	switch command := flags.Arg(0); command {
	case "lane":
		code, err = laneCommand(flags.Args()[1:], stdin, stdout, stderr)
	case "poll":
		code, err = pollCommand(ctx, getenv, logger)
	case "ready":
		code, err = readyCommand(ctx, getenv, logger)
	case "fetch":
		code, err = fetchCommand(ctx, getenv, logger)
	case "publish-review":
		code, err = publishReviewCommand(ctx, getenv, logger)
	case "publish-build":
		code, err = publishBuildCommand(ctx, getenv, logger)
	default:
		_, _ = fmt.Fprintf(stderr, "pit-crew: unknown command %q\n%s", command, usage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pit-crew %s: %v\n", flags.Arg(0), err)
	}
	return code
}

func newLogger(w io.Writer, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if level != "" {
		if err := lvl.UnmarshalText([]byte(level)); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}

// laneCommand is the port of `python3 agent/lanes.py --repo owner/name`: the lane
// of a `git diff --name-status` read from stdin, with the same exit codes.
func laneCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("pit-crew lane", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "the repository, as owner/name")
	config := flags.String("config", "", "lanes.json")
	if err := flags.Parse(args); err != nil {
		return 2, nil
	}
	if *repo == "" || *config == "" {
		return 2, errors.New("--repo and --config are needed")
	}
	cfg, err := lanes.Load(*config)
	if err != nil {
		return 2, err
	}
	text, err := io.ReadAll(stdin)
	if err != nil {
		return 2, err
	}
	changes, err := lanes.ParseNameStatus(string(text))
	if err != nil {
		return 2, err
	}
	_, _ = fmt.Fprintln(stdout, cfg.Classify(*repo, changes))
	return 0, nil
}

// client mints the step's token and returns a GitHub client with it.
func client(ctx context.Context, getenv func(string) string, owner string, repos []string, permissions apptoken.Permissions, logger *slog.Logger) (*github.Client, error) {
	cfg, err := apptoken.FromEnv(getenv, owner)
	if err != nil {
		return nil, err
	}
	// The key must be there before anything is tried; a missing mount is a
	// configuration error, not a network one.
	if _, err := os.Stat(cfg.KeyPath); err != nil {
		return nil, fmt.Errorf("APP_KEY_FILE: %w", err)
	}
	gh := &github.Client{BaseURL: getenv("GITHUB_API_URL"), Logger: logger}
	// The pod's network may not be up yet: wait for it before the first real call.
	if err := gh.WaitForNetwork(ctx); err != nil {
		return nil, err
	}
	token, err := apptoken.Mint(ctx, &http.Client{Timeout: 30 * time.Second}, cfg, repos, permissions)
	if err != nil {
		return nil, err
	}
	gh.Token = token.Value
	return gh, nil
}

func pollCommand(ctx context.Context, getenv func(string) string, logger *slog.Logger) (int, error) {
	policies, err := poller.PoliciesFromEnv(getenv)
	if err != nil {
		return 2, err
	}
	limits, err := poller.LimitsFromEnv(getenv)
	if err != nil {
		return 2, err
	}
	gh, err := client(ctx, getenv, policies.Build.Owner, nil, apptoken.Permissions{"metadata": "read", "contents": "read", "pull_requests": "read", "issues": "read"}, logger)
	if err != nil {
		return 2, err
	}
	jobs, err := kube.New(kube.Config{APIURL: getenv("POLL_KUBE_API"), TokenFile: getenv("POLL_KUBE_TOKEN_FILE"), CAFile: getenv("POLL_KUBE_CA_FILE"), Namespace: getenv("POLL_NAMESPACE")}, getenv)
	if err != nil {
		return 2, err
	}
	p := &poller.Poller{Source: gh, Jobs: jobs, Policies: policies, Limits: limits, Logger: logger}
	if _, err := p.Run(ctx); err != nil {
		return 1, err
	}
	return 0, nil
}

func readyCommand(ctx context.Context, getenv func(string) string, logger *slog.Logger) (int, error) {
	policies, err := poller.PoliciesFromEnv(getenv)
	if err != nil {
		return 2, err
	}
	gh, err := client(ctx, getenv, policies.Build.Owner, nil, apptoken.Permissions{"metadata": "read", "contents": "read", "pull_requests": "read", "statuses": "write"}, logger)
	if err != nil {
		return 2, err
	}
	list, err := poller.Ready(ctx, gh, gh, policies, logger)
	if err != nil {
		return 1, err
	}
	logger.InfoContext(ctx, "ready", "count", len(list))
	if path := getenv("READY_LIST_FILE"); path != "" {
		if err := poller.WriteReadyList(path, list); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

func fetchCommand(ctx context.Context, getenv func(string) string, logger *slog.Logger) (int, error) {
	cfg, err := fetch.ConfigFromEnv(getenv)
	if err != nil {
		return 2, err
	}
	repos := []string{cfg.Repo, cfg.AgentRepo}
	if cfg.IssueRepo != "" {
		repos = append(repos, cfg.IssueRepo)
	}
	gh, err := client(ctx, getenv, "", distinct(repos), apptoken.Permissions{"metadata": "read", "contents": "read", "pull_requests": "read", "issues": "read"}, logger)
	if err != nil {
		return 2, err
	}
	cfg.Token = gh.Token
	return fetch.Run(ctx, cfg, gh, logger), nil
}

func publishReviewCommand(ctx context.Context, getenv func(string) string, logger *slog.Logger) (int, error) {
	cfg, err := review.ConfigFromEnv(getenv)
	if err != nil {
		return 2, err
	}
	gh, err := client(ctx, getenv, "", []string{cfg.Repo}, apptoken.Permissions{"metadata": "read", "pull_requests": "write", "statuses": "write"}, logger)
	if err != nil {
		return 2, err
	}
	return review.Run(ctx, cfg, gh, logger), nil
}

func publishBuildCommand(ctx context.Context, getenv func(string) string, logger *slog.Logger) (int, error) {
	cfg, err := build.ConfigFromEnv(getenv)
	if err != nil {
		return 2, err
	}
	gh, err := client(ctx, getenv, "", []string{cfg.Repo}, apptoken.Permissions{"metadata": "read", "contents": "write", "pull_requests": "write"}, logger)
	if err != nil {
		return 2, err
	}
	cfg.Token = gh.Token
	return build.Run(ctx, cfg, gh, logger), nil
}

func distinct(items []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range items {
		key := strings.ToLower(item)
		if !seen[key] {
			seen[key] = true
			out = append(out, item)
		}
	}
	return out
}
