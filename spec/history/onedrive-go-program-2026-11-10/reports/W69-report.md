# W69 report: a read by path returns its item

Commit `d945e278` on `fix/path-read-returns-its-item` (parent `4886b38c`, origin/main at the fetch before install). Not pushed. Installed: `onedrive-go version d945e278`.

## Re-check against main
The brief's line numbers held at `4886b38c`. Nothing it asked for was already fixed: `GetChildByName` already refused a colon (the brief knew), `GetItemByPath` and `ListChildrenByPathPages` did not; the paged listing (#929) was there to stop at the page that holds a name. No new lint rule bit (0 issues).

## DoR, in short
- (i) `GetItemByPath`'s one product caller is `MountSession.ResolveItem` (session.go:533) for sessions without a mount root; through it stat, get, ls fallback, mkdir, cp, mv (dest, parent, source), put target, rm (`ResolveDeleteTarget`), delete convergence and every `WaitPathVisible` (put, mv, rm, mkdir, sync executor :165/:233). `ErrNotFound` means "absent" (mv/put go on to create, rm/WaitPathVisible fall back to the parent listing); other errors are the command's error. Worst case of a wrong item: `rm` (or `mv --force`) deletes or sets aside an unrelated item of the same name in another folder. No caller depends on a wrong-parent answer.
- (ii) The fallback runs only when an answer has no `parentReference.path` with a `/root:` marker. Measured on the personal test account with main and the branch: by-path reads at depth 1, 2 and 3 all carried the path (0 of 3 took the fallback), as L-32 recorded. Business/SharePoint unprobed. The CLI JSON-contract fake omits the path on every answer (test only).
- (iii) `GetChildByName`'s one caller is `FindExistingChild` (EnsureFolder, cli putFile after a 409); `ErrNameNotPathAddressable` lists the folder.
- (iv) `readCreatedFileByName`: whole listing on a 2xx create without an item (rare; every new-file create can reach it). `lookupUploadTarget` by name: an upload completed without its item, an uncertain final fragment, a gone/complete persisted session (rare error paths, sync and put). `resolveItemFromMountRoot`: every path read below a mounted folder, all pages of every folder on the path: each `WaitPathVisible` of a shortcut/shared-folder sync. Others: `resolveItemFromParentListing` (session.go:922, fallback after a by-path miss), `readCreatedFolderAfterEmptySuccessBody` (graph, items_mutation.go:76); replace_destination.go:107, tree_download.go:96 and ls list for real reasons.
- (v) Escaped before: content download, the child path route's parent. Raw: GetItem, children, by-path long listing, create folder/file, move, fileSystemInfo, delete, permanent delete, permissions, restore, copy, item upload session, simple upload, folder delta.

## Per brief item
- Colon refused before any request in both by-path reads, one function (`refuseUnaddressableNames`) also used by `GetChildByName`: done.
- Fallback compares the parent via `itemSitsIn`, parent read once through `GetItemByPath`: done, with one deviation: a top-level answer reads the drive root once. `itemSitsIn` places an answer under the root alias only by a parent path, which this answer lacks, so "the drive root needs no read" cannot hold.
- `readCreatedFileByName`, `lookupUploadTarget`, `resolveItemFromMountRoot` read one name, paged listing stopped at the page holding it as fallback: done; the two driveops ones share `FindExistingChild` (no new export), graph has its own small pair.
- Item IDs escaped on every item route by one helper (`itemRoute`): done.
- Spec: four rules in graph-client.md, citing the new tests: done; also drive-transfers.md, quirks (colon measurement, case-insensitivity text), R-6.7.8 and R-6.7.22.
- CLI wording: unchanged; every command prints the path.
- Beyond: long by-path listing now placed by `GetItemByPath` before listing; parent-listing fallback stops at its page; `FindExistingChild`'s read answering an item without an ID is an error; its messages drop "destination"; the sync tests' uploaders (engineMockClient, creatingTestUploader) gain the two methods and an `ItemReader` pin (without them a sync test of a failed outcome lookup failed at run time: "uploader cannot read items back").
- Product change: 224 lines added, 97 removed.

