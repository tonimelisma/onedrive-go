# Definition of Ready: a shared folder that is gone says so; a run counts shared folders as shared folders

Worktree `~/src/onedrive-go-wt/shared-folder-gone-says-so`, branch `fix/shared-folder-gone-says-so`, HEAD `a6f6fbf3`. Step 1 found that decision (a) applied; the work stopped, and the decisions of 2026-11-14 (item 12) replaced (a) before any code was edited.

## Step 1 findings (from the code at a6f6fbf3)

### (i) `SelectedCount`

- Definition, `internal/multisync/report.go:91-93`: `return len(s.Results)`, one entry per mount startup result.
- Shortcut children are in it by a deliberate line, `internal/multisync/orchestrator_run_once.go:97-98`:
  `startupResults := append([]MountStartupResult{}, parentStartup.Results...)` then
  `startupResults = append(startupResults, childCoordinator.startupResults()...)`, fed by
  `orchestrator_run_once_children.go:259` `c.appendStartup(childStartup.Results...)`. Each child has its own selection index (`mount_spec.go:318-321`). Watch builds its summary from `decisions.Mounts`, children included (`orchestrator_watch.go:208-218`).
- A side effect beside it: when a drive's shortcuts cannot be set up, `orchestrator_run_once_children.go:239-245` appends a second, fatal result for the parent itself, so that drive is counted twice.
- Callers and what each prints:
  - `internal/cli/sync_render.go:50` (`runOnceLeftWorkError`): `<= 1` gives "sync left N file(s) unsynced; run 'onedrive-go status' for details"; otherwise "%d of %d selected drives left files unsynced (see above)". The first number counts mounts with left work (children included, lines 39-45). Means configured drives.
  - `sync_render.go:96` and `:105` (`runOnceDrivesError`): `<= 1` returns the first error alone (the console left it unprinted); otherwise "%d of %d selected drives failed or were skipped (see above)". The sentence means configured drives; the `<= 1` guard is paired with the console's `multi` below and must keep that one's meaning, or one error prints twice or never.
  - `internal/cli/sync_console.go:216` (`multi := ... > 1`): no count printed; decides the "<name>: " prefix on each summary line and whether per-mount startup and report errors print in the summary. Means "more than one mount reports".
  - `internal/cli/sync_startup_messages.go:103`: `== 0` returns the raw error. No count printed.
  - `internal/multisync/report.go:164` (`WatchStartupError.Error`): `== 0` gives "no drives could start syncing; run 'onedrive-go status' for details"; the number it prints counts failures, not `SelectedCount`.
  - `internal/multisync/watch_reporting_test.go:459`: a test.
- No branch that changes what a run does reads it: every caller only picks a sentence, a prefix, or which error carries the message (the exit-status cause stays reachable in both forms, `sync_render.go:100-106`).
- No `--json` field carries it (`internal/cli/sync_console_json.go` fields at lines 14-35).

### (ii) The issue row of a root read failure

- `isObservationRemoteReadDenied` (`internal/sync/observation_findings.go:119-121`) is `errors.Is(err, graph.ErrForbidden) || errors.Is(err, graph.ErrNotFound)`: 403 and 404 are merged before anything is stored.
- All three root paths (`internal/sync/engine_primary_root.go:135-143`, `196-204`, `226-234`, the last being `executeMountRootObservation`) store the same batch, `rootRemoteReadDeniedObservationFindingsBatch(driveID)`: one `ObservationIssue{Path: "/", DriveID, IssueType: "remote_read_denied", ScopeKey: ScopeKey{Kind: ScopePermRemoteRead, Param: ""}}`.
- What is stored, every field: the child's own store table `observation_issues` has exactly three columns, `path`, `issue_type`, `scope_key` (`internal/sync/schema.go:214-218`); `DriveID` is not a column. For a 403 and a 404: `"/"`, `"remote_read_denied"`, the root remote-read scope key. Identical. No detail field exists on this table, free or otherwise; `block_scopes` (`schema.go:225-230`) holds only trial timing.
- The code says so itself: `sharedFolderUnreadable` (`internal/sync/engine_shortcut_release.go:121-133`) reads the row as "OneDrive answered 'not found' or 'forbidden'".
- The parent store's `shortcut_roots.blocked_detail` (`schema.go:242`) is the binding's lifecycle detail, written by the parent's topology (the shortcut item's own target), never by the child's root observation; using it would change shortcut lifecycle, which is out of scope.
- What the descriptor can reach: `describeStatusCondition(key)` takes only the `ConditionKey` (`internal/cli/status_condition_descriptors.go:44`, `:112`); the group beside it also carries `ConditionType`, `ScopeKey` and `Paths` (lines 42-55), and the caller knows a child (`status_snapshot.go:753` `Kind: statusDriveKindSharedFolder`). So "at a shared folder's root" can be decided from the stored row; 404 against 403 cannot.

