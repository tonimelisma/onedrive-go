# H1 report: fsroot mutations through os.Root

## What changed
- `internal/fsroot/fsroot.go`: `rootHandle` gains `Lstat`, `MkdirAll`, `Remove`, `Rename`; `rootOps` is only `openRoot` and `tempSuffix` (the `mkdirAll`, `remove`, `rename`, `createTemp`, `lstat`, `syncDir` fields are gone); one `withRoot` helper opens, runs and closes the handle for every operation; `Remove`, `Rename`, `CreateTemp`, `AtomicWrite` (subdirectory mkdir, temp file, rename, directory sync, cleanup on one handle), `Stat`, `ReadDir`, `Open`/`OpenFile` and `normalizeNotExist` all resolve through it; temp files are claimed with `OpenFile(O_RDWR|O_CREATE|O_EXCL, 0600)` on `crypto/rand.Text` suffixes, retried on `os.ErrExist` up to `tempNameAttempts` (100); the root directory itself is still created by path (`Root.MkdirAll`). Exported API unchanged.
- `internal/fsroot/fsroot_test.go`: new containment and collision tests; failure injection rewritten onto a fake `rootHandle` (`fakeRootHandle`, `interposeFakeHandle`); `fakeTempFile` counts closes.
- `spec/design/system.md`: managed-state bullet states the rooted guarantee with test citations; ops bullet describes fsroot's two seams; atomic-write bullet says fsroot has no unsupported-dir-sync tolerance.
- `spec/design/graph-client.md`: the token-file "one managed-state root capability" sentence now cites R-6.4.10 and the tests.
- `spec/requirements/non-functional.md`: new `R-6.4.10` [verified], managed-state containment.

## Tests added (all `// Validates: R-6.4.10` unless noted)
- `TestRoot_Remove_RefusesSymlinkedParent`: remove of `sub/x` with `sub -> outside` fails, outside file intact. FAILED before the fix (nil error, file deleted).
- `TestRoot_Rename_RefusesSymlinkedParent` (source, destination): neither pulls a file in nor replaces one outside. Both FAILED before the fix.
- `TestRoot_AtomicWrite_RefusesSymlinkedParent`: outside target intact, no temp file outside. FAILED before.
- `TestRoot_AtomicWrite_RefusesDirectoryCreationBelowSymlinkedParent` (the MkdirAll-below path, reachable only through AtomicWrite): nothing created outside. FAILED before.
- `TestRoot_CreateTemp_RefusesSymlinkedDir`: no temp file outside. FAILED before.
- `TestRoot_AtomicWrite_FollowsSymlinksThatStayInside`: a symlinked root path and an in-root link still work (passed before and after).
- `TestRoot_AtomicWrite_TempNameCollisionRetries`: a taken name (another writer's file) and a planted symlink to an outside file are skipped untouched; the third suffix wins.
- `TestRoot_CreateTemp_StopsAfterBoundedNameCollisions`: gives up after `tempNameAttempts` with `os.ErrExist`.
- `TestRoot_CreateTemp_RejectsPatternWithSeparator` (no requirement): parity with `os.CreateTemp`.
- `TestWriteTempFile_ReportsEachStepFailure` (no requirement): chmod, write, sync, close failures each report their step and close once.
- Rewritten: `TestRoot_AtomicWrite_FailureInjection` (create, chmod via closed file, write via read-only file, rename plus cleanup failure; temp removed is the one created), `TestCleanupTempOnError_JoinsRemovalError`, `TestAtomicWriteSyncsTargetDirectory` (syncs `nested` through the handle), `TestAtomicWriteReportsDirectorySyncFailure` (a real `Sync` on a closed directory fails and is reported).

## Checks
- Pre-fix run of the containment tests: 6 of 6 cases failed with "An error is expected but got nil".
- `go test -race -count=1` (GOTOOLCHAIN=go1.26.11): fsroot, tokenfile, logfile, config, driveops all `ok`.
- `go build ./...`, `GOOS=darwin go build ./...`, `go vet ./...`, `GOOS=darwin go vet ./internal/fsroot/`: all pass.
- Commit `924d54c5` on `fix/fsroot-os-root`, 5 files.
- golangci-lint `./internal/fsroot/...`: `0 issues.` (reports/H1-lint.log).

## Deviations
- The brief and analysis say the directory sync has a "tolerance for filesystems that cannot sync a directory"; the code at 68ca8f3b has none (every error is reported and `TestAtomicWriteReportsDirectorySyncFailure` pins it). Kept as is and documented.
- No `mkdirRoot` op: nothing injects it, so `Root.MkdirAll` calls `os.MkdirAll(r.dir)` directly.
- Error wording: a root that fails to open now reports the wrapped os error ("opening root X: ...") instead of a bare `os.ErrNotExist`; AtomicWrite creating a missing root says "creating root directory". `errors.Is(err, os.ErrNotExist)` holds as before; "creating temp file" is kept for `internal/graph`'s test.
- Sync-step and close-step failures are now injected into `writeTempFile` directly: a real `*os.File` returned by a fake handle cannot be made to fail those two steps.
- In the worktree `go` resolves to 1.27.1 (GOTOOLCHAIN=auto never downgrades); every check here ran with `GOTOOLCHAIN=go1.26.11`.

## Recommendations
- fsroot's `normalizeNotExist` still maps any failure on an absent name to `os.ErrNotExist`, so EACCES/ENOSPC on a create can read as "missing"; adopt synctree's errno guard (R-2.10.60 style).
- fsroot has no tolerance for EINVAL/ENOTSUP on a directory fsync (FUSE, SMB home folders): config and token writes there would fail every time; consider `localpath.IsDirSyncUnsupported`.
- `Root.CreateTemp` has no caller outside the package; unexport it.
- `spec/design/threat-model.md` evidence row for rooted boundaries could cite the new containment tests.
- The state database is opened by SQLite by path; only its folder creation goes through fsroot, so R-6.4.10 deliberately omits it.

## Unfinished
- Nothing.

## For the reviewer to run
- `internal/sync` (store open: `-run` on the store-open tests), `internal/cli` (cleanup.go, auth_flow.go `writeManagedFile`), `internal/graph` (`TestSaveToken_CreateTempError` and token tests).
