# Definition of Ready: a removed shared folder's local copy leaves by the delete rule

Worktree `~/src/onedrive-go-wt/shared-folder-release`, branch
`fix/shared-folder-release-by-delete-rule`, one commit on top of f5b735a4.

1. PASS **Task intent**: when a shared folder is removed from the account, its
   local copy leaves only through the rule of a folder deleted on OneDrive:
   the child engine, at the end of its final drain, removes each synced file
   whose content still matches the baseline (to the trash) and each synced
   folder once empty (bundled junk under the configured rule); whatever the
   rule does not remove stays, is counted and named, keeps the drain from
   being clean, and is shown by `status`. The parent removes the alias root
   only as an empty directory; a root that is not empty goes back to the final
   drain. `synctree.RemoveTreeNoFollow` is deleted.
   Done means: the eight decided items each with a test; a source-reading test
   holds that no product code removes a tree but the junk removal of the
   ordinary folder delete; the "only locally" table in the report; status shows
   the kept items; a second sync with nothing changed does nothing new; the
   re-add route plans no remote delete; a crash at any release step converges
   with no remote delete.
2. PASS **Non-goals**: the exit status rule and the report counts of f5b735a4
   (only read, not changed); panic boundaries; e2e waits; the `trash` CLI
   reaching shortcut children's trashes; any store schema change.
3. PASS **Governing docs**: `spec/requirements/sync.md` R-2.4.8 (line 52, read
   by range); `spec/design/sync-engine.md` (shortcut lifecycle, lines 900-980,
   one-shot report 500-560), `sync-control-plane.md` (lines 185-330),
   `sync-shortcut.md`, `sync-execution.md` (local delete), `sync-domain.md`
   (issue types), `cli.md` (status conditions), `system.md` (synctree);
   CLAUDE.md lines 55-236; SUBAGENT-RULES.md.
4. PASS **Authority owner**: the child engine (`internal/sync`, its executor and
   its store) owns the release of its own tree, its baseline rows and the kept
   findings it records; the parent engine owns `shortcut_roots` and the alias
   root directory; multisync only classifies the child report and acknowledges
   a clean drain; the CLI owns the wording of the new status condition.
5. PASS **Code scope**: `internal/sync` (new `engine_shortcut_release.go`,
   `engine_run_once.go` call site, `engine_startup.go` untouched,
   `shortcut_root_lifecycle.go`, `shortcut_root_planner_cleanup.go`,
   `shortcut_root_status.go`, `shortcut_root_transition.go`, `api_types.go`,
   `report_left_work.go`, `issue_types.go`, `condition_keys.go`,
   `truth_status.go`, `observation_findings.go`, three `engine_runtime_*`
   switch sites, `executor_delete.go` wording helper, `types.go`,
   `store_write_baseline.go`, `store_delete_ledger.go`), `internal/synctree`
   (delete `RemoveTreeNoFollow`), `internal/multisync` (run options, drain
   detail), `internal/cli` (status condition descriptor), spec docs. Not
   touched: store schema, `internal/localpath`, `internal/localtrash`,
   `spec/design/cli.md` tables beyond the new condition row.
6. PASS **Regression plan**: first test, run on the old code and failing there:
   a tree walk stopped at a symbolic link, then a normal run, deleted the two
   walked files in the shared folder (`remote.deletes = [item-b item-a]`;
   neither the root identity check nor the delete hold stopped it). New tests:
   the re-add route after a release that kept files; per kind (filter-hidden
   file, file without a baseline row, edited file, symlink, FIFO) what stays and
   how the drain is held; a synced tree leaves to the trash with no baseline
   row; bundled junk; a symlink sorted after regular files leaves nothing
   removed; the parent refuses a non-empty root and the binding returns to the
   final drain; crash points during the release, both crash models; an
   unreachable shared folder releases only what matches; multisync names the
   leftovers; status shows the kept condition; the source-reading test.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, `go vet -tags
   'e2e e2e_full' ./e2e/...`; `go test -count=1 ./internal/multisync/...
   ./internal/synctree/...`; targeted `-run` in `internal/sync` and
   `internal/cli` plus `-run 'Shortcut|FinalDrain|LocalDelete|DeleteLocal'`
   and the `ListsEvery|Table|Governs|Exhaustive|Registered` tests; `-race` with
   `-run` on the new tests; one targeted golangci-lint at the end into
   W30-lint.log. No live tests (shortcut live tests cannot run on this host).
8. PASS **Data safety**: no file leaves without the executor's verified delete
   (content checked through the descriptor that is removed, to the trash);
   nothing is deleted on OneDrive by a release; the release's baseline row
   leaves in a local-delete commit made before the file is removed and
   checkpointed, so no crash or lost WAL can leave "local absent, row present"
   for a later run to read as a local delete; a release that cannot take the
   whole tree takes nothing while the shared folder is reachable, so a blocked
   release never re-downloads the folder on every pass.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names or
   credential-bearing logs involved; tests use `owner@example.com`-style
   placeholders already in the suite.
10. PASS **Live provider and destructive operations**: none. No live account,
    no git history rewrite, no dependency change, no push.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does the ack protocol need a schema change to show what was kept?
      Current assumption: no; the child records the kept items as observation
      issues of a new type in its own store, which `status` already reads for a
      shortcut child. Validation step: status test over a child store.
      Owner path if wrong: `internal/sync/shortcut_root_*`, `internal/cli`.
    - Question: can the executor's local delete run for a file whose remote item
      still exists without a new action kind? Current assumption: yes,
      `ExecuteLocalDelete` reads only the action's baseline view.
      Validation step: release tests over the fake remote. Owner path if wrong:
      `internal/sync/executor_delete.go`.
    - Question: what makes a crash between removing a file and committing its
      outcome safe when the remote item still exists? Current assumption: the
      release commits the row's removal first and checkpoints the WAL before
      removing; refused files get their row back. Validation step: crash-point
      tests in both crash models. Owner path if wrong: the release file.
