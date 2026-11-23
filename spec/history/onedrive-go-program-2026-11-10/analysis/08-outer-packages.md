# 08 — Packages outside `internal/sync`

Repository `~/src/onedrive-go`, branch `main`, commit `70301c0e`. Read-only analysis; no test, build or devtool command was run, nothing was executed, no network.

**Citations.** Paths are relative to the repository root unless they start with `/`. Two external trees are cited by absolute path: the pinned Go toolchain (`~/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.10.linux-amd64/src`, shortened to `$GOROOT`) and the OAuth fork (`~/go/pkg/mod/github.com/tonimelisma/oauth2@v0.0.0-20260209071456-249baba404ab`, shortened to `$OAUTH2`).

**Markers.** VERIFIED = I read the code that proves it. SUSPECTED = not proven; the sentence says what would settle it.

**Repo rules every proposal below has to satisfy** (`CLAUDE.md` of the repo; `spec/design/system.md:405-421` for what `devtool verify` checks): no backwards-compatibility code; bounded fan-out; every goroutine has an owner; atomic writes; errors never swallowed; tests first with `// Validates: R-x.y.z`; no `time.Sleep` in tests; failure injection on I/O paths; every production `.go` file is named by exactly one design doc `GOVERNS:` line; `forbidigo` bans raw `os.*` filesystem calls outside `fsroot`, `synctree`, `localpath` (`spec/design/system.md:237`).

`GOVERNS` lines that decide whether a new file needs a doc edit (all VERIFIED):

| Doc | Line | Form | Consequence |
|---|---|---|---|
| `spec/design/sync-control-plane.md` | 3 | glob `internal/multisync/*.go` | Item 6 split needs no edit |
| `spec/design/cli.md` | 3 | glob `internal/cli/*.go` | new cli files need no edit |
| `spec/design/drive-transfers.md` | 3 | explicit driveops file list | Item 1 files must be added |
| `spec/design/config.md` | 3 | explicit file list | Item 2 file must be added |
| `spec/design/graph-client.md` | 3 | explicit list incl. `internal/tokenfile/*`, `internal/graphtransport/*` | Item 9 and 11 files must be added |
| `spec/design/retry.md` | 3 | explicit list | Item 3 file must be added |

## Verdicts at a glance

| # | Item | Verdict |
|---|---|---|
| 1 | Recursive `get`/`put` | Defect real for `get` (goroutine per file) and worse for shared-folder `get` (goroutine per directory too, silent drop on cancel). `put` is serial; its costs are a double walk and unbounded maps. |
| 2 | `drive add` | Real. Two different step orders, three separate durable writes, an unlocked catalog read-modify-write, an existence check outside the config lock. Several kill points leave states a re-run does not repair. |
| 3 | Retry-After | Real. Two byte-identical parsers; neither accepts the HTTP-date form; no upper bound; integer overflow possible. |
| 4 | Upload-session shim | Real and isolated: one `UnmarshalJSON`, one unused version field, five tests. Other dead or compat-flavoured code listed. |
| 5 | JSON output | One helper plus 17 copies of the same encoder plus one NDJSON emitter that drops its error. Errors as text on stderr under `--json` is specified, not a defect. |
| 6 | `orchestrator_watch.go` | Split into 7 files, pure moves. |
| 7 | `fsroot` | Defect real: reads go through `os.Root`, every mutation uses a joined absolute path. The pinned toolchain has every `os.Root` method needed. |
| 8 | Non-rewindable bodies | Not a defect in the retry transport (it refuses, loudly). One latent silent path in `graph`'s own 401 retry, unreachable today. |
| 9 | Cross-process token refresh | No coordination exists (VERIFIED). Whether the service invalidates the old refresh token on rotation is SUSPECTED; the repo's own comment says it does. |
| 10 | "Four output leftovers" | The repo records no such list. What it does record is listed with IDs. |
| 11 | Other hangs and silent failures | Strongest: OAuth token-endpoint calls have no response timeout and run under a mutex every request of the account needs. |

---

## Item 1 — Recursive `get` and `put`

### Current behaviour

**Plain `get -r` — VERIFIED: one goroutine per file, created before the semaphore.** `internal/cli/get.go:315-332`:

```go
state.wg.Add(1)

go func() {
    defer state.wg.Done()

    // Acquire semaphore slot before starting the download.
    select {
    case state.sem <- struct{}{}:
    case <-ctx.Done():
        ...
        return
    }

    defer func() { <-state.sem }()
```

The semaphore (`defaultDownloadConcurrency = 4`, `internal/cli/get.go:25`) bounds running transfers, not goroutines. Directory descent is inline and does no network I/O because every listing was cached by the counting pass (`internal/cli/get.go:281-282`, comment `:288-299`), so the walker reaches every file of the tree almost at once and parks one goroutine per file on the semaphore.

**The counting pass holds the whole tree in memory — VERIFIED.** `countRemoteFiles` stores every folder listing in `state.childCache` (`internal/cli/get.go:242-263`, `state.childCache[remotePath] = children` at `:260`; type at `:172-185`) before the first download starts.

**Shared-folder `get` — VERIFIED: worse.** `internal/cli/get_shared.go:171-177` starts a goroutine per subdirectory, and `:180-181` one per file:

```go
if child.IsFolder {
    state.wg.Add(1)
    go func(child graph.Item, childLocalPath string) {
        defer state.wg.Done()
        downloadSharedRecursive(ctx, cc, tm, state, driveID, &child, childLocalPath)
    }(child, childLocalPath)
    continue
}
```

This is the shape `internal/cli/get.go:288-293` says was removed ("a goroutine per subdirectory made the goroutine count scale with the number of directories") and that `spec/design/drive-transfers.md:462-482` says does not exist. On cancellation the shared file goroutine returns without recording anything (`internal/cli/get_shared.go:184-189`), so a cancelled run reports fewer failures than files it skipped; plain `get` records the error (`internal/cli/get.go:324-329`). The error line format also differs (`internal/cli/get_shared.go:197` versus `internal/cli/get.go:340`).

**`put -r` — VERIFIED: not the goroutine shape.** `uploadFolder` is sequential (`internal/cli/put.go:200-260`). Its costs: two complete walks of the local tree (`countUploadFiles` `internal/cli/put.go:262-272`, then the upload walk through `walkUploadTree`/`walkUploadTreeEntry` `:274-318`), each directory read as one full slice (`localpath.ReadDir` at `:275` and `:304`), and `uploadWalkState.dirIDs map[string]string` growing with the number of directories (`internal/cli/put.go:193-198`).

