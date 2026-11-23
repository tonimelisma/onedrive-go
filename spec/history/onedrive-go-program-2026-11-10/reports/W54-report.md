# Report: an existing name after a 409 is found with one lookup

Commit 546f6d3c on 341d9820, not pushed. DoR: W54-dor.md (second pass).

## What changed
- `internal/graph/items_fetch.go`: `GetChildByName(ctx, driveID, parentID, name)`, one GET of `items/{parent}:/{name}:`; the answer counts only when it sits directly in that parent (for the alias `root`, when `parentReference.path` is the top of the drive) and its name is `namekey.Equal`; else `ErrNotFound`. `ErrNameNotPathAddressable` (escaped longer than `maxPathAddressedLength`, or holding `:`) and `ErrInvalidPath` (empty) before any request. `childPathRoute` builds the route once; `fetchItemByPathChunks` uses it too.
- `internal/driveops/session.go`: `ChildLookup` and `FindExistingChild` (read the name; list once only for a name the route cannot carry or a read that finds nothing equal); `EnsureFolder` uses it and says when a file holds the name; `resolveConvergingPath` returns once.
- `internal/driveops/transfer_upload.go`: `UploadFileToItem` uses `uploadMtime` and `recordUploadPerf`; both documented.
- `internal/cli/put.go`: `putFile` uses `FindExistingChild` (`putChildLister` gone); `putJSONPath` gives both JSON forms `"/" + CleanRemotePath(path)`; the default remote path is computed once.
- Tests: graph `items_fetch_test.go` (new); driveops `session_test.go`, `transfer_manager_test.go`; cli `put_overwrite_test.go`, `folder_transfer_test.go`, `json_contract_test.go`, `json_contract_fake_graph_test.go` (the fake reads one name, overwrites by ID, counts listings and reads), `put_test.go`.
- Spec: `drive-transfers.md` (what a put does when the name exists, and the cost), `cli.md` (the JSON path form), `graph-client.md` (one bullet).

