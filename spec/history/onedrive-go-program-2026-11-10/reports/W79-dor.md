# W79 Definition of Ready

Worktree `~/src/onedrive-go-wt/remote-move-into-recreated-folder`,
branch `fix/remote-move-into-recreated-folder`, HEAD `0104b8f2` (main), tree
clean but for the ignored `.testdata`. Line numbers are of that commit.
Reproductions ran on the base through a probe added with `go test -overlay`
(scratch `w79/probe_test.go`, logs `w79/probe-*.log`); nothing in the worktree
was edited before this report.

1. PASS **Task intent**: a move sent to OneDrive into a folder made here is
   planned only when that folder can exist on OneDrive when the move runs;
   while OneDrive keeps a synced item at the folder's path in this pass the
   move waits for the next pass. Riders: the hold of either side walks up a
   chain of new folders; the dependency rule "a move into a folder below a
   path another move vacates waits for it" finds its partner under OneDrive's
   name folding.
2. PASS **Non-goals**: a folder moved on OneDrive into a new folder made at its
   own path (k to k/k); the executor; `internal/multisync`, `internal/graph`,
   `internal/cli`; planner speed; any planner function outside move
   composition and move dependencies; the create order of a folder OneDrive
   made below a path a local move vacates (seen in (iv), see 12).
3. PASS **Governing docs**: `spec/design/sync-planning.md` (Move Composition
   lines 511-656, Dependency Build 734-749), `spec/requirements/sync.md`
   (R-2.17.1, R-2.17.6), repository `CLAUDE.md` 55-236 and 252-278,
   SUBAGENT-RULES.md.
4. PASS **Authority owner**: move composition (`composeMoves`, emit,
   `planner_move_composition.go`) owns which moves a pass plans and which it
   defers; `buildDependencies` (`planner_dependencies.go`) owns their order.
   No durable state, runtime lifecycle or output changes; a held move is a
   Debug "move waits for the next pass" notice as today.
5. PASS **Code scope**: `internal/sync/planner_move_composition.go`,
   `planner_dependencies.go`, tests in `planner_move_composition_test.go`,
   `engine_move_recreate_test.go`, own lines in `dispatch_order_test.go`;
   `spec/design/sync-planning.md`. Not touched: executor, store, observation,
   other packages.
6. PASS **Regression plan**: tests first, each seen failing on the base:
   composition rows for the first shape, the two-folder shape, the deeper
   chain on both sides; the dependency row "into a new X"; three RunOnce
   scenarios (first shape, two-folder shape, deeper chain on OneDrive), all
   in `runOnceScenarios()`. Reproductions below.
7. PASS **Verification plan**: build, vet (plain and tagged), the new tests,
   `go test ./internal/sync/...` and `-race ./internal/sync/` behind the
   lock, each new RunOnce 200 times, lint once last, `RealRepoIsClean`.
8. PASS **Data safety**: the change only holds moves for a later pass (the
   existing deferral) and adds dependency edges; it plans no delete,
   overwrite or new action type; no store change. The first shape today puts
   f inside the wrong folder on OneDrive; holding it removes that.
9. PASS **Secret and credential safety**: no token, `.env`, account or
   `.testdata` is read or written; tests use invented ids.
10. PASS **Live provider and destructive operations**: none; fakes only, no
    e2e, no push, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns**:
    - Question: does an exact `x2` taken on OneDrive reproduce the first shape?
      Current assumption: only partly (see (i)); the case variant `X2` does,
      fully. Validation step: the RunOnce scenario uses `X2`, the composition
      row an occupant `X2`. Owner path if wrong: `findTargetCollision`.
    - Question: does the two-folder shape need a park? Current assumption: no,
      decision (d) applied on OneDrive opens it (see (iii)). Validation: its
      RunOnce converges in one sync within `oneShotMaxPasses`. Owner path if
      wrong: `parkOneOccupancyCycle`.
    - Seen in (iv), not in scope: on the base the plan creates OneDrive's
      `x/sub` here inside the old `x` before `x` moves, so it is carried to
      `x2/sub` and moved back a pass later.

