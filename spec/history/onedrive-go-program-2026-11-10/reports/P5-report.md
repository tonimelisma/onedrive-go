# P5: pre-split sync rows (W57, W34, W20)

Branch `misc/pre-split-sync-rows`, worktree `~/src/onedrive-go-wt/pre-split-sync-rows`, made with `devtool worktree add` from origin/main `a8ad2f27`. Pushed; no PR, no install.

| Commit | Row | State |
|---|---|---|
| `4930b912` | W57 crash model holds the replacing move's crash point | cherry-pick of `c6e273a6`, clean, unchanged (test files plus one line of `sync-engine.md`) |
| `8ae31ebf` | W34 `status --perf` counts a committed transaction once | done |
| `cc7cdf72` | W20 the account identity check never fails or stalls a command | done, rider included |

## Old worktrees (left as they are)

`identity-check-not-a-precondition` and `perf-tx-counted-once` both sit at `0ff7c754` (#911) with no commits of their own and **no uncommitted changes**: only the untracked `.testdata` link. `crash-model-replacing-move` is also clean apart from `.testdata`. Main did not already hold W34 or W20, so both were built from scratch on the new branch.

## W34 (`8ae31ebf`)

- Confirmed: `perfTx.Commit` and `perfTx.Rollback` each called `recordPerfTx`, and the `finalizeTxRollback` every store function defers called `Rollback` again after a successful commit.
- Fix in `internal/sync/tx.go` only. `perfTx` carries a `recorded` flag, and its `record()` method replaces `recordPerfTx`. `Commit` always records. `Rollback` records unless it returned `sql.ErrTxDone`. Callers are unchanged. Commit and Rollback are never called from two goroutines (a perfTx belongs to the function that defers its rollback), so a plain bool is enough, and the type's comment says so.
- Deviation from the literal rule: a rollback that returned a driver error other than `ErrTxDone` still ended the transaction, because database/sql had already marked it done. So it records, which keeps "exactly once".
- A failed commit, from `database/sql` source (Go 1.27): `Commit` CAS-sets `done` before calling the driver. After a driver commit failure, the deferred `Rollback` therefore returns `ErrTxDone`: recorded once, by the commit. I could not produce that case with SQLite in a test without contriving a driver failure. A commit refused because the context ended returns before the CAS. Then the context watcher (`awaitDone`) or the deferred `Rollback` does the rollback, and the transaction is still recorded once. That case is tested.
- Tests (`tx_test.go`, cite R-6.6.15; no requirement defines what the count means):
  - `TestPerfTx_RecordsEachEndedTransactionOnce`: failed on the base with 3 for 2 transactions.
  - `TestPerfTx_RecordsTheTimeFromBeginToCommitOnce`: failed with count 2 and 2000 ms where 1000 ms was expected.
  - `TestPerfTx_RecordsAFailedCommitOnce`: failed with 2.
  - All three pass after the fix.
- Sweep:
  - `engine_current_plan.go:516`: a read transaction that only rolls back. Recorded once, on that rollback.
  - `schema.go:351`, `shortcut_root_store.go:132`, `store_inspect.go:327`: raw `sql.Tx`, never recorded, so no double count. Parked as P5-p1 and P5-p2.
  - `pending_create_lifetime_test.go` only asserts positive or zero counts, so it is unaffected.
- Docs: `spec/design/system.md` perf section, one sentence plus the test names.

## W20 (`cc7cdf72`)

- `probeAccountIdentity(ctx, cid, scope) (config.EmailReconcileResult, error)` keeps its signature. The errors it can still return:
  - the catalog cannot be read;
  - a sign-in failure (`isSignInRequired`), from loading the saved login or from the request;
  - an email change that could not be applied (`reconcileGraphUser`).
  
  Any other failure is logged at Debug and the command goes on with the stored identity.
- Inside it, `askAccountIdentityIfDue` makes the request under the budget (it returns `(user, answered, err)`, because `nilnil` lint is on), and `applyAccountIdentity` reconciles and records the next check. `probeAccountIdentities` asks several accounts at the same time through `runBounded`, under one budget, then applies the answers one after another in order, so two email changes never run at once.
- Budget: `identityCheckBudget = 5s`, with the cause `errIdentityCheckBudgetSpent`, and the seam `cc.seams.identityCheckBudgetContext`. It shares the new `budgetContext` helper with status's `liveOverlayBudgetContext`; each keeps its own constant.
- When due: the catalog field `CatalogAccount.IdentityCheckDueAt` (`identity_check_due_at`) holds RFC 3339 text in UTC, the way `CachedAt` is stored. The constants are `config.IdentityCheckAnsweredInterval` (24 h) and `config.IdentityCheckUnansweredInterval` (15 min).
  - `CatalogAccount.IdentityCheckDue(now)` treats a missing time, an unparsable time and a time more than 24 h ahead as due.
  - `config.RecordAccountIdentityCheck` writes under `UpdateCatalog`, the catalog lock. Login records the 24 h through the same `recordIdentityCheck` method inside `recordLogin`.
  - The clock is a new `cc.seams.now`; the CLI had no clock before.
- What gets recorded:

  | Outcome | Next check |
  |---|---|
  | Answered and applied | 24 h, under the renamed ID after a change |
  | Failed, or the budget ran out | 15 min |
  | Command's own context ended | nothing |
  | Sign-in failure | nothing |
  | Change that could not be applied | nothing, so the advice "stop that sync, then run the command again" still works |

  A catalog write that fails is logged at Debug.
- A failed apply, per caller (unchanged):

  | Caller | What a failed apply does |
  |---|---|
  | `Session` (file commands) | fails the command |
  | `sharedBootstrapMetaClient` | fails the command |
  | `sharedTargetClients` | fails the command |
  | `reconcileSyncAccounts` (sync start) | Debug, carries on |
  | account views (`drive list`/`search`/`add`, `shared`) | Debug, carries on |

- Rider: `loadDriveListSnapshot` runs `discoverAvailableDrives` beside `discoverSharedTargets`, both through `runBounded`. A top-level panic becomes a degraded notice for each account. `discoverSharedTargets` searches its accounts at the same time, merges the results in account order, and turns a panic into that account's degraded notice. Discovery has no budget.
- New requirement R-6.8.31, in the R-6.8 section after R-6.8.29. R-6.8.30 is taken by R4 (refactor/token-saves-and-refresh), so W20 uses .31. It is cited from the tests and from `cli.md`'s Implements line.
- Docs:
  - `spec/design/cli.md`: the paragraph that said no command decorates local state with a live request, plus a new table row naming all 12 tests;
  - `spec/design/config.md`, Email Reconciliation: the due-time record;
  - no user-facing doc claimed a check on every command.
- Tests are in `internal/cli/identity_check_test.go`, plus `TestLogin_RecordsTheIdentityCheck` in `internal/config/drive_enroll_test.go`. Before the fix, each was run on its own against the old code with a 90 s deadline:

  | Test | Before the fix |
  |---|---|
  | `TestSession_AFailedIdentityCheckDoesNotFailTheCommand` | hung (90 s deadline; the old code waits out a 1 h Retry-After with no deadline) |
  | `TestSession_AnIdentityCheckThatDoesNotAnswerEndsAtItsBudget` | did not return within 30 s |
  | `TestSession_ASignInFailureFromTheIdentityCheckStillFailsTheCommand` | passed (pin, by design) |
  | `TestProbeAccountIdentity_SkipsWhileNotDue` | 1 request where 0 were expected |
  | `TestProbeAccountIdentity_AsksWhenDue` (4 cases) | due time not recorded |
  | `TestProbeAccountIdentity_AFailureIsRetriedAfterFifteenMinutes` | hung |
  | `TestProbeAccountIdentity_AnEmailChangeIsStillAppliedWhenDue` | due time not recorded |
  | `TestProbeAccountIdentity_AnInterruptedCheckRecordsNothing` | returned an error |
  | `TestAccountViewSnapshot_AsksEveryAccountAtOnceAndKeepsEachRecord` (4 accounts that answer only once all have asked) | did not return |
  | `TestSharedTarget_AFailedIdentityCheckDoesNotFailTheCommand` | hung |
  | `TestDriveList_ASlowIdentityAnswerDoesNotDelayTheListing` | did not return |
  | `TestLogin_RecordsTheIdentityCheck` | not run before the fix: the API it tests is new, so it would only have failed to compile |

  All pass after the fix. No test sleeps for a budget. The stand-in budget closes a channel, and its context carries a 10 min deadline, as the status tests' does, so the transport returns a 503 with a 1 h Retry-After at once (R-6.8.8).
- Not done: the live proof (time `ls` twice with `--debug`; the second run sends no `GET /me`). That is yours.

## Checks run

- `go build ./...`, `go vet ./...` and `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: OK.
- `gofumpt -l` and `goimports -l` over the touched packages: clean.
- `go test -p 1 -count=1 ./internal/cli/... ./internal/config/... ./internal/perf/...`: all OK.
- Focused runs: `TestPerfTx_|TestFinalizeTxRollback` in `internal/sync` OK. The cli selection `TestSession_A|TestProbeAccountIdentity_|TestAccountViewSnapshot_|TestSharedTarget_AFailedIdentity|TestDriveList_|TestCLIContextSession_|TestShared|TestStatus_` OK.
- `go test -p 1 -count=1 -timeout 90m ./internal/sync/...`, once, behind `heavy.lock`: OK (1291 s).
- Spec citations checked by hand: every test name added to a spec exists exactly once, and every requirement id cited exists.
- Not run, as instructed: `-race`, `devtool verify`, golangci-lint (it needs about 4 GB with memory short; CI's lint job will run it), live e2e.

## Process note

In the main checkout I once ran `git checkout --detach` by mistake while reading a file. I restored `main` at once at the same commit `a8ad2f27`, with no file changes.

## Parked

Added to PARKED.md: P5-p1 (raw `sql.Tx`s missing from `status --perf`), P5-p2 (`schema.go` joins `ErrTxDone`), P5-p3 (interrupted `drive list` starts no discovery, so it shows no degraded notice).

## Review fix R-P5a (`969fbfb8`, on top of `cc7cdf72`)

- B1: the identity-check fan-out, the per-account shared search and the drive discovery beside it called the lazy `cc.runtime()` from goroutines, and the race detector flagged it in the drive list tests. Each fan-out now takes the runtime once on the command goroutine and passes it down: `probeAccountIdentities` passes it to `askAccountIdentityIfDue`/`askAccountIdentity`, and `loadDriveListSnapshot` passes it to `discoverAvailableDrives` and `discoverSharedTargets`, whose callers in `drive_add.go`, `shared.go` and the tests now pass it too. It reaches `discoverSharedTargetsForAccount` the same way. No other production call of `cc.runtime()` runs on a goroutine: `drive_search` is sequential, `status` already passes the runtime down, and `discoverDrivesForToken` gets it as a parameter.
- S1: renumbered R-6.8.30 to R-6.8.31 in all six files.
- Checks: `go vet ./...` and the e2e-tag vet: OK. `go test -race -p 1 -count=1 ./internal/cli/...` behind `heavy.lock`: OK, with no DATA RACE. `TestDriveList_JSONKeepsSharedEntryWithoutOwnerIdentity`, `TestDriveList_NamesASiteWhoseLibrariesCouldNotBeListed` and `TestAccountViewSnapshot_AsksEveryAccountAtOnceAndKeepsEachRecord` were confirmed passing under `-race`. The config login tests pass.


## Merged as #959

- Squashed to one commit and rebased onto `f2250310`; the one conflict was the requirement list (main's R-6.8.30 and this change's R-6.8.31, both kept). No behaviour conflict.
- `go vet` plain and with all tags clean. `-race -p 1` of `internal/cli`, `internal/config` and `internal/sync` passed (sync 4849 s). `TestModel_CrashAtSeededPointsRecovers` passed over the seeds of the nights of 2026-11-20, -21 and -22 UTC (12 each).
- pre-pr's lint flagged one `nilerr` (the command's own stop in `askAccountIdentityIfDue`); fixed with a reasoned `nolint`, then pre-pr passed.
- The start stage claimed the four failures of the nightly on `a8ad2f27` (two `internal/sync` package timeouts, one stress failure); none is fixed here, all released (already parked as R2-n1, R2-n2, W101-n2).
- Merged `899617bc`, post-merge passed, installed `899617bc`. Live on the test account: `drive list` 2.9 s, no error; `TestE2E_DriveList_HappyPath_Text` passed.
