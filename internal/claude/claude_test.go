package claude

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// stub writes an executable that records its arguments, its environment and its
// standard input under dir, then runs the script given.
func stub(t *testing.T, dir, name, script string) string {
	t.Helper()
	return executable(t, dir, name, "printf '%s\\n' \"$@\" > \"$STUB_DIR/"+name+".args\"\nenv > \"$STUB_DIR/"+name+".env\"\ncat > \"$STUB_DIR/"+name+".stdin\"\n"+script)
}

// executable writes a shell script under dir and returns its path.
func executable(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // a test's own executable
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a file in the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func write(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const token = "sk-ant-oat01-" + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"

func runConfig(t *testing.T, dir, script string) RunConfig {
	t.Helper()
	out := filepath.Join(dir, "out")
	_ = os.Mkdir(out, 0o700)
	return RunConfig{
		TokenFile: write(t, filepath.Join(dir, "token"), token+"\n"),
		Brief:     write(t, filepath.Join(dir, "brief.md"), "---\nname: twr-reviewer\ntools: Read\n---\n\nYou are the Reviewer.\n"),
		Task:      write(t, filepath.Join(dir, "task.md"), "## This task\n\nReview those-who-run/ecu#31.\n"),
		WorkDir:   dir,
		AddDirs:   []string{"/work/data", "/work/out"},
		Tools:     "Read,Grep,Glob,Write,Bash",
		MaxTurns:  80,
		Model:     "opus",
		Timeout:   time.Minute,
		Bin:       stub(t, dir, "claude", script),
		Out:       out,
		EnvScrub:  true,
	}
}

const okResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":12,"duration_ms":4200,"total_cost_usd":0.42,"result":"done","session_id":"x"}`

func TestRunStartsClaudeWithTheTokenAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig(t, dir, "echo '"+okResult+"'\n")
	environ := []string{"STUB_DIR=" + dir, "PATH=/usr/bin:/bin", "HOME=" + dir, "ANTHROPIC_API_KEY=x", "ANTHROPIC_AUTH_TOKEN=x",
		"CLAUDE_CODE_USE_BEDROCK=1", "AWS_SECRET_ACCESS_KEY=y", "GITHUB_REPOSITORY=those-who-run/ecu", "LANG=C.UTF-8"}
	var stderr bytes.Buffer
	if code := Run(t.Context(), cfg, environ, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	args := strings.Split(strings.TrimSpace(read(t, filepath.Join(dir, "claude.args"))), "\n")
	want := []string{"-p", "--restricted", "--strict-mcp-config", "--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--tools", "Read,Grep,Glob,Write,Bash", "--max-turns", "80", "--output-format", "json", "--no-session-persistence",
		"--model", "opus", "--add-dir", "/work/data", "--add-dir", "/work/out"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args %q\nwant %q", args, want)
	}
	env := read(t, filepath.Join(dir, "claude.env"))
	for _, line := range []string{"CLAUDE_CODE_OAUTH_TOKEN=" + token, "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1", "DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, ".claude"), "LANG=C.UTF-8"} {
		if !strings.Contains(env, line+"\n") {
			t.Errorf("the environment lacks %s", line)
		}
	}
	for _, gone := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "AWS_SECRET_ACCESS_KEY", "GITHUB_REPOSITORY"} {
		if strings.Contains(env, gone+"=") {
			t.Errorf("the environment still has %s", gone)
		}
	}
	prompt := read(t, filepath.Join(dir, "claude.stdin"))
	if prompt != "You are the Reviewer.\n\n## This task\n\nReview those-who-run/ecu#31.\n" {
		t.Errorf("prompt %q", prompt)
	}
	if strings.Contains(stderr.String(), token) {
		t.Error("the token was printed")
	}
	if strings.Contains(stderr.String(), "env scrub is off") {
		t.Errorf("warned with the scrub on: %s", stderr.String())
	}
	var result Result
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, "out", "agent.json"))), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "ok" || result.NumTurns != 12 || result.CostUSD != 0.42 || result.DurationMS != 4200 || result.Exit != 0 {
		t.Errorf("agent.json %+v", result)
	}
}

