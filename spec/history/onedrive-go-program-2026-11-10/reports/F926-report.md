# F926 report

Commit `4e7b9c76` on `fix/move-order-into-recreated-folder`, not pushed.

## What changed

- `planner_dependencies.go`: `movePrecedes` gains `movesIntoVacatedPath` (a
  same-side move landing strictly below the path another move vacates, from a
  source outside it, waits for that move); `addMoveDeps` rule comment.
- `planner_move_composition.go`: `planMove` holds, through
  `waitsForALocalFolderHolder`, a local move into a folder OneDrive made at a
  path a synced item relocated here still holds (`localFolderHolder`) once that
  item is decided (`landsWhereADecidedHolderIs`), and that item's own local
  move once such a move is planned (`holdsALocalLanding`);
  `parkOneOccupancyCycle` does not park such an item locally; header comment.
- `planner_move_composition_test.go`: two tests added, one expectation updated.
- `spec/design/sync-planning.md`: Move Composition and `buildDependencies`
  paragraphs.

## Tests

- `TestComposeMoves_FolderMovedIntoAFolderMadeWhereAnotherFolderLeftWaits`:
  pass-1 composition (renamed, same name, holder decided after the mover, two
  folders each moved into a new folder at the other's path); moves, deferrals,
  no cycle. Fails on the base in all 4 rows.
- `TestBuildDependencies_MoveIntoThePathAnotherMoveVacatesWaitsForIt`: the
  edge here and on OneDrive; a rename below the path and the other side are
  not held. Fails on the base in 2 of 4 rows.
- `TestBuildDependencies_FileRebasedBelowAnotherFoldersVacatedPath`: `f`'s
  move also waits for `x`'s directly (`{0, 3}`, `{0, 2}`), the cycle and kept
  counts unchanged: the new edge, redundant there with the chain through the
  new `x`.
- The scenario test is unchanged; made on OneDrive it now takes three working
  passes and a quiet one in one sync (x; new x and f; new f and b.txt).

## Checks

- Scenario `-count=500 -timeout 60m` behind the lock: ok, 500/500 (1505 s).
- build, vet, vet with e2e/e2e_full/integration/stress tags, gofumpt: clean.
- `go test ./internal/sync/...` behind the lock: ok after the expectation
  update (the first run failed only there).
- `-race` on the touched files' tests (`TestComposeMoves_`,
  `TestBuildDependencies_`, `TestIsolateDependencyCycles_`,
  `TestDropReconciliationOfComposedMoves_`, `TestPath*`) and the scenario: ok.
- RealRepoIsClean: four PASS. Lint: 0 issues (a first run flagged gocyclo on
  `planMove`; fixed by the helper, tests and lint rerun).

## Deviations

- "Whichever composition decides first moves" instead of waiting for the
  holder: with the holder decided later (x moved deeper), f moves into the old
  x and x waits a pass; ordered, converges, two extra local renames. Waiting
  would stall two folders each moved into a new folder at the other's path.
- The commit order is not a separate defect: the worker commits before the
  engine releases dependents (`worker.go:236-252`); no separate test.

## Also fixed

- None.

## Seen and not fixed

- `planner_move_composition.go:1600` (`recreatedSourceCreates`), `:887`
  (`resolve`): the remote twin when the holder's remote move waits. Made here:
  x moved to x2, new x, f moved into it, while OneDrive holds an unsynced entry
  at x2: composition plans `remote f -> x/f`, defers x, and the create of the
  new x is dropped with x's move, so f lands in the old x on OneDrive (checked
  with composeMoves; scratch `f926/remote-holder-deferred-check.txt`). Fix:
  mirror the rule on the remote side, with a test.
- `planner_move_composition.go:946` (`planMove`): a folder moved on OneDrive
  into a new folder made at its own path (k to k/k) is planned as the local
  rename `k -> k/k`, into its own subtree, which rename(2) refuses (checked
  with composeMoves; scratch `f926/self-nesting-check.txt`). Fix: park it
  beside its path first, as `parkOneOccupancyCycle` does; tests at the
  composition and RunOnce levels.

## Unfinished / for the reviewer

- Nothing unfinished. Only `internal/sync` changed; `internal/multisync` and
  `internal/cli` were not run.


## Review fixes (recorded by the orchestrator, 2026-11-15)

The fix agent's own write of this section was refused. This record is
from what was checked in the code after its hand-back; paths are under
`internal/sync/`.

- Finding 1: `cannotLandInTheHolder` (`planner_move_composition.go`,
  was `landsWhereADecidedHolderIs`) also holds a move whose holder is
  a file or a frozen folder. Rows `a file x moved deeper, decided
  after f` and `a frozen x moved deeper, decided after f` of
  `TestComposeMoves_FolderMovedIntoAFolderMadeWhereAnotherFolderLeftWaits`;
  `TestRunOnce_FolderMovedOnOneDriveIntoAFolderMadeWhereAFileLeftConverges`,
  also in `runOnceScenarios()`.
- Finding 2: the holder check runs after the loop, on the final new
  path; row `inside a folder OneDrive moved`.
- Finding 3, composition half: the holder is looked up with
  `occupantAt`, so a new folder X is held as x is; row `a new folder
  X`. The dependency half is not done: `moveDepCandidates`
  (`planner_dependencies.go`) looks up partners by exact path.
  Composition already holds the pair, so the edge is a second guard.
  Home: a rider on plan row W79.
- Finding 4: the term stays, held by
  `TestComposeMoves_FolderAMoveLandsInIsNotParkedToBreakASwap`.
- Finding 5 and notes 6, 11 and 12: done; the section of
  `spec/design/sync-planning.md` is rewritten.
- Note 10 (a move into a deeper new chain goes stale on the first
  pass): not changed here; same rider on W79.
- Mine: the comment in `executor.go` above
  `validateLocalMoveParentExists` no longer tells history.
- The agent's checks, by its account: build, both vets, gofumpt, the
  targeted tests plain and with the race detector,
  `TestReverseDispatch_RunOnceScenariosPass`, the whole
  `./internal/sync/` package plain, `RealRepoIsClean`. Mine, after the
  rebase onto `41919e44` (commit `dac58b56`): both vets; lint and the
  pre-pr gate are recorded in the merge section.


## Merged as #932

Squash commit `0104b8f2` on main, 2026-11-16 01:01 PST. The branch was
one commit, `dac58b56`, on `41919e44`. All of this ran on that commit:

- Lint of `./internal/sync/...`: 0 issues.
- `RealRepoIsClean`: four PASS. Start stage: no nightly claim.
- Pre-PR gate (`verify --dod --stage pre-pr`): passed 00:16 PST.
- Planner, dependency and scenario tests with the race detector:
  ok, 270 s.
- The once-flaky scenario test 500 times in a row: ok, 1835 s.
- Pre-merge: PR CI (Linux and macOS shards) and review threads passed.
- Live, test account, private folder removed afterwards (00:01 to
  00:03 PST): the folder case and the file case each converged in one
  sync, exit 0, moves by rename, nothing in the trash; a second sync
  was up to date.

The PR was opened while the race and repeat runs were still going, and
its description said so; the final line replaced it before the merge.
