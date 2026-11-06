# Agents and contributors

`pit-crew` is part of Those Who Run v2; [`README.md`](README.md) says what it is.

The rules for every v2 repository live once, in paddock's
[`AGENTS.md`](https://github.com/those-who-run/paddock/blob/main/AGENTS.md), and apply here in full, for
coding agents and for people. This file adds only what is specific to this
repository, and never repeats or weakens a rule from there.

## Issues

Issues are off here: every issue lives in
[those-who-run/paddock](https://github.com/those-who-run/paddock/issues). Read one with
`gh issue view N -R those-who-run/paddock --comments`. A PR here says
`Closes those-who-run/paddock#N.` only when every acceptance criterion is met,
and `Part of those-who-run/paddock#N` otherwise, as paddock's `AGENTS.md` says.

## Definition of done

`make check` passes. It runs what CI runs, in three parts:

- the commit hooks on every file (CI's `checks / checks`);
- the Go checks, in the order go.yml in `those-who-run/.github` runs them (CI's
  `go / check`): gofmt, `go mod tidy` for the module and `tools/go.mod`, go vet,
  golangci-lint with [`.golangci.yml`](.golangci.yml), the tests with the race
  detector (a skipped test fails, and so does total coverage below the floor,
  `COVERAGE_FLOOR` in the [`Makefile`](Makefile)), go build and govulncheck;
- the repository's own checks (with the Go tests, CI's `test`): every internal
  Markdown link and anchor resolves, the guard and settings in `.claude/` are
  `those-who-run/.github`'s `agent/` at the commit CI pins, and the tests in
  `tests/`.

It needs Go (the version in `go.mod`'s `toolchain` line, which the `go`
command fetches when `GOTOOLCHAIN` allows it), `python3`, `pre-commit` and the
network. `make prereqs` checks for each tool, and says how to fix any that is
missing; `make check` and `make hooks` run it first. CI's `image / build`,
where the repository ships an image, also builds the Dockerfile and
smoke-tests the image; `make check` doesn't.

A change also carries a test for each acceptance criterion of its issue
([Go](#go) says where), docs changed in the same PR as what they describe,
and a recording that pins a behaviour where one applies.

## What is specific to this repository

- **Two trust boundaries, in code.** The model's step never holds a GitHub
  credential (`twr-claude run` refuses to start with one in its environment),
  and the publish steps never hold a model. A change that moves a credential
  across that line, or lets a publisher act on anything but the files the
  model wrote and the facts the fetch step recorded, is wrong whatever else it
  does.
- **Everything read from GitHub is data.** A marker, a status or a label event
  counts only when the configured account wrote it. A new rule that reads
  GitHub says which account it trusts, and its test includes a stranger
  writing the same thing.
- **Decisions are pure.** `internal/ladder` and `internal/publish` take data
  and return an answer; the network, the clock and the file system stay in the
  callers. A new decision goes there, with a table test over realistic
  fixtures, before any command uses it.
- **Fakes at the HTTP boundary.** `internal/github`, `internal/apptoken` and
  `internal/kube` are tested against `net/http/httptest` servers that answer as
  GitHub and the API server do; the poller is tested against an in-memory
  GitHub. No mocks that assert their own stubs.
- **Nothing private.** This repository is public: no host names, addresses,
  cluster contexts, home paths or e-mail addresses, in code, tests, docs or
  commits. `internal/privatescan`'s own tests build their failing values from
  parts. Say "the maintainer's cluster". The lab's own patterns are supplied
  at run time, never committed here.
- **`docs/jobs.md` is a contract.** The Kubernetes manifests in the
  maintainer's private repository are written from it: a change to a command
  line, an environment variable, a mount, an exit code or a placeholder changes
  that page in the same PR.
- **Standard library only**, unless a dependency is truly unavoidable; then it
  is listed in `docs/dependencies.md` with its reason, as every Go component
  does.

## Go

- **Packages.** Anything not meant to be imported by another module lives
  under `internal/`, which the `go` command keeps other modules from
  importing. A package outside `internal/` is an API other repositories may
  build against.
- **Tests.** Each acceptance criterion has a test in a `_test.go` file named
  for what it checks, beside the code it tests, such as
  [`internal/dependencies`](internal/dependencies/dependencies_test.go) for the dependency table.
  Fixtures go under `testdata/`, which the `go` command skips.
- **Tool versions.** The Go version is `go.mod`'s `go` and `toolchain`
  lines. golangci-lint and govulncheck are pinned in
  [`tools/go.mod`](tools/go.mod), checksummed in `tools/go.sum`, and run with
  `go tool -modfile=tools/go.mod`, as the Makefile does; never a copy
  installed some other way. Renovate bumps them; a new tool is added there
  the way `those-who-run/.github`'s
  [workflows README](https://github.com/those-who-run/.github/blob/main/.github/workflows/README.md#toolsgomod)
  says.
- **Formatting and linting.** gofmt (`make fmt`) and golangci-lint with
  [`.golangci.yml`](.golangci.yml) (`make lint`). The commit hooks, `make
  check` and CI's `go / check` all refuse unformatted code and lint findings.
  A finding is fixed, or silenced at its line with
  `//nolint:<linter> // <why>`.
- **Generated files.** There are none yet. One that is added is written by a
  `make` target or a `//go:generate` line, starts with Go's
  `// Code generated … DO NOT EDIT.` line, is committed, and is never edited by
  hand: change its source and generate it again. That line also leaves the
  file out of the coverage floor, in `make check` and in CI.
- **Dependencies.** Declared in `go.mod` and locked in `go.sum`, both
  committed and tidy (`go mod tidy`; the commit hooks and CI check it). No
  `vendor/` directory. Each direct `require` has a row in
  [`docs/dependencies.md`](docs/dependencies.md) saying why it is there, added
  in the same PR; `internal/dependencies` fails `make check` without it.
- **Contracts.** The contracts module, and any other module of the project,
  is required at a released tag (`vX.Y.Z`), never at a pseudo-version from a
  branch or an untagged commit, and never through a `replace` to a local path
  or a fork.

## Make targets

A new clone or worktree runs `make prereqs`, then `make hooks`, first.

- `make prereqs`: checks every tool `make check` and `make hooks` need is
  there (Go at `go.mod`'s `toolchain` version, or a `GOTOOLCHAIN` that lets
  the `go` command fetch it, gofmt, the C compiler the race detector needs,
  `python3`, `pre-commit`, `git` and `make`), offline and in well under 2 s.
  Each one missing or unusable gets a line saying how to fix it.
- `make check`: everything a PR must pass, as CI runs it.
- `make test`: the Go tests with the coverage floor, and the repository's own
  checks: CI's `test` job, while iterating.
- `make fmt`: formats the Go code; `make lint`: golangci-lint.
- `make hooks`: installs the commit hooks. A new clone or worktree runs it
  once, after `make prereqs`, so the hooks run on every commit and commit
  message.

`make help` lists the rest, among them each Go check on its own.

## The guard

`.claude/settings.json` and `.claude/hooks/guard.py` are copies: a change to
them is made in `those-who-run/.github`, then copied here unchanged with the
bump of the `checks.yml` pin in `.github/workflows/ci.yml`.
`.claude/guard.json` holds this repository's own values.
