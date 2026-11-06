# Pit crew

The no-model machinery of the Those Who Run merge ladder: the poller that picks work, the fetch step that prepares a checkout, the publish steps that check an agent's output before anything reaches GitHub, and `twr-claude`, the wrapper that runs Claude Code on the maintainer's subscription. It also holds the runner image the agent step runs in.

Issues are off in this repository. Every issue, the roadmap and the decisions
(ADRs) live in [those-who-run/paddock](https://github.com/those-who-run/paddock). A pull request here
names its paddock issue: `Closes those-who-run/paddock#N.` when it meets every
acceptance criterion, `Part of those-who-run/paddock#N` otherwise
([`AGENTS.md`](AGENTS.md)).

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
  stops the process. The example reads `ADDR` (`:8080`) and
  `LOG_LEVEL` (`info`).
- **Logging with `log/slog`, as JSON** on standard output, one object per line,
  with key-value pairs.
- **A health endpoint**, `GET /healthz`, which answers 200 while the process can
  serve.

## From the template

This repository was made from `chassis` with `copier copy`, and
`.copier-answers.yml` records the release it came from and the answers given.
Renovate proposes each new `chassis` release as a PR that runs
`copier update`; chassis's README has the rest.

## Releases

Merging the release PR tags `vX.Y.Z`, and the release workflow publishes
`ghcr.io/those-who-run/twr-pit-crew:X.Y.Z` once the maintainer approves it in
the `release` environment.
