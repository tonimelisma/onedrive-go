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
`XDG_CONFIG_HOME` and `XDG_DATA_HOME` set, so development runs can never
touch your real OneDrive configuration or files.

The full verification tier a session runs is:

```bash
go run ./cmd/devtool verify public
```

It runs formatting, lint, build (including the cross-compiled platforms), the
unit tests with the race detector and coverage gate, and the repo's
documentation and traceability checks. Coverage is measured across packages
(`-coverpkg=./...`), and the gate's number is written only in requirement
R-6.10.4, which devtool reads; to change the number, change that line.
The live suites (`verify e2e`, `verify e2e-full`) need test accounts, which
exist only in CI (no session or container ever holds `.env`, `.testdata` or
token files), so they run only there; see "E2E & Integration Tests" in
[CLAUDE.md](CLAUDE.md).

CI is tiered. Every pull request and every push to `main` runs the quick lane
in parallel jobs: `verify lint` (golangci-lint, then a pinned actionlint over
`.github/workflows`, which `go run` fetches itself, so nothing is installed
for it), and `verify quick` (format, the repo rules,
the builds and `go test -short`, split into shards with `--shard k/n`) on
Linux plus `verify platform` (host build and the same short tier) on macOS. `-short` skips the tests marked exhaustive (randomized
model runs, replays, the reverse-dispatch and case-insensitive re-runs of
every scenario, the reconcile matrix); the race detector and the coverage gate do not run there.
Every push to `main` also runs `verify race`, the `./internal/sync/...` tree
under the race detector and unshortened, so a data race in the sync packages
fails the push that brought it. The full tier (`verify public` on both
platforms, `stress` over `./internal/multisync` and the `./internal/sync/...`
tree, `verify seeds`, and the full live suite) runs nightly and on manual
dispatch, so a data race elsewhere or a coverage drop can reach `main` and be
caught by the next nightly run. `verify seeds` runs the seeded model tests
without the race detector on a seed range that advances every day; a failing
seed prints a `seed-replay:` line whose command reruns it (run it at the
commit the night tested), and gets a `seed-failure` issue that stays open
until the fix closes it (`Closes #N` in its pull request). A rerun on the same
UTC day runs the same seeds; a later rerun of a night passes `--seed-start
<n>` with the start that night printed. Before a PR,
`go run ./cmd/devtool verify pr` (lint, then quick) is the local gate, and it
is what the `pre-pr` stage below runs; `verify public` runs the full
offline tier, and takes more than an hour on a 2-core host (its race-tier
deadline is 30 minutes, or 180 core-minutes divided by the cores on a host
with fewer than 3, and `--unit-timeout` overrides it). devtool
runs every command, and builds the benchmark's subject, with the Go toolchain
`go.mod` pins, so a newer system Go does not change what is built, linted or
measured.

## How a change is made

Work is done in Claude Code cloud sessions, one PR-backed increment at a
time on the session's assigned branch, each started from the current
`origin/main` (`spec/reference/cloud-development.md` explains the setup).
Builder subagents work in worktrees the harness makes under
`.claude/worktrees`; once their work is in `origin/main`,
`go run ./cmd/devtool worktree prune-merged` removes them.

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
go run ./cmd/devtool verify --dod --stage post-merge --pr <number>
```

`start` audits unresolved review threads from recent merged PRs and writes the
ignored `.dod-pr-comments.json` manifest. Every unresolved thread must be
classified as `fixed`, `already_fixed`, or `non_actionable` before the DoD can
pass. `fixed` and `already_fixed` require `what_changed`, `how_fixed`, and
evidence entries; `non_actionable` requires a reason. There is no passing
deferred state.

`pre-pr` runs the `pr` profile (or the profile named before `--dod`), checks
the branch is based on latest `origin/main`, and rejects undecided PR-comment
carryover. `pre-merge` waits for the pull request's required CI, posts the
manifest's templated replies, resolves handled review threads, and requires
zero unresolved review threads. `post-merge`, run on the increment's branch
with a clean checkout, squash-merges the pull request over REST pinned to the
head it checked, marks the branch's nightly claims fixed, restarts the branch
from the new `origin/main` and pushes it, waits for the push-to-`main` CI run
of the merge commit to succeed, prunes the merged builder worktrees, and ends
with the read-only cleanup audit. How the two CI waits decide that CI has
stalled, which cancelled jobs they rerun and how GitHub requests are retried
is in the Verification Policy of
[system.md](spec/design/system.md).

## Releases

Every merge to `main` is released, with no manual step (R-6.10.41). When the
push-to-`main` CI run of a commit succeeds, `.github/workflows/release.yml`:

1. refuses to run from any ref but `refs/heads/main`;
2. picks the version with `scripts/release-version.sh`: the highest
   `vMAJOR.MINOR.PATCH` tag with its patch bumped (`v0.1.0` for the first), or
   the tag the commit already has when a rerun finishes its release; a commit
   a newer release already contains is skipped;
3. builds `onedrive-go-<tag>-linux-amd64` and `onedrive-go-<tag>-darwin-arm64`
   from that exact commit (`CGO_ENABLED=0`, `-trimpath`, the tag stamped into
   `--version`) and writes `SHA256SUMS`;
4. smoke-tests the Linux binary with `scripts/release-smoke.sh` (it prints its
   tag, and `sync --watch` keeps running on a paused throwaway drive);
5. attests the binaries' build provenance, creates the tag with the job's
   `GITHUB_TOKEN`, publishes the release with generated notes, and verifies
   the published assets the way a consumer does.

One release runs at a time (concurrency group `release`). The owner can run
the workflow by hand from `main` with an optional `commit` (one whose push CI
passed) and a higher `version`. Nobody tags or releases by hand, and no
workflow runs on tag pushes. To check a download:

```bash
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify onedrive-go-vX.Y.Z-linux-amd64 --repo tonimelisma/onedrive-go \
  --signer-workflow tonimelisma/onedrive-go/.github/workflows/release.yml \
  --source-ref refs/heads/main
```

## Release readiness

The automatic releases above are builds of `main` before launch. A release
announced as ready for use is not ready until these proof gates are current
for its SHA:

- The nightly `verify public` and `e2e-full` runs are green for the release
  SHA; broader SharePoint and
  shared-library claims require matching live coverage.
- Any public performance claim has a repo-owned benchmark artifact with
  scenario, machine, date, method, and subject-under-test context (see
  [performance-benchmarking.md](spec/design/performance-benchmarking.md)); the
  live benchmark runs only by dispatch of `.github/workflows/bench.yml` from
  `main`, whose `bench-result` artifact is that artifact.
- Side-effect expectations are current, especially dry-run and destructive
  command behavior.
- README, user docs and spec language use only `verified`, `implemented`,
  `designed`, `planned`, or `future` claims and contain no launch-blocking
  stale promises.

## License

By contributing you agree that your contribution is licensed under the
[MIT License](LICENSE).
