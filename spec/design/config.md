# Configuration

GOVERNS: internal/config/account_owner.go, internal/config/catalog.go, internal/config/catalog_lifecycle.go, internal/config/config.go, internal/config/decoder.go, internal/config/defaults.go, internal/config/discovery.go, internal/config/display_name.go, internal/config/drive.go, internal/config/drive_enroll.go, internal/config/email_change_journal.go, internal/config/email_reconcile.go, internal/config/env.go, internal/config/failure_class.go, internal/config/holder.go, internal/config/load.go, internal/config/managed_io.go, internal/config/mount_state_path.go, internal/config/mutation_lock.go, internal/config/paths.go, internal/config/resolved_validator.go, internal/config/resolver.go, internal/config/size.go, internal/config/token_resolution.go, internal/config/toml_lines.go, internal/config/unknown.go, internal/config/validate.go, internal/config/validate_drive.go, internal/config/validated_state.go, internal/config/validator.go, internal/config/write.go

Implements: R-3.7 [verified], R-4.1 [verified], R-4.1.5 [verified], R-4.2 [verified], R-4.3 [verified], R-4.4 [verified], R-4.8.1 [verified], R-4.8.2 [verified], R-4.8.3 [verified], R-4.8.4 [verified], R-4.8.5 [verified], R-4.8.6 [verified], R-4.9.1 [verified], R-4.9.2 [verified], R-4.9.3 [verified], R-4.9.4 [verified], R-6.3.4 [verified], R-6.6.31 [verified], R-6.8.16 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## Clearing The Account Auth Requirement Has One Owner

`ClearAccountAuthRequirement` is the single writer of that field's cleared
state. It used to accept an `AuthClearSource` and ignore it, which advertised
a boundary the catalog does not have: the catalog records whether a
requirement stands, never how it ended. The parameter is gone, and callers name
the reason in their own logging instead, so the log is where the source lives
and the signature no longer implies otherwise.

`ApplyPlainLogout` was a verbatim copy of that function under a different name,
giving one durable field two writers. Logout now calls the shared path, which
also means a logout-driven clear is logged with its source like every other
clear; previously it was the one clear that happened silently. That is what
`AuthClearSourceLogout` was for -- it existed but nothing used it, which was
the visible end of this duplication.

## Overview

TOML configuration with flat global settings and per-drive sections. Drive sections are identified by `:` in the section name (e.g., `["personal:user@example.com"]`).

## Ownership Contract

- Owns: Config-file schema, catalog schema, durable lifecycle mutation rules for config/catalog-managed inventory, child state DB path derivation, override resolution, path discovery, drive-section resolution, token-path resolution, and validation policy.
- Does Not Own: OAuth exchange, Graph request behavior, sync runtime orchestration, or durable sync-store contents.
- Source of Truth: The loaded `Config` snapshot plus derived `ResolvedDrive` values at the config/CLI selection boundary. `Holder` is the single in-process source of truth for reloadable config state; sync orchestration receives `multisync.StandaloneMountConfig` values compiled at the CLI edge, and runtime session/engine construction consume mount-owned identity compiled above their boundaries.
- Allowed Side Effects: Reading and writing config and managed inventory/state files through `fsroot`, plus statting arbitrary user-selected local paths through `localpath` and syncing the data directory through `localpath` once an account email change has renamed files in it.
- Mutable Runtime Owner: `config.Holder` owns the current `*Config` pointer behind `Holder.mu`. The package starts no background goroutines, owns no long-lived channels, and uses no timers; its only wait is the bounded lock poll of a config, catalog or account email change (`mutation_lock.go`), which runs in the caller's goroutine.
- Error Boundary: `config` translates parse/load/validation outcomes into either fatal errors or warnings before callers act, following [error-model.md](error-model.md).

## Verified By

