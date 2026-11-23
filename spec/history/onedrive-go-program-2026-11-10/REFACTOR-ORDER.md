# Refactor order (2026-11-18, main `5122e6a4`)

Owner's decision 2026-11-18 21:10: finish the refactor first, polish later in one batch. Everything not listed here is in `PARKED.md`. W101 is being fixed now as its own PR, ahead of the refactor.

## Working rules
- No new plan rows during the refactor. A finding in code a PR touches is fixed in that PR; anything else is one line appended to PARKED.md.
- One review per PR; a second, delta-only review only if the first has blocking findings. Whole-commit re-reviews only for PRs that move users' files.
- `-race` on the touched packages only, once, at ship time.
- After the refactor: one or a few misc PRs clear PARKED.md in bulk.

## Still needed? (checked by grep on main)
All 45 core rows are still open on main; none is done or moot. Partial progress the briefs must start from:
- E2: `startObservers` (`engine_watch.go`) starts the remote observer before the fallible hash-cache and mount loads.
- E3: `timerMu` (`engine_watch_state.go`), `depGraph.mu` (`dep_graph.go`) and the `Engine` atomics are all still there. The worktree `engine-vestigial-locks` has no commits and sits on #898's base.
- E4, C3: two `discardScope*` functions; generation is still 28; `hash_panic` is still stored (`issue_types.go`).
- E13: `newExecutorConfig`/`newWorkerPool` exist, but nine `Set*` setters remain on `executorConfig`.
- F5: `syncClock` exists (`domain_clock.go`), but about 9 direct `time.Now()` calls remain in product code.
- G2/G8: `namekey` has only `Key`/`Equal`/`EqualPath`; `nfcNormalize` (`observer_remote.go`) and `storedPathOf` remain.
- G6: L1/L2 are fixed on main (R-2.8.22, see the row's rider); L3/L7 remain.
- G7: `changeSource` is gone; `changeEvent` is still named in 18 product files.
- G12: the probe is done; `maxComponentLength = 255` still feeds `validateOneDriveName` (`scanner.go`).
- S1-S6: `end-state.md` is pinned to `70301c0e`, so S1 re-runs the model first. `verify_layering.go` is still in devtool.
- B6/B7: the `replace golang.org/x/oauth2` fork and `OnTokenChange` (`graph/auth_token.go`) remain; `tokenfile` has no lock.
- B10: no heartbeat in the tree. B11 is gated on D14's soak.
- F1-F4, G9-G11, I1-I3, E5-E12: none of their types exist yet (`baselineView`, `decide`, `planIdentityKeyed`, `engineHooks`, `syncsched`, ...). `GetCaseVariants` and `RecentLocalDeletes` remain.

W row that is a real prerequisite: **W8**. Its plan entry says it goes before E2 (`RunWatch`) and E7 (`engine_current_plan.go`), in the same files; its tests are absent on main. Two W rows overlap but are not prerequisites, so they stay parked: W46 (F5's seeded jitter is W46's product half; what is left after F5 is the replay assertion, after W45) and W97 (finishes G7's `changeEvent` deletion on the local side).

## PRs in order
Grouping rule (owner, 2026-11-18 21:20): about 5 rows per PR, fewer only where a dependency or size forces it (said in the row). "Files" = whether the PR changes which file operations a sync performs (if yes, a whole-commit re-review). Each row is one commit, in the order listed.

| # | Rows | Goal | Depends on | Briefs | Files |
|---|------|------|-----------|--------|-------|
| R1 | W8, E2, E3, E4, C3 | Engine before the split: entry points carry their collector; observers start after set-up and are waited for; one goroutine owns state; `retry_work` has one write path; foreign key and CHECKs (generation 29, `hash_panic` renamed `hash_error`) | - | all five | no; install, then reset the test drive's state |
| R2 | G1, G2, G8, G3, G12 | Names: typed keys, one lookup rule, one stored form; creates never adopt a twin; the 255-byte name check deleted | - | all five | yes (`put` refuses a case-variant overwrite; long names sync) |
| R3 | G4, G5, G6, G7 | Observation: a twin of a synced file is a collision; dead local event channel deleted; one local sum type committed in one transaction; remote side without `changeEvent` | R2 | all four | yes (G4). 4 rows: the other G rows need F1 or must follow the split |
| R4 | B6, B7 | Token saves are errors; refresh coordinated across processes (B7 may be only a test and a note) | - (other packages; any free slot) | both | no. 2 rows: the only auth rows; B10/B11 need the post-split loop |
| R5 | S1, S2, S3, S4 | Split, prepare in place: re-run the model on current main; rename and file moves by tool; seams, interfaces, errors; tests split per target, `ForTest` check | R1-R3 (D22); open sync branches merged or rebased | none (design `end-state.md`) | no |
| R6 | S5, S6 | The generated move (reviewed as a tool plus a verdict); depguard replaces `verify_layering.go`; un-export | R5 | none | no. 2 rows: the move is thousands of generated lines and must land alone with its cleanup |
| R7 | E6, E11, E13, E7, E8 | Engine construction: hooks in structs (engine and per package); constructors take input structs; `runConfig`/`flowInputs`; `protectedRootSet` and `housekeeper` | R6 | none | no |
| R8 | E5, B10, E9, E10, E12 | Watch and scheduling: phase table; loop heartbeat (observe only); `observation` type; `engineFlow.engine` deleted; `syncsched` package | R7 | B10 only | no |
| R9 | F1, F2, F3, F4, C3 | State and planner (C3 moved here 11-19 from R1/R3b: read-permission scopes write blocked rows without a `block_scopes` row, and its write-order rework spans the same 5 paths R9 rewrites; decide its foreign key form there): read-only baseline view; nothing fallible after commit; `decide` in Go checked against the SQL, then Go decides and the `CASE`s go (reverted if slower than 23 s for 100,000 items) | R6 | none | yes (F4). 4 rows: F5-F7 need R8's scheduler |
| R10 | F5, F6, F7, I1, I2 | Models: one clock, in-memory `synctree`, scheduler seam and watch mode; identity-keyed planner (pure, not wired) and plan diff in the models | R8, R9 | none | no |
| R11 | G9, G10, G11, I3 | Collisions and shadow: one local collision detector; scan reuses committed hashes only if a measurement under `~/tommi` shows a saving; watch-side detection deleted (its commit drops if the agreement test fails); shadow comparison in live watch, then a soak | R9, R10 | none | yes (G9). 4 rows: only B11 is left, and it is gated |
| R12 | B11 | Watchdog ping follows the heartbeat | R8 plus D14's soak | none | no. 1 row: gated on a soak |

Totals: 46 rows (45 core plus W8) in 12 PRs. Briefs for the post-split rows are written at dispatch, after R6, because the split changes every path they would name.

In flight beside this order: W57 (`test/crash-model-replacing-move`, `c6e273a6`, built, unreviewed), plus the W20 and W34 worktrees. Ship or rebase them before R5, since the generated split conflicts with any open sync branch.
