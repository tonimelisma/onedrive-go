# A5 Definition of Ready: e2e waits check content, not names

Branch `test/e2e-content-waits`, worktree `~/src/onedrive-go-wt/e2e-content-waits`, base `main` at `90c60a8f`.

1. PASS **Task intent**: repo-maintenance outcome. Every live e2e wait on a file a running `sync --watch`
   daemon uploads, after which the test stops the daemon, edits or deletes the file, or changes the remote
   copy, checks the file's size or content instead of its name, so no test can stop the daemon between the
   create of the empty item and its content (the test-bug half of LI-20261110-01).
2. PASS **Non-goals**: no product code; the 62 SAFE name-only waits (all after a command that has exited)
   stay as they are; timeouts unchanged; local-side waits unchanged (analysis 01d B3: none racy); no new live
   test; the optional strengthening of SAFE sites (01b, 01d A2) is left for a later increment.
3. PASS **Governing docs**: `CLAUDE.md` 55-236 (E2E and test style); `spec/design/system.md` Verification
   Policy (the e2e harness bullets); `spec/reference/live-incidents.md` LI-20261110-01;
   `spec/reference/live-recurring-families.md`; `spec/design/drive-transfers.md` "New files and their
   placeholder"; `spec/design/graph-client.md` Transfers (`item_create.go`, the two-request create);
   analysis `01b` Item 2 and `01d` lines 1-52 and 124-209, each racy site re-read in the current code.
4. PASS **Authority owner**: the e2e harness owns the waits: `e2e/remote_read_helpers_test.go`
   (`pollRemoteEventually` and the wait helpers). The fact they rely on, that a new file is created empty
   and filled by a second request, is owned by `internal/graph/item_create.go` and is not changed. No
   durable state, runtime lifecycle or user-facing output is touched.
5. PASS **Code scope**: `e2e/remote_read_helpers_test.go`, `e2e/e2e_test.go` (one timeout constant moved
   there after lint; refreshed), `e2e/sync_watch_e2e_test.go`,
   `e2e/sync_watch_full_test.go`, `e2e/orchestrator_e2e_test.go`, `e2e/socketio_e2e_test.go`,
   `e2e/sync_shortcut_full_e2e_test.go`; docs `spec/design/system.md`,
   `spec/reference/live-incidents.md`, `spec/reference/live-recurring-families.md`. Not touched:
   `internal/`, `cmd/`, the SAFE waits, `.testdata`, `.env`.
6. PASS **Regression plan**: a live wait has no failing unit test to write first. The new pure predicate
   `remoteStatSizeReady` is covered by `TestRemoteStatSizeReady` (it rejects the 0-byte item a create
   leaves). Each converted site's checked fact is listed in `A5-report.md`; the reviewer runs each touched
   live test once.
7. PASS **Verification plan**: `go build ./...`; `go vet -tags 'e2e e2e_full' ./e2e/...` and
   `go vet -tags e2e ./e2e/...` (compile only); gofumpt and goimports on touched files; `devtool verify lint`
   once before the commit. Reviewer: the pre-pr stage and one live run of each test listed in the report.
   No race or stress need: no product or concurrent code changes.
8. PASS **Data safety**: test-only change. The new waits are reads (`stat --json`, `get` into
   `t.TempDir()`). The change removes a way the suite left empty placeholder files on the test drive. No
   local deletes outside temp dirs, no state DB writes, no overwrites.
9. PASS **Secret and credential safety**: no token file, `.env`, CI secret or account name is read,
   written or logged; `.testdata` and `.env` are neither read nor staged.
10. PASS **Live provider and destructive operations**: this increment runs nothing live. The reviewer's
    live runs create and clean up only the tests' own folders on the test accounts, as today. No history
    rewrite, no release artifacts, no dependency upgrade.
11. PASS — the start stage ran on this branch; the last nightly's one failing test (TestE2E_SyncWatch_PauseResume) is fixed on main by #860 and awaits the next nightly, so this increment claims nothing.
12. ASSUMPTION **Unknowns**:
    - Question: does `stat --json` report the size the same way on a second drive and on a shared-folder
      selector? Current assumption: yes; it is the same CLI command, run through the same
      `pollRemoteEventually` with an explicit drive, as the name waits it replaces. Validation step: the
      reviewer's live runs of `TestE2E_SyncWatch_ControlSocketReload`,
      `TestE2E_Orchestrator_WatchSimultaneous` and `TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget`.
      Owner if wrong: `e2e/remote_read_helpers_test.go` (`waitForRemoteStatSizeForDrive`).
    - Question: does a new small file stay at size 0 until its content commits? Current assumption: yes
      (`graph-client.md` Transfers; analysis 01b Item 2; the content is one request below 4 MiB). Validation
      step: the same live runs; a size wait that passed early would show as an empty file in the
      content-asserting tests. Owner if wrong: the helper's doc comment and `system.md` policy bullet.
