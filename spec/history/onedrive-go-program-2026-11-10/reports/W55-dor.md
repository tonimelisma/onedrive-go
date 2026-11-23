# W55 Definition of Ready: folder listings are read a page at a time

## Callers of the listing (graph `ListChildren`, `ListChildrenByPath`, driveops `MountSession.ListChildren`)
- driveops `DownloadTree` root and `subfolder` (tree_download.go): walks only. Moves to the paged form.
- cli `hintAboutLeftoverAside` (replace_destination.go, via `MountSession.ListChildren`): walks only, a note per aside name. Moves to the paged form (`MountSession.ListChildrenPages`, new).
- cli `runLs` (ls.go, via `MountSession.ListChildren`): whole list: the table sorts folders first then by name, `--json` is one document, and the file fallback decides on the listing's error before any output. Slice.
- driveops `resolveItemFromMountRoot`, `listParentChildren` (both branches, for `resolveItemFromParentListing`): lookup by name through `matchChildByName`, where an exact match anywhere beats an earlier folded one. Slice.
- driveops `FindExistingChild` (session.go): the lookup after a 409, out of scope. Slice.
- driveops `lookupUploadTarget` (transfer_verify.go) and graph `readCreatedFileByName` (item_create.go): lookup by name, first file whose name is equal. Slice (they could stop at the first match; named in the report).
- graph `readCreatedFolderAfterEmptySuccessBody` -> `findCreatedFolder` (items_mutation.go): lookup by name; a folded match counts only if no second one is in the whole listing. Slice.
- graph `listChildrenRecursiveDepth` (`ListChildrenRecursive`): its result is every descendant in one slice, which sync's shared-folder enumeration (engine_mount_root.go) compares with what it knows. Slice.
- internal/sync, out of scope, listed and left on the slice form: `adoptExistingRemoteFolder` (executor.go), the refused-create resolution (executor_transfer.go), the recursive delete walk (executor_delete.go), the `ListChildren`/`ListChildrenRecursive` seams (domain_io_seams.go), engine_mount_root.go. The slice form keeps its signature, so none changes.
- Test callers: graph items_test.go (`TestListChildren_*`, `TestListChildrenByPath_*`, `TestListChildrenRecursive_*`), integration_test.go (tag integration); driveops session_test.go (`TestMountSession_ListChildren*`), the tree tests through `fakeTree`; cli tests through the HTTP fake `fakeGraphDrive`.

## Retry today when page k fails
- Each page is one GET of its own URL (`listChildrenPage`). The CLI's metadata client wraps a `RetryTransport`, which resends that request (GET is replay safe) on a 5xx, 429 or network failure up to its policy; the first page of the drive root's listing also has the documented 404 quirk retry (`TestListChildren_RootTransient404Retry`). Pages 1..k-1 are never asked again. Sync's transport retries nothing (R-6.8.7).
- When page k still fails, or fails in a way nothing retries (a decode error, a 4xx, a nextLink off the base URL), `fetchAllChildren` returns nil and the error: the caller sees the error only and the k-1 pages read are dropped.
- After: the same requests and the same retries; the caller has been handed pages 1..k-1 and then sees page k's error. The slice form still returns nil and the error.

## What DownloadTree holds per folder
- Today: per folder on the walk's stack, the folder's whole children slice (every page, plus the append growth of `fetchAllChildren`), alive from its listing until the walk of that folder (its subfolders included) ends.
- After: per folder on the stack, the page being walked (at most 200 items) and the client's link to the next page; a page is dropped before the next is asked for. The queue is unchanged (2xTreeWorkers files).

## Test fakes
- `fakeTree` (driveops tree tests) is the one fake of the paged form: it hands a folder on in pages of a set size and can fail a given page. `tmMockUploader` (driveops) and the sync mocks fake the slice form for interfaces that keep it. The HTTP fakes (graph httptest servers, driveops `newTestSession`, cli `fakeGraphDrive`) serve both forms, since the paging is in the client.

## Decisions within the brief
- Shape: a callback per page, `type ChildrenPageFunc func(page []Item) (keepListing bool)`, `ListChildrenPages(ctx, driveID, parentID, visit) error` and `ListChildrenByPathPages`. Delta hands its pages out as a cursor because sync keeps the token; its page loop, `deltaAllPages`, is one loop in the client calling a function per page. The listing keeps that loop, its page cap and the next link in the client; the slice forms are that loop appending.
- Consequence: a later page that fails is the folder's failure event after its earlier pages were walked (as an upload's partial read already is); the root's first page failing is still the `TreeRootError` before anything is reported. A late root page is now a `TreeFolderFailed` for the root, not the command's error.
- Also fixed in items_fetch.go: the page cap failed a listing of exactly `defaultMaxListPages` pages (it counted a page past the last). Found during the work: `SearchDriveItems` (drives_shared.go) had the same miscount, and testing the cap cost 10,000 requests a listing, so the cap became the client field `maxListPages` (like `maxDeltaPages`), read by every non-delta loop (drives_identity.go, drives_shared.go, drives_sites.go, items_permissions.go), with one table test at a cap of 3 replacing the 10,000-request drives test.
- `ListRecycleBinItems` (items_permissions.go) shared the old loop: it is now the slice form over the same loop.

## Checklist
1. PASS Task intent: a folder listing is handed on a page at a time; a recursive get and the aside hint walk pages, so a folder of any size is never one slice there.
2. PASS Non-goals: internal/sync and delta, retry rules, the 409 lookup, put, ls output, the lookups by name.
3. PASS Governing docs: spec/design/graph-client.md (Paged Listings, Item Operations), spec/design/drive-transfers.md (Folder downloads, Recursive Transfer Concurrency), R-1.1.3, R-1.2.1, R-6.7.32. No requirement states a memory bound for a listing.
4. PASS Authority owner: graph.Client owns the paging, its cap and retries; DownloadTree's walk owns the tree's events; MountSession owns path routing for both forms.
5. PASS Code scope: internal/graph/items_fetch.go (plus the cap field: client.go, client_construction.go and the four loops above), internal/driveops/tree_download.go, tree.go (one comment) and session.go, internal/cli/replace_destination.go, tests, the two design docs. Not touched: internal/sync, put, requirement files.
6. PASS Regression plan: graph tests on a three-page server (handed on before the next page; a failed page retried as itself; cancellation and early stop ask for nothing more; the slice form unchanged); tree tests (a page walked before the next is asked; a late page failure fails only its folder); the exact page cap.
7. PASS Verification plan: build, vet plain and tagged, graph/driveops/cli tests plain then -race behind the lock, untagged e2e, RealRepoIsClean, lint behind the lock.
8. PASS Data safety: no sync state; a download still creates a folder only after its first page is read and writes nothing for a refused name.
9. PASS Secret safety: no token, .env or .testdata read; tests use synthetic names.
10. PASS Live/destructive: nothing against OneDrive or GitHub, no push, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION Question: does a children nextLink stay valid while the walk spends a long time in the subtree of an earlier page? Current assumption: unknown; a page that fails then is that folder's failure, retried by the transport first. Validation step: a live get of a folder of more than 200 children with large subfolders (not run here). Owner if wrong: tree_download.go.