// With the scrub off, claude is told so explicitly (never left to its own
// default) and the step says once, on standard error, what that gives up.
func TestRunWithTheEnvScrubOff(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig(t, dir, "echo '"+okResult+"'\n")
	cfg.EnvScrub = false
	var stderr bytes.Buffer
	if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir, "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1"}, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	env := read(t, filepath.Join(dir, "claude.env"))
	if !strings.Contains(env, "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0\n") || strings.Contains(env, "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1\n") {
		t.Errorf("the environment does not turn the scrub off:\n%s", env)
	}
	want := "twr-claude: subprocess env scrub is off (TWR_CLAUDE_ENV_SCRUB=0): commands the model runs can read its environment, including the token\n"
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q lacks the warning", stderr.String())
	}
	if strings.Contains(stderr.String(), token) {
		t.Error("the token was printed")
	}
}

func TestRunRefusesWithAGitHubToken(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig(t, dir, "echo '"+okResult+"'\n")
	for _, kv := range []string{"GH_TOKEN=x", "GITHUB_TOKEN=x", "GH_ENTERPRISE_TOKEN=x", "SOMETHING=ghs_" + strings.Repeat("a1B2", 9), "OTHER=github_pat_" + strings.Repeat("a1_B", 12)} {
		var stderr bytes.Buffer
		if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir, kv}, &stderr); code != ExitRefused {
			t.Errorf("%s: exit %d, want refused", kv, code)
		}
		if _, err := os.Stat(filepath.Join(dir, "claude.args")); err == nil {
			t.Errorf("%s: claude was started", kv)
		}
		if strings.Contains(stderr.String(), "ghs_") || strings.Contains(stderr.String(), "github_pat_") {
			t.Errorf("%s: the token's value was printed: %s", kv, stderr.String())
		}
	}
	if name := GitHubTokenIn([]string{"GH_TOKEN=", "PATH=/bin", "GH_REPO=those-who-run/ecu"}); name != "" {
		t.Errorf("an empty GH_TOKEN or a non-token GH_ variable refused: %s", name)
	}
}

func TestRunOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		want    int
		outcome string
	}{
		{"login expired, said on stderr", "echo 'Login expired · Please run /login' >&2\nexit 1\n", ExitLoginExpired, "login-expired"},
		{"token revoked, said in the result", `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["Failed to authenticate: OAuth token revoked"]}'` + "\nexit 1\n", ExitLoginExpired, "login-expired"},
		{"the session limit", `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["You have hit your session limit"]}'` + "\nexit 1\n", ExitPlanLimit, "plan-limit"},
		{"a 429", "echo 'API Error: Request rejected (429)' >&2\nexit 1\n", ExitPlanLimit, "plan-limit"},
		{"the turn limit", `echo '{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":80}'` + "\nexit 1\n", ExitMaxTurns, "max-turns"},
		{"an error result with exit 0", `echo '{"type":"result","subtype":"error_during_execution","is_error":true}'` + "\n", ExitFailed, "failed"},
		{"a plain failure", "echo nope >&2\nexit 1\n", ExitFailed, "failed"},
		{"the result's own text does not fool it", `echo '{"type":"result","subtype":"success","is_error":false,"result":"line 401 and 429 are fine"}'` + "\n", ExitOK, "ok"},
		// What a bogus token gives with the runner image: subtype success, one turn,
		// nothing on standard error.
		{"a rejected token, exit 1", `echo '{"subtype":"success","is_error":true,"num_turns":1,"terminal_reason":"api_error","result":"Failed to authenticate. API Error: 401 OAuth access token is invalid."}'` + "\nexit 1\n", ExitLoginExpired, "login-expired"},
		{"api_error with a 401 alone", `echo '{"subtype":"success","is_error":true,"num_turns":1,"terminal_reason":"api_error","result":"API Error: 401 Unauthorized"}'` + "\nexit 1\n", ExitLoginExpired, "login-expired"},
		{"a 401 from a curl the model ran, in an ok result", `echo '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"curl returned HTTP 401 Unauthorized; failed to authenticate against the test server, as the task expected. OAuth access token is invalid there by design."}'` + "\n", ExitOK, "ok"},
		{"a 401 from a curl the model ran, in a result that failed otherwise", `echo '{"subtype":"error_during_execution","is_error":true,"num_turns":3,"result":"curl returned HTTP 401 from the test server, then the build broke"}'` + "\nexit 1\n", ExitFailed, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := runConfig(t, dir, c.script)
			var stderr bytes.Buffer
			if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr); code != c.want {
				t.Errorf("exit %d, want %d: %s", code, c.want, stderr.String())
			}
			if !strings.Contains(read(t, filepath.Join(dir, "out", "agent.json")), `"outcome": "`+c.outcome+`"`) {
				t.Errorf("agent.json %s", read(t, filepath.Join(dir, "out", "agent.json")))
			}
		})
	}
}

