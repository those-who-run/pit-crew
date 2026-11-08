# The Jobs: what the manifests must provide

The contract between `pit-crew` and the Kubernetes manifests that run it, which
live in the maintainer's private repository. Everything a manifest needs is here:
each step's command line, environment, mounts and Secrets (names and keys, never
values), what each step reads and writes on the shared volumes, its exit codes, the
Job template's placeholders, the poller's RBAC, each step's egress, and suggested
limits. The manifests are written from this page, so it is kept true as the code
changes; where the two disagree, the code is wrong or this page is, and a test is
missing.

Nothing on this page has run end to end. See [What the first real run must
prove](#what-the-first-real-run-must-prove).

## The shape

One CronJob, the poller, with no model in it. It reads GitHub, decides what is due
([the ladder](../README.md#the-ladder)), and creates one Job per unit of work from a
template the manifests give it. Each Job is three containers, run one after the
other:

```
initContainers:
  fetch    (twr-pit-crew image, reviewer App key)    pit-crew fetch
  agent    (twr-pit-crew-runner image, Claude token)  twr-claude run
containers:
  publish  (twr-pit-crew image, reviewer or builder App key)
           pit-crew publish-review | pit-crew publish-build
```

- The agent container has no GitHub credential; `twr-claude run` refuses to start
  if one is in its environment. The publish container has no model.
- `fetch` writes the data volume and the checkout; the agent mounts the data volume
  read-only, so what `publish` reads back about the task is what `fetch` wrote.
- Each step mints its own hour-long installation token from the App key mounted
  in that step only.
- A failed `fetch` or `agent` container fails the Job before `publish` runs, so a
  review that did not happen posts nothing.

## Images

| Image | Holds | User |
|---|---|---|
| `ghcr.io/those-who-run/twr-pit-crew` | `/pit-crew` and `/twr-claude`, git, CA certificates | 65532 (`nonroot`) |
| `ghcr.io/those-who-run/twr-pit-crew-runner` | `twr-claude`, Claude Code, git, Go, Python with uv, Node, buf, kustomize, kubeconform; no container runtime | 65532 (`agent`, home `/home/agent`) |

Both run with a read-only root file system; each container needs an emptyDir at
`/tmp`, and the agent one at `/home/agent` too. Pin both by digest in the
manifests. The small image was built once and passed its smoke test; the runner
image has not been built: it installs several toolchains, and the night's load
rules allowed no container build that is not small. CI's `runner / build` is its
first build.

Both images set `GIT_CONFIG_GLOBAL=/etc/pit-crew/gitconfig`, a file they ship
that marks `/work/repo` and `/work/agent` as `safe.directory`. The two are
emptyDir mount points, owned by root, and `git clone` into an existing directory
keeps it as the repository root, so git running as 65532 refused them on the
first Job (`fatal: detected dubious ownership in repository at '/work/agent'`).
`pit-crew fetch` also passes the same two entries to its own git commands through
the environment, so its clones work whatever the image's configuration; the file
is for every git the agent and publish steps run after it. A manifest that
mounted its own gitconfig ConfigMap at that path and set `GIT_CONFIG_GLOBAL` as an
interim can drop both once it runs these images. Both images also set
`GIT_CONFIG_NOSYSTEM=1`, so git reads this one file and never `/etc/gitconfig`.

## Volumes

All emptyDir, created per Job, sized with `sizeLimit`.

| Mount | fetch | agent | publish | Holds |
|---|---|---|---|---|
| `/work/data` | read-write | read-only | read-only | The task's facts, written by `fetch` |
| `/work/repo` | read-write | read-write | read-only | The checkout the agent works in |
| `/work/agent` | read-write | read-only | read-only | `those-who-run/.github` at the pinned release: briefs, skills, `agent/lanes.json` |
| `/work/out` | read-write (creates it, writes nothing) | read-write | read-only | What the agent wrote for publishing |
| `/hold` | (none) | (none) | read-write | What a publisher held back for the maintainer (a volume the lab syncs to the vault inbox) |

The read-only mounts on the agent container are what makes `/work/data` and
`/work/agent` trustworthy to `publish`: the model cannot change the recorded head
SHA, the lane, the issue text or its own brief.

## Secrets

Made by hand with `kubectl`, one per step, as the lab's other services are. Names
are suggestions; the manifests choose.

| Secret | Key | Mounted at | In |
|---|---|---|---|
| `twr-reviewer-key` | `private-key.pem`, `client-id` | the key at `/secrets/app/private-key.pem`; the client ID as `APP_CLIENT_ID` through `secretKeyRef` | `fetch`, `publish-review`, the poller, `ready` |
| `twr-builder-key` | `private-key.pem`, `client-id` | the same | `publish-build` |
| `twr-claude-token` | `token` | `/secrets/claude/token` | `agent` |
| `twr-private-patterns` (optional) | `patterns`, `allow` | `/etc/pit-crew/private/` | both publishers, wherever `PUBLISH_PATTERNS_FILE` or `PUBLISH_ALLOW_FILE` names a path in it |

`twr-claude login` writes `twr-claude-token` with `kubectl`, with an annotation
`pit-crew/issued-at` carrying the date; `twr-claude status` reads the annotation
and says the token's age, never its value.

## Common environment

Every `pit-crew` step reads these.

| Variable | Default | Meaning |
|---|---|---|
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | `json` | `json` (one object per line) or `text` (slog's `key=value` form, for reading a Job's log by eye); the same records either way |
| `GITHUB_API_URL` | `https://api.github.com` | The API |
| `APP_CLIENT_ID` | (required where a token is minted) | The App's client ID |
| `APP_KEY_FILE` | (required) | The App's private key, PEM |
| `APP_INSTALLATION_ID` | (looked up) | The installation on the organisation |
| `LADDER_OWNER` | `those-who-run` | The organisation |
| `LADDER_MAINTAINER` | (empty: nobody) | The maintainer's login; only his label starts a build or asks for a review again |
| `LADDER_REVIEWER` | `twr-reviewer[bot]` | The review poster's login; its markers and statuses count |
| `LADDER_BUILDER` | `twr-builder[bot]` | The build publisher's login |
| `LADDER_AUTHORS` | (empty: nobody) | Comma-separated logins whose pull requests are reviewed and who may author a commit in one |
| `LADDER_COMMITTERS` | same as authors | Logins that may commit one (add `web-flow` for GitHub's web merges) |
| `LADDER_HOLD_LABEL` | `hold` | |
| `LADDER_READY_LABEL` | `agent-ready` | |
| `LADDER_TRIAGE_LABEL` | `needs triage` | |
| `LADDER_REREVIEW_LABEL` | `re-review` | On a pull request, applied by the maintainer after the review record at the head: the head counts as unreviewed again (the poller); `publish-review` takes it off once it has posted or held |
| `LADDER_ISSUE_REPO` | `those-who-run/paddock` | Where issues live |
| `LADDER_COMPONENT_PREFIX` | `component: ` | The label prefix that names a repository |
| `LADDER_COMPONENTS` | (empty) | `label=owner/name` pairs, comma-separated, where the name alone does not say; an empty value means never built by a Job |
| `LADDER_BUILD_REPOS` | (empty: no builds) | Repositories a build may land in, comma-separated |
| `LADDER_REPOS` | the installation's | Repositories to poll for pull requests |

With nothing set, nothing qualifies: an empty allowlist reviews nobody and builds
nothing.

Each step mints its token with only the permissions it uses, which the App must
have been granted:

| Step | App | Permissions asked for |
|---|---|---|
| `poll` | reviewer | metadata, contents, pull requests, issues: read |
| `ready` | reviewer | metadata, contents, pull requests: read; statuses: write |
| `fetch` | reviewer | metadata, contents, pull requests, issues: read |
| `publish-review` | reviewer | metadata: read; pull requests, statuses: write |
| `publish-build` | builder | metadata: read; contents, pull requests: write |

A token for `poll` and `ready` covers every repository of the installation; the
others are scoped to the repositories the step touches.

A pull request's labels, label events and comments are read and written through
the issues endpoints (`/repos/{owner}/{repo}/issues/{number}/…`), which GitHub
grants to the Pull requests permission for a pull request: reading a pull
request's timeline for the `re-review` label's event needs pull requests: read,
which `poll` and `ready` have, and removing the label
(`DELETE /repos/{owner}/{repo}/issues/{number}/labels/{name}`) needs pull
requests: write, which `publish-review` has. Issues: write is not asked for. Should
GitHub refuse the removal all the same, `publish-review` logs a warning and its
exit code stands; the review is posted and its record is newer than the label's
event, so the label re-opens nothing until the maintainer applies it again.

## The poller: `pit-crew poll`

Runs on a schedule (every 5 minutes is the plan's figure). Reads GitHub and the
Jobs in its namespace, decides, and creates at most one Job per run. It writes
nothing to GitHub.

| Variable | Default | Meaning |
|---|---|---|
| `POLL_START_JOBS` | `false` | Until `true`, the poller logs its decisions and creates nothing |
| `POLL_JOB_TEMPLATES` | (required to start anything) | A directory with `review.json`, `build.json` and `fix.json`; a kind with no file is never started |
| `POLL_NAMESPACE` | the pod's own | Where Jobs are created and counted |
| `POLL_MAX_ACTIVE` | `1` | Start nothing while this many Jobs are active |
| `POLL_DAILY_CAP` | `12` | Start nothing once this many Jobs were created in the last 24 h |
| `POLL_QUIET` | (none) | Quiet windows, `HH:MM-HH:MM` comma-separated, in `POLL_TZ`; a window may cross midnight |
| `POLL_TZ` | `UTC` | The quiet windows' time zone |
| `POLL_BACKOFF_PLAN_LIMIT` | `3h` | Start nothing for this long after an agent exited with the plan limit reached |
| `POLL_BACKOFF_LOGIN_EXPIRED` | `6h` | The same for a login that has expired |
| `POLL_IDLE_GATE_URL` | (none) | GET it before starting: 200 means idle; anything else, start nothing |
| `POLL_IDLE_GATE_FIELD` | (none) | With it set, idle needs 200 **and** that top-level field of the JSON body being boolean `true`; a missing field, another type, a body that is not a JSON object, a non-200 or a transport error all mean live. A health endpoint answers 200 during a race too: ecu's `/healthz` says `"idle": true` or `false`, so `POLL_IDLE_GATE_URL=http://ecu.<namespace>.svc:8080/healthz` with `POLL_IDLE_GATE_FIELD=idle` is the gate the plan means |
| `POLL_IDLE_GATE_CMD` | (none) | Run it instead (or as well): exit 0 means idle |
| `POLL_HEARTBEAT_URL` | (none) | POST the run's summary as JSON |
| `POLL_HEARTBEAT_FILE` | (none) | Write the same summary to a file |
| `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT` | set in a pod | The API server; the service account token and CA come from `/var/run/secrets/kubernetes.io/serviceaccount/` |
| `POLL_KUBE_API`, `POLL_KUBE_TOKEN_FILE`, `POLL_KUBE_CA_FILE` | (in-cluster values) | To run it outside a pod |

The gates, in order: quiet window, back-off, daily cap, active Jobs, idle gate.
A gate that cannot be read (the idle gate's URL down, the Jobs list refused)
closes: the poller starts nothing and says so in the heartbeat.

What is due on a pull request in the ladder, decided from what GitHub holds
([`internal/ladder`](../internal/ladder/pr.go)):

- `review` when there is no review record at the head (reason
  `no-review-at-head`), when the builder answered since the last one
  (`answered-since-review`), or when the pull request carries the
  `re-review` label, the event that applied it is the maintainer's
  (`LADDER_MAINTAINER`) and newer than the newest review record at the head
  (`re-review`). The label counts whatever the record says, held or a verdict,
  so the maintainer can ask for a second look after a bad review; the label from
  any other account, or one applied before the record, changes nothing. The
  label's event is read from the pull request's timeline, and only when the
  label is on;
- `fix` or `stuck` when the verdict is `changes` on a builder's pull request;
- `ready` when the verdict is `ready` or `nits` and every required check is
  green;
- otherwise nothing, with the reason (`verdict-held`, `checks-pending`, …).

### Job names and idempotency

Job names are deterministic, so a unit of work is tried once per Job lifetime:

- review: `review-<repo>-<number>-<sha8>-f<rounds>` (the rounds answered so far,
  so a builder's answer at the same head gets a new review), with `-a<asks>`
  appended when the review was asked for again with the `re-review` label (the
  times the maintainer has applied it to the pull request), since the first
  review's Job at that head is still there for its TTL;
- build: `build-<repo>-<issue>`;
- fix: `fix-<repo>-<number>-r<round>`.

`<repo>` is the repository name, lower-cased, with anything but letters, digits
and hyphens dropped. A Job that already exists is "already started", not an error.
The template's `ttlSecondsAfterFinished` is the retry interval for a failed unit;
the daily cap counts Jobs by `creationTimestamp`, so the TTL must be at least
86,400 s (a day) for the cap to hold, and 172,800 s (two days) is suggested.

### The Job template

A JSON Job (the API form; a kustomize ConfigMap may hold it), one per kind;
[`examples/review.json`](../examples/review.json) is one for the review kind. The
poller parses it, replaces placeholders in every string value, and adds labels and
annotations. Placeholders, written `{{name}}`:

| Placeholder | Value |
|---|---|
| `{{job_name}}` | The name above |
| `{{kind}}` | `review`, `build` or `fix` |
| `{{repo}}` | `owner/name` |
| `{{owner}}`, `{{name}}` | Its two halves |
| `{{number}}` | The pull request number (review, fix); empty for a build |
| `{{head_sha}}` | The head reviewed or fixed; empty for a build |
| `{{base_ref}}` | The base branch |
| `{{issue_repo}}`, `{{issue}}` | The issue a build or fix comes from; empty for a review |
| `{{round}}` | The fix round (fix); empty otherwise |

The poller sets `metadata.name` to the Job name, `metadata.namespace`, and adds the
labels `app.kubernetes.io/managed-by: pit-crew` and `app.kubernetes.io/component:
<kind>` and the annotations `pit-crew/repo`, `pit-crew/number`, `pit-crew/head-sha`,
`pit-crew/issue`; it reads them back to count and to find the agent container's
exit code. The agent container must be named `agent` for the exit code to be
found.

### Heartbeat

One JSON object per run, logged, POSTed to `POLL_HEARTBEAT_URL` and written to
`POLL_HEARTBEAT_FILE` when set:

```json
{"at": "2026-11-06T09:00:00Z", "pulls": 14, "issues": 3,
 "decisions": {"review": 1, "fix": 0, "ready": 2, "none": 11, "build": 0},
 "reasons": {"author-not-allowlisted": 6, "no-review-at-head": 1, "verdict-and-checks": 2},
 "started": "review-ecu-31-1f0c5d2e-f0", "gate": "", "active": 0, "today": 3,
 "plan_limit_until": "", "login_expired_until": "", "errors": 0,
 "github_unavailable": false}
```

`gate` names the gate that stopped a start (`quiet`, `backoff`, `cap`, `active`,
`idle`, `dry-run`), or is empty. The maintainer's alerting reads it; the ladder
does not.

`github_unavailable` is `true` for a run that read nothing because GitHub was
down: a call answered 5xx, or timed out, after the client's retries (five, with
backoff, for a read; none for the token's mint). That covers the first call,
`GET /rate_limit`, the token's mint and the first list. Such a run logs a
warning, writes this heartbeat with `errors: 1` and nothing else counted, starts
nothing, and exits 0, so a GitHub incident does not fail the CronJob's Job; the
next scheduled run tries again. Alerting that should notice an outage that
lasts reads this field.

Exit codes of `pit-crew poll`: 0 the run finished, GitHub being unavailable
included; 2 configuration, a missing App key, or GitHub refusing the App (a 4xx
while minting the token or on the first call); 1 anything else that stopped the
run (a 4xx listing the installation's repositories or the issues, for one).

### RBAC

A ServiceAccount for the poller alone, with a Role in the agents' namespace:

| Resource | Verbs | Why |
|---|---|---|
| `batch/jobs` | `create`, `get`, `list` | Start work; count active Jobs and today's |
| `pods` | `list` | Read the agent container's exit code from finished Jobs' pods |

Nothing else, and no cluster-wide binding. Every other container runs with
`automountServiceAccountToken: false`.

## The fetch step: `pit-crew fetch`

Clones the repository at the recorded head, writes what the agent needs as data
files, works out the lane, and prepares the working tree.

| Variable | Meaning |
|---|---|
| `FETCH_KIND` | `review`, `build` or `fix` |
| `FETCH_REPO` | `owner/name` |
| `FETCH_NUMBER` | The pull request (review, fix) |
| `FETCH_HEAD_SHA` | The head the poller saw; the step stops with exit 11 if the pull request's head is now another |
| `FETCH_ISSUE_REPO`, `FETCH_ISSUE` | The issue (build; for a fix, read from the pull request body) |
| `FETCH_ROUND` | The fix round (fix) |
| `FETCH_AGENT_REPO` | `those-who-run/.github` |
| `FETCH_AGENT_REF` | The release tag of `.github` to read briefs and skills from (required) |
| `FETCH_DIR` | `/work` |
| `FETCH_BUILDER_NAME`, `FETCH_BUILDER_EMAIL` | The git identity set in the checkout for a build or fix |
| `FETCH_BASE_REF` | The base branch for a build, `main` by default |
| `FETCH_GIT_BASE` | What clone URLs start with, `https://github.com/` by default; the tests point it at local repositories |

The App token is used for the clone through git's environment, never written to
`.git/config`; a test proves the checkout holds no token.

What it writes under `/work/data`:

| File | Holds |
|---|---|
| `task.json` | kind, repo, number, head and base SHAs, base ref, lane, issue, round, branch, the `.github` ref and its SHA, paths |
| `task.md` | The task statement for the agent: the facts above and where everything is, in words; no text from GitHub |
| `pr.json` | The pull request: title, body, author, labels, head, base |
| `commits.json` | Its commits: SHA, author and committer logins and addresses, message |
| `comments.json` | Its conversation comments, oldest first |
| `changes.tsv` | `git diff --name-status base...head` |
| `lane` | `light`, `standard` or `sensitive` |
| `issues/<owner>-<name>-<number>.json` | Each linked issue (from the body's `Closes`/`Part of` lines, or the build's issue) with its comments |
| `pr-files/<path>` | For a review: the pull request's own `CLAUDE.md`, `AGENTS.md`, `CLAUDE.local.md`, `.mcp.json` and `.claude/**` where they differ from the base's, kept as data |
| `swapped.md` | For a review: which steering files the working tree has from the base and not the pull request, and that a check comparing `.claude/` against a pin (`scripts/guard_copy.py`) fails there for that reason alone on a pull request that bumps the pin; `task.md` points the reviewer at it |

The working tree at `/work/repo`:

- review: the head commit checked out, with the base branch's `CLAUDE.md`,
  `AGENTS.md`, `CLAUDE.local.md`, `.mcp.json` and `.claude/` in place of the pull
  request's where they differ (added, replaced or removed as the base has them),
  so a pull request cannot steer its own reviewer. `git diff HEAD` therefore shows
  those files as changed; the diff to review is `git diff <base>...<head>`, both
  SHAs in `task.json`;
- build: the base branch checked out on a new branch `agent/<issue>-<slug>`, the
  slug from the issue title, with `user.name` and `user.email` set to the
  builder's;
- fix: the pull request's branch checked out at its head, identity set the same.

`/work/agent` holds `those-who-run/.github` at `FETCH_AGENT_REF`; the agent's
brief and skills are read from there, never from the checkout.

Exit codes: 0 done; 2 configuration; 11 the head moved; 1 anything else.

## The agent step: `twr-claude run`

The only container with the Claude token, and the only one with a model. Its
command line is fixed by the wrapper; the manifest sets the environment.

| Variable | Default | Meaning |
|---|---|---|
| `TWR_CLAUDE_TOKEN_FILE` | `/secrets/claude/token` | The `setup-token` token, one line |
| `TWR_CLAUDE_BRIEF` | (required) | The brief, as an absolute path: an agent file (`/work/agent/agent/agents/twr-reviewer.md`) or a skill's `SKILL.md`; YAML front matter is stripped |
| `TWR_CLAUDE_TASK` | `/work/data/task.md` | The task statement, appended to the brief as the prompt |
| `TWR_CLAUDE_WORKDIR` | `/work/repo` | Where `claude` runs |
| `TWR_CLAUDE_ADD_DIRS` | `/work/data,/work/agent,/work/out` | Directories the file tools may reach besides the working directory |
| `TWR_CLAUDE_TOOLS` | `Read,Grep,Glob,Write,Bash` | The `--tools` list, given to `--allowedTools` as well |
| `TWR_CLAUDE_MAX_TURNS` | `80` | The turn limit |
| `TWR_CLAUDE_MODEL` | (the CLI's default) | `--model` |
| `TWR_CLAUDE_TIMEOUT` | `25m` | Wall-clock limit on the `claude` process |
| `TWR_CLAUDE_BIN` | `claude` | The CLI |
| `TWR_CLAUDE_ENV_SCRUB` | `1` | `1` (or `true`, `yes`) sets `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1` for the CLI, which keeps the token out of the commands the model runs; `0` (or `false`, `no`) sets it to `0`. Anything else is a configuration error. See below for when it must be `0` |
| `HOME` | `/home/agent` | Must be writable: the CLI keeps its state there |

It starts `claude -p --restricted --strict-mcp-config --permission-mode dontAsk
--permission-prompts none --tools <list> --allowedTools <list> --max-turns <n>
--output-format json --no-session-persistence --add-dir ...` with the prompt on
standard input, the
token as `CLAUDE_CODE_OAUTH_TOKEN` in that process's environment only, and
`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, every `GH_*`/`GITHUB_*` token and the
cloud-provider switches cleared. It sets `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` to `1`
or `0` from `TWR_CLAUDE_ENV_SCRUB` (always set, so the CLI's own default never
decides), `DISABLE_AUTOUPDATER=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and
`CLAUDE_CONFIG_DIR=$HOME/.claude`. It writes `/work/out/agent.json` with the
result's `subtype`, `num_turns`, `duration_ms`, `total_cost_usd` and `is_error`,
and `permission_denials` (an integer, the tool calls the CLI denied) when there
were any; and `/work/out/agent.md`, the model's final text, whenever the CLI
produced a result object.

The scrub needs bubblewrap, and bubblewrap needs user namespaces. The runner
image has no bubblewrap, and the Job's `RuntimeDefault` seccomp profile denies
user namespaces to a container without `CAP_SYS_ADMIN`, so installing it would
not help: with the scrub on, the first Job's agent container stopped at once with
`Claude Code could not start: bubblewrap is required for subprocess env scrubbing
and isolation`. The manifest sets `TWR_CLAUDE_ENV_SCRUB=0` for now, and `run`
then logs one line before starting: `twr-claude: subprocess env scrub is off
(TWR_CLAUDE_ENV_SCRUB=0): commands the model runs can read its environment,
including the token`. With it off, a command the model runs can read the token;
what still bounds that is the NetworkPolicies' egress limits on the agent
container, and that the token can be rotated at any time with `twr-claude
login`.

Exit codes: 0 the run finished (whatever the model concluded); 2 configuration;
3 refused to start (a GitHub token in the environment); 70 login expired or
invalid; 75 the plan's usage limit reached; 71 the turn limit; 72 the time limit;
1 the run failed otherwise.

See the [wrapper's page](twr-claude.md) for what was checked against the CLI and
what was not.

## What the agent writes to `/work/out`

| Kind | Files |
|---|---|
| review | `verdict.json` (always), `comment.md` (kind A always; B to D when something is wrong), `private.md` (only when needed), as the reviewer's brief defines them |
| build | `pr-title.txt` (one line, a Conventional Commit), `pr-body.md` (the template as `twr-pr-ready` writes it, ending with the issue line); the commits are on the branch in `/work/repo` |
| fix | `answer.md` (as `twr-fix-review` writes it, without a marker); `pr-body.md` only when the evidence table changed; the commits on the branch |

`twr-claude run` adds `agent.json`, its own summary of the run, and `agent.md`,
the model's final text.

## The publish steps

### `pit-crew publish-review`

| Variable | Default | Meaning |
|---|---|---|
| `PUBLISH_REPO`, `PUBLISH_NUMBER`, `PUBLISH_HEAD_SHA` | (required) | The same values the Job gave `fetch`; cross-checked against `task.json` |
| `PUBLISH_DIR` | `/work` | |
| `PUBLISH_HOLD_DIR` | (required) | Where a held run's output and `private.md` go |
| `PUBLISH_ALLOW_FILE` | (none) | Optional: more public hosts, in paddock's `private_scan.allow` format. Unset means the built-in allowlist alone |
| `PUBLISH_PATTERNS_FILE` | (none) | Optional: the lab's own patterns, `name regexp` per line. Unset means no extra patterns. A path that is set but missing or unreadable, or a line that does not parse, is a configuration failure (exit 2): nothing is posted |

Reads `/work/out/verdict.json`, `comment.md` and `private.md`. If the head moved:
exit 11, nothing posted. If the output fails a check: nothing of the model's is
posted, every regular file in `/work/out` (`agent.json` and `agent.md` among
them, so a run that wrote no verdict still shows what it said) is copied with
its name, mode 0600, to `/hold/<repo>-<number>-<sha8>/` beside `reasons.md`, the
status `review / verdict` is set to `error` with `held: <codes>`, exit 10.
Otherwise:
the comment is created, or updated when the reviewer already commented on this
head, ending in the marker with the verdict; `review / verdict` is set; exit 0.
`private.md`, when present, always goes to the hold directory and never to GitHub.

The status's state is `success` for `ready`, `nits` and `skipped`, `failure` for
`changes`, and `error` for `blocked` and `held`. Its description starts with the
verdict, and that word, not the state, is what the ladder reads: a `skipped` head
is green but never reaches `ladder / ready`. A `reason` in `verdict.json` goes to
the Job's log and nowhere else, as one value with anything unprintable replaced,
cut at 500 bytes.

A held review stays held: the `held` status is a review record at that head, so
the poller's decision for the pull request is `verdict-held` and no review Job
starts again until the head moves. That is deliberate for a hold on a private
value or a mention, which a second run would repeat. It also holds after an
operational failure (the first Job on the maintainer's cluster held with
`verdict-missing` because the wrapper had not granted the Write tool), where a
second run is what is wanted. For that the maintainer applies the label
`re-review` (`LADDER_REREVIEW_LABEL`) to the pull request: it counts only when
the event that applied it is by the maintainer's login (as `agent-ready` does for
an issue) and is newer than the review record at the head; then the head counts
as unreviewed, the poller starts a review Job again, and this step takes the
label off once it has posted or held, so one label is one more run. The removal
is logged; one GitHub refuses is a warning and leaves the exit code as it was
(see [the permissions](#common-environment)). A head that moved posts nothing
and leaves the label, for the run at the new head to consume.

### `pit-crew publish-build`

| Variable | Default | Meaning |
|---|---|---|
| `PUBLISH_MODE` | (required) | `build` or `fix` |
| `PUBLISH_REPO`, `PUBLISH_ISSUE_REPO`, `PUBLISH_ISSUE` | (required) | |
| `PUBLISH_NUMBER`, `PUBLISH_HEAD_SHA`, `PUBLISH_ROUND` | (fix) | The pull request, the head the review was of, the round |
| `PUBLISH_BUILDER_NAME`, `PUBLISH_BUILDER_EMAIL` | (required) | The identity every commit must carry |
| `PUBLISH_PROTECTED` | `.claude/**,**/CODEOWNERS` | Paths a build may not touch |
| `PUBLISH_BASE_REF` | `main` | The base branch the pull request targets and the push is checked against |
| `PUBLISH_GIT_BASE` | `https://github.com/` | What the push URL starts with |
| `PUBLISH_DIR`, `PUBLISH_HOLD_DIR`, `PUBLISH_ALLOW_FILE`, `PUBLISH_PATTERNS_FILE` | as above | |

It never runs git inside `/work/repo`: that repository's configuration and hooks
are the model's to write. It makes a repository of its own under `/tmp`, reads the
agent's objects through `GIT_ALTERNATE_OBJECT_DIRECTORIES`, reads the branch's
head from the ref file, and does every check there. On pass it pushes the branch
(never with force), then opens the pull request (build) or, for a fix, replaces
the body when the builder rewrote it and posts the answer with its fix marker. A
fix whose pull request head moved since the fetch exits 11 and posts nothing. On refusal: the reasons and the agent's files go to the hold
directory, exit 10. A refused build leaves nothing on GitHub, so the Job's TTL is
its retry interval; the maintainer removes `agent-ready` to stop it.

### `pit-crew ready`

No model, reviewer App. For every open pull request in the ladder: sets
`ladder / ready` to `success` when the verdict at the head is ready or nits and
every required check is green, to `pending` otherwise where it was set before;
sets `ladder / stuck` to `failure` when the fix rounds are used up. Writes the
ready list to `READY_LIST_FILE` (Markdown) when set; the file's volume is the
lab's (the plan puts the list in the vault). Only writes a status that differs
from the one there. Runs on its own schedule, or after `poll`.

### `pit-crew lane`

Not a step: the port of `python3 agent/lanes.py --repo owner/name`, for checking
the classifier by hand. Reads `git diff --name-status` on standard input and
prints the lane; `--config` names the `lanes.json` to use.

## Egress, by host

| Step | Needs |
|---|---|
| poller | `api.github.com`; the Kubernetes API server at the endpoint the in-cluster configuration gives (`KUBERNETES_SERVICE_HOST:PORT`, the `kubernetes.default.svc` ClusterIP, which kube-proxy DNATs to the node's own address and the API server's port, so an egress policy must allow that endpoint, not only the Service); the idle gate (ecu's `/healthz`) and the heartbeat host the lab chooses |
| `ready` | `api.github.com` |
| fetch | `api.github.com`, `github.com` (clone) |
| agent | `api.anthropic.com`, `platform.claude.com` (the token exchange), `claude.ai`; `github.com` only for `git fetch origin main` in a fix (anonymous; the repositories are public); `proxy.golang.org` and `sum.golang.org`, `pypi.org` and `files.pythonhosted.org`, `registry.npmjs.org` for `make check` |
| publish | `api.github.com`, `github.com` (push) |

Every private range denied on egress for all of them. A NetworkPolicy allows by
address, not by name, so until an egress proxy exists the practical policy is
"deny private ranges, allow the internet", as the plan says.

## Resources

For one node with 6 cores and 8 GB shared with other workloads, one Job at a
time:

| Container | CPU request / limit | Memory request / limit |
|---|---|---|
| poller | 50m / 200m | 32Mi / 64Mi |
| fetch | 100m / 500m | 64Mi / 256Mi |
| agent | 500m / 2 | 1Gi / 2.5Gi |
| publish | 100m / 500m | 64Mi / 256Mi |

`activeDeadlineSeconds: 2400` on the Job (the agent's own limit is 25 minutes),
`backoffLimit: 0` (a failed unit is not retried by Kubernetes; the TTL and the
poller decide), `ttlSecondsAfterFinished: 172800`. emptyDir `sizeLimit`: 2Gi for
`/work/repo`, 4Gi for `/home/agent`, 1Gi for `/tmp`, 256Mi for the rest. The
runner image sets no `GOPATH`, `GOCACHE` or `XDG_CACHE_HOME`, so the Go module
and build caches, golangci-lint's cache and pre-commit's environments land under
`$HOME` when the agent runs the tests or `make check`, and `go test` links its
binaries under `/tmp`. At 256Mi for `$HOME`, a review on the maintainer's
cluster was evicted (`Usage of EmptyDir volume "home" exceeds the limit`). The
emptyDirs are on the node's disk, not in memory.

## What the first real run must prove

None of this has been run against a cluster, a GitHub App or a real Claude
session. The first run, with `POLL_START_JOBS=false` first, must show:

1. The poller mints a token, lists the installation's repositories and logs one
   decision per open pull request, with the expected reasons.
2. With `POLL_START_JOBS=true` and only `review.json` in place, exactly one Job
   is created for a pull request by an allowlisted author, and none for a fork, a
   draft or a stranger's.
3. In that Job, `fetch` leaves no token in `/work/repo/.git/config` or anywhere
   on the volumes (`grep -r ghs_ /work` from the agent container finds nothing),
   and the agent container's `env` shows no `GH_TOKEN`, `GITHUB_TOKEN` or
   `ANTHROPIC_API_KEY`.
4. `twr-claude run` starts the CLI with the subscription token: a session that
   reads files and writes `/work/out/verdict.json`. The flags `--restricted`,
   `--strict-mcp-config`, `--permission-prompts none` and `--max-turns` are
   accepted by the installed version (`--max-turns` is in the documentation but not
   in the local `--help` of 2.1.319).
5. A command the model runs (`env`, `cat /proc/self/environ`) does not show the
   token, with `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`. With the scrub off
   (`TWR_CLAUDE_ENV_SCRUB=0`, which the first Job needed: the runner image has no
   bubblewrap and the seccomp profile denies it user namespaces) this cannot
   hold, and the run must not be read as proving it. The controls that remain
   are the NetworkPolicies' egress limits on the agent container (item 8) and
   rotating the token with `twr-claude login`.
6. A revoked token exits 70 and shows in the heartbeat as `login_expired_until`;
   a usage-limit refusal exits 75 and backs the poller off.
7. `publish-review` posts one comment per head with the marker and sets
   `review / verdict`; a planted private value is held and lands in `/hold`.
8. From the agent container, the NAS, NATS, the node and the Kubernetes API are
   unreachable, and `api.anthropic.com` is reachable.
9. No Job starts while the idle gate says a session is live, and the heartbeat
   says `gate: idle`.
10. A build Job's `publish-build` pushes the branch and opens the pull request as
    `twr-builder[bot]`, and a commit authored as anyone else is refused.
