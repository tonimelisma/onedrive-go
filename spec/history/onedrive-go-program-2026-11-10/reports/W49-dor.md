# W49 Definition of Ready / Done (written before any product edit)

## Item 1 on the unchanged tree (51e554fa), 10 runs each, plain
- `one run same name` (steps here): 10 runs, 10 failures. Each: OneDrive and this computer both end with the file at `photos/photos/50%.txt`, not `docs/50%.txt`.
- `one run same name on OneDrive`: 10 runs, 0 failures.

## Mechanism (found with planner input logging, since removed; unit row `TestComposeMoves_EntryMovedIntoTheFolderRecreatedAtAMovedFoldersPath/a_file_put_back,_moved_here` fails on base with exactly this)
1. Pass 1: composition resolves the file before its folder (emit order by target depth then path, `planner_move_composition.go:577-585`), so `planMove` finds no earlier move to rebase its OneDrive source through, compares `docs/50%.txt` with its target `docs/50%.txt`, finds them equal (`:935`) and defers it; only the folder moves on OneDrive, carrying the file to `photos/photos/50%.txt`.
2. The same plan keeps SQLite's `baseline_update` for `docs/50%.txt` (one path on all three sides; the rename changed the local ctime), which waits for the new docs (`planner_dependencies.go:219`), so runs after the folder move and records the file at `docs/50%.txt` again, now under the new docs, although OneDrive holds it in the moved folder.
3. Pass 2: `location` resolves that baseline path's parent by path to the new docs (`:254-265`), so `chooseTarget` (`:302-311`) reads the file as unchanged here and moved on OneDrive, and the plan moves the local file back to `photos/photos/50%.txt`.

## Rule put in its place
A move whose target lies at or below the path a same-side move of one of its folders vacates is decided after that folder's move and runs after it: on OneDrive a file's move names the source the folder's move gives it and runs in the same pass; any other such move (a local one, or a folder's) waits for the next pass.

## Folder put back (`mv docs photos/photos; mkdir docs; mv photos/photos/sub docs/sub`, one sync), unchanged tree
Does not fail alike: 10 of 10 pass, and 10 of 10 with the steps on OneDrive. No baseline update is planned for a folder, so its baseline follows the folder move and the next pass moves it. Kept as cases `one run folder same name` (and `... on OneDrive`).

## Item 2 on the unchanged tree (`mv docs archive; mv archive/sub docs`, one sync)
- On OneDrive: fails, cycle as read: local move `docs/sub->docs` waits for `docs->archive` to vacate `docs`; `docs->archive` waits for its child to leave first.
- On this computer: fails with a different cycle. SQLite leaves sub unpaired (`docs/sub` is `local_missing`: the local `docs` row holds the parent's baseline row), so the plan is folder move, remote create `docs`, remote move `docs/sub/inner.txt->docs/inner.txt`, remote delete `docs/sub`; delete waits for the file's move out, which waits for the create, which waits for the folder move, which waits for the delete. The pairing is in the comparison (`sqlite_compare.go`), outside the planner; see the report.

## Files the fix touches
`internal/sync/planner_move_composition.go`, `internal/sync/planner_dependencies.go` (only if the exception goes), tests `engine_move_recreate_test.go`, `planner_move_composition_test.go`, `dispatch_order_test.go` (end of the run-once table), `spec/design/sync-planning.md`.

## DoR checklist
1. PASS **Task intent**: a file put back at its old path in a folder re-created where its moved folder was ends where the user left it after one sync; a child folder moved to its moved parent's old path syncs without a dropped cycle; moves into a re-created folder run in the first pass.
2. PASS **Non-goals**: no store, executor, observation-SQL, chaos, CLI, multisync, graph or e2e change; no new decision-table row; no store generation change; cycle drop stays.
3. PASS **Governing docs**: `spec/design/sync-planning.md` (move composition, R-2.17.6 ordering), `spec/requirements/sync.md` R-2.17.1/R-2.17.6, `CLAUDE.md` conventions.
4. PASS **Authority owner**: move composition (`planner_move_composition.go`) owns where items end and which moves run; `planner_dependencies.go` owns order; the executor and store are untouched.
5. PASS **Code scope**: the files above; not the other branches' files except the end of the run-once table.
6. PASS **Regression plan**: the scenario cases above (item 1 local fails 10/10 on base), the item 2 scenario, the composition rows (fail on base), rewritten dependency rows.
7. PASS **Verification plan**: each new case 20/20 plain and 20/20 under `-race`; `go test ./internal/sync/...`; race on the touched tests; build, vets, gofumpt, convention test, lint behind the lock.
8. PASS **Data safety**: a move is rebased only for a file on OneDrive whose folders up to the moving one all stay with it; a chain through a folder SQLite deletes keeps today's order, so a remote folder delete never loses its wait for a move out of it.
9. PASS **Secret and credential safety**: no tokens, accounts or `.testdata` read or written.
10. PASS **Live provider and destructive operations**: none; scenario fakes only.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns**: Question: does the exception for a move into a re-created folder become unneeded? Current assumption: yes, once such moves are rebased or wait. Validation step: remove it and run the re-created-folder tests. Owner path if wrong: `planner_dependencies.go` keeps it and the report names the case.
