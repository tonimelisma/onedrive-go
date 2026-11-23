# W14 report: fsroot precise absence errors and directory sync on FUSE and SMB

Commit `bca5be85` on `fix/fsroot-leftovers` (base `5de4ff7f`), 5 files, not pushed.

## What changed
- `internal/fsroot/fsroot.go`: `normalizeNotExist` maps to `os.ErrNotExist` only when the failure says not-exist/ENOTDIR (`isMissingName`) and the rooted lstat agrees; `Remove` now normalizes too; `syncDirectory` tolerates `localpath.IsDirSyncUnsupported` errnos; new `rootOps.syncDir` seam (takes the already opened dir, never a path); `CreateTemp` deleted, `createTempIn` renamed `createTemp`; `ReadFile`/`openWithRoot` name the file on a close failure; `ReadDir` passes `filepath.ToSlash`; doc comments on every exported method; package doc states the absence contract.
- `internal/fsroot/fsroot_test.go`: new tests below; `createTempThroughHandle` and `openThenReturnClosed` helpers, `fakeRootHandle.closeErr`; two dir-sync tests renamed to `TestRoot_AtomicWrite_*`.
- `spec/design/system.md`: Verified-By rows (both atomic writers; new fsroot row); atomic-write bullet (both writers tolerate, why fsroot imports localpath, stale "config, tokens via localpath" removed); managed-state bullet (absence rule, state DB opened by path and why); ops bullet (`syncDir`).
- `spec/design/threat-model.md`: evidence row cites the `TestRoot_*_RefusesSymlinkedParent` tests; error-classification bullet covers fsroot.
- `spec/requirements/non-functional.md`: R-6.4.10 (absence only when absent; state DB outside, opened by path); R-6.5.4 (unsupported directory fsync is best effort).

## Tests added
- `TestRoot_OpenFile_CreateFailureKeepsItsErrno` (R-6.4.10): EACCES/ENOSPC/EROFS/plain error on an absent name keep their error. FAILED before (all read "file does not exist").
- `TestRoot_OpenFile_PermissionDeniedIsNotNotExist` (R-6.4.10): real r-x folder, create says permission denied. FAILED before.
- `TestRoot_AtomicWrite_PermissionDeniedIsNotNotExist` (R-6.4.10, named by the entry): PASSED before; see deviations.
- `TestRoot_AtomicWrite_ToleratesUnsupportedDirectorySync` (R-6.5.4): EINVAL/ENOTSUP/EOPNOTSUPP through `syncDir`, write succeeds, one sync of `nested`. FAILED before.
- `TestRoot_NameBelowAFileIsNotExist` (R-6.4.10): Stat/Open/Remove of `file/x` are absent; ReadDir of a file keeps ENOTDIR. FAILED before (Stat, then Remove).
- `TestRoot_Stat_RefusesSymlinkedParent` (R-6.4.10): reads beneath an escaping link fail and never read as absent. Pin, passed before.
- `TestRoot_ReadFile_ReportsReadAndCloseFailures`, `TestRoot_Open_ReportsRootAndFileCloseFailures`: both failures carry step and name. FAILED before.

## Checks
- `go build ./...`, `go vet ./...`, `GOOS=darwin go vet` (fsroot, tokenfile, logfile), `go vet -tags 'e2e e2e_full' ./e2e/...`: pass.
- `go test -count=1` and `-race -count=1` `./internal/fsroot/...` (GOTOOLCHAIN=go1.26.11): ok.
- golangci-lint `./internal/fsroot/...`: `0 issues.` (W14-lint.log), run after the other verify had ended.
- Every test name the docs cite exists (grep of the 16 cited names).

## Deviations
- `TestRoot_AtomicWrite_PermissionDeniedIsNotNotExist` cannot fail before the fix: `AtomicWrite` never called `normalizeNotExist` (its create error already kept EACCES). The bug lived in `Open`/`OpenFile`/`Stat`/`ReadDir`; `TestRoot_OpenFile_*` are the failing regression tests. The AtomicWrite test stays as the config/token contract.
- The directory sync could not be faked through the handle (`rootHandle.Open` returns `*os.File`), so it got its own op `syncDir`; a pipe stand-in would answer EBADF under darwin's F_FULLFSYNC.
- `CreateTemp` deleted, not unexported: nothing in production, inside the package or out, called it.
- ENOTDIR on a parent now reads as absence (as the entry asks); before, `Stat("file/x")` returned ENOTDIR. No production caller passes a multi-component name, so the change is reachable only in tests today.
- `Remove` now normalizes absence too, so `Remove("file/x")` says not-exist like `Stat`; missing-name messages from Remove read "file does not exist" instead of the raw `removeat` error (`errors.Is` unchanged).
- errno-test decision: `fsroot` imports `localpath` (no new leaf package): localpath imports nothing internal, synctree already imports it for this predicate, system.md names it the single definition.

