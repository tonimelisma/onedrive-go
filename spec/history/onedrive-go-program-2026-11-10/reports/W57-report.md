# W57 report: the crash model draws the crash point of the replacing move

Branch `test/crash-model-replacing-move` (worktree `~/src/onedrive-go-wt/crash-model-replacing-move`, made with
`devtool worktree add`), one commit `c6e273a6` on origin/main `99c582d4` (#948). Test file and one spec row only.
Not pushed, no PR, no gate (start stage not run either), no install (test-only).

## Definition of ready (on `99c582d4`; every line number of the brief checked and correct there)

- Open seed issues: `gh issue list --state open` lists none (2026-11-18), so no open issue's replay can change.
  The plan row's `:319`/`:278` and its seeds 318011/318012 are stale; those rows merged.
- (i) No test fails on an unlisted crash point. No `_test.go` parses `crash_points.go` (its only mention is the
  comment at `crash_points_test.go:23`); `allCrashPoints` (`:355`) is a hand list of 9 of the 10 constants,
  without `crashPointLocalMoveDestinationTrashed` (`crash_points.go:21`).
- (ii) Trees (`crash_points_test.go:314-321`): `docs/a.txt`, `docs/b.txt`, `e.txt`, `f.txt`, `g.txt`, `keep.txt`,
  synced by one pass (`:324`), then `wal_checkpoint(TRUNCATE)` (`:327`). Round (`:330-336`): local rename
  `docs/a.txt`->`a2.txt`; remote move `docs/b.txt`->`b2.txt`; local create `c.txt`; remote create `d.txt`;
  remote edit `e.txt`; remote delete `f.txt`; local delete `g.txt`. A replacing move is a remote delete of a
  synced file plus a remote move of another synced item onto its name: the composer marks it
  `moveDisplaceReplace` -> `ReplaceLocalTarget` (`planner_move_composition.go:1567-1569`), and the executor's
  `clearReplacedLocalMoveTarget` takes the verified file out (`executor_preconditions.go:832-853`, point at
  `:841-842`) before the rename. It fits after the existing edits. Placement matters: with `r.txt`->`s.txt` at
  the root the replacing move dispatched before `docs/b2.txt` and so took the first visit of
  `exec.local_move_done`; inside `docs` (`docs/r.txt`->`docs/s.txt`) it dispatches last (probe of the dispatch
  order and every hook call, not committed).
- (iii) All 9 listed points are reached today; each capture takes the first visit only (`hook`, `:65-71`).
  First visits, in dispatch order (c, d, e, f, g, docs/a2, docs/b2): `store.commit_observation` (pass start),
  `upload.created_unrecorded` and `upload.remote_done` (c.txt), `store.commit_outcome` (c.txt), `download.renamed`
  (d.txt), `delete.local_trashed` (f.txt), `delete.remote_done` (g.txt), `exec.remote_move_done` (docs/a2.txt),
  `exec.local_move_done` (docs/b2.txt). After the change the same, then the new point (docs/s.txt). An unreached
  point fails the every-point test in `reopen`: `require.True(c.captured, "crash point %s was never reached")`
  (`:256`). The model instead skips recovery when nothing was captured (`engine_model_based_test.go:330`): an
  unreached draw is a batch without a crash, silently.
- (iv) Each seed has its own crash stream, `rand.NewPCG(seed^0xc2b2ae3d27d4eb4f, seed)` (`:313`), apart from the
  edit stream (`:302`). Every batch calls `armCrash` (`:327`): `points[rng.IntN(len(points))]`, then
  `rng.IntN(2) == 0` for the lost WAL (`:368-369`). The model never uses `crashRecoveryScenario`, so the
  scenario change touches no seed; the longer list does. `IntN(n)` takes one `Uint64` and returns
  `floor(u*n)` (rejection below `n/2^64`), so each batch still takes two values and the lost-WAL draw is the
  same; `floor(10u)` equals `floor(9u)` for half of all `u`, otherwise it is one higher. Computed offline for
  the 40 seeds 1-4 and the three days: 111 of 240 batch draws change point, 27 draw the new point, and only
  seed 320003 draws the same six points as before. A seed replays as before up to its first changed batch.
- (v) The lost-commits half: the every-point test runs `lostWAL` false and true for every point of
  `allCrashPoints` (`:430-432`); `lostWAL` keeps only the main database file (`captureDatabase`, `:94-96`),
  checkpointed before the round. Same list, so adding the point runs both halves for it.
- (vi) A night runs 12 crash-family seeds: `seedsEnv` sets `ONEDRIVE_GO_MODEL_SEED_START` and
  `ONEDRIVE_GO_MODEL_SEEDS=12` (`verify_seeds.go:26`, `:127`, `:137-151`), which the crash family reads
  (`modelSeedVars`, `engine_model_based_test.go:161`; the env count wins over `modelCrashSeedCount` 4). Start
  `1 + 1000 * days since 2026-01-01 UTC` (`:115-122`): 11-16 = 319001, 11-17 = 320001, 11-18 = 321001.
  My run of 11-18's 12 seeds on `99c582d4`: 12 PASS, 44.8 s wall (prebuilt binary, behind the lock).

## What changed (`internal/sync/crash_points_test.go` +103/-8, `spec/design/sync-engine.md` one row)

- `crashRecoveryScenario` also syncs `docs/r.txt` and `docs/s.txt`; after the existing edits OneDrive deletes
  `docs/s.txt` and moves `docs/r.txt` onto its name. The dispatch order and every first visit of the other
  points are unchanged (probe above).
- `allCrashPoints` holds `crashPointLocalMoveDestinationTrashed`, last; both halves of the every-point test run
  at it. Every case also asserts that the file displaced at `docs/s.txt` is in the trash once
  (`assertTrashHoldsReplacedOnce`), and the test's Validates line adds R-2.17.9.
- New `TestAllCrashPoints_HoldsEveryDeclaredCrashPoint` (R-6.5.2): reads the const block of `crash_points.go`
  with `go/parser`; fails naming each constant that `allCrashPoints` does not hold exactly once, and each list
  entry no constant declares.
- `sync-engine.md:110`: the point joins the list of points; the sentence saying it had only its own test, no
  power loss and no draw now says its own test also holds the state the crash leaves. All four test names the
  row cites exist and ran.

## Seen failing first

- The new test on the old list: `allCrashPoints holds crashPointLocalMoveDestinationTrashed
  ("exec.local_move_destination_trashed") once`, expected 1, actual 0.
- Point listed, scenario unchanged: both subtests failed `crash point exec.local_move_destination_trashed was
  never reached` (no silent hole). With the scenario change both pass.
- The trash-once assertion failed in every case in its first form, which counted `e.txt`'s entry too (the
  download of the remote edit also trashes under `replaced`); it now matches the path.

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: clean. gofumpt and
  goimports: clean. Plan-id and process-word check on the commit: clean.
