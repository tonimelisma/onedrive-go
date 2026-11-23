# Sweep of parked items, checked against main 05336a1a (2026-11-11)

Every recommendation, unfinished item, out-of-scope defect and undone deviation
in the eighteen `*-report.md` files and `A7-seeds.md`, plus the reviewer's
list, checked in `~/src/onedrive-go` at `05336a1a`. Paths are relative
to that checkout. A6 (PR #877), A7 (branch only) and A9 (PR #878) are not on
main; their items were checked against main as it stands. Where the reviewer
named the same smell as a report, the line names both sources. The sweep PRs
are in `SWEEP-plan.md`.

| Source | Status | Where (today) | What is wrong |
|---|---|---|---|
| A1 rec 1 | DONE (#867) | `e2e/sync_watch_e2e_test.go:104`, `:353`, `:377` | BasicRoundTrip and ControlSocketReload stopped the daemon after a name-only wait; #867 made both wait for content or size. |
| A1 rec 2, deviation 10 | TODO -> W4 | `internal/graph/item_create.go:246-248`, `:294-297` | Never probed live whether an orphan upload session keeps moving the eTag; three immediate delete attempts could then all lose. |
| A1 rec 3 | DONE (#864) | `internal/driveops/transfer_session.go:211`, `:242-256` | A failed large-file session request left its created item unrecorded; `recordItemOfFailedSession` now runs the create hook. |
| A1 rec 4 | TODO -> W4 | `internal/graph/upload_transfer.go:330`; `internal/graph/auth_browser.go:105` | Session cancel and the browser callback's header timeout borrow the sign-in server's `shutdownTimeout` instead of their own constants. |
| A1 rec 5 | DONE (#864) | `internal/driveops/transfer_session.go:74-77` | `cancelSessionTimeout`'s comment named only the session cancel; it now names the placeholder removal it also bounds. |
| A1 rec 6, deviation 15 | DONE (#867) | `spec/reference/live-recurring-families.md:137-165` | No family entry covered the empty item a stopped create leaves; #867 added one linked to LI-20261110-01. |
| A1 rec 7 | DONE (#864) | `spec/design/sync-engine.md:568-570` | Three "drain" wordings promised that shutdown waits for in-flight work; #864 rewrote them as cancel, not drain. |
| A1 unfinished | DONE (#860) | PR #860, merged and installed | Lint, push, PR, the live PauseResume proof, the install and the nightly claim were left to the launcher; all done. |
| A2 rec 1 | TODO -> W2 | `internal/devtool/bench.go:621-624` | The benchmark builds its subject with plain `os.Environ()`, so with the host Go, not the pinned toolchain. |
| A2 rec 2 | TODO -> W1 | `internal/devtool/nightly.go:85-89`; `internal/devtool/verify_dod_ci_wait.go:163` | Two private types decode the same GitHub job JSON; one shared type would serve the nightly check and the CI wait. |
| A2 rec 3 | TODO -> W1 | `.github/workflows/ci.yml:88`, `:131` (no `nproc` or core count anywhere) | The 3-core and 4-core runner assumptions behind the unit-test deadline are never logged, so never confirmed. |
| A2 rec 4 | COVERED BY A10 ("leases renew in the pre-pr and pre-merge stages") | `internal/devtool/verify_dod.go:194-203` | Nightly claims are taken and renewed only in the start stage, so a long increment can lose its lease. |
| A2 rec 5 | TODO -> W1 | `internal/devtool/worktree.go:90`; `internal/devtool/cleanup_audit.go:33` | "origin/main" is spelled as a literal and as a second constant beside `originMainRef` (`verify_dod.go:24`). |
| A3 rec 1 | TODO -> W5 | `internal/sync/store_pending_creates.go:138-141` | `SyncStore.ClearPendingCreate` has no caller anywhere; it should be deleted or used after a committed create. |
| A3 rec 2 | TODO -> W5 | `internal/driveops/transfer_session.go:471-483` | When Graph's cleanup did remove the empty item, the `removed` answer is only logged and the pending record lingers. |
| A3 rec 3 | TODO -> W5 | `internal/sync/store_pending_creates.go:26-36` | Item-naming rows drop only when the item gains content or vanishes, so a zero-byte file's row stays forever. |
| A3 rec 4 | TODO -> W4 | `spec/reference/graph-api-quirks.md:1146` | Unprobed on Business: does a canceled session move an empty item's mtime and hash? Doubt: needs a Business drive. |
| A3 rec 5 | TODO -> W8 | `internal/sync/doc.go:24` | The package doc still says the watch loop owns "graceful drain", the wording #864 retired everywhere else. |
| A4 rec 1 | TODO -> W8 | `internal/sync/engine_run_once.go:31`; `internal/sync/engine_watch.go:58` | `RunOnce` and `RunWatch` never put the engine's own perf collector on their context; an owner that forgets loses every count. |
| A4 rec 2 | TODO -> W8 | `internal/sync/engine.go:429` | `Engine.Close` runs under the orchestrator's context, so anything it records misses the mount. Doubt: it records nothing today. |
| A4 rec 3 | TODO -> W3 | `e2e/status_helpers_test.go:74` (parsed, never asserted) | No end-to-end test checks that `status --perf` shows a non-zero request count for a syncing mount. |
| A4 rec 4 | TODO -> W6 | `internal/cli/status_perf.go:136-160` | `status --perf` shows per-mount snapshots only; the owner aggregate `GET /v1/perf` returns is never shown. Needs a product decision. |
| A5 rec 1 | TODO -> W3 | `e2e/sync_watch_full_test.go:465`, `:528` | Two name-only waits in RapidChurn and GracefulShutdown now only duplicate the content and size waits beside them. |
| A5 rec 2 | TODO -> W3 | `e2e/sync_full_test.go:593`, `:647`; `e2e/cli_commands_e2e_test.go:654` | Tests wait on a name for modified files, and read content right after `--force` without waiting for it. |
| A5 rec 3 | TODO -> W3 | `e2e/remote_read_helpers_test.go:176-178` | A timed-out poll prints `get`'s stdout, which is not the file's bytes, so the last content seen is unknown. |
| A5 rec 4 | TODO -> W3 | `e2e/` and `internal/devtool/` (no such check exists) | Nothing stops a new name-only wait in a test that starts a daemon; the rule exists only as prose. |
| A5 rec 5 | TODO -> W3 | full-tier run of the 16 nightly-only tests #867 touched | Sixteen of the seventeen tests #867 changed run only in the full tier; nobody has read their result yet. |
| A6 rec 1 | TODO -> W3 | `e2e/sync_hardening_full_e2e_test.go:235` | NamingCorpus checks content unpolled after a best-effort `--download-only` pass that may not have converged. |
| A6 rec 2 | TODO -> W3 | `e2e/shared_items_helpers_test.go:541-542` | `findActiveShortcutChild` asserts inside a poll predicate, so one transient non-active state fails the whole test. |
| A6 rec 3 | TODO -> W3 | `e2e/sync_full_test.go:819`, `:937` | The create-create conflict and dry-run tests run their single pass right after a remote put, before delta shows it. |
| A6 rec 4 | COVERED BY B2 ("`runSyncThroughTransientFailures` runs through the allow-error runner") | `e2e/e2e_test.go` (A6's helper, PR #877) | Once a pass that left work exits 1, the converge helper must accept that exit code instead of failing. |
| A6 checks | COVERED BY A6 ("the rest through the nightly") | the `ForDrive` and `AllDrives` sites in PR #877 | The per-drive and all-drives converge sites cannot run on this one-drive host; only CI proves them. |
| A7 defect | COVERED BY A15 ("the stale-precondition check (`errActionPreconditionChanged`) runs before the HTTP branch") | `internal/sync/engine_result_classify.go:67`, `:134` | A create 409 already judged stale waits on the durable retry curve instead of being replanned in the same run. |
| A7 rec 2, reviewer | TODO -> W4 | `spec/reference/graph-api-quirks.md`, `spec/reference/live-incidents.md` (no entry) | No quirk or incident entry records the HTTP 400 Graph gave a folder create in the 2026-11-11 nightly. |
| A7 rec 3, reviewer | TODO -> W12 | `internal/sync/scenario_remote_session_test.go` (on A7's branch, not on main) | The fake's upload-session client bypasses the chaos layer, so the storm never covers the large-file route. |
| A7 rec 4, reviewer | TODO -> W12 | `internal/sync/chaos_remote_test.go:551-553` | Under `-race` the chaos model runs seed 1 only, and that seed draws no create or session fault. |
| A7 unfinished | COVERED BY A7 (status row: "lint + full test + gate next") | `internal/sync` (about 35 files share the fake) | A7 changed the shared scenario fake, so the whole sync package must run before it merges. |
| A7-seeds | COVERED BY A16 ("an intent-only row is dropped when the plan is built from a complete local observation") | `internal/sync/store_pending_creates.go:58-64` | Chaos seeds 9 and 11 leave an intent-only pending row after the user renames the local file away. |
| A7-seeds, reviewer | TODO -> W12 | `internal/sync/chaos_remote_test.go:535-537` | A failing chaos seed never prints its fault-kind summary, because the log line runs only after the run returns. |
| A8 rec 1 | TODO -> W1 | `internal/devtool/verify_failed.go:56` | `verify failed-test` reruns through plain `runner.Run`, so a rerun whose pattern matches nothing passes. |
| A8 rec 2 | TODO -> W2 | `internal/devtool/verify.go:577-599` | The report-only macOS leg still builds a full coverage profile; skip it if the leg nears its deadline. Measure first. |
| A8 rec 3 | TODO -> W2 | `internal/devtool/verify_test_match.go:102-105` | A package tree with no test files at all prints no marker and passes both no-tests-matched guards. |
| A8 rec 4 | TODO -> W2 | repository `CLAUDE.md`, verification tiers (no number recorded) | Nobody measured the local two-core `verify default` time after `-coverpkg=./...`. Doubt: a measurement, not code. |
| A8 unfinished | DONE (#872) | `spec/requirements/non-functional.md:186` | The coverage number 83.5 was provisional; it was set to 81.7 from the full-tier dispatch run before merge. |
| A9 rec 1, reviewer | TODO -> W1 | `.github/workflows/ci.yml:262-263`, `:369-371` | Job-level `env` puts the `ONEDRIVE_TEST_DRIVE` values into every step's printed environment header in public logs. |
| A9 rec 2 | TODO -> W2 | `internal/devtool/verify_stress.go:195` | The stress x3 step still buffers with `CombinedOutput`, so a hang shows nothing until the step ends. |
| A9 rec 3 | TODO -> W1 | `internal/devtool/nightly.go:668-690` | A timeout panic's "running tests:" block is never parsed, so a hung test is reported without its name. |
| A9 rec 4, reviewer | TODO -> W1 | `internal/devtool/verify_failed.go:75-81`, `:106-108`; `internal/devtool/nightly.go:679` | Two hand-rolled go test log parsers; failed-test's raw form breaks on CI timestamps and guesses JSON by the first brace. |
| A9 rec 5, deviation, reviewer | TODO -> W1 | `internal/devtool/verify_dod_ci_wait.go:99-121` | The CI stall window is the longest job in `ci.yml` (now 190 minutes), not the longest job a PR runs. |
| A9 deviation | COVERED BY A11 ("a replay line printed on failure") | `internal/devtool/verify_failed.go:151-170` | `failed-test` keeps go test's shuffle seed, but no model-seed replay line exists to keep. |
| A14 rec 1 | TODO -> W5 | `internal/driveops/transfer_verify.go:186-198` | A failed lookup wraps only the lookup error and drops the upload's own. Doubt: no create mark reaches it today. |
| A14 rec 2 | TODO -> W5 | `internal/sync/store_pending_creates.go:26-36` | A claimed zero-byte file keeps its item row, so a later mtime-only remote change is absorbed into the baseline. |
| A14 rec 3 | TODO -> W4 | `internal/graph/item_create.go:118-122` | Every non-Graph error, including token and request-building failures that never left, is marked "create outcome unknown". |
| A14 rec 4 | TODO -> W15 | `e2e/` (no test injects a fault on the wire) | No live test loses a create's answer on the wire; only fakes cover that half of R-5.6.15. Doubt: harness cost. |
| B3 rec 1, reviewer | TODO -> W6 | `internal/cli/sync_flow.go:96-99` | The one-shot early refusal ignores an unresponsive owner, so a busy watch gets the banner, then a lock failure. |
| B3 rec 2, reviewer | TODO -> W6 | `internal/cli/status_runtime.go:27`; `internal/cli/status_perf.go:34` | `status --perf` probes the control socket twice, so a busy watch costs two two-second probe timeouts. |
| B3 rec 3, reviewer | TODO -> W6 | `internal/cli/control_client.go:316-326` | `decodeControlJSONResponse` turns every decode failure into `internal_error`, so a body-read timeout never reads as "did not answer". |
| B3 rec 4 | TODO -> W7 | `internal/multisync/control_socket.go:178-190` | The goroutine that closes the control server when the context ends has no owner that waits for it. |
| B3 deviation | COVERED BY B9 ("the control server logs handler panics through slog") | `internal/multisync/control_socket.go:201-209` | `http.Server.ErrorLog` is unset, so the control server's own errors and panics bypass slog. |
| B4 rec 1 | TODO -> W13 | `internal/config/resolver.go:178-179` | An expired pause is cleared by two separately locked deletes, so a stuck holder stalls a reload twice per drive. |
| B4 rec 2 | TODO -> W8 | `internal/sync/engine.go:530-532` | A catalog locked by another process fails the mount at start-up, where the fatal-auth path only warns. |
| B4 rec 3 | TODO -> W13 | `internal/config/mutation_lock.go:61-71` | Config writers take no context, so Ctrl-C cannot end the ten-second lock wait. |
| B4 rec 4 | TODO -> W13 | `internal/config/mutation_lock.go:82-100` | flock on a directory descriptor can fail on NFS or FUSE, failing every config change. Doubt: unconfirmed. |
| B5 rec 1, deviation | TODO -> W6 | `internal/cli/sync_flow.go:186`; `internal/cli/sync_runtime.go:187` | `sync` builds a second session runtime for its engine, so one process keeps two token-source caches. |
| B5 rec 2 | TODO -> W6 | `internal/cli/shared_target.go:146`, `:175` (compare `internal/cli/root.go:366-371`) | The shared-target clients do not flush the token cache after a probe renames the account, unlike `cc.Session`. |
| B5 rec 3 | TODO -> W4 | `spec/design/graph-client.md:22-24` | The profile list omits the `notices` parameter every builder takes (`internal/graphtransport/profiles.go:47-59`). |
| C1 rec 1 | TODO -> W9 | `internal/sync/sqlite_compare.go:606`; `internal/sync/sqlite_compare_test.go:692` | A local edit equal to the unchanged remote uploads bytes OneDrive already holds. Needs a product decision. |
| C1 rec 2 | TODO -> W9 | `internal/sync/enums.go:37`, `:221` | `actionType.Direction()` and the `direction` type have no production caller, only one test. |
| C1 rec 3 | TODO -> W8 | `internal/sync/engine_current_plan.go:76`, `:95`, `:589`; `internal/sync/sqlite_compare.go:798` | Planning-state load errors are wrapped three ways by entry point, and a comparison query error is wrapped twice. |
| C1 rec 4 | TODO -> W9 | `internal/sync/planner_actions.go:215`; `internal/sync/planner_dependencies.go:287`, `:519`; `internal/sync/planner_move_composition.go:1492` | Unreachable `default:` branches return a quiet value instead of refusing an action type nobody listed. |
| C2 rec 1, reviewer | TODO -> W10 | `internal/sync/truth_status.go:172-174` | `observationIssueBlocksRemoteTruth` is two equality tests, so an unknown issue type fails open. |
| C2 rec 2 | COVERED BY C4 ("add the type to the managed list") | `internal/sync/observation_findings.go:10`; `internal/sync/scanner.go:498` | The interrupted-removal condition never clears after the user removes the leftover file. |
| C2 rec 3, reviewer | TODO -> W10 | `internal/sync/condition_keys.go:153-178` | `conditionKeyForIssueType` is a lookup table, so a new issue type silently maps to "unexpected condition". |
| C2 rec 4, reviewer | TODO -> W10 | `internal/sync/issue_types.go:30`; `internal/sync/scanner.go:289`, `:324` | `hash_panic` also names ordinary read errors and its condition key says hash error; renaming it needs a store generation. |
| C2 rec 5, reviewer | TODO -> W10 | `internal/sync/engine_results.go:15`, `:101`, `:135` | `fallbackFailureSummaryIssueType` is a summary and log key, not an issue type, yet is named as one. |
| H1 rec 1 | TODO -> W14 | `internal/fsroot/fsroot.go:598-604` | Any failure on an absent name becomes `os.ErrNotExist`, so a permission or disk-full error reads as "missing". |
| H1 rec 2, deviation | TODO -> W14 | `internal/fsroot/fsroot.go:544-549` | A directory fsync that FUSE or SMB refuses with EINVAL or ENOTSUP fails every config and token write. |
| H1 rec 3 | TODO -> W14 | `internal/fsroot/fsroot.go:391` | `Root.CreateTemp` is exported, but nothing outside the package calls it. |
| H1 rec 4 | TODO -> W14 | `spec/design/threat-model.md:81` | The rooted-boundary evidence row cites none of the containment tests #874 added. |
| H1 rec 5 | TODO -> W14 | `spec/requirements/non-functional.md:58` | The state database opens by path outside the rooted guarantee; R-6.4.10 leaves it out without saying so. |
| H2 rec 1 | TODO -> W11 | `internal/sync/store_write_block_scopes.go:56` | A trial time past 2262 is stored as a wrapped `UnixNano`, so after a restart the trial is due at once. |
| H2 rec 2 | TODO -> W11 | `internal/sync/store_retry_work.go:202`; `internal/sync/engine_runtime_lifecycle.go:279` | Other stored times share the conversion; checked today, only the block-scope trial takes a server-given wait. |
| H2 rec 3 | DONE (before #866) | `internal/retry/transport.go:303`, `:366-376` | A saturated wait was thought silent; `announceLongWait` already warns once per episode with the wait's length. |
| H3 rec 1 | TODO -> W4 | `internal/graph/client_auth.go:18`, `:313-323` | `do` takes an `io.Reader`, so a body that cannot be rewound can still be passed; the retry should use GetBody. |
| H3 rec 2 | TODO -> W4 | `internal/graph/client_auth.go:323` | `Seek(0, SeekStart)` rewinds to the slice's start, not to where the reader stood when the call began. |
| Reviewer | TODO -> W1 | `internal/devtool/nightly.go:590-601`, `:620-624`, `:644-650` | One transient GitHub 5xx on the job-log fetch kills the DoD start stage; nothing retries or names GitHub as the cause. |
| Reviewer | COVERED BY A10 ("Post-merge reads push runs only") | `internal/devtool/verify_dod_ci_wait.go:592-617`, `:684-700` | Post-merge reads every run on the merge commit whatever its event, so an unrelated failed run fails it (mechanism inferred). |
| Reviewer | TODO -> W12 | `internal/sync/engine_test_helpers_test.go:586`; `internal/sync/engine_watch_test.go:83-593` | Watch tests wait ten seconds for an event and fail under host load alone. |

## Count by status

- DONE: 8 (#860 once, #864 three times, #867 twice, #872 once, and one that was already true before #866).
- COVERED BY a planned PR: 10 (A10 twice; A6, A7, A11, A15, A16, B2, B9 and C4 once each).
- TODO: 70, in fifteen sweep PRs W1 to W15 (`SWEEP-plan.md`).
- Total: 88 lines. Every reviewer item is on a line of its own or merged into the report line that names the same smell.