## Also fixed
- `fsroot.go` ReadFile: read+close failure lost "reading/closing <name>" context.
- `fsroot.go` openWithRoot: file-close error after a root-close failure was joined bare.
- `fsroot.go` ReadDir: fs.FS name now slash-separated.
- `fsroot.go`: exported methods had no doc comments; package doc lacked the absence contract.
- `fsroot_test.go`: `TestRoot_MissingPathsNormalizeToNotExist` gained `Validates: R-6.4.10`; duplicated closed-handle fake folded into one helper.
- `system.md:404-406`: claimed config and tokens are written by `localpath.AtomicWrite` (only trash records are).

## Not fixed (12)
- `internal/synctree/synctree.go:1160` normalizeNotExist lets a failure without an errno reach the absence check, and maps ENOTDIR on a parent only if the unrooted lstat says ENOENT. Fix: fsroot's two-answer rule (failure and lstat both not-exist or ENOTDIR).
- `internal/synctree/synctree.go:1173` and `internal/fsroot/fsroot.go` `isMissingName`: the missing-name errno set is defined twice. Fix: one exported predicate (e.g. in localpath) both use; do it with the item above (H9 owns synctree).
- `internal/localpath/localpath.go:115-117` ReadFile joins read and close errors without context. Fix: wrap each as fsroot now does.
- `internal/localpath/localpath.go:21-24` `clean == ""` after filepath.Clean is dead (Clean never returns ""). Fix: delete.
- `internal/cli/cleanup.go:57-71` managedParentIsNotADirectory re-derives "root is not a directory" (os.OpenRoot answers a plain error without ENOTDIR) and treats its own OpenPath/Stat failures as "no". Fix: fsroot classifies a root that is a file, callers drop the helper.
- `internal/logfile/logfile.go:65-84` cleanOwnOldLogs swallows the listing, Info and removal errors; `:63` `24 * time.Hour` unnamed. Fix: return or log them once; name the day.
- `internal/driveops/session_store.go:163` Load reopens and shadows `root` for the corrupt-file cleanup. Fix: reuse the open root.
- `internal/driveops/session_store.go:332-335` CleanStale skips an entry whose Info fails without a word. Fix: collect into the returned error as DeleteForScope does.
- `internal/config/write.go:770`, `internal/tokenfile/tokenfile.go:87` call root.MkdirAll before AtomicWrite, which creates the root itself. Fix: drop the extra call.
- `internal/config/email_reconcile.go:611` renameManagedPathIfPresent renames to `filepath.Base(target)` inside the source's folder, so a target in another folder lands beside the source. Fix: refuse differing parents or open the target's root.
- `internal/sync/store.go:110-115` the DSN interpolates dbPath unescaped into a `file:` URI (a `?` or `#` in the data path changes it) and busy_timeout 5000 / journal_size_limit 67108864 are unnamed. Fix: build the URI with net/url, name the constants.
- `.golangci.yml:128-136` the leaf-internal depguard rule lists internal/tokenfile, yet tokenfile.go:16 imports internal/fsroot and lint passes on main, so the rule does not bite. Fix: correct the file glob or drop tokenfile from it and its "leaf package" comment (tokenfile.go:2-4).

## Unfinished
- Nothing.

## For the reviewer to run
- `internal/tokenfile`, `internal/logfile`, `internal/config`, `internal/cli` (cleanup.go readers, `managedPathExists`), `internal/driveops` (session store), `internal/graph` (token save/load), `internal/sync` store-open tests (`-run` on NewSyncStore/openSyncStore): all branch on fsroot's `os.ErrNotExist` or call AtomicWrite/Remove.

## Second pass (amended into one commit, now `9f7dcf24`, 27 files; branch is one behind `origin/main`, not rebased)

