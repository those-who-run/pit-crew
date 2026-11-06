// Package claude is twr-claude: the wrapper that runs Claude Code on the
// maintainer's subscription. `run` is the agent step's entrypoint in the runner
// image; `login` and `status` are the maintainer's, on his own machine, and put
// the subscription token in the cluster's Secret and ask how old it is.
//
// The token is the one credential the model's step holds. `run` reads it from a
// mounted file, gives it to the `claude` process alone, and clears every API key
// and GitHub token from that process's environment; it refuses to start at all
// with a GitHub token in its own. docs/twr-claude.md says what was checked
// against the CLI and what was not.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The exit codes of `twr-claude run`. The poller acts on ExitLoginExpired and
// ExitPlanLimit.
const (
	ExitOK           = 0
	ExitFailed       = 1
	ExitConfig       = 2
	ExitRefused      = 3
	ExitLoginExpired = 70
	ExitMaxTurns     = 71
	ExitTimeout      = 72
	ExitPlanLimit    = 75
)

// RunConfig is `run`'s configuration, from the environment.
type RunConfig struct {
	TokenFile string
	// Brief is the agent file or skill whose body starts the prompt.
	Brief string
	// Task is the task statement the fetch step wrote, which ends the prompt.
	Task    string
	WorkDir string
	AddDirs []string
	// Tools is the --tools list, comma-separated.
	Tools    string
	MaxTurns int
	Model    string
	Timeout  time.Duration
	Bin      string
	// Out is where agent.json is written.
	Out string
}

// RunConfigFromEnv reads the configuration; an unset variable takes its default
// and a value that cannot be read is an error.
func RunConfigFromEnv(getenv func(string) string) (RunConfig, error) {
	cfg := RunConfig{
		TokenFile: or(getenv("TWR_CLAUDE_TOKEN_FILE"), "/secrets/claude/token"),
		Brief:     getenv("TWR_CLAUDE_BRIEF"),
		Task:      or(getenv("TWR_CLAUDE_TASK"), "/work/data/task.md"),
		WorkDir:   or(getenv("TWR_CLAUDE_WORKDIR"), "/work/repo"),
		Tools:     or(getenv("TWR_CLAUDE_TOOLS"), "Read,Grep,Glob,Write,Bash"),
		Model:     getenv("TWR_CLAUDE_MODEL"),
		Bin:       or(getenv("TWR_CLAUDE_BIN"), "claude"),
		Out:       or(getenv("TWR_CLAUDE_OUT"), "/work/out"),
		MaxTurns:  80,
		Timeout:   25 * time.Minute,
	}
	for _, dir := range strings.Split(or(getenv("TWR_CLAUDE_ADD_DIRS"), "/work/data,/work/agent,/work/out"), ",") {
		if dir = strings.TrimSpace(dir); dir != "" {
			cfg.AddDirs = append(cfg.AddDirs, dir)
		}
	}
	if cfg.Brief == "" {
		return cfg, errors.New("TWR_CLAUDE_BRIEF: the brief to run is not set")
	}
	if v := getenv("TWR_CLAUDE_MAX_TURNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("TWR_CLAUDE_MAX_TURNS: %q is not a number of turns", v)
		}
		cfg.MaxTurns = n
	}
	if v := getenv("TWR_CLAUDE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("TWR_CLAUDE_TIMEOUT: %q is not a duration", v)
		}
		cfg.Timeout = d
	}
	return cfg, nil
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// Variables that must never reach the model's process: another credential that
// would outrank the subscription token, a cloud-provider switch, or a GitHub
// token of any kind.
var (
	clearedExact = []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS",
	}
	clearedPrefixes = []string{"GH_", "GITHUB_"}
	// A GitHub token by its shape, whatever variable carries it.
	githubTokenRE = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
)

// GitHubTokenIn returns the name of the first variable in environ that carries a
// GitHub token: GH_TOKEN, GITHUB_TOKEN, GH_ENTERPRISE_TOKEN, or any variable whose
// value has a token's shape.
func GitHubTokenIn(environ []string) string {
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
			if value != "" {
				return name
			}
		}
		if githubTokenRE.MatchString(value) {
			return name
		}
	}
	return ""
}

// childEnv is the environment the claude process gets: the step's, less what is
// cleared, plus the token and the switches that keep the run quiet and the token
// out of the commands the model runs.
func childEnv(environ []string, token, home string) []string {
	var env []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if cleared(name) {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"CLAUDE_CODE_OAUTH_TOKEN="+token,
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
	)
}

func cleared(name string) bool {
	for _, exact := range clearedExact {
		if name == exact {
			return true
		}
	}
	for _, prefix := range clearedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "CLAUDE_CODE_OAUTH_TOKEN" || name == "CLAUDE_CONFIG_DIR"
}

