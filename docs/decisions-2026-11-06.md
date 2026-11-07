# Decisions made overnight (for review)

`pit-crew` was built unattended on the night of 2026-11-06 from the approved plan
("Those Who Run: agents build, a merge ladder, the maintainer merges last", phase
P2). Where the plan and the brief did not settle a choice, the conventional,
correct option was taken and recorded here with the alternative not taken. Each
is open to reversal.

## The repository

1. **One repository from `chassis` v1.0.5, as a local git history, no remote.**
   The brief's instruction. The maintainer creates `those-who-run/pit-crew` on
   GitHub and pushes `main`. Alternative: everything in the private `home-lab`
   to start (the 29 Oct note's "simplest for a first week"); the plan chose the
   public repository.
2. **The example service is gone.** `chassis` ships `internal/service` with a
   health endpoint; `pit-crew`'s commands run to completion in Jobs, so it serves
   nothing, and `image.yml`'s smoke test runs `pit-crew -version` and checks the
   user instead of calling `/healthz`. Alternative: keep a `/healthz` server in
   the poller; it would be one more process in a CronJob pod with nobody to ask
   it.
3. **Two images from one repository, as two caller jobs.** The org's reusable
   `image.yml` takes `image` and `dockerfile` inputs, so `image.yml` here has two
   jobs, `image / build` (`twr-pit-crew`, the `Dockerfile`) and `runner / build`
   (`twr-pit-crew-runner`, `Dockerfile.runner`), and `release.yml` publishes both.
   `repos.json` must list both checks. Alternative: a second repository for the
   runner; the plan names one.
4. **The small image is Debian, not distroless.** The fetch and publish steps run
   `git`, which distroless's static image does not have. Alternative: implement
   enough of the git protocol in Go; not worth it.

## The ladder's decisions (`internal/ladder`)

5. **A review record is a marker comment or a `review / verdict` status, whichever
   is newer.** A clean release, pin or dependency review posts no comment (the
   brief: "kinds B to D only when something is wrong"), so without the status as
   a record such a pull request would be reviewed every poll. The poster writes
   both; either alone is enough. Alternative: always post a comment; twenty
   "looks fine" comments a week was what the brief wanted to avoid.
6. **The poster adds `verdict=` to the reviewer's marker.** The brief's marker is
   `<!-- twr-review sha= kind= lane= -->`; the poster writes it back with the
   verdict, so a comment alone says what the review concluded and the status can
   be rebuilt from it. The reviewer's own marker is checked first and must agree.
   Alternative: keep the verdict in the status only; then a comment without its
   status (the poster failing between the two writes) is a review with no
   conclusion.
7. **A builder's answer after the review re-opens the review at the same head.**
   An answer that contests every item pushes nothing, so the head does not move
   and the plan's "no marker for the current head" rule would never review the
   contest; the fix loop would then burn its second round unread. The poster
   updates the existing comment for that head rather than posting a second one,
   so "one comment per head" still holds. Alternative: require a new commit for an
   answer; the `twr-fix-review` skill says not to change code to make a comment
   go away.
8. **Fix rounds are counted as distinct round numbers in the builder's own
   comments.** The same round posted twice counts once; a round pasted by anyone
   else counts for nothing.
9. **"Every required check green" reads the branch ruleset; without one, every
   check reported must be green and there must be at least one.** An empty
   required list never passes by default. `skipped` and `neutral` count as green,
   as GitHub counts them for a required check.
10. **One `agent-ready` label event starts at most one build.** Beyond the plan's
    "no open pull request or `agent/<issue>-*` branch", a builder pull request
    created after the label event means that event is spent, whatever became of
    the pull request (merged with `Part of`, closed unmerged). The maintainer adds
    the label again to ask for another build. Alternative: the plan's two checks
    alone, after which a `Part of` merge with the branch auto-deleted would be
    built again once the Job's TTL passed.
11. **Linked pull requests come from the issue's timeline, and only those whose
    body has a `Closes` or `Part of` line for the issue count.** A mention in
    prose does not. An open one by an allowlisted author blocks a build; a
    stranger's does not, so a stranger cannot stop builds by opening pull
    requests.
12. **The target repository comes from the `component:` label**, mapped to
    `<owner>/<name>` unless `LADDER_COMPONENTS` says otherwise, and must be in
    `LADDER_BUILD_REPOS`. Two labels naming two repositories, or none, is no
    build; two naming one (`component: deploy` and `component: paddock`) is fine.
13. **Committers may be a wider list than authors** (`LADDER_COMMITTERS`), for
    GitHub's `web-flow` on web merges and update-branch commits. The default is
    the authors list.
14. **Logins compare without case.** GitHub's do.
15. **Job names are deterministic** (`review-<repo>-<n>-<sha8>-f<rounds>`,
    `fix-<repo>-<n>-r<round>`, `build-<repo>-<issue>`), so a unit of work is tried
    once per Job lifetime, and a failed unit is retried only when its Job's TTL
    passes. Alternative: `generateName` and a database of what was tried; the
    plan says no database.
16. **The poller starts at most one Job per run, fix before review before build.**
    A fix continues work already read; a review unblocks a merge; a build makes
    new work.
17. **The daily cap is a rolling 24 hours**, counted from the Jobs' creation
    times, so the TTL must be at least a day. Alternative: a calendar day in a
    zone; a rolling window has no burst at midnight.
18. **The idle gate fails closed.** A gate URL that is down or a gate command that
    cannot run means "not idle". The gate itself is the lab's.
19. **Back-off is read from finished Jobs' agent exit codes**, which needs `list`
    on pods. The poller is the only pod with a service-account token, and this
    is the only read beyond Jobs. Alternative: the agent writing its outcome to
    the shared volume and the publisher carrying it to GitHub; there is nowhere
    on GitHub for "the plan limit was hit".
20. **`ready` is a separate command**, run on its own schedule, and the poller
    writes nothing to GitHub. `ready` also sets `ladder / stuck`, and takes a
    `ladder / ready` it set earlier back to `pending` when the head no longer
    qualifies.

## The publishers (`internal/publish`)

21. **A review comment that names a person (`@login` outside code) is held.** A
    bot that can be steered into naming people is a way to reach them. Not in the
    plan's list; cheap, and the kind of thing the plan's "private findings have a
    private path by construction" is about.
22. **A held review sets `review / verdict` to `error` with `held: <codes>`**, so
    the pull request is not reviewed again at that head and the maintainer sees
    why. The codes are public words; the detail and the files go to the hold
    directory only.
23. **The private scan has a code mode for diffs**: in source, a dotted name is a
    selector (`time.Local`, `cfg.Internal`) far more often than a host, so only
    URL hosts and `user@host` count there. Prose (titles, bodies, comments,
    commit messages) gets the full scan. Alternative: the full scan on code,
    which would hold most Go changes.
24. **Paddock's `bootstrap` rule is not ported.** It is about NATS stream
    definitions in paddock's own files and has no meaning for a comment or a
    diff of another repository.
25. **Two rules were added to the port**: `home-path` (`/home/<name>`,
    `/Users/<name>`, with the image's and CI's users allowed) and `secret`
    (GitHub, Anthropic and Slack token shapes, PEM private keys). gitleaks is not
    in the image; CI's gitleaks runs after the push.
26. **The build publisher's test-weakening check is a first net, not CI's guard**:
    a test file deleted or renamed away, a skip added to a test file, each needing
    a `Tests weakened:` line of ten characters or more, as CI's check reads it.
    Go, Python and the JavaScript runners' skips are known.
27. **The pull request body is checked against the template as `twr-pr-ready`
    writes it**: both headings in order, the `AI assistance:` and
    `New direct dependencies:` lines filled, the evidence table with a row, the
    two checkboxes present (ticked or not: an honest unticked box is
    information), and the last line `Closes` or `Part of` this build's issue,
    with only the tool trailer allowed after it.
28. **The build publisher never runs git inside the model's repository.** The
    model can write `.git/config` and `.git/hooks`, which would run in the step
    that holds the builder's token. The publisher makes a repository of its own,
    reads the model's objects through `GIT_ALTERNATE_OBJECT_DIRECTORIES`, reads
    the branch's head from its ref file, and does every check and the push from
    there. A test plants a `pre-push` hook, a `url.insteadOf` redirect and an
    alias in the model's `.git` and shows that none of them runs and the push
    reaches the origin.
29. **The fix publisher appends the fix marker itself** and refuses an answer that
    carries one, as the review poster refuses a comment with a second marker.

## The fetch step and the wrapper

30. **The clone's token goes through git's environment**
    (`GIT_CONFIG_COUNT`/`KEY`/`VALUE` with an `http.extraheader`), never into the
    URL or `.git/config`, so the working tree the model gets holds no credential.
31. **The brief goes in the prompt, not the system prompt.** `--append-system-prompt-file`
    is in the CLI reference but not in the local help; the prompt on standard
    input (brief body, then the task statement) is what the 29 Oct note did and
    needs no unverified flag. Alternative: `--agents` with the agent file;
    unverified too.
32. **Skills are read as files.** The task statement points the model at the
    pinned `.github` checkout's `agent/skills/` and tells it to read a skill when
    the loop names it, rather than relying on `--restricted` loading project
    skills, which the help does not promise.
33. **`run` refuses to start with a GitHub token in its environment** (`GH_TOKEN`,
    `GITHUB_TOKEN`, any `GH_*`/`GITHUB_*` variable whose value looks like a token),
    exit 3. The plan says it in words; this is the code.
34. **Exit codes 70 and 75 are read from the result text and the exit status**,
    since the documentation does not give codes for an expired login or the usage
    limit; the strings matched are the documented error messages.

## The images

35. **Claude Code is installed from npm at a pinned version**, not with the native
    installer: `npm install -g @anthropic-ai/claude-code@<version>` is one line
    with npm's own integrity check against the registry's record, and the
    version is an `ARG` beside the other tools'. Alternative: the native
    installer with its signed `manifest.json`; it would need GPG in the build.
36. **Every tool in the runner is downloaded by version and checked against its
    published SHA-256**, in `ARG` pairs at the top of `Dockerfile.runner`;
    Debian's packages come at the base image's digest. Renovate bumps the base
    images; the tool versions are bumped by hand with their checksums.
37. **The runner's base is Node's Debian image** (Node for Claude Code), on the
    same Debian release as the small image, which is Debian's slim image with git
    added.

## After the manifests were written (2026-11-07)

The home-lab manifests were drafted from `docs/jobs.md` and found gaps between
the contract and the code; these fix them.

38. **The idle gate can read a JSON field** (`POLL_IDLE_GATE_FIELD`). A health
    endpoint answers 200 during a race too; ecu's `/healthz` carries `"idle"`.
    With the field set, idle needs 200 and that top-level field being boolean
    `true`; a missing field, another type, a non-object body, non-JSON, a
    non-200 or a transport error all mean live. Alternative: a separate idle
    endpoint in ecu, or the lab's idle-gate Job as the command gate; reading the
    field needs nothing new in ecu.
39. **`fetch` writes `data/swapped.md`** for a review: which steering files the
    working tree has from the base rather than the pull request (only those the
    pull request changes), where the pull request's own are, and that a check
    comparing `.claude/` against a pin (`scripts/guard_copy.py`, `make guard`)
    fails in this checkout for that reason alone on a pull request that bumps
    the pin. `task.md` tells the reviewer to read it first when there was a
    swap, and says so plainly when there was none. Alternatives: not swapping
    the `.claude/` copies (gives up the plan's protection against a pull request
    steering its reviewer); or `fetch` patching the guard check out of the
    checkout (editing the code under review). The reviewer brief in `.github`
    should gain one sentence under kind A, step 5: "If `data/swapped.md` in the
    data files lists swapped steering files, a failure of the guard-copy check
    (`make guard`, `scripts/guard_copy.py`) is the swap's, not the pull
    request's: compare the pull request's versions under `data/pr-files/`
    against the pin by hand and say so under Checked."
40. **Only steering files the pull request changes are swapped and copied to
    `pr-files/`**, by blob ID; an unchanged one is left as it is and listed by
    neither. Alternative: swapping every steering file whatever its content,
    which made `git status` noisy and `swapped.md` wrong.
41. **The App key Secret carries the client ID too** (`client-id` beside
    `private-key.pem`), read as `APP_CLIENT_ID` through `secretKeyRef`, and
    `twr-private-patterns` is an optional Secret mounted where
    `PUBLISH_PATTERNS_FILE` points. `docs/jobs.md` now says `fetch` creates
    `/work/out` (so the mount is read-write there), that `TWR_CLAUDE_BRIEF` is an
    absolute path, that the pattern and allow files are optional when unset and
    a hard failure when set and missing, and that the poller's egress must allow
    the API server endpoint the in-cluster configuration gives, after kube-proxy's
    DNAT, not only the Service name.

## Not done, and why

- The small image was built once with podman and passed the smoke test image.yml
  runs (`-version` for both binaries, git present, user 65532); it is 197 MB. The
  runner image was not built: the load rules allowed no container build that is
  not small, and this one installs several toolchains. Its first build is CI's
  `runner / build`, and a wrong checksum or URL will show there.
- Nothing was run against a cluster, a GitHub App or a real Claude session;
  `docs/jobs.md` lists what the first real run must prove.
- The report that goes with this file says exactly which numbered pieces of the
  brief are done, partly done or not started.
