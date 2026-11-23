# F926 Definition of Ready and Definition of Done

Worktree `~/src/onedrive-go-wt/move-order-into-recreated-folder`, branch
`fix/move-order-into-recreated-folder`, base `a0dbe10b`.

## Findings (established before any product edit)

### Step 1: what differs between a failing and a passing round after pass 1

Evidence: temporary trace lines at the commit point
(`store_write_baseline.go`, `CommitMutation`, after `applyOutcomeTx`, printing
the action and the baseline rows of item-x, item-f, item-b), case
`renamed on OneDrive`, run with `-count=150` and `-count=300` (11 failing
rounds of 300). Logs: scratch `f926/trace1.log` (pass), `f926/trace3.log`.

- Pass 1's plan is identical in every round: `local_move item-x x -> x2` and
  `local_move item-f f -> x/f`, no edge.
- Passing round (trace1.log:21-28): rename f -> x/f into the old x, then
  x -> x2. Commit order f, then x: f's commit writes item-f at `x/f`; x's
  commit (`commitMove`, `store_write_baseline.go:858`) repaths the baseline
  subtree `x/` -> `x2/` (`:884-888`, `repathSubtreeTx`), so item-f is at
  `x2/f`, which is where it is on disk. Pass 2 moves it locally to `x/f`.
- Failing round, wrong tree (trace3.log:95-102): both renames start, f's
  rename into the old x happens before x's rename (disk ends `x2/f`), but x's
  executor returns and commits first (item-x at `x2`, item-f still at `f`),
  then f's commit writes item-f at its executed destination `x/f`
  (`commitUpsert`, `:872`). The baseline says `x/f`, the disk holds `x2/f`:
  pass 2 reads the local position as the user's move and plans
  `remote_move item-f x/f -> x2/f` (trace3.log:123).
- Failing round, error (trace3.log:12, 31): x's rename runs first; f's rename
  then fails with ENOENT (`renameat f x/f`), one error in the pass.
- The commit happens in the worker before the result reaches the engine
  (`worker.go:236-252`), so a dependency edge also fixes the commit order:
  with an edge the second action starts after the first committed. The commit
  order is not a separate defect; it matters only for two actions without an
  edge whose paths interact.

### Step 2: why the local creates of the new x and the new f are not in pass 1

By reading (confirmed by the trace: `RECONCILED 0 actions` in pass 1,
`folder_create local x/f` in pass 2):
- One comparison row per path. At `x` the baseline holds item-x, which OneDrive
  moved to `x2`: `sqlite_compare.go:526` classifies the path
  `remote_move_source`, `:649` reconciles it as `remote_move`, and
  `planner_actions.go:494-496` builds no action for a move (composition owns
  moves). The new OneDrive folder at the same path has no row of its own. Same
  for `f`.
- Composition plans the twin only for a rename and recreate made here
  (`recreatedSourceCreates`, `planner_move_composition.go:1538`, from
  `inputs.recreated`, filled at `:1260` only for `local_move_source` with a
  local entry present).
- In pass 2, with the baseline at `x2`, `x` is `remote_only_create` and
  `folder_create_local` (`sqlite_compare.go:536`, `:661`).

### Third unknown: may a pass hold a move whose destination parent is neither on disk nor created?

Today it can. `parentState` settles an unsynced target parent without a
synced ancestor (`planner_move_composition.go:806-810`); `destinationFree`
checks only the destination; `localParentPresent` (`:1113`) is used only for
a move an ancestor's move carries. The executor refuses a missing parent
(`executor.go:518-530`). R-2.17.6 says a move runs only after its target
parent is in place.

### Remote twin

Case `renamed` (steps made here), traced (scratch `f926/trace4.log:25-28`):
`remote_move x -> x2`, `remote_move f -> x/f deps=[create x]`,
`folder_create remote x deps=[x's move]`, `folder_create remote f deps=[f's
move]`. The chain through the re-created x orders the two moves: no gap.

## Decision