// A run that did not end ok says why on standard error, after the summary: the
// result's subtype, is_error and text, with any token the model echoed replaced
// by <token>; it is the one place the reason survives the pod.
func TestRunSaysWhyItFailed(t *testing.T) {
	dir := t.TempDir()
	result := `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"duration_ms":2215,` +
		`"result":"Bash failed: env printed CLAUDE_CODE_OAUTH_TOKEN=` + token + ` and git refused the push","errors":["the model stopped: ` + token + `"]}`
	cfg := runConfig(t, dir, "echo '"+result+"'\nexit 1\n")
	var stderr bytes.Buffer
	if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr); code != ExitFailed {
		t.Errorf("exit %d, want %d", code, ExitFailed)
	}
	out := stderr.String()
	want := "twr-claude: failed (exit 1, 1 turns, 2215 ms)\n" +
		"twr-claude: result: subtype error_during_execution, is_error true, 1 turns\n" +
		"twr-claude: result text:\nBash failed: env printed CLAUDE_CODE_OAUTH_TOKEN=<token> and git refused the push\n" +
		"twr-claude: result error: the model stopped: <token>\n"
	if out != want {
		t.Errorf("stderr:\n%s\nwant:\n%s", out, want)
	}
	if strings.Contains(out, token) {
		t.Error("the token was printed")
	}
	if strings.Contains(read(t, filepath.Join(dir, "out", "agent.json")), "Bash failed") {
		t.Error("agent.json carries the result's text, which docs/jobs.md does not list")
	}
}

// The text is bounded: a long one shows its end, labelled.
func TestRunBoundsTheReason(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 3000) + "the end"
	cfg := runConfig(t, dir, `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"`+long+`"}'`+"\nexit 1\n")
	var stderr bytes.Buffer
	Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr)
	out := stderr.String()
	_, after, ok := strings.Cut(out, "twr-claude: result text (last 2000 characters):\n")
	if !ok {
		t.Fatalf("stderr %q: no bounded text", out)
	}
	if after != strings.Repeat("x", 2000-len("the end"))+"the end\n" {
		t.Errorf("the tail is %d characters: %q", len(after), after)
	}
}

// When standard output is not a result object at all, its end is shown as such,
// with any token in it redacted; when it is empty, that is said instead.
func TestRunShowsRawOutputWithoutAResult(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig(t, dir, "echo 'node: segmentation fault near "+token+"'\necho 'core dumped'\nexit 139\n")
	var stderr bytes.Buffer
	if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr); code != ExitFailed {
		t.Errorf("exit %d, want %d", code, ExitFailed)
	}
	out := stderr.String()
	want := "twr-claude: no result object on standard output; what is there:\nnode: segmentation fault near <token>\ncore dumped\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("stderr:\n%s\nwant it to end with:\n%s", out, want)
	}
	if strings.Contains(out, token) || strings.Contains(out, "result: subtype") {
		t.Errorf("stderr:\n%s", out)
	}

	dir = t.TempDir()
	cfg = runConfig(t, dir, "exit 1\n")
	stderr.Reset()
	Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr)
	if !strings.HasSuffix(stderr.String(), "twr-claude: no result object on standard output, and nothing else there\n") {
		t.Errorf("stderr %q", stderr.String())
	}

	dir = t.TempDir()
	cfg = runConfig(t, dir, `echo '{"type":"result","subtype":"error_during_execution","is_error":true}'`+"\nexit 1\n")
	stderr.Reset()
	Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr)
	if !strings.HasSuffix(stderr.String(), "is_error true, 0 turns\ntwr-claude: the result carries no text\n") {
		t.Errorf("stderr %q", stderr.String())
	}
}