// Args is the claude command line for a run, without the binary.
func Args(cfg RunConfig) []string {
	args := []string{
		"-p", "--restricted", "--strict-mcp-config",
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--tools", cfg.Tools,
		"--max-turns", strconv.Itoa(cfg.MaxTurns),
		"--output-format", "json", "--no-session-persistence",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	for _, dir := range cfg.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	return args
}

// Prompt joins the brief's body (its YAML front matter dropped) and the task
// statement.
func Prompt(brief, task []byte) string {
	body := string(brief)
	if strings.HasPrefix(body, "---\n") {
		if _, rest, ok := strings.Cut(body[4:], "\n---\n"); ok {
			body = rest
		}
	}
	return strings.TrimSpace(body) + "\n\n" + strings.TrimSpace(string(task)) + "\n"
}

// Result is the summary `run` writes to agent.json: the CLI's result fields that
// say how the run went, never the token.
type Result struct {
	Exit       int     `json:"exit"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	// Outcome is the wrapper's reading: ok, login-expired, plan-limit, max-turns,
	// timeout or failed.
	Outcome string `json:"outcome"`
}

// The documented error strings for an expired or invalid login and for the
// plan's usage limit, matched without case in the CLI's output.
var (
	loginExpiredRE = regexp.MustCompile(`(?i)login expired|not logged in|oauth token (?:revoked|has expired|expired)|invalid authentication credentials|authentication_error|invalid api key`)
	planLimitRE    = regexp.MustCompile(`(?i)hit your (?:session|weekly|opus|sonnet|monthly|\w+) limit|usage limit|rate_limit|request rejected \(429\)|temporarily limiting requests|credit balance is too low`)
)

// Classify reads the CLI's exit code and output and names the outcome.
func Classify(exit int, stdout, stderr []byte) (Result, int) {
	var result Result
	// The result is the last JSON object on standard output.
	if start := bytes.IndexByte(stdout, '{'); start >= 0 {
		_ = json.Unmarshal(stdout[start:], &result)
	}
	result.Exit = exit
	text := string(stdout) + "\n" + string(stderr)
	switch {
	case exit == 0 && !result.IsError:
		result.Outcome = "ok"
		return result, ExitOK
	case result.Subtype == "error_max_turns":
		result.Outcome = "max-turns"
		return result, ExitMaxTurns
	case loginExpiredRE.MatchString(text):
		result.Outcome = "login-expired"
		return result, ExitLoginExpired
	case planLimitRE.MatchString(text):
		result.Outcome = "plan-limit"
		return result, ExitPlanLimit
	default:
		result.Outcome = "failed"
		return result, ExitFailed
	}
}

// Run starts claude for one task and returns the exit code. environ is the step's
// own environment; stderr gets the CLI's standard error and the wrapper's own
// lines, and nothing that is a credential.
func Run(ctx context.Context, cfg RunConfig, environ []string, stderr io.Writer) int {
	if name := GitHubTokenIn(environ); name != "" {
		_, _ = fmt.Fprintf(stderr, "twr-claude: refusing to start: %s is set, and the model's step never holds a GitHub token\n", name)
		return ExitRefused
	}
	token, err := os.ReadFile(cfg.TokenFile) //nolint:gosec // the path is the Secret's mount, from the configuration
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: token: %v\n", err)
		return ExitConfig
	}
	tokenText := strings.TrimSpace(string(token))
	if tokenText == "" {
		_, _ = fmt.Fprintln(stderr, "twr-claude: the token file is empty")
		return ExitConfig
	}
	brief, err := os.ReadFile(cfg.Brief) //nolint:gosec // the brief's path is the configuration
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: brief: %v\n", err)
		return ExitConfig
	}
	task, err := os.ReadFile(cfg.Task) //nolint:gosec // the task's path is the configuration
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: task: %v\n", err)
		return ExitConfig
	}
	home := os.Getenv("HOME")
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Bin, Args(cfg)...) //nolint:gosec // the binary and the arguments are the wrapper's own
	cmd.Dir = cfg.WorkDir
	cmd.Env = childEnv(environ, tokenText, home)
	// Once the time limit kills claude, a child it left holding the output is not
	// waited for.
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(Prompt(brief, task))
	var stdout, errBuf bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(stderr, &errBuf)
	start := time.Now()
	runErr := cmd.Run()
	exit := 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		exit = exitErr.ExitCode()
	default:
		_, _ = fmt.Fprintf(stderr, "twr-claude: start %s: %v\n", cfg.Bin, runErr)
		return ExitConfig
	}
	result, code := Classify(exit, stdout.Bytes(), errBuf.Bytes())
	if ctx.Err() != nil {
		result.Outcome, code = "timeout", ExitTimeout
	}
	if result.DurationMS == 0 {
		result.DurationMS = time.Since(start).Milliseconds()
	}
	if data, err := json.MarshalIndent(result, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(cfg.Out, "agent.json"), append(data, '\n'), 0o600); err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude: write agent.json: %v\n", err)
		}
	}
	_, _ = fmt.Fprintf(stderr, "twr-claude: %s (exit %d, %d turns, %d ms)\n", result.Outcome, exit, result.NumTurns, result.DurationMS)
	return code
}