### What changed, per item
1. `internal/localpath/localpath.go`: ReadFile reads through `readAndClose`; a new `closeAfterFailure` names the path on a close that follows a failure. The same bare join in `OpenRegular` (stat failure, not-regular) uses it too.
2. `localpath.go` `cleanPath`: the dead `clean == ""` check is gone. No twin exists in the package (grep of `== ""` and `Clean(` over its files).
3. `internal/fsroot/fsroot.go`: `withRootFor(target, op)` and `rootOpenError` classify a root that failed to open. Missing, or lying below a file: absent (`os.ErrNotExist`), and so is everything beneath it. A root that is itself a file (decided by `os.Stat` of the root path, because `os.OpenRoot` gives a bare "not a directory" with no errno): every name beneath it is absent, and a listing of the root reports `syscall.ENOTDIR`. This is consistent with the in-root ENOTDIR rule. Package doc and R-6.4.10 say so. `internal/cli/cleanup.go`: `managedParentIsNotADirectory` deleted, and `managedPathExists` takes fsroot's answer. No other caller re-derived it (grep of ENOTDIR, "not a directory" and IsDir checks over the callers).
4. `internal/logfile/logfile.go`: retention is `RemoveExpired(path, days) error`, separate from `Open(path)`. It must run first, because an expired current log is removed and Open starts it afresh. It returns every listing, age-check and removal failure, joined, and an absent folder is nil. `retentionDay` is now a named constant. `internal/cli/root.go`: `buildLoggerDualWithStatusWriter` calls it first, builds the logger (body moved to `buildFileLogger`), then logs one Warn "could not remove expired log files". Why returned rather than logged: logfile runs before the logger exists, and the log works without retention, so the failure is not Open's.
5. `internal/driveops/session_store.go` Load: the corrupt-file cleanup removes through the already open `root` and `name`.
6. CleanStale: the per-entry loop moved into `removeStaleEntries`. An Info failure (other than not-exist) and a removal failure go into the joined error with the count, as DeleteForScope does. The per-file Warn is gone; callers already log the returned error.
7. `internal/config/write.go`, `internal/tokenfile/tokenfile.go`: the extra `root.MkdirAll` is dropped. The modes were the same: AtomicWrite calls `r.MkdirAll(dirPerm)` with the same constant both callers passed (configDirPermissions 0o755, DirPerms 0o700), so nothing was stricter to keep. Proved by pins. Three tests now expect AtomicWrite's "creating root directory" message instead of the dropped ones (config write_test, tokenfile_test, graph auth_test).
8. Why the rule did not bite: golangci-lint passes `files` through a `${base-path}`/`${config-path}` replacer only. depguard 2.2.1 then compiles each pattern with gobwas/glob and matches it, anchored, against the file's absolute path, so `internal/tokenfile/*.go` can never match. All four path-scoped rules were dead, not only the leaf rule (`$test` expands to `**/*_test.go`, which is why the negations "worked"). `.golangci.yml`: every pattern now starts with `**/`. The leaf list is now driveid, errclass, namekey, localpath; tokenfile imports fsroot and retry imports perf, so neither is a leaf. Proof: a temporary `_ ".../internal/errclass"` in `internal/driveid/id.go` gave 0 issues under the old config and `depguard: 1` ("not allowed from list 'leaf-internal'") under the new one. The import was then removed and driveid shows no diff. `tokenfile.go:2-4` comment rewritten. Docs fixed: `system.md:87` (leaf list) and `:255` (why patterns need `**/`), `graph-client.md:648`, `retry.md:9`.

### Tests (all fail-first unless marked pin)
- `TestRoot_RootThatIsAFileHasNothingBeneathIt` (R-6.4.10): Stat/ReadFile/Remove/Rename beneath a file root or below-file root are absent; listing a file root gives ENOTDIR, a below-file root not-exist. FAILED first.
- `TestManagedPathExists_DistinguishesAbsentFromUnreadable`: new case, a file two folders up. FAILED first with the old fsroot (seen by stashing the fsroot change).
- `TestReadAndClose_NamesThePathInBothFailures`: FAILED first (bare join).
- `TestRemoveExpired_ReportsWhatItCannotListOrRemove` (R-4.7.3): removal refused, listing refused, no folder yet. The first two FAILED first; "no folder" is a pin.
- `TestRemoveExpiredEntries_ReportsAnEntryItCannotStat` (R-4.7.3): fake DirEntry. FAILED first.
- `TestBuildLoggerDual_ReportsAnExpiredLogItCannotRemoveOnce` (R-4.7.3, R-6.6.1): exactly one Warn in the log, log still opens. FAILED first.
- `TestSessionStore_CleanStale_ReportsASessionItCannotRemove`, `TestSessionStore_RemoveStaleEntries_ReportsAnEntryItCannotStat` (R-5.2): both FAILED first.
- `TestRoot_AtomicWrite_CreatesMissingFoldersWithDirPerm`, the `DirPerms` check added to `TestSave_CreatesDirectory`: pins, passed before and after the MkdirAll drop.
- `TestGolangciConfig_DepguardFilePatternsMatchAbsolutePaths` (R-6.10.2): FAILED first, naming all six relative patterns. The existing errcheck contract test now shares its config reader.