// A run that ended ok shows its shape in one line, and nothing more.
func TestRunShowsTheShapeOfAnOkRun(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig(t, dir, "echo '"+okResult+"'\n")
	var stderr bytes.Buffer
	if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := "twr-claude: ok (exit 0, 12 turns, 4200 ms)\ntwr-claude: result: subtype success, is_error false, 12 turns, cost $0.4200\n"
	if stderr.String() != want {
		t.Errorf("stderr:\n%s\nwant:\n%s", stderr.String(), want)
	}
}

func TestRunTimesOut(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig(t, dir, "exec sleep 5\n")
	cfg.Timeout = 200 * time.Millisecond
	var stderr bytes.Buffer
	if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &stderr); code != ExitTimeout {
		t.Errorf("exit %d, want %d", code, ExitTimeout)
	}
}

func TestRunConfigErrors(t *testing.T) {
	dir := t.TempDir()
	good := runConfig(t, dir, "")
	cases := []struct {
		name   string
		change func(*RunConfig)
	}{
		{"no token file", func(c *RunConfig) { c.TokenFile = filepath.Join(dir, "none") }},
		{"an empty token", func(c *RunConfig) { c.TokenFile = write(t, filepath.Join(dir, "empty"), "\n") }},
		{"no brief", func(c *RunConfig) { c.Brief = filepath.Join(dir, "none") }},
		{"no task", func(c *RunConfig) { c.Task = filepath.Join(dir, "none") }},
		{"no claude", func(c *RunConfig) { c.Bin = filepath.Join(dir, "none") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := good
			c.change(&cfg)
			if code := Run(t.Context(), cfg, []string{"STUB_DIR=" + dir}, &bytes.Buffer{}); code != ExitConfig {
				t.Errorf("exit %d, want %d", code, ExitConfig)
			}
		})
	}
}

func TestRunConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cfg, err := RunConfigFromEnv(env(map[string]string{"TWR_CLAUDE_BRIEF": "agent/agents/twr-reviewer.md", "TWR_CLAUDE_MAX_TURNS": "40", "TWR_CLAUDE_TIMEOUT": "10m", "TWR_CLAUDE_ADD_DIRS": "/a, /b,"}))
	if err != nil || cfg.MaxTurns != 40 || cfg.Timeout != 10*time.Minute || !reflect.DeepEqual(cfg.AddDirs, []string{"/a", "/b"}) || cfg.Tools == "" || cfg.TokenFile == "" || !cfg.EnvScrub {
		t.Errorf("%+v %v", cfg, err)
	}
	for _, v := range []string{"1", "true", "yes"} {
		if cfg, err := RunConfigFromEnv(env(map[string]string{"TWR_CLAUDE_BRIEF": "x", "TWR_CLAUDE_ENV_SCRUB": v})); err != nil || !cfg.EnvScrub {
			t.Errorf("TWR_CLAUDE_ENV_SCRUB=%s: scrub %v, %v", v, cfg.EnvScrub, err)
		}
	}
	for _, v := range []string{"0", "false", "no"} {
		if cfg, err := RunConfigFromEnv(env(map[string]string{"TWR_CLAUDE_BRIEF": "x", "TWR_CLAUDE_ENV_SCRUB": v})); err != nil || cfg.EnvScrub {
			t.Errorf("TWR_CLAUDE_ENV_SCRUB=%s: scrub %v, %v", v, cfg.EnvScrub, err)
		}
	}
	for _, bad := range []map[string]string{
		{},
		{"TWR_CLAUDE_BRIEF": "x", "TWR_CLAUDE_MAX_TURNS": "0"},
		{"TWR_CLAUDE_BRIEF": "x", "TWR_CLAUDE_TIMEOUT": "soon"},
		{"TWR_CLAUDE_BRIEF": "x", "TWR_CLAUDE_ENV_SCRUB": "off"},
	} {
		_, err := RunConfigFromEnv(env(bad))
		if err == nil {
			t.Errorf("%v: no error", bad)
		}
		if v, ok := bad["TWR_CLAUDE_ENV_SCRUB"]; ok && err != nil && !strings.Contains(err.Error(), "TWR_CLAUDE_ENV_SCRUB: \""+v+"\"") {
			t.Errorf("%v: %v does not name the variable and the value", bad, err)
		}
	}
}

