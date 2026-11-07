# twr-claude: the Claude login wrapper

Claude runs on the maintainer's subscription, through a long-lived token that
`claude setup-token` makes, never through an API key. `twr-claude` has two halves:

- `twr-claude login` and `twr-claude status`, which the maintainer runs on his
  own machine, with his own `kubectl` context, to put the token in the cluster's
  Secret and to ask how old it is;
- `twr-claude run`, the agent step's entrypoint in the runner image, which gives
  the token to the `claude` process alone and starts it headless.

## `login`

Runs `claude setup-token`, which opens the browser authorisation flow and prints
the token; the wrapper reads the token from that process's output and writes it
straight into the Secret with `kubectl create secret generic ... --from-file`
through a pipe, so it never reaches the terminal, the shell history or a file. It
annotates the Secret with `pit-crew/issued-at`, so `status` and the heartbeat can
say how old the token is. `--from-stdin` reads a token pasted on standard input
instead, for when the CLI's output cannot be captured.

Nothing about `login` has been run for real: `claude setup-token` needs the
subscription and a browser, and this was built unattended. What is tested is the
wrapper's handling with a stub `claude` and a stub `kubectl` on `PATH`: the token
goes to `kubectl`'s standard input and nowhere else, and the Secret's name,
namespace, key and annotation are what the manifests expect.

## `status`

Reads the Secret's metadata with `kubectl get secret -o json` and prints its age
and the date it was issued, never its value. It warns past a configurable age,
since a `setup-token` token lasts a year.

## `run`

Reads the token from the mounted file, clears `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN`, every `GH_*` and `GITHUB_*` token and the cloud-provider
switches from the environment, exports the token as `CLAUDE_CODE_OAUTH_TOKEN` for
the `claude` process only, and starts:

```
claude -p --restricted --strict-mcp-config --permission-mode dontAsk \
  --permission-prompts none --tools <list> --max-turns <n> \
  --output-format json --no-session-persistence --add-dir <dirs>
```

with the brief and the task statement as the prompt on standard input. It refuses
to start at all if a GitHub token variable is set: the model's step never holds a
write credential, and this is where that is enforced.

`TWR_CLAUDE_ENV_SCRUB` (`1` by default) decides what `run` sets
`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` to, and `run` always sets it, so the CLI's own
default never decides. On, the CLI strips the token from the environment of the
commands the model runs; that needs bubblewrap and user namespaces, which the
runner image and the Job's seccomp profile do not give it, so the CLI refused to
start on the first Job. With `TWR_CLAUDE_ENV_SCRUB=0` the CLI starts without
that isolation, and `run` says so once on standard error before starting:
`twr-claude: subprocess env scrub is off (TWR_CLAUDE_ENV_SCRUB=0): commands the
model runs can read its environment, including the token`. A value that is not
`1`, `true`, `yes`, `0`, `false` or `no` is a configuration error (exit 2).

Exit codes: 0 the run finished; 2 configuration; 3 refused to start; 70 the login
has expired or the token is invalid; 75 the plan's usage limit was reached; 71 the
turn limit; 72 the time limit; 1 the run failed otherwise. The poller backs off on
70 and 75.

## What was checked, and what was not

Checked against the installed CLI (`claude --help`, version 2.1.319) and the
official documentation on 2026-11-06:

- The token's variable is `CLAUDE_CODE_OAUTH_TOKEN`; `ANTHROPIC_API_KEY` and
  `ANTHROPIC_AUTH_TOKEN` take precedence over it, which is why `run` clears
  them. `setup-token` prints the token to the terminal and saves it nowhere
  (documentation, and the local help's one-line description).
- `--bare` never reads OAuth credentials, so it cannot use the subscription
  (documentation, and the local help). The isolation it gave is rebuilt with
  `--restricted` (ignores user, project and local settings; removes Bash and the
  other code-running tools unless `--tools` names them; confines the file tools
  to the working directories) and `--strict-mcp-config` (no MCP servers but the
  ones on the command line). Both are in the local help.
- `--permission-mode dontAsk` is a choice in the local help; the documentation
  says it denies every call that would prompt. `--permission-prompts none` is in
  the local help and denies anything that would prompt in print mode.
- `--tools` takes a comma-separated list of built-in tool names (local help and
  documentation).
- `--output-format json` gives one result object with `subtype`, `is_error`,
  `num_turns`, `duration_ms` and `total_cost_usd` (documentation of the SDK's
  result message; the CLI's own page says less).
- The documented error strings for an expired login ("Login expired",
  "OAuth token revoked", "OAuth token has expired", "Invalid authentication
  credentials", 401) and for the usage limit ("You've hit your session limit",
  "weekly limit", "Opus limit", "Request rejected (429)") are what `run` maps to
  exit codes 70 and 75.
- `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1` strips credentials from the environment
  of the commands the model runs (documentation). Whether it strips
  `CLAUDE_CODE_OAUTH_TOKEN` by name is not stated; a run with the scrub on checks
  that `env` run by the model does not show the token. Seen on the first Job:
  with it on and no bubblewrap, the CLI exits at once with `Claude Code could not
  start: bubblewrap is required for subprocess env scrubbing and isolation`, and
  its message names `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0` as the way to disable
  it, which `TWR_CLAUDE_ENV_SCRUB=0` sets.
- Claude Code with a subscription token needs `api.anthropic.com`,
  `platform.claude.com` and `claude.ai` (the network documentation).

Not checked, because it needs a real session or the subscription:

- `--max-turns` is in the CLI reference but not in the local `--help` of
  2.1.319. If the installed version refuses it, `run` exits 1 at once and the
  first real run will show it.
- The exact exit codes and result `subtype` values of a headless run that hits
  an expired token or the usage limit. The documentation says the CLI exits
  non-zero and prints the failure as the result; `run` reads both the exit code
  and the result's text.
- Whether `claude setup-token` needs a terminal. If it does, `login --from-stdin`
  is the way.
- The subscription's terms for unattended use: the plan's note, and the
  maintainer's to check.