### Checks
- Non-race `go test -count=1` over fsroot, localpath, logfile, tokenfile, driveops, config, cli, graph: all ok. `-race` over fsroot, localpath, logfile, tokenfile: ok.
- devtool `-run "ListsEvery|Table|Governs|Exhaustive|Registered|TestGolangciConfig_"`: ok.
- `go build ./...`, `go vet ./...`, `GOOS=darwin go vet` over the touched packages, `go vet -tags 'e2e e2e_full' ./e2e/...`: pass.
- Whole-repository `golangci-lint run ./...` (run once, last, 630 s on the loaded host): `0 issues.` (W14-lint.log).

### Deviations
- Item 4: `logfile.Open` lost its retention parameter and retention became `RemoveExpired`. Open deleting files was a second side effect, and the caller needs the retention failure after it builds the logger.
- Item 6: removal failures moved from an in-store Warn to the returned error as well, not only Info failures, so the store reports partial failure one way. Effect: on the engine-close path (`internal/sync/engine.go:434`, out of scope) a removal failure now logs at Debug, where it used to Warn; after a sync pass it stays at Warn (`driveops/cleanup.go:44`). On a partial failure that caller logs the Warn but not the count.
- Item 3: a data directory below a file now reads as empty to config discovery (absent), where it used to be an error. A data directory that is itself a file still errors (ENOTDIR).
- Item 1 also covers `OpenRegular`, and item 4 the chmod-then-close path in `logfile.Open`. Same class, but neither close failure can be produced without a seam, so both are untested.

### Seen and not fixed (new)
- `internal/localpath/rename_noreplace.go:106-111` copyExclusive swallows the removal of a partial copy (`_ = os.Remove(dst)`), and a later retry then reads that leftover as ErrTargetExists. Fix: join the removal failure into the returned error.
- `internal/synctree/spelling.go:63` is a third copy of the missing-name predicate (fsroot `isMissingName`, synctree `missingPathErrno`). Fold it into the synctree rider.
- `internal/fsroot/fsroot.go` withRootFor: `os.OpenRoot` opens the root path for reading, so a root that is a FIFO blocks forever. Fix: lstat the root and refuse a non-directory before opening (costs a syscall per operation), or open with O_NONBLOCK upstream.
- `internal/sync/engine.go:434-442` (out of scope): logs CleanStale's error at Debug and its count only on success. Fix: Warn on error, and log the count in both cases.
- `internal/config/write.go:22` configDirPermissions is 0o755 while the config file is 0o600, so the config folder is listable by other local users. Fix: 0o700, if the owner agrees.

## Third pass (rebased onto `origin/main` 27e76cc3, still one commit, amended: `0b7299fb`, 35 files, not pushed)

