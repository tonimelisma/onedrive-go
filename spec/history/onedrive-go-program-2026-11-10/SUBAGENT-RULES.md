# Rules for an agent implementing one PR of the onedrive-go program

You implement one pull request's worth of change in a git worktree of
`tonimelisma/onedrive-go` (the owner's own repository, a OneDrive sync client
in Go). Another agent reviews your commit, runs the full verification, and
opens the PR. Read all of this before you start.

## Where you work

- Only inside the worktree path your brief names. Never edit or run anything in
  `~/src/onedrive-go` (the main checkout), `~/sync-test-root`, `/srv`, `~/.config` or
  `~/.local`. Never start, stop or signal the running `onedrive-go` client.
- Your working directory is not the worktree. Use `go -C <worktree> ...` and
  `git -C <worktree> ...`, and absolute paths for file tools.
- One command per Bash call. No `&&`, `;`, pipes or `bash -c` strings.

## What you must not run

- `go run ./cmd/devtool verify ...` in any form (the reviewer runs it; the host
  has two cores and one full verify at a time).
- Any e2e test (`./e2e`, build tags `e2e`, `e2e_full`), and never the built
  CLI against an account. To check that e2e code compiles, use
  `go -C <worktree> vet -tags 'e2e e2e_full' ./e2e/...`, which runs nothing.
- `git push`, `gh pr create`, or any other write to GitHub.

## How you work

1. Read the repository's `CLAUDE.md` lines 55 to 236 (philosophy, coding
   conventions, test style) and the design doc that governs the files you
   touch (the routing table at `CLAUDE.md` lines 5 to 53 says which).
2. Read the analysis section your brief names. It was written read-only at
   commit `70301c0e` and carries no authority: **check every claim you rely on
   against the code**. Where the code disagrees, follow the code and record
   the difference as a deviation.
3. Tests first. Write the failing tests, run them and see them fail for the
   right reason, then make the change. Every bug fix has a regression test.
   Every new test carries a `// Validates: R-x.y.z` comment naming the
   requirement it proves. Tests use testify, never `time.Sleep`.
4. **Fix what you find where you already are** (owner's rule, 2026-11-11:
   every smell gets fixed). In a file your change edits anyway, a defect, a
   wrong or stale comment, dead code, a swallowed error, a misleading name,
   a magic number or a missing test is fixed in the same commit, with a test
   wherever behaviour changes, and listed in your report under "Also fixed".
   A smell in a file you do not otherwise touch, or a fix that would more
   than double your diff, is not fixed here: it goes into your report under
   "Not fixed", with file and line, one line on what is wrong and one line on
   the fix. The reviewer turns every such entry into a planned PR, so list
   all of them; nothing you noticed stays unwritten.
5. Update the documents in the same commit: the governing design doc under
   `spec/design/`, the requirement lines under `spec/requirements/`, and
   reference docs the brief names. Every production `.go` file must be named
   by exactly one design doc `GOVERNS:` line; every `TestXxx` a design doc
   cites must exist. A new or renamed file needs its `GOVERNS:` entry.
6. Format with `~/go/bin/gofumpt -w <files>` and
   `~/go/bin/goimports -w <files>` (check `.golangci.yml` for the
   local-prefix setting). They are not on `PATH`.
7. Run the tests of the packages you touched with the race detector:
   `go -C <worktree> test -race -count=1 ./internal/<pkg>/...`. Also run
   `go -C <worktree> build ./...` and `go -C <worktree> vet ./...`.
   **Never run another package's test suite**, and never `./...` tests: the
   host has two cores and other agents are working. When your change could
   affect a package you did not touch, name it in your report and the
   reviewer runs it.
8. Commit locally on the worktree's branch, one commit, message in the
   repository's style (imperative summary line, body that says what and why).
   End the message with the line
   `Co-Authored-By: Claude <noreply@anthropic.com>`. No model names anywhere.
   Do not commit `.dod-pr-comments.json` or anything git-ignored.

## Conventions that are often missed

- Zero users: no backward-compatibility shims, no deprecated aliases.
- Errors are wrapped `fmt.Errorf("verb noun: %w", err)` and never swallowed.
- Every goroutine has an owner that waits for it; every external call has a
  timeout; no magic numbers (named constants); unexported by default.
- The linters `gochecknoglobals` and `exhaustive` are on.
- Never write an account name, an email address, a drive ID, a token or any
  other credential into code, tests, docs, commit messages or your report.
- Never write the plan's labels (A7, B3, D15, "wave C" and the like) or the
  words "the plan", "the brief", "the reviewer" into code, tests, spec files
  or the commit message: a reader of the repository has none of them.