Candidate (ii): the edge in the dependency rules (a same-side move whose
destination lies strictly below the path another same-side move vacates, and
whose source does not, waits for that move) plus, in composition, a local move
into a folder that is not a synced item, at a path that a synced item
relocated on this computer holds in the local snapshot, is never planned
together with that item's own local move: whichever composition decides first
is planned, the other waits for the next pass. On this computer the new folder
is created only in a later pass (step 2), so the move cannot land in it in
this one. Not (i) alone: with the edge and no create the move into x fails
every time (deterministic error). Planning the create of OneDrive's new x in
pass 1 (the plan "holds the create between them") would be a new
reconciliation rule for `remote_move_source` paths, larger than this change.
"Whichever is decided first" rather than "wait for the holder": a wait closes
loops (two folders each moved into a new folder at the other's path) that
would then never converge, and with the edge both moves of such a pair would
be a dependency cycle.

## DoR (12 items)

1. PASS **Task intent**: one sync of "x moved to x2, new x, f moved into it, new
   f, b.txt moved into new f" made on OneDrive never moves a folder on OneDrive
   to a place nobody put it, and no pass's result depends on which of two
   unordered moves runs or commits first.
2. PASS **Non-goals**: planning OneDrive's rename-and-recreate creates in the
   same pass; the remote-side holder case (see report); `internal/cli`,
   `multisync`, `graph`, `devtool`, e2e, workflows; no store generation, no
   new outcome kind or issue type.
3. PASS **Governing docs**: `spec/design/sync-planning.md` (Move Composition,
   Dependency Build, the `buildDependencies` paragraph), `spec/requirements/sync.md`
   R-2.17.6, R-2.17.1; repo `CLAUDE.md` 55-236.
4. PASS **Authority owner**: move composition (`planner_move_composition.go`)
   owns which moves a pass plans; `buildDependencies`
   (`planner_dependencies.go`) owns their order; the store owns the baseline
   (unchanged); the executor (unchanged) refuses a missing parent.
5. PASS **Code scope**: `internal/sync/planner_move_composition.go`,
   `planner_dependencies.go`, tests in `planner_move_composition_test.go` /
   `planner_dependencies_test.go`, possibly `engine_move_recreate_test.go`;
   `spec/design/sync-planning.md`. Not touched: `executor*.go` (another branch
   edits `executor.go`; only my temporary trace is there and it is removed),
   `actions.go`, `crash_points.go`, `executor_conflict.go`.
6. PASS **Regression plan**: a composition-level test of this scenario's
   pass-1 inputs that fails on the base every time (base plans both moves),
   a dependency test for the new edge (base has no edge), the scenario case
   500/500.
7. PASS **Verification plan**: build, vet (plain and e2e,e2e_full,integration,
   stress tags), gofumpt, `go test ./internal/sync/...` whole behind the lock,
   `-race` on the new and touched tests, RealRepoIsClean, the `ListsEvery`/
   `Table`/`Governs`/`Exhaustive`/`Registered` tests, the 500-round run, lint
   last.
8. PASS **Data safety**: the change only plans fewer moves in a pass (a move
   waits for the next pass) and adds an order between two moves; no delete,
   no overwrite, no store change.
9. PASS **Secret and credential safety**: no tokens, `.env`, `.testdata` or
   accounts read or written; tests use fakes.
10. PASS **Live provider and destructive operations**: none; no live test, no
    OneDrive command, no history rewrite, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does the new edge close a dependency cycle in any plan the
      package tests build? Current assumption: no (the analysed pairs are
      consistent). Validation step: whole `./internal/sync/...` suite, including
      the scanning-oracle and model tests. Owner if wrong:
      `planner_dependencies.go`.
    - Question: does deferring the second of the two moves change an existing
      scenario's pass count beyond the four-pass bound? Current assumption: the
      `renamed on OneDrive` cases take three working passes and one quiet one.
      Validation step: the scenario test and the 500-round run. Owner if
      wrong: `planner_move_composition.go`, `engine_run_once.go`.

## Definition of Done

- The new composition and dependency tests fail on the base and pass after.
- `TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges`
  passes 500 of 500.
- Whole `./internal/sync/...` suite green; race runs of the touched tests green;
  build, vet, gofumpt, RealRepoIsClean, lint 0 issues.
- Rule comment in `planner_dependencies.go`, composition comments and
  `spec/design/sync-planning.md` describe the new rules with test names that
  exist.
- No `tmpTrace` anywhere in `internal`; one commit, not pushed.