### What changed, per item
1. `internal/localpath/rename_noreplace.go`: `copyExclusive`'s error-path cleanup is now `discardPartialCopy(out, dst, cause, remove)`. A close failure other than ErrClosed, or a removal failure other than not-exist, is joined to the copy's error. Before, both were discarded with `_ =`.
2. `internal/fsroot/fsroot.go`: `withRootFor` calls `checkRootIsDirectory(target)` before `os.OpenRoot`: an `os.Stat` of the root path. The results are: missing gives not-exist; ENOTDIR (the path runs through a file) gives not-exist; any other stat failure is returned as it is; not a directory (file, FIFO, device) gives ENOTDIR for a listing of the root and not-exist for a name beneath it. Only a directory reaches OpenRoot, and an open that still fails keeps its own error. `rootOpenError` and `rootIsAFile` are gone. The package doc, `system.md` (managed-state bullet) and R-6.4.10 state the order.
3. `internal/sync/engine.go` Close: a CleanStale error is Warn with `deleted` (it was Debug with no count); success stays Info with `deleted`. `internal/driveops/cleanup.go`: the CleanStale Warn now carries `count`; success stays Info with `count`. A seam exists for both: `newTestEngineWithLogger` builds a real engine with a data folder and a caller's logger, so both are tested.
4. `internal/config/write.go` `configDirPermissions` 0o755 → 0o700. It covers the config folder, the mutation-lock folder and the data folder the catalog writes into (`catalog.go:141` uses `atomicWriteFile`). I swept every folder the client creates (grep of octal modes, `MkdirAll`/`Mkdir`/`MkdirTemp` in production code): tokens, logs, upload sessions, store folder, engine locks, trash, control socket, perf capture and dry-run scratch (`MkdirTemp`) were already 0o700. Left alone: the sync folder (`cli/sync_dir.go`, 0o700 already), user content (`cli/get.go` 0o755 for `get` targets, `driveops/transfer_target.go` 0o777 minus umask for download parents, `sync/executor.go` 0o755), and devtool's 0o750 worktrees (tooling). No existing folder is ever chmodded.
5. `.golangci.yml`: (a) the leaf list is computed, not assumed. A script listed each internal folder's non-test imports. It gave exactly authstate, driveid, errclass, localpath, loglevel, namekey, sdnotify, sharedref, which matches the brief's count. (b) Three lax rules: `tokenfile-only-fsroot`, `retry-only-perf`, `fsroot-only-localpath`. Each denies `github.com/tonimelisma/onedrive-go/internal/` and allows its one dependency by its exact path (`$`), so a sibling sharing the prefix is not allowed. (c) `TestGolangciConfig_DepguardPackagePatternsNameExistingPackages`; the existing contract test shares its reader. Docs: `system.md:87` and `:255`, `graph-client.md:648`, `retry.md:9`, and the `tokenfile.go` package comment name the rules.
Docs also: R-6.8.23 (every state folder 0700, existing ones keep their mode, the sync folder not covered), `config.md` (config folder mode), `threat-model.md:61`, `drive-transfers.md` and `sync-engine.md` (Warn with the count on both CleanStale paths).

### Tests (each seen failing first unless marked)
- `TestDiscardPartialCopy_ReportsAPartialCopyItCannotRemove` (R-6.4.9): FAILED first (EBUSY not in the chain).
- `TestRoot_RootThatIsAFIFOAnswersWithoutOpeningIt` (R-6.4.10): FAILED first. My first version's release did not hold the FIFO's write end open, so the second OpenRoot blocked again and `go test -timeout 2m` killed the binary; the stack showed it blocked in `os.openRootNolog`, the bug itself. The fixed test keeps the write end open until the goroutine answers, then fails at 1 s.
- `TestCleanTransferArtifacts_ReportsStaleSessionCleanupWithTheCount` (R-5.2): "removal refused" FAILED first (no `count`); "removed" is a pin.
- `TestEngineClose_ReportsStaleSessionCleanupWithTheCount` (R-5.2, internal/sync): "removal refused" FAILED first (level=DEBUG, no count); "removed" is a pin.
- `TestAtomicWriteFile_CreatesItsFoldersOwnerOnly` (R-6.8.23): "new folders" and "folder created by the mutation lock" FAILED first (0o755); "existing folder keeps its mode" is a pin.
- `TestGolangciConfig_DepguardPackagePatternsNameExistingPackages` (R-6.10.2): FAILED with a temporarily renamed pattern (`**/internal/sharedrefs/*.go`), passes with the real one.
- depguard proofs (targeted `--enable-only depguard` runs): with no extra import, the ruled packages give 0 issues, so tokenfile→fsroot, retry→perf and fsroot→localpath pass. Throwaway files importing `internal/errclass` in tokenfile, retry, fsroot and sdnotify (a new leaf) gave 4 issues, each naming its own list. The files were removed and `git status` shows none.