func TestPrompt(t *testing.T) {
	if got := Prompt([]byte("No front matter.\n"), []byte("Task.")); got != "No front matter.\n\nTask.\n" {
		t.Errorf("%q", got)
	}
	if got := Prompt([]byte("---\nname: x\n---\nBody.\n"), []byte("Task.\n")); got != "Body.\n\nTask.\n" {
		t.Errorf("%q", got)
	}
}

func secretConfig(t *testing.T, dir, kubectlScript string) SecretConfig {
	t.Helper()
	return SecretConfig{Namespace: "twr-agents", Name: "twr-claude-token", Key: "token", Context: "my-cluster",
		Kubectl: stub(t, dir, "kubectl", kubectlScript), WarnAfter: 330 * 24 * time.Hour,
		Now: func() time.Time { return time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC) }}
}

// kubectlStub answers `create secret ... --dry-run=client -o json` with the
// manifest kubectl would render, and records the rest.
const kubectlStub = `case "$*" in
  *--dry-run=client*) token=$(cat "$STUB_DIR/kubectl.stdin"); printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"twr-claude-token","namespace":"twr-agents","creationTimestamp":null},"data":{"token":"%s"}}' "$(printf '%s' "$token" | base64 -w0)";;
  *apply*) cp "$STUB_DIR/kubectl.stdin" "$STUB_DIR/applied.json"; cp "$STUB_DIR/kubectl.args" "$STUB_DIR/apply.args"; echo 'secret/twr-claude-token configured';;
esac
`

// terminal stands in for the maintainer's stderr: onWrite sees everything
// written so far, so a test can answer a prompt the way the maintainer would.
type terminal struct {
	bytes.Buffer
	onWrite func(sofar string)
}

func (tm *terminal) Write(p []byte) (int, error) {
	n, err := tm.Buffer.Write(p)
	tm.onWrite(tm.String())
	return n, err
}

// afterPrompt is the maintainer at the keyboard: it gives the code only once
// the prompt has been seen, and gives up (end of input) after a while, so a
// wrapper that keeps the prompt back fails instead of hanging.
type afterPrompt struct {
	seen <-chan struct{}
	code io.Reader
}

func (r *afterPrompt) Read(p []byte) (int, error) {
	select {
	case <-r.seen:
		return r.code.Read(p)
	case <-time.After(10 * time.Second):
		return 0, io.EOF
	}
}

// secretManifest is what the kubectl stub recorded `apply` receiving.
type secretManifest struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

func applied(t *testing.T, dir string) secretManifest {
	t.Helper()
	var secret secretManifest
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, "applied.json"))), &secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

var tokenInSecret = base64.StdEncoding.EncodeToString([]byte(token))

