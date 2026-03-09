<!-- ABSORBED → spec/design/sync-planning.md -->
# Filtering Conflicts Analysis

> **Decision IDs FC-1 through FC-12.** This document analyzes 12 conflicts,
> asymmetries, and design gaps across the current built-in exclusion system,
> the Phase 10 user-configurable filtering design, and the Phase 10.5 junk
> cleanup design. Each issue includes competitor research, options analysis,
> a recommendation, and test requirements. Cross-referenced from
> [sync-algorithm.md](sync-algorithm.md), [configuration.md](configuration.md),
> and [architecture.md](architecture.md).

---

## Table of Contents

1. [FC-1: Remote observer has no built-in exclusion filtering](#fc-1-remote-observer-has-no-built-in-exclusion-filtering)
2. [FC-2: Built-in `.db` exclusion is too aggressive](#fc-2-built-in-db-exclusion-is-too-aggressive)
3. [FC-3: `desktop.ini` in two exclusion systems](#fc-3-desktopini-in-two-exclusion-systems)
4. [FC-4: Filter in Planner (E20) vs. built-ins in Observer](#fc-4-filter-in-planner-e20-vs-built-ins-in-observer)
5. [FC-5: Built-in exclusions vs. future `skip_files` overlap](#fc-5-built-in-exclusions-vs-future-skip_files-overlap)
6. [FC-6: `.odignore` killed by `skip_dotfiles`](#fc-6-odignore-killed-by-skip_dotfiles)
7. [FC-7: `.odignore` files — sync or not?](#fc-7-odignore-files--sync-or-not)
8. [FC-8: `auto_clean_junk` deletion wars](#fc-8-auto_clean_junk-deletion-wars)
9. [FC-9: Stale files after filter changes](#fc-9-stale-files-after-filter-changes)
10. [FC-10: `sync_paths` parent traversal gaps](#fc-10-sync_paths-parent-traversal-gaps)
11. [FC-11: `.nosync` vs remote observer](#fc-11-nosync-vs-remote-observer)
12. [FC-12: Non-empty directory delete](#fc-12-non-empty-directory-delete)

---

## FC-1: Remote observer has no built-in exclusion filtering

**Severity:** Active bug
**Decision:** Apply `isAlwaysExcluded()` and `isValidOneDriveName()` symmetrically in the remote observer
**Code fix:** Roadmap increment 6.Xa (pre-Phase 10)

### Problem

The local observer applies `isAlwaysExcluded()` and `isValidOneDriveName()` at every filtering point (FullScan, Watch events, `scanNewDirectory`, `addWatches`). The remote observer applies neither — it only filters root items and Personal Vault. A `.tmp`, `.partial`, `.db`, `~$doc.docx`, or invalid-name file uploaded to OneDrive via the web UI or another client flows through the remote observer into the planner, generating a download action. The local observer would then ignore the downloaded file on subsequent scans, creating a ghost entry in baseline with no local counterpart.

This violates the S7 safety invariant ("Never upload partial or temp files") — while S7 prevents local→remote pollution, it does nothing about remote→local pollution.

**Code references:** `scanner.go` (local filtering), `observer_remote.go` (no filtering beyond vault/root).

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | Hardcoded `IsTemporary`/`IsInternal` applied during scan on ALL sides symmetrically |
| **rclone** | Same filter chain applied to both local and remote listings |
| **Dropbox** | Hardcoded exclusion list applied server-side (files rejected on upload AND excluded from download) |
| **abraunegg** | `skip_file` patterns applied during both remote delta processing AND local scanning |

### Recommendation

Apply `isAlwaysExcluded()` and `isValidOneDriveName()` in the remote observer's `classifyItem()`, before emitting any ChangeEvent. This makes the S7 safety invariant truly symmetric. Items failing these checks should be logged at Debug level and silently dropped.

### Test requirements

- **Unit** (`observer_remote_test.go`): Feed delta items with `.tmp`, `.partial`, `~$doc.docx`, `desktop.ini`, `CON` names through remote observer, assert no ChangeEvents emitted
- **Unit** (parameterized symmetry test): Confirm both observers agree on all excluded patterns — same inputs produce same include/exclude decisions
- **E2E** (`e2e_full`): Upload a `.tmp` file via Graph API directly, run sync, assert file is NOT downloaded locally

---

## FC-2: Built-in `.db` exclusion is too aggressive

**Severity:** Active bug (false positives)
**Decision:** Narrow to sync engine database path only
**Code fix:** Roadmap increment 6.Xb (pre-Phase 10)

### Problem

`alwaysExcludedSuffixes` includes `.db`, matching ANY file ending in `.db` via `strings.HasSuffix`. This catches legitimate non-SQLite files (`exports.db`, `contacts.db`, custom data files). The intent is to protect SQLite databases from mid-transaction corruption, but the suffix is too broad. Additionally, `.sqlite`, `.sqlite3`, `.sqlite-wal`, `.sqlite-shm` are NOT excluded — a SQLite database using the `.sqlite` extension with WAL mode would be synced mid-transaction, causing the exact corruption `.db` exclusion aims to prevent.

**Code references:** `scanner.go` `alwaysExcludedSuffixes` slice.

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | No built-in `.db` exclusion. Users add SQLite patterns to `.stignore` if needed. |
| **rclone** | No built-in exclusions at all. |
| **Dropbox** | No `.db` exclusion. Relies on file locking to prevent corruption. |
| **abraunegg** | Default `skip_file` does NOT include `.db` — only `~*\|.~*\|*.tmp\|*.swp\|*.partial`. |
| **Nextcloud/ownCloud** | `sync-exclude.lst` includes `*.db` but users can edit it (with "may render client unusable" warning). |

### Options

**Option A: Narrow to sync engine database only.** Replace `.db`/`.db-wal`/`.db-shm` suffix matching with exact-path matching for the sync engine's own database file. Other `.db` files sync normally. Users who want to exclude their own SQLite databases use `skip_files = ["*.db"]` (Phase 10).

- Pro: No false positives. Matches what Syncthing, rclone, Dropbox, and abraunegg do.
- Con: If a user has a SQLite database that's actively written to, syncing it may corrupt it. But this is true for `.sqlite` files already, so the current protection is inconsistent.

**Option B: Expand to cover all SQLite extensions.** Add `.sqlite`, `.sqlite3`, `.sqlite-wal`, `.sqlite-shm` to `alwaysExcludedSuffixes`.

- Pro: Consistent protection against SQLite corruption regardless of extension.
- Con: Doubles down on the false-positive problem — even more legitimate files silently excluded.

**Option C: Keep `.db` exclusion, add documentation.**

- Pro: No code change, no regression risk.
- Con: Users will be surprised when their `.db` files don't sync. No competitor does this.

### Recommendation

**Option A** — narrow to sync engine database path only. Remove `.db`/`.db-wal`/`.db-shm` from `alwaysExcludedSuffixes`. Add path-based exclusion for the sync engine's own baseline SQLite file. Document in the "built-in exclusions" section that `.db` files are no longer auto-excluded, and recommend `skip_files = ["*.db", "*.sqlite"]` for users who want SQLite protection. This matches industry consensus.

### Test requirements

- **Unit**: Test that `legitimate-data.db` is NOT excluded after the fix
- **Unit**: Test that the sync engine's own database path IS excluded
- **Unit**: Test that `.sqlite`/`.sqlite3` files are NOT excluded (documenting the behavior)
- **E2E** (`e2e_full`): Create a file named `test-data.db` locally, sync, assert it appears on remote

---

## FC-3: `desktop.ini` in two exclusion systems

**Severity:** Documentation/design gap
**Decision:** Document the overlap, remove from Phase 10.5 junk list
**Doc update only** (no code change needed)

### Problem

`desktop.ini` is rejected by `isValidOneDriveName()` as a reserved pattern (`scanner.go:596`) AND listed in the Phase 10.5 junk file cleanup default list. Since `isValidOneDriveName` prevents it from ever being synced in either direction, it can never have a baseline entry. Phase 10.5's `auto_clean_junk` would never encounter it as a sync candidate — the cleanup is unreachable for this file.

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Official OneDrive** | `desktop.ini` is on the hardcoded never-sync list. The official client silently removes cloud copies uploaded by other tools during migration. |
| **Dropbox** | `desktop.ini` is on the hardcoded never-sync list. |
| **abraunegg** | `desktop.ini` is not in default `skip_file` but is rejected by OneDrive API restrictions. |

### Recommendation

Document the overlap explicitly. `desktop.ini` stays in `isValidOneDriveName` (it's a real OneDrive API restriction). Remove it from the Phase 10.5 default junk list (it's redundant). Add a comment in both locations cross-referencing each other. The non-empty directory delete handling (FC-12) covers the case where `desktop.ini` exists locally and blocks folder removal.

---

## FC-4: Filter in Planner (E20) vs. built-ins in Observer

**Severity:** Design gap
**Decision:** Keep the two-tier split, amend E20 to document it explicitly
**Doc update** (E20 amendment, sync-algorithm.md section 6 rewrite)

### Problem

Architecture decision E20 says "The filter runs in the planner, not in the observers." But the current built-in exclusions (`isAlwaysExcluded`, `isValidOneDriveName`) run in the observers. When Phase 10 arrives, there will be filtering in two different pipeline stages:

- **Observer-level**: Built-in exclusions — items never produce ChangeEvents
- **Planner-level**: Config patterns, `sync_paths`, `.odignore` — items produce ChangeEvents but are filtered before action generation

Different code paths for conceptually the same operation (excluding a file from sync).

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | ALL filtering (hardcoded + user patterns) at scan time. Single location. |
| **rclone** | ALL filtering at list time (both local and remote). Single location. |
| **rsync** | ALL filtering at file list generation on sender side. Single location. |
| **abraunegg** | Filtering applied at both remote delta processing AND local scanning, but the same functions called in both places. |

### Options

**Option A: Move built-ins to planner.** Single filtering location.

- Pro: Clean architecture — one place for all filtering.
- Con: Observer produces ChangeEvents for files that will always be filtered, wasting memory/CPU. Watch mode would add watches on excluded directories.

**Option B: Keep split, document the two-tier model.** Built-in exclusions stay in observers (performance), config-based filtering goes in planner (flexibility, hot-reload).

- Pro: Performance — excludes high-volume noise at the earliest point.
- Pro: Practical — nobody needs to hot-reload the built-in exclusion list.
- Con: Two filtering locations to maintain. Conceptual complexity.

**Option C: Duplicate built-ins in both locations.**

- Pro: Defense in depth.
- Con: Redundant work. Three filtering locations to maintain.

### Recommendation

**Option B** — keep the split, update E20 to document the two-tier model explicitly.

**Tier 1 (Observer-level):** Built-in safety exclusions. Applied in both local and remote observers. Hardcoded, not configurable, for safety/correctness (S7, OneDrive API restrictions). Filter at the earliest possible point. Cannot be overridden.

**Tier 2 (Planner-level):** User-configured exclusions (`skip_files`, `skip_dirs`, `skip_dotfiles`, `max_file_size`, `sync_paths`, `.odignore`). Applied symmetrically in the planner. Hot-reloadable. This is where E20 applies.

The four-layer cascade documented in [sync-algorithm.md §6](sync-algorithm.md#6-filtering) adds Layer 0 (observer-level) before the existing three planner layers.

---

## FC-5: Built-in exclusions vs. future `skip_files` overlap

**Severity:** Design gap
**Decision:** Document built-in exclusions as Layer 0, fix examples
**Doc update** (configuration.md, sync-algorithm.md, roadmap.md, prd.md)

### Problem

`isAlwaysExcluded()` hardcodes `.tmp`, `.swp`, `~*`, `.~*`, `.crdownload`, `.partial`. The Phase 10 `skip_files` examples in the PRD and roadmap include `"~*"`, `"*.tmp"`, `"*.partial"` — patterns already always excluded. If a user adds `skip_files = ["*.tmp"]`, removing it later would NOT start syncing `.tmp` files because the built-in list still blocks them. The built-in list is invisible to the user.

Also: `isAlwaysExcluded` uses suffix matching; `skip_files` uses glob patterns. Different matching semantics for the "same kind of thing."

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | Built-in exclusions minimal (only Syncthing's own temp files). All other patterns user-configured. No overlap. |
| **Dropbox** | Hardcoded list documented and non-overridable. Users know what's on it. |
| **abraunegg** | Default `skip_file` is a user-configurable default, not a separate system. Replacing it replaces the defaults. |
| **rclone** | No built-in exclusions. Zero overlap. |

### Recommendation

1. Document the built-in exclusion list explicitly in `configuration.md` as a "Layer 0" that runs before all user-configured filters and cannot be overridden.
2. Remove overlapping examples from PRD/roadmap `skip_files` examples. Replace with patterns NOT in the built-in list: `["*.log", "*.pyc", "*.o"]`.
3. When Phase 10 is implemented, log the effective built-in exclusions at startup so users see what's always excluded.
4. After FC-2 is resolved (narrowing `.db`), review the remaining built-in list for anything that should move to the `skip_files` default example instead.

---

## FC-6: `.odignore` killed by `skip_dotfiles`

**Severity:** Design gap (will bite in Phase 10)
**Decision:** Exempt the configured `ignore_marker` filename from `skip_dotfiles`
**Doc update** + future code fix in Phase 10.0

### Problem

If `skip_dotfiles = true`, then `.odignore` (which starts with `.`) would be excluded by the config pattern layer (Layer 2). Since the cascade is monotonic-exclusion, `.odignore` would be killed before Layer 3 (`.odignore` processing) ever runs. The roadmap Phase 10.0 item 3 says "except `.odignore`" but this exception is not in the formal spec (sync-algorithm.md section 6.1).

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | `.stignore` excluded at the protocol level (hardcoded), never affected by user patterns. |
| **Nextcloud** | Uses `.sync-exclude.lst` (not a dotfile), sidestepping the issue. |
| **abraunegg** | `skip_dotfiles` is a separate boolean check. No per-directory ignore files exist. |

### Recommendation

**Exempt the configured `ignore_marker` filename from `skip_dotfiles`**, matching the standard approach (`.gitignore` is never affected by `.*` entries in `.gitignore` itself). Document the exemption explicitly in `configuration.md` and `sync-algorithm.md`.

### Test requirements (Phase 10.0)

- **Unit**: Filter with `skip_dotfiles=true` does NOT exclude the configured `ignore_marker` filename
- **Unit**: Filter with `skip_dotfiles=true` and custom `ignore_marker=".myignore"` does NOT exclude `.myignore` but DOES exclude `.odignore`

---

## FC-7: `.odignore` files — sync or not?

**Severity:** Design gap
**Decision:** Never sync `.odignore` (Syncthing/Dropbox model)
**Doc update** (sync-algorithm.md, configuration.md) + built-in exclusion addition in Phase 10.1

### Problem

The roadmap says `.odignore` files "are never synced to OneDrive (always excluded)." But what if `.odignore` already exists on the remote (put there by another client, the web UI, or a shared folder)? Should it be downloaded? If yes, ignore rules propagate automatically. If no, users must manually replicate ignore rules on each machine.

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | `.stignore` is NEVER synced. Each device has independent ignore rules. `#include` directive allows referencing synced files for shared patterns. |
| **Dropbox** | `rules.dropboxignore` is local-only, does NOT sync. Must be created on each device. |
| **rsync** | Per-directory `.cvsignore` files ARE transferred and processed on the receiving side. |
| **Nextcloud** | Behavior inconsistent — sometimes synced, sometimes not. |

### Options

**Option A: Never sync.** `.odignore` always excluded from sync in both directions. Per-device ignore rules.

- Pro: No chicken-and-egg problem. Different devices can have different rules.
- Pro: Matches most competitors.
- Con: Users must manually replicate rules on new devices.

**Option B: Sync like any other file.**

- Pro: Set up once, applies everywhere.
- Con: Chicken-and-egg: file needs to sync before rules take effect. Badly-written rules propagate everywhere.

**Option C: Download but never upload (hybrid).**

- Pro: Admin can set rules centrally via web UI.
- Con: Asymmetric, confusing. No competitor does this.

### Recommendation

**Option A** — never sync `.odignore`. Add the configured `ignore_marker` value to the built-in exclusion list (Tier 1) so it's filtered in both observers. Document that ignore rules are per-device.

### Test requirements (Phase 10.1)

- **Unit**: `.odignore` is in the built-in exclusion list and filtered by both observers
- **E2E**: Create `.odignore` locally, sync, assert it does NOT appear on remote
- **E2E**: Upload `.odignore` via Graph API, sync, assert it is NOT downloaded locally

---

## FC-8: `auto_clean_junk` deletion wars

**Severity:** Design gap (Phase 10.5 as designed would cause loops)
**Decision:** Replace "auto-clean" with "built-in junk exclusion"
**Doc update** (roadmap Phase 10.5 rewrite, sync-algorithm.md)

### Problem

Phase 10.5 designs `auto_clean_junk` to "delete matching files from the remote side during upload-direction sync." This creates infinite loops in cross-platform shared folders:

1. macOS creates `.DS_Store` when Finder visits a folder
2. Sync engine deletes it from remote (junk cleanup)
3. macOS recreates `.DS_Store` next time Finder opens the folder
4. Goto 2

The official OneDrive client has this exact problem. Microsoft's workaround is `defaults write com.apple.desktopservices DSDontWriteNetworkStores true`.

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Dropbox** | Hardcoded exclusion — `.DS_Store` never synced. No loop. But never cleaned from remote. |
| **Syncthing** | No auto-cleanup feature. Users handle junk via `.stignore`. |
| **Official OneDrive** | Has the loop problem. Admin can block file types via Group Policy. |
| **rclone** | Batch tool — no loop possible. |

### Options

**Option A: Exclude junk, don't clean it.** Add `.DS_Store`, `Thumbs.db`, etc. to built-in exclusions. Never sync. Never delete remote copies.

- Pro: No deletion wars. Simple.
- Con: Junk uploaded by other tools accumulates on remote.

**Option B: Exclude + one-time CLI cleanup.** Built-in exclusion + `onedrive-go clean-remote-junk` manual command.

- Pro: No loops. User controls when cleanup happens.
- Con: Requires user action.

**Option C: Auto-clean with loop detection.**

- Pro: Cleans junk automatically in the common case.
- Con: Complex. Race conditions. Slow loops may not trigger detection.

### Recommendation

**Option A** — add common junk files to the built-in exclusion list. Do NOT auto-clean. Reclassify Phase 10.5 from "auto-clean junk" to "built-in junk exclusion" (simpler, safer). If users want remote cleanup, provide it as a manual CLI command in a later phase.

**Built-in junk exclusion list:** `.DS_Store`, `Thumbs.db`, `._*` (macOS resource forks), `__MACOSX/` directories. Note: `desktop.ini` is already covered by `isValidOneDriveName` (FC-3).

---

## FC-9: Stale files after filter changes

**Severity:** Design gap (Phase 10)
**Decision:** Warn and freeze (default), with `stale_action = "untrack"` option
**Doc update** (sync-algorithm.md, configuration.md)

### Problem

When a user changes filter rules to exclude a previously-synced file, the file has a baseline entry but is now filtered. The design says "logged as a warning, never auto-deleted." But:

- If the remote copy is later modified, the delta delivers an update. The planner filters it. The local copy silently diverges.
- If the user deletes the local stale file, the planner would normally generate a `RemoteDelete`. But the filter excludes the path. Does the delete propagate or not?
- The local safety scan would also need to be filter-aware.

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | Silently diverges. No warning. File stays on all peers. |
| **rclone** | Left untouched on both sides. No divergence tracking (batch tool). |
| **Dropbox** (xattr ignore) | Remote copy DELETED when xattr set. Local becomes local-only. |
| **abraunegg** | Bug — `skip_dir` on previously-synced directory can still trigger remote deletion. |
| **Official OneDrive** | Selective sync removes local copy, remote stays. No divergence. |

### Recommendation

**Warn and freeze** as the default. When a filter change excludes a baselined file: log a warning with file path, freeze the baseline entry (don't update, don't delete), surface in `status` output. Available as `stale_action = "untrack"` to remove from baseline instead.

**Key decision:** Deleting a locally-stale file should NOT propagate to remote. The filter says "this path is excluded" — no sync operations in either direction, including deletes. This matches Syncthing and rclone behavior and avoids the abraunegg bug.

### Test requirements (Phase 10)

- **Unit**: File has baseline entry, matches new `skip_files` pattern → no actions generated
- **Unit**: Stale file deleted locally → no `RemoteDelete` generated
- **Unit**: Stale file modified remotely → no `Download` generated
- **E2E**: Sync a file, add its name to `skip_files`, modify remote copy, sync again, assert local copy unchanged

---

## FC-10: `sync_paths` parent traversal gaps

**Severity:** Design gap (Phase 10.2)
**Decision:** Sync parent metadata with lightweight baseline entries
**Doc update** (sync-algorithm.md, configuration.md)

### Problem

The design says for `sync_paths`: "Parent directories are traversed but NOT synced themselves." If `sync_paths = ["/Documents/Work"]`, the engine must create `/Documents/` and `/Documents/Work/` locally, but `/Documents` has no baseline entry. If someone renames `/Documents` to `/Docs` on the remote:

- Delta delivers a rename for `/Documents` (an item with no baseline entry)
- The planner can't generate a `LocalMove` because there's no baseline for `/Documents`
- Children under `/Documents/Work` now have a new parent path (`/Docs/Work`) but local still uses `/Documents/Work`

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Dropbox** (Selective Sync) | Tracks folders by path, not ID. Rename creates a "selective sync conflict." |
| **Official OneDrive** (Files On-Demand) | Always syncs the entire folder tree as metadata. No parent-traversal problem. |
| **rclone** | No baseline. Each run traverses fresh. Parent renames just work. |
| **abraunegg** | Auto-creates parent paths. Has bugs with moved directories not being tracked (Issue #3480). |

### Recommendation

**Option A** — sync parent metadata with lightweight baseline entries. The baseline entry for a traversal parent should have a flag (e.g., `sync_paths_traversal = true`) that tells the planner "process renames and deletes for this folder, but don't sync its direct children unless they're within a sync_path." This matches what the official OneDrive client does (syncing the full tree as placeholders).

### Test requirements (Phase 10.2)

- **Unit**: Parent of `sync_path` renamed remotely → `LocalMove` generated for parent, child paths updated
- **Unit**: Parent of `sync_path` deleted remotely → appropriate cascade behavior
- **E2E**: Sync with `sync_paths`, rename parent folder remotely, sync again, assert local structure matches

---

## FC-11: `.nosync` vs remote observer

**Severity:** Low (current root-level implementation is safe)
**Decision:** Document explicitly; add remote-side exclusion requirement for future per-directory `.nosync`
**Doc update** (note in sync-algorithm.md)

### Problem

The `.nosync` guard file halts local scanning and watch setup. But it doesn't affect the remote observer. If a directory has a baseline (was previously synced), remote changes to items in that directory still flow through the delta endpoint. The planner would generate download/update actions for files the local observer is intentionally ignoring.

Current `.nosync` is root-level only (aborts entire sync). But the design mentions per-directory `.nosync` as a future feature, which would create a more severe version of this problem.

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | `.stignore` applied symmetrically. If a folder is ignored, remote changes also ignored. |
| **abraunegg** | No per-directory nosync equivalent. `sync_list` is the partial-sync mechanism. |

### Recommendation

For the current root-level `.nosync`, this is a non-issue — if `.nosync` is present, the engine aborts entirely (both local and remote). Document this explicitly. For future per-directory `.nosync`, the design must include remote-side exclusion — the planner should check `.nosync` status before generating actions for items in nosync-guarded directories.

---

## FC-12: Non-empty directory delete

**Severity:** Active limitation (permanent silent failure)
**Decision:** Two-tier disposable approach
**Code fix:** Roadmap increment 6.Xc (Tier 1 pre-Phase 10; Tier 2 requires Phase 10)

### Problem

When a folder is deleted remotely, the DAG ensures tracked children are deleted first. But untracked files (OS junk, editor temps, files matching filters) remain, making the directory non-empty. The executor's `deleteLocalFolder` refuses to delete non-empty directories — a permanent failure retried every cycle forever.

### Competitor behavior

| Tool | Approach |
|------|----------|
| **Syncthing** | Block + `(?d)` opt-in perishable prefix |
| **rsync** | Block + perishable `p` modifier |
| **rclone** | Force-delete (purge) |
| **Official OneDrive** | Force-delete to Recycle Bin |
| **Google Drive** | Preserve to Lost and Found |
| **Resilio** | Archive then delete |
| **abraunegg** | Silent failure (orphaned directories) |

### Recommendation: Two-tier disposable approach

**Tier 1 — Built-in disposable list (always active, implemented in 6.Xc):**

Files matching any of these are silently removable when blocking a parent directory deletion:

- Files matching `isAlwaysExcluded()` (`.tmp`, `.swp`, `.partial`, `.crdownload`, `~*`, `.~*`)
- OS junk: `.DS_Store`, `Thumbs.db`, `._*` prefix, `__MACOSX/`
- Files matching `isValidOneDriveName() == false` (`desktop.ini`, `.lock`, `~$*`, reserved names)

When these are the only remaining files in a directory being deleted, silently remove them and proceed with the directory deletion.

**Tier 2 — User-configured perishable patterns (Phase 10):**

New config option `perishable_files` per drive. Example: `perishable_files = ["*.pyc", "*.o", "__pycache__/", "node_modules/"]`. Files matching these patterns are removed when blocking a parent directory removal.

**Tier 3 — Unknown files (fail-safe):**

If ANY remaining file matches neither tier 1 nor tier 2, the directory delete FAILS. A `slog.Warn` identifies the blocking files by name. Surfaced via status/health system when available.

### Test requirements

- **Unit**: `deleteLocalFolder` with only `.DS_Store` remaining → success
- **Unit**: `deleteLocalFolder` with only `.tmp` files remaining → success
- **Unit**: `deleteLocalFolder` with unknown file remaining → failure with file name in error
- **Unit**: `deleteLocalFolder` with mix of disposable + unknown → failure
- **Unit**: `deleteLocalFolder` with `desktop.ini` only → success (caught by `!isValidOneDriveName`)
- **E2E** (`e2e_full`): Create folder+file on remote, sync (downloads), create `.DS_Store` locally in that folder, delete folder on remote, sync, assert folder deleted locally

---

## Summary

| ID | Issue | Severity | Resolution | When |
|----|-------|----------|------------|------|
| FC-1 | Remote observer missing built-in filtering | Active bug | Symmetric filtering in `classifyItem()` | 6.Xa |
| FC-2 | `.db` exclusion too aggressive | Active bug | Narrow to sync engine DB path | 6.Xb |
| FC-3 | `desktop.ini` dual-system | Doc gap | Document overlap, remove from 10.5 junk list | Now (doc) |
| FC-4 | E20 vs. observer built-ins | Design gap | Two-tier model, amend E20 | Now (doc) |
| FC-5 | Built-in vs. `skip_files` overlap | Design gap | Document Layer 0, fix examples | Now (doc) |
| FC-6 | `.odignore` killed by `skip_dotfiles` | Design gap | Exempt `ignore_marker` from `skip_dotfiles` | Phase 10.0 |
| FC-7 | `.odignore` sync behavior | Design gap | Never sync (add to built-ins) | Phase 10.1 |
| FC-8 | `auto_clean_junk` deletion wars | Design gap | Exclude junk, don't auto-clean | Phase 10.5 rewrite |
| FC-9 | Stale files after filter changes | Design gap | Warn and freeze, no delete propagation | Phase 10.0 |
| FC-10 | `sync_paths` parent traversal | Design gap | Lightweight baseline entries for parents | Phase 10.2 |
| FC-11 | `.nosync` vs. remote observer | Low | Document; require remote-side exclusion for future per-dir | Doc only |
| FC-12 | Non-empty directory delete | Active limitation | Two-tier disposable + fail-safe | 6.Xc (Tier 1), Phase 10 (Tier 2) |