- `go test -count=1 -run "TestCrashRecovery_|TestModel_Crash|TestAllCrashPoints_" ./internal/sync/` behind the
  lock: ok (33 s); the same with `-race -timeout 30m`: ok (349 s).
- `go test -count=1 -timeout 60m ./internal/sync/...` behind the lock: ok (370 s).
- `go test -race -count=1 -timeout 90m ./internal/sync/` behind the lock: ok (4243 s).
- golangci-lint over `./internal/sync/...` behind the lock, last: `0 issues.` (`scratchpad/w57/W57-lint.log`). A first
  run flagged `rangeValCopy` in `assertTrashHoldsReplacedOnce`; fixed by indexing, commit amended from `e43757c7`
  to `c6e273a6`, crash tests rerun behind the lock: ok. The amend touches only that helper, which the model
  does not use, so the seed runs on `e43757c7` stand.
- `RealRepoIsClean`: 4 PASS.
- Seeds, `TestModel_CrashAtSeededPointsRecovers` on `e43757c7`, a prebuilt binary, one day per run, behind the
  lock: 319001-319012 12/12 PASS (34.7 s wall), 320001-320012 12/12 PASS (31.8 s), 321001-321012 12/12 PASS
  (35.4 s).
- Extra, with a build overlay that logs each draw (worktree untouched): of 216 batch draws, 87 reached their
  point; the new point was drawn 26 times and reached 0 times.

## Failing seeds

None: 36 of 36 passed.

## Seen and not fixed (2)

- `engine_model_based_test.go:365-381` and the model's edits (`engine_model_ops_test.go:91-106`): the model draws
  the new point but its edits never reached it over three days (0 of 26 draws): a remote delete followed by a
  remote move onto the freed name, with the local copy unchanged, comes up only by chance. More widely, 129 of
  216 draws crashed nowhere, and such a batch runs without a crash, silently. Ready plan-row line:
  `| Wnn | The crash model's edits reach the replacing move's crash point | test/model-replace-by-move | s |
  planned |  |  |`. Change: a remote operation that deletes a synced file and moves another synced item onto
  its name. Reproduce the finding: `ONEDRIVE_GO_MODEL_SEED_START=319001 ONEDRIVE_GO_MODEL_SEEDS=12 go test
  -overlay scratchpad/w57/overlay.json -v -run '^TestModel_CrashAtSeededPointsRecovers$' ./internal/sync`
  (the overlay logs `crash-draw batch N point P lostWAL B reached B` after every batch's sync). It renumbers
  what every seed does.
- `executor_local_move_noreplace_test.go:59` `requireTrashHoldsOnce` requires the whole trash to hold one
  entry, so the crash scenario, whose trash also holds `f.txt` and `e.txt`, needed its own
  `assertTrashHoldsReplacedOnce`. Fix: one helper with a reason and path filter, used by both.