func TestLoginFromSetupToken(t *testing.T) {
	t.Setenv("STUB_DIR", t.TempDir())
	dir := os.Getenv("STUB_DIR")
	cfg := secretConfig(t, dir, kubectlStub)
	cfg.Claude = stub(t, dir, "claude", "echo 'Open https://claude.ai/oauth/authorize?x in your browser' >&2\necho 'Your token: "+token+"'\n")
	var stderr bytes.Buffer
	if code := Login(t.Context(), cfg, false, strings.NewReader(""), &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := read(t, filepath.Join(dir, "claude.args")); strings.TrimSpace(got) != "setup-token" {
		t.Errorf("claude args %q", got)
	}
	if strings.Contains(stderr.String(), token) {
		t.Error("the token reached the terminal")
	}
	if !strings.Contains(stderr.String(), "Your token: <token>") || !strings.Contains(stderr.String(), "issued 2026-11-06") {
		t.Errorf("stderr %q", stderr.String())
	}
	secret := applied(t, dir)
	if secret.Metadata.Name != "twr-claude-token" || secret.Metadata.Namespace != "twr-agents" || secret.Metadata.Annotations[IssuedAtAnnotation] != "2026-11-06T09:00:00Z" {
		t.Errorf("metadata %+v", secret.Metadata)
	}
	if secret.Data["token"] != tokenInSecret {
		t.Errorf("data %v", secret.Data)
	}
	args := read(t, filepath.Join(dir, "apply.args"))
	if !strings.HasPrefix(args, "--namespace\ntwr-agents\n--context\nmy-cluster\napply\n-f\n-\n") {
		t.Errorf("apply args %q", args)
	}
	// The token never touched a file under the stub directory but kubectl's own
	// recorded stdin, which stands in for the pipe.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "kubectl.stdin" || e.Name() == "applied.json" || e.Name() == "claude" || e.IsDir() {
			continue
		}
		if strings.Contains(read(t, filepath.Join(dir, e.Name())), token) {
			t.Errorf("%s holds the token", e.Name())
		}
	}
}

// On a headless host the CLI cannot open a browser: it prints the sign-in URL
// and a prompt with no newline, waits for the code on stdin, and prints the
// token only then. The maintainer can only paste the code once the prompt has
// reached the terminal, so a wrapper that holds the CLI's output back until it
// exits never gets the code, and the token never comes.
func TestLoginShowsSetupTokensPromptOnAHeadlessHost(t *testing.T) {
	t.Setenv("STUB_DIR", t.TempDir())
	dir := os.Getenv("STUB_DIR")
	cfg := secretConfig(t, dir, kubectlStub)
	const url = "https://claude.ai/oauth/authorize?x"
	const prompt = "Paste code here if prompted > "
	cfg.Claude = executable(t, dir, "claude", "echo 'Browser didn'\\''t open. Use the url below to sign in:'\necho '"+url+"'\n"+
		"printf '%s' '"+prompt+"'\nread code\nprintf '%s\\n' \"$code\" > \"$STUB_DIR/code\"\necho\necho 'Your token: "+token+"'\n")
	seen := make(chan struct{})
	var once sync.Once
	stderr := &terminal{onWrite: func(sofar string) {
		if strings.Contains(sofar, prompt) {
			once.Do(func() { close(seen) })
		}
	}}
	stdin := &afterPrompt{seen: seen, code: strings.NewReader("the-code#the-state\n")}
	if code := Login(t.Context(), cfg, false, stdin, stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(read(t, filepath.Join(dir, "code"))); got != "the-code#the-state" {
		t.Errorf("the CLI read %q as the code: the prompt did not reach the terminal while it waited", got)
	}
	out := stderr.String()
	if i, j, k := strings.Index(out, url), strings.Index(out, prompt), strings.Index(out, "Your token: <token>"); i < 0 || j < i || k < j {
		t.Errorf("stderr %q: want the URL, then the prompt, then the token's line", out)
	}
	if strings.Contains(out, token) {
		t.Error("the token reached the terminal")
	}
	if got := applied(t, dir).Data["token"]; got != tokenInSecret {
		t.Errorf("kubectl received %q", got)
	}
}

func TestForwardKeepsTheTokenBackWhateverTheReads(t *testing.T) {
	cases := []struct {
		name, in, out, token string
	}{
		{"a prompt with no newline", "Open https://claude.ai/oauth/authorize?x\nPaste code here > ", "Open https://claude.ai/oauth/authorize?x\nPaste code here > ", ""},
		{"the token on its line", "Your token: " + token + "\nDone.\n", "Your token: <token>\nDone.\n", token},
		{"the token last, with no newline", token, "<token>", token},
		{"too short to be a token", "sk-ant-oat01-tooshort\n", "sk-ant-oat01-tooshort\n", ""},
		{"words that start like the prefix", "ask sk-an skip\n", "ask sk-an skip\n", ""},
	}
	for _, c := range cases {
		for _, oneByte := range []bool{false, true} {
			var r io.Reader = strings.NewReader(c.in)
			if oneByte {
				r = iotest.OneByteReader(r)
			}
			var out bytes.Buffer
			if got := forward(r, &out); got != c.token || out.String() != c.out {
				t.Errorf("%s (one byte at a time: %v): token %q, wrote %q", c.name, oneByte, got, out.String())
			}
		}
	}
}

func TestLoginFromStdin(t *testing.T) {
	cases := []struct{ name, stdin string }{
		{"the token, Enter, then whatever else", token + "\nsk-ant-oat01-" + strings.Repeat("junk", 8) + "\nmore\n"},
		{"the token with no newline", token},
		{"a blank line, then the token with spaces around it", "\n  " + token + "  \n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("STUB_DIR", t.TempDir())
			dir := os.Getenv("STUB_DIR")
			cfg := secretConfig(t, dir, kubectlStub)
			cfg.Claude = filepath.Join(dir, "no-claude")
			var stderr bytes.Buffer
			if code := Login(t.Context(), cfg, true, strings.NewReader(c.stdin), &stderr); code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "twr-claude: paste the token from `claude setup-token`, then Enter\n") {
				t.Errorf("stderr %q: no prompt", stderr.String())
			}
			if got := applied(t, dir).Data["token"]; got != tokenInSecret {
				t.Errorf("kubectl received %q, want the first line alone", got)
			}
		})
	}
}

