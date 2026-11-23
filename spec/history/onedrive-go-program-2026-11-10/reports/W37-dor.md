# Definition of Ready: a chaos run is replayable from its seed

Worktree `~/src/onedrive-go-wt/chaos-replay-from-seed`, branch
`test/chaos-replay-from-seed`, base `3e5c1bd4` (main).

1. PASS **Task intent**: repo-maintenance outcome: a chaos model run is a
   function of its seed and of what the client did, never of scheduling; a
   test runs one seed twice in one process and compares the two fault logs
   per key; a failing chaos run prints one line that reruns it.
2. PASS **Non-goals**: no product file changes; not the planner, not the
   dependency-cycle report of a chaos seed, no new chaos faults, no change to
   what the model edits or checks.
3. PASS **Governing docs**: `spec/design/system.md` Verification Policy (the
   chaos model bullet, lines 335-354); repository `CLAUDE.md` lines 55-279;
   `SUBAGENT-RULES.md`; requirement lines R-2.3.1 (`spec/requirements/sync.md`)
   and R-6.8.9 (`spec/requirements/non-functional.md`), which the chaos test
   cites.
4. PASS **Authority owner**: test code only. The chaos layer
   (`chaosGraph`, `internal/sync/chaos_remote_test.go`) owns every draw, its
   storm flag and the new fault log; the model run
   (`internal/sync/engine_model_based_test.go`) owns the passes that set the
   storm and the rerun line it logs. No product truth, mutation or output.
5. PASS **Code scope**: `internal/sync/chaos_remote_test.go`,
   `internal/sync/chaos_remote_session_test.go`,
   `internal/sync/scenario_remote_session_test.go` (callers of the layer),
   `internal/sync/engine_model_based_test.go`, one new test file for the
   replay and rerun-line tests if the chaos file grows too long, and
   `spec/design/system.md`. Not touched: every product `.go` file.
6. PASS **Regression plan**: first a new test that runs one chaos seed twice
   and compares the fault logs per key, in order within the key; run for
   seeds 1 and 2 (`-count=20`), plain, `-race`, `GOMAXPROCS=1` and `2`, with
   the result tabled in the report before any change to how draws are made.
   Then a test that parses the rerun line and checks the test name exists and
   the seed and batch count are the run's. The race seed's fault assertion
   stays, resting on the replay property.
7. PASS **Verification plan**: the step-one runs again on the final tree;
   `go build ./...`, `go vet ./...`, the tagged vet of e2e, graph and devtool;
   `go test -count=1 ./internal/sync/...` behind the host lock; one `-race`
   run of every chaos test and the added tests behind the lock;
   `TestFindTestConventionViolations_RealRepoIsClean`; the package's
   `ListsEvery`/`Table`/`Governs`/`Exhaustive`/`Registered` tests; lint of
   `./internal/sync/...` last, behind the lock. No `devtool verify`.
8. PASS **Data safety**: test code over temp directories and the in-memory
   fake OneDrive; no local or remote user data, no state DB outside test temp
   directories.
9. PASS **Secret and credential safety**: no token files, `.env`, CI secrets
   or account names involved; nothing is written that names one.
10. PASS **Live provider and destructive operations**: none; no e2e, no live
    account, no history rewrite, no dependency upgrade.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does one seed already fix a chaos run? Current assumption:
      unknown; the per-call streams already key on operation, key and the
      call's count on that key. Validation step: the step-one runs. Owner
      affected if wrong: `chaosGraph.call` and its draws.
    - Question: which requirement does the replay test prove? Current
      assumption: the ones the chaos test it replays cites (R-2.3.1,
      R-6.8.9), as `TestModel_SameSeedReplaysIdentically` cites R-2.3.1; no
      requirement names test replay. Validation step: the reviewer's gate.
      Owner affected if wrong: the `Validates:` lines of the new tests.
    - Question: what form carries the batch count in the rerun line? Current
      assumption: the chaos test takes it only through the race detector
      (`modelRaceBatches` under `-race`, `modelBatches` otherwise), so the
      line carries `-race` exactly when the run had it. Validation step: the
      parse test maps the flag back to the run's count. Owner affected if
      wrong: the rerun-line builder.
