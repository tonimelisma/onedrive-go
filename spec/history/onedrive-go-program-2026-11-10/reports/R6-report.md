# R6 report: the generated split of internal/sync (S5) and depguard (S6)

Branch `refactor/split-move`, worktree `~/src/onedrive-go-wt/split-move`, on
main `197f4d24` (#961). Six commits, pushed; no PR, no install.

| Commit | What | Size |
|---|---|---|
| `976a2ba3` | Prep: `OutcomeCommitTimeout` moves unchanged to `domain_worker.go` (multisync reads it; in `worker.go` it would land in private `syncexec`; the model had flagged multisync→syncexec 1/1, R5 had not resolved it) | 3 files, +17/-14 |
| `3ba8ac60` | S5, generated: the move | 633 files, +19,202/-17,369 |
| `d0ea62ae` | Lint cleanup of the generated move, by hand, nothing else: 51 product lines wrapped (lint showed 37, capped by max-same-issues), 2 dupl directives on near-twin blocks the qualifiers took over the threshold, 4 dead test helpers deleted | 29 files, +154/-116 |
| `9eddb6bc` | S5, by hand: docs, gates, source-reading tests | 57 files, +618/-292 |
| `f543dcd0` | S6, by hand: depguard replaces `verify_layering.go` | 14 files, +206/-442 |
| `a93a60f9` | S5 lint rules: wrapcheck exclusion, helper exemptions, stale directives | 12 files, +31/-15 |

Whole branch: 676 files, +20,147/-18,167.

## Rerunning the generator

Tool: `tools/splitmove/splitmove.go` (1,342 lines, standard library only;
header comment states exactly what it rewrites). Inputs: the simulator's
output on the prep commit and R5's configuration.

```
S=$HOME/.cache/onedrive-go-tmp/r6            # any scratch dir
git -C ~/src/onedrive-go worktree add --detach $S/rerun 976a2ba3
cd ~/agent/notes/onedrive-go-sync-split-2026-11-10
tools/run.sh ~/agent/notes/onedrive-go-program-2026-11-10/reports/R5-conf/final.txt $S/final $S/rerun
go -C $S/rerun run ~/agent/notes/onedrive-go-program-2026-11-10/tools/splitmove/splitmove.go \
  -repo $S/rerun -sim $S/final -conf ~/agent/notes/onedrive-go-program-2026-11-10/reports/R5-conf/final.txt
git -C $S/rerun add -A internal e2e && git -C $S/rerun diff --cached --stat 3ba8ac60   # empty
```

Done 2026-11-20: the rerun is identical to `3ba8ac60`. A spec cut out of a parenthesized group is copied with the group's `//nolint`/`//go:` directives, specs of one group stay one group, and trailing line comments go with their spec (this replaced `079c53ec`, which lost the store template's `//nolint:gochecknoglobals`). About 2 minutes (it
type-checks the Linux and the macOS file sets; the two must agree per file;
`-dry` prints the plan). Simulator verdict on the prep commit: 0 errors, 0
violations, 0 cycles, 0 illegal test references.

What it does: each file moves whole to its package's directory (package from
the simulator's tables; disagreement stops the run); helper files become
ordinary files of `syncfixture`/`syncstoretest`; cross-package references get
qualifiers and imports; 232 unexported helper identifiers other packages use
are capitalised (simulator: 216 + 16), three mock function/channel fields that
meet a method of the same name take `Func`/`Ch`; private test copies go to
`helper_copies_test.go` in sync, syncdomain, syncexec, syncstore; consumers
(cli, multisync, syncverify, e2e) get the new qualifiers and imports; planner
goldens move to `syncplan/testdata`. Output gofmt/gofumpt/goimports-clean.

## S6 un-export

Tool: `tools/splitmove/unexport/unexport.go` (863 lines). Rerun:
`go -C <wt> run .../unexport/unexport.go -repo <wt> -base 9b16a5c0 -dry`.
Result: 872 identifiers exported since `9b16a5c0` (lower-case there); 869 are
named by another package (both file sets, tests and e2e tags included); 2 are
test-type methods kept for interface satisfaction; 1 (`syncdomain.StrDownload`)
is used only in its package but is kept, because end-state 10.5 exported it to
complete its enum (parked R6-2). So nothing is un-exported and there is no
generated S6 commit.

## Hand edits (`9eddb6bc`, `f543dcd0`, `a93a60f9`; lint cleanup `d0ea62ae` above)

- A `doc.go` for each of the 8 new packages; GOVERNS paths (221 path
  substitutions in docs and two tests, by script), doc.go and helper globs;
  the CLAUDE.md routing table is by package directory; the onboarding guide,
  system.md, R-6.10.20 and R-6.10.25 follow.
- Stress lane: member lists split by package (5 packages; sync's lists keep
  their names). Seeds: each family names its package, so the planner family
  replays in `syncplan`. Without this its replay line would name the wrong
  package. The replay writer takes its package from the test's directory.
- panicguard: the exception keys point at syncexec; the tree's packages share
  recover boundaries; helper packages are excluded. Mutex guard comments in
  the fixtures (6).
- Source-reading tests: they read the tree's package directories, not one
  directory (context-error sites, detached contexts, real clock, local-scan
  issue types, stored times, DSN builders, the module walk); qualified calls
  count; the module root is found by go.mod. One sync test lost its planner
  entry, because the writer names the caller's package.