**Remote names are joined into local paths unchecked — VERIFIED absent, exploitability SUSPECTED.** `childLocal := filepath.Join(localPath, child.Name)` (`internal/cli/get.go:313`; same at `:306` and `internal/cli/get_shared.go:169`). `DownloadToFile` rejects only an empty target and an empty item ID (`internal/driveops/transfer_download.go:261-267`). A grep of non-test code in `internal/graph`, `internal/cli/get.go`, `internal/cli/get_shared.go`, `internal/driveops/transfer_download.go`, `transfer_target.go` and `session.go` for `Separator`, `IsLocal`, `Valid*Name`, `Invalid*Name`, `Unsafe*Name` finds only `internal/cli/get.go:137` (a trailing-separator test on the user's target). Whether Graph can return a child named `..` or containing `/` cannot be proven from code; the client does not defend against it.

**The correct shape already exists in the repo — VERIFIED.** `internal/cli/shared_discovery.go:274-307` (`enrichSharedTargets`): `const enrichConcurrency = 5` (`:280`), the semaphore is taken before `go` (`:289-293`), then `wg.Add(1)` and the goroutine (`:295-303`), then `wg.Wait()` (`:306`).

### Failure scenario / cost

`get -r` of a folder with N files creates N goroutines, each holding a copy of its `graph.Item` and a stack, on top of the N cached items; the first byte is not downloaded until the whole tree has been listed. For shared folders add one goroutine per directory. This breaks the repo rule "Never let fan-out scale with input size". A shared `get -r` interrupted with Ctrl-C under-reports failures.

### Proposed end state

New files in `internal/driveops` (add all three to `spec/design/drive-transfers.md:3`):

```go
// tree.go
const DefaultTreeWorkers = 4

type TreeEventKind int

const (
    TreeFolderCreated TreeEventKind = iota + 1
    TreeFileDone
    TreeFileFailed   // transfer failed, name refused, or not started because of cancellation
    TreeFolderFailed // listing, local read or folder creation failed; siblings continue
)

type TreeEvent struct {
    Kind       TreeEventKind
    RemotePath string
    LocalPath  string
    Size       int64
    HashVerified bool
    Err        error
}

type TreeOptions struct {
    Workers int             // <= 0 means DefaultTreeWorkers
    OnEvent func(TreeEvent) // called from one goroutine, in completion order; may be nil
}

type TreeSummary struct {
    Files, Folders, Failed int
    Bytes                  int64
}
```

```go
// tree_download.go
// ChildLister returns one folder's children. The CLI adapts both existing
// listings: MountSession.ListChildren(ctx, remotePath) (session.go:513) and
// graph.Client.ListChildren(ctx, driveID, folderID) (used at cli/get_shared.go:127).
type ChildLister func(ctx context.Context, remotePath string, folder *graph.Item) ([]graph.Item, error)

func (tm *TransferManager) DownloadTree(
    ctx context.Context, list ChildLister, driveID driveid.ID,
    root *graph.Item, remotePath, localDir string, opts TreeOptions,
) (TreeSummary, error)
```

```go
// tree_upload.go
type FolderEnsurer interface {
    // Same contract as Session.EnsureFolder (session.go:882-904).
    EnsureFolder(ctx context.Context, parentID, name string) (*graph.Item, error)
}

func (tm *TransferManager) UploadTree(
    ctx context.Context, folders FolderEnsurer, driveID driveid.ID,
    localDir, remoteParentID string, opts TreeOptions,
) (TreeSummary, error)
```

(The return type of `EnsureFolder` was not re-read; match the existing method.)

Mechanics, identical for both directions:

- `Workers` goroutines range over `jobs chan treeJob` with capacity `Workers`. One collector goroutine drains `results chan TreeEvent` (capacity `Workers`), calls `OnEvent` and builds the summary. The function starts them and waits for all of them before returning: the owner is the call.
- The caller's goroutine is the only walker. Depth-first; per folder: list (or read) it, create it, send each file as a job with `select { case jobs <- j: case <-ctx.Done(): }`. A full channel blocks the walk: that is the back-pressure. At most `2 × Workers` files are in flight or queued, and at most one listing per level of depth is held.
- No counting pass, no `childCache`, no `dirIDs` map (the parent ID travels on the walk stack).
- Download refuses a child whose name is empty, `.` or `..`, contains a path separator, or fails `filepath.IsLocal`, as a `TreeFileFailed`/`TreeFolderFailed` event.
- Upload reads directories in batches (`ReadDir(n)` on an open handle) instead of one slice per directory. This needs a batch reader in `localpath` (`localpath.ReadDir` at `internal/localpath/localpath.go:234` returns the whole slice).
- Cancellation: the walker stops sending and closes `jobs`; every job still queued is reported as `TreeFileFailed` with `ctx.Err()`; the function returns `ctx.Err()` wrapped, with the summary so far. Nothing is dropped silently.
- A per-file failure is an event, not a return value. The returned error is for cancellation and for a root that cannot be listed or read.

`internal/cli` keeps: flag parsing, target resolution, building the `TransferManager` (the `min_free_space` parse duplicated at `internal/cli/get.go:95-100`, `:209-214` and `internal/cli/get_shared.go:213-224` becomes one helper), progress (`cc.Progressf`), text and JSON rendering from events, and `bulkTransferError` (`internal/cli/format.go:216-228`).

Deleted from `internal/cli`: `downloadState`, `countRemoteFiles`, `downloadRecursive` (`get.go:172-185`, `:242-355`), `countSharedFiles`, `downloadSharedRecursive` (`get_shared.go:126-211`), `uploadWalkState`, `countUploadFiles`, `walkUploadTree`, `walkUploadTreeEntry` (`put.go:193-198`, `:262-318`).

Two visible consequences, to be decided, not hidden:

1. Progress loses its total. Today it prints `Downloaded %d/%d files` (`internal/cli/get.go:352`). Without a counting pass it prints a running count. Keeping the total means a second listing of the whole tree; it must then keep only a counter.
2. `put -r` becomes parallel. Pass `Workers: 1` from `put` to keep today's serial behaviour, or accept the change.

### Existing tests

`internal/cli/get_test.go`: `TestJoinRemotePath`, `TestCountRemoteFiles_PopulatesCache`, `TestCountRemoteFiles_SurvivesSubdirError`, `TestDownloadRecursive_RespectsContextCancellation` (`:94-133`), `TestGetFolderJSONOutput_Serialization`, `TestGetFolderJSONOutput_EmptyErrors` (`:177-194`), `TestPrintGetJSON`, `TestPrintGetFolderJSON`, `TestDownloadRecursive_DirectoryDescentDoesNotFanOutGoroutines` (`:269-306`, `R-6.7.1`, helper `buildFolderOnlyTree` `:242-251`). No test covers the file fan-out or the shared-folder path's fan-out. `internal/cli/put_test.go:119`, `:144`.

### Tests to write first

`internal/driveops/tree_download_test.go` and `tree_upload_test.go`, with a fake `Downloader`/`Uploader` (interfaces at `internal/driveops/interfaces.go:25`, `:31`) that blocks on a gate channel and records the peak number of concurrent calls, and a generated `ChildLister`. `internal/driveops/test_helpers_test.go` exists but was not read; put the fakes there if it already holds transfer fakes.

- `TestDownloadTree_InFlightNeverExceedsWorkers` — 10,000 files in one folder, gate closed; assert peak calls equals `Workers`.
- `TestDownloadTree_WalkBlocksWhileWorkersAreBusy` — gate closed; assert the lister was not called for folders after the one holding job `2×Workers+1`.
- `TestDownloadTree_GoroutinesDoNotScaleWithFilesOrFolders` — same technique as `get_test.go:269-306`.
- `TestDownloadTree_CancellationReportsEveryUnstartedFile`.
- `TestDownloadTree_RefusesUnsafeRemoteNames` — `..`, `a/b`, empty.
- `TestDownloadTree_ListFailureIsReportedAndSiblingsContinue`; `TestDownloadTree_TransferFailureIsReportedAndOthersContinue`.
- `TestUploadTree_CreatesParentBeforeChildren`, `TestUploadTree_InFlightNeverExceedsWorkers`, `TestUploadTree_ReadDirFailureReported`, `TestUploadTree_CancellationReportsEveryUnstartedFile`.

In `internal/cli`: keep the serialization tests; replace the `countRemoteFiles`/`downloadRecursive` tests with one test per command that the events render to today's text and JSON; add a shared-folder `get -r` test (none exists for fan-out).

### Spec and requirement updates

`spec/design/drive-transfers.md:3` (GOVERNS) and `:462-482` (rewrite: the bound lives in `driveops`, one walker, bounded queue); `spec/design/cli.md:326-329` (`R-6.6.28`) and `:389-391` (`R-6.6.29`, progress text if the total goes); `R-6.7.1` stays the requirement the bound tests validate.

### Risks

Progress text change; `put` parallelism; the JSON folder document still accumulates one entry per file because `R-6.6.33` demands one document (`spec/design/cli.md:339-340`), so `--json` memory stays proportional to the file count by specification.

---

## Item 2 — `drive add` as one transaction

### Current behaviour (VERIFIED)

**Direct drive**, `addNewDriveWithOps` (`internal/cli/drive_add.go:155-217`):

1. token file must exist (`:168`);
2. snapshot of the prior catalog record (`:177`);
3. `config.EnsureDriveInConfigDetailed` — config write (`:182`);
4. only `if ensureResult.Added`: `ops.registerDrive` — catalog write (`:186-188`); on failure, config rollback (`:189`);
5. `materializeDriveSyncDir(syncDir)` (`:195`); on failure, config rollback, then catalog rollback only if the config rollback succeeded (`:196-206`).

**Shared drive**, `addSharedDriveWithOps` (`internal/cli/drive_add.go:361-444`): unlocked "already configured" check (`:371-379`); network call for the display name (`:407`); then catalog write **first** (`:422`), config write 1 `AppendDriveSection` (`:426`), config write 2 `SetDriveKey(..., "display_name", ...)` (`:432`), sync directory (`:437`). The order is the reverse of the direct path, and the config change is two separately locked writes.

**Rollbacks** are compare-and-delete helpers (`config.DeleteDriveSectionIfDriveEquals` `internal/config/write.go:428-473`, `DeleteDriveKeyIfDriveEquals` `:351-398`, `restoreDriveCatalogSnapshot` `internal/cli/drive_add.go:314-333`); their own failures are only logged (`internal/cli/drive_add.go:228`, `:307`, `:475`, `:490`).

**Config writes.** `EnsureDriveInConfigDetailed` loads the config, tests for the section and computes the collision-free `sync_dir` outside the lock; only the final append is locked (`internal/config/write.go:160-165`, `:189-194`). `appendDriveSectionSnapshot` appends without re-checking that the section is absent (`internal/config/write.go:106-130`). The lock is a blocking `syscall.Flock(LOCK_EX)` on the config directory's descriptor with no timeout and no context (`internal/config/write.go:770-806`, flock at `:795`), and when the directory is missing and `createDir` is false the mutation runs unlocked (`:783-785`).

**Catalog writes have no lock at all.** `internal/config/catalog.go:153-162`:

```go
func UpdateCatalogForDataDir(dataDir string, update func(*Catalog) error) error {
    catalog, err := LoadCatalogForDataDir(dataDir)
    ...
    if err := update(catalog); err != nil {
        return err
    }
    return SaveCatalogForDataDir(dataDir, catalog)
}
```

Callers include the daemon (`internal/multisync/shortcut_child_artifacts.go:487`) and the CLI (`internal/config/catalog_lifecycle.go:28`, `:83`, `:111`, `:140`, `:167`; `internal/config/validated_state.go:152`, `:172`; `internal/config/email_reconcile.go:367`; `internal/cli/auth_flow.go:239`; `internal/cli/drive_add.go:320`).

**Login has the same multi-step shape** (`internal/cli/auth_flow.go:36-110`): token move (`:62`), catalog (`:66`; a `RecordLogin` failure is only a warning, `:139-141`, and login still reports success), config (`:69`), sync directory (`:73`) with a three-part snapshot rollback (`:158-269`).

**The daemon sees intermediate states.** It polls `config.toml` every 5 s and reloads after two matching stamps (`internal/multisync/watch_config_file.go:16`, `:97-112`); the CLI notifies it only after the last step (`internal/cli/drive_add.go:123-134`).

### What a failure or a kill leaves behind (VERIFIED from the step order)

| Path | Kill between | Left behind | Does a re-run repair it? |
|---|---|---|---|
| direct | config write and catalog write (`:182` / `:188`) | section in `config.toml`, no catalog drive record | **No.** Re-run takes the "exists" branch, `Added` is false, `registerDrive` is skipped (`:186`). `UpsertDrive` is otherwise called only by login, reconcile, purge and rollback (`internal/config/catalog_lifecycle.go:65`, `:99`, `:129`, `:176`; `internal/config/email_reconcile.go:392`, `:398`; `internal/cli/auth_flow.go:247`; `internal/cli/drive_add.go:322`). |
| direct | catalog write and sync dir | no sync directory | Yes (`:195` runs regardless of `Added`). |
| shared | catalog write and config write 1 | catalog record without a section | Yes. |
| shared | config write 1 and 2 | section without `display_name` | **No.** Re-run returns at `:376-379`. |
| shared | config write 2 and sync dir | no sync directory | **No.** Same early return. |
| both | two processes adding the same drive | both pass the unlocked existence check and both append | Duplicate TOML table; strict load then fails. SUSPECTED on the exact error (settle: feed a duplicate table to `config.Load`). |
| any | daemon and CLI update the catalog together | one update lost | Silent. |

What a missing catalog drive record breaks downstream is SUSPECTED (settle by reading the readers of `Catalog.DriveByCanonicalID`).

On macOS the config and data directories are the same directory (`internal/config/paths.go:51` and `:73`); this matters for the lock design below.

### Proposed end state

New file `internal/config/drive_enroll.go` (add to `spec/design/config.md:3`):

```go
type AddDriveRequest struct {
    ConfigPath  string
    DataDir     string
    Drive       driveid.CanonicalID
    Owner       driveid.CanonicalID // zero for owned drives; the parent account for shared drives
    DisplayName string              // resolved by the caller before the transaction; "" means default
    MakeSyncDir func(path string) error // production: the body of cli/sync_dir.go:13
}

type AddDriveResult struct {
    Added       bool // this call wrote the section
    Repaired    bool // an existing enrollment was completed
    SyncDir     string
    DisplayName string
}

func AddDrive(req AddDriveRequest, logger *slog.Logger) (AddDriveResult, error)
```

One function, under one lock scope, in this order:

1. Take the config lock, then the catalog lock (new; flock on the data directory's descriptor, same technique as `write.go:781-795`). If both directories are the same file (`os.SameFile`), take one lock. Bounded wait: non-blocking flock retried on a timer up to a fixed limit, then an error naming the lock; clock and timer injected.
2. Read config and catalog under the locks. Decide: absent, complete, or incomplete (section without `sync_dir`, shared section without `display_name`, catalog record missing, directory missing).
3. Compute `sync_dir` from the snapshot read under the lock.
4. Create the sync directory. This is the only step that fails for outside reasons, and a leftover empty directory is harmless.
5. Write the catalog (idempotent upsert, always, also for an existing section).
6. Write the config **once**: a new section carries `sync_dir` and `display_name` together (`driveSection`, `internal/config/write.go:93-95`, gains the optional key); an incomplete section gets its missing keys in the same write. This is the commit point. `appendDriveSectionSnapshot` refuses a section that already exists.
7. If step 6 fails after step 5 changed the catalog, write the catalog snapshot from step 2 back. No comparison is needed: the lock is still held. A failure of that write is returned joined with the first error.

Result: a failure leaves config and catalog unchanged; a kill leaves at most an empty directory and a catalog record without a section, and running the command again completes it. A section without its catalog record or directory can no longer be produced, and if one exists the next run repairs it.

`UpdateCatalogForDataDir` takes the same catalog lock, which fixes the daemon/CLI lost update for every caller. `withConfigMutationLock` gets the bounded wait and stops running unlocked.

The CLI keeps: selector resolution, the network lookup of the display name (before the transaction), output, `notifyDaemonIfRunning`. Deleted from `internal/cli/drive_add.go`: `rollbackDriveAddConfig`, `restoreDriveAddConfigMutation`, `deleteDriveAdd*IfUnchanged`, `rollbackDriveAddCatalog`, `restoreDriveCatalogSnapshot`, `rollbackSharedDriveAdd`, `loadExistingCatalogDrive`, the `driveAddDurableOps` seam (`:136-146`). Deleted from `internal/config/write.go` once nothing calls them: `DeleteDriveKeyIfDriveEquals`, `DeleteDriveSectionIfDriveEquals`, the `BackfilledSyncDir`/`DriveBeforeSyncDirBackfill` result fields, `EnsureDriveInConfigDetailed` itself.

Login calls the same transaction with the account record added to step 5 (what `RecordLogin` writes, `internal/config/catalog_lifecycle.go:16-68`). Its snapshot machinery (`internal/cli/auth_flow.go:144-269`) shrinks to the token: install the token, run the transaction, and on failure put the previous token back. A catalog failure becomes an error instead of the warning at `auth_flow.go:139-141`.

### Existing tests

`internal/cli/drive_test.go`: `TestAddNewDrive_NoToken` `:947`, `_WithToken` `:961`, `_SyncDirCreateFailureRollsBackConfig` `:990`, `_CatalogMutationFailureRollsBackConfig` `:1013`, `_SyncDirCreateFailureRollsBackBackfilledSyncDir` `:1036`, `TestRestoreDriveAddConfigMutation_*` `:1063`, `:1087`, `:1110`, `TestAddSharedDrive_*` `:1299`, `:1326`, `:1342`, `:1369`, `:1407`, `TestRollbackSharedDriveAdd_*` `:1455`, `:1487`. `internal/cli/json_results_test.go:108` `TestDriveAdd_JSONReportsAddedThenAlreadyConfigured`. `internal/config/write_test.go`: `TestAppendDriveSection_ConcurrentCreatesPreserveAllSections` `:127`, `_CrossProcessCreatesPreserveAllSections` `:160` (with worker `:215`), `TestEnsureDriveInConfig_*` `:1362-1474`, `TestDeleteDriveKeyIfDriveEquals_*` `:593`, `:613`. `internal/config/catalog_lifecycle_test.go:60`, `:78`. No test covers a concurrent catalog update or two adds of the same drive.

### Tests to write first

`internal/config/drive_enroll_test.go`:

- `TestAddDrive_FailureAtEachStepLeavesConfigAndCatalogUnchanged` — table over injected failures of mkdir, catalog write, config write (injection through `configIO`, `internal/config/managed_io.go`, and `MakeSyncDir`).
- `TestAddDrive_RerunCompletesEachPartialState` — start from each row of the table above (section without catalog record; section without `display_name`; no directory), assert `Repaired` and the complete end state.
- `TestAddDrive_CrossProcessSameDriveAddsOneSection` — modelled on `write_test.go:160-229`.
- `TestAddDrive_SharedSectionIsOneWrite` — count writes through the `configIO` fake; assert the first written file already holds `display_name`.
- `TestAddDrive_SameDirectoryForConfigAndDataDoesNotSelfDeadlock`.
- `TestUpdateCatalog_CrossProcessUpdatesAreNotLost`.
- `TestConfigLock_WaitIsBounded` — injected clock, a held lock, no sleep.

The rollback tests in `internal/cli/drive_test.go` (`:990-1110`, `:1369-1487`) are deleted with the code they test; the CLI keeps the output tests.

### Spec and requirement updates

`R-3.3.5` (`spec/requirements/drive-management.md:42-53`) and `R-3.3.1` (`:27-34`) say drive add and login "roll back" config, catalog and token when the directory cannot be created; the text becomes "leave config and catalog unchanged" (and, for login, "restore the previous token"). `spec/design/config.md:448` (the paragraph describing `EnsureDriveInConfigDetailed`, the rollback-safe conditional deletes and "Shared drives bypass `EnsureDriveInConfig`"), `:50` (Verified By row naming the rollback tests), `:342-350` (catalog: add the lock). `spec/design/cli.md:20-28` already says inventory mutation is owned by `internal/config`; the code then matches it. `spec/design/drive-identity.md` governs `internal/cli/drive_*.go` (`:3`) and was not read.

### Risks

Lock order must be fixed everywhere (config, then catalog). The same-directory case on macOS deadlocks if two descriptors are flocked. A bounded lock wait turns a stuck holder into an error where today it is a hang; the limit needs choosing. Login's requirement text changes.

---

## Item 3 — One Retry-After parser

### Current behaviour (VERIFIED)

`internal/graph/client_preauth.go:205-221` and `internal/retry/transport.go:436-452` (`parseRetryAfterHeader`) have identical bodies:

```go
func parseRetryAfter(resp *http.Response) time.Duration {
    if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
        return 0
    }
    ra := resp.Header.Get("Retry-After")
    if ra == "" {
        return 0
    }
    seconds, err := strconv.Atoi(ra)
    if err != nil || seconds <= 0 {
        return 0
    }
    return time.Duration(seconds) * time.Second
}
```

- HTTP-date form: **not accepted**; `Atoi` fails and the result is 0, so the caller falls back to its own backoff (`internal/retry/transport.go:421`).
- Zero and negative: 0.
- Huge: no upper bound; `time.Duration(seconds) * time.Second` overflows `int64` above about 9.2e9 seconds and can turn negative.
- No whitespace trim. Only 429 and 503 are considered.
- Clock: `time.Now()` and `time.Until` are called directly where the value is used (`internal/retry/transport.go:406`, `:385`).

Callers: `parseRetryAfterHeader` from `internal/retry/transport.go:403`; `parseRetryAfter` from `internal/graph/client_preauth.go:33`, `:110` and `internal/graph/upload_transfer.go:226`. The value reaches sync through `GraphError.RetryAfter` (`internal/graph/errors.go:171-174`; consumers `internal/sync/domain_graph_error.go:35`, `internal/sync/scope_lifecycle_policy.go:88-93`), where it is used with no cap. The transfer stall watchdog deliberately does not run across retry waits (`internal/graph/transfer_watchdog.go:29-31`), so nothing else bounds an absurd value.

### Failure scenario

A server or proxy that answers 429 with `Retry-After: <HTTP-date>` is retried on the client's short backoff instead of the requested time. A value such as `Retry-After: 99999999999` yields an overflowed duration.

### Proposed end state

New file `internal/retry/retry_after.go` (add to `spec/design/retry.md:3`); `internal/graph` already imports `retry`:

```go
// MaxRetryAfter is a sanity ceiling, not a policy cap.
const MaxRetryAfter = time.Hour

type RetryAfter struct {
    Delay   time.Duration // 0: absent, malformed, zero, negative or already past
    Clamped bool          // the server asked for more than the limit
}

// ParseRetryAfter reads a Retry-After value in either form of RFC 9110
// section 10.2.3: delay-seconds or HTTP-date.
func ParseRetryAfter(value string, now time.Time, limit time.Duration) RetryAfter

// ResponseRetryAfter applies the status gate (429 and 503) and MaxRetryAfter.
func ResponseRetryAfter(resp *http.Response, now time.Time) RetryAfter
```

Rules: trim spaces; all-digit values parse with `strconv.ParseInt` and a range error or a value above `limit` returns `limit` with `Clamped`; otherwise `http.ParseTime`, delay = `t.Sub(now)`, non-positive gives 0, above `limit` clamps. Callers log once when `Clamped`.

Both old functions are deleted. `RetryTransport` gains `Now func() time.Time` (nil means `time.Now`) used at `transport.go:385`, `:403-418`. `graph.Client` gets the same injected clock for its three call sites (whether it already has one was not checked; settle by grepping `now` in `internal/graph/client_construction.go`).

The ceiling contradicts written requirements, so the owner has to choose it: `R-2.10.7` ("no cap — server is ground truth", `spec/requirements/sync.md:133`) and `R-2.10.14` ("honored exactly with no cap", `:140`). One hour is my proposal: a value above it is a fault or a clock error (the date form depends on the local clock), and clamping costs one early request that receives a fresh Retry-After.

### Existing tests

`internal/retry/transport_test.go:136` `TestRetryTransport_429RetryAfter`, `:559` `TestRetryTransport_RetryAfterHeader_503`, `:721` `TestRetryTransport_RetryAfterParsing` (`R-6.8.8`; rows `5`, `0`, `-1`, `abc`, empty), `:1122`; `internal/graph/client_test.go:399-428` `TestDo_RetryAfterHandling` (`R-6.8.1`), `:992` `TestTerminalError_RetryAfter`; `internal/graph/upload_test.go:1849` `TestSimpleUpload_RetryAfter503ResendsFullBody`. No test uses the date form or a huge value.

### Tests to write first

`internal/retry/retry_after_test.go`, table-driven with a fixed `now`: `5`, ` 5 `, `0`, `-1`, `abc`, empty, `99999999999999999999` (range error → limit, clamped), `9223372037` (duration overflow → limit, clamped), a date 90 s ahead in each of the three formats `http.ParseTime` accepts, a past date, a date a year ahead; `ResponseRetryAfter` on 200, 429, 503. Add a date-form row with injected `Now` to `TestRetryTransport_RetryAfterParsing` and to `TestDo_RetryAfterHandling`. Helpers to reuse: `roundTripFunc`, `makeResponse`, `testPolicy`, the `Sleep` seam.

### Spec and requirement updates

`spec/design/retry.md:3`, `:76`, `:185-186`; `spec/design/graph-client.md:490`, `:494-496`; `R-6.8.6` (`spec/requirements/non-functional.md:149`), `R-6.8.8` (`:151`); `R-2.10.7`, `R-2.10.14` (`spec/requirements/sync.md:133`, `:140`).

### Risks

Behaviour change where a date form was silently ignored (the wait gets longer and correct). The ceiling is a requirement change.

---

## Item 4 — Upload-session compat shim and other compat or dead code

### The shim (VERIFIED) — `internal/driveops/session_store.go`

```go
// v0: unversioned (no "version" key), uses "remote_path".
// v1: added "version" field, still uses "remote_path".
// v2: renamed JSON key "remote_path" → "local_path" (B-300).
const currentSessionVersion = 2          // :35-39

func (r *SessionRecord) UnmarshalJSON(data []byte) error {   // :64-88
    type alias SessionRecord
    var raw struct {
        alias
        RemotePath string `json:"remote_path"`
    }
    ...
    // If LocalPath is empty but RemotePath has a value, migrate from old key.
    if r.LocalPath == "" && raw.RemotePath != "" {
        r.LocalPath = raw.RemotePath
    }
```

Compat-only: the whole `UnmarshalJSON` (`:64-88`), the version-history comment (`:35-38`, `:41-42`), and the `ExpiresAt` comment "Zero for records written before it was kept" (`:53-56`). The `Version` field (`:44`) is written (`:188`) and never checked on load (`:109-179`); the load does not reject unknown fields.

Deleting it entails: remove `UnmarshalJSON`; make `Load` strict (`DisallowUnknownFields`, `Version != currentSessionVersion` treated like a corrupt file: the session is restarted); in `internal/driveops/session_store_test.go` delete `TestSessionStore_OldFormatVersionZero` (`:178`) and `TestSessionStore_V1ToV2Migration` (`:208`), rewrite `TestSessionStore_VersionField` (`:155`), `_V2RoundTrip` (`:236`), `_SaveWritesV2JSON` (`:263`) into one round-trip test, and add `TestSessionStore_Load_RejectsUnknownVersion` and `_RejectsUnknownField`. Update `spec/design/drive-transfers.md:313-323`. Cost to a user: an upload interrupted by the old binary restarts from zero once.

Same file, a silent skip: `CleanStale` ignores an entry whose `Info()` fails (`:332-335`); see Item 11.

### Other compat, legacy or dead code (VERIFIED unless marked)

| Where | What | Action |
|---|---|---|
| whole repo, non-test | no cobra `Aliases:`, `Deprecated`, `MarkDeprecated`, `MarkHidden`, `Hidden:` (grep empty) | none: there are no flag or command aliases |
| `internal/graph/delta.go:119-133` | `buildDeltaPath`, `buildFolderDeltaPath`, both "Kept for test compatibility"; called only from `delta_test.go:340-377` and `quirks_test.go:195`, `:205` | delete; tests call `resolveDeltaToken` (`:106-117`) |
| `internal/graph/items_permissions.go:125-127` | exported `HasWriteAccess`, called only from `fuzz_test.go:84` and `items_test.go:2421-2781` | delete or unexport into the tests |
| `internal/graph/items_permissions.go:74`, `:102`, `:152` | `permissionApplicabilityLegacy` — a live code path with a misleading name | rename |
| `internal/config/drive.go:391-397` | `applyDriveOverrides` is a no-op (`_ = resolved; _ = drive; _ = logger`), still called at `:248` | delete function and call |
| `internal/cli/status_snapshot.go:892-896` | dead `history bool` parameter (`_ = history`) | remove |
| `internal/cli/status_render_sections.go:8-9`, `:110-112`, `:114-117` | dead `history` parameter; `printMountSyncSections` pure delegate; `printConditionSection` ignores `nextIndent` | remove |
| `internal/cli/status_render.go:511-514` | `printSyncStateStoreLines` returns nil and does nothing | delete |
| `internal/config/discovery.go:16-17` | comment calls `cidFileSuffix` "legacy"; the constant is live (`:46`, `:54`) | fix the comment |
| `internal/config/write.go:165-186` | `sync_dir` backfill for a section that lacks it; `spec/design/config.md:448` specifies it as repair | folded into Item 2 |
| `internal/driveid/itemkey.go:19` | comment containing "legacy" — not read | SUSPECTED comment only |
| `internal/devtool/bench_live.go:404`, `:429` | not read | SUSPECTED |

No `--history` flag remains: `internal/cli/status.go` contains no "history" (grep empty), and devtool has `TestE2E_Status_NoLegacyHistorySurface` (`internal/devtool/verify_e2e.go:96`).

Not compat code: the sync-store generation check is refuse-and-reset (`internal/sync/schema.go:12`); the catalog refuses unknown schema versions (`internal/config/catalog.go:112-114`); `R-3.8` "migrate" is a `[future]` feature (`spec/requirements/drive-management.md:114-116`).

---

## Item 5 — One JSON encoder path

### Emission sites (VERIFIED)

**Helper**: `writeJSONDocument(w io.Writer, v any) error` (`internal/cli/json_output.go:15`): `json.NewEncoder`, `SetIndent("", "  ")`, `Encode`. Used by 14 sites: `sync_pause_resume.go:64`, `:142`, `:197`; `trash.go:149`, `:201`, `:226`; `drive_rebind_root.go:78`; `auth_flow.go:99`, `:366`; `recycle_bin_flow.go:115`, `:129`; `drive_remove.go:79`; `drive_reset_sync_state.go:115`; `drive_add.go:130`.

**Own copy of the same encoder** (17 sites): `drive_search.go:185`, `mkdir.go:101`, `cp.go:180`, `get.go:149`, `get.go:161`, `ls.go:108`, `drive_list_render.go:46`, `recycle_bin.go:137`, `recycle_bin.go:164`, `stat.go:110`, `mv.go:158`, `shared.go:107`, `perf.go:108`, `rm.go:209`, `status_render.go:84`, `put.go:170`, `put.go:182`.

**NDJSON for `sync --json`**: `internal/cli/sync_console_json.go:76-83`:

```go
func (c *syncConsole) emitJSON(event *syncJSONEvent) {
    line, err := json.Marshal(event)
    if err != nil {
        return
    }
    c.write(c.out, string(line))
}
```

`internal/cli/control_client.go:230` encodes a control-socket request; it is not output.

### Differences

| Aspect | Helper | 17 copies | `emitJSON` |
|---|---|---|---|
| Indent | two spaces | two spaces | none (one line) |
| Trailing newline | yes (`Encode`) | yes | added by `c.write` |
| HTML escaping | on (default) | on | on |
| Encode error | returned, wrapped `encode JSON output` | returned, wrap text differs per command | **dropped** (`:78-80`) |

So the bytes agree today; what differs is the number of places that must stay in agreement, the error text, and the dropped error. Two real output differences sit one level up:

- Empty lists: `drive_search.go:181-183` turns a nil slice into `[]`; the folder documents of `get` and `put` leave nil slices, which encode as `null` (`internal/cli/get.go:44-49`; test `TestGetFolderJSONOutput_EmptyErrors`, `internal/cli/get_test.go:177-194`, should be read to confirm which of the two it pins — SUSPECTED until then).
- `drive search` prints a partial-failure warning as text while in `--json` mode (`internal/cli/drive_search.go:62`); it goes to the status stream, which the contract permits.

**Error output under `--json`** (VERIFIED): there is no JSON error document. `mainWithWriters` → `writeCommandError(statusWriter, commandErrorText(...))` (`internal/cli/root.go:704-742`, `:639-648`; `internal/cli/error_presentation.go:251-285`) writes text to stderr and never looks at the `--json` flag. That is the specified contract: "Warnings, prompts … and errors stay on stderr" (`spec/design/cli.md:343-344`, `R-6.6.33` at `spec/requirements/non-functional.md:99`). It is not a defect; a JSON error document would be a requirement change.

### Proposed end state

`internal/cli/json_output.go` is the only place in `internal/cli` that encodes output:

```go
// One indented document, one trailing newline.
func writeJSONDocument(w io.Writer, v any) error

// One compact line, one trailing newline (sync --json).
func writeJSONLine(w io.Writer, v any) error
```

Both build the encoder in one unexported constructor, so indent and escaping are set once. Decide HTML escaping there, explicitly: keeping it on changes nothing; turning it off (`SetEscapeHTML(false)`) changes bytes only for names containing `&`, `<` or `>`, which today print as `&` and so on. I recommend off, since the consumers are scripts, not browsers.

The 17 `print*JSON` bodies shrink to building the value and calling `writeJSONDocument`. `emitJSON` calls `writeJSONLine` and reports a failure through the console's warning path instead of returning silently. Documents never contain `null` for a list: each output struct gets a constructor or a `normalize()` that replaces nil slices. A lint rule (`forbidigo`, like the `os.*` rule at `spec/design/system.md:237`) bans `json.NewEncoder` and `json.Marshal` in `internal/cli` outside `json_output.go` and `control_client.go`.

### Existing tests

`internal/cli/json_contract_test.go:62-91` `TestCommandTree_EveryLeafHonorsJSON` (`R-6.6.33`): every runnable leaf must declare `jsonDocument`, `jsonLines` or `jsonRejected` (`:34-53`) and its stdout must parse (`:93-137`). It checks validity, not bytes. `internal/cli/json_results_test.go` holds per-command results; goldens `TestStatusOutputGoldenText`, `TestStatusOutputGoldenJSON` (`spec/design/cli.md:39-40`).

### Tests to write first

- `internal/cli/json_output_test.go`: `TestWriteJSONDocument_Format` and `TestWriteJSONLine_Format`, byte-exact, including a value containing `&<>` and an encode failure (a channel value) returning an error.
- Extend `assertJSONContract` (`json_contract_test.go:93-109`): a `jsonDocument` must start with `{`, use two-space indentation, end with exactly one newline, and contain no `null` where the schema says array.
- `TestSyncConsole_EmitJSONFailureIsReported`.
- A test that runs the lint rule's pattern over `internal/cli` if the rule lives in devtool rather than golangci config.

### Spec and requirement updates

`spec/design/cli.md:339-340` currently allows "`writeJSONDocument` or the command's own printer": delete the alternative. Add the nil-list rule and the escaping decision to `cli.md:333-378`. `R-6.6.33` text needs no change unless escaping is changed.

### Risks

Golden files change if escaping is turned off. `null` → `[]` is a visible change for `get`/`put` folder documents.

---

## Item 6 — Splitting `internal/multisync/orchestrator_watch.go`

1,373 lines, read in full. Proposed files, pure moves within the package, no signature change, no GOVERNS edit (glob at `spec/design/sync-control-plane.md:3`). Line numbers are the current start of each declaration.

| New file | Responsibility | Declarations |
|---|---|---|
| `orchestrator_watch.go` (kept) | entry, main loop, startup | `watchRunnerEventBuffer` :20; `RunWatch` :47; `reloadForReason` :117; `startWatchRuntime` :222; `startInitialWatchRunners` :279; `validateInitialWatchStart` :341; `closeControlSocket` :355; `configPath` :1312; `mountIDsForSpecs` :1333; `sortedRunnerMountIDs` :1345; `setControlMountIDs` :1354; `controlMountIDs` :1364 |
| `watch_wake.go` | timers | `watchRestartWake` :131 (`arm` :137, `stop` :150); `watchPauseExpiryWake` :163 (`arm` :169, `stop` :182); `pauseExpirySlack` :191; `nextPauseExpiryDelay` :195; `reconcileWatchInterval` :1320 |
| `watch_runner.go` | one runner's life and its exit event | `watchRunner` :24; `watchRunnerEvent` :35; `closeEngine` :368; `startWatchRunner` :388; `runWatchRunnerEngine` :484; `registerMountPerfCollector` :511; `removeMountPerfCollector` :519; `handleWatchRunnerEvent` :647; `reportWatchRunnerExit` :700; `parentSnapshotEventIsCurrent` :727; `handleFinalDrainWatchRunnerEvent` :743; `watchParentDrainAckers` :785; `watchParentArtifactCleanupAckers` :799 |
| `watch_reload.go` | config reload | `reload` :532; `loadReloadSelection` :815 |
| `watch_reconcile.go` | applying a mount set | `reconcileWatchRunners` :597; `reconcileWarningKey` :618; `logReconcileOutcome` :624; `applyWatchMountSet` :840; `reportShortcutCleanup` :874; `reconcileWatchRunnersForParent` :883; `warnParentReconcileFailure` :942; `standaloneDisplayName` :952; `applyWatchMountSetForParent` :962; `runnableMountMap` :991; `startReloadWatchRunners` :1181; `sortedRunnableMountIDs` :1232 |
| `watch_runner_stop.go` | stopping runners | `stopInactiveWatchRunners` :1002; `removeChildRunnablesForParents` :1048; `removeCleanupWorkForParents` :1065; `parentWatchRunnersNeedingStop` :1082; `stopOrderForWatchRunners` :1099; `stopChildWatchRunnersForParent` :1117; `stopInactiveChildWatchRunnersForParent` :1145 |
| `watch_mount_equivalence.go` | pure comparison | `mountSpecsEquivalentForWatchRestart` :1250; `mountSpecCoreEquivalent` :1257; `mountSpecIdentityEquivalent` :1264; `mountSpecRemoteEquivalent` :1272; `mountSpecRuntimeEquivalent` :1279; `childEngineSpecsEquivalent` :1288; `mountSpecTuningEquivalent` :1295; `contentFilterConfigsEquivalent` :1301 |

Existing tests are white-box in the same package (`orchestrator_test.go`, `watch_lifecycle_test.go`, `watch_reporting_test.go`, `watch_pause_expiry_test.go`, `reload_log_level_test.go`) and need no change. No new tests: the check is that the diff contains only moved lines and that `spec/design/sync-control-plane.md` does not name the old file for a moved function (grep the doc for `orchestrator_watch.go` in the same PR).

For a later, separate PR (behaviour-neutral refactors, not moves), all VERIFIED: `watchParentDrainAckers` (`:785-797`) and `watchParentArtifactCleanupAckers` (`:799-813`) have identical bodies; the stop sequence "cancel, wait `done`, close engine, delete from map" is written out at `:1031-1035`, `:1136-1139`, `:1172-1175` and once without cancel at `:678-680`; `<-wr.done` waits with no bound.

---

## Item 7 — `fsroot` mutations and `os.Root`

### Toolchain

`go.mod` pins `go 1.26.0` and `toolchain go1.26.10`. `os.Root` in that toolchain (`$GOROOT/os/root.go`) has: `OpenInRoot` :25, `OpenRoot` :82, `Name` :90, `Close` :96, `Open` :102, `Create` :108, `OpenFile` :117, `OpenRoot` :132, `Chmod` :139, `Mkdir` :149, `MkdirAll` :161, `Chown` :170, `Lchown` :176, `Chtimes` :182, `Remove` :188, `RemoveAll` :194, `Stat` :200, `Lstat` :209, `Readlink` :216, `Rename` :223, `Link` :235, `Symlink` :247, `ReadFile` :253, `WriteFile` :264, `FS` :359. It has no `CreateTemp`, no no-replace rename and no directory-sync helper. The repo's own comment says the mutating methods arrived in 1.25 (`go.mod:5-9`).

### Current behaviour — the defect is real (VERIFIED)

`internal/fsroot/fsroot.go`: the `rootHandle` interface has only `Open`, `OpenFile`, `Stat`, `FS`, `Close` (`:16-22`). The default operations are plain `os` functions (`:70-90`):

```go
mkdirAll: os.MkdirAll,
remove:   os.Remove,
rename:   os.Rename,
createTemp: func(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) },
lstat:   os.Lstat,
syncDir: syncDirectory,
```

and the mutating methods join a path and call them (`:355-383`):

```go
func (r *Root) Remove(name string) error {
    path, err := r.pathFor(name)
    ...
    if err := r.ops.remove(path); err != nil {
```

| Operation | Lines | Through `os.Root`? |
|---|---|---|
| `Open`, `OpenFile`, `ReadFile`, `Stat`, `ReadDir` | 227-345 | yes |
| `MkdirAll` | 347-353 | no (`os.MkdirAll`) |
| `Remove` | 355-366 | no (`os.Remove`) |
| `Rename` | 368-383 | no (`os.Rename`) |
| `CreateTemp` / `createTempFile` | 385-430 | no (`os.CreateTemp`) |
| `AtomicWrite` (mkdir, temp, rename, dir sync, cleanup) | 432-498 | no |
| `syncDirectory` | 100-117 | no (`os.Open`) |
| `normalizeNotExist` | 520-530 | no (`os.Lstat`) |

`cleanRelative` (`:164-181`) is a lexical check, and names may contain subdirectories, so a symlinked directory below the root redirects remove, rename, temp-file creation and mkdir outside the root. The design doc claims otherwise: "open/read/temp-file creation/chmod/fsync/rename all stay inside one managed-state root capability" (`spec/design/graph-client.md:610`).

Severity, stated plainly: `fsroot` roots are the user's own config and data directories. Redirecting them requires write access to those directories, which already gives access to the token. The fix is about making the code match its documented guarantee and its sibling `synctree`, which does it right (`internal/synctree/synctree.go:29-42`, `:54-125`: every mutation delegates to `*os.Root`).

Production callers of `fsroot`: `internal/config/write.go:771`, `:813`; `internal/config/managed_io.go:32`, `:46`, `:60`; `internal/config/email_reconcile.go:562`; `internal/logfile/logfile.go:29`; `internal/tokenfile/tokenfile.go:39`, `:82`, `:101`; `internal/sync/store.go:100`; `internal/cli/cleanup.go:19`, `:41`, `:63`, `:74`, `:91`; `internal/driveops/session_store.go:111`, `:163`, `:183`, `:212`, `:234`, `:310`.

### Mutations inside the sync tree that bypass the rooted capability

Rooted, for contrast (VERIFIED): sync downloads write through `syncDownloadTarget{tree *synctree.Root}` (`internal/sync/executor_transfer.go:288-341`); file conflict copies use `e.syncTree.Rename`, `OpenFile`, `Remove` (`internal/sync/executor_conflict.go:78`, `:165`, `:152`).

Bypassing:

| Where | What | Status |
|---|---|---|
| `internal/sync/executor_conflict.go:299` | directory conflict copy: `localpath.RenameNoReplace(sourceAbs, candidate)` with absolute paths (`:289-325`) | VERIFIED. Inside `internal/sync`, so cross-reference only. `os.Root` has no no-replace rename; the fix is `renameat2` on a rooted parent descriptor, in `synctree`. |
| `internal/localtrash/localtrash.go:375-410` | `Restore`: a lexical symlink check (`refuseSymlinkedAncestors`, `:396`), then `localpath.MkdirAll` (`:399`), `moveNoReplace` (`:402`), and on a cross-device move `localpath.CreateTemp`, `Chtimes`, `RenameNoReplace`, `Remove` (`:454-460`, `:487-525`) — absolute paths into the sync folder | VERIFIED. Check-then-act: a symlink swapped in after the check redirects the restore. Existing test `TestTrash_RestoreRefusesSymlinkedAncestor`. |
| `internal/synctree/detach_unix.go:173`, `:400`, `:419`, `:422`, `:429`, `:447`, `:451` | plain `os.Remove`, `os.MkdirAll`, `os.CreateTemp`, `os.Rename` on the caller's `dest` of `DetachRegularFile`/`PreserveRegularFile` (`:103`, `:153`) | SUSPECTED outside the sync tree (the trash). Settle by reading both bodies and their callers. |
| `internal/cli/get.go:273`, `internal/cli/get_shared.go:155`, `internal/driveops/transfer_target.go:46-99` | CLI downloads write to arbitrary user paths through `localpath` | VERIFIED, by design: the target is whatever the user named, not the sync tree. |

Other `localpath` mutation callers work on managed or operational paths, not the sync tree: `internal/multisync/owner_lock.go:47`, `:109`; `internal/multisync/control_socket.go:90-593`; `internal/perf/capture.go:106-211`; `internal/cli/auth_login.go:283`, `:292`; `internal/cli/sync_dir.go:22`; `internal/sync/engine_root_lock.go:105`, `:108`; `internal/sync/store_reset.go:40`; `internal/sync/store_scratch.go:71`, `:79`; `internal/config/write.go:781`.

### Proposed end state

In `internal/fsroot/fsroot.go`:

```go
type rootHandle interface {
    Open(name string) (*os.File, error)
    OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
    Stat(name string) (os.FileInfo, error)
    Lstat(name string) (os.FileInfo, error)
    MkdirAll(name string, perm os.FileMode) error
    Remove(name string) error
    Rename(oldname, newname string) error
    FS() fs.FS
    Close() error
}
```

- `rootOps` keeps `openRoot`, `mkdirRoot` (the root directory itself is the anchor, so creating it stays `os.MkdirAll(r.dir)`; `Open` must keep working on a directory that does not exist yet, `:130-139`) and a `tempSuffix func() string` seam. The fields `remove`, `rename`, `createTemp`, `lstat`, `syncDir` are deleted; failure injection moves to a fake `rootHandle`.
- `Remove`, `Rename`: `root.Remove(rel)`, `root.Rename(relSrc, relDst)`.
- Temp files: `root.OpenFile(dir/prefix+suffix+tail, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)` in a bounded retry loop on `os.ErrExist`.
- Directory sync: `root.Open(dirRel)`, `Sync`, with today's tolerance for filesystems that cannot sync a directory (`:100-117`).
- `normalizeNotExist`: `root.Lstat`.
- Subdirectory creation inside the root: `root.MkdirAll(rel, perm)`.

### Existing tests

`internal/fsroot/fsroot_test.go`: `TestRoot_AtomicWrite_RejectsRootEscape` :70, `TestRoot_OpenAndOpenFile_RejectRootEscape` :185 (lexical escapes only), `TestRoot_FileLifecycleOperations` :85, `TestRoot_AtomicWrite_CleansTempFileWhenRenameFails` :202, `TestRoot_AtomicWrite_FailureInjection` :246 (uses the `rootOps` function fields; helpers `runAtomicWriteFailureCase` :325, `fakeTempFile` :377-397), `TestAtomicWriteSyncsTargetDirectory` :408, `TestAtomicWriteReportsDirectorySyncFailure` :436. No test puts a symlink under the root for a mutation.

### Tests to write first

In `internal/fsroot/fsroot_test.go`, modelled on `internal/synctree/symlink_containment_test.go`; each builds `root/sub -> outside` and asserts the outside tree is untouched and an error is returned:

- `TestRoot_Remove_RefusesSymlinkedParent`
- `TestRoot_Rename_RefusesSymlinkedParent` (source and destination)
- `TestRoot_AtomicWrite_RefusesSymlinkedParent`
- `TestRoot_CreateTemp_RefusesSymlinkedDir`
- `TestRoot_AtomicWrite_TempNameCollisionRetries` (the new suffix seam)

Rewrite `TestRoot_AtomicWrite_FailureInjection` onto a fake `rootHandle`.

### Spec updates

`spec/design/graph-client.md:610` becomes true; `spec/design/system.md:352-373`, `:426-438` (boundary description). No requirement ID found that states the containment guarantee for managed state; add one if the owner wants it enforced by a test name.

### Risks

A managed file that is itself a symlink to somewhere outside its directory already cannot be read (reads are rooted, `:227-280`), so no new restriction arises for reads; writes replace the link by rename in both versions. `Root.Rename` across two roots is not possible; all current renames are within one root (`AtomicWrite`, `:460`).

---

## Item 8 — Request bodies that cannot be rewound

### Verdict: the retry transport is loud (VERIFIED); one latent silent path in `graph`

Transport, `internal/retry/transport.go`:

- `requestBodyReplayable` (`:465-473`) is checked before any retry: on a network error (`:190`) and on a retryable response (`:259`, which returns the response as it is).
- `rewindBody` (`:480-506`) uses `GetBody`, else `Seek(0, io.SeekStart)`, else returns `errBodyNotReplayable` (`:457`).
- Covered by `R-6.8.19` (`spec/requirements/non-functional.md:162`) and `TestRetryTransport_NonReplayableBodyIsNotRetried`.

Senders give the transport what it needs: `doPreAuth` builds each attempt from `makeReq()` and sets `GetBody` (`internal/graph/client_preauth.go:62-76`); `UploadChunk` creates a fresh `io.NewSectionReader` per call (`internal/graph/upload_transfer.go:68-88`); `doRawUpload` sets `ContentLength` and `GetBody` (`:169-177`, `:239-245`).

**The latent silent path**: `graph`'s own `rewindBody` (`internal/graph/client_auth.go:299-314`) is documented "Returns nil when body is nil or not seekable"; `retryUnauthorized` calls it (`:280`) and then resends with the same `body` (`:284`). A non-seekable body would be resent already consumed, with no error. It cannot happen today: every caller of `c.do`/`doRequest`/`doRequestVia` passes nil, `http.NoBody` or a `bytes.Reader` (for example `internal/graph/items_permissions.go:282`, `:325`; `upload_session.go:47`; `item_create.go:78`; `items_copy.go:64`; `items_mutation.go:45`, `:223`, `:276`, `:409`).

### Fix

Make `graph`'s `rewindBody` return an error for a body that is non-nil, not `http.NoBody` and not an `io.Seeker`, and have `retryUnauthorized` return it wrapped with the original 401. Stronger alternative: change `doRequest*` to take `[]byte` so the non-rewindable case cannot be written. Test first in `internal/graph/client_test.go`: `TestDo_401WithNonSeekableBodyFailsLoudly` (an `io.Reader` wrapper without `Seek`, a 401 response, assert an error that names the body and that the server saw one request).

Side fact (VERIFIED, not silent): `doRawUpload` does not go through `doOnce` (`internal/graph/upload_transfer.go:179`, `:191`), so a 401 on the simple-upload request gets no refresh-and-retry and surfaces as a `GraphError`.

Spec: `spec/design/graph-client.md:526` (`R-6.8.17`), `:528-582` ("401 Token Refresh") gains one sentence.

---

## Item 9 — Token refresh across processes

### Current behaviour (VERIFIED)

- A token source is built by loading the file once (`internal/graph/auth_token.go:27-41`, `:53-68`). From then on the process works from memory.
- A refresh is persisted by a hook that only warns on failure (`internal/graph/auth_token.go:104-112`):

  ```go
  OnTokenChange: func(tok *oauth2.Token) {
      if err := tokenfile.Save(tokenPath, tok); err != nil {
          logger.Warn("couldn't save the refreshed sign-in; the next run may ask you to sign in again. ...
          return
      }
  ```

- `ForceRefresh` redeems the refresh token held in memory (`:190-203`).
- `invalid_grant` maps to "sign-in required" (`:243-248`; `R-6.6.23`, exit status 77 per `R-6.6.31`).
- No lock file, no re-read before a refresh, no re-read after a failure. `tokenfile.Save` is an atomic write that also creates the directory (`internal/tokenfile/tokenfile.go:76-96`).
- The fork adopts a rotated refresh token in memory (`$OAUTH2/oauth2.go:299-301`) and calls the hook after releasing its mutex (`:337-339`).

Inside one process the repo already treats this as a real hazard: "Multiple drives sharing a token path share one TokenSource, preventing OAuth2 refresh token rotation races (two independent refreshes can invalidate each other's refresh tokens)" (`internal/driveops/session.go:98-102`; cache at `:255-271`). But seven CLI call sites build their own source from the same file and bypass that cache: `internal/cli/email_reconcile.go:33`, `shared_discovery.go:136`, `shared_target.go:157`, `status_live.go:47`, `drive_add.go:507`, `drive_search.go:111`, `drive_catalog.go:221` (the last once per account inside a goroutine, `:171-193`). `FlushTokenCache` only empties the map; sessions already built keep their source (`internal/driveops/session.go:301-310`).

Between processes (watch daemon plus any CLI command):

1. **Last writer wins on the file**, and the other process never learns: it keeps redeeming its own in-memory refresh token.
2. **Logout can be undone.** `logout` deletes the token first (`internal/cli/auth_logout.go:114`, `:148`) and does not notify the daemon: `notifyDaemonIfRunning` is called only from `internal/cli/auth_flow.go:96`, `drive_remove.go:54`, `:61` and `drive_add.go:127`. Until the daemon's config poll reloads (5 s interval, two matching stamps, `internal/multisync/watch_config_file.go:16`, `:97-112`), a silent refresh in the daemon calls `tokenfile.Save`, which recreates the file.
3. **Login replaces the file by a plain rename**, without a lock or a directory sync (`internal/cli/auth_login.go:281-302`, `localpath.Rename` at `:292`), and the login's own token source stays bound to the pending path (`internal/cli/auth_flow.go:22`, `:284`, `:299`), so a refresh by that source later in the same process would write the pending file again.

SUSPECTED: that the service invalidates the previous refresh token when it issues a new one. If it does, the loser of a simultaneous refresh gets `invalid_grant` and the user is told to sign in again on a healthy login. Not provable from code or without network; settle with a live test on the test account: refresh in two processes from the same file, then use the older refresh token.

The specs say nothing about cross-process refresh; `spec/design/graph-client.md:590` says "Token refresh state is delegated to the token source implementation … it does not run a background refresh coordinator".

### Proposed end state

`internal/tokenfile/lock.go` (new; add to `spec/design/graph-client.md:3`):

```go
// Lock is an exclusive advisory lock on "<token file>.lock", a stable file
// that is never removed while the account exists.
type Lock struct{ /* *os.File */ }

func AcquireLock(ctx context.Context, tokenPath string) (*Lock, error) // non-blocking flock retried until ctx ends
func (l *Lock) Release() error
```

The lock file name ends in `.lock`, so token discovery skips it (it requires the `token_` prefix and the `.json` suffix, `internal/config/discovery.go:46`).

`internal/graph/auth_refresh.go` (new; add to GOVERNS): a refresher that replaces the fork's refresher as the source behind `oauth2.ReuseTokenSource` (`$OAUTH2/oauth2.go:401`) and is also what `ForceRefresh` calls:

```go
type coordinatedRefresher struct {
    ctx       context.Context
    cfg       *oauth2.Config
    tokenPath string
    current   *oauth2.Token // last token this process used
    lockWait  time.Duration
    now       func() time.Time
    logger    *slog.Logger
}

func (r *coordinatedRefresher) Token() (*oauth2.Token, error)
func (r *coordinatedRefresher) forceRefresh(rejectedAccessToken string) (*oauth2.Token, error)
```

`Token()`:

1. Take the lock (bounded by `lockWait`; a timeout is an error that says another process is refreshing).
2. Read the file. Missing file: return the sign-in-required error and do not refresh (logout wins; nothing recreates the file).
3. If the file's refresh token differs from `current`, adopt the file's token. If its access token is still valid, return it: no network call.
4. Otherwise redeem `current.RefreshToken`.
5. On success, save under the same lock. If the save fails, keep serving the token, mark it unsaved, and retry the save on each later call until it succeeds; log each failure once.
6. On `invalid_grant`, read the file once more; if its refresh token differs from the one just used, redeem that one; otherwise return sign-in required.

`forceRefresh` is the same path, except that step 3 adopts the file's token whenever its access token differs from the rejected one.

Step 6 looks redundant under the lock; it is the fallback for a filesystem where flock is not supported (the lock call then returns "unsupported" and the refresher proceeds unlocked) and for a writer that does not take the lock.

Around it:

- Login installs the token with a `tokenfile.Install(pendingPath, finalPath)` that takes the lock, writes through `fsroot.AtomicWrite` (directory sync included) and removes the pending file; afterwards the process uses a source built from the final path. `moveToken` (`internal/cli/auth_login.go:281-302`) is deleted.
- Logout deletes the token under the lock and calls `notifyDaemonIfRunning`.
- The seven CLI call sites obtain their source from the `SessionRuntime` cache through one exported method, so a process has one source per token file.
- The `OnTokenChange` hook (`internal/graph/auth_token.go:104-112`) is removed; persistence is step 5.

### Existing tests

`internal/graph/auth_test.go` (`TestDoLogin_Success`, `TestSaveLoadToken_RoundTrip`), `internal/graph/client_auth_refresh_test.go` (`TestTokenBridge_ForceRefreshRedeemsRefreshTokenOncePerBurst`, `TestDoOnce_401_ForcesRefreshWhenCachedTokenIsUnchanged`, `TestTokenBridge_RefreshFailureIsReturnedNotWarned`, `TestTokenBridge_InteractionRequiredIsPlainSignInRequired`), `internal/cli/auth_test.go:271`, `:288` (`TestMoveToken_*`). None involves two sources on one file.

### Tests to write first

`internal/graph/auth_refresh_test.go`, with an `httptest` token endpoint that counts redemptions per refresh token and rejects any refresh token it has already rotated, a temp token file, an injected clock:

- `TestRefresh_TwoSourcesOneFile_SecondAdoptsFirstsToken` — one redemption in total.
- `TestRefresh_RotatedTokenOnDiskIsUsedInsteadOfStaleMemory`.
- `TestRefresh_InvalidGrantRereadsAndRetriesOnce`.
- `TestRefresh_MissingFileIsSignInRequiredAndFileStaysAbsent`.
- `TestRefresh_SaveFailureKeepsTokenAndRetriesSave` (failure injected through the `fsroot` fake).
- `TestForceRefresh_AdoptsNewerTokenFromDisk`.
- `TestRefresh_LockWaitIsBounded`.

`internal/tokenfile/lock_test.go`: mutual exclusion between two handles, release, a cross-process case modelled on `internal/config/write_test.go:160-229`. `internal/cli`: `TestLogout_NotifiesDaemon`, `TestLogin_InstallsTokenUnderLock`.

### Spec and requirement updates

`spec/design/graph-client.md:3`, `:590` (replace the sentence), a new "Cross-process refresh" subsection beside `:528-582`, `:610`. A new requirement in the `R-6.8` family for "two processes sharing one sign-in never invalidate each other" (no existing ID; grep of `spec/` for "rotat" and "refresh token" found only `spec/design/system.md:422`, `spec/design/cli.md:284` and two lines of `spec/reference/output-journeys-vs-abraunegg.md`). `R-3.1.3` (`spec/requirements/drive-management.md:9`) gains the daemon reload for logout, as `R-3.3.1`, `R-3.3.5` and `R-3.3.7` already have.

### Risks

A refresh now waits on another process, so the token-endpoint timeout of Item 11 must land first or a stuck refresh in one process blocks the other until the lock wait expires. `logout --purge` must remove the lock file; whether `purgeOrphanedFiles` would do so was not read (SUSPECTED; settle by reading `internal/cli/auth_logout.go:204-300`). The lock file's name contains the account name exactly as the token file's does.

---

## Item 10 — What the repo itself records as planned output work

**There is no recorded list of four output leftovers** (VERIFIED by grep of `spec/` and the commit log).

- `[planned]` appears nowhere in `spec/design/cli.md` and on no CLI-output requirement. Every `[planned]` in `spec/`: `spec/reference/graph-api-quirks.md:60`; `spec/design/system.md:448`, `:468`, `:469`, `:470`; `spec/requirements/non-functional.md:12-16` (`R-6.1.6`–`R-6.1.10`), `:194` (`R-6.10.15`); `spec/design/graph-client.md:613`, `:623`; `spec/design/performance-benchmarking.md:5`.
- "leftover" in `spec/design/cli.md:383` and `:453` is about drive remnants and an aside file; "cosmetic" in `spec/design/system.md:377` is about linter versions. `git log --grep` finds only titles of merged work.

What is recorded as not yet done on the CLI and its output:

| ID | What | Status | Where |
|---|---|---|---|
| `R-6.6.5` | progress bars, colour-coded transfer output | `[future]` | `spec/requirements/non-functional.md:71` |
| `R-6.6.6` | TUI | `[future]` | `:72` |
| fix list item 8 | progress bars | "remains `[future]` (R-6.6.5)" | `spec/reference/output-journeys-vs-abraunegg.md:371-381`, `:397` |
| fix list items 7, 9 | "resolved differently"; "done for state and wording; canonical IDs stay in `drive list`" | closed with a recorded deviation | same |
| `R-2.3.7`, `R-2.10.4`, `R-2.10.32`, `R-2.10.45`, `R-2.14.3` | status and retry-scope requirements | `[designed]`, not `[verified]` | `spec/requirements/sync.md:36`, `:130`, `:158`, `:171`, `:225`; `spec/requirements/index.md:25` |
| `R-4.5.1`, `R-4.6.1`–`R-4.6.7` | `setup`, `service …` commands | `[future]` | `spec/requirements/configuration.md:29-41` |
| `R-3.8.1`–`R-3.8.3` | `migrate` | `[future]` | `spec/requirements/drive-management.md:114-116` |

Whether the five `[designed]` items are implemented and merely not promoted is SUSPECTED (settle by reading `spec/design/cli.md:477-602` and its Verified By table against the tests).

Output inconsistencies this analysis found in code that the repo does **not** record (candidates for what the owner remembers), all VERIFIED:

1. `put` reports folder progress through `cc.Statusf` (`internal/cli/put.go:385`) while `get` uses `cc.Progressf` (`internal/cli/get.go:110`, `:352`), against `spec/design/cli.md:389-391`.
2. Empty lists print as `null` in some documents and `[]` in others (Item 5).
3. Shared-folder `get` formats a failed file differently from plain `get` (`internal/cli/get_shared.go:197` versus `internal/cli/get.go:340`).
4. Dead output plumbing in `status`: the `history` and `nextIndent` parameters and the no-op `printSyncStateStoreLines` (Item 4).

---

## Item 11 — Other hangs and silent failures outside `internal/sync`, `multisync`, `synccontrol`

### 11.1 OAuth token-endpoint calls have no response timeout — VERIFIED

Nothing in `internal/graph` puts an HTTP client into the OAuth context (grep for `oauth2.HTTPClient`, `HTTPClient`, `WithValue(` is empty), so the fork uses `http.DefaultClient` (`$OAUTH2/internal/transport.go:21-27`). Its transport has dial and TLS timeouts but no `ResponseHeaderTimeout` (`$GOROOT/net/http/transport.go:46-57`), and the client's `Timeout` is zero. This covers the silent refresh (`internal/graph/auth_token.go:149`, `:151`), device login (`internal/graph/auth_device.go:28`, `:41`) and the browser code exchange (`internal/graph/auth_browser.go:228`).

The refresh runs while the fork holds its mutex (`$OAUTH2/oauth2.go:322-336`), with the long-lived context the source was built with (`internal/multisync/orchestrator_watch.go:395`; `internal/cli/root.go:377`). A token endpoint that accepts the connection and never answers therefore blocks every Graph request of that account until the process is stopped. `ForceRefresh` holds `refreshMu` as well (`internal/graph/auth_token.go:179-180`).

Fix: a token-endpoint client in `internal/graphtransport` with the same dial, TLS and response-header bounds as the metadata profile (`internal/graphtransport/profiles.go:14-25`, `:118-133`) and no retry wrapper, installed with `context.WithValue(ctx, oauth2.HTTPClient, client)` where the token source and the login flows are built. Test first: an `httptest` server that accepts and never writes, an injected short response-header timeout, assert `Token()` returns an error and that a second caller is released. Docs: `spec/design/graph-client.md:604` and the profile list; GOVERNS already names `profiles.go`.

### 11.2 Metadata response bodies have no stall bound — bound VERIFIED absent, hang SUSPECTED

All Graph clients run with `Timeout: 0` (`internal/graphtransport/profiles.go:55`, `:69`, `:73`, `:81`) and rely on transport bounds (response header 2 min, dial 30 s, TLS 15 s, keep-alive, HTTP/2 ping; `:14-25`, `:118-133`). The stall watchdog (`internal/graph/transfer_watchdog.go:14-58`) is applied only to downloads and upload chunks (`internal/graph/download.go:243`, `:371`, `:436`; `internal/graph/upload_transfer.go:65`). Reading the body of a listing or a delta page has no deadline of its own. A body that stops mid-stream on a connection that still answers pings would wait until the caller's context ends. Settle by checking whether `internal/graph/response_body.go` bounds reads (it was read; I found no deadline, but did not trace every caller). Fix: wrap metadata body reads in the same watchdog with the same 2-minute idle limit.

### 11.3 Blocking config lock — VERIFIED

`syscall.Flock(..., LOCK_EX)` with no timeout (`internal/config/write.go:795`). A stopped or stuck holder hangs every config-writing command and the daemon's pause-expiry write (`internal/multisync/orchestrator_watch.go:827`). The same function runs the mutation unlocked when the directory is missing (`internal/config/write.go:783-785`). Fix in Item 2.

### 11.4 Silent failures — VERIFIED

| Where | What happens | Fix |
|---|---|---|
| `internal/cli/drive_catalog.go:234-238` (and `:216-219`) | a client bootstrap failure during drive discovery is logged at debug level and the account contributes nothing; no degraded notice. The shared path does it right (`internal/cli/shared_discovery.go:152-158`). | return a degraded-account notice (`R-3.3.2` requires the account to stay visible, `spec/requirements/drive-management.md:35`) |
| `internal/cli/auth_flow.go:139-141` | login succeeds although the catalog record could not be written | Item 2 |
| `internal/cli/auth_flow.go:271-275` | failure to clear "sign-in required" after a successful login is only warned | return the error, or fold into the Item 2 transaction |
| `internal/cli/sync_console_json.go:77-80` | JSON event dropped | Item 5 |
| `internal/cli/get_shared.go:187-188` | cancelled file not reported | Item 1 |
| `internal/graph/auth_token.go:104-112` | refreshed token not saved, only warned; lost at exit | Item 9 step 5 |
| `internal/cli/drive_add.go:228`, `:307`, `:475`, `:490` | failed rollbacks only warned | Item 2 removes rollback |
| `internal/config/catalog.go:153-162` | lost update | Item 2 |
| `internal/driveops/session_store.go:332-335` | `CleanStale` skips an entry whose `Info()` fails | count and return the skipped entries in the result, log at warn |
| `internal/config/catalog_lifecycle.go:149-151` | purge skips a catalog drive whose ID does not parse | return an error naming it |
| `internal/cli/auth_login.go:292` | token installed by rename with no directory sync; a crash right after login can lose it | Item 9 (`tokenfile.Install`) |

### 11.5 Checked and not a defect — VERIFIED

- `graph.NewClient` falls back to `http.DefaultClient` for a nil client (`internal/graph/client_construction.go:28-30`), but every production caller passes one (`internal/cli/root.go:416-435`; `internal/driveops/session.go:288`, `:293`). Refuse nil, as the nil token source already is (`:20-22`). Same for `perf.RoundTripper` with a nil `Inner` (`internal/perf/http.go:13-16`; always set at `internal/graphtransport/profiles.go:57`, `:70`, `:74`, `:83`).
- `context.Background()` at `internal/cli/root.go:475` (only when the command has no context) and `:713` (the process root context, `spec/design/cli.md:407-411`); `internal/driveops/hash.go:84-89` hashes a local descriptor, no network.
- Goroutine owners: `internal/cli/signal.go:23-63`, `internal/cli/sync_notify.go:175-194`, `internal/cli/shared_discovery.go:274-307`, `internal/graph/auth_browser.go:108` (shut down at `:41`, `:182`) are owned and waited for. `internal/perf/logging.go:47` stops only when `Complete` is called (`:73-86`) and does not watch its context; its single production caller completes it (`internal/cli/root.go:760`, `:718`, `:778`). `internal/cli/drive_catalog.go:171-195` starts one goroutine per signed-in account and waits for all: owned, but not bounded; use the `shared_discovery.go:274-307` shape.
- Ignored errors (`_ =`, `nolint:errcheck`): the write- and remove-path ones are documented cleanups on an error path (`internal/driveops/transfer_target.go:166-167`; `internal/localpath/rename_noreplace.go:108-109`; `internal/localtrash/localtrash.go:457`, `:501-502`; `internal/synctree/detach_unix.go:429-441`). None was shown to hide a failed write of user data.

---

## Not read

Stated so no claim above is taken further than its evidence: `spec/design/drive-identity.md`; `spec/reference/cli-output-guidelines.md`; `spec/design/config.md` beyond a keyword skim (`:3`, `:40-50`, `:82-98`, `:222`, `:342-350`, `:448`); `internal/cli/failure_class.go`; `internal/cli/auth_logout.go:204-300`; bodies of `internal/synctree/detach_unix.go`; `internal/driveops/test_helpers_test.go`; `internal/sdnotify`, `internal/logfile`, `internal/perf/capture.go`, `internal/perf/context.go`, `internal/cli/console_log_bootstrap.go`, `internal/driveid/itemkey.go`, `internal/devtool/bench_live.go`.

---

## PR breakdown, in dependency order

Each PR is tests first, updates the named spec lines and GOVERNS in the same change, and is independent of later ones.

| # | PR | Contains | Depends on |
|---|---|---|---|
| 1 | `fsroot`: mutations through `os.Root` | Item 7. Symlink-containment tests, fake `rootHandle`, `graph-client.md:610` | — |
| 2 | One Retry-After parser | Item 3. `retry/retry_after.go`, clock on `RetryTransport` and `graph.Client`, requirement text for the ceiling (owner decides the value) | — |
| 3 | `graph`: loud body rewind, refuse nil HTTP client | Items 8 and 11.5 | — |
| 4 | Token-endpoint client with timeouts | Item 11.1. `graphtransport` profile, wiring in token source and login flows | — |
| 5 | Cross-process token refresh | Item 9. `tokenfile/lock.go`, `graph/auth_refresh.go`, `tokenfile.Install`, logout under lock plus daemon notify, one token source per process | 1, 4 |
| 6 | Config and catalog transaction | Item 2 and 11.3. Catalog lock, bounded lock wait, `config.AddDrive`, login on the same transaction, rollback code and tests deleted, `R-3.3.1`/`R-3.3.5` text | 1; after 5 (both rewrite `completeLogin`) |
| 7 | Delete compat and dead code | Item 4. Session-store shim and strict load, test-only wrappers, no-ops, dead parameters | — |
| 8 | One JSON encoder | Item 5. `writeJSONDocument`/`writeJSONLine` only, nil-list rule, lint rule, contract test extension | 7 (touches the same status files) |
| 9 | Tree transfer engines in `driveops` | Item 1 and the `get_shared` silent drop. `DownloadTree`, `UploadTree`, CLI reduced to rendering, remote-name check | 8 |
| 10 | Remaining silent failures | Items 11.2 and 11.4 not covered above: drive-discovery degraded notice, bounded discovery fan-out, `CleanStale` skip, purge skip, metadata body stall bound | 3 |
| 11 | Split `orchestrator_watch.go` | Item 6, pure moves | none; merge when no other branch touches the file |

Open decisions for the owner before the PRs that need them: the Retry-After ceiling (PR 2); HTML escaping in JSON output (PR 8); whether folder progress keeps a total and whether `put -r` becomes parallel (PR 9); the lock-wait limits (PRs 5 and 6).
