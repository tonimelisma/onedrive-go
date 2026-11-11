# Contributing to onedrive-go

Thanks for your interest. This file covers how work is done in this repo. The
full rules, including coding conventions and the Definition of Ready and
Definition of Done checklists, are in [CLAUDE.md](CLAUDE.md) (also available as
[AGENTS.md](AGENTS.md)); this is the short version.

## Where to start

- [Developer onboarding guide](spec/reference/developer-onboarding.md): an
  architecture tour and a reading order.
- [System architecture](spec/design/system.md).
- [Requirements](spec/requirements/index.md): what each capability must do and
  its status (`verified`, `implemented`, `designed`, `planned`, `future`).
- The routing table in [CLAUDE.md](CLAUDE.md) names the design doc to read
  before changing each package.

## Reporting bugs and asking for features

Open an [issue](https://github.com/tonimelisma/onedrive-go/issues). The bug
template asks for `onedrive-go --version`, your OS, `onedrive-go status` and
the command you ran. Security problems go through [SECURITY.md](SECURITY.md),
not public issues.

## Building and testing

You need Go 1.26 or newer.

```bash
source scripts/dev-env.sh        # scratch XDG roots, required by unversioned builds
go build ./...
go test ./internal/<package>/
golangci-lint run
```

An unversioned build reports version `dev` and refuses to run without
`XDG_CONFIG_HOME`, `XDG_DATA_HOME` and `XDG_CACHE_HOME` set, so development
runs can never touch your real OneDrive configuration or files.

The canonical local verification entrypoint is:

```bash
go run ./cmd/devtool verify default
```

It runs formatting, lint, build (including the cross-compiled platforms), the
unit tests with the race detector and coverage gate, the repo's documentation
and traceability checks, and the fast live E2E suite. Coverage is measured
across packages (`-coverpkg=./...`), and the gate's number is written only in
requirement R-6.10.4, which devtool reads; to change the number, change that
line. The live suites need test accounts; see "E2E & Integration Tests" in
[CLAUDE.md](CLAUDE.md).
Contributors without test accounts can run `go run ./cmd/devtool verify public`,
which is the same without the live suite.

CI is tiered. Every pull request and every push to `main` runs the quick lane
in parallel jobs: `verify lint`, and `verify quick` (format, the repo rules,
the builds and `go test -short`, split into shards with `--shard k/n`) on
Linux plus `verify platform` (host build and the same short tier) on macOS. `-short` skips the tests marked exhaustive (randomized
model runs, replays, the reverse-dispatch re-run of every scenario, the
reconcile matrix); the race detector and the coverage gate do not run there.
The full tier (`verify public` on both platforms, `stress` over
`./internal/multisync` and the `./internal/sync/...` tree, and the full live
suite) runs nightly and on manual dispatch, so a data race or a coverage
drop can reach `main` and be caught by the next nightly run. Before a PR,
`go run ./cmd/devtool verify pr` (lint, then quick) is the local gate, and it
is what the `pre-pr` stage below runs; `verify default` still runs
everything, on a host that can finish it (its race-tier deadline grows on
hosts with fewer than 3 cores, and `--unit-timeout` overrides it). devtool
runs every command with the Go toolchain `go.mod` pins, so a newer system Go
does not change what is built or linted.

## How a change is made

Work is done in PR-backed increments from isolated worktrees, each started
from current `origin/main` (the command fetches it first):

```bash
go run ./cmd/devtool worktree add --path <path> --branch <type>/<task-name>
```

Branch types are `feat`, `fix`, `refactor`, `test`, `docs` and `chore`.

Every change keeps the specs honest:

- Every production `.go` file is named by exactly one design doc's `GOVERNS:`
  line, every test cited as evidence in a design doc exists, and every path in
  the CLAUDE.md routing table resolves. `devtool verify` enforces all three.
- Tests use testify (`assert`/`require`) and carry a `// Validates: R-x.y.z`
  comment naming the requirement they prove.
- Bug fixes come with a regression test that fails without the fix.
- Behavior changes update the governing design doc and requirements in the
  same PR, and `README.md` when user-visible capability or setup changes.

### Definition of Done stages

The Definition of Done is automated through staged verifier runs:

```bash
go run ./cmd/devtool verify --dod --stage start
go run ./cmd/devtool verify --dod --stage pre-pr
go run ./cmd/devtool verify --dod --stage pre-merge --pr <number>
go run ./cmd/devtool verify --dod --stage post-merge --pr <number> --worktree <path> --branch <branch>
```

`start` audits unresolved review threads from recent merged PRs and writes the
ignored `.dod-pr-comments.json` manifest. Every unresolved thread must be
classified as `fixed`, `already_fixed`, or `non_actionable` before the DoD can
pass. `fixed` and `already_fixed` require `what_changed`, `how_fixed`, and
evidence entries; `non_actionable` requires a reason. There is no passing
deferred state.

`pre-pr` runs the `pr` profile (or the profile named before `--dod`), checks
the branch is based on latest `origin/main`, and rejects undecided PR-comment
carryover. `pre-merge` waits for PR CI (`verify`, `integration`, and `e2e`; PR
`stress` may be skipped), posts the manifest's templated replies, resolves
handled review threads, and requires zero unresolved review threads.
`post-merge` performs the squash merge, handles already-completed
multi-worktree merge and cleanup cases, fast-forwards root `main`, removes the
`.worktreeinclude` entries and then the increment worktree and branch, waits
for the push-to-`main` CI run of the merge commit to succeed (every push to
`main` runs the quick lane), and runs `cleanup-audit`. Both CI waits go on
while any check changes state and fail after the longest timeout of the jobs
that run for the awaited event (a pull request, or a push after merge) plus 10
minutes without a change; a job the runner cancelled in the run of the commit
they wait for is rerun, at most twice (`--ci-max-reruns`). A check that has
not started while its job waits in a concurrency group gets one whole run of
that group instead. A GitHub request that fails with an HTTP 5xx, a dropped
connection or a network that cannot reach GitHub is repeated with backoff, at
most four attempts in all.

## Release readiness

A release is not ready until these proof gates are current for the release
SHA:

- `go run ./cmd/devtool verify default` passes cleanly.
- Manual live E2E is green for the release SHA; broader SharePoint and
  shared-library claims require matching live coverage.
- Any public performance claim has a repo-owned benchmark artifact with
  scenario, machine, date, method, and subject-under-test context (see
  [performance-benchmarking.md](spec/design/performance-benchmarking.md)).
- Side-effect expectations are current, especially dry-run and destructive
  command behavior.
- README, user docs and spec language use only `verified`, `implemented`,
  `designed`, `planned`, or `future` claims and contain no launch-blocking
  stale promises.

## License

By contributing you agree that your contribution is licensed under the
[MIT License](LICENSE).