| Behavior | Evidence |
| --- | --- |
| The config file is written owner-only, because its section keys name the signed-in accounts (R-6.8.23). | `TestAtomicWriteFile_SetsPermissions` |
| Drive resolution applies pause semantics consistently, including expired timed pauses. | `TestResolveDrives_ExcludesPausedByDefault`, `TestResolveDrives_IncludePausedWhenRequested`, `TestClearExpiredPauses_ClearsExpired` |
| `buildResolvedDrive` owns defaulting and per-drive override materialization for sync callers. | `TestBuildResolvedDrive_GlobalDefaults`, `TestBuildResolvedDrive_NoPerDriveOverridesBeyondDriveFields`, `TestBuildResolvedDrive_TimedPauseExpired` |
| Standalone shared-folder drives always preserve the canonical remote root item even when the backing drive ID comes from the catalog. | `TestBuildResolvedDrive_SharedCanonicalSetsRootItem`, `TestBuildResolvedDrive_SharedCatalogDrivePreservesRootItem` |
| Mount-root delta capability is resolved in config from shared-drive ownership facts before sync engine construction. | `TestBuildResolvedDrive_SharedBusinessOwnerDisablesFolderDelta`, `TestBuildResolvedDrive_SharedUnknownOwnerDefaultsFolderDeltaCapable`, `TestStandaloneMountSelectionFromResolvedDrives_PreservesMountBoundaryFields` |
| Managed shortcut children keep stable child mount IDs and retained state DB paths without becoming synthetic configured drives. Parent-owned alias lifecycle state lives in the parent sync store, while multisync owns child-artifact purge using config path/catalog primitives only. | `TestMountStatePath_UsesManagedMountPrefix`, `TestRunOnce_ParentCleanupRequestPurgesShortcutChildStateArtifacts`, `TestPurgeShortcutChildArtifacts_IgnoresExplicitMountID`, `internal/multisync/shortcut_child_work_test.go`, `internal/sync/shortcut_root_state_test.go` |
| Token-owner resolution stays config-owned for shared and business-derived drives. | `TestDriveTokenPath_Shared_WithCatalogDrive`, `TestTokenAccountCID_Shared`, `TestTokenAccountCID_SharePoint` |
| Control-socket path derivation keeps the socket under the data dir when possible, falls back to a stable hashed runtime dir when necessary, and fails explicitly when neither path can satisfy the Unix socket length budget. | `TestControlSocketPath_UsesDataDirWhenShortEnough`, `TestControlSocketPath_UsesShortRuntimePathWhenDataDirIsTooLong`, `TestControlSocketPath_ReturnsErrorWhenFallbackStillExceedsLimit` |
| Child mount state DB path derivation is stable, collision-resistant, and bounded by common basename limits. | `TestMountStatePath_UsesManagedMountPrefix`, `TestMountStatePath_EncodesManagedMountIDWithoutCollisions`, `TestMountStatePath_LongIDUsesBoundedFilename` |
| Token and state-database discovery treats a missing data directory as empty and an unreadable one as a Warn plus an error naming the directory, never as "nothing found". | `TestDiscoverCIDFiles_ReadDirError`, `TestDiscoverCIDFiles_MissingDataDirIsNotAnError`, `TestDiscoverTokensIn_NonexistentDir` |
| Config writes are locked across processes, and an append refuses a section that is already there. | `TestAppendDriveSection_CrossProcessCreatesPreserveAllSections`, `TestAppendDriveSection_RefusesASectionThatIsThere` |
| The catalog is refused whole when a record key, a record's `canonical_id`, a drive's `owner_account_canonical_id` or an account's `primary_drive_canonical_id` is not a canonical drive ID, with an error naming the file, the record and the field and saying how to recover (R-4.1.4, R-4.1.5). | `TestDecodeCatalog_RefusesAMalformedCanonicalID`, `TestUpdateCatalog_RefusesToStoreAMalformedCanonicalID`, `TestDecodeCatalog_AcceptsWhatTheWritersWrite` |
| Drive-section values are written as the kind the drive-key schema gives their key, a value the key cannot hold is refused with the file unchanged, and a section name keeps a quote or a backslash through append, find, set and delete (R-4.2.2). | `TestSetDriveKey_StringKeyGivenTrueLoadsBackAsAString`, `TestSetDriveKey_RefusesAValueItsKeyCannotHold`, `TestConfigSchema_DriveKeyKindsMatchTheDriveStruct`, `TestDriveSection_NameWithQuoteAndBackslashRoundTrips`, `TestParseSectionHeader_ReadsEveryHeaderForm`, `TestTOMLString_EveryValueLoadsBackUnchanged` |
| Enrolling a drive (`AddDrive`, for `drive add` and `login`) is one transaction under the config lock and then the catalog lock: a failure at any step, a held lock included, leaves config and catalog as they were; a failed catalog restore is reported and leaves what a rerun completes; a rerun completes each partial state and reports it; a complete enrollment writes nothing; a shared folder's section is one write; two processes adding one drive add one section; one directory for config and data is one lock; a config that would not load is never written (R-3.3.5, R-3.3.1, R-4.4.4). | `TestAddDrive_FailureAtEachStepLeavesConfigAndCatalogUnchanged`, `TestAddDrive_FailedCatalogRestoreLeavesWhatARerunCompletes`, `TestAddDrive_RerunCompletesEachPartialState`, `TestAddDrive_CompleteEnrollmentWritesNothing`, `TestAddDrive_SharedSectionIsOneWrite`, `TestAddDrive_CrossProcessSameDriveAddsOneSection`, `TestAddDrive_SameDirectoryForConfigAndDataDoesNotSelfDeadlock`, `TestAddDrive_RefusesAConfigThatWouldNotLoad` |
| A config or catalog change waits at most `configLockTimeout` (10 s, polled every 50 ms) for another holder and then fails with `ErrConfigLocked` naming the locked directory, without running the change; a holder that lets go within the wait is waited for; catalog read-modify-writes run under the same lock, so concurrent updates from two processes both survive (R-4.4.4). | `TestWithConfigMutationLock_HeldLockTimesOutWithTheDirectoryNamed`, `TestWithConfigMutationLock_AcquiresWhenTheHolderReleases`, `TestSetDriveKey_CrossProcessHolderIsNotWaitedForForever`, `TestUpdateCatalog_CrossProcessUpdatesAreNotLost` |

Transfer validation behavior is not user-disableable. The config surface intentionally has no `disable_download_validation` or `disable_upload_validation` escape hatches; transfer correctness policy lives in the transfer and observation layers, not in mutable config toggles.

## Override Chain

Four-layer resolution: defaults → config file → environment → CLI flags. CLI flags replace (never merge) config values.

Only two environment variables: `ONEDRIVE_GO_CONFIG` (config path override) and `ONEDRIVE_GO_DRIVE` (drive selection). All other configuration uses the config file or CLI flags.

## File Locations

Platform-specific paths following XDG (Linux) and Application Support (macOS) conventions. Config and data may share the same directory on macOS.