func TestLoginFromStdinRefusesWhatIsNotAToken(t *testing.T) {
	// The browser's authorisation code, and a token with the code's tail.
	for _, line := range []string{"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6#9f8e7d6c-5b4a-3c2d-1e0f-a9b8c7d6e5f4", token + "#state"} {
		t.Setenv("STUB_DIR", t.TempDir())
		dir := os.Getenv("STUB_DIR")
		cfg := secretConfig(t, dir, kubectlStub)
		var stderr bytes.Buffer
		if code := Login(t.Context(), cfg, true, strings.NewReader(line+"\n"), &stderr); code != ExitFailed {
			t.Errorf("%q: exit %d, want %d", line, code, ExitFailed)
		}
		if !strings.Contains(stderr.String(), "that is not a setup-token token (they start sk-ant-oat01-); the browser's code goes to `claude setup-token`'s own prompt") {
			t.Errorf("%q: stderr %q", line, stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "kubectl.args")); err == nil { //nolint:gosec // the test's own temp dir
			t.Errorf("%q: kubectl was run", line)
		}
	}
}

func TestLoginFailures(t *testing.T) {
	t.Setenv("STUB_DIR", t.TempDir())
	dir := os.Getenv("STUB_DIR")
	t.Run("no token in the output", func(t *testing.T) {
		cfg := secretConfig(t, dir, kubectlStub)
		cfg.Claude = stub(t, dir, "claude", "echo 'nothing here'\n")
		var stderr bytes.Buffer
		if code := Login(t.Context(), cfg, false, strings.NewReader(""), &stderr); code != ExitFailed || !strings.Contains(stderr.String(), "--from-stdin") {
			t.Errorf("exit %d: %s", code, stderr.String())
		}
	})
	t.Run("setup-token fails", func(t *testing.T) {
		cfg := secretConfig(t, dir, kubectlStub)
		cfg.Claude = stub(t, dir, "claude", "exit 3\n")
		if code := Login(t.Context(), cfg, false, strings.NewReader(""), &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("no claude", func(t *testing.T) {
		cfg := secretConfig(t, dir, kubectlStub)
		cfg.Claude = filepath.Join(dir, "no-claude")
		var stderr bytes.Buffer
		if code := Login(t.Context(), cfg, false, strings.NewReader(""), &stderr); code != ExitFailed || !strings.Contains(stderr.String(), "claude setup-token:") {
			t.Errorf("exit %d: %s", code, stderr.String())
		}
	})
	t.Run("an empty stdin", func(t *testing.T) {
		cfg := secretConfig(t, dir, kubectlStub)
		if code := Login(t.Context(), cfg, true, strings.NewReader("\n"), &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("kubectl cannot render", func(t *testing.T) {
		cfg := secretConfig(t, dir, "exit 1\n")
		if code := Login(t.Context(), cfg, true, strings.NewReader(token), &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("kubectl renders something else", func(t *testing.T) {
		cfg := secretConfig(t, dir, "echo '[]'\n")
		if code := Login(t.Context(), cfg, true, strings.NewReader(token), &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("apply fails", func(t *testing.T) {
		cfg := secretConfig(t, dir, strings.Replace(kubectlStub, "*apply*) cp", "*apply*) exit 1; cp", 1))
		if code := Login(t.Context(), cfg, true, strings.NewReader(token), &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
}

func TestStatus(t *testing.T) {
	t.Setenv("STUB_DIR", t.TempDir())
	dir := os.Getenv("STUB_DIR")
	secret := func(issued string) string {
		return `echo '{"metadata":{"name":"twr-claude-token","creationTimestamp":"2026-01-01T00:00:00Z","annotations":{"` + IssuedAtAnnotation + `":"` + issued + `"}},"data":{"token":"c2VjcmV0"}}'` + "\n"
	}
	t.Run("fresh", func(t *testing.T) {
		cfg := secretConfig(t, dir, secret("2026-10-01T00:00:00Z"))
		var stdout bytes.Buffer
		if code := Status(t.Context(), cfg, &stdout, &bytes.Buffer{}); code != ExitOK {
			t.Errorf("exit %d", code)
		}
		if got := stdout.String(); !strings.Contains(got, "issued 2026-10-01, 36 days old") || strings.Contains(got, "c2VjcmV0") || strings.Contains(got, "secret") {
			t.Errorf("stdout %q", got)
		}
		if got := read(t, filepath.Join(dir, "kubectl.args")); !strings.Contains(got, "get\nsecret\ntwr-claude-token\n-o\njson") {
			t.Errorf("args %q", got)
		}
	})
	t.Run("old", func(t *testing.T) {
		cfg := secretConfig(t, dir, secret("2025-11-01T00:00:00Z"))
		var stdout bytes.Buffer
		if code := Status(t.Context(), cfg, &stdout, &bytes.Buffer{}); code != 4 || !strings.Contains(stdout.String(), "older than 330 days") {
			t.Errorf("exit %d: %s", code, stdout.String())
		}
	})
	t.Run("no annotation falls back to the creation time", func(t *testing.T) {
		cfg := secretConfig(t, dir, `echo '{"metadata":{"creationTimestamp":"2026-11-01T00:00:00Z"},"data":{"token":"x"}}'`+"\n")
		var stdout bytes.Buffer
		if code := Status(t.Context(), cfg, &stdout, &bytes.Buffer{}); code != ExitOK || !strings.Contains(stdout.String(), "created 2026-11-01, 5 days old") {
			t.Errorf("exit %d: %s", code, stdout.String())
		}
	})
	t.Run("missing", func(t *testing.T) {
		cfg := secretConfig(t, dir, "echo 'Error from server (NotFound)' >&2\nexit 1\n")
		if code := Status(t.Context(), cfg, &bytes.Buffer{}, &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("the wrong key", func(t *testing.T) {
		cfg := secretConfig(t, dir, `echo '{"metadata":{},"data":{"other":"x"}}'`+"\n")
		if code := Status(t.Context(), cfg, &bytes.Buffer{}, &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
	t.Run("not JSON", func(t *testing.T) {
		cfg := secretConfig(t, dir, "echo nope\n")
		if code := Status(t.Context(), cfg, &bytes.Buffer{}, &bytes.Buffer{}); code != ExitFailed {
			t.Errorf("exit %d", code)
		}
	})
}

func TestSecretConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cfg, err := SecretConfigFromEnv(env(map[string]string{"TWR_CLAUDE_WARN_AFTER": "720h", "TWR_CLAUDE_KUBE_CONTEXT": "my-cluster"}))
	if err != nil || cfg.Namespace != "twr-agents" || cfg.Name != "twr-claude-token" || cfg.Key != "token" || cfg.WarnAfter != 720*time.Hour || cfg.Context != "my-cluster" {
		t.Errorf("%+v %v", cfg, err)
	}
	if _, err := SecretConfigFromEnv(env(map[string]string{"TWR_CLAUDE_WARN_AFTER": "never"})); err == nil {
		t.Error("no error")
	}
}