## Tests added or changed (each seen failing first, except the perf one, which holds a refactor)
- `TestGetChildByName_SendsTheNameAsOneEscapedSegment`: `#`, `%`, space, trailing dot, NFC and NFD `é` on the wire.
- `TestGetChildByName_AcceptsTheStoredSpellingOfTheSameName`: case and ß/SS differences accepted, stored spelling returned.
- `TestGetChildByName_RefusesAnAnswerThatIsNotTheAskedName`: another parent, an unequal name, the root alias without or below the top path.
- `TestGetChildByName_PlacesAnAnswerForTheRootAliasByItsParentPath`; `TestGetChildByName_RefusesANameTheRouteCannotCarry` (long, long escaped, colon, empty; no request).
- `TestEnsureFolder_Conflict`, `_ConflictFindsTheFolderInAnotherCase`: one read, zero listings (the fake counts).
- `TestEnsureFolder_ConflictListsOnceWhenTheReadCannotFindTheName`: the three fallback cases by name; `_ConflictNotFound` (listing finds nothing: today's error); `_AReadThatFailsIsTheError` (403, no listing); `_AFileHoldingTheNameIsSaidSo`.
- `TestPutFile_ExistingFileIsOverwrittenByIDConditionally`, `_ExistingFolderIsAnError`: the fake fails the test on any listing after the 409.
- `TestRunPut_FolderUploadedAgainListsNoFolder`: second upload of 3 files in 2 folders: 0 listings, 5 reads, same IDs and `/Backup/...` paths. Before: 5 listings (red run with the folder half done: 3 listings and 2 reads; the folder half's red run listed once per folder).
- `TestPut_JSONPrintsEachPathWithOneLeadingSlash`: single put (default, `two.txt`, `/three.txt`, `Docs/`) and folder put (`Backup`, `/Copy/`); `TestRunPut_FolderReportsEveryFileAsTextAndJSON` now holds `/Copy/...`.
- `TestUploadFileToItem_CountsInThePerfRecordAndKeepsTheFilesMtime`.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1` cli, driveops, graph: pass; the same with `-race` under the lock: pass.
- Untagged `go test -count=1 ./e2e/`: pass. `RealRepoIsClean` (four checks): pass.
- Lint of cli, driveops, graph under the lock: `0 issues.` (W54-lint.log). It ran twice: the first run found two `assert.NoError` in a test fake (testifylint require-error), fixed, then the driveops tests again with `-race`, then the lint again.
- Spec citations checked by hand: every test name put into the three spec files exists; every requirement cited exists.

## Deviations
- `GetChildByName` places an answer for the root alias `root` by its `parentReference.path`: `mkdir` starts `EnsureFolder` from that alias, and no answer carries it as its parent ID, so every `mkdir` whose first segment exists would otherwise list the drive root.
- A name holding `/` is not refused: `url.PathEscape` sends it as `%2F` in one segment, and the parent check refuses a deeper answer.
- The text line of a single put still names the path as typed; only `--json` changed, as decided.

## Open for the live check (not documented in the quirks notes, or a guess)
- What a path read answers for a case-different name, an NFD name, and a router-sensitive name such as `v1.0` (an upload of it was misrouted). A wrong item is refused and listed for once; an error other than 404 fails the put.
- The parent ID goes out escaped (`!` as `%21`), as the long-path lookup already sends it.
- An item ID returned in another letter case (DriveId Casing Inconsistency) fails the exact parent check and costs one listing.
- `internal/graph/items.go` near `:252` says `:` cannot occur in a OneDrive name; if so, the colon fallback never runs.
- Tagged e2e tests that read a folder put's JSON paths or upload a tree twice: none. A single put onto an existing name (the changed path): `TestE2E_Sync_ConvergentEdit`, `TestE2E_Sync_DirectionalModes_PreserveEditEditConflict`.

## Also fixed
- `resolveConvergingPath` (session.go) has one return for the listing step.
- `EnsureFolder` says `"<name>" is a file on OneDrive` when a file holds the name.
- put's default remote path is computed once, before the folder branch.
- The four put tests that only round-trip `encoding/json` or `writeJSONDocument` are gone; the paths a folder put prints are held by tests that run it.
- `FindExistingChild` matches a listing with `matchChildByName`, the package's one rule for a listed name (exact spelling first).
- The cli fake drive matches names by `namekey.Equal`, not `strings.EqualFold`.
- Comments: `putJSONOutput` (also a folder entry), `uploadMtime`, `recordUploadPerf`.

## Seen and not fixed
1. internal/driveops/session.go:914-954: `resolveItemFromParentListing` and `listDeleteParentChildren` are named and worded for delete ("resolve delete target", "resolve delete parent") but also serve `WaitPathVisible` through `resolveConvergingPath`, so a put's visibility wait reports a "delete target"; fix: name and word them for a lookup by parent listing (e2e/sync_e2e_test.go:599 holds the text as a fixture).
2. internal/driveops/transfer_upload.go:230-276 and :326-367: `UploadFile` and `UploadFileToItem` repeat one preparation (stat, size limit, hash, the two source validations, mtime, open); fix: one preparation step both call.
3. internal/cli/get_test.go:113, :134, :180, :199: four tests round-trip `encoding/json` or `writeJSONDocument` of a literal; fix: drop them, as done for put (the folder get's paths are held by `TestRunGet_FolderReportsEveryFileAsTextAndJSON`).
4. internal/graph/items_fetch.go:78 (`GetItem`) and :240 (`ListChildren`): item routes interpolate the item ID unescaped while the path routes escape it; fix: one rule for an item ID in a route.
5. internal/cli/put.go:134 (`runPut`, the `Uploaded %s` line): the text names the path as typed while `--json` prints `putJSONPath`; fix: print one form in both, if wanted.

## Not tested here
No other package needs a run: no caller outside cli and driveops uses the changed functions.

## Review fixes
Commit 768bff73 (amended in place on 341d9820, not pushed). Files changed since the handed-back commit: `internal/driveops/session.go`, `internal/driveops/transfer_upload.go`, `internal/cli/get_test.go`, `e2e/sync_e2e_test.go`.

### Entry 1 (lookup by parent listing): done, checked and tightened
- No reader holds an old text or the old name: a tree-wide search (product, tests of every build tag, spec, docs, scripts) for the delete-target and delete-parent texts and the old function name finds only two unrelated spec phrases. The one fixture, `e2e/sync_e2e_test.go:599`, holds the new text; no test cites the new texts otherwise.
- The texts read well in every caller. A delete: rm wraps as `resolving "/a/b.txt": resolve "a/b.txt" from the listing of parent "a": graph: not found` (main shows the not-found summary; the chain is the verbose form); `DeleteResolvedPath` as `confirm delete path "a/b.txt" state: list parent "a": ...`. A visibility wait (put, mkdir, mv, rm's parent check, sync's parent check): a not-found never surfaces (the wait retries and ends in `PathNotVisibleError`), so only `list parent "a": ...` can, e.g. `confirming upload "a/b.txt" visibility: list parent "a": list children for "a": ...`.
- Comments made true of the code: `listParentChildren` lists through `ListChildren` (by path, or from the mount root by ID) and falls back only for a folder below the root; it now says so and names the resolver it calls. The empty-path text now has the same form as the others (`resolve "/" from the listing of its parent`).
- Also fixed in the same file: `ResolveDeleteTarget` and `resolveConvergingPath` were the same code (read by path, then the parent listing); `ResolveDeleteTarget` now calls `resolveConvergingPath`, which has a doc comment, and the parent fallback in `listParentChildren` calls it instead of the delete-named one. No behavior changes; the existing delete and visibility tests hold both.

### Entry 2 (one preparation for both uploads): done
- `prepareUploadSource` returns an `uploadSource` (file, size, hash, mtime): stat, source check, size limit, hash, expected-hash check, source check again, mtime, open, in that order with the same error texts; the caller closes the file. `UploadFile` and `UploadFileToItem` call it after their own parameter check and debug line, as before. The double-open comment moved with the hash step.
- Differences found between the two preparations: none in a text, an order or a check. What differed stays in the callers: the parameter checks, the debug line, the name sent (the given name vs the local file's base name), and `UploadFile` copying `opts.Progress` into a local first (dropped; it passes `opts.Progress` as the other does).
- Existing tests of both functions pass unchanged; no test added (the existing ones hold every step of the preparation through both callers).

### Entry 3 (`internal/cli/get_test.go`): done, all four removed
- `TestGetJSONOutput_Serialization` and `TestGetFolderJSONOutput_Serialization` ran only `encoding/json` on a literal: no product code. The field names and their order are held by `TestGetFolderJSONOutput_Golden`; the folder document from a real run by `TestRunGet_FolderReportsEveryFileAsTextAndJSON`.
- `TestGetJSONOutput` and `TestGetFolderJSONOutput` ran `writeJSONDocument` (and `withEmptyLists`) on a literal. A single get's document through `writeJSONDocument` is held by `TestCommandTree_EveryLeafHonorsJSON/onedrive-go get` (one indented document of the get type, no unknown field, no null list); `withEmptyLists` keeping a full list and filling an empty one by the golden test and `TestGetFolderJSONOutput_EmptyErrors`. None of the four was cited by a spec file.

### Checks
- `gofumpt -l` and `goimports -l` on the four files: no output.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1` driveops, cli, graph and untagged e2e: pass; driveops and cli with `-race` under the lock: pass.
- `RealRepoIsClean` (four tests): pass.
- Lint of `./internal/driveops/...` and `./internal/cli/...` under the lock: `0 issues.` (W54-lint2.log).

### Seen and not fixed
1. internal/cli/folder_transfer_test.go:119: `TestRunGet_FolderRefusesAnUnsafeRemoteName` runs in text mode only, so no test reads a folder get's `errors` list in --json after a failed file (the removed literal tests did not either); fix: run it once more with --json and hold its one `errors` entry and the exit error.
2. internal/cli/json_contract_test.go:424: no test reads the values of a single-file get's --json document (path, size, hash_verified); the contract case holds its shape only; fix: one get of a fake file with --json holding the three values.
3. internal/driveops/session.go:916: a failed listing reads `list parent "a": list children for "a": ...` or `list parent "a": list children of parent "a" by id: ...`, naming the folder twice; fix: drop the path from one of the two wraps.

## Review fixes (second review)

Commit `33a96721` (amended, one commit on main 12a48bb2, not pushed).

1. `FindExistingChild` lists the parent once on a 400 of the one-name read too (helper `readCannotSettleName`: ErrNotFound, ErrNameNotPathAddressable, ErrBadRequest); every other error is returned as before.
   Tests in `session_test.go` (EnsureFolder level, the `folderConflict` fake now answers a set `readStatus`/`readCode` instead of the old `readRefused` bool): `TestEnsureFolder_ConflictListsOnceWhenTheReadCannotFindTheName` row "a read answered 400" for a folder named `$value` (found, one read, one listing); `TestEnsureFolder_ConflictNotFound` row "a read answered 400" (same "not found in parent" error as a 404, one listing); `TestEnsureFolder_AReadThatFailsIsTheError` rows 403 and 500 (returned, no listing). The two 400 rows were seen failing (`HTTP 400 invalidRequest` returned); the 403/500 rows hold existing behaviour and passed before and after.
   Spec: drive-transfers.md and graph-client.md name the 400; graph-api-quirks.md "Name Routing In Path-Addressed URLs" gains a dated paragraph with the measured names and answers.
2. drive-transfers.md: the past-tense clause is gone; "...no listing, apart from such names"; "the 409 stands for a file; a folder reports that it was not found in its parent".
3. session.go: the outer wrap is `resolve %q from the listing of its parent: %w` with the cleaned child path; new test `TestMountSession_ResolveDeleteTarget_AFailedParentListingNamesThePathOnce` holds the whole text. Commit message corrected.
4. `itemSitsIn` compares the parent ID with `strings.EqualFold` (no existing item-ID comparison helper in the repository); row "a.txt" with an upper-case parent ID in `TestGetChildByName_AcceptsTheStoredSpellingOfTheSameName`, seen failing.
5. Root-alias branch also requires `item.ParentDriveID` to be zero or `.Equal(driveID)`; row "the root alias at the top of another drive" in `TestGetChildByName_RefusesAnAnswerThatIsNotTheAskedName`, seen failing; the accepting root-alias test's answer now carries a `driveId` in upper case, so the same drive still passes.
6. Wire-test rows `a?b` → `a%3Fb` and `a/b` → `a%2Fb`: pass, no product change.
7. cli.md: "a put to a drive path" prints `putJSONPath`, `CleanRemotePath` trims leading and trailing slashes only (`a//b` stays), and a put to a shared target prints its selector as `path`.
8. Nothing.
9. `nameLongerThanAPathRoute` comment: "longer than the 240 escaped characters a path-addressed read takes".
10. Commit message: the get/put test sentence reworded ("are gone; tests that run the command hold what they held"), plus the new holds of A and B.
A. `TestRunGet_FolderRefusesAnUnsafeRemoteName` runs the tree once more with --json: exit error `1 item failed to download`, `files` holds ok.txt only, `errors` is exactly `<target>: refused the remote name "a/b": it holds a path separator`. Passed first time: no fault in get.
B. New `TestGet_JSONPrintsTheFileItWrote` (json_contract_test.go): a single get with --json, decoded as a map, holds `path`, `size` and `hash_verified` exactly: `true` for a fake file given its quickXorHash (new `QuickXorHash` field of the CLI fake, emitted under `file.hashes` only when set), `false` without one.

How a 400 is told: `errors.Is(err, graph.ErrBadRequest)`. `classifyStatus` maps every 400 to that sentinel inside `*graph.GraphError`, other packages already branch on it (recycle_bin_flow.go), so no new type or status inspection was needed; the error text with the code word is unchanged.

Newer error text: no test or message of this commit quoted a Graph error text without the code word; the new test's expected text uses `graph: HTTP 403 accessDenied: denied`.

### Checks
- `gofumpt -l`, `goimports -l` on the 7 changed Go files: no output.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- Touched tests by name, one call per package (graph, driveops, cli): pass; the same with `-race` in one call over the three packages under the lock: pass.
- `go test -count=1 ./internal/graph/... ./internal/driveops/... ./internal/cli/...`: pass; untagged `./e2e/`: pass.
- `RealRepoIsClean` (four tests): pass.
- Lint of the three packages under the lock: `0 issues.` (W54-lint3.log).
- Test names in the changed spec text (`TestPut_JSONPrintsEachPathWithOneLeadingSlash`, `TestRunPut_FolderReportsEveryFileAsTextAndJSON`) exist.

### Seen and not fixed
- internal/driveops/session.go:1026: after a listing, `FindExistingChild` treats a match with an empty ID as not found, silently; a listed child without an ID is malformed Graph input; smallest fix: return an error naming the child instead of `false`.
- internal/cli/json_contract_fake_graph_test.go:88: the CLI fake's root ID is the alias `root`, so no CLI test passes through the parent-path rule for the alias (held in graph by `TestGetChildByName_PlacesAnAnswerForTheRootAliasByItsParentPath`); smallest fix: give the fake root its own ID and answer the alias for it.


## Third fixes (done by the orchestrator, 2026-11-15)

Commit `db394558` on main `12a48bb2`.

The second-review section's claims were checked at the places it names
(`FindExistingChild`, `readCannotSettleName`, `GetChildByName`, `itemSitsIn`,
the `folderConflict` fake and its three tests): they hold.

Its two "seen and not fixed" entries:

1. A listed item of the asked name without an item ID read as "not found"
   (a check new on this branch). Now an error: `the listing of the
   destination folder holds "<name>" without an item ID`. New test
   `TestEnsureFolder_AListedFolderWithoutAnIDIsAnError`, seen failing first
   (it got `folder "docs" reported as existing but not found in parent`).
   One sentence each in `drive-transfers.md` and the commit message.
2. The CLI fake's root item carries the alias `root` as its ID: a rider on
   plan row W73.

Live proof on the test account, an isolated build of this commit, a private
folder removed at the end (2026-11-15, about 14:05 PST):

- `mkdir` of a folder named `$value`, then of a child below it: exit 0, two
  one-name reads and one listing (the read of that name answers 400 and the
  listing settles it). The first `mkdir '$value'` alone exits 1 at the
  visibility confirm; main does the same; that is plan row W73.
- Names `v1.0` and `v1.0x`: exit 0, by the listing (the read answers 404
  though the item exists).
- 16 other router-like and marked names, case variants, NFD variants and a
  250-letter name: exit 0 with no listing.
- A name with a colon: OneDrive refuses it (400), exit 1.
- A folder put run twice: the second run takes 14 one-name reads and one
  listing.
- `mkdir` onto a file: `"v2.0" is a file on OneDrive`, exit 1.

Not known: why Graph answers 400 for `$value` and 404 for names that start
with `v1.0`; whether a Business drive does the same.

Checks after the fix: `go test ./internal/driveops/ ./internal/cli/` pass,
`gofumpt -l` clean. Lint and the live e2e tests are in the "Merged" section.


## Merged as #923

- Squash-merged on 2026-11-15 at about 15:17 PST; main is `af2a6cf5`
  ("Find an existing name after a 409 with one read, not a listing").
- Gate on the commit rebased onto `b8b72df8`: local pre-PR gate passed
  (lint `0 issues.`), PR CI and the review-thread gate passed.
- Live e2e tests, one at a time on this host: `TestE2E_Sync_ConvergentEdit`
  and `TestE2E_Sync_DirectionalModes_PreserveEditEditConflict` pass. The
  two shared-folder tests stopped at their fixture check: this host
  holds no shared-folder fixture. They run in the nightly full tier.
- Installed at 15:22 PST (`onedrive-go --version` names `af2a6cf5`).
- Not known: why OneDrive answers 400 for a child named `$value` and
  404 for names that start with `v1.0`, and whether a Business drive
  does the same.