## Tests (each seen failing on unchanged code)
- TestPathReads_RefuseAColonInAnyNameBeforeAnyRequest: requests were sent (418 from the fake).
- TestGetItemByPath_AnswerWithoutParentPathIsPlacedByItsParentsID: the other-folder answer was accepted; no parent read.
- TestItemRoutes_EscapeTheItemIDAsOnePathSegment: 15 of 16 routes sent "!" raw.
- TestCreateFile_ReadsTheCreatedFileBackByItsName: 0 reads, 4 pages instead of 1 read / 2 pages.
- TestLookupUploadTarget_ReadsTheNameAndListsOnlyWhenTheReadCannotSettleIt: 0 reads, 3 pages.
- TestMountSession_ResolveItem_MountRootReadsEachNameOnce: 0 reads, 4 pages.
- TestListChildrenByPath_LongPathListsOnlyTheFolderAtThePath: listed the wrong folder (seen with the old line put back).
- TestEnsureFolder_AFolderWithoutAnIDIsAnError (was ...AListedFolder...): read case added.
All new tests are in graph and driveops; the stress lane's placement rule names only internal/sync tests, so none needs a declaration (TestStressRepeatedStepsDeclareEveryTestTheirRuleNames passes).

## Checks
- `go build ./...`, `go vet ./...`, tagged vet (e2e, e2e_full, integration, stress): pass.
- graph, driveops, cli plain: pass; same with -race behind the lock: pass.
- 76 sync tests of the ten files that reach transfer lookups (pattern run): pass. Whole sync tree not run.
- RealRepoIsClean: 4 PASS; routing-table and stress-declaration rules: pass; cited test names checked by hand (10 of 10 exist).
- `devtool verify lint` behind the lock: 0 issues, actionlint pass.

## Live proof (test account, private folder, removed after)
- Main: stat and put of a name with a colon each reached OneDrive and failed with its 400 "Resource not found for the segment"; mkdir of such a name failed with OneDrive's 400 "invalid character"; nothing was created.
- Branch: stat and put failed before any request with the client's own "name cannot be carried in a path-addressed request", the path in the message; mkdir unchanged (OneDrive's 400); nothing created.
- Both: nested by-path reads carried their parent paths, so the branch added no read.

## Seen and not fixed
- internal/graph/items_mutation.go:76 `readCreatedFolderAfterEmptySuccessBody` lists the whole parent on each attempt; fix: one read of the name per attempt, keeping `findCreatedFolder`'s ambiguity rule for the listing fallback.
- internal/sync/executor.go:369 `adoptExistingRemoteFolder` lists the whole parent after a 409; fix: a FindExistingChild-shaped read.
- internal/sync/executor_transfer.go:756 a file create's 409 lists the whole folder; same fix.
- internal/sync/domain_io_seams.go:30 `itemClient` declares `GetItemByPath`, which no sync product code calls; fix: drop it and its mock methods.
- Drive IDs go into every route unescaped (itemRoute, items_fetch.go root route, delta.go:32, drives_identity.go:220, socketio.go:27); a Business drive ID holds "!"; fix: escape it in the same helper.
- internal/graph/drives_sites.go:84 a site ID (commas) goes into its route unescaped; fix: url.PathEscape.
- internal/cli/mv.go:90 the `item.ParentID == ""` branch is unreachable now; TestResolveDest_ForceEmptyParentID passes through the not-a-folder branch; fix: drop the branch, rewrite the test.
- internal/cli/json_contract_fake_graph_test.go:203 the fake answers without `parentReference.path`, so every by-path read there takes the no-path branch and its extra reads; fix: give the fake the parent's path.
- driveops `ItemReader` (and the other optional capabilities) are type-asserted, so a mock that loses a method fails only at run time; fix: pin each test uploader with `var _` as done here.
- resolveItemFromMountRoot: an empty segment ("a//b") is now ErrInvalidPath from the one-name read instead of not found; fix, if wanted: clean the path at the session boundary.
- Business/SharePoint: whether by-path answers carry the parent path is unprobed.

Packages for the reviewer: internal/sync (whole tree; its test uploaders changed), internal/multisync.


## Merged as #949

- The five commits were squashed into one and rebased onto main `99c582d4` (#948): two conflicts, both kept (a test next to #948's new put test in `internal/cli/json_contract_test.go`; the `putPrintedPath` sentence of `spec/design/cli.md` with this change's cleaning of empty names). The commit message's last paragraph names the two tests renamed against main.
- After the rebase: `go vet` plain and with all tags, `go test` of cli, driveops, graph and localpath, RealRepoIsClean (4 PASS), start stage (nightly green, nothing claimed), pre-pr (lint 0 issues, quick tier), pre-merge, then squash-merge `2686d99d` at 05:07 PST 2026-11-18; installed from main at 05:10 (`onedrive-go --version` prints 2686d99d); post-merge passed at 05:33 (worktree and branch removed).
- Live probe on the test account with a build of the branch: `stat` and `put` of a name with a colon exit 1 before any request ("name cannot be carried in a path-addressed request"); `mkdir` of such a name gets OneDrive's 400 "invalid character"; nothing stray was created; the probe folder and tree were removed.
- Leftover planned as W108: the CLI has no sentence of its own for the disagreement error (it prints the raw chain with item IDs), nor for the colon refusal (the path three times).