### Checks
- Non-race `go test -count=1` over fsroot, localpath, tokenfile, driveops, config, devtool, logfile, cli, graph: all ok. internal/sync: only `-run TestEngineClose_ReportsStaleSessionCleanupWithTheCount`, ok (the full sync suite is yours).
- `go build ./...`, `go vet ./...`, `GOOS=darwin go vet` over this pass's packages (internal/sync included), `go vet -tags 'e2e e2e_full' ./e2e/...`: pass.
- Whole-repository `golangci-lint run ./...`, run once (527 s): 2 issues, both in the new FIFO test (`fsroot_test.go:415` gosec G304 on opening the test's own FIFO, `:418` testifylint require-error). W14-lint.log keeps that run. Both were fixed (a `//nolint:gosec` with its reason, `require.NoError`), the FIFO test reran ok, and `golangci-lint run ./internal/fsroot/...` then gave `0 issues.` (W14-lint-fsroot.log). I did not run the whole repository a second time on the loaded host; nothing outside `fsroot_test.go` changed after it.

### Deviations
- Item 2: a stat that fails for a reason other than absence (say EACCES on a folder above the root) now returns the stat error ("opening root X: stat X: permission denied") where it used to return OpenRoot's error for the same cause.
- Item 2: a gap remains. The stat sees a directory, something swaps it for a FIFO, and only then does OpenRoot run; it still blocks then, because `os.OpenRoot` takes no O_NONBLOCK. It cannot be closed from fsroot.
- Item 3: the engine keeps its `deleted` key and the post-pass cleanup its `count` key, as each already used for the success line; I did not unify them.

### Seen and not fixed
- `internal/driveops/cleanup.go:44` (now with the count): a persistent session-removal failure Warns after every sync pass, while the trash purge and the protected-root refresh beside it warn once per streak (R-6.6.30, `Engine.trashPurgeFailing`). Fix: give the session cleanup the same streak gate in `engine_housekeeping.go`.
- `internal/driveops/stale_partials.go:89-96`: logs a Warn per partial it cannot remove and returns only a walk failure. That is the opposite shape to CleanStale's now, so the post-pass caller reports the two cleanups differently. Fix: collect and return them as CleanStale does, and let the caller Warn once.
- `internal/cli/get.go:21` creates `get` target folders 0o755. This is user content, not state, and conventional, but it differs from the sync folder's 0o700 (`cli/sync_dir.go:11`). Fix: decide one rule for folders the client makes in user space.

## Review (coordinator, 2026-11-12; amended into the one commit, now `4e76aac7`, on `origin/main` 27e76cc3)

Third pass accepted. The three "Seen and not fixed" entries are closed:
- Session cleanup's Warn after every run, and the partial cleanup's Warn per stuck file: fixed in review. `CleanStalePartials` returns `(int, error)` with every partial it cannot remove joined to a failed walk; a partial already gone is neither a failure nor counted, and the check unwraps (`errors.Is`, where `os.IsNotExist` did not). `CleanTransferArtifacts` returns `TransferArtifactCleanup{PartialsRemoved, PartialsErr, SessionsRemoved, SessionsErr}` and logs no failure. The engine reports each through `reportHousekeepingOutcome`, which all four housekeeping steps now share: one Warn per failure streak, Debug on repeats, Info on recovery, and the count removed at Info. `Engine.Close` keeps its own Warn with `deleted`. R-6.6.30 names the two cleanups.
- The three folder-mode rules for a user's tree: planned as W18.

Corrected before the PR: my first description of the partial cleanup's defect ("a Warn on every pass") was wrong. `postSyncHousekeeping` runs once when a one-shot run that executed something ends and once when a watch's bootstrap settles; watch maintenance repeats only the trash purge. The defect was one Warn per stuck file and nothing returned. Every comment, test comment and doc (`sync-engine.md`, `drive-transfers.md`, R-6.6.30) says it that way.

Also fixed in review:
- `TestGolangciConfig_DepguardPackagePatternsNameExistingPackages` skipped negated patterns; it strips the `!` first.
- `TestRoot_RootThatIsAFIFOAnswersWithoutOpeningIt` failed a good run after 1 s on a loaded host; the bound is 10 s (only a failing run waits for it).

Considered and rejected: adding the session cleanup to the hourly watch maintenance. An expired session file costs nothing while it waits for the next run's end.

Tests of the fix, each seen failing first: `TestCleanStalePartials_ReturnsEveryPartialItCannotRemove`, `TestCleanTransferArtifacts_ReturnsEachCleanupsFailureAndLogsNone`, `TestPostSyncHousekeeping_WarnsOncePerStreakWhenLeftoversCannotBeRemoved`.

Checks: full non-race `go test` over `internal/sync`, `driveops`, `cli`, `multisync` (ok, 605 s); `devtool`, `fsroot`, `driveops`, `localpath`, `config` once more after the last two test edits (ok); targeted lint over driveops, sync, devtool, fsroot: `0 issues.`; no plan ids or process words in the diff.