### (iii) How `status` heads a shortcut child

- Text (`internal/cli/status_render.go:317-336`, `258-264`, `408-442`): under the parent drive's "Shared folders:" line, indented one level deeper, the heading is the child's display name (`root.DisplayName`), or "Shared folder" when it has none; its detail lines include "Shared folder: Managed through the parent drive" (line 340).
- JSON (`internal/cli/status_snapshot.go:750-769`, fields at 97-120): an entry of the parent drive's `shared_folders` array with `"kind": "shared_folder"` and `"name": <display name>`. Status never calls a child a drive.

## The 12 items

1. PASS **Task intent**: a shared folder whose root answers 404 says it is gone, one whose root answers 403 says it cannot be read, each with its next step; a run's exit sentence counts configured drives and names shared folders as shared folders. Exit rule and exit codes unchanged.
2. PASS **Non-goals**: no change to exit codes, `--json` names or order, the release of a removed shared folder, shortcut planning or sync, `internal/graph`, the JSON encoder; `internal/multisync` read only.
3. PASS **Governing docs**: `spec/design/cli.md` (routing table row for `internal/cli/sync_render.go`, `status*.go`), `spec/design/sync-engine.md` (`internal/sync/engine_*`, `observation_findings.go`), `spec/design/sync-store.md` (the schema, if an addition is decided), `spec/requirements/sync.md`. Docs with the texts: `docs/troubleshooting.md` holds "Remote read blocked"; `docs/scripting.md` holds no "selected drives" example.
4. PASS **Authority owner**: the child engine's root observation owns the stored row (`internal/sync`); the CLI owns phrasing (`status_condition_descriptors.go`, `sync_render.go`); multisync owns the startup summary.
5. ASSUMPTION **Code scope**: `internal/cli/status_condition_descriptors.go`, `internal/cli/sync_render.go` and their tests, plus whatever the decision under item 12 adds. Validation: the decision. Note: (c) asks for `SelectedCount` to be split into two named methods so no caller keeps it; the method lives in `internal/multisync/report.go`, which is out of scope beyond reading.
6. ASSUMPTION **Regression plan**: status tests for a child root answering 404 and 403 (text and JSON) and a `sync_render` test of a one-drive run with two shortcut children, one with left work. Blocked on item 12: the 404 test cannot be written against the stored row as it is.
7. PASS **Verification plan**: as the brief lists (build, vets, `./internal/cli/...` plain and under `-race` behind the lock, `./internal/sync/...` if touched, targeted lint into the lint log).
8. PASS **Data safety**: presentation only today; a store addition would be decided by the owner.
9. PASS **Secret and credential safety**: no tokens, accounts or logs used; tests use made-up names.
10. PASS **Live provider and destructive operations**: none; no live run, no push.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. PASS **Unknowns and assumptions** (refreshed after the decisions of 2026-11-14):
    - Decided: a new issue type and condition key `remote_root_not_found`, written by the three root paths on `graph.ErrNotFound`; 403 keeps `remote_read_denied`; same path and scope key; the new type takes the arm of `remote_read_denied` in every per-type and per-family switch and list; `sharedFolderUnreadable` accepts both; no schema change, no generation bump (as #900 added `removed_folder_kept`).
    - Decided: `SelectedCount` is split into `SelectedDriveCount` (distinct standalone mount IDs, so a drive named by two results counts once) and `ReportingMountCount` (every result); `internal/multisync/report.go`, its callers and tests are in scope for that split only.
    - Question: can the distinct drive count be made from what a result holds? Current assumption: yes; every drive result carries its canonical ID as `Identity.MountID` with `ProjectionKind` standalone (`mount_spec.go:537-547`, `sync_runtime.go:100-106`); only the process failure of `orchestrator_run_once.go:169-176` names no drive. Validation step: the multisync test of one drive whose shared folders cannot be set up. Owner: `internal/multisync/report.go`.
    - Question: how does status know a store's root is a shared folder's root? Current assumption: the caller knows (`buildChildStatusDrive` reads a child's store); it passes the store's root kind down to the condition descriptors. Validation step: the status tests of both answers at both roots. Owner: `internal/cli/status_snapshot.go`, `internal/cli/status_sync_state.go`, `internal/cli/status_condition_descriptors.go`.

Code scope after the decisions (item 5): `internal/sync` (issue_types.go, condition_keys.go, observation_findings.go, engine_primary_root.go, engine_shortcut_release.go, truth_status.go, engine_runtime_lifecycle.go, engine_runtime_completion.go, engine_runtime_permissions.go, watch_condition_log.go and their tests), `internal/multisync/report.go` and its callers and tests, `internal/cli` (status_condition_descriptors.go, status_sync_state.go, status_snapshot.go, sync_render.go, sync_startup_messages.go, one line of sync_console.go, and tests), docs and spec as listed.