Dev builds (`version == "dev"`) require full XDG isolation before any command
bootstrap can proceed: `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, and `XDG_CACHE_HOME`
must all be set. Partial isolation is rejected because a single missing root can
fall back to production config, token/state, or cache locations. `scripts/dev-env.sh`
is the canonical local helper for setting those roots.

`ControlSocketPath()` normally places the Unix control socket under
`DefaultDataDir()`. Unix socket path length is platform-bounded, so when the
full data-dir socket path is too long the config layer derives a stable,
hash-named runtime directory under `os.TempDir()` and places only the socket
there. Durable state, tokens, logs, and catalog/state artifacts remain under the normal
config/data/cache roots.

`ControlSocketPath()` returns `(path, error)`, not an empty-string sentinel.
The config layer revalidates the hashed runtime fallback against the same Unix
socket soft limit as the primary path. If neither path fits, it returns an
explicit error describing both candidates. Callers then decide policy at their
own boundary: sync-owner startup treats the error as fatal because the socket
is the single-owner lock, while some CLI mutation/reload flows can intentionally
degrade when the derivation proves no daemon can exist.

`config` uses two filesystem boundaries, not ad hoc `os.*` calls:

- `fsroot` for repo-managed state selected by the config layer: config files, catalog, tokens, and state DB path discovery
- `localpath` for arbitrary local paths supplied by the user or derived from config semantics: `sync_dir` existence/type validation

Config entrypoints still accept full path strings, so `managed_io.go` establishes the managed root per call via `fsroot.OpenPath` or `fsroot.Open`. This keeps the path trust boundary explicit without broad `gosec` carve-outs.

## Persisted Facts And Lifecycle

`config.toml` persists drive sections only. It never contains account sections.
Durable account, drive, and sync-state facts are split across managed files:

- `config.toml`: configured drive sections keyed by canonical drive ID
- `catalog.json`: durable account inventory, drive inventory, profile cache, ownership, and account-level auth requirement
- `token_*.json`: saved login owned by the account
- `state_*.db`: retained configured-standalone-drive sync state
- `state_mount_*.db`: retained managed-child-mount sync state

`config` owns both the persistence mechanics and the durable lifecycle rules
that coordinate config/catalog/token path artifacts. CLI commands may still own
Graph/OAuth flows, token deletion, and user interaction, but they do not
hand-edit `CatalogAccount` or `CatalogDrive` records directly for product
behavior. Automatic shortcut lifecycle is not a config-owned durable fact: the
parent engine stores parent shortcut-root lifecycle in its sync store and
multisync compiles runtime work from ephemeral parent-declared child work
snapshots.

The config file itself is durable even when it has zero drive sections. Removing
the final configured drive or logging out the final configured account leaves
`config.toml` in place with only the global template/comments that remain after
text-level edits.

### Derived Account States

CLI account surfaces derive one lifecycle state from the durable facts above:

| State | Durable facts |
| --- | --- |
| `logged_in_with_configured_drives` | usable saved login + at least one configured drive |
| `logged_in_without_configured_drives` | usable saved login + zero configured drives |
| `auth_required_missing_login` | known account metadata/state remains but no usable saved login exists |
| `auth_required_invalid_saved_login` | token file exists but the saved login is invalid or unreadable, or the token file cannot be checked at all (auth reason `saved_login_unreadable`, whose action names the data directory rather than `login`) |
| `auth_required_sync_rejected` | usable saved login plus a persisted catalog auth requirement |
| `absent` | no durable account facts remain |

### Derived Drive Inventory States

Drive surfaces derive inventory state per canonical drive ID. Authentication
remains account-scoped; drive inventory is separate from whether its owning
account currently has a usable saved login.

| State | Durable facts |
| --- | --- |
| `configured` | drive section exists in `config.toml` |
| `retained_state` | no drive section, but a retained state DB still exists |
| `known_catalog_only` | the catalog knows the drive, but no config section or retained state DB exists |
| `absent` | no durable drive fact exists |

### Transition Summary

| Operation | Durable mutation |
| --- | --- |
| `login` | write saved login, then one enrollment transaction: upsert account/primary-drive catalog records, append (or complete) the primary drive section |
| `drive add` | one enrollment transaction: register the drive in the catalog and append (or complete) one drive section |
| `drive remove` | delete one drive section only |
| `drive remove --purge` | delete one drive section, then (after a running watch is told) drive-owned retained state under the store lock; prune the catalog drive record when no longer needed, and keep it while a kept store remains |
| `logout` | delete saved login and all configured drive sections for the selected account |
| `logout --purge` | `logout` durable mutations plus delete retained state DBs (after a running watch is told, under the store lock) and purge the selected account/drives from the catalog, using best-effort recovery when validated state cannot load |
| external token deletion | removes saved login only; catalog/state may still keep the account known |
| invalid/unreadable token file | keeps the saved-login file path but moves the derived account state to `auth_required_invalid_saved_login`; a token file that cannot be checked (unreadable data directory) lands in the same state with reason `saved_login_unreadable` |
| persisted catalog auth requirement creation | keeps other durable facts intact but moves the derived account state to `auth_required_sync_rejected` |
| successful authenticated proof | clears the persisted catalog auth requirement and returns the derived account state to the matching logged-in state |

Degraded discovery is intentionally outside this lifecycle model. It is a
command-local discovery overlay, not a persisted account or drive state.

Enrolling a drive is one transaction owned by `config` (`AddDrive`,
`drive_enroll.go`); both `drive add` paths (an account's own drive and a shared
folder) and `login` call it, and `internal/cli` rolls nothing back. The CLI
keeps what is not durable inventory: selector resolution, the token
precondition, the shared folder's display-name lookup (a Graph request, made
before the transaction; the transaction never replaces a configured name), the
token install and output. Under the config lock and then the catalog lock (one
lock when both files live in one directory, compared with `os.SameFile` after
both directories exist), the transaction reads `config.toml` and `catalog.json`
once and decides everything from that snapshot: the drive's display name, its
`sync_dir` (`DefaultSyncDir` over the other drives' directories and the account
names in the catalog as this enrollment will leave it), the catalog record
(`registerOwnedDrive`, `registerSharedDrive`, or for login `recordLogin`, which
also writes the account), and the config text. A new section carries `sync_dir`
and, for a shared folder, `display_name` in one append, which refuses a section
that is already there; an existing section gets only the keys it lacks, and its
existing values are never replaced. The new config text must pass the strict
load (so two drives can never be left sharing a sync directory) before anything
is written. Then, in order: the sync directory is created (the CLI's
`materializeDriveSyncDir`, passed as `MakeSyncDir`), the catalog is written if
it changed, and the config is written once if it changed, which is the commit
point. When the config write fails, the catalog bytes read under the lock are
written back (or the catalog file removed when this run created it), still
under the lock; when that also fails, both errors are returned. A failure
therefore leaves config and catalog as they were. A failure after the sync
directory was created also leaves that directory, empty: it is not removed,
since it may be a folder the user already had. A kill leaves at most an empty
sync directory and a catalog record without a section. Running the same
enrollment again completes it. An enrollment whose section exists but lacks
something (its `sync_dir`, a shared folder's `display_name`, its catalog record
or for login the account's, or its sync directory on disk), which a section
written by hand or by an earlier version of the client can, is completed and
reported as completed (`AddDriveResult.Completed`, `completed` in the commands'
JSON); one that is complete writes nothing (a login still refreshes the
account's catalog facts). Login installs the new token before the transaction
and, when the transaction fails, puts the previous token back (or removes the
new one when there was none); a catalog failure during login is an error, not a
warning.

`logout --purge` is intentionally recovery-oriented. When validated config and
catalog state cannot be loaded together, callers may still fall back to the
best recoverable config, token, and catalog inputs and perform durable cleanup
against those facts instead of hard-failing before any purge happens.

## Internal Organization

`config` stays one package, but the responsibilities are separated internally so each layer has one job:

- `load.go`: public config-loading entrypoints and the `configLoader` coordinator
- `decoder.go`: second-pass drive-section decoding for strict and lenient loads
- `failure_class.go`: shared-domain classification for config load outcomes
- `resolver.go`: config-path selection plus single-selection and multi-selection resolution
- `validator.go`: whole-config validation orchestration
- `resolved_validator.go`: post-override resolved-drive validation against the local filesystem
- `validate.go` and `validate_drive.go`: field- and drive-specific validation rules

The public API stays stable (`Load*`, `Resolve*`, `Validate*`), but those entrypoints delegate into these internal collaborators instead of re-mixing decode, resolution, and validation logic in one file.

## Field Reference

This table is the authoritative config-package view of the current schema.
`Command class` names the current consumer boundary at a coarse level:
`sync`, `sync --watch`, `transfer commands`, `all CLI`, or `display`.

### Global Fields

| Key | Type | Default / effective default | Valid range / shape | Command class | Notes |
| --- | --- | --- | --- | --- | --- |
| `transfer_workers` | `int` | `8` | `4..64` | `sync`, `transfer commands` | Shared transfer worker-pool size. |
| `check_workers` | `int` | `4` | `1..16` | `sync` | Parallel local hashing worker count. |
| `min_free_space` | `string` | `1GB` | parseable size string; `0` disables | `sync`, `get`, shared download commands | Disk reservation floor for downloads. |
| `poll_interval` | `string` | `5m` | duration `>= 30s` | `sync --watch` | Remote observation fallback poll cadence. |
| `websocket` | `bool` | `false` | boolean | `sync --watch` | Enables Socket.IO remote wakeups where supported. |
| `dry_run` | `bool` | `false` | boolean | `sync` | Config-owned default for dry-run sync. CLI flag may override. |
| `log_level` | `string` | `info` | `debug`, `info`, `warn`, `error` | `all CLI` | File-log verbosity. Console verbosity is owned by `--verbose`, `--debug`, and `--quiet`. |
| `log_file` | `string` | `""` | path string | `all CLI` | Empty disables file logging. A non-empty path enables dual-channel logging. |
| `log_format` | `string` | `auto` | `auto`, `json`, `text` | `all CLI` | Console log format. `auto` is human text: plain "Warning: …" lines at default verbosity, timestamped key=value with `--verbose`/`--debug`, and `<N>`-prefixed lines under the systemd journal. `text` forces key=value, `json` forces JSON. File logs are structured JSON. |
| `log_retention_days` | `int` | `30` | `>= 1` | `all CLI` | Log rotation retention window. |

`dry_run = true` is only a config-owned default for one-shot `sync`; it is not
a global app mode and it does not make watch mode dry-run capable. When the
effective one-shot value is true, the CLI follows the same bootstrap contract
as any other one-shot sync: sync-bootstrap reconciliation, token refresh
persistence, log-file open/create, and control-socket ownership still happen.
Dry-run changes only the sync execution/commit semantics after setup.

### Per-Drive Fields

| Key | Type | Default / effective default | Valid range / shape | Command class | Notes |
| --- | --- | --- | --- | --- | --- |
| `sync_dir` | `string` | computed by `DefaultSyncDir()` when omitted | absolute or `~/...`; if present, must be a directory | `sync` | Login and `drive add` materialize the configured path. Sync does not create a missing path; engine startup reports that as a per-mount failure. File-operation commands require only the section, not this field. |
| `paused` | `bool?` | unset / inherited as not paused | boolean when present | `sync`, `pause`, `resume`, `status`, `drive list` | `Drive.IsPaused()` is the single source of truth. |
| `paused_until` | `string?` | empty | RFC3339 timestamp when present | `sync`, `pause`, `resume`, `status`, `drive list` | Timed pause expiry owned by config resolution. |
| `display_name` | `string` | derived by `DefaultDisplayName()` when omitted | string | `display`, selector matching | Human-facing label for status and drive selection. |
| `owner` | `string` | empty | string | `display` | Shared-drive owner label. |
| `ignored_dirs` | `[]string` | empty | normalized root-relative directory paths; no absolute paths, `..`, root, or globs | `sync` | Excludes exact directory subtrees from local and remote sync visibility. |
| `included_dirs` | `[]string` | empty | normalized root-relative directory paths; no absolute paths, `..`, root, or globs | `sync` | Empty means whole drive. Non-empty narrows sync to listed directories, descendants, and required ancestor containers. |
| `ignored_paths` | `[]string` | empty | normalized root-relative slash globs; no absolute paths, `..`, or root pattern | `sync` | Excludes matching files or directories from local and remote sync visibility; a directory match excludes its subtree. |
| `ignore_dotfiles` | `bool` | `false` | boolean | `sync` | Excludes any path with a component beginning with `.`. |
| `ignore_junk_files` | `bool` | `true` | boolean | `sync` | Excludes bundled OS/editor/IDE/browser junk, Office `~$` owner files and SQLite sidecars from local and remote sync visibility (R-2.4.14). On unless set to `false` (P-2); stored as `*bool` so an explicit `false` survives (`DriveFilterConfig.JunkFilesIgnored`). |
| `follow_symlinks` | `bool` | `false` | boolean | `sync` | When false, local symlinks are ignored. When true, local observation may project symlink target content with cycle and delete-safety rules. |
| `cross_filesystems` | `bool` | `false` | boolean | `sync` | When false, a folder inside the sync folder that is a mount point for another filesystem is not crossed: it is recorded as a `local_mount_boundary` read boundary, so a disk that is not mounted never reads as deleted files (P-11). When true, local observation descends into it like any folder. |

## Config File Manipulation

The config file is read with a TOML parser (`BurntSushi/toml`) but written with line-based text edits (`toml_lines.go`). This preserves all comments — both the initial defaults template and user additions. No TOML round-trip serialization. Every drive-section value is written as the kind the drive-key schema (`newDriveKeyKinds`, `unknown.go`, held to the `Drive` struct by a schema test) gives its key: a bool key takes only `true` or `false`, written bare; a string key is always a TOML basic string (`tomlString`), so a display name `true` stays a name; a list key cannot be set to one value; and a key the schema does not have is refused, leaving the file as it was. Section headers are written and read by one pair, `renderSectionHeaderLine` and `parseSectionHeader`, as TOML strings with their escapes undone, so a quote or a backslash in a SharePoint library's name finds its section again; a header may also be a literal string or carry a trailing comment.

The first-login template is generated from the same default constants the
runtime uses, so commented-out defaults stay aligned with the real schema.

All authoritative config/account/drive-metadata writes go through
`fsroot.Root.AtomicWrite`. The config package does not leave temp-file and
rename choreography to callers. The file is written `0600`
(`configFilePermissions`): its `[personal:<email>]` section keys name the
signed-in accounts, and the threat model counts other local users reading
managed files as an actor. A folder these writes create (the config folder,
the data folder the catalog lives in, any missing one above them) is `0700`
(`configDirPermissions`), as the XDG base directory rule asks of a directory
created where none existed; it used to be `0755`, which let every account on
the machine list the folder that names the signed-in accounts. A folder that
already exists keeps its mode (`TestAtomicWriteFile_CreatesItsFoldersOwnerOnly`).

Section-header rewrites are also config-owned. `RenameDriveSections()` edits
only the quoted section name, leaving comments, key order, and section body
text intact. Email reconciliation uses this instead of re-encoding TOML. A
section whose old name is gone is renamed already, and with nothing to
rename the file is not rewritten
(`TestRenameDriveSections_NothingToRenameLeavesTheFileAlone`); a section at
both names is a `driveSectionCollisionError` that names both, and the file is
left as it was (`TestRenameDriveSections_Collision`).

## Drive Sections

Implements: R-3.4.1 [verified], R-3.4.3 [verified]

Each drive section contains per-drive settings (`sync_dir`, paused state,
display metadata, and sync visibility filters). When a drive section omits `sync_dir`,
`buildResolvedDrive` computes the deterministic default local path for that
canonical ID before command-specific validation runs. Drive resolution
(`ResolveDrive`) matches by exact canonical ID → exact display_name
(case-insensitive) → substring. Ambiguous matches produce an error with
suggestions. `ResolveDrive()` returns both `*ResolvedDrive` and `*Config` —
the raw config is needed by shared drive token resolution.

For `shared:email:sourceDriveID:sourceItemID` drives, `buildResolvedDrive`
always preserves `RemoteRootItemID` from the canonical ID even when `DriveID` is
resolved from a catalog drive record. Catalog-backed shared drives therefore
keep the configured mount-root observation boundary instead of silently
falling back to whole-drive observation.

`buildResolvedDrive` also resolves `ResolvedDrive.RemoteRootDeltaCapable`.
That boolean is derived once from token-owner identity facts through the shared
helper `RemoteRootDeltaCapableForTokenOwner(...)`:

- personal owner account -> folder delta capable
- business/sharepoint owner account -> enumerate only
- no drive record, or one without an owner -> default to capable and let
  runtime fallback prove otherwise (the record's owner is read through
  `CatalogDrive.Owner`; a catalog that was read or written cannot hold one
  that is not a drive ID)

The sync engine consumes that resolved boolean directly for configured
standalone drives, while managed child mounts derive the same capability from
their explicit token-owner identity. Runtime startup does not reload catalog
metadata just to rediscover mount-root delta support.

`websocket` is a live watch-mode control. When `websocket = true`, watch mode
fetches a OneDrive Socket.IO endpoint and establishes an outbound websocket
wake source. The wake source does not replace delta: it only interrupts the
normal wait so the remote observer runs delta sooner. `poll_interval` remains
the fallback poll cadence even when websocket is enabled.

### Pause State

`Drive.IsPaused(now time.Time) bool` is the single source of truth for whether a drive is currently paused. All callers (CLI commands, the multi-drive control plane, drive resolution) use this method instead of checking `Paused`/`PausedUntil` fields directly. Logic: nil/false → not paused; true without expiry → indefinite; true with future expiry → active; true with past expiry → expired (not paused); true with unparseable expiry → indefinite (safe default).

`ResolvedDrive.Paused` is expiry-aware: `buildResolvedDrive` calls `IsPaused(time.Now())`, so `ResolveDrives(includePaused=false)` correctly includes drives with expired timed pauses.

`ClearExpiredPauses(cfgPath, cfg, now, logger)` is config-level housekeeping: removes stale `paused`/`paused_until` keys from config for drives whose timed pause has expired. Updates both in-memory config and on-disk file. Called by the control-plane reload path before resolving drives. Each key delete waits at most the bounded config-lock wait; when it fails, including with `ErrConfigLocked`, the pause still ends in memory, a Warn names the file that still lists it, and the next reload tries the delete again.

Default sync directories are computed deterministically from the canonical ID + cached metadata using a two-level collision scheme: base name → + display_name → + email. `DefaultSyncDir()` is the single entry point.

## Token Resolution

`DriveTokenPath()` determines which OAuth token file a drive uses. Shared and SharePoint drives share their account's primary token. Resolution uses catalog ownership for shared drives and canonical account type rules for the other drive types — this is business logic in `config`, not identity logic in `driveid`.

`TokenAccountCanonicalID()` is the authoritative token-owner lookup for
runtime callers that need the owning account identity itself instead of only
the token file path. SharePoint resolves to the business account with the same
email. Shared drives resolve through catalog ownership, not by scanning config
or legacy metadata files.

## Email Reconciliation

Implements: R-3.7.1 [verified], R-3.7.2 [verified], R-3.7.3 [verified]

`config.ReconcileAccountEmail()` owns durable email-change mutation. Given the
current authenticated account type, stable Graph user GUID, and current Graph
email, it:

- finds stored personal/business catalog accounts with the same `UserID`
- computes exact old → new canonical-ID rewrites
- refuses the change while any new name is taken (a token or state DB file,
  a config section), naming what to remove, before anything is renamed or
  recorded
- records the whole change in a journal, then runs four steps in this order:
  1. moves owned state DBs (`state_*`), through the `StateDBMover` its caller
     passes in
  2. renames owned token files (`token_*`)
  3. rewrites config section headers in place through `RenameDriveSections()`
  4. rewrites catalog account IDs and the drives those accounts own, in one
     catalog write; a drive of any other account keeps its ID, even one under
     the same old address with another account type
     (`TestReconcileAccountEmail_LeavesAnotherAccountsCatalogDrivesAlone`).
     A re-keyed drive's stored name follows the address only when it is the
     default derived from the old ID; a name Graph or the user supplied stays
     (`TestReconcileAccountEmail_CatalogDefaultDriveNameFollowsTheAddress`)

A state DB is never renamed here. It moves with its whole file family under
the store lock that excludes every engine using it, which `internal/sync`
owns (`syncengine.MoveStateDBs`) and config cannot import, so the CLI passes
that mover in. The mover takes every source's and target's lock before it
moves anything, and the state DBs move before any token, section or catalog
record: a store a sync still has open refuses the whole change and every old
name stays, consistent for the next try (R-3.7.2,
`TestReconcileAccountEmail_AStateDBInUseChangesNothing`,
`TestReconcileGraphUser_RefusesWhileASyncUsesTheState`). Renaming the
database file alone under a live engine left its log behind under the old
name.

Ownership stays in `config` because the package already owns canonical-ID to
path mapping, token-owner resolution, and text-level config mutation.

Config also owns the state DB file family, `StateDBFamily` (the `-wal`,
`-shm` and `-journal` sidecars, then the database), beside
`DriveStatePath`/`DriveStatePathForDataDir` and `MountStatePathForDataDir`;
`internal/sync`, `internal/multisync` and the CLI delete and move stores by
it. `DiscoverStateDBsForEmail` finds a store by any file of its family and
returns it once, by its database path, whether or not the database file is
there, so a log or journal that outlived its database (the reset and the
purge used to leave the journal) is retained state `logout --purge` deletes
(`TestDiscoverStateDBsForEmailIn_FindsAStoreByAnyFileOfItsFamily`,
`TestLogoutPurge_RemovesALoneSidecarOfTheAccount`). The email rename plans
such a name too, and `MoveStateDBs` deletes its files under the store lock
instead of moving them: sidecars without their database are not a store, and
left where they are they would sit under an email no account has any more,
where no command looks
(`TestMoveStateDBs_RemovesTheLeftoversOfAStoreWithoutItsDatabase`,
`TestMoveStateDBs_KeepsLeftoversWhileTheirStoreIsLocked`).

### The change is journaled and rolled forward

No single write covers files in the data directory, sections of the config
file and records of the catalog, so the change is journaled
(`email_change_journal.go`). Before the first rename the whole list of
renames is written to `<data dir>/email-change/journal.json` (atomic write,
mode 0600): the account and drive ID renames, the absolute path of the config
file the change started with, and every state DB and token move by its file
name in the data directory. A name with a separator is refused when the
journal is built and again when it is read, so a journal renames nothing
outside the data directory (the token rename used to go by the target's base
name inside the source's folder, so a target in another folder landed beside
its source, `TestDataDirFileName_RefusesAFileOutsideTheDataDir`). The whole
list is recorded because a plan rebuilt after an interruption would see
half-renamed inputs. The journal is read strictly: an unknown field, data
after it, another `schema_version`, an empty or self-renaming entry, or two
renames that meet are an error that names the file, and the journal is kept
(`TestDecodeEmailChangeJournal_RefusesWhatItCannotFinishExactly`,
`TestEmailChangeJournal_AJournalThatCannotBeReadFailsClosed`).

The journal's folder is also the lock: every writer and finisher of a
journal holds an exclusive lock on `<data dir>/email-change/` with the
bounded wait of every config change, so one email change runs per data
directory. Lock order: that lock, then the store locks the mover takes, then
the config and catalog locks the later steps take; never the reverse.

The mover writes the journal itself, through the `begin` callback of
`StateDBMover`: once it holds every source and target store lock, after its
checks pass and before its first rename. So a store in use refuses with no
journal and nothing renamed, and a change that cannot be recorded renames
nothing (`TestReconcileAccountEmail_RecordsTheWholeChangeBeforeTheFirstRename`,
`TestReconcileAccountEmail_RefusesAMoverThatDidNotRecordTheChange`). The
steps then run in order, and every step is idempotent: a source that is gone
is done, and a source and a target that both exist is the only refusal. The
data directory is synced, and the journal deleted, after the last step.

The invariant behind every refusal: with no journal, nothing of a change is
at a new name, so a file or section at a new name was there first and the
change is refused naming it
(`TestReconcileAccountEmail_CollisionFailsWithoutMutation`,
`TestReconcileAccountEmail_ConfigSectionsAtBothNamesRefuseTheChange`); with a
journal, everything at a new name was put there by the change, so a file or
section at both names was created under the old name afterwards, and the
refusal names that one to remove and keeps both
(`TestCompleteEmailChange_ATokenAtBothNamesSaysWhichToRemove`,
`TestCompleteEmailChange_AConfigSectionAtBothNamesSaysWhichToRemove`,
`TestMoveStateDBs_AResumedMoveWithADatabaseAtBothNamesSaysWhichToRemove`).

| Interrupted after | On disk | The next command |
| --- | --- | --- |
| the checks, target sidecars cleared | every name old, no journal | plans afresh |
| the journal was written | journal; every name old | runs every step |
| some state DBs moved, or one store's sidecars but not its database | journal; each store whole at the old name, whole at the new, or split | finishes each store without deleting at its target, then the rest |
| the state DBs | journal; stores new; tokens, sections, catalog old | tokens, sections, catalog |
| the tokens | journal; stores, tokens new | sections, catalog |
| the config sections | journal; only the catalog old | catalog |
| the catalog | journal; every name new | deletes the journal |
| the journal was deleted | every name new, no journal | nothing |

`TestReconcileAccountEmail_AFailureAfterAnyStepIsFinishedByTheNextCommand`
stops a change at every one of these boundaries, from the record to the
catalog, and between two state DB moves, and shows the next command finishes
it with every name new and each new name holding what the old one held; a
change stopped before it was recorded leaves nothing to finish and is planned
afresh (`TestReconcileAccountEmail_AChangeStoppedBeforeItWasRecordedStartsAfresh`).

**Where a change is finished.** `CompleteEmailChange` finishes a journal
it finds and reports its renames. The CLI calls it once at its entry, in
`initializeCLIContext`, before any command resolves its config (cli.md),
never inside `config.Load`: completing needs the state DB mover of
`internal/sync`, and a config read that renames files is a side effect no
reader expects. With no journal it is one stat: no lock is taken and nothing
is created (`TestCompleteEmailChange_WithNoJournalTouchesNothing`).
`ReconcileAccountEmail` also finishes a journal it finds, under the lock,
before it plans (`TestReconcileAccountEmail_FinishesAnInterruptedChangeItFinds`).
An engine neither waits for nor finishes a change: it refuses to start on a
store a journal names, at either name (`UnfinishedEmailChangeStateDBs`,
sync-engine.md).

The reconciliation planner is conservative about authority:

- personal/business ownership comes from the canonical-ID type
- SharePoint ownership comes from token-owner resolution to the business account
- shared-drive ownership comes from the managed catalog, not by guessing

### Catalog JSON Policy

`catalog.json` is the durable authority for accounts and drives, so decode is
strict:

- unknown JSON fields are rejected
- `schema_version` is required
- only the exact supported version is accepted
- the decoder must reach EOF after the first top-level object, so trailing JSON
  or garbage is rejected
- every account and drive key must be a canonical drive ID exactly as
  `CanonicalID.String` renders it (a SharePoint ID with an empty site or
  library, or a trailing colon, parses but renders differently, and every
  lookup goes through the rendering), each record's `canonical_id` must equal
  its key, and a drive's `owner_account_canonical_id` and an account's
  `primary_drive_canonical_id` must be rendered canonical drive IDs when set
  (`checkCanonicalIDs`). One that is not refuses the whole catalog, with an
  error naming the file, the record and the field and saying to correct or
  remove that entry, or remove the file and run `onedrive-go login` for each
  account. Writing runs the same check: a write that would store such a value
  fails with that error and leaves the file as it was, so a catalog on disk
  holds one only when it was edited by hand or written by another program.
  Readers parse a record's ID and its references in one place
  (`CatalogAccount.ID`, `CatalogAccount.PrimaryDrive`, `CatalogDrive.Owner`)
  and never meet one they would have to skip
- every save is a full atomic rewrite of the file
- every read-modify-write (`UpdateCatalog`, `UpdateCatalogForDataDir`, and so
  every catalog writer in the CLI and the running sync) loads, updates and
  saves under the bounded lock on the catalog's directory (see Auto-Creation);
  drive enrollment (`AddDrive`) writes the catalog under the same lock, which
  it holds inside the config lock,
  so a CLI command and a running sync updating the catalog together cannot
  lose either update; reads take no lock, since the atomic rewrite means a
  reader always sees one whole version

Runtime callers do not silently accept future or partially shaped catalog
files. A malformed or unsupported catalog is treated as an actionable local
state error that must be repaired explicitly.

`MountStatePath(mountID)` gives each managed child mount a stable retained-state
DB path independent of configured drive canonical IDs. Configured standalone
drives still use `DriveStatePath(...)`; managed child mounts now own their own
durable store namespace instead of pretending to be synthetic configured drives.
The filename uses a fixed-length SHA-256 digest of the managed mount ID encoded
with raw URL base64 under the `state_mount_*.db` prefix. That keeps every
managed child state filename below common filesystem basename limits while
preserving collision-resistant separation for IDs that differ only by path or
punctuation characters.

Automatic child mount IDs are stable across placeholder rename or move because
they are derived from `(NamespaceID, BindingItemID)`, not from the local
projection path or the content-root identity. A shortcut can therefore move
inside the parent namespace without losing its retained child mount state DB.
Parent engines own the authoritative parent protected-path state in
`shortcut_roots`; config only derives the child state DB path from the stable
mount ID.

When multisync releases a managed shortcut child, config provides only the
stable state path, wired once into multisync's child artifact cleanup
executor. The parent snapshot supplies the exact child
mount ID and local-root cleanup scope; multisync must not reconstruct that
scope from parent namespace facts. The control plane owns deleting child-owned
artifacts: the `state_mount_*.db` SQLite file family and upload sessions
tagged with the child mount scope. This purge is guarded by the child-mount ID
shape (`parent|binding:<id>`) so parent drive state is not removed by shortcut
lifecycle cleanup. A child mount has no catalog record: the catalog holds only
canonical drive IDs, which a child mount ID is not.

## Optional Catalog Fields

Catalog-backed account and drive records may omit cached presentation fields
such as display name, org name, remote drive ID, or shared-owner identity.
Missing optional catalog fields are normal input, not exceptional control flow.

Callers branch on the presence of the specific catalog field they need instead
of treating absent optional state as a load failure.

## Validation

Implements: R-4.8.1 [verified], R-4.8.2 [verified], R-4.8.3 [verified], R-6.4.3 [verified]

Unknown config keys are fatal errors (`unknown.go`). There are no delete-threshold keys: the delete-safety hold uses fixed policy (`TestLoad_UnknownKey_DeleteSafetyThresholdsAreNotConfiguration`). Per-drive validation checks sync_dir, filter patterns, size parsing, and drive-specific constraints. Global validation checks log level, transfer workers, and safety thresholds. `checkSyncDirOverlap()` prevents overlapping sync directories using `filepath.Clean` + `strings.HasPrefix` with separator suffix. Called at both config load and control-plane startup.

`ValidateResolved` and `ValidateResolvedForSync` treat only `os.ErrNotExist` as an acceptable sync-dir stat result. If the local path is unreadable or the filesystem returns another error, validation fails instead of silently accepting a broken path.

### Validation Tiers [verified]

Implements: R-4.8.4 [verified]

Two loading paths: strict (`Load`/`LoadOrDefault`) for data commands, lenient (`LoadOrDefaultLenient`) for informational commands.

**Strict path** (default): unknown keys and validation errors are fatal. Used by `sync`, file operations, `drive add`, `drive remove`.

**Lenient path**: TOML syntax errors remain fatal (can't produce Config). Unknown keys, validation errors, and drive section issues (type mismatches, unknown drive keys) are collected as `ConfigWarning` values. Malformed drive sections are skipped — other drives remain usable. Used by `drive list`, `status`, `shared`.

Internal refactoring supports both paths cleanly: `collectUnknownGlobalKeyErrors`, `collectDriveUnknownKeyErrors`, and `collectValidationErrors` return `[]error` slices. The strict wrappers (`checkUnknownKeys`, `Validate`) join them into a single error. The lenient path converts them to warnings.

Strict and lenient loading also share one loader/decoder pipeline: `configLoader` owns managed-file reads and base TOML decode, while `driveSectionDecoder` owns the second pass that extracts and validates drive tables. That keeps strict and lenient behavior aligned instead of maintaining two separate decode implementations. The second pass decodes each drive table from the original document's `toml.Primitive` (`MetaData.PrimitiveDecode`) instead of re-encoding the table into a scratch buffer, so a type error reports the line in `config.toml` where the bad value is (R-4.1.5). Lenient-load warnings go through `LogWarnings(path, …)`, which puts the config path and the problem in the message itself.

`ClassifyLoadOutcome(err, warnings)` is the single config-owned translation
step into the shared failure model: fatal read/parse errors map to `fatal`,
successful loads with warnings map to `actionable`, and clean loads map to
`success`.

Every failure returned by the exported read, parse, validate and resolve
entrypoints (`Load`, `LoadOrDefault`, `LoadOrDefaultLenient`,
`Validate`, `ValidateResolved`, `ValidateResolvedForSync`, `ResolveDrive`,
`ResolveDrives`, `MatchDrive`, and the config-vs-catalog check in
`LoadValidatedState`) matches `ErrInvalidConfig` (`failure_class.go`). The tag
wraps the error without changing its text or hiding its chain, and is
idempotent across entrypoints that call each other. Catalog and token-file
failures are not tagged. The CLI exits 78 (EX_CONFIG) on it so a service
manager stops restarting a run that would fail the same way (R-6.6.31).

**Sync-specific validation** (`ValidateResolvedForSync`): enforces that the resolved `sync_dir` is set and absolute, and that an existing path is a directory. That validation happens after `buildResolvedDrive` has already applied either the explicit per-drive `sync_dir` or the deterministic runtime default for drives that omit it. Non-existent paths are accepted by validation because engine startup owns root availability and reports a missing root as a per-mount `ErrMountRootUnavailable`. Login and `drive add` own creating sync roots when they enroll or repair drives; one-shot sync, watch startup, dry-run, and reload never create them. The CLI compiles resolved drives into `multisync.StandaloneMountConfig`; malformed per-drive sync settings become mount startup failures instead of aborting the whole selection. File operations do not require an explicit `sync_dir` in config.

## Config Holder

`config.Holder` wraps `*Config` + immutable config path behind an `RWMutex`. Both `SessionRuntime` and `multisync.OrchestratorConfig` share the same `*Holder` instance. On control-socket reload, multisync reloads the config file, uses the CLI-supplied standalone-mount compiler, then one `holder.Update(newCfg)` call atomically updates config for all consumers.

## Runtime Ownership

`config` has one long-lived mutable runtime structure: `Holder`.

- `Holder.mu` guards exactly one field: the current `*Config` snapshot.
- `Holder.path` is immutable after construction and intentionally read without locking.
- Reload callers own goroutine lifetime outside the package. `config` exposes synchronous load/update operations only.
- The package owns no channels or timers. Control-socket request handling lives in the control plane; `config` only provides the atomic snapshot swap. The one wait it performs, the bounded config/catalog lock poll, is synchronous in the caller's goroutine.

## Auto-Creation

`login` creates the config file from a template string with all global settings as commented-out defaults. Drive sections are appended. Subsequent `login` calls add new drive sections without disturbing existing content, and repair an existing drive section that is missing `sync_dir` by writing the deterministic default. Config read-modify-write mutations run under an advisory `flock` on the config directory when that parent directory exists, while config creation paths create the parent directory before taking the same lock; concurrent processes must preserve all successfully appended sections. The lock is taken by polling a non-blocking `flock` every `configLockPollInterval` (50 ms) for at most `configLockTimeout` (10 s), never by a blocking `flock`: a stopped or stuck holder (SIGSTOP, a hung filesystem, a foreign program holding the lock) would otherwise hang every config-writing command, and the watch daemon's reload, for as long as it lived. When the wait runs out the change is not made, and the error wraps `ErrConfigLocked` and names the directory (R-4.4.4). No caller has a context to give, so the wait is bounded rather than cancelable. The catalog takes the same lock on its own directory, the data directory; on macOS that is also the config directory, so there the two are one lock. `flock` locks belong to an open file description and every acquisition opens the directory afresh, so a locked callback must never call another locking function: it would wait on its own caller and fail after the timeout. Every locked callback only reads and rewrites its own file, with one exception: drive enrollment (`AddDrive`, used by login and `drive add`) holds the config lock and, inside it, the catalog lock (`withConfigAndCatalogLocks`), always in that order and as one lock when both files are in one directory (see Persisted Facts And Lifecycle). Nothing takes them in the other order; email reconciliation takes them one after the other. An account email change holds one more lock around them, on its own folder `<data dir>/email-change/` (a directory no other lock names), and takes it first, so the nesting cannot wait on itself. Enrollment reads account names from the catalog snapshot it read under the lock, never token files.

Removing a drive or logging out an account deletes the relevant drive sections
through `DeleteDriveSection()`, but it does not delete `config.toml` itself.

When no config file exists, `DiscoverTokens()` scans the data dir for `token_*.json` files and extracts canonical IDs from filenames. One token → auto-select. Multiple → prompt with list.

Discovery (`discovery.go`: `DiscoverTokens`, `DiscoverStateDBsForEmail`)
distinguishes a data directory that does not exist yet, which has nothing in
it, from one that cannot be listed, which is reported once at Warn and
returned as an error naming the directory. Callers never read that error as
"no accounts" or "no state databases": `MatchDrive` with no configured drives
says the data directory cannot be read instead of sending the user to
`login`, the CLI account snapshot fails with the same message, and email
reconciliation refuses to plan renames it could not enumerate.

### Rationale

- **Flat structure** (no `[filter]` sub-sections): simpler to read/write/manipulate. Drive sections are the only nesting.
- **Text-level writes**: TOML libraries strip comments on round-trip. Users care about their comments.
- **Global defaults with per-drive overrides**: shared defaults keep common policy centralized, while drive sections can still diverge when a specific library or account needs different exclusions.
- **Fatal on unknown keys**: prevents silent misconfiguration from typos.
