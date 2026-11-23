# Track 5 — Spec and doc drift audit of onedrive-go at 26b3bc8b

Read-only audit, 2026-10-31. Repo: `~/src/onedrive-go` at `26b3bc8b`. Nothing was modified; no `go test` was run. Method: every user-facing claim in `README.md`, `docs/*.md`, `CONTRIBUTING.md`, `SECURITY.md`, `contrib/systemd/onedrive-go.service` was checked against the code and against the commit bodies of `f9f0bd18..26b3bc8b` (PRs #802–#825). CLI surface was checked by building the binary and reading every `--help` plus the cobra definitions. Requirement evidence was checked mechanically (`// Validates:` citations vs `[verified]` IDs) and by reading the cited tests for the R-6.2/R-6.4 rows.

**Headline.** The user docs were last touched in PR #800 (2026-10-25) and the "known gaps" they describe were closed by #803–#808, #815, #819 and #822. Today the docs *under*-state safety in several places (they say local deletes are permanent, there is no mass-delete limit, an unmounted sync folder is not detected, renames lose mtime) and *mis*-state defaults in others (`ignore_junk_files` is now on by default with a much longer list; `cross_filesystems` exists and is off, which stops sync descending into a nested mount). The FAQ's "move the sync folder" recipe is now wrong in a way that costs the user their baseline for no reason. Nine user-visible CLI additions (`trash list|restore|empty`, `drive rebind-root`, `sync --confirm-deletes`, `sync --allow-root`, `status --perf`, `perf capture`) are absent from README and docs. Design docs are largely current for #805–#825; the stale spots are `spec/reference/rearchitecture-candidates.md` (every "Landed" paragraph stops at #808), one hypothetical package naming in `system.md`, and the triple-duplicated routing rows in `CLAUDE.md`.

---

## 1. User docs: claim vs truth

### 1a. `docs/safety.md` — "Known gaps" table (lines 159–169), row by row

| Row (docs/safety.md line) | Claim today | Truth at 26b3bc8b | Status |
| --- | --- | --- | --- |
| 161 Unmounted / emptied / replaced sync folder not detected; also a filesystem mounted *inside* the folder | every synced file looks deleted, deletes sent to OneDrive | Sync state is bound to the folder's identity (xattr token `user.onedrive-go.root`, else inode + fsid + birth time); a different folder is **refused** with "run `drive rebind-root`" (`internal/sync/observation_root_identity.go:32`, R-6.2.12, store gen 19/23). A nested mount is a `local_mount_boundary` read boundary unless `cross_filesystems = true`, and a crossed mount later found unmounted is unavailable truth, not deletes (R-6.2.15, `scanner_unavailable.go:208`). Even if both missed, the structural hold catches an emptied side (R-6.2.5). | **Fixed** by #805 (root identity, mount boundary) and #822 (birth time). Residual: a filesystem with neither user xattrs nor birth time, where `rm -rf; mkdir` hands back the same inode (`spec/design/data-model.md:279`) — then the hold is the guard. |
| 162 No limit on number of deletes | large tree deleted on one side is deleted on the other, permanently locally | Delete-safety hold: per direction, deletes are held when the other side is empty while the baseline holds items, or when deletes exceed `max(50, 30 % of baseline items)` (`internal/sync/planner_delete_safety.go:33-35`). Held deletes show in `status` as "Deletes held for confirmation" and in the sync summary ("Held N deletes for confirmation… run 'onedrive-go sync --confirm-deletes'", `internal/cli/sync_console.go:361`); only one-shot `sync --confirm-deletes` releases exactly the recorded set. Local deletes also go to a 30-day trash (below). | **Fixed** by #805 (R-6.2.5, R-6.4.2, R-6.4.3). |
| 163 Unsyncable changes look like deletes (rename to `report?.docx`, replace with symlink/FIFO, read errors other than EACCES) | original deleted on OneDrive | A synced path now occupied by something the scan cannot represent is `local_unsyncable_replacement` (matched by inode), an unreadable entry is a read boundary, a hash error is `hash_error`; all are unavailable truth, never a delete (R-6.2.13, R-6.2.14; `engine_unavailable_not_absent_test.go`). | **Fixed** by #805. |
| 164 `%` sequences decoded (`a%20b.txt` renamed to `a b.txt`) | | Names pass through verbatim; `parentReference.path` is no longer decoded (R-6.7.8, R-6.7.23). Upgrade note in #804: a folder an earlier build showed decoded is renamed back to its real name once. | **Fixed** by #804. |
| 165 Name variants (`straße`/`STRASSE`, NFC/NFD `café`, `a😀.txt`/`a.txt`) not detected as collisions; second file can overwrite first | | `internal/namekey` replays every probed OneDrive equivalence; two local names with one key are a collision whose issue names both files and neither uploads (R-2.12.3). New files are created with `conflictBehavior=fail`, so a same-key sibling can never be replaced (R-5.6.14). Remote twins are `remote_name_twin` and neither syncs (R-2.12.6). | **Fixed** by #804 (collision detection and no-replace create), #815/#823 (NFD spelling on disk). |
| 166 Upload checks incomplete (concurrent remote edit overwritten; upload not compared with what was sent) | | Overwrites carry `If-Match` on the preflight eTag; a remote edit after preflight is a 412 that replans (R-5.6.2, probed on Personal; Business unprobed — on such a drive it would replan, never overwrite). Every upload is verified by QuickXorHash or size; a mismatch is read back once, then fails `upload_verification_failed` and commits no baseline (R-5.5.2). | **Fixed** by #803. Residual honestly stated in R-5.6.2: Business `If-Match`-on-session behaviour unprobed. |
| 167 Download checks incomplete (last attempt accepted after retries; hashless files unchecked; no fsync before rename) | | A download that cannot be verified fails `download_integrity` after 3 attempts and leaves the local file untouched; hashless items are checked by size and an HTML/JSON error body is rejected; only a deterministic server transform (same bytes at the reported size, or the HEIC quirk) is accepted (R-5.5.1, R-5.8.2). Partials are fsynced before the rename and the directory after (R-5.10.1). | **Fixed** by #803. Residual: hashless items are size-checked only (true for Business/SharePoint items without QuickXor). |
| 168 Edits that keep size and mtime are not seen | | The fast path now also requires equal ctime and identity (device, inode, birth time); `cp -p`, `touch -r`, `rsync -t --inplace` move ctime and are re-read (R-2.2.4). | **Mostly fixed** by #808. Residual: an edit that also restores ctime (requires root or clock games) — `spec/design/sync-observation.md:435` records that the rolling re-verify budget (P-12) is not implemented. |
| 169 Two sync clients on one folder not detected | | A second onedrive-go engine *sharing the same data directory* cannot open the same folder (flock on a lock file named after the root's device+inode, R-2.9.5). Another client's marker (`.dropbox`, `.stfolder`, `.nextcloudsync.log`, `.owncloudsync.log`) or a root under `~/Library/CloudStorage` produces a one-time warning (R-2.9.6). | **Partially fixed** by #802. Still true for: a second onedrive-go install with a different data directory or user; another tool that leaves no marker (rclone bisync, abraunegg's `onedrive`). |

### 1b. `docs/safety.md` — other sections

| Section / line | Claim | Truth now | Correction |
| --- | --- | --- | --- |
| 47–49 "Later syncs" table | Deleted/Unchanged → deleted on OneDrive; Unchanged/Deleted → deleted locally after check | Both still subject to the hold (R-6.2.5); local delete goes to the local trash (R-6.4.9); a local folder kept only for ignored content is not re-created online (R-2.17.2, #825) | Add a sentence under the table about the hold and the trash (see 5-11). |
| 53–57 renames/type changes | "otherwise handled as a delete plus a new file" | Still true; identity now includes birth time so a reused inode is never a move (R-2.17.1, #819) | Fine. |
| 71–72 Conflicts | no separate command | Still true | Fine. |
| 83–87 "On your computer, deletes are real deletes… does not move files to a local trash" | | **False.** Every local file sync deletes, and the previous version of every file a download replaces, goes to a per-drive trash under the data directory for 30 days (`internal/localtrash/localtrash.go:44`), listed/restored with `trash list`, `trash restore <id>`, emptied with `trash empty --confirm` (R-6.4.9). | Rewrite (5-3). |
| 79–82 "Only `rm --permanent` and `recycle-bin empty` bypass [the recycle bin]" | | Still true for OneDrive; `trash empty --confirm` is the local equivalent | Mention beside it. |
| 91–94 folder deleted only after contents; "(with `ignore_junk_files = true`…)" | | `ignore_junk_files` is on by default (R-2.4.14, `internal/config/config.go:83-88`) | Reword: "(junk files such as `.DS_Store` do not count unless you set `ignore_junk_files = false`)". |
| 96–106 "What never counts as a delete" | lists interrupted listing, unreadable folder, read-only share, filtered paths | Add: a folder that is a mount point of another filesystem (R-6.2.15); a synced file replaced by something sync cannot represent, a renamed-to-invalid-name item, an unreadable entry (R-6.2.14); an item missing from a full listing is confirmed gone by a lookup by ID before it is deleted (R-6.2.2, #805/#808); a sync folder that is not the one the state was built from (R-6.2.12). | Extend (5-11). |
| 110–114 "Downloads are atomic" | temp name, hash check, rename, resume | Also: partial fsynced before rename, directory fsynced after; the replaced file goes to the local trash; a file whose SQLite `-wal`/`-journal` sibling is non-empty is never replaced (R-2.4.15) | Extend (5-9). |
| 115–116 Uploads resumable | | Also verified by hash/size after upload (R-5.5.2) | Extend (5-9). |
| 126–135 "What is skipped" — "Case collisions. Two local files whose names differ only in letter case" | | Any two names OneDrive treats as one (case, accent composition, emoji/supplementary-plane code points, ZWJ); `status` wording already says so (`status_condition_descriptors.go:317`) | Reword (5-13). |
| 137–142 "One sync at a time" | one `sync` per configuration | Also: one engine per sync folder per data directory (R-2.9.5); `sync` refuses to run as root without `--allow-root` (R-6.3.7) | Add. |
| 149–151 "drive reset-sync-state… next sync behaves like a first sync" | | True. Missing sibling: `drive rebind-root` (keeps state; use after moving/restoring the folder). | Add (5-24). |

### 1c. `docs/comparison.md` (onedrive-go column)

| Line | Claim | Truth | Correction |
| --- | --- | --- | --- |
| 27 Maturity "Pre-release, no releases yet" | | Still true (no tags) | — |
| 45 Mass-delete protection "None yet" | | Hold at `max(50, 30 %)` or an emptied side; `sync --confirm-deletes` (R-6.2.5) | 5-6 |
| 46 Local deletes "Deleted outright, after checking the file is unchanged" | | Kept in a per-drive local trash for 30 days (`trash list/restore/empty`) | 5-6 |
| 47 Filtering "Per drive: included and ignored folders, glob patterns, dotfiles, junk files" | | Still true; junk now default-on | — |
| 56 Machine-readable output | | Still true | — |
| 63–65 "abraunegg… with national-cloud support, a local trash for remote-caused deletes and a big-delete safeguard" | implies onedrive-go lacks all three | onedrive-go now has the latter two | 5-6 |
| 9–16 Sources and date | "Checked on 2026-10-25 … onedrive-go: this repository's main branch" | onedrive-go column is from 2026-10-25 | Update the date when corrected. |

### 1d. `docs/configuration.md` vs `internal/config`

Global keys (`defaults.go`, `validate.go`): `transfer_workers` 8 / 4–64 ✔, `check_workers` 4 / 1–16 ✔, `min_free_space` "1GB", `""`/`"0"` disables ✔, `poll_interval` "5m" / ≥30s ✔, `websocket` false ✔, `dry_run` false ✔, `log_level` "info" / debug,info,warn,error ✔ (NOTICE is a console level only, not a `log_level` value — docs correct), `log_file` "" ✔, `log_format` "auto"/auto,text,json ✔, `log_retention_days` 30 / ≥1 ✔. Size units and Go durations ✔. `login` writes all ten as commented defaults (`write.go:46-61`) ✔. Resolution order and selector rules ✔. Overlap/duplicate `sync_dir` is a config error ✔ (`validate_drive.go`). Unknown keys with "did you mean" ✔; delete thresholds are rejected as unknown (R-6.4.3).

Drive keys:

| Key | Docs line | Docs say | Code |
| --- | --- | --- | --- |
| `ignore_junk_files` | 105 | default `false`; list `.DS_Store`, `Thumbs.db`, `__MACOSX`, `.tmp/.swp/.partial/.crdownload`, names starting `~`, `.~`, `._` "(but not Office's `~$` lock files)" | **default `true`** (`config.go:83-88`, `JunkFilesIgnored()`); list per R-2.4.14 also includes `.Spotlight-V100`, `.Trashes`, `.fseventsd`, `.TemporaryItems`, `.Trash-*`, `$RECYCLE.BIN`, `Icon\r`, `*~`, `~*` **including `~$*`**, `*.swo`, `*.swx`, `4913`, `#*#`, `.#*`, `*___jb_tmp___`, `*___jb_old___`, and SQLite `*-wal`/`*-shm`/`*-journal` sidecars of a database whose name has an extension; `4913`, `~*`, `*~` apply to files only (`content_filter.go:230-289`). `spec/design/config.md:229` is already correct. |
| `cross_filesystems` | — | **missing** | exists, default `false`; a mount point inside the sync folder is not crossed and is reported as `local_mount_boundary` (R-6.2.15, `config.go:90-94`, `spec/design/config.md:231`) |
| `follow_symlinks` | 106 | ✔ | ✔ |
| `sync_dir` | 100 | absolute or `~/`; missing folder → drive unavailable | ✔, plus: the folder must be the one the state was built from, otherwise refused with `drive rebind-root` (R-6.2.12) |
| others | | ✔ | ✔ |

Section "Changing the configuration": reload on disk change and SIGHUP ✔ (poll every 5 s, `watch_config_file.go:16`; a new version is applied only after two identical reads, #813 — "within a few seconds" still accurate).

### 1e. `docs/running-as-a-service.md` and `contrib/systemd/onedrive-go.service`

All checked claims hold: `Type=notify`, `READY=1`/`STATUS=`/`RELOADING=1`/`STOPPING=1`, watchdog at half `WATCHDOG_USEC` (`sdnotify.go:29`), exit 77/78 semantics (`failure_class.go:17-26`), 130 only for non-watch commands, clean stop 0, no `network-online.target`, one sync per configuration. **Missing:** `sync` refuses to run as root (effective UID 0) unless `--allow-root` is given, as a usage error, exit 2 (R-6.3.7). A *system* unit without `User=` will therefore fail immediately and, since exit 2 is not in `RestartPreventExitStatus`, be restarted every 30 s. The docs only show a user unit, so this is a note, not a bug in the shipped unit (5-25). Also worth a line: a held delete set is reported in the journal and in `status`, and `sync --confirm-deletes` must be run one-shot (it is mutually exclusive with `--watch`, `sync.go:405`) — the service does not need to be stopped first, but the one-shot run will refuse while the watch owns the lock, so the operator must `systemctl --user stop` first.

### 1f. `docs/troubleshooting.md`

| Line | Claim | Truth / gap |
| --- | --- | --- |
| 25–43 status table | 18 rows | Missing rows for conditions added since #802: "Deletes held for confirmation" (`deletes_held`, action `sync --confirm-deletes`), `local_mount_boundary` (disk inside the sync folder not mounted / `cross_filesystems`), `local_unsyncable_replacement`, `local_interrupted_removal` (a `.onedrive-go.*.detach` file left by an interrupted removal: never deleted, reported), "Two OneDrive items, one name here" (`remote_name_twin`), `local_database_open` (open SQLite DB not replaced), "Blocked as malware", `unsupported_operation`, `local_read_only`, `local_file_too_large`, `local_cross_device`, `local_busy`, `download_integrity`, `upload_verification_failed`, `remote_folder_delete_unverifiable`, `transfer_url_rejected`, name collision on create (R-5.6.15). |
| 38 "Case collision — Two local names differ only in letter case" | | Reason text in code: "differ only in letter case, in how accents are composed, or in emoji" |
| 77–83 "The sync folder is missing" | | Add a sibling section "The sync folder is not the one that was synced before": message says the folder lacks or carries a different `user.onedrive-go.root` marker and names `drive rebind-root`; show its prompt ("Sync state … remembers N files; … holds M files now. Type REBIND"). |
| 85–89 "another onedrive-go sync is already running" | | ✔; also a per-folder lock: a second engine sharing the data dir fails even via a different path/symlink (R-2.9.5). |
| 104–111 remote changes slow | | ✔ |
| 113–126 "Too many folders to watch" | "when it is reached, new folders are noticed only by the periodic rescan (every 5 minutes)" | Now: at startup or when a watcher cannot be created, watch mode falls back to full local scans (one at once, then at the safety-scan interval), warns once naming the limit and the sysctl, and retries establishing the watcher after 10 min doubling to 1 h; a limit hit for a *new* folder after startup leaves that folder to the safety scan (R-2.8.12). kqueue: descriptor budget is projected and falls back when it would exceed half of `ulimit -n` (R-2.8.18). On network/FUSE roots the rescan runs every 2 min (R-2.8.19). |
| 128–132 network | "can't reach OneDrive, retrying" | Since #817 the recovery is also printed ("all-clear" at NOTICE) and warnings carry `[HH:MM:SS]` in a terminal watch. ✔ |

### 1g. `docs/faq.md`

| Line | Claim | Truth | Severity |
| --- | --- | --- | --- |
| 149–155 alongside another client: "onedrive-go does not detect this" | | Partially detects: same-data-dir second engine is refused (R-2.9.5); known markers warn (R-2.9.6). Still undetected: second install with another data dir/user, markerless tools. | medium |
| 174–187 "How do I move my sync folder": "onedrive-go would see every synced file as deleted and delete it on OneDrive"; recipe = stop, `mv`, edit `sync_dir`, **`drive reset-sync-state`**, dry-run, sync | | **Wrong and wasteful.** (a) An empty new folder is refused by the root-identity check (R-6.2.12), and the structural hold would hold the deletes anyway. (b) After `mv` on the same filesystem the folder keeps its xattr token and sync simply continues; across filesystems (or without xattrs) sync refuses and names `drive rebind-root`, which keeps the whole baseline. `reset-sync-state` throws the baseline away and forces a merge-style first sync (every differing file becomes a conflict copy). | **high** |
| 157–163 first sync deletes nothing | | ✔ (also dry-run) | — |
| 203–206 `rm` not permanent | | ✔ | — |
| 232–237 how quickly | "full rescan every 5 minutes" | 2 minutes on network/FUSE roots (R-2.8.19); fine as default | low |
| 249–254 where data goes | | ✔ | — |

### 1h. `docs/limitations.md`

| Line | Claim | Truth | Severity |
| --- | --- | --- | --- |
| 3–5 "Data-safety gaps that are being fixed are listed separately in safety.md#known-gaps" | | the list is stale (1a) | medium |
| 28–29 "Performance has not been benchmarked, so no speed or memory claims are made" | | Still no end-to-end claim, but #812 added a repo benchmark of the comparison SQL: a quiet pass over 100k items spends ~23 s in the two planning queries on a 2-core host (`spec/design/performance-benchmarking.md`). That is a production-relevant number worth stating as a known cost. | low |
| 37–39 "Local deletes do not use a trash folder… deleted outright" | | **False** since #805 (R-6.4.9) | **high** |
| 58–60 letter case | | OK, but the equivalence is wider (see 1b) | low |
| 64–67 "Emoji in names… are the same name there. Keep such files in different folders (see safety known gaps)" | | Still the same name on OneDrive, but onedrive-go now detects the pair as a collision and uploads neither until one is renamed (R-2.12.3); nothing is overwritten. | medium |
| 75–79 "A rename resets the modification time on OneDrive… onedrive-go does not yet preserve the original time when it renames" | | **Fixed** in #807: sync moves and `mv` carry `fileSystemInfo.lastModifiedDateTime` (R-2.17.5, R-1.7.4). A rename made *on the web* still resets it (OneDrive behaviour); onedrive-go then records the new remote time without re-transferring. | medium |
| 82–91 extra versions, empty-file time | | ✔ (R-5.3.1, R-5.6.5) | — |
| 96–98 shared folders from other orgs | | ✔ | — |

### 1i. `docs/scripting.md`

| Line | Claim | Truth |
| --- | --- | --- |
| 114–125 `--json` contract; mutating commands list | | ✔; the list could add `trash list/restore/empty` and `drive rebind-root` (`cli.md:330-333` documents their shapes). |
| 160–175 `sync --json` events and `action` values | | ✔ (`sync_console_json.go:41-58`). Held deletes are *not* in the `summary` object (no `held` field); scripts must read `status --json` issue type `deletes_held`. Worth one sentence. |
| 183–190 exit codes | | ✔ (`failure_class.go`). Add that `sync` as root without `--allow-root` is a usage error (2). |
| 198–205 env vars | | ✔ |

### 1j. `README.md`

| Line | Claim | Truth |
| --- | --- | --- |
| 11–16 Status warning: "Data-safety hardening is in progress (see known gaps)" | | The hardening program (#802–#825) has landed; the remaining gaps are the residuals in 1a. The verifier (`internal/devtool/verify_readme.go`) scans only the `## Status` section for stale roadmap phrases, so a rewrite must avoid those phrases (check `staleReadmeStatusPhrases()` before editing). |
| 18–22 "File commands, two-way sync, drive management, configuration and transfers are implemented and covered by tests; some status and retry details are still being finished, and performance has not been measured yet" | | Matches `spec/requirements/index.md`. OK. |
| 74 Go 1.26 | | `go.mod` `go 1.26.0` ✔ |
| 131–150 "How your files are kept safe": "Deletes on your computer are real deletes, so the recycle bin is where a mistake is undone"; "There is no limit on how many deletes one sync propagates, and an unmounted or emptied sync folder is not yet detected" | | **Both false** (R-6.4.9, R-6.2.5, R-6.2.12). |
| 154–166 Commands table | | Missing: `trash list / restore / empty`; `drive rebind-root`; `sync --confirm-deletes`, `--allow-root`; `status --perf`; `perf capture`; `rm` needs `-r` for folders; `drive list --all`. |
| 168–169 global flags | | ✔ (`root.go:487-495`) |
| 230–239 exit codes | | ✔ |
| 243–252 Limitations | | ✔ |

### 1k. `CONTRIBUTING.md`, `SECURITY.md`

`CONTRIBUTING.md`: all commands and stage names check out (`ci.yml` jobs `verify-matrix`, `verify`, `stress`, `integration`, e2e). `SECURITY.md`: token files `token_<type>_<email>.json` mode 0600 (`token_resolution.go:34`, `tokenfile.go`), scopes `offline_access Files.ReadWrite.All User.Read` (`graph/auth.go:14-16`), control socket 0600 in a 0700 dir with the short hashed `$TMPDIR/odgo-<hash>/` fallback (`multisync/control_socket.go:25-30`, `config/paths.go:149`) — all ✔. One addition worth making: the local trash under the data directory now holds copies of deleted/replaced user files for 30 days (same 0700 data dir), which belongs under "What onedrive-go stores".

---

## 2. CLI surface vs docs

Registered (from `root.go:addRootSubcommands` and `--help`): `login [--browser]`, `logout [--purge]`, `status [--perf]`, `perf capture [--duration --output --trace --full-detail]`, `shared`, `drive list [--all] | add [id|url|name] | remove [--purge] | rebind-root [--yes] | reset-sync-state [--yes] | search <term>`, `ls`, `get`, `put`, `rm [-r|--recursive] [--permanent]`, `mkdir`, `stat`, `sync [--download-only --upload-only --dry-run --watch --full --confirm-deletes --allow-root]`, `pause [duration]`, `resume`, `mv [-f]`, `cp [-f]`, `recycle-bin list | restore <id> | empty [--confirm]`, `trash list | restore <entry-id> | empty [--confirm]`, cobra `completion`. Global: `--config --account --drive(repeatable) --json -v/--verbose --debug -q/--quiet --version`; `--verbose/--debug/--quiet` mutually exclusive.

| Finding | Where |
| --- | --- |
| Undocumented in README/docs: `trash` (all three), `drive rebind-root`, `sync --confirm-deletes`, `sync --allow-root`, `status --perf`, `perf capture`, `drive list --all`, `rm -r`, `drive reset-sync-state --yes`, `recycle-bin empty --confirm`, `mv/cp --force` (docs mention `--force` only in passing in spec) | README §Commands, docs/*.md |
| Documented but nonexistent | none found |
| Help text drift: `sync --help` Long says "Run a one-shot sync run between…" (doubled word) and never mentions `--watch`, `--full`, `--confirm-deletes`, `--allow-root`, which appear only in the flag list | `internal/cli/sync.go:383-386` |
| `drive` Short/Long "Manage drives (list, add, remove, search)" omit `rebind-root` and `reset-sync-state` | `internal/cli/drive.go:11-15` |
| `get`/`put` have no Long help at all (a one-line Short) | `get.go`, `put.go` |
| `onedrive-go --help` lists `completion` (cobra default); harmless, undocumented | — |

---

## 3. Requirements status

### 3a. Safety rows R-6.2 and R-6.4 — evidence read in detail

| Req | Status | Evidence actually asserting it | Verdict |
| --- | --- | --- | --- |
| R-6.2.1 (S1) | verified | `TestPlannerPlanCurrentState_LocalAbsenceDeletesRemoteOnlyWithBaseline`, `sqlite_compare_test.go:442` | sound. **Mis-citation:** `internal/devtool/verify_runner_test.go:42,145` also cite R-6.2.1/R-6.2.2 for `TestRunVerifyDefaultRunsExpectedSteps` — a devtool step-list test that asserts nothing about deletes (probably meant R-6.10.1). |
| R-6.2.2 (S2) | verified | `engine_absence_confirmed_test.go` (6 tests: converter-skipped items, unlisted parent aborts listing, live orphan not deleted, 410 flavours), `TestRunOnce_EmptiedMountPointPlansNoRemoteDeletes` | sound |
| R-6.2.4 (S4) | verified | `executor_delete_unhashed_test.go` (no-hash + changed → preserved; no-hash + unchanged → deleted; no evidence → preserved), `TestExecutor_LocalDelete_HashMatch/HashMismatch*`, `synctree/detach_test.go` (verify-then-act on the same inode) | sound. **Mis-citations:** `internal/cli/side_effect_contract_test.go:21,37,63` (status/ls/rm do not mutate managed state) and `executor_conflict_reservation_test.go:6,35,55` (conflict-copy name reservation) cite R-6.2.4 but assert nothing about delete verification. |
| R-6.2.5 (S5) | verified | `engine_delete_safety_test.go` (mass remote held, mass local after anomalous refresh held, below threshold proceeds, moves ignored, emptied side holds few, confirm keeps later deletes), `planner_delete_safety_test.go` (folders count, lost+found, dependents held, confirm releases exactly the held set, held row without item ID), `cli/sync_delete_safety_test.go` (flag one-shot only, summary text, status descriptor) | sound; thresholds `50`/`30 %` are asserted via scenario sizes. No live E2E exercises the hold (e2e grep: none) — acceptable, deterministic. |
| R-6.2.11 (S9) | verified | design row `sync-planning.md:74` names `TestPlanCurrentState_HashlessSameSizeIsNotTreatedAsEqual` and siblings, but **no test carries `Validates: R-6.2.11`** (they cite R-2.2.7) | evidence exists; traceability comment missing |
| R-6.2.12 (S10) | verified | `engine_root_identity_test.go` (15 citations: emptied mount point plans no deletes, replaced folder refused, token decides, rebind flag consumed, birth time), `synctree/root_identity_test.go`, `cli/drive_rebind_root_test.go` | sound |
| R-6.2.13 (S11) | verified | `TestFullScan_RootReadDirEIOReturnsError`, `TestScopePathMatches_RootBoundaryCoversEverything`, `TestRootDeviceForScan_UnreadableRootAbortsScan` | sound |
| R-6.2.14/15 (S12/S13) | verified | `engine_unavailable_not_absent_test.go` (11 citations) | sound |
| R-6.2.7 (S7) | verified | **no `Validates:` citation anywhere** | evidence must be located or the status demoted; `TestContentFilter_SpellingRenameArtifactIsAlwaysExcluded` and the partial-name tests exist but are not cited for it |
| R-6.4.1 | verified | **no `Validates:` citation**; semantically covered by the R-6.2.4 tests above | add citations or merge into R-6.2.4 |
| R-6.4.2 | verified | cited by the hold tests ("released only by explicit confirmation, never by timer") — `TestDeleteSafetyHold_ConfirmReleasesExactlyTheHeldSet`, `TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked` | sound; no test asserts "never by a timer/retry" directly, but nothing in the planner has a timer path |
| R-6.4.3 | verified | `TestLoad_UnknownKey_DeleteSafetyThresholdsAreNotConfiguration` | sound |
| R-6.4.9 | verified | `localtrash_test.go` (10), `executor_local_trash_test.go`, `cli/trash_test.go`, `localpath/rename_noreplace_test.go` | sound |
| R-6.2 and R-6.4 **group headers** | `[implemented]` | every child is `[verified]` | promote headers to `[verified]` |

### 3b. `[verified]` requirements that no test cites via `Validates:` (mechanical; the verifier accepts a design-doc `Implements:` of the group instead, which is why these pass `verify public`)

R-1.2.1 R-1.2.2 R-1.2.3 R-1.3.1 R-1.3.2 R-1.3.3 R-1.3.6 R-1.4.1 R-1.4.2 R-1.5.3 R-1.6.1 R-1.7.2 R-1.9.1 R-1.9.2 R-1.9.3 R-1.9.5 R-2.1.5 R-2.1.7 R-2.2.1 R-2.4.4 R-2.4.9 R-2.5.3 R-2.5.4 R-2.6.2 R-2.7.1 R-2.10.6 R-2.10.13 R-2.10.18 R-2.10.20–25 R-2.10.27 R-2.10.30 R-2.10.31 R-2.10.35–40 R-2.14.5 R-2.16.3 R-3.1.2 R-3.1.7 R-3.1.8 R-3.4.2 R-3.5.2 R-3.5.3 R-4.2.3 R-5.1.1 R-5.1.2 R-6.1.4 R-6.1.5 R-6.2.7 R-6.2.11 R-6.3.1 R-6.4.1 R-6.6.9 R-6.6.10 R-6.6.12 R-6.7.4 R-6.7.7 R-6.7.18 R-6.7.21 R-6.7.25 R-6.7.27 R-6.8.13 R-6.9.1 R-6.10.3 R-6.10.6 R-6.10.9 R-6.10.19.

Spot-checks of 20 of them: R-2.1.5 (dry-run) and R-2.1.7 (follow-up passes) are exercised by `engine_one_shot_quiescence_test.go`, `engine_move_*_test.go` and `multisync/orchestrator_test.go` (grep hits for the bare IDs) — only the `Validates:` prefix is missing. R-6.1.4 "startup under 1 s" and R-6.1.5 "binary under 20 MB" are `[verified]` with **no test, no verifier rule and no benchmark artifact** (only prose in `performance-benchmarking.md`); by the index's own promotion rule they should be `[target]` or `[implemented]`. R-5.1.1/R-5.1.2 (worker pool sizes) are checked by config validation tests citing R-4.8.2 only. R-2.2.1, R-3.1.2 (browser login), R-3.4.2, R-6.3.1 are covered by tests citing neighbouring IDs. The R-2.10.18–40 cluster (per-drive scope isolation) relies on `sync-execution.md`/`sync-store.md` group rows. None of the 20 looked false; all are traceability gaps.

### 3c. `[implemented]`/`[designed]` items that look complete (promotion candidates)

- Group headers `R-6.2`, `R-6.4`, `R-3.3` are `[implemented]` while all children are `[verified]`.
- `[designed]` with substantial citing tests: R-2.10.4 (9 tests), R-2.10.5 (13), R-2.10.33 (17), R-2.10.45 (7), R-2.14.3 (5), R-2.5.1 (3), R-2.15.1 (3), R-2.10.32 (1), R-6.8.11 (1) — promote after confirming each cited test asserts the stated behaviour.
- `[designed]` with zero citing tests but code that plainly exists: R-2.3.7 (status sampling 10/5 — `examples_limit` is in `status --json`), R-2.10.14 (60 s trial interval), R-2.10.41, R-6.5.3, R-6.8.7, R-6.8.10, R-6.8.15 (`retry_work`/`block_scopes` machinery) — these need a `Validates:` test each before promotion.

### 3d. `[planned]`/`[future]` that matter for production use

- R-6.1.6–R-6.1.10 and R-6.10.15 (performance validation): the only measured number is the #812 comparison-SQL benchmark (quiet pass at 100k items ≈ 23 s of planning per pass; a 5k-item folder rename ≈ 2.8 s at 10k items). No memory/CPU/idle-watch numbers exist for R-6.1.1–R-6.1.3.
- `spec/design/sync-observation.md:435`: rolling re-verify budget (P-12) not implemented — the only remaining way a content change can be missed is an edit that restores mtime *and* ctime.
- `spec/design/sync-observation.md:645`: a synced item moved *into* an excluded folder is still a delete on OneDrive (recycle bin). This is a data-handling behaviour users will hit (e.g. moving a file into `node_modules` or a dotfolder under `ignore_dotfiles`) and is documented nowhere user-facing.
- R-4.6 `service install`, R-3.8 migration: `future`, correctly stated in docs.

---

## 4. Design-doc drift for #805–#825 and ghost architecture

| PR | Behaviour | Design doc coverage | Verdict |
| --- | --- | --- | --- |
| #805 root identity | xattr `user.onedrive-go.root`, inode+fsid fallback, refusal wording, `rebind-root` | `data-model.md:268-281`, `sync-observation.md:67`, `sync-store.md:179`, `cli.md:41` | current; `data-model.md:279` "Remaining gap… same inode on the same filesystem" should add "and no recorded birth time" after #822 |
| #805 delete hold | thresholds, structural rule, `lost+found`, confirm-exact-set | `sync-planning.md:540-572` (states `max(50, 30 %)`, `held_deletes`, `deletes_held`, one-shot only) | current |
| #805 trash | 30-day retention, pending/committed/orphan, purge policy, hourly tick | `sync-execution.md:515-538`, `cli.md:40,331-333` | current |
| #805/#808 confirmed absence | GetItem confirmation of orphans, 410 flavours, loop guard | `sync-engine.md:450-500`, `sync-observation.md:115` | current |
| #806 watch degradation | fallback, recovery rescan, kqueue budget, network-FS cadence | R-2.8.12–20 in `sync.md`; `sync-observation.md` | current (user docs stale, see 1f) |
| #807 rename keeps mtime | `MoveOptions{KeepModified}` | `graph-client.md:301-303` | current |
| #810–#812 move composition, chaos/model/shims, crash points, dispatch seam | | `sync-planning.md`, `sync-engine.md`, `system.md`, `performance-benchmarking.md` | current |
| #813 settled reload | two identical reads | `sync-control-plane.md`, R-4.4.3 | current |
| #815/#823 NFD spelling | `Root.Spelling`, `shortcutRootOnDisk`, `onDiskSiblingName` | `sync-execution.md:450+`, `sync-observation.md` | current |
| #816 follow-up passes | up to four passes | `sync-planning.md`, `sync-engine.md`, R-2.1.7 | current |
| #817 NOTICE | `internal/loglevel`, journald 5 | `cli.md:209-211` | current |
| #818 placeholder by ID + emptiness | | `drive-transfers.md:261-276`, `sync-store.md`, R-5.6.15 | current |
| #819/#822 birth time | `statx STATX_BTIME`, `SameIdentity`, root birth time | `sync-observation.md:510-523`, `data-model.md`, `system.md` | current |
| #820 replay-safe create | `retry.WithReplaySafe` | `retry.md`, `graph-client.md`, R-6.8.21 | current |
| #824 move records ctime | | `sync-execution.md`, `data-model.md` | current |
| #825 kept_local_folders | gen 24 | `data-model.md`, `sync-execution.md`, `sync-store.md` | current |

Ghost-architecture sweep (deleted packages `internal/{clishape,graphhttp,reviewgate,syncexec,syncobserve,syncplan,syncscope,syncstore,synctypes,trustedpath}`, `cmd/review-gate`, concepts `sync_paths`, `ignore_marker`, `classify_as_big_delete`, `BACKLOG.md`):

- No live reference to any deleted package in code, specs or docs. `SyncStore` the *type* is current.
- `spec/design/system.md:448` uses `syncdomain` / `syncstore` / `syncplanner` as names for a hypothetical split. They coincide with deleted package names; the paragraph should say "hypothetical names" so a reader does not take them as existing or former packages (low).
- `spec/reference/live-incidents.md:35,695,719` mention `sync_paths`/`ignore_marker` as retired config — labelled historical; fine.
- `docs/comparison.md:45` and `output-journeys-vs-abraunegg.md:357` mention `classify_as_big_delete` as abraunegg's option — correct usage.
- `internal/devtool/verify_backlog_ids.go:17` and `retired-backlog-ids.md` mention `BACKLOG.md` as dissolved — labelled historical; fine.
- Design docs cite eleven test *files* that do not exist (not caught by the verifier, which checks `TestXxx` names, not file paths): `drive-transfers.md` → `internal/cli/sync_helpers_test.go`, `internal/driveops/download_test.go`, `internal/driveops/upload_test.go`, `internal/graph/upload_session_test.go`; `graph-client.md` → `internal/graph/auth_browser_test.go`, `auth_device_test.go`, `drives_shared_test.go`, `errors_test.go`, `upload_session_test.go`; `retry.md` → `internal/retry/throttle_gate_test.go`; `sync-planning.md` → `internal/sync/planner_dependency_test.go` (low).
- `spec/design/threat-model.md` (R-6.10.7) does not list the sync-root identity binding, the delete hold or the local trash among mitigations for "unmounted volume / replaced folder reads as mass delete"; its evidence rows predate #805 (low).

---

## 5. `CLAUDE.md`

| Line(s) | Problem |
| --- | --- |
| 29, 30, 31 | Row `internal/sync/shortcut*.go, internal/sync/protected_roots.go → sync-shortcut.md` appears **three times verbatim**. Keep one. |
| 28 vs 29 | `internal/sync/protected_roots.go` is listed in the sync-engine row (28) *and* the sync-shortcut row (29); `GOVERNS:` puts it in `sync-shortcut.md`. Remove it from row 28. Row 28 also lists `shortcut_topology.go`, `shortcut_root_lifecycle.go`, `shortcut_root_planner*.go`, `shortcut_root_transition.go`, `shortcut_root_publication.go`, `shortcut_root_status*.go`, `shortcut_alias_mutation.go`, all of which match row 29's `shortcut*.go`; two rows claim them with different "read first" docs. |
| 25 vs 33 | `internal/sync/scope_key.go` and `scope_semantics.go` appear in both the sync-domain row and the sync-store row. |
| 13 vs 14 | `internal/cli/drive_reset_sync_state.go → cli.md` vs `internal/cli/drive*.go → drive-identity.md`; `GOVERNS` has `drive_*.go` under drive-identity, so row 13 contradicts the verifier's owner (verifier resolves by specificity, but the table tells a human the opposite doc). |
| 34 | The long cli row does not list `trash.go`, `drive_rebind_root.go`, `sync_dir.go`, `journal.go`, `console_log*.go`, `control_client.go`, `error_presentation.go`, `json_output.go`, `replace_destination.go`, `sync_console*.go`, `sync_startup_messages.go`, `account_lifecycle.go`, `drive_catalog.go` etc. (all governed by `cli.md`/`drive-identity.md`). Not required to be exhaustive, but `trash.go` and `drive_rebind_root.go` are new user-facing safety surfaces. |
| 9–34 | No routing rows for `internal/localtrash/`, `internal/localpath/`, `internal/fsroot/`, `internal/namekey/`, `internal/sharedref/`, `internal/authstate/`, `internal/synctest/` (all `GOVERNS` by `system.md`), `internal/loglevel/` (`cli.md`), `internal/synccontrol/` (`sync-control-plane.md`; row 32 lists only `multisync` and `cli/sync.go`). |
| 224 | Fast-E2E test list is **correct** (all four exist under the `e2e` tag; 11 fast tests in all). |
| 3 | "Fast, safe OneDrive CLI" — fine. No instruction contradicts the current layout otherwise. |

---

## 6. `spec/reference/rearchitecture-candidates.md` ("updated 2026-10-28 after PRs #799-#808")

| # | Candidate | What landed after it was written (#809–#825) | "Landed" paragraph current? |
| --- | --- | --- | --- |
| 1 | Identity-keyed reconciliation | #810 composes moves per item by (parent item, name) instead of path (`planner_move_composition.go`), #819/#822 add birth time to local identity — the "generation guard" the proposal asks for — and #811/#812 give the model test deterministic replay. "Remaining non-converging seeds" are now 0/80 (#810) and 500/500 (#812 planner model). | **Stale**: mentions only #808. |
| 2 | Name model | #815 `synctree.Root.Spelling`/`SpellingOfParent`, #823 collision peers and shortcut aliases by spelling on disk — i.e. "D-025 (NFD names on byte-exact Linux filesystems)" is closed. | **Stale**: "Still open: D-025" is wrong. D-023 and the no-lint point remain. |
| 3 | Positive-evidence deletion | #811 synced-folder tombstone pulls the full refresh forward; #812 undecided orphans back off and apply to mount roots; #822 root birth time; #825 kept-local-folder record. "Moving a synced item into an excluded folder still deletes it online" is **still true** (`sync-observation.md:645`). | Partly stale (misses #811/#812/#822/#825); "still open" lines correct. |
| 4 | Name-free Graph addressing | #820 replay-safe creates. No structural change. | Current. |
| 5 | Intents and receipts | #818 placeholder recognised by ID + emptiness (not creation eTag), #824 move outcome re-reads ctime at the destination. "The move reply's echo is not checked" — still true (no `echo` check in `executor*.go`). | Mostly current; add #818/#824. |
| 6 | Journaled executor with crash points | #811 **landed the crash-point harness** (`internal/sync/crash_points.go`, D-091: six named boundaries, test hook, recovery must converge). The proposal's "`ONEDRIVE_GO_CRASHPOINT=<name>` injector" exists as an in-process test hook rather than an env var. | **Stale**: "Landed" lists only the pending-create record. |
| 7 | Deterministic simulation | #811 dispatch-order seam + serialized worker (D-090), virtual-clock harness (D-095); #812 chaos Graph (D-092), filesystem shims incl. case-insensitive volume and errno injection (D-094), planner-level model test and golden matrix (D-093). | **Stale**: "Its limits are non-deterministic dispatch (D-090) and no virtual clock (D-095)" — both closed. Remaining: time.Now/os.* sites outside seams, uploads/hashing via `localpath` bypass the shim (noted in `system.md`). |
| 8 | Per-drive capability profile | nothing | Current. |
| 9 | Metadata vs content change | #810 D-057 (identity never a content signal), D-059b (missing remote hash not an edit), #824 ctime. | Add #810. |
| 10 | "Why" log | nothing | Current. |
| — | Sequencing | "Item 7 starts with WS9's dispatch seam" — WS9 is done. | Stale. |

---

## Prioritized correction list

Severity: **high** = safety/data-handling claim wrong in either direction; **medium** = wrong default, flag, limit, command or procedure; **low** = wording/staleness.

### 5-1 (high) `docs/safety.md` §Known gaps, row "unmounted, emptied or replaced sync folder"
Current: "**An unmounted, emptied or replaced sync folder is not detected.** If `sync_dir` is on a disk … every synced file looks deleted and the deletes are sent to OneDrive. The same applies to a separate filesystem mounted *inside* the sync folder. | Keep `sync_dir` on a local, always-present disk…"
Replace the whole "Known gaps" table (rows 161–169) with:
```
| Today | What to do meanwhile |
| --- | --- |
| **A folder re-created at the sync path can pass the identity check on a few filesystems.** onedrive-go marks the sync folder with an extended attribute and, where the filesystem has none, falls back to its inode, filesystem ID and creation time. On a filesystem with neither user extended attributes nor creation times (some FUSE, SMB and FAT mounts), a folder deleted and re-created at the same path that happens to get the same inode is accepted; the delete hold below is then the only guard. | Keep `sync_dir` on a local ext4, Btrfs, XFS, APFS or tmpfs filesystem, or `pause` the drive before unmounting or restoring it. |
| **Fewer than 50 deletes (and under 30 % of your files) are not held.** The delete hold is for mass loss, not for individual mistakes. | Local deletes can be undone from `trash list` / `trash restore` for 30 days; OneDrive deletes from the recycle bin. |
| **Moving a synced file into a folder you excluded with a filter deletes it on OneDrive** (to the recycle bin). The file is kept on your computer. | Move the file out of the excluded folder, or restore it from the recycle bin. |
| **An edit that keeps a file's size, modification time *and* change time is not seen.** Normal tools cannot do this (even `cp -p` and `touch -r` move the change time); it needs root or a changed clock. | Rare in practice. |
| **A second sync client on the same folder is only partly detected.** A second onedrive-go using the same data directory is refused, and the markers of Dropbox, Syncthing, Nextcloud and ownCloud trigger a warning; a second onedrive-go under another user or data directory, or a tool without a marker (rclone, abraunegg/onedrive), is not detected. | Do not point two sync clients at the same folder. |
| **Files OneDrive reports without a hash** (some work, school and SharePoint files) are checked by size only after transfer. | None needed; a size mismatch still fails the transfer. |
| **Concurrent-edit protection on work, school and SharePoint drives is unprobed.** Overwrites are conditional on the file's version (`If-Match`); OneDrive Personal honours this. On other drive types the worst case is that an upload is replanned, never that an edit is overwritten silently. | Check OneDrive's version history after a conflict. |
```
Evidence: R-6.2.12 (`spec/requirements/non-functional.md:32`), `data-model.md:279`, R-6.2.5 (`planner_delete_safety.go:33-35`), `sync-observation.md:645`, R-2.2.4 + `sync-observation.md:435`, R-2.9.5/R-2.9.6 (`engine_root_lock.go:175-196`), R-5.5.1, R-5.6.2. PRs #802, #803, #804, #805, #808, #815, #819, #822.

### 5-2 (high) `docs/safety.md` §Known gaps intro (lines 155–157)
Current: "onedrive-go is pre-release, and the safety work above is not finished. These are the behaviors to know about **today**; each is being fixed, and this list will shrink as fixes land."
Replace: "onedrive-go is pre-release. The protections above are implemented and tested, but they have been used only in the project's own testing. These are the limits of the protection **today**."
Evidence: #802–#825 commit bodies; `spec/requirements/non-functional.md` R-6.2/R-6.4 children all `[verified]`.

### 5-3 (high) `docs/safety.md` §Deletes, bullet "On your computer, deletes are real deletes" (lines 83–87)
Current: "**On your computer, deletes are real deletes.** onedrive-go does not move files to a local trash. Before deleting a local file because it was deleted on OneDrive, it checks the file against the baseline (by hash, or by size and modification time when no hash was recorded). If it does not match, your file is kept and uploaded again instead."
Replace: "**On your computer, deletes go to onedrive-go's own trash.** Every file sync deletes on your computer, and the previous version of every file a download replaces, is moved to a per-drive trash in the data directory and kept for 30 days. `onedrive-go trash list --drive <drive>` shows it, `trash restore <id>` puts a file back where it was (never overwriting anything; the next sync uploads it again), and `trash empty --confirm` empties it. Before deleting a local file because it was deleted on OneDrive, onedrive-go checks the file against the baseline (by hash, or by size and modification time when no hash was recorded) and acts on exactly the file it checked. If it does not match, or the file is replaced while being checked, your file is kept and uploaded again instead."
Then add a new bullet: "**Many deletes at once are held for your confirmation.** If one side has no files at all while the last sync recorded some, or if more than 50 files and folders — and more than 30 % of what is synced — would be deleted toward one side, those deletes (and anything that depends on them) are held. Everything else keeps syncing. `status` lists them as 'Deletes held for confirmation', the sync summary says 'Held N deletes for confirmation', and `onedrive-go sync --confirm-deletes` applies exactly that set after you have looked. The hold lifts by itself if the files come back. Renames and moves never count."
Evidence: R-6.4.9, R-6.2.5, R-6.4.2; `internal/localtrash/localtrash.go:44`; `internal/cli/trash.go:22-26`; `internal/cli/sync_console.go:373-375`; `sync-planning.md:548-572`.

### 5-4 (high) `README.md` §How your files are kept safe (lines 134–135, 148–150)
Current (134–135): "**Deletes on OneDrive go to the OneDrive recycle bin.** Deletes on your computer are real deletes, so the recycle bin is where a mistake is undone."
Replace: "**Deletes go to a recycle bin on both sides.** Deletes on OneDrive go to the OneDrive recycle bin; files sync deletes or replaces on your computer go to onedrive-go's own per-drive trash for 30 days (`trash list`, `trash restore`)."
Current (148–150): "There is no limit on how many deletes one sync propagates, and an unmounted or emptied sync folder is not yet detected. Read [docs/safety.md](docs/safety.md) for the full model and its known gaps."
Replace: "- **Mass deletes are held for confirmation.** If one side is suddenly empty, or more than 50 files and 30 % of what is synced would be deleted, nothing is deleted until you run `sync --confirm-deletes`.\n- **A replaced or unmounted sync folder is refused, not synced.** The sync state is bound to the folder it was built from; if a different folder (or an empty mount point) is at the path, sync stops and names `drive rebind-root` as the way to accept it.\n\nRead [docs/safety.md](docs/safety.md) for the full model and its remaining limits."
Evidence: R-6.4.9, R-6.2.5, R-6.2.12; `observation_root_identity.go:32`.

### 5-5 (high) `docs/faq.md` §How do I move my sync folder after it has synced? (lines 174–187)
Current: "Do not just point `sync_dir` at a new, empty folder: onedrive-go would see every synced file as deleted and delete it on OneDrive (see known gaps). Instead: 1. Stop syncing… 2. Move the folder with `mv`. 3. Change `sync_dir`… 4. Run `onedrive-go drive reset-sync-state --drive <drive>` and then `onedrive-go sync --dry-run` to check, and `onedrive-go sync`. After the reset the next sync merges both sides like a first sync, so nothing is deleted."
Replace: "1. Stop syncing (`systemctl --user stop onedrive-go`, or Ctrl-C).\n2. Move the folder with `mv`.\n3. Change `sync_dir` in the config file.\n4. Start syncing again. onedrive-go recognizes the folder by a marker it keeps on it, so a move within one filesystem needs nothing more. If the folder was copied to another filesystem or restored from a backup without extended attributes, sync refuses to start and names the fix: run `onedrive-go drive rebind-root --drive <drive>`, check that the file counts it shows match, and type `REBIND`. Your sync state is kept, so nothing is re-transferred.\n\nPointing `sync_dir` at a new, empty folder by mistake is safe: the folder is refused as not the one that was synced, and even if it were accepted, deleting everything on one side is held for confirmation (see [how it keeps files safe](safety.md#deletes)). Use `drive reset-sync-state` only when you want to discard the sync state; it makes the next sync merge both sides like a first sync."
Evidence: R-6.2.12; `internal/cli/drive_rebind_root.go:251-263`; `data-model.md:268-278`; #805.

### 5-6 (high) `docs/comparison.md` rows 45–46 and "Choosing" (63–65)
Row 45 current: "| Mass-delete protection | None yet (see [safety](safety.md#known-gaps)) |"
Replace: "| Mass-delete protection | Deletes are held for confirmation when one side is empty or more than max(50, 30 % of synced items) would be deleted; `sync --confirm-deletes` applies them (fixed policy, not configurable) |"
Row 46 current: "| Local deletes caused by remote deletes | Deleted outright, after checking the file is unchanged |"
Replace: "| Local deletes caused by remote deletes | Checked against the last synced version, then kept in onedrive-go's per-drive trash for 30 days (`trash list` / `trash restore`) |"
Lines 63–65 current: "…released, packaged, widely used, with national-cloud support, a local trash for remote-caused deletes and a big-delete safeguard."
Replace: "…released, packaged, widely used, with national-cloud support, bandwidth limits and desktop integration."
Also update line 9 "Checked on 2026-10-25" for the onedrive-go column. Evidence: R-6.2.5, R-6.4.3, R-6.4.9.

### 5-7 (high) `docs/limitations.md` §Command limits (lines 37–39)
Current: "- **Local deletes do not use a trash folder.** When a file is deleted on OneDrive, the local copy is deleted outright (after checking it is unchanged). Restore from the OneDrive recycle bin if needed."
Replace: "- **Local deletes use onedrive-go's own trash, not the desktop trash.** Files sync deletes or replaces on your computer are kept for 30 days under onedrive-go's data directory, not in the FreeDesktop or Finder trash; use `onedrive-go trash list` and `trash restore <id>`."
Evidence: R-6.4.9; `internal/localtrash/localtrash.go`.

### 5-8 (high) `docs/safety.md` §Known gaps row "Some changes that make a file unsyncable look like deletes" (line 163) — covered by 5-1 (row removed). Replacement prose for §What never counts as a delete (after line 106):
Add bullets: "- **A synced file replaced by something sync cannot handle.** A synced path now holding a symbolic link, FIFO or device, a file renamed to a name OneDrive rejects or to an excluded name, or an entry that cannot be read or hashed, is reported in `status` and left alone on OneDrive.\n- **A disk mounted inside the sync folder.** A folder that is the mount point of another filesystem is not entered (unless `cross_filesystems = true`), and a disk that was synced and is later not mounted reads as unavailable, not as deleted.\n- **An item missing from a full listing.** Before an item that a full listing did not return is deleted locally, OneDrive is asked for it by ID; only a definite 'gone' answer deletes it.\n- **A sync folder that is not the one that was synced.** See 'Previewing and starting over'."
Evidence: R-6.2.14, R-6.2.15, R-6.2.2, R-6.2.12.

### 5-9 (high) `docs/safety.md` §Transfers (lines 110–116)
Current: "…checked against the hash OneDrive reports, and only then renamed over the real name. An interrupted download never leaves a half-written file at the real name, and large downloads resume where they stopped.\n- **Uploads of large files use resumable upload sessions**, and resume after an interruption."
Replace: "…checked against the hash OneDrive reports (or its size when OneDrive has no hash), written to disk (`fsync`), and only then renamed over the real name; the file it replaces goes to the local trash. A download whose content cannot be verified fails after three attempts and leaves your file untouched. An interrupted download never leaves a half-written file at the real name, and large downloads resume where they stopped, but only while OneDrive still has the same version. A file that an application has open as a SQLite database is never replaced underneath it.\n- **Uploads are verified and conditional.** Every upload goes through a resumable session that resumes after an interruption; afterwards the stored file is compared with what was sent (hash, or size), and a mismatch fails the upload instead of being recorded as synced. Overwrites are conditional on the version onedrive-go last saw, so an edit made on OneDrive while the upload was in flight makes the upload fail and be replanned rather than overwrite the edit. A rename made by onedrive-go keeps the file's modification time."
Evidence: R-5.5.1, R-5.5.2, R-5.10.1, R-5.2.2, R-2.4.15, R-5.6.2, R-2.17.5; #803, #807.

### 5-10 (high) `docs/safety.md` §Known gaps rows "`%` sequences" (164) and "name variants" (165) — removed by 5-1. Add to §What is skipped (line 134–135), replacing the "Case collisions" bullet:
Current: "- **Case collisions.** Two local files whose names differ only in letter case cannot both exist on OneDrive, so neither is uploaded until one is renamed."
Replace: "- **Name collisions.** Two local names that OneDrive treats as one — differing only in letter case, in how accents are composed, in emoji, or by characters OneDrive ignores — cannot both exist there, so neither is uploaded until one is renamed; `status` names both. A new file is never created over an existing OneDrive item of the same name. Two OneDrive items that this computer would store under one name are likewise left alone and reported."
Evidence: R-2.12.3, R-2.12.6, R-5.6.14; `status_condition_descriptors.go:315-324`; #804.

### 5-11 (high) `docs/safety.md` §Later syncs (after the table, line 52) and §Deletes folder bullet (91–94)
Add after the table: "Deletes in the last three rows are subject to the delete hold and, on your computer, go to the local trash (see Deletes below)."
Folder bullet current: "…(with `ignore_junk_files = true`, leftover junk files such as `.DS_Store` do not count)."
Replace: "…(junk files such as `.DS_Store` do not count unless you set `ignore_junk_files = false`). A local folder that stays only because it holds content a filter excludes is not re-created on OneDrive."
Evidence: R-2.4.14 default-on (`config.go:83-88`), R-2.17.2 (#825).

### 5-12 (medium) `docs/limitations.md` §OneDrive behavior, rename bullet (lines 75–79)
Current: "**A rename resets the modification time on OneDrive.** Renaming a file, on the web or through onedrive-go, sets its OneDrive modification time to the time of the rename; the content is unchanged. onedrive-go does not yet preserve the original time when it renames, so after a rename the file's time on OneDrive differs from the local one."
Replace: "**A rename on the OneDrive website resets the modification time there.** OneDrive sets a renamed file's modification time to the time of the rename unless the rename carries the old time, which onedrive-go's `mv` and sync moves do. After a rename made on the web, onedrive-go records the new time without transferring the file again."
Evidence: R-2.17.5, R-1.7.4, `graph-client.md:301-303`; #807.

### 5-13 (medium) `docs/limitations.md` §OneDrive's own rules, "Emoji in names" (lines 64–67)
Current: "…so `notes😀.txt` and `notes.txt` are the same name there. Keep such files in different folders (see [safety](safety.md#known-gaps))."
Replace: "…so `notes😀.txt` and `notes.txt` are the same name there. onedrive-go treats such pairs as a name collision: neither is uploaded until one is renamed (see [safety](safety.md#what-is-skipped))."
Evidence: R-2.12.3; #804.

### 5-14 (medium) `docs/configuration.md` §Drive settings, `ignore_junk_files` row (line 105)
Current: "| `ignore_junk_files` | boolean | `false` | Skip common junk: `.DS_Store`, `Thumbs.db`, `__MACOSX`, names ending in `.tmp`, `.swp`, `.partial` or `.crdownload`, and names starting with `~`, `.~` or `._` (but not Office's `~$` lock files). |"
Replace: "| `ignore_junk_files` | boolean | `true` | Skip OS, editor and browser junk on both sides: `.DS_Store`, `Thumbs.db`, `__MACOSX`, `.Spotlight-V100`, `.Trashes`, `.fseventsd`, `.TemporaryItems`, `.Trash-*`, `$RECYCLE.BIN`, `Icon\r`, names starting with `._`, `.~`, `~` (including Office `~$` owner files), `#…#` or `.#`, names ending in `~`, `.tmp`, `.swp`, `.swo`, `.swx`, `.partial`, `.crdownload`, `___jb_tmp___` or `___jb_old___`, the Vim probe file `4913`, and the `-wal`, `-shm` and `-journal` sidecars of a SQLite database whose name has an extension. The `~`, `~…` and `4913` rules apply to files only, so a folder named `~Projects` syncs. Set to `false` to sync everything. |"
Evidence: `internal/config/config.go:83-101`, R-2.4.14, `content_filter.go:230-289`, `spec/design/config.md:229`; #802.

### 5-15 (medium) `docs/configuration.md` §Drive settings — add `cross_filesystems` row after `follow_symlinks` (line 106)
Add: "| `cross_filesystems` | boolean | `false` | By default a folder inside the sync folder that is the mount point of another disk or filesystem is not entered; `status` reports it as a local read boundary and nothing under it is synced or deleted on either side. When true, such folders are synced, and onedrive-go remembers them so that a disk that is later not mounted reads as unavailable rather than as every file in it deleted. |"
Also `sync_dir` row (line 100): append "The folder must be the one the drive's sync state was built from; after moving or restoring it, see `drive rebind-root` in [safety](safety.md#previewing-and-starting-over)."
Evidence: `config.go:90-94`, R-6.2.15, R-6.2.12; #805.

### 5-16 (medium) `README.md` §Commands table (lines 154–166)
Replace rows: "| `sync` | Sync once; `--watch`, `--download-only`, `--upload-only`, `--dry-run`, `--full`, `--confirm-deletes` (apply deletes held for confirmation), `--allow-root` |", "| `status` | Accounts, drives, shared folders, and anything that needs you (`--perf` adds live performance data from a running sync) |", "| `mkdir`, `mv`, `cp`, `rm` | Create, move, copy (server-side) and delete (`rm -r` for folders; `rm` uses the recycle bin, `--permanent` skips it) |", "| `drive list`, `add`, `remove`, `search` | Choose which drives are configured; find SharePoint libraries (`drive list --all` lifts the SharePoint site cap) |", "| `drive reset-sync-state`, `drive rebind-root` | Start a drive's sync state over (files untouched) / accept a moved or restored sync folder, keeping the state |", and add "| `trash list`, `restore`, `empty` | Files sync deleted or replaced on this computer, kept for 30 days per drive |".
Evidence: `--help` output; `internal/cli/{sync,trash,drive_rebind_root,rm,drive,status}.go`.

### 5-17 (medium) `README.md` §Status (lines 13–14)
Current: "Data-safety hardening is in progress (see [known gaps](docs/safety.md#known-gaps))."
Replace: "The data-safety model (delete hold, local trash, sync-folder identity, verified transfers) is implemented and tested, with the remaining limits listed in [docs/safety.md](docs/safety.md#known-gaps)."
Check `internal/devtool/verify_readme.go` `staleReadmeStatusPhrases()` before applying (the verifier scans this section).

### 5-18 (medium) `docs/troubleshooting.md`
(a) Add rows to the §Problems `status` reports table (after line 43):
"| Deletes held for confirmation | More was deleted on one side than looks intended (an empty side, or over 50 items and 30 % of the drive). Nothing is deleted; everything else keeps syncing. | Check the folder. If the deletes are right, `onedrive-go sync --confirm-deletes`; if not, restore the files and the hold clears. |", "| Disk not mounted / local read boundary | A folder inside the sync folder is a mount point for another filesystem (not entered unless `cross_filesystems = true`), or a disk that was synced is not mounted now. | Mount the disk, or set `cross_filesystems`. |", "| Synced file replaced by something sync cannot handle | A synced path now holds a symbolic link, FIFO or device, or a name OneDrive rejects. Nothing is deleted on OneDrive. | Rename or move the entry. |", "| Two OneDrive items, one name here | OneDrive has two items this computer would store under one name. Neither syncs. | Rename one of them on OneDrive. |", "| Database open in an application | A download would replace a SQLite database whose `-wal`/`-journal` file is in use. | Close the application; it is retried. |", "| Blocked as malware | OneDrive's scan flagged the file; it will not let it be downloaded. | Delete or restore it on the OneDrive website. |", "| Interrupted removal | A `.onedrive-go.<id>.detach` file was left in the sync folder by an interrupted delete or replace. It is never deleted automatically. | Inspect it; move it out or delete it yourself. |", "| Transfer could not be verified | A download or upload did not match OneDrive's hash or size after retries; your file is untouched. | Retry later; report it if it persists. |".
(b) Reword line 38 to "Name collision | Two local names that OneDrive treats as the same (letter case, accent composition or emoji). Neither is uploaded. | Rename one of them. |".
(c) New section after line 83: "## The sync folder is not the one that was synced\n\nSync records which folder its state belongs to (a marker in the folder's extended attributes, with its inode, filesystem and creation time as fallback) and refuses to run when a different folder is at `sync_dir`: an unmounted disk's empty mount point, a wiped or restored folder, a re-pointed symbolic link. The error says whether the folder lacks the `user.onedrive-go.root` marker or carries a different one. If you moved or restored the folder on purpose, run `onedrive-go drive rebind-root --drive <drive>`: it shows how many files the state remembers and how many the folder holds now, and asks you to type `REBIND` (or pass `--yes`). The sync state is kept; if files are missing from the folder, the next sync holds their deletes for confirmation instead of deleting them on OneDrive."
(d) Replace lines 115–117 ("When a tree comes close to it, onedrive-go warns; when it is reached, new folders are noticed only by the periodic rescan (every 5 minutes).") with: "When a tree comes close to it, onedrive-go warns. If the limit is already reached when watching starts (or the inotify instance or open-file limit is), watch mode keeps syncing by scanning the whole folder every 5 minutes instead, warns once with the setting to raise, and tries to watch again after 10 minutes, doubling up to an hour. If the limit is reached for a new folder later, that folder is picked up by the periodic rescan."
Evidence: `status_condition_descriptors.go:144,316-324,398`; R-6.2.5, R-6.2.14, R-6.2.15, R-2.12.6, R-2.4.15, R-2.10.70, R-6.4.9, R-5.5.1/2; R-6.2.12 + `drive_rebind_root.go:251-263`; R-2.8.12.

### 5-19 (medium) `docs/faq.md` §Can I use onedrive-go alongside another OneDrive client? (lines 152–153)
Current: "…and can undo or duplicate each other's work; onedrive-go does not detect this."
Replace: "…and can undo or duplicate each other's work. onedrive-go refuses to start a second sync of the same folder from the same data directory and warns when it finds another client's marker (Dropbox, Syncthing, Nextcloud, ownCloud) or a folder under `~/Library/CloudStorage`, but it cannot detect every other client, so keep the folders separate."
Evidence: R-2.9.5, R-2.9.6; `engine_root_lock.go:175-196`.

### 5-20 (medium) `docs/running-as-a-service.md` — add after line 68
Add: "`sync` refuses to run as root unless `--allow-root` is given (it would leave root-owned files in your folder), and exits with status 2. Run it as a user unit, or set `User=` in a system unit."
Evidence: R-6.3.7; `internal/cli/sync.go:497-528`.
Also add to §Day-to-day commands: "`onedrive-go sync --confirm-deletes --drive <drive>` applies deletes the running sync held for confirmation; stop the service first, since only one sync may run, and `--confirm-deletes` is one-shot only."
Evidence: `sync.go:405` (mutually exclusive with `--watch`), R-2.9.4.

### 5-21 (medium) `docs/safety.md` §Previewing and starting over (after line 151)
Add: "- `drive rebind-root` accepts the folder now at a drive's sync path as its sync folder after you moved or restored it on purpose. It shows the remembered and present file counts and asks you to type `REBIND`. The sync state is kept; missing files are held for confirmation rather than deleted on OneDrive."
Evidence: R-6.2.12; `drive_rebind_root.go:251-263`.

### 5-22 (medium) Requirement traceability mis-citations
- `internal/devtool/verify_runner_test.go:42,145`: `// Validates: R-6.2.1, R-6.2.2` on devtool step-list tests → should be `R-6.10.1`.
- `internal/cli/side_effect_contract_test.go:21,37,63` and `internal/sync/executor_conflict_reservation_test.go:6,35,55`: `R-6.2.4` cited by tests that do not exercise local-delete verification → cite `R-6.2.8`/`R-2.3.5` respectively (or drop).
- Add `// Validates: R-6.2.11` to `TestPlanCurrentState_HashlessSameSizeIsNotTreatedAsEqual` and siblings; `R-6.4.1` to the `TestExecutor_LocalDelete_HashMismatch*` tests; locate or write the `R-6.2.7` test (client partials never upload).
- Demote R-6.1.4 and R-6.1.5 from `[verified]` to `[target]` (no test, rule or artifact), or add a verifier rule for the binary size.
Evidence: §3a/§3b above.

### 5-23 (medium) `spec/requirements/non-functional.md:18,47` and `drive-management.md:111` group headers
Current: "## R-6.2 Data Integrity [implemented]", "## R-6.4 Safety [implemented]", "## R-3.3 Drive Management Commands [implemented]".
Replace each `[implemented]` with `[verified]` (every child is `[verified]`); mirror in `spec/requirements/index.md` if the summary column changes.

### 5-24 (medium) `spec/reference/rearchitecture-candidates.md`
- Line 5–6: "updated 2026-10-28 after PRs #799-#808" → "updated <date> after PRs #799-#825".
- §1 "Landed": append "#810 composes moves per item by (parent item, name) with SQLite keeping only structural pairing; #819/#822 add the entry's birth time to local identity, the generation guard this item asks for; the model test converges 80/80 seeds (#810) and the planner-level model 500/500 (#812)."
- §2 "Still open": replace "D-025 (NFD names on byte-exact Linux filesystems) and D-023 … remain" with "D-023 (cross-side grouping in `sqlite_compare.go`) remains; D-025 was closed by #815 (`synctree.Root.Spelling`) and #823."
- §3 "Landed": append "#811 makes a synced folder's tombstone pull the full refresh forward; #812 backs off undecided orphans and applies orphan confirmation to shared-folder mounts; #822 binds the root's birth time; #825 records a folder kept for ignored content so it is not re-created online."
- §5 "Landed": append "#818 recognises the client's placeholder by item ID and emptiness rather than its creation eTag; #824 records the destination's change time after an applied move."
- §6 "Landed": replace with "the pending-create record (#804) and the crash-point harness of #811 (`internal/sync/crash_points.go`, D-091): six named commit boundaries with a test hook that kills the run at each, and recovery must converge. The env-var injector proposed below exists as an in-process hook."
- §7 "Evidence": replace "Its limits are non-deterministic dispatch (D-090) and no virtual clock (D-095)" with "#811 added the engine-owned dispatch order and serialized worker (D-090) and the `testing/synctest` virtual clock (D-095); #812 the chaos Graph layer (D-092), filesystem shims (D-094) and the planner-level model (D-093). Remaining: uploads, hashing and followed-symlink reads bypass the filesystem seam via `localpath` (system.md), and `time.Now`/`os.*` sites outside the seams."
- §9 "Landed": append "#810: identity is never a content signal (D-057) and a hash known on one side only is not an edit (D-059b)."
- §Sequencing: "Item 7 starts with WS9's dispatch seam" → "Item 7's harness landed in WS9; what remains is routing the last direct I/O through the seams."

### 5-25 (low) `CLAUDE.md` routing table
Delete lines 30 and 31 (exact duplicates of line 29). Remove `internal/sync/protected_roots.go` and the `shortcut_*` patterns from line 28 (owned by `sync-shortcut.md` per `GOVERNS`). Remove `internal/sync/scope_key.go, internal/sync/scope_semantics.go` from line 33 (or from line 25) so each appears once. Change line 13 to point `drive_reset_sync_state.go` at `spec/design/drive-identity.md` (its `GOVERNS` owner), keeping `sync-store.md`/`sync-engine.md` in "Also consult". Add rows: "| `internal/localtrash/`, `internal/localpath/`, `internal/fsroot/`, `internal/namekey/`, `internal/sharedref/`, `internal/authstate/`, `internal/synctest/` | `spec/design/system.md` | |", "| `internal/loglevel/` | `spec/design/cli.md` | |", and add `internal/synccontrol/` to line 32 and `internal/cli/trash.go` to line 34.
Evidence: `GOVERNS:` lines of `spec/design/*.md`.

### 5-26 (low) `spec/design/system.md:448`
Current: "…giving `syncdomain` <- `syncstore`/`syncplanner`/... <- `sync`."
Replace: "…giving a hypothetical `syncdomain` <- `syncstore`/`syncplanner`/... <- `sync` (names used for illustration only; no such packages exist, and the earlier packages of similar names were collapsed into `internal/sync`)."

### 5-27 (low) `spec/design/data-model.md:279`
Current: "Remaining gap: on a filesystem without extended attributes, a folder recreated at the same path that happens to get the same inode on the same filesystem is accepted;"
Replace: "Remaining gap: on a filesystem without extended attributes and without recorded birth times, a folder recreated at the same path that happens to get the same inode on the same filesystem is accepted;"
Evidence: #822 (`SameRoot` compares birth times when both known).

### 5-28 (low) Design docs citing test files that do not exist
`drive-transfers.md` (`internal/cli/sync_helpers_test.go`, `internal/driveops/download_test.go`, `internal/driveops/upload_test.go`, `internal/graph/upload_session_test.go`), `graph-client.md` (`auth_browser_test.go`, `auth_device_test.go`, `drives_shared_test.go`, `errors_test.go`, `upload_session_test.go`), `retry.md` (`throttle_gate_test.go`), `sync-planning.md` (`planner_dependency_test.go`): replace with the files that now hold those tests (`transfer_download_test.go`, `transfer_upload_test.go`, etc.) or with `TestXxx` names, which the verifier checks.

### 5-29 (low) `internal/cli/sync.go:383-386` and `drive.go:11-15` help text
`sync` Long: "Run a one-shot sync run between the local directory and OneDrive." → "Sync the local folder with OneDrive once, or continuously with --watch.\n\nBy default sync is bidirectional; --download-only and --upload-only make it one-way. --dry-run previews without changing anything. --full lists everything on OneDrive instead of using the change feed. --confirm-deletes applies deletes an earlier run held for confirmation (one-shot only)."
`drive` Short/Long: "(list, add, remove, search)" → "(list, add, remove, search, rebind-root, reset-sync-state)".

### 5-30 (low) `docs/scripting.md`
Line 121–123: add `trash list`, `trash restore`, `trash empty` and `drive rebind-root` to the list of mutating/JSON commands (shapes in `spec/design/cli.md:330-333`). After line 175 add: "Held deletes are not part of the `summary` object; read `status --json` and look for issue `type` `deletes_held`." After line 190 add: "`sync` run as root without `--allow-root` is a usage error (2)."

### 5-31 (low) `SECURITY.md` §What onedrive-go stores
Add: "- Files sync deleted or replaced on this computer are kept for 30 days in a per-drive trash under the same data directory (`onedrive-go trash list`), with the data directory's owner-only permissions."

### 5-32 (low) `docs/limitations.md:28-29`
Append: "The one repo benchmark so far measures sync planning cost: a quiet pass over 100,000 synced files spends about 20 s in planning queries on a 2-core machine (`spec/design/performance-benchmarking.md`)."
Evidence: #812 benchmark table.

### 5-33 (low) `spec/design/threat-model.md`
Add the sync-root identity binding (R-6.2.12), the delete hold (R-6.2.5) and the local trash (R-6.4.9) to the mitigations table for the "unmounted volume / replaced folder / mass delete" threat, with `engine_root_identity_test.go`, `engine_delete_safety_test.go`, `localtrash_test.go` as evidence.

---

## Facts verified as correct (no change needed)

README build/install instructions and Go 1.26 requirement; dev-build XDG refusal; global flags; exit-code table; systemd unit semantics and watchdog math; config file locations and env overrides; all ten global config defaults and ranges; filter pattern semantics; reload behaviour (5 s poll, SIGHUP); `--json` contract and `sync --json` event/action names; `status --json` shape; SECURITY.md token file names, scopes and socket permissions; CONTRIBUTING verifier stages and CI job names; CLAUDE.md fast-E2E test list; recycle-bin personal-account limitation; Personal Vault and OneNote skipping; conflict-copy naming; first-sync merge semantics; `drive reset-sync-state` semantics; proxy env vars; no ghost references to deleted packages in code or docs.
