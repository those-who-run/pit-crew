# Pit crew

The no-model machinery of the Those Who Run merge ladder: the poller that picks
work, the fetch step that prepares a checkout, the publish steps that check an
agent's output before anything reaches GitHub, and `twr-claude`, the wrapper that
runs Claude Code on the maintainer's subscription. It also holds the runner image
the agent step runs in.

Issues are off in this repository. Every issue, the roadmap and the decisions
(ADRs) live in [those-who-run/paddock](https://github.com/those-who-run/paddock). A pull request here
names its paddock issue: `Closes those-who-run/paddock#N.` when it meets every
acceptance criterion, `Part of those-who-run/paddock#N` otherwise
([`AGENTS.md`](AGENTS.md)).

## The ladder

Claude agents write the code and open the pull requests; each pull request climbs
the same rungs, and the maintainer is the last gate on every one. `pit-crew` runs
the rungs that need no model, and starts the ones that do in a sandbox where the
model holds no GitHub credential.

| Rung | What | Who | Here |
|---|---|---|---|
| 0 Spec | An issue triaged, then labelled `agent-ready` | The maintainer | The poller checks the label event's actor, and that `needs triage` and `hold` are absent |
| 1 Build | One issue into one pull request, with the shared skills | A build Job, as `twr-builder` | `pit-crew fetch`, `twr-claude run`, `pit-crew publish-build` |
| 2 CI | The repository's required checks | GitHub Actions | Read, never set |
| 3 Review | An independent read at the head SHA | A review Job, as `twr-reviewer` | `pit-crew fetch`, `twr-claude run`, `pit-crew publish-review`: one marker comment per head and the status `review / verdict` |
| 3b Fix | The builder answers each item, the reviewer reads again | A fix Job | Two rounds at most, counted from markers; then `ladder / stuck` |
| 4 Ready | Every required check green and the verdict `ready` or `nits` at the head | `pit-crew ready` | The status `ladder / ready` and the ready list |
| 5 Merge | Approve, queue, land | The maintainer | Nothing here touches a merge |

Three rules hold throughout, and the code enforces what it can:

- **The model never holds a write credential, and the step that writes never
  holds a model.** `twr-claude run` refuses to start with a GitHub token in its
  environment; the publish steps read the model's files and nothing else of it.
- **State lives on GitHub.** Markers in comments, commit statuses and labels; the
  poller keeps no database and survives any restart.
- **Everything read from GitHub is data.** A marker, a status or a label event
  counts only when the right account wrote it. A stranger who pastes a marker into
  a comment changes nothing ([`internal/ladder`](internal/ladder/ladder.go)).

## What is here

| Package or command | Does |
|---|---|
| [`internal/lanes`](internal/lanes/lanes.go) | The lane of a change (`light`, `standard`, `sensitive`) from its paths: a port of `agent/lanes.py` in `those-who-run/.github`, reading the same `lanes.json` |
| [`internal/ladder`](internal/ladder/ladder.go) | The decisions: review, fix, stuck, ready for a pull request, build for an issue; the markers and the status names. Pure functions |
| [`internal/publish`](internal/publish/publish.go) | What a publisher refuses: a moved head, a comment over 15 KB, a stray marker, a mention, a private value, a commit not the builder's, a protected path, a weakened test, a body off the template. Pure functions |
| [`internal/privatescan`](internal/privatescan/privatescan.go) | The private-value scan: paddock's rules ported, plus home paths and credential shapes, plus the lab's own patterns at run time |
| [`internal/apptoken`](internal/apptoken/apptoken.go) | An hour-long GitHub App installation token, scoped to named repositories and permissions |
| [`internal/github`](internal/github/client.go) | The REST calls the steps need, with retries while a pod's network comes up and on rate limits |
| [`internal/kube`](internal/kube/kube.go) | Create and list Jobs, read a container's exit code, as the pod's service account |
| [`internal/poller`](internal/poller/poll.go) | One poll: read, decide, start at most one Job within the limits, heartbeat; and the ready pass |
| `cmd/pit-crew` | `poll`, `fetch`, `publish-review`, `publish-build`, `ready`, `lane` |
| `cmd/twr-claude` | `login`, `status` (the maintainer, on his own machine) and `run` (the agent step) |

[`docs/jobs.md`](docs/jobs.md) is the contract the Kubernetes manifests are written
from: each step's command line, environment, mounts, Secrets, exit codes, the Job
template's placeholders, the poller's RBAC and each step's egress.
[`docs/twr-claude.md`](docs/twr-claude.md) is the wrapper: what was checked against
the Claude Code CLI and what was not. [`docs/decisions-2026-11-06.md`](docs/decisions-2026-11-06.md)
records the choices made while this was built unattended, for review.

## Checks

A new clone or worktree runs `make prereqs` first: it checks that every tool
`make check` and `make hooks` need is there, and says how to fix any that isn't.
Then `make hooks` installs the commit hooks. `make check` runs what CI runs;
[`AGENTS.md`](AGENTS.md) says what that is.

## Conventions

The ones every Go component shares, from the
[`chassis`](https://github.com/those-who-run/chassis) template:

- **Configuration from the environment**, read once at start-up into a typed
  struct; an unset variable takes its default and a value that can't be read
  stops the process. The defaults do nothing: with no allowlist nobody is
  reviewed, with no maintainer nothing is built, and the poller starts no Job
  until `POLL_START_JOBS=true`.
- **Logging with `log/slog`, as JSON** on standard output, one object per line,
  with key-value pairs. A log line never carries a token, a key or a value the
  private scan found.
- **Standard library only.** JWT signing, the GitHub REST API and the Kubernetes
  Jobs API are small enough to do without a dependency, and a public repository
  that runs with a credential is better off with fewer of them.

Unlike the other components, `pit-crew` serves no health endpoint: its commands
run to completion in Jobs and a CronJob, and the poller's heartbeat is how it says
it is alive.

## From the template

This repository was made from `chassis` with `copier copy`, and
`.copier-answers.yml` records the release it came from and the answers given.
Renovate proposes each new `chassis` release as a PR that runs
`copier update`; chassis's README has the rest.

## Releases

Merging the release PR tags `vX.Y.Z`, and the release workflow publishes two
images once the maintainer approves it in the `release` environment:
`ghcr.io/those-who-run/twr-pit-crew:X.Y.Z` (the Go binaries, git and CA
certificates, for the no-model steps) and
`ghcr.io/those-who-run/twr-pit-crew-runner:X.Y.Z` (the agent step: Claude Code,
git, Go, Python with uv, Node, buf, kustomize and kubeconform; no container
runtime).