## Reproductions on the base

(i) First shape. Composition row: x (baseline x, local x2, remote x), f
(f, x/f, f), a new local x, an OneDrive occupant at x2: `findTargetCollision`
(`planner_move_composition.go:497-538`, the occupant claim 516-524) undoes x's
local relocation (`undoLocalRelocations` 378), x's local move back to x finds
the new local x (`destinationFree` 1212-1238) and is deferred (`resolve` 927),
so `recreatedSourceCreates` skips the create of the new x (1637), and f's
remote move f->x/f is planned (`planMove` 949-1020 has no OneDrive-side hold;
`waitsForALocalFolderHolder` 1052 returns at once for a remote move). Probe:
`FIRST local [] remote [f->x/f] deferred map[x:true]`. RunOnce: x renamed
x2, x made, f moved into it here, and on OneDrive `X2` made (a case variant,
which SQLite does not reconcile at x2 but composition folds,
`foldMovePath` 554): pass 1 moves item-f into item-x on OneDrive (graph move
`item-f->f` under the old x), downloads X2; passes 2-4 plan nothing, "move
waits for the next pass" for x and for f (x/f->x/f), StillOpen 2, as long as
X2 lasts. After X2 is removed: one pass moves item-x to x2 carrying f, makes
x; the next moves item-f again: graph moves `[item-f->f item-x->x2
item-f->f]`. With an exact `x2` the collision does not last: SQLite merges a
folder x2 (keep.txt moved into the new x2, item-x stays at x holding f) or
conflict-copies the local x2 for a file x2; both converge in two passes with
the renamed folder's OneDrive identity left at x
(`w79/probe-runonce.log`, `w79/probe-file.log`, `w79/probe-case.log`).

(ii) Second shape. Here x moved into a new f, f into a new x. Composition:
remote moves `[x->f/x f->x/f]`, nothing deferred; with the two creates of
`recreatedSourceCreates` the plan is x's move -> create f -> f's move ->
create x -> x's move (`addParentFolderDep` 219, `movesIntoVacatedPath` 526,
`createsAtVacatedSource` 590) and `isolateDependencyCycles`
(`planner.go:281`) keeps 0 of 4. RunOnce: every pass logs the four actions
dropped as a dependency cycle; nothing ever moves.

(iii) One rule serves both. The hold for OneDrive moves: the move into the
folder waits while the item at its OneDrive path stays there this pass
(decided without an OneDrive move, at its target there, a file or frozen); a
pending folder there does not hold it (decision (d): the first decided moves),
and an item already moved away this pass does not either, because on OneDrive
the folder is created after that move in the same pass. Of two folders each
moving into a new folder at the other's path, the first decided moves (into
the old folder at that path, which leaves a pass later, as on this computer
in the existing row "each into a new folder at the other's path") and the
second waits, because its holder's planned move lands at its own path. That
loop check is the whole of the second shape's fix, estimated at about 20
lines, no park and no create after a park.

(iv) Deeper chain (on OneDrive x->x2, x and x/sub made, f->x/sub/f).
Composition: local `[x->x2 f->x/sub/f]`: `localFolderHolder` (1032-1046)
looks only at `x/sub` (1036), which no synced item holds. RunOnce: pass 1
plans 2 moves and 1 create; f's local move fails "local move destination
parent x/sub is missing" and is superseded (superseded=1); pass 2 plans it
again and converges.

(v) Rider 1, dependency row `LocalMove x->x2`, `LocalMove f->X/f`: deps
`[[] []]`, no edge: `moveDepCandidates` (`planner_dependencies.go:438-461`)
looks up `movesByOldPath` by exact ancestor path (446-449) and
`movesIntoVacatedPath` (526-533) compares exact prefixes.
