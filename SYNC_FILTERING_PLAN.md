# Sync Filtering Configuration and Boundary Cleanup Plan

## Summary

Add per-drive sync filtering as a real product surface, remove the old implicit/marker behavior, and make one planner-facing current-state projection the authoritative filtering boundary. There must be no filter fingerprints and no compatibility shims.

Filters apply to sync only. Direct file operations (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`, `mv`, `cp`) continue to show and operate on provider truth without consulting sync DB state or sync filters.

Correctness comes from one rule: the planner compares raw `baseline` against filtered current local and remote views. Local observation may persist only filtered local truth because full local observation replaces `local_state`. Remote observation should persist raw manageable remote truth because Graph deltas are incremental and unchanged hidden remote items may need to become visible later. The planner-visible remote view is filtered at planning time. Early filtering during local observation, remote delta handling, and watch setup is useful for performance and noise reduction, but it must share the same `ContentFilter` semantics.

External behavior to incorporate:

- Microsoft documents OneDrive/SharePoint invalid names, SharePoint root `forms`, `_vti_`, OneNote notebook limits, size limits, and path restrictions: [Microsoft Support](https://support.microsoft.com/en-us/office/restrictions-and-limitations-when-you-sync-sharepoint-libraries-to-your-computer-through-onedrive-for-work-or-school-14fdf1b3-61e5-49cd-a9e1-a66588505b4e).
- Microsoft Graph delta returns stateful/sparse change records, including deleted records that must be interpreted against prior state: [Microsoft Graph delta](https://learn.microsoft.com/en-us/graph/api/driveitem-delta?view=graph-rest-1.0).
- Microsoft Graph package items are not normal file/folder items; OneNote notebooks are the important package example: [Microsoft Graph package facet](https://learn.microsoft.com/en-us/graph/api/resources/package?view=graph-rest-1.0).
- rclone has explicit include/exclude filter semantics that are separate from symlink handling: [rclone filtering](https://rclone.org/filtering/).
- rclone ignores symlinks by default and makes link handling opt-in: [rclone docs](https://rclone.org/docs/#links).
- abraunegg exposes explicit skip-dir, skip-file, skip-dotfiles, sync-list, and skip-symlinks configuration. It also supports `.nosync`; copy the explicit config ideas, not marker-file behavior: [abraunegg usage](https://github.com/abraunegg/onedrive/blob/master/docs/usage.md).
- Syncthing ignore rules are root-relative and ignored items are not synchronized. Its delete-disposable marker is useful as a caution, but this client must not silently delete invalid user files: [Syncthing ignores](https://docs.syncthing.net/users/ignoring.html).

## Product Decisions

Add these per-drive config keys. All default to off or empty.

| Key | Default | Scope | Behavior |
|---|---:|---|---|
| `ignored_dirs = []` | empty | local + remote | Root-relative slash paths to exclude as full directory subtrees. |
| `included_dirs = []` | empty | local + remote | Empty means the full drive is in scope. Non-empty means only these root-relative directories, descendants, and required ancestor containers are in scope. |
| `ignored_paths = []` | empty | local + remote | File or directory glob patterns to exclude. Patterns with `/` match root-relative paths. Patterns without `/` match basenames. A matched directory excludes its full subtree. |
| `ignore_dotfiles = false` | false | local + remote | Exclude any path with any component beginning with `.`. |
| `ignore_junk_files = false` | false | local + remote | Exclude bundled OS/app/editor/browser junk and temp files. |
| `follow_symlinks = false` | false | local only | Skip symlinks by default. When true, observe symlink targets with cycle protection and symlink-safe delete behavior. |

Bundled `ignore_junk_files` patterns:

- `.DS_Store`
- `Thumbs.db`
- `__MACOSX`
- `._*`
- `.~*`
- `*.tmp`
- `*.swp`
- `*.partial`
- `*.crdownload`
- `~*`, except `~$*`

`~$*` is intentionally not treated as junk. It should remain an invalid OneDrive-name issue for visible local paths, because Microsoft treats leading `~$` names as disallowed rather than as harmless OS trash.

`*.partial` needs one extra safety rule. User-created files ending in `.partial` are only product junk when `ignore_junk_files = true`. Client-owned transfer artifacts are different: they are internal runtime artifacts and must never upload even when `ignore_junk_files = false`. The implementation distinguishes owned transfer artifacts with namespaced `.onedrive-go.*.partial` files instead of treating every `*.partial` as disposable internal state.

Precedence:

1. Structural boundaries win first: shortcut protected roots, child mount roots, Personal Vault, OneNote/package items, malformed remote items, and remote root placeholders.
2. `included_dirs` narrows the visible sync universe.
3. `ignored_dirs`, `ignored_paths`, `ignore_dotfiles`, and `ignore_junk_files` remove content from that universe.
4. Ignore wins over include.
5. Ignored or out-of-include content does not create local observation issues.
6. OneDrive compatibility failures are not user filters. They remain always-on local observation issues for visible paths.

Always-on local observation issues:

- Invalid OneDrive names.
- SharePoint root-level `Forms`, only when the configured parent drive is SharePoint and only at that library root.
- Path too long.
- File too large.
- Case collision.
- Local read denied.
- Hash failure or recovered hash panic.
- Symlink safety block when `follow_symlinks = true` and a remote delete would recurse through a symlinked directory target.

Always-on remote/provider exclusions:

- OneNote/package items.
- Personal Vault.
- Remote root and mount-root structural rows.
- Shortcut placeholders after conversion into shortcut topology.
- Malformed unusable remote delta records.
- Provider-only junk or system artifacts that have no local equivalent and cannot be managed safely.

Non-content filters:

- Retry work and blocked scopes are not product filters. They are execution/planning bookkeeping and should only be pruned after filtered planning decides what work still exists.
- Sync mode action suppression is not product filtering. `download-only` and `upload-only` still observe both sides, then defer forbidden action classes.
- Permission/read-only action suppression is not product filtering. Remote-write-denied scopes can block uploads, remote deletes, remote moves, and remote folder creates while still allowing downloads; that is action admission, not content visibility.
- Drive discovery normalization is not content filtering. Personal phantom/system drives should remain filtered through the `/me/drive` discovery path, but this does not belong in sync content filtering.

## Historical Pre-Implementation Filtering Inventory

This is the filtering and filtering-like behavior found in the code audit before the implementation work in this increment. References in this section are intentionally historical: they explain what was consolidated, preserved as a separate boundary, or deleted.

1. Junk/temp names: local and remote, live.
   - Where: `IsAlwaysExcluded()` in `internal/sync/scanner.go`, called by local `shouldObserveWithFilter()` and by remote `ItemConverter.ClassifyItem()`.
   - Why: never sync OS/editor/download junk such as `.DS_Store`, `Thumbs.db`, `*.partial`, `*.tmp`, `*.swp`, `*.crdownload`, `._*`, and `.~*`.
   - Status: real hard-coded bidirectional content filtering.
   - Decision: make this `ignore_junk_files = false` by default, backed by the shared `ContentFilter`.

2. Remote junk commit guard: remote, live defensive duplicate.
   - Where: `CommitObservation` in `internal/sync/store_write_observation.go`.
   - Why: prevents excluded junk from entering `remote_state` even if conversion missed it.
   - Status: live but duplicative and in the wrong authority boundary.
   - Decision: remove product filtering from the store. The store should persist raw manageable remote observations; planner visibility and wake decisions should apply the product filter outside the store.

3. `.nosync` guard: local only, live.
   - Where: full scan in `internal/sync/scanner.go`, watch handling in `internal/sync/observer_local.go`, sentinel error in `internal/sync/errors.go`.
   - Why: likely copied from other OneDrive clients as an unmounted-root safety marker.
   - Status: hidden marker behavior, not a product config surface.
   - Decision: delete completely. If the product needs mount-disappeared protection, implement it as explicit sync-root identity safety, not a magic filename.

4. `LocalFilterConfig.SkipDotfiles`: local only, effectively dead.
   - Where: `shouldSkipConfiguredPath()` in `internal/sync/scanner.go`.
   - Why: skip any path with a dot-prefixed component.
   - Status: no TOML key and no normal multisync wiring; reachable only through tests or nonstandard constructors that set `EngineMountConfig.LocalFilter`.
   - Decision: replace with `ignore_dotfiles = false`, bidirectional when enabled.

5. `LocalFilterConfig.SkipDirs`: local only, effectively dead as product config.
   - Where: `shouldSkipConfiguredPath()` in `internal/sync/scanner.go`; also reused by stale partial cleanup.
   - Why: skip rooted local subtrees.
   - Status: no config key.
   - Decision: replace with `ignored_dirs = []` plus `included_dirs = []`, both bidirectional.

6. `LocalFilterConfig.SkipFiles`: local only, effectively dead.
   - Where: `shouldSkipConfiguredPath()` in `internal/sync/scanner.go`.
   - Why: skip local path glob patterns.
   - Status: no config key.
   - Decision: replace with `ignored_paths = []`, bidirectional, covering both files and directories.

7. `LocalFilterConfig.SkipSymlinks`: local only, effectively dead and the wrong default.
   - Where: `internal/sync/symlink_observation.go`.
   - Why: skip symlink aliases.
   - Status: no config key; current behavior follows symlinks by default unless this internal field is set.
   - Decision: replace with `follow_symlinks = false`. Default skip. Opt-in follow must have cycle protection and symlink-safe delete semantics.

8. Shortcut protected roots: local parent only, live.
   - Where: `SetProtectedRoots()` in `internal/sync/observer_local.go`, path suppression in `internal/sync/scanner.go`, lifecycle/event handling in `internal/sync/observer_local_handlers.go`.
   - Why: prevent the parent engine from treating child mount roots as ordinary parent content.
   - Status: live structural ownership enforcement, not user ignore semantics.
   - Decision: keep separate from `ContentFilter`. Explain together with remote shortcut placeholders because they are the two sides of the same parent/child boundary.

9. Invalid OneDrive local names: local only, live.
   - Where: `ValidateOneDriveName()` in `internal/sync/scanner.go`.
   - Why: local files with impossible OneDrive names cannot upload safely.
   - Status: reportable observation issue, not bidirectional ignore.
   - Microsoft facts to preserve: disallowed characters include `" * : < > ? / \ |`; leading/trailing spaces are invalid; reserved names include `.lock`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9`, `_vti_`, `desktop.ini`, and names beginning with `~$`.
   - Decision: keep as mandatory local issues for visible paths only. Junk files are optional filters; invalid names are not.

10. SharePoint root `Forms`: local only, live.
    - Where: local validation in `internal/sync/scanner.go`; `RejectSharePointRootForms` is configured through multisync mount spec/config.
    - Why: SharePoint reserves root `Forms` in document libraries.
    - Status: validation rule, not user filtering.
    - Decision: keep as local issue only when the configured parent drive is SharePoint and only at the library root. Do not reject personal OneDrive folders named `Forms` or nested folders unless Microsoft requirements say so.

11. Path length limit: local only, live.
    - Where: local path validation in `internal/sync/scanner.go`.
    - Why: OneDrive/SharePoint have a decoded path limit; current code uses `MaxOneDrivePathLength = 400`.
    - Status: reportable observation issue.
    - Decision: keep as mandatory local issue for visible paths only and auto-resolve from latest observation.

12. File size limit: local only, live.
    - Where: full scan in `internal/sync/scanner.go`, watch/write handling in `internal/sync/observer_local_handlers.go`.
    - Why: OneDrive has an individual-file upload limit; current code uses `MaxOneDriveFileSize` for 250 GB.
    - Status: reportable observation issue.
    - Decision: keep as mandatory local issue for visible paths only and auto-resolve when the file is removed, shrunk, or moved out of visible scope.

13. Case collision detection: local only, live.
    - Where: full scan in `internal/sync/scanner.go`, watch early rejection in `internal/sync/observer_local_handlers.go`.
    - Why: local case-sensitive filesystems can contain sibling names that OneDrive cannot safely represent together.
    - Status: reportable observation issue.
    - Decision: keep as mandatory local issue for visible paths only and auto-resolve when the collision disappears.

14. Local read denied and hash failure/panic: local only, live.
    - Where: `internal/sync/scanner.go`, `internal/sync/single_path.go`, observation finding/reconcile code.
    - Why: unavailable or unhashable local truth must block unsafe structural planning.
    - Status: observation issues, not filters.
    - Decision: keep as mandatory local issues for visible paths only. Do not plan deletes from incomplete local observation.

15. OneNote package items: remote only, live.
    - Where: Graph normalization in `internal/graph/normalize.go` and sync item conversion paths.
    - Why: Graph `package` items are not normal file/folder items; OneNote notebooks are the important example.
    - Status: provider-specific remote filter.
    - Decision: sync should exclude them remotely. Direct file operations should show Graph truth if the command can represent it, so sync-only package exclusion should not live in generic Graph normalization.

16. Malformed remote delta items: remote only, live.
    - Where: empty ID/name/path and recoverability checks in `internal/sync/item_converter.go`.
    - Why: malformed or sparse provider records cannot safely materialize state or actions.
    - Status: provider safety, not user filtering.
    - Decision: keep as remote observation validation. Deleted/sparse delta records are a real Graph category; literally empty IDs are defensive handling. Depending on severity, skip, surface observation failure, or force remote resync.

17. Remote root and mount-root item suppression: remote only, live.
    - Where: root skip and mount-root skip in `internal/sync/item_converter.go`.
    - Why: the sync root itself is the mount boundary, not a child item under itself.
    - Status: structural root suppression.
    - Decision: keep and document as structural, not as user content filtering.

18. Shortcut placeholders: remote parent only, live.
    - Where: shortcut handling in `internal/sync/item_converter.go` and shortcut topology files.
    - Why: the parent engine converts remote shortcut placeholders into shortcut topology and child work. The child engine syncs target contents.
    - Status: topology ownership, not ignore semantics.
    - Decision: keep separate from product filters. Document together with protected roots: item 8 is the local side, item 18 is the remote side.

19. Personal Vault: remote only, live.
    - Where: `internal/sync/item_converter.go`.
    - Why: vault lock/unlock can make content appear unavailable or deleted and can cause data loss if treated as ordinary remote truth.
    - Status: provider safety.
    - Decision: keep remote-only. There is no normal local equivalent that should become bidirectional filtering.

20. Sync-mode action filtering and permission-scope action admission: neither local nor remote observation.
    - Where: planner action filtering in `internal/sync/planner.go`.
    - Where: remote-write permission scope admission in `internal/sync/scope_semantics.go`.
    - Why: upload-only/download-only suppress action types after planning; remote-write-denied scopes suppress remote-mutating actions under read-only shared-folder boundaries.
    - Status: not content filtering; not retry/block scope implementation.
    - Decision: keep out of content filtering except for tests that prove content visibility, sync-mode suppression, and permission/action admission remain separate.

21. Disposable cleanup during local folder delete: local executor only.
    - Where: `internal/sync/executor_delete.go`.
    - Why: junk/temp files should not block removing an otherwise empty folder.
    - Status: cleanup behavior, not observation filtering.
    - Decision: keep only for owned artifacts or explicitly disposable junk. Do not treat invalid names, unreadable children, or arbitrary `ignored_paths` matches as disposable.

Additional dead code and drift to remove:

- `EngineMountConfig.LocalFilter` exists in `internal/sync/api_types.go`, and `Engine` stores it, but normal multisync setup does not populate it and drive config has no keys for it.
- Config docs mention filter-pattern validation even though the config schema has no filter fields.
- `.odignore` has no live filtering behavior. Active marker-era watch-capture fixtures must be deleted; only explicit tests proving marker-looking files are ordinary files should remain.
- `spec/reference/onedrive-filtering.md` is a placeholder that should be replaced with real external facts or deleted.

## Plain-English Taxonomy

Bidirectional product content filters:

- `ignore_junk_files`, when enabled.
- `ignore_dotfiles`, when enabled.
- `ignored_paths`.
- `ignored_dirs`.
- `included_dirs`.

Local-only observation issues:

- Invalid names.
- SharePoint root `Forms`.
- Path too long.
- File too large.
- Case collisions.
- Local read denied.
- Hash failure or recovered hash panic.
- Symlink safety blocks created by opt-in symlink following.

Remote-only provider/structural exclusions:

- OneNote/package items.
- Personal Vault.
- Remote root and mount-root structural rows.
- Shortcut placeholders.
- Malformed unusable remote records.
- Provider-only junk/system artifacts with no safe local equivalent.

Not filters:

- Retry work.
- Blocked scopes.
- Upload-only/download-only action suppression.
- Read-only shared-folder remote-write action suppression.
- Direct file operation behavior.
- Drive discovery normalization.
- Executor cleanup, even when it uses filter decisions to decide which junk is disposable.

## Superseded Earlier Ideas

The earlier no-fingerprint sketch had a few ideas that should not be carried into implementation:

- Do not run a broad transaction that removes excluded paths from durable `baseline`, `local_state`, `remote_state`, `observation_issues`, `retry_work`, and block scopes as the primary filtering mechanism. `baseline` stays raw; retry and block state prune after planning. `local_state` is replaced by local observation. `remote_state` stays a raw manageable remote mirror.
- Do not treat a remote item that leaves the visible set as a provider delete. Remote observation updates the raw remote row to its new path; the filtered planner view then stops seeing it at the old visible path.
- Do not keep the remote commit guard as a second product filter by calling the compiled content filter from `CommitObservation`. Store code should not own product visibility.
- Do not treat `.nosync` as a local-only feature to preserve. It should be removed, not documented as a supported local guard.
- Do not classify retry/block scope behavior as content filtering. It is downstream planning/execution bookkeeping that should naturally prune after filtered planning.

## Code Path Details to Preserve

This is the end-to-end implementation walk that should guide the change. It intentionally names current files and functions so the implementation does not miss side-door filtering.

1. Config has no real product filtering keys today.
   - `config.Drive` currently contains sync directory, pause fields, display name, and owner, but no include/ignore/junk/symlink product keys.
   - Unknown drive keys are rejected in `internal/config/unknown.go`, so every new key must be registered there.
   - `LocalFilterConfig` in `internal/sync/api_types.go` is explicitly local-only.
   - Multisync mount construction in `internal/multisync/mount_engine_config.go` does not populate `LocalFilter` from drive config.
   - Consequence: the existing filter type is not the product surface. Replace or rename it into bidirectional content-filter config rather than trying to stretch the old local-only vocabulary.

2. Parent, child, and multisync need one explicit filter handoff.
   - Parent mount specs are built from configured drives in `internal/multisync/mount_spec.go`.
   - Shortcut child run commands in `internal/sync/shortcut_topology.go` currently carry the child local root, remote drive ID, remote item ID, and root identity, but not filter policy.
   - Parent shortcut publication happens in `internal/sync/shortcut_root_publication.go`.
   - Consequence: inherited child filters must be added to the parent-published child command. Multisync should carry the command and launch engines; it should not interpret include/ignore semantics.

3. Local observation already has a useful early pruning point, but policy is hard-coded/mis-shaped.
   - The main local filter path is `shouldObserveWithFilter()` in `internal/sync/scanner.go`.
   - It currently applies hard-coded junk, local-only skip dirs/files/dotfiles, invalid OneDrive name checks, and path-length checks.
   - Oversized file checks happen later after stat.
   - Consequence: reuse the early pruning shape, but feed it from `ContentFilter` and keep validation issues separate from product visibility filters.

4. Watch and single-path observation mostly share the local filter path.
   - Watch event handling calls the same local filtering before create/write/delete handling in `internal/sync/observer_local_handlers.go`.
   - Single-path retry/trial observation also uses the local filter path in `internal/sync/single_path.go`.
   - Observation issue reconciliation in `internal/sync/observation_reconcile_policy.go` distinguishes full managed batches from path-scoped managed paths.
   - Consequence: whole scans can replace the managed issue family, but path-scoped watch/single-path observation can only reconcile the paths it proved. If planning requires globally fresh issue state, the engine must run or require a current full local observation before planning.

5. Symlink behavior is currently opposite of the desired product default.
   - `internal/sync/symlink_observation.go` follows symlinks unless `SkipSymlinks` is set.
   - It already has cycle protection based on resolved directory paths.
   - Local delete starts with `Stat` in `internal/sync/executor_delete.go`, which follows symlinks.
   - Consequence: `follow_symlinks = true` is not just an observation toggle. It requires alias-root semantics, no-follow delete helpers, and blocked descendant deletes under followed directory symlinks.

6. Graph package filtering used to affect direct CLI behavior and must stay sync-only.
   - `normalizeListedItems()` in `internal/graph/normalize.go` must keep package items as provider truth.
   - Delta normalization must do the same.
   - `ls` calls `session.ListChildren` in `internal/cli/ls.go`, which flows through Graph list methods.
   - Consequence: if OneNote/package filtering is sync-only, move it out of generic Graph normalization so direct file commands can show provider truth.

7. Remote conversion combines several unrelated suppression reasons.
   - `internal/sync/item_converter.go` skips root items, empty IDs, mount roots, shortcut placeholders, packages, Personal Vault, and deletes without recoverable paths.
   - Consequence: split the vocabulary in code/docs. Some are structural, some are provider safety, some are malformed-input handling, and hard-coded user junk should move behind configured product filtering.

8. Store-level remote filtering is a hidden policy owner.
   - `CommitObservation` in `internal/sync/store_write_observation.go` silently skips hard-coded junk before writing `remote_state`.
   - Consequence: remove this product policy from the store. Tests that currently expect filtered junk to be absent from `remote_state` should move to the remote observation projection/mutation boundary, not store internals.

9. Planner filtering must happen before comparison SQL.
   - Comparison SQL in `internal/sync/sqlite_compare.go` reads directly from `baseline`, `local_state`, and `remote_state`.
   - Reconciliation SQL in the same file also reads those tables directly.
   - `loadCurrentInputsTx` in `internal/sync/engine_current_plan.go` queries comparison/reconciliation rows before planner construction.
   - Consequence: filtering `localRows` and `remoteRows` after `queryComparisonStateWithRunner` is too late. Use filtered CTEs or transaction-local scratch tables inside the planning read transaction.

10. Retry and block scopes are already downstream cleanup.
    - After planning, retry work and block scopes are pruned to current actions in `internal/sync/engine_current_plan.go`.
    - Consequence: keep this model. Filtered paths produce no actions, and stale retry/block state falls away without fingerprints.

11. Executor cleanup currently has the highest data-loss smell.
    - `IsDisposable` in `internal/sync/executor_delete.go` treats invalid OneDrive names as safe to remove.
    - `FindNonDisposable` treats read errors as disposable-ish by returning empty.
    - Consequence: remove invalid-name disposal and make unreadable children block deletion. Observation issues and disposable cleanup must not disagree about whether user content is safe.

12. Transfer-artifact cleanup is filtering-adjacent.
    - Post-sync housekeeping in `internal/sync/engine_housekeeping.go` deletes stale partial files and upload sessions.
    - `internal/driveops/stale_partials.go` already has `SkipDirs`.
    - Current housekeeping skips configured local `SkipDirs` and protected shortcut roots.
    - Consequence: once `included_dirs` exists, housekeeping must also avoid out-of-include subtrees. Owned artifact cleanup must not walk content the user excluded from sync.

13. Remote orphan materialization is not filtering, but it interacts badly with filtering.
    - `internal/sync/item_converter.go` can materialize an item whose parent is missing as just its leaf name.
    - Consequence: do not let missing-parent remote items become fabricated root-level paths. With include/ignore rules, a fabricated path can be classified incorrectly and lead to bogus plans.

## Behavioral Side Effects to Document

1. Adding a filter hides both current sides and should cause baseline cleanup only, not local or remote deletes.
2. Moving a visible local file into an ignored directory looks like a local deletion from the visible sync set. The remote visible copy may be deleted. This is probably correct, but it must be documented.
3. Moving a visible remote file into an ignored directory looks like a remote deletion from the visible sync set. The local visible copy may be deleted. This is also probably correct, but it must be documented.
4. Removing a filter later can cause resync or conflict if local and remote changed differently while hidden.
5. `included_dirs` needs ancestor semantics: ancestors are traversal/materialization containers, but siblings are excluded.
6. Path matching is case-sensitive for v1. If the matcher is still `slashpath.Match`-style case-sensitive matching, macOS users on case-insensitive filesystems may be surprised.
7. Symlink following should be treated as a narrow opt-in feature. If safe directory-symlink delete semantics are too large for the first implementation, narrow `follow_symlinks` to file symlinks only or keep directory symlink following disabled until the alias-root design is complete.

## Implementation Drift Items

These are not new product requirements; they are concrete places where code or specs disagreed with this plan during the audit. Status reflects the implementation direction in this increment.

1. `spec/requirements/sync.md` no longer says user-configured bidirectional path narrowing is removed or that symlink skipping has no user switch.
2. `spec/design/sync-observation.md` and `spec/design/data-model.md` now distinguish filtered local snapshots from raw manageable remote mirrors. Planner SQL uses filtered local/remote temp views before comparison.
3. `spec/requirements/non-functional.md` now frames junk/temp behavior as configurable product filtering plus transfer-artifact cleanup boundaries, not unconditional deletion or upload suppression for arbitrary user files.
4. `spec/design/graph-client.md` now keeps package items visible to Graph callers and assigns sync-only package filtering to remote observation.
5. `spec/reference/onedrive-filtering.md` now contains the external facts that informed the product boundary.
6. `.nosync` E2E and verifier references have been removed; marker-looking files are tested as ordinary files.
7. Historical references to deleted `sync_paths`, `ignore_marker`, and `.odignore` concepts may remain only when explicitly historical. Active docs/tests/fixtures should not use that vocabulary for current behavior.

## Target Architecture

The target design has three distinct concepts that must not be collapsed:

1. Product visibility filters.
   - Owned by the new `ContentFilter`.
   - Configurable per drive.
   - Mostly bidirectional: include dirs, ignored dirs, ignored paths, dotfiles, junk.
   - Local-only for symlink following because OneDrive has no symlink object type.

2. Provider/structural boundaries.
   - Owned by sync observation/conversion/topology code.
   - Not user-configurable.
   - Includes OneNote/package exclusion for sync, Personal Vault, root rows, mount roots, shortcut placeholders, malformed remote items, and protected roots.

3. Local admissibility issues.
   - Owned by local observation.
   - Not product visibility filters.
   - Always-on for visible content.
   - Projected from the latest observation so they auto-resolve when no longer observed.

Data ownership:

- The store owns durable sync state: filtered `local_state`, raw manageable `remote_state`, raw `baseline`, retry work, block scopes, and observation issues.
- The filter owns derived visibility decisions. It does not perform I/O or mutate durable state.
- Local observation owns applying visibility decisions before committing `local_state`.
- Remote observation owns raw provider classification and may use visibility decisions to suppress planner wakeups, but it should not remove raw manageable rows merely because they are hidden by product filters.
- The planner owns comparison of raw `baseline` against filtered current local/remote projections.
- The executor owns side effects and must enforce delete safety, especially around symlinks, unreadable children, invalid names, and owned artifacts.
- Multisync owns mount lifecycle and child launch. It transports child filter configuration but does not interpret filter policy.
- Parent sync engines own shortcut topology and parent-relative filter translation for automatic children.

## Future Filtering Modules

This plan is not a radical centralization where every kind of filtering moves into one giant gate. It centralizes one thing: product content visibility. Other filtering-like decisions stay near the authority that owns them.

1. Config validation and resolution.
   - Owns user-facing keys, defaults, shape validation, path normalization, and resolved-drive plumbing.
   - Does not decide whether an observed item is visible during a sync pass.

2. `ContentFilter`.
   - Owns pure product visibility decisions for `included_dirs`, `ignored_dirs`, `ignored_paths`, `ignore_dotfiles`, `ignore_junk_files`, and `follow_symlinks`.
   - Is deterministic and side-effect free.
   - Does not own provider structural exclusions, observation issue creation, store writes, execution cleanup, retry work, or block scopes.

3. Planner-visible current-state projection.
   - Owns the authoritative correctness boundary.
   - Builds filtered views of current `local_state` and `remote_state` before comparison and move detection.
   - Is usually a no-op for `local_state` rows after local observation, but is essential for `remote_state` because remote rows are raw manageable provider truth.
   - Keeps `baseline` raw so newly hidden paths become baseline cleanup, not local or remote delete actions.

4. Local observation admission.
   - Uses `ContentFilter` early to avoid walking, hashing, watching, and waking on ignored/out-of-include content.
   - Owns local-only admissibility issues: invalid names, path too long, file too large, case collision, read denied, hash failure, SharePoint root `Forms`, and symlink safety blocks.
   - Owns local structural suppression for protected shortcut child roots.

5. Remote observation conversion.
   - Owns provider/structural suppression: root rows, mount-root rows, shortcut placeholders, Personal Vault, OneNote/package items, malformed remote records, and unrecoverable deletes.
   - Persists raw manageable remote observations even when product filters currently hide their paths.
   - Uses `ContentFilter` to decide whether a raw remote mutation changes the filtered planner-visible projection.
   - If an ignored delta does not change the filtered projection, it should only update raw remote state/cursor/progress and should not wake the planner.

6. Store and observation issue reconciliation.
   - Owns durable state and projection-based issue reconciliation.
   - Does not own product content filtering.
   - Persists the raw manageable remote observation it is given, except for provider records that sync observation has already classified as unmanageable.

7. Shortcut parent/child filter handoff.
   - Parent engines own translating parent-relative filter config through shortcut aliases into child-relative filter config.
   - Multisync only carries parent-declared child commands and launches child engines.
   - Child engines apply their own child-relative `ContentFilter` to independent child state.

8. Executor, cleanup, and action admission.
   - Do not own product visibility.
   - Own delete safety, symlink no-follow behavior, owned transfer-artifact cleanup, disposable-junk cleanup, and permission/sync-mode action suppression.
   - May consult filter-derived facts only to avoid touching excluded/protected content or to decide explicitly disposable junk; they must not invent a second content policy.

9. Direct file operations.
   - Stay outside sync filtering.
   - Graph/file commands show provider truth unless the command itself owns a provider limitation.

## Implementation Plan

### 1. Update Requirements and Design Docs First

Update the docs before or alongside code so the implementation has one explicit contract:

- `spec/requirements/sync.md`: restore bidirectional path narrowing as an implemented product capability; remove the statement that marker/path narrowing is removed.
- `spec/requirements/configuration.md`: document the six per-drive config keys, defaults, validation, and examples.
- `spec/requirements/non-functional.md`: clarify that temp/junk upload prevention is controlled by `ignore_junk_files`, while owned transfer artifacts are always protected.
- `spec/design/config.md`: add the config schema, validation rules, and resolved-drive behavior.
- `spec/design/sync-observation.md`: separate product visibility, provider/structural suppression, and local issue projection.
- `spec/design/sync-planning.md`: state that planner input filtering is the correctness boundary and delta filtering is only an optimization.
- `spec/design/sync-store.md`: state that durable state stores raw observations and that visibility is derived.
- `spec/design/sync-engine.md`: document parent/child filter ownership and retry/block cleanup behavior.
- `spec/design/data-model.md`: clarify which state tables are raw and which planner views are filtered.
- `spec/design/graph-client.md`: remove sync-only OneNote/package filtering from generic Graph normalization, unless direct command requirements explicitly say otherwise.
- `spec/reference/onedrive-filtering.md`: replace the placeholder with the researched Microsoft/rclone/abraunegg/Syncthing facts or delete it if the facts live elsewhere.

### 2. Add Per-Drive Config Surface

Implement config without compatibility shims:

- Add a nested or flattened filter config to `config.Drive`.
- Add unknown-key validation for:
  - `ignored_dirs`
  - `included_dirs`
  - `ignored_paths`
  - `ignore_dotfiles`
  - `ignore_junk_files`
  - `follow_symlinks`
- Normalize `ignored_dirs` and `included_dirs` as root-relative slash paths.
- Reject empty entries, `.` root entries, absolute paths, paths containing `..`, paths with duplicate separators, and paths that normalize outside the drive root.
- Treat directory path matching as exact subtree matching, not basename-anywhere matching.
- Validate `ignored_paths` as glob patterns using the matcher semantics implemented by sync.
- `ignored_paths` applies to both files and directories. If a directory path matches, prune the whole directory subtree.
- Keep `ignored_dirs` as exact root-relative subtree paths. Use `ignored_paths` for glob/path-pattern matching.
- Default all booleans to `false` and all lists to empty.
- Thread resolved config through `ResolvedDrive`, sync CLI runtime, multisync mount specs, and `sync.EngineMountConfig`.

### 3. Build the Sync Content Filter

Replace `LocalFilterConfig` with a sync-owned filter model:

- Introduce `ContentFilterConfig` as the resolved product config passed into sync.
- Introduce `ContentFilter` as a deterministic evaluator constructed from config.
- Keep it side-effect free and independent of filesystem, Graph, SQLite, logging, and clocks.
- Expose decisions needed by callers:
  - whether a root-relative path is within include scope,
  - whether a directory subtree should be pruned,
  - whether a file path is ignored,
  - whether a local path may produce observation issues,
  - whether a watch should be installed for a directory,
  - whether a remote change is visible to the planner,
  - whether a path is junk under the configured bundle.
- Do not make `ContentFilter` own structural remote exclusions or local admissibility validation. Those remain separate policy families.

### 4. Implement the Planner Filtering Boundary

Make this the correctness center of the feature:

- Add filtered current-state views before comparison SQL runs.
- Feed comparison and move detection with:
  - raw `baseline`,
  - filtered `local_state`,
  - filtered `remote_state`.
- Do not filter `baseline` directly.
- If a path exists only in `baseline` because both current sides are hidden by a new filter, the plan should produce baseline cleanup only.
- Use transaction-local temp tables or CTEs for visible local/remote rows.
- Do not mutate durable state to build the plan. Durable state is changed by observation/commit paths, not by planner reads.
- Do not add filter fingerprints, filter epochs, synthetic observations, or forced state resets.
- Keep retry/block cleanup after filtered planning. Paths hidden by filters naturally lose planned work and can be pruned by existing retry/block cleanup.

### 5. Handle Filter Config Changes

Filter config changes should be handled by normal engine lifecycle, not by persisted filter fingerprints:

- Add effective filter config to mount spec equivalence. If any of `ignored_dirs`, `included_dirs`, `ignored_paths`, `ignore_dotfiles`, `ignore_junk_files`, or `follow_symlinks` changes, the affected watch runner is not equivalent and must restart on control-socket reload.
- Restarting an affected engine is enough to apply local filter changes because bootstrap/run-once planning performs a full local observation and `ReplaceLocalState` replaces the whole `local_state` table.
- Do not force full remote observation just because filters changed. `remote_state` is raw manageable remote truth, and the planner applies current filters when reading the remote view.
- If the app was off while config changed, no special config-change detection is needed. Startup builds engines from current config, performs full local observation, and filters existing raw `remote_state` during planning.
- Remote freshness remains owned by normal remote observation rules: use the stored delta cursor, run full remote refresh when the cursor is missing/expired or the refresh cadence is due, and run requested `FullReconcile` cases. Filter config changes are not one of those reasons.
- If a user edits config while watch is running but does not request reload, the running engine keeps its current immutable filter config until the next control reload or process restart.

### 6. Fix Local Observation

Use the same `ContentFilter` everywhere local observation decides what to inspect:

- Full scan prunes ignored directory subtrees.
- Full scan does not descend into directories outside `included_dirs`, except ancestors needed to reach included roots.
- Full scan skips ignored paths before hashing, validation, or issue creation.
- Watch setup does not install watches under ignored or out-of-include directories.
- Watch event handling drops ignored/out-of-include events before hashing or planning wake.
- New-directory scan and recursive watch installation use the same filter decisions.
- Single-path retry/trial observation uses the same filter decisions.
- Ignored or out-of-include paths do not create local observation issues.
- `.nosync` files are ordinary files unless matched by explicit config.

Issue reconciliation:

- Full local observation replaces the managed issue families for the complete visible tree.
- Path-scoped observation only replaces the managed path set it actually proved.
- The issue list should be a projection of the latest local observation. If the latest observation no longer finds an issue, it disappears automatically.
- Planning cycles that depend on local state should use current local observation first, so resolved issues do not linger.

### 7. Fix Remote Observation and Store Ownership

Remote observation should maintain raw manageable remote truth while still avoiding unnecessary planner work:

- Remove product junk filtering from `CommitObservation`.
- Persist raw manageable remote rows even when their paths are currently ignored or outside `included_dirs`.
- If a non-deleted remote delta is visible, upsert the raw row into `remote_state`.
- If a non-deleted remote delta is hidden, still upsert the raw row into `remote_state`; the planner-visible view filters it out.
- If a remote delete arrives, remove the raw row from `remote_state` as today.
- Keep structural remote exclusions before persistence only when the item cannot be represented or safely synced at all, such as malformed records, root rows, Personal Vault, mount roots, and shortcut placeholders after topology conversion.
- Move OneNote/package filtering out of generic Graph list/search normalization if it is sync-only.
- Sync remote observation excludes OneNote/package items from the planner-visible projection.
- Direct file operations continue to see provider truth.
- Filter remote deltas before planner wake/reporting as an optimization only.
- Decide planner wake from the filtered projection before and after the raw remote mutation:
  - hidden to hidden: update raw `remote_state`, advance cursor/progress, no planner wake;
  - visible to hidden: update raw `remote_state`, advance cursor/progress, wake planner because the visible remote view lost a path;
  - hidden to visible: update raw `remote_state`, advance cursor/progress, wake planner because the visible remote view gained a path;
  - visible to visible with changed content/path/deletion: update raw `remote_state`, advance cursor/progress, wake planner.
- Cursor advancement must be based on raw observation/progress success, not only visible emitted events.
- Full remote refresh orphan detection uses the raw remote seen set before filtering. Filtering before orphan detection would synthesize fake deletes for ignored-but-existing remote items.
- Full remote refresh should update raw manageable `remote_state`; planner visibility remains a derived filtered view.
- If parent materialization fails after enrichment, treat the observation as incomplete and do not advance the cursor for that batch.

### 8. Fix Symlink Semantics

Change symlink handling from permissive-by-default to explicit opt-in:

- Default behavior: skip symlinks locally.
- Skipped symlinks are not uploaded, downloaded, deleted, or reported as issues.
- `follow_symlinks = true` enables observation of symlink targets with cycle protection.
- Preserve and expand existing cycle detection.
- Record enough local metadata to know whether a visible path is a symlink boundary or descends through a symlinked directory.
- If the remote item corresponding to a symlink boundary is deleted, delete only the symlink itself using no-follow filesystem calls.
- If a remote delete targets a descendant under a followed directory symlink, do not delete target content. Produce a local safety issue or blocked condition instead.
- Executor delete paths must use no-follow helpers for symlink-sensitive operations.
- Do not rely on `Stat` or `RemoveAll` behavior where following symlinks would be unsafe.

### 9. Fix Executor and Cleanup Safety

Remove unsafe cleanup behavior:

- Delete invalid-name handling from disposable cleanup.
- Treat unreadable children as delete blockers, not as empty/disposable directories.
- Only delete configured junk/temp names when `ignore_junk_files = true`.
- Do not treat arbitrary `ignored_paths` matches as disposable.
- Owned transfer artifacts are separate from junk files and may be cleaned regardless of `ignore_junk_files`, but only if ownership is provable.
- Owned transfer artifacts must also be excluded from upload regardless of `ignore_junk_files`; arbitrary user `*.partial` files follow the configured junk policy.
- Replace broad `*.partial` cleanup with the namespaced `.onedrive-go.*.partial` transfer-artifact pattern.
- Transfer cleanup must respect:
  - `included_dirs`,
  - `ignored_dirs`,
  - protected roots,
  - shortcut child mount roots,
  - symlink boundaries.

### 10. Remove `.nosync` Completely

Remove all code, tests, docs, and verifier references for hidden marker behavior:

- Delete `.nosync` constants and sentinel errors.
- Delete scan/watch abort paths.
- Delete E2E cases that assert `.nosync` guard behavior.
- Delete verifier references to `.nosync` E2E names.
- Remove docs/spec references that present `.nosync` as supported.
- Keep or add tests proving `.nosync` and `.odignore` are ordinary files unless matched by explicit config.

### 11. Implement Shortcut Child Filter Ownership

Automatic shortcut children need parent-derived filters without giving multisync policy ownership:

- A standalone configured drive uses its own filter config.
- A parent engine owns translation from parent-relative filters to child-root-relative filters for automatic shortcut children.
- Multisync carries the `ShortcutChildRunCommand` and launches child engines; it does not decide filter semantics.
- If `included_dirs` does not include the shortcut alias or any descendant, do not launch the child.
- If `ignored_dirs` hides the shortcut alias, do not launch the child.
- If filters apply below the shortcut alias, pass child-relative include/ignore rules to the child engine.
- Parent engine owns shortcut topology and protected roots.
- Child engine owns child content state and filtering inside the child root.
- Child cleanup and final drain must not mutate parent-owned filter policy.

### 12. Keep Direct File Operations Independent

Direct commands must remain provider/file operations, not sync-planner operations:

- `ls` should show OneNote/package items if Graph returns them and the command can represent them.
- `get`, `put`, `rm`, `mkdir`, `stat`, `mv`, and `cp` should not consult sync filter config.
- If a direct command has a provider-specific limitation, that limitation should live in the direct command/driveops contract, not in sync content filters.
- Removing sync-only filters from generic Graph normalization may require moving package suppression into sync remote observation instead.

### 13. Implementation Order

Recommended implementation increments:

1. Update specs and requirements so the target contract is explicit.
2. Add config fields, validation, defaults, docs, and resolved-drive plumbing.
3. Add `ContentFilter` and unit tests for precedence and matching.
4. Replace local scan/watch/single-path filtering with `ContentFilter`; delete `.nosync`.
5. Add planner filtered current-state views and baseline cleanup tests.
6. Remove store-level junk filtering; keep raw remote persistence and move remote visibility filtering to planner projection plus remote wake checks.
7. Fix Graph normalization so direct operations are not affected by sync-only package filtering.
8. Rework junk/transfer cleanup and disposable deletion safety.
9. Change symlink default and implement symlink-safe delete/block behavior.
10. Implement shortcut child filter translation and launch suppression.
11. Sweep docs, verifier rules, tests, fixture names, and stale concepts.
12. Run full default verification and targeted sync/filter test suites.

## Required Tests

### Config Tests

- New keys are accepted in drive sections.
- Unknown-key validation recognizes the new keys.
- Defaults are off/empty.
- Invalid include/ignore paths fail validation.
- Invalid path glob patterns fail validation.
- Resolved drives carry filter config into multisync mount config.
- CLI sync runtime passes filter config to `sync.EngineMountConfig`.

### Reload and Restart Tests

- Filter config participates in watch mount equivalence.
- Control reload restarts only mounts whose effective filter config changed.
- Restart after filter config change runs full local observation before planning.
- Filter config changes do not force full remote observation by themselves.
- Startup after config changed while the app was off uses current config without needing a stored filter fingerprint.

### Filter Unit Tests

- `included_dirs` includes ancestors, included roots, and descendants.
- `included_dirs` excludes siblings and unrelated roots.
- Empty `included_dirs` includes the whole drive.
- `ignored_dirs` removes exact subtrees.
- Ignore wins over include.
- `ignored_paths` basename patterns match file and directory basenames.
- `ignored_paths` slash patterns match root-relative file and directory paths.
- Directory paths matched by `ignored_paths` prune the entire directory subtree.
- `ignore_dotfiles` removes any path with a dot-prefixed component.
- `ignore_junk_files` applies equally to local and remote paths.
- Junk bundle matches the documented list.
- Junk bundle does not classify `~$*` as junk.
- Arbitrary user `*.partial` files are visible when `ignore_junk_files = false`.
- Client-owned transfer artifacts are never uploaded even when `ignore_junk_files = false`.

### Planner Tests

- Baseline row plus filtered-away local/remote rows produces baseline cleanup, not local or remote delete.
- Filtered local-only paths do not produce uploads.
- Filtered remote-only paths do not produce downloads.
- Filtered baseline/current combinations do not produce local deletes.
- Removing a filter after local and remote changes reintroduces both sides and plans from current truth.
- Existing retry work for newly filtered paths is pruned after planning.
- Block scopes with no remaining blocked work are pruned after planning.
- Move detection uses filtered local/remote views.
- Baseline is not filtered directly.

### Local Observation Tests

- Full scan skips ignored directory subtrees without hashing descendants.
- Full scan skips out-of-include subtrees while traversing required include ancestors.
- Watch setup does not add watches under ignored or out-of-include directories.
- Watch event handling drops ignored/out-of-include events before planning wake.
- New-directory scan uses the same filter decisions.
- Single-path observation uses the same filter decisions.
- Ignored paths do not create invalid-name, path-length, file-size, read-denied, hash, or case-collision issues.
- Visible invalid names/path length/file size/case/read-denied/hash issues are reported.
- Managed local issues auto-resolve when absent from the next full observation.
- Path-scoped observation only reconciles the proved path set.
- `.nosync` no longer aborts scan/watch.
- `.odignore` remains an ordinary file unless matched by explicit config.
- SharePoint root-level `Forms` is an issue only for configured SharePoint parent library roots.

### Remote Observation Tests

- Hidden remote deltas still update raw manageable `remote_state`.
- Hidden remote deltas advance the cursor when raw observation succeeds.
- Hidden-to-hidden remote deltas do not wake the planner.
- Visible-to-hidden remote deltas wake the planner because the filtered remote view lost a path.
- Hidden-to-visible remote deltas wake the planner because the filtered remote view gained a path.
- Removing or broadening a remote filter changes the planner-visible remote view without a remote refresh, because `remote_state` retained raw manageable rows.
- Full refresh orphan detection uses raw seen IDs before filtering.
- Ignored existing remote items are not synthesized as deletes.
- Parent materialization failure does not fabricate a root-level path.
- Parent materialization failure prevents cursor advancement for the incomplete batch.
- OneNote/package items are excluded from sync projection.
- Direct `ls` can show OneNote/package items if Graph returns them.
- Personal Vault, root/mount-root, shortcut placeholder, and malformed remote behavior remains structural and remote-only.

### Store Tests

- `CommitObservation` no longer drops product-junk names.
- Store persists the raw manageable remote observation it is given.
- Store removes `remote_state` by item ID only for provider deletes or explicit state repair, not for product visibility changes.
- Filtered planning views are derived without mutating durable state.
- Observation issue reconciliation remains projection-based.

### Executor and Cleanup Tests

- Invalid OneDrive names are not disposable.
- Unreadable children block delete.
- Configured junk can be disposable only when `ignore_junk_files = true`.
- Custom `ignored_paths` matches are not disposable.
- Owned transfer artifacts are cleaned only when ownership is provable.
- Broad user-created `*.partial` files are not deleted as owned artifacts.
- Owned transfer artifacts are not uploaded when junk filtering is disabled.
- Cleanup does not walk excluded, out-of-include, protected, child mount, or symlink-target subtrees.

### Symlink Tests

- Symlinks are skipped by default.
- `follow_symlinks = true` follows file symlinks with cycle protection.
- `follow_symlinks = true` follows directory symlinks with cycle protection.
- Watch setup does not recurse forever through symlink cycles.
- Remote delete of a symlink boundary removes only the symlink.
- Remote delete below a symlinked directory does not delete target content.
- Remote delete below a symlinked directory produces the planned safety issue or blocked condition.
- No-follow filesystem helpers are used for symlink boundary deletes.

### Shortcut Child Tests

- Parent include rules suppress automatic child launch when the shortcut alias is out of scope.
- Parent ignore rules suppress automatic child launch when the shortcut alias is ignored.
- Parent filters below the shortcut alias are translated into child-relative filters.
- Child engines receive the translated filter config.
- Standalone shared-folder mounts use their own config, not inherited parent config.
- Child final drain and cleanup do not mutate parent-owned filter policy.
- Parent protected roots still prevent parent/child overlap.

### Docs and Drift Tests

- Requirements traceability is updated for the new product behavior.
- Design docs no longer claim bidirectional path narrowing is removed.
- Graph docs no longer describe sync-only package filtering as generic Graph normalization.
- `.nosync`, deleted filter names, old marker-file concepts, and stale `.odignore` fixture names are removed or explicitly marked historical.
- Verifier rules and E2E listings no longer reference deleted `.nosync` tests.

## Risks and Side Effects

- Adding bidirectional filters can hide previously synced content. The planner boundary must turn this into baseline cleanup only, not local or remote deletes.
- Filtering deltas before planner wake can accidentally lose wakeups if filtered-view change detection is wrong. Cursor advancement must still be tied to raw observation success.
- Persisting hidden raw remote rows means `remote_state` can contain paths the planner currently ignores. Code and docs must be explicit that `remote_state` is raw manageable provider truth, not the filtered sync set.
- Removing Graph-level package filtering can change direct command output. That is the desired product boundary, but tests and docs must be updated together.
- Changing symlink default from follow to skip changes behavior. No compatibility shim is required, but tests and docs must make the new default explicit.
- Treating unreadable children as blockers can make deletes fail where they previously succeeded. This is safer and intentional.
- Removing broad `*.partial` cleanup can leave old user or legacy temp files behind. Owned artifact cleanup should be explicit rather than destructive.
- SharePoint `Forms` handling can cause false positives if drive type detection is wrong. The issue must only apply when the configured parent drive is known SharePoint.
- Include-only filters for shortcut aliases can prevent child engines from launching. That is correct, but parent/child status should explain why the child is absent or filtered.

## Acceptance Criteria

- All new config keys load, validate, document, and reach sync engines.
- All product filters are evaluated by one shared content filter model.
- Planner comparison uses filtered current local/remote views and raw baseline.
- Adding a filter never produces local or remote delete actions for newly hidden content.
- Removing a local filter re-presents local truth on the next full local observation.
- Removing or broadening a remote filter re-presents remote truth through filtered planner views over raw `remote_state`, not through a filter fingerprint or forced remote refresh.
- Filter config changes restart affected watch runners on reload and always cause a full local observation before planning.
- `.nosync` behavior is gone from code, tests, docs, E2E listings, and verifier references.
- Junk filtering is default-off and bidirectional when enabled.
- Local issues are always projected from latest visible observation state and auto-resolve.
- Remote structural exclusions remain remote-only and do not affect direct file operations unless a direct command owns that limitation.
- Symlinks are skipped by default and safe when explicitly followed.
- Shortcut children receive correct child-relative filters and preserve parent/child ownership.
- Store code no longer owns product filtering.
- Executor cleanup no longer deletes invalid or unreadable user content as disposable.
- Docs, requirements, reference material, and verifier metadata match the final architecture.

## Assumptions

- There is no backwards-compatibility requirement for config format, DB schema, tests, docs, or internal APIs.
- `included_dirs` and `ignored_dirs` are root-relative path lists, not basename-anywhere glob lists, for v1.
- Directory path matching is slash-path, exact-subtree matching.
- `ignored_paths` are glob patterns for both files and directories and may match basename-only or root-relative paths.
- A directory matched by `ignored_paths` excludes the whole subtree.
- Path matching is case-sensitive for v1.
- Remote Graph API narrowing is not part of this increment because whole-root delta remains the reliable remote truth source.
- Early local and remote filtering is valuable for performance and noise reduction, but planner projection is the only correctness boundary.
- Disposable cleanup is bundled only with `ignore_junk_files` and owned transfer artifacts, not arbitrary `ignored_paths` matches.
- Direct file operations remain independent of sync filtering.
- The first implementation should favor clear ownership and correct state transitions over clever delta/API-call reduction.
