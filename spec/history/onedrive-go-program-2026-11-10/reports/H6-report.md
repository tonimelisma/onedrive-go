# H6 report: one JSON encoder for command output

Commit `10891567` on `91113542`, branch `refactor/cli-json-encoder`, not pushed. 33 files.

## What changed
- `json_output.go`: `newJSONOutputEncoder` (HTML escaping off) feeds `writeJSONDocument` (two-space indent) and `writeJSONLine` (compact); one error text `encode JSON output: ...`; `emptyIfNil`.
- The 17 own encoders now call `writeJSONDocument`: `drive_search`, `mkdir`, `cp`, `get` (2), `ls`, `recycle_bin` (2), `drive_list_render`, `stat`, `mv`, `perf`, `rm`, `status_render`, `put` (2), `shared`. With the 14 existing calls: 31 `writeJSONDocument` calls plus 1 `writeJSONLine`.
- `withEmptyLists` on the 12 types that can hold a list: resume, drive search, trash list, logout, get folder, put folder, drive list, status (pointer receiver, fills each account's drives too), recycle-bin empty, shared, and the new named list types `lsJSONOutput`, `recycleBinListJSONOutput` (same bytes). Printers hand values over as `x.withEmptyLists()` (the `auth_flow`, `sync_pause_resume`, `trash`, `recycle_bin_flow` edits are that call only).
- `sync_console_json.go`, `sync_console.go`: `emitJSON` writes through `writeJSONLine` under `mu`; the first failure prints `Warning: sync --json output is incomplete: <err>` once via `printErr`, later lines are still written (field `jsonFailureReported`).
- `.golangci.yml`: forbidigo rule and two exclusion rules (below). `spec/design/cli.md`: one-encoder rule, escaping, empty-list rule, stricter contract description, Verified By row. `spec/design/system.md`: lint paragraph names the rule. R-6.6.33 says nothing the change makes false; unchanged.

## Lint rule (and proof)
`pattern: ^json\.(NewEncoder|Marshal|MarshalIndent)$`, `pkg: ^encoding/json$`, `msg: encode command output only through writeJSONDocument or writeJSONLine in internal/cli/json_output.go`; exclusions by that text: `path-except: internal/cli/`, and `path: (_test\.go|internal/cli/json_output\.go|internal/cli/control_client\.go)` (comment names control_client.go's control-socket request body as the non-output encode). Proof: a planted `internal/cli/zz_planted_encoder.go`, lint of `./internal/cli/...`:
`zz_planted_encoder.go:9:9: use of json.NewEncoder forbidden because "encode command output only through ..." (forbidigo)` and `:13:9: use of json.Marshal forbidden ...`; no hit on `control_client.go` or tests. File removed.

## Tests
- `TestWriteJSONDocument_Format`, `TestWriteJSONLine_Format`: byte-exact; `&<>` unescaped; `[]`; a channel value returns the error and writes nothing.
- `TestSyncConsole_EmitJSONFailureIsReported`: two failed lines, one warning, the next line written.
- `TestJSONOutputTypes_NoListPrintsNull`: each of the 30 declared output types probed with lists nil at depths 0-6; a list-holding type without `withEmptyLists` fails.
- `TestJSONOutputCalls_PrintDeclaredTypesWithTheirListsFilled`: parses the package, types every value passed to the two functions from the source, fails unless a contract case declares the type and a list-holding value goes through `withEmptyLists`; also fails for a JSON-tagged type no declared type holds, and for a stale declaration.
- `TestCommandTree_EveryLeafHonorsJSON` extended: each case declares `schema`/`otherSchemas`; checks opening bracket, two-space indent, one trailing newline, unknown fields, null lists; lines compact and newline-terminated.
- `TestGetFolderJSONOutput_EmptyErrors` pinned `null`; now pins `[]` through the printer. `TestPrintItemsTable` checks the caller's slice keeps its order.
- Red first: all new tests failed before the change except the contract extension (its scenarios already matched). Plants caught: dropping a list from `putFolderJSONOutput.withEmptyLists`, and dropping the call in `trash.go`.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -race -count=1 ./internal/cli/...`: pass (whole package, its list/table tests included).
- Lint `./internal/cli/...`: 0 issues (`H6-lint.log`).
- Goldens: none changed (status goldens hold no empty list and no `&<>`); `-update` (golden_test.go) not used.
- e2e: no expectation changed; searched `e2e/` for `null`, the three Unicode escapes, string literals holding `&<>`, and nil checks on list fields.
- Hand-built JSON search: grep of non-test `internal/cli` for `json.NewEncoder|Marshal|MarshalIndent`, `.Encode(`, `SetIndent`, `RawMessage`, `json.Compact|Indent|Valid`, `encoding/json` imports, brace/bracket string literals and `Fprintf` of braces: only the 17 copies, `emitJSON`, `control_client.go:252`, and the slog JSON log handlers (see Not fixed 5).

## Bytes that change
(b) `&`, `<`, `>` in any name. (c) `shared` `accounts_requiring_auth`, `get`/`put` folder `files` and `errors`, `status` `accounts` and an account's `drives`: `null` became `[]`. Everything else already printed `[]`.

## Kept as absent by meaning (all `omitempty`, none prints null)
Pointers: status `sign_in_required`, `storage`, `auto_retry`, `sync_state`, `sync_state.perf`; sync event `size`, `duration_ms`, `failed`, `will_retry`. Map: sync event `counts`. Lists left `omitempty`: `accounts_requiring_auth` (drive list, search), `accounts_degraded` (drive list, shared), status `protected_reserved_paths`, `shared_folders`, `retrying_paths`, `issues`, issue `paths`.

## Deviations
- The emitJSON failure is driven through a failing writer: `syncJSONEvent` cannot fail to encode. Reported once per console, so a closed stdout does not flood stderr; text mode still drops stdout write errors silently (Not fixed 4).
- `statusOutput.withEmptyLists` lives in `status_render.go` beside its printer, beyond the encoding lines; pointer receiver because gocritic hugeParam refuses a 168-byte value.

## Also fixed
- `drive_search.go`: optional variadic `authRequired` became a plain parameter (`optionalAuthRequirements` removed); dead nil checks and a constant expression in the site printer.
- `stat.go`: test-only wrappers removed, `*WithOptions` renamed; `stat.go`, `recycle_bin.go` (2) reuse `itemType`.
- `ls.go`: `printItemsTable` sorted the caller's slice in place; sorts a copy.
- `perf.go`: dead nil check, import grouping. `drive_list_render.go`: comment that described history.

## Not fixed (5)
1. `internal/cli/status_render.go:122` `normalizeStatusAccounts` builds `Drives` with `append([]statusDrive(nil), ...)` (nil when empty) and `withEmptyLists` fills it again: two normalizing steps for one document. Fix: have it produce non-nil lists, once the status cleanup lands.
2. `internal/cli/get.go:44`, `internal/cli/put.go:39`: `getFolderJSONOutput` and `putFolderJSONOutput` are one shape with one normalizer. Fix: one folder-transfer document type.
3. `internal/cli/perf.go:83`, `:86`: "live perf capture unavailable; check logs" written twice (case and default); fallthrough is refused by gocritic, dropping the case by exhaustive. Fix: a named message constant.
4. `internal/cli/sync_console.go:577`: `write` drops stdout write errors through a no-op `if`; JSON mode now reports the first, text mode never does. Fix: one policy for both modes.
5. `internal/cli/root.go:1002`, `:1214`: `slog.NewJSONHandler` writes JSON logs (console `log_format = json`, log file) outside `json_output.go`, with HTML escaping on. Logs, not command output; left. Fix if wanted: decide whether log JSON follows the escaping decision.

Unfinished: none. Other packages to test: none touched; the lint rule is repository-wide config, so the full `verify lint` should show it skipping issues outside `internal/cli`.


## Review (2026-11-13, about 16:30 Pacific)

A read-only reviewer read commit `10891567`; its findings are in `scratchpad/R-H6-findings.md` (no must-fix; eight should-fix; five smells outside the diff). My reading of them:

- **Accepted for a second pass** (brief `scratchpad/H6-pass2-brief.md`, a new writer, after the dead-code change it stacks on is accepted): docs and the requirement state the escaping and no-null rules; encode and write errors are told apart; the lint exemption is narrowed to a file that holds only the control-socket encode; the list probe has no silent depth limit; the pass-through wrappers go; one normalizing step per document (status included); `accounts_requiring_auth` is always present in both documents; one folder-transfer document type for `get` and `put`; the `perf.go` message constant; the "second forbidigo rule" wording.
- **Its own plan row (W40)**: what a sync does when a write to stdout fails (should-fix 3, smell 4, "Not fixed 4"). Decision in PLAN.md: the sync finishes, says so once on stderr, and exits 1 when nothing else failed, in text and in JSON.
- **Refuted**: "Not fixed 5" and the reviewer's smell 5 say `slog.NewJSONHandler` writes with HTML escaping on. `go doc log/slog.JSONHandler.Handle` says values are formatted as with an encoder with `SetEscapeHTML(false)`. Nothing to change.
- **Not yet run by me**: the whole `internal/cli` tests, build, vet, tagged vet on the first pass. They run on the second pass's commit.

## Second pass

Commit `eecd2c47` on `origin/main` at `bd2711b0` (fetched; main may have moved since and was not rebased onto again), 50 files. Not pushed.

### Rebase (4 conflicts, main kept, this change on top)
- `stat.go`: main deleted the two-argument `printStatText` wrapper and kept `printStatTextWithOptions` with a local `itemType` variable. Kept main's deletion; applied this change's rename of the one real printer to `printStatText(w, item, opts)` and its use of the shared `itemType(item)`. This is a judgment call: the name is reused for the surviving three-argument function, not to bring back the deleted wrapper. It is symmetric with `printStatJSON`.
- `stat_test.go`: the three call sites take the renamed function. Main's `TestPrintStatTextWithOptions*` became `TestPrintStatText*` (no spec cites them).
- `sync_console_json.go`: main had added a comment saying a Marshal error is dropped. This change's write-failure behavior replaces that, so kept this change's comment. Main's `still_open`/`incomplete` summary is untouched.
- `sync_console_test.go`: kept main's new `TestSyncConsole_JSONSummaryOfADriveThatLeftNothingIsDone` and this change's `flakyWriter` plus `TestSyncConsole_EmitJSONFailureIsReported`. Main's `Validates: R-6.6.19, R-6.6.28` stays on the next test.

### Decisions
- (a) `docs/scripting.md` states the escaping rule, the `[]` rule (naming the lists left out when empty) and that `accounts_requiring_auth` is always present. It names what changed: `&<>` everywhere; `null`→`[]` in `shared` `accounts_requiring_auth`, folder `get`/`put` `files`/`errors`, and `status` `accounts`/`drives`; and the new presence in `drive list`/`drive search`. R-6.6.33 states both rules; `cli.md` agrees.
- (b) `writeJSONOutput` encodes into a buffer, then writes it in one call. The errors are "encode JSON output: …" and "write JSON output: …". `newJSONOutputEncoder` and `encodeJSONOutput` are gone.
- (c) The encode moved to `internal/cli/control_request.go` (`encodeControlRequest`, unexported, nothing else in the file). The exclusion path is now `(_test\.go|internal/cli/json_output\.go|internal/cli/control_request\.go)$`. Proof (scratch log `H6-lint-proof.log`, plants removed): `control_client.go:312:9: use of json.Marshal forbidden … (forbidigo)` and `zz_planted_encoder.go:6:9: … (forbidigo)`; no hit on `control_request.go` or tests.
- (d) `listDepth` walks the type with no depth limit. It keeps an on-path set (recursion) and a per-struct memo, and returns the deepest list's level. It fails for a list inside a recursive type, or under a map whose key is not a string. The probe runs levels 0 to depth+1. Arrays are probed in place. `jsonProbeDepth` is gone.
- (e) Removed `printCpJSON`, `printMkdirJSON`, `printMvJSON`, `printRmJSON`, `printGetJSON`, `printGetFolderJSON`, `printPutJSON`, `printPutFolderJSON`, `printRecycleBinRestoreJSON` and `printPerfCaptureJSON`; callers call `writeJSONDocument`. Kept, because each builds its document from parts: `printItemsJSON`, `printRecycleBinJSON`, `printStatJSON`, `printDriveListJSON`, `printDriveSearchJSON`, `printSharedJSON`, and `printStatusJSON` (builds via `newStatusOutput`; called twice from `status_snapshot.go`).
- (f) Pre-fills dropped in `recycle_bin_flow.go`, `sync_pause_resume.go`, `trash.go`, `auth_logout.go`, `ls.go` and `recycle_bin.go`; each type's `withEmptyLists` is the one step. For status, `statusOutput.withEmptyLists` is gone. `newStatusOutput` normalizes once, through `normalizeStatusAccounts`, which now returns non-nil accounts and drives (also for nil input). `computeSummary` no longer re-normalizes, and `printStatusText` uses `newStatusOutput` too. The list tests learned a declared normalizer by name (`schemaBuiltBy("newStatusOutput", …)`); the scan checks the value comes through the step its declaration names.
- (g) `accounts_requiring_auth` has no `omitempty` and is filled in the drive list. Pinned by `TestPrintDriveListJSON_NilSlicesRenderAsEmptyArrays`.
- (h) One generic `folderTransferJSONOutput[F]` with one `withEmptyLists`, in `format.go` beside `bulkTransferError`; field names and types unchanged. `TestGetFolderJSONOutput_Golden` and `TestPutFolderJSONOutput_Golden` were written and their goldens generated (`-update`) on the old types. They passed unchanged after the refactor. Compared with main, the bytes differ only by (a): `&<>` unescaped, `errors` `[]`.
- (i) `perfCaptureUnavailableMessage`; the three constant errors use `errors.New`.
- (j) `system.md` names the rule by its message.
- (k) untouched: `sync_console.go` `write`, the JSON line policy, `root.go` slog.

### Deviations
- (g) also covers `drive search` (`drive_search.go`), a third document carrying the field with `omitempty`. R-3.3.11 calls it an array, and leaving it out kept the same inconsistency. Pinned by `TestPrintDriveSearchJSON_EmptySlice`.
- Tests named after the removed wrappers were renamed to the type they test (`TestCpJSONOutput` and the like); none is cited by a spec.

### Tests (red first)
- Seen failing before the fix: `TestWriteJSONOutput_WriteFailureIsNamed` (read "encode JSON output: broken pipe"), `TestSyncConsole_EmitJSONFailureIsReported` (same), `TestPrintDriveListJSON_NilSlicesRenderAsEmptyArrays` and `TestPrintDriveSearchJSON_EmptySlice` (field absent), `TestListDepth_FindsTheDeepestList` (a stub returning the old fixed 6 missed 7 levels and the recursive type).
- The two goldens pin bytes that must not change (green before and after).
- Plants caught, then reverted: a nil-`Drives` `normalizeStatusAccounts` (NoListPrintsNull at depth 1, through `newStatusOutput`); `printStatusJSON` writing a variable instead of `newStatusOutput(...)` (the scan); the drive list's auth fill dropped (NoListPrintsNull, plus the pin).

### Also fixed
- `get.go:152` (`localFileTarget`), `put.go:138` (`resolvePutFileTarget`), `sync_console_test.go:509`: comments that told history now say what the code does.
- `recycle_bin.go`: `formatRecycleBinJSON`/`formatRecycleBinTable` write, so they are now `printRecycleBin*` (and their tests).

### Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -count=1 ./internal/cli/...`: pass, run twice (the second on the final tree, 9.7 s).
- Spec citations: every `Test…` a design doc cites exists. The only unmatched names are in `spec/reference/*` and the `TestIntegration_` prefix, as on main.
- e2e decoding: `accounts_requiring_auth` (`w7_live_caller_proof_e2e_test.go:42,53`), status `accounts`/`drives` (`status_helpers_test.go:18,52`, `w7…:26-36`, `drive_list_e2e_test.go:332-390`, `fileops_full_e2e_test.go:138`) all decode into slices without nil checks, so `[]` decodes. No e2e decodes folder `get`/`put` or `drive search`. `internal/devtool/bench.go:751` accepts a present `[]`.
- Lint `./internal/cli/...` on the final tree, behind the host lock: `0 issues.` (`H6-lint.log`). The proof run before it found one real issue, an unchecked type assertion in `json_lists_test.go`, which is fixed.

### Seen and not fixed (3)
1. `internal/cli/status_snapshot.go:253`: passes `[]statusAccount{}` to mean "no accounts", which `newStatusOutput` now handles for nil. Fix: pass nil (status files are in flight elsewhere).
2. `internal/cli/control_client.go:283`, `:318`: the decode-failure block (message, close error, internal `controlDaemonError`) is written twice. Fix: one helper both call.
3. `internal/cli/recycle_bin.go:100`: `itemType` is shared by `stat.go` and the recycle-bin files but lives in `recycle_bin.go`. Fix: move it beside `formatSize` in `format.go`.

## Third pass (review fixes)

Commit `4d3b8e20` on `bd2711b0` (the same one commit, amended; not pushed).

### What changed
- `internal/cli/control_client.go`: one helper, `controlResponseReadError(resp, decodeErr, closeErr)`, turns a failed decode or close into the internal `controlDaemonError` for both `readControlErrorResponse` and `decodeControlJSONResponse`; each still closes the body itself, once. `capturePerf` encodes its request; `postJSONInto` takes the encoded payload (nil for none).
- `internal/cli/control_request.go`: `encodeControlRequest` takes `synccontrol.PerfCaptureRequest`, the one body the CLI sends, instead of `any`; its comment says the rule enforces a direct call.
- `internal/cli/json_output.go`, `spec/design/cli.md`, commit message: say that a direct call of `json.NewEncoder`, `json.Marshal` or `json.MarshalIndent` elsewhere fails lint, and that `encodeControlRequest` takes only the perf capture request.
- `internal/cli/format.go`, `format_test.go`: `itemType` with `typeFile`/`typeFolder`, and `TestItemType_File`/`_Folder`, moved from `recycle_bin.go`/`recycle_bin_test.go`.
- `internal/cli/json_lists_test.go`: `listDepth` refuses an interface, a pointer to a list and an embedded unexported struct, and each error names the type and the field (`T.Field:` for every struct it passes through); fields tagged `json:"-"` are skipped. `fieldCanPrintNullList` no longer strips pointers before its omitempty test (a set pointer to a nil list is not omitted). The comments at the probe test, `canPrintNullList`, `nullListsIn`, `listDepth`, its test and `listProbe` say what is checked, and that a self-encoding type is not looked into. One comment at `cliSource` names the three things the scan trusts.
- `internal/cli/sync_console_test.go`: the `flakyWriter` comment is rewritten.
- `spec/requirements/drive-management.md` R-3.3.10: `accounts_degraded` is left out when empty. R-3.3.11 needed nothing: `drive search` has no `accounts_degraded`, and both of its lists are always present.
- `spec/requirements/non-functional.md` R-6.6.33: one sentence: the error on stderr names the step that failed (`write JSON output: ...` or `encode JSON output: ...`).

### Tests
- `TestDecodeControlJSONResponse_ClosesOnceAndNamesEachFailure` (new): a success answer that does not decode (no test reached that path), decode and close failing together and close failing alone, on both paths; it checks the exact message, the status, `internal_error`, and that the body is closed exactly once.
- `TestListDepth_FindsTheDeepestList`: new refused cases (interface field, interface below containers, pointer-to-list field, embedded unexported struct), and every refused case now checks that the error names its type and field. The "pointer to a list" depth case became a pointer to a struct of lists (depth 1). A `json:"-"` case pins that unprinted fields are skipped.
- A temporary diagnostic test (deleted) walked all 30 declared types: no interface, no pointer to a list, no embedded unexported struct.

### Stopped: shape (b), a type that encodes itself
- A printed type has one: `perf.Snapshot.StartedAt` and `UpdatedAt` are `time.Time` (`internal/perf/collector.go:43-44`), reached as `statusOutput.Accounts[].Drives[].SyncState.Perf`. Failing on every self-encoding type would fail `TestJSONOutputTypes_NoListPrintsNull`, so (b) is not implemented. Self-encoding types stay leaves, and the comments say so.
- Options: (1) refuse every self-encoding type except a named set known to print a scalar (`time.Time`); (2) refuse only self-encoding types whose Go kind is a slice, map or interface, or a struct with an exported field. Option 2 catches `json.RawMessage` and lets `time.Time` through.

### Deviations
- Item A uses one helper, not two. A helper that closes the body hides the close from bodyclose: the first lint run flagged `getJSON` and `postJSONInto`. So each caller closes the body, and one helper builds the error from the decode and close results.
- Item C (b): not done (above).

### Where the review was wrong
- Finding 4 ("no type the CLI prints has any of these shapes"): wrong for (b), because of `time.Time` in `perf.Snapshot`.
- Finding 2's helper for the close block: bodyclose does not accept a close made in a helper.
- Note 6 on `json_output.go:13-16`: that text already named the three functions. The overclaim was in `control_request.go`, `cli.md` and the commit message.

### Also fixed
- `sync_console_test.go:509`: "is said on stderr" now reads "is reported". `:531` and `:558`: two comments told history, with `Validates` not on the line above `func`. They now describe the behaviour, and `Validates` sits above `func`.

### Checks
- `go build ./...` and `go vet ./internal/cli/...`: pass. `gofumpt -l` and `goimports -l` on the 10 changed Go files: no output.
- `go test -count=1 ./internal/cli/...`: pass (12.1 s, final tree).
- Lint on the five packages, behind the lock: `0 issues.` (`H6-lint.log`). The first run found 3 issues, now fixed: two bodyclose, one gocritic sloppyReassign. There was no forbidigo issue outside `internal/cli`, so the `path-except` exception holds.
- `grep -c "u0026" docs/scripting.md`: 2. `git status --short`: only the `.testdata` link that devtool made (untracked; it is not part of the change).

### Seen and not fixed (1)
1. `internal/cli/shared.go:89`: `sharedItemType(isFolder bool)` repeats the folder/file choice that `itemType` makes. Fix: one helper in `format.go` that takes the bool and that `itemType` calls (`shared.go` was out of scope here).


## Review of the third pass (2026-11-14, about 03:55)

Commit `1103ceeb` on `bd2711b0` (the one commit, amended by the reviewer; not pushed).

- Read by file against the second pass (14 files, +298/−151): the control client's one error helper, the request encoder's narrowed parameter, the list walk and its test. Nothing to send back.
- **Shape (b) decided and done: fail closed.** `listDepth` refuses every type that encodes itself, naming it, except one `printsScalarItself` names (`time.Time`, or a pointer to one). The writer's second option (refuse by Go kind) lets a struct with only unexported fields and a `MarshalJSON` method through, which is exactly the shape that hides a list. The test saw the new cases fail first ("An error is expected but got nil" for raw JSON), then pass: `json.RawMessage`, a type with its own `MarshalJSON`, and a struct that embeds a time (which makes the whole struct encode as a time) are refused; a struct with a time and a pointer to a time holds no list. All 30 declared types pass: `time.Time` is the only self-encoding type they reach.
- **The "Seen and not fixed" entry is fixed**: `sharedItemType` is gone; `folderOrFile(isFolder)` in `format.go` makes the choice once and `itemType` calls it.
- Checks on `1103ceeb`: `gofumpt -l` on the three files, no output; `go build ./...`, `go vet ./internal/cli/...`, `go test -count=1 ./internal/cli/...` pass. Lint runs in the pre-PR gate.



## Merged as #909 (`dd0932f4`, 2026-11-14 about 13:38 PST)

- One commit `dfeea9a7` on main at `fa601d30`. The pre-pr gate passed on its first full run (19 minutes); pre-merge passed with no review threads; post-merge passed and removed the `cli-json-encoder` worktree and its branch.
- Installed `dd0932f4`; the watch client reports "Syncing, watching for changes", 19 files.
- Live shape check on the test account with the installed binary (shapes only, no values printed): `status --json`, `status --perf --json`, `ls / --json`, `drive list --json` and `shared --json` each exit 0, parse as JSON, hold no null anywhere and no escaped HTML character. `recycle-bin list --json` exits 1 with the refusal for a personal account, which is the same text the tree held before this change; so that command's document was not seen live. Its golden file is the proof.
- Not seen live: `drive search --json`, folder `get` and `put` with `--json`, a `sync --json` line that cannot be written. Their tests are the proof.
- No schema change and no store-generation bump.
- Free to start after this: the rows that edit `internal/cli` output on top of it.