- The repository has tests that check its own lists, and a `-run` pattern
  skips them. A new `TestRunOnce_` test in `internal/sync` must be added to
  `runOnceScenarios()` in `dispatch_order_test.go`; then run
  `TestReverseDispatchTable_ListsEveryRunOnceScenario` and
  `TestReverseDispatch_RunOnceScenariosPass/<your test>`. Before you hand
  back, run every test of the package whose name contains `ListsEvery`,
  `Table`, `Governs`, `Exhaustive` or `Registered` (they are fast).
- `main` moves while you work. Do not rebase; the reviewer does. Write tests
  against the types as they are in your worktree, and name in your report any
  declaration from another file that your tests lean on.

## Shell and output rules

- Never put a backtick inside a double-quoted shell argument.
- Keep command output small. Run every test, build and lint through the quiet
  runner, which writes the full output to a log and prints only the tail:
  `python3 ~/agent/notes/onedrive-go-program-2026-11-10/tools/runq.py
  -n 40 -C <worktree> <logfile> -- <command...>`. `-e K=V` (repeatable) sets
  an environment variable; `-e PATH=...` replaces `PATH`. Log files go in the
  scratchpad directory your brief names, prefixed with your PR id.
- grep: `-l` or `-m N` on single files; over a tree only `-rl`, never a count
  per file. The host's grep rejects bounded repeats (`x{2,3}`).
- Give every `go test` a `-timeout` (default is 10 minutes, then it is killed).
- A test of `internal/sync` runs with a `-run` pattern for the tests you
  touched, never the whole package (it takes 25 minutes with `-race`).

## Lint

The linter is strict. Rules that have bitten earlier PRs: errcheck with
check-blank (no `_ =` for errors), testifylint (`require.Error`,
`require.ErrorContains`, `require.ErrorIs`, never `assert` for errors),
goconst (counts test files too), dupl, gosec G101 and G120 in tests, gocyclo
15, funlen 100 lines / 50 statements, mnd (also in tests), wrapcheck,
nolintlint (a `//nolint` needs the linter name and an explanation), no
`time.Sleep` in tests, misspell (US spelling), govet shadow, unparam,
gochecknoglobals, goimports with the local prefix, gofumpt.

Run the linter on the packages you touched, before you commit and again after
any later change:

    python3 .../tools/runq.py -n 60 -C <worktree> -e GOTOOLCHAIN=go1.26.11 \
      -e PATH=~/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      <report dir>/<id>-lint.log -- golangci-lint run --allow-parallel-runners \
      ./internal/<pkg>/... [more packages]

(one line in practice). `0 issues.` is the pass; the `level=warning ...
exclusion_rules` lines are normal. It takes about 2 minutes for small
packages and about 8 for `internal/sync`. Fix every issue; never hand back
with issues open or without the lint log.

**One lint at a time on this host.** It has 6.9 GB of RAM and one
golangci-lint run takes about 4 GB; two at once swap and both time out. Never
run `devtool verify lint` (the whole repo); use the targeted command above.
Before you start it, run `pgrep -f "golangci-lin[t] run"`; if it prints a PID,
wait until it is gone (the reviewer's gate lints for about 5 minutes at the
start of each run).

## Layering (checked by the reviewer's gate, not by lint)

Each production file is governed by one design doc (`GOVERNS:`, line 3 of the
doc), and each sync design doc has a `LAYER:` rank (line 5): `sync-domain.md`
0, `sync-store.md` 1, `sync-planning.md` and `sync-observation.md` 2,
`sync-execution.md` 3, `sync-engine.md` 4. A file may not reference a
declaration owned by a file of a higher layer. Put a new declaration in a
file governed at or below the lowest layer that uses it.

## Definition of Ready item 11

Write exactly: "ASSUMPTION **Review carryover and nightly**: the brief states
the DoD start stage has already been run in this worktree; no carryover
threads or nightly claims were handed to this increment."

## What you hand back

Stage files by name (never `git add -A` or `git add .`; the worktree holds
untracked local files that must not be committed). Never hand back without a
commit.


Write two files in the directory your brief names as the report directory
(outside the worktree), then reply with a short final message.

- `<id>-dor.md`: the Definition of Ready report in the format of the
  repository's `CLAUDE.md` lines 252 to 278: all 12 items, each starting with
  `PASS`, `FAIL` or `ASSUMPTION`, with evidence. For item 11 write exactly
  what your brief gives you.
- `<id>-report.md`, **at most 60 lines**: what changed (one line per file);
  the tests you added and what each proves (one line each); the checks you
  ran and their result; deviations from the analysis or the brief, **only
  those that change behaviour, scope or an interface** (not line-number
  corrections, wording choices or things you did as asked); "Also fixed"
  (one line each); "Not fixed" (every smell you saw and left, one entry each:
  `file:line`, what is wrong, the fix; this list may run past the 60 lines);
  anything left unfinished; packages the reviewer should test that you did
  not.

Final message, at most 200 words: the commit hash, the number of files
changed, the names of the tests added, deviations in one line each, and
whether anything is unfinished. Do not paste diffs or logs.
