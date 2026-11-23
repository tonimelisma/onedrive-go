# W43 definition of done and readiness (written before code edits; base 51e554fa)

## Readiness (12 items)
1. PASS Task intent: every e2e read of a rewritten file waits for its content; no poll predicate can fail the test; a source test holds the second rule.
2. PASS Non-goals: product code, devtool buckets, shortcut waits, new live tests, the items the next change owns (Redact, IDs in messages, raw output in messages, whole-output Contains, debug log records, socket.io messages).
3. PASS Governing docs: spec/requirements/non-functional.md R-6.10.17; spec/design/system.md Verification Policy (line 524); spec/reference/live-recurring-families.md lines 146-183; spec/requirements/sync.md R-2.8.12.
4. PASS Authority owner: the e2e suite owns its waits and predicates; the CLI owns the missing-item sentence (internal/cli/error_presentation.go); OneDrive owns createdDateTime.
5. PASS Code scope: e2e/ test files and the three spec files above; no internal/, cmd/ or pkg/ file.
6. PASS Regression plan: planted-source test for the predicate scan, real-tree predicate scan, debounce source test, missing-sentence tests, created_at decode test; each seen failing first.
7. PASS Verification plan: build, vet (plain and tagged), gofumpt, untagged `go test ./e2e/`, the devtool convention test, lint under the lock; live runs are the owner's (W43-live-names.txt).
8. PASS Data safety: test code only; no live run here; cleanup paths unchanged except the missing-item match, which is stricter.
9. PASS Secrets: no token, .env, .testdata or account name read or written.
10. PASS Live/destructive: none run here; the owner runs the listed tests on the test account.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION Question: does the daemon upload a file written during a timed pause with no reload after the pause ends? Current assumption: yes, through watchPauseExpiryWake (internal/multisync/watch_wake.go:39-101, R-2.8.12). Validation: the owner's live run of TestE2E_SyncWatch_TimedPauseExpiry. Owner if wrong: internal/multisync/watch_wake.go.

## getRemoteFile callers
- cli_commands_e2e_test.go:515 copy.txt: new file an exited `cp` made (cp waits for its monitor, internal/cli/cp.go:176): stays.
- edge_cases_e2e_test.go:156 churn.txt: new remote file an exited one-shot sync made: stays.
- sync_demoted_full_e2e_test.go:82 conflict.txt: existed ("original" uploaded), then put-overwritten: moves to the content wait.
- sync_full_test.go:636 local-deferred.txt: made by the exited upload pass and never rewritten; the read checks that the download-only pass did not rewrite it: stays.
- sync_full_test.go:873 fragile.txt: existed, deleted remotely, recreated by the converge passes: moves (the name wait at 872 goes with it).
- sync_full_test.go:915 collision.txt: new file an exited put made (895); the upload-only pass must leave it: stays.
- sync_full_test.go:1144 converge-create.txt: new file an exited put made (1120): stays.
- sync_watch_e2e_test.go:121, 272, 384: see the daemon-exit reads below.

## Asserting predicates in e2e files the base commit did not touch
- Literals passed straight to a poll primitive (suitePollPrimitives, testify Eventually/Never): only sync_safety_full_e2e_test.go:115 (assert.Eventually); its literal calls remoteFileNames (:116), which calls require.NoError (:56). No literal calls an assertion directly.
- Literals passed to suite functions that reach a primitive: sync_edge_cases_full_test.go:97 (requireSyncEventuallyConverges) calls require.Truef at :106; sync_edge_cases_full_test.go:257 calls runCLIWithConfig at :267, which calls require.NoErrorf.
- Every other untouched file: none.

## Candidate rule (part 4) and what it finds today
Rule: a function literal with a result, or a suite function passed by name that returns one, given to a poll primitive or to a suite function that reaches one, calls no testify assertion on `t` (first argument `t`) and no `t.Fatal`/`t.Fatalf`/`t.Error`/`t.Errorf`, directly or through suite functions. Run over the tree as it is (scratch probe, removed):
- sync_edge_cases_full_test.go:106 require.Truef inside the nested-deletion setup predicate: fault.
- sync_edge_cases_full_test.go:267 runCLIWithConfig inside the transfer-workers predicate: fault (a lagging listing fails the test instead of being waited through).
- sync_safety_full_e2e_test.go:116 remoteFileNames inside assert.Eventually: fault (asserts on testify's goroutine, and a failed ls reads as "no files").
- No false finding. The brief's narrower form (primitives only, direct calls only) finds nothing at all; the two widenings each catch a fault above. `assert.ObjectsAreEqual(want, tree)` (sync_hardening_full_e2e_test.go:195, 350) takes no `t` and is not counted. No declared-safe entry is needed.

## Reads after the daemon exited (sync_watch_e2e_test.go)
- :121 BasicRoundTrip: waitForRemoteFileContent at :104, same opsCfgPath, path and watchContent: repeat, dropped.
- :272 PauseResume: waitForRemoteFileContent at :265, same path and pausedContent: repeat, dropped.
- :384 ControlSocketReload: waitForRemoteFileContent at :353, same path and beforeReloadContent: repeat, dropped.
The three getRemoteFileForDrive exceptions go; the list keeps the four shortcut entries.

## The CLI's sentence for a missing item
internal/cli/error_presentation.go:211 `<path> was not found on OneDrive.` plus :185 ` Run 'onedrive-go ls <parent>' to see what is there.` when the path has a parent; printed as `Error: ...` (:302) by internal/cli/root.go:799; under `--debug` (the suite adds it, cli_runner_test.go:44) a `Details: ...` line follows (:304).
- `rm -r <path>` (e2e_test.go:233, sync_e2e_test.go:896): rm.go:65 (resolving), :168/:174 (deleting) -> `Error: /<folder> was not found on OneDrive. Run 'onedrive-go ls /' to see what is there.`
- `stat <path>` (sync_watch_full_test.go:581): stat.go:55 -> `Error: /<folder>/paused-file.txt was not found on OneDrive. Run 'onedrive-go ls /<folder>' to see what is there.`
- `ls <folder>` (deleteDisappearanceReady; every waitForRemoteDeleteDisappearance caller passes ls): ls.go:49 -> same form. A mount-root segment miss wraps graph.ErrNotFound too (internal/driveops/session.go:852).

## created_at
stat --json `created_at` = formatAPITime(item.CreatedAt) (internal/cli/stat.go:97) = top-level driveItem `createdDateTime` (internal/graph/items.go:64, :240). The client sends only fileSystemInfo.lastModifiedDateTime (internal/graph/upload.go:36-38, upload_session.go:37); the create body holds name, file facet and conflict behaviour (item_create.go:60-64); no product file sends a creation time. So it is OneDrive's own time of the create request: part 6 is written. An empty or invalid value falls back to the CLI's now (items.go:237-240), which can only read as late.

## Pure predicate tests in the tagged remote_read_helpers_test.go
TestClassifyFixtureSeedVisibilityAttempt, TestRemoteStatSizeReady, TestDeleteDisappearanceReady.
