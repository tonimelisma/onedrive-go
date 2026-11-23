# A7: chaos seeds 9 and 11 fail without the churn variant

Test: `TestModel_ChaosGraphConverges` (internal/sync, exhaustive tier; nightly runs seeds 1-2 only).

Reproduce seed 9 from the repository root, no `-race`, not `-short` (215 s here):

    ONEDRIVE_GO_MODEL_SEED=9 go test -count=1 -timeout 20m -run '^TestModel_ChaosGraphConverges$' -v ./internal/sync/

Seed 11 is the same with `ONEDRIVE_GO_MODEL_SEED=11` (326 s). Tree it ran on: base 3e4c21a7 plus A7, with the `churnPlaceholder` line in engine_model_based_test.go commented out. With that hook nil, the chaos fault streams are the base's, because A7's only extra draw sits behind it. Not yet rerun on 05336a1a.

First 15 lines of the failure (seed 9):

    chaos_remote_test.go:558:
      Error Trace: internal/sync/sync_invariants_test.go:156
                   internal/sync/sync_invariants_test.go:89
                   internal/sync/engine_model_based_test.go:339
                   internal/sync/chaos_remote_test.go:558
      Error:       Should be zero, but was 1
      Test:        TestModel_ChaosGraphConverges/seed=9
      Messages:    invariant: no pending creates after convergence
    chaos_remote_test.go:558:
      Error Trace: internal/sync/engine_model_based_test.go:340
                   internal/sync/chaos_remote_test.go:558
      Error:       sync invariants violated
      Test:        TestModel_ChaosGraphConverges/seed=9
      Messages:    batch 4: seed 9 (rerun with ONEDRIVE_GO_MODEL_SEED=9); operations:
                     setup remote mkdir x

Fault kinds: a failing seed does not print its summary (the t.Logf at chaos_remote_test.go:560 runs only after the run returns). The fault that left the row, from the run log: `upload-session:reset` before the create reached OneDrive. The create of `docs/50%.txt` failed as "create outcome unknown: chaos upload-session: read tcp: connection reset", and the intent was kept (executor_transfer.go:515). The model then did "case variants: local create docs/50%.txt, remote create docs/50%.Txt" and the user renamed the local file to `docs/resolved-1-50%.txt`. Leftover row: `pending_creates: docs/50%.txt` (no item ID). Storms in the operations list: delta 408, incomplete change lists, concurrent remote writes.
Seed 11 shows the same shape: an intent-only row at `b.txt`, after "case variants: local create b.txt, remote create B.txt" and a rename to `resolved-1-b.txt`.
