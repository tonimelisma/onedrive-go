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
and traceability checks, and the fast live E2E suite. The live suites need
test accounts; see "E2E & Integration Tests" in [CLAUDE.md](CLAUDE.md).
Contributors without test accounts can run `go run ./cmd/devtool verify public`,
which is the same without the live suite.

## How a change is made

Work is done in PR-backed increments from isolated worktrees. Fetch first,
then start from current `origin/main`:

```bash
git fetch origin
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

`pre-pr` runs the local verification profile, checks the branch is based on
latest `origin/main`, and rejects undecided PR-comment carryover. `pre-merge`
waits for PR CI (`verify`, `integration`, and `e2e`; PR `stress` may be
skipped), posts the manifest's templated replies, resolves handled review
threads, and requires zero unresolved review threads. `post-merge` performs the
squash merge, handles already-completed multi-worktree merge and cleanup
cases, fast-forwards root `main`, removes the increment worktree and branch,
validates the configured post-merge CI-skip rule when applicable, and runs
`cleanup-audit`.

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