- S6: one lax depguard rule per package of the tree (9) and the driveops rule
  widened. The new `TestGolangciConfig_EverySyncTreePackageHasItsImportRule`
  (Validates R-6.10.20) was seen failing first. `verify_layering.go`, its
  tests, its step and the `LAYER:` lines are gone.

## Test counts

Top-level functions of the test files `go list` selects, `./internal/sync/...`
(all moved packages; no Fuzz or Example exists), per side; "all tags" is
`e2e e2e_full integration stress race`:

| | linux default | linux all tags | darwin default | darwin all tags |
|---|---|---|---|---|
| main `197f4d24` | 1,915 Test + 6 Bench | 1,915 + 6 | 1,909 + 6 | 1,909 + 6 |
| head | 1,915 + 6 | 1,915 + 6 | 1,909 + 6 | 1,909 + 6 |

The name sets are identical in each of the four cells, so no name is missing.
The darwin cells lack six Linux-only tests on both sides.

The reviewer's 2,331 → 2,336 was `go test -list .` of internal/sync **and
internal/devtool**: main is 1,921 + 416 = 2,337 (#961 added one devtool test
after that review); head is 1,921 + 412. The devtool difference is S6 by design:
five tests of the retired layering check (`TestFindLayeringViolations_*` ×4,
`TestReadLayerRanks_ReadsOnlyDocsThatDeclareOne`) are gone, and one test,
`TestGolangciConfig_EverySyncTreePackageHasItsImportRule`, is new.

## Checks

- `go build ./...` and `go vet ./...` (linux, tags `e2e,e2e_full,integration,stress`, darwin/arm64); gofumpt and goimports clean.
- Governs, cited-test and routing gates (temporary devtool test, deleted): 0; devtool `RealRepoIsClean` 4 PASS.
- One run, `-p 1 -timeout 90m`, behind `heavy.lock`: syncobserve, syncplan, syncdomain, multisync, cli, syncverify, panicguard and devtool all ok. 10 source-reading tests failed in sync (8), syncexec and syncshortcut; they were fixed and rerun green. syncstore did not build, because I edited one of its tests during the run; it was rerun whole and its one failure (DSN builder) was fixed and rerun green.
- Lint once, the whole repo with e2e tags (`reports/R6-lint.log`): 122 issues before the lint commit.
- Final run on head `a93a60f9`, `-p 1 -count=1 -timeout 90m`, TMPDIR, behind `heavy.lock`: all 12 packages ok (sync 538 s, syncplan 813 s, syncexec, syncobserve, syncdomain, syncshortcut, syncstore, multisync, cli, syncverify, panicguard, devtool).

## Lint

Whole repo, e2e tags, `--max-same-issues=0 --max-issues-per-linter=0`, after
the fixes: 4 issues left (two dupl directives placed on the wrong line); after
moving them, a dupl/nolintlint/lll run of internal/sync reports 0 issues
(`reports/R6-lint.log` is the 4-issue run).

Parked: R6-1 (ExecutorStore name), R6-2 (StrDownload), R6-3 (fixture file
names). Scratch: `~/.cache/onedrive-go-tmp/r6` (simulator output, logs).

## Review fixes (R6a)

Four hand commits on `a93a60f9`; the generated `3ba8ac60` is unchanged
(reviewer's rerun: tree `3e4f10fd`). Head `fdb15533`.

| Commit | Finding | Fix |
|---|---|---|
| `745a36de` | B1, B2 | localpath's folder-mode gate keys `internal/sync/syncstore.syncStoreDirPerm` and skips the two helper packages; testutil reads `syncdomain/scope_key.go` and leaves the helper packages out of the product sources it counts |
| `e1a95eb2` | S1 | `TestLogSeedReplayOnFailure_ThePlannerFamilyReplaysInThisPackage` in syncplan, seen failing first (wrong package expected); the fake subtest, replay parser and package checks move to `syncfixture/seed_replay.go` |
| `ffe97f62` | S2 | the wrapcheck exclusion matches only tree functions and pointer-receiver methods; calls through tree interfaces stay checked; the three directives are back (wrapcheck: 0 findings) |
| `fdb15533` | S3 | the contract test also parses the imports of every non-test file of the tree, build tags ignored, against its table; seen failing on a planted `syncstore` import in `inotify_other.go`; R-6.10.20 and system.md updated |

Sweep: every Go string literal outside `internal/sync` that names a sync path,
qualified name or moved file name was read. Only devtool's synthetic fixtures
remain, and they need no change.

Checks: `go test -p 1 -count=1 -timeout 120m ./...` behind `heavy.lock`, all
39 packages with tests ok, no failure; lint over the whole repo, e2e tags,
uncapped: `0 issues.` (`reports/R6-lint.log`).


## Merged as #962

- One more hand commit before the PR: `syncfixture/race_enabled.go` still declared the unexported flag (the generator rewrites the Linux and macOS sets without the `race` tag), so every package reading it failed to build under `-race`. Found by the `-race -p 1` run over the tree, devtool and testutil; after the fix that run passed (sync 4,499 s, syncplan 149 s, syncstore 160 s). `go vet` with tags `race,e2e,e2e_full,integration,stress` is clean. `3ba8ac60` is unchanged. Parked R6-4.
- Start (nightly on the base green, nothing claimed or released), pre-pr (`0 issues.`), pre-merge and merge passed; squash-merged as `f0ac23e5` on 2026-11-20, installed (`onedrive-go version f0ac23e5`).
- Live: `TestE2E_Sync_UploadOnly` on the test account, from the branch, passed.
