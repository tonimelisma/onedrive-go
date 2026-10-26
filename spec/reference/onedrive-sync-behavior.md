# OneDrive Sync Behavior

Observable OneDrive behaviors that affect sync client design. For standard API documentation (delta queries, upload sessions, permissions), see [Microsoft's official docs](https://learn.microsoft.com/en-us/graph/api/resources/onedrive). For API bugs and inconsistencies, see [graph-api-quirks.md](graph-api-quirks.md).

## Eventual Consistency

OneDrive is an eventually-consistent system. Changes made via the API may not immediately appear in delta responses. A just-created item is not guaranteed to appear in the next delta query. The delta token represents a checkpoint, not a wall-clock time.

After upload, OneDrive may take time to process a file (thumbnails, metadata extraction, content indexing). During this window, the file's reported properties may be incomplete or in flux. Do not re-query an item immediately after upload and expect stable metadata.

## Delta Checkpoint Gap

The `@odata.deltaLink` (checkpoint token) only appears in the **last page** of a delta response. If file downloads fail within the final batch and the deltaLink is saved, the only way to retry those failed items is a full resync. There is no mechanism to partially commit a delta checkpoint. The sync engine must track individual item failures independently of the delta token.

## Folder Rename Propagation

When a folder is renamed, **only that folder** appears in the delta response. None of its children or grandchildren get delta entries. The sync engine must infer path changes for all descendants by walking parent-child relationships. This means the engine cannot rely on delta alone to discover all affected paths — it must maintain its own path-reconstruction capability from parent IDs.

## Case-Insensitive Namespace

OneDrive uses a case-insensitive, case-preserving namespace (consistent with Windows/NTFS). Two files named `README.md` and `Readme.md` cannot coexist in the same folder. See also [graph-api-quirks.md](graph-api-quirks.md#onedrive-is-case-insensitive) for an API bug where path-based queries return false matches. On case-sensitive filesystems (Linux), the sync client must enforce this constraint:

- Before uploading, check for case-insensitive collisions at the target path
- When two local items would collide under case-insensitive rules, flag a conflict
- Path-based API queries perform case-insensitive matching

Folding goes beyond ASCII. A live probe on a OneDrive Personal account on
2026-10-25 created `straße` then `STRASSE`, `i̇stanbul` then `İstanbul`, and
`σ` then `Σ` in one folder with `conflictBehavior=fail`; every second create
returned **409**. A collision key must therefore use full Unicode case folding
(where `ß` folds to `ss`), applied to the NFC form of the name, not
`strings.ToLower` or `strings.EqualFold`, which treat `straße` and `STRASSE` as
different. At the time of writing the local collision detection in
`internal/sync/observer_local_collisions.go` and `internal/sync/scanner.go`
still uses `strings.ToLower`, so these pairs are not yet caught locally; the
fix is tracked as naming hardening work.

## Naming Restrictions

OneDrive enforces naming rules that differ from POSIX. The following are rejected:

- **Reserved names** (case-insensitive): `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9`
- **Reserved patterns**: names starting with `~$`, names containing `_vti_`, `forms` at root level (SharePoint)
- **Invalid characters**: `"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `|`
- **Trailing dot** (`.`): rejected in practice but **not documented** in Microsoft's official naming restrictions. Discovered via abraunegg/onedrive issue #2678.
- **Leading/trailing whitespace**: rejected

## File Size Constraints

- Maximum file size for upload, download, and sync: 250 GB
- Microsoft Graph simple upload (`PUT /content`) supports files up to 250 MB
- This client intentionally uses simple upload only up to 4 MiB and routes larger files through upload sessions
- Upload session (resumable): supported up to 250 GB
- Zero-byte files must use simple upload (upload sessions require at least one non-empty chunk)

## Conflict Behavior

The `@microsoft.graph.conflictBehavior` parameter controls name collisions on upload:
- `fail` — return an error
- `replace` — overwrite the existing item
- `rename` — auto-rename (e.g., `file (1).txt`)

Default for PUT is `replace`. This is a URL parameter, not a request body field.

## Upload Session Authentication

Upload session URLs include embedded authentication. Fragment uploads do **not** require a Bearer token. Sending a Bearer token with fragment uploads can cause spurious 401 errors when the token expires during a long upload — the pre-authenticated URL would have succeeded without it.

This is the only documented spurious-401 class currently captured in this
repository. It is a pre-authenticated upload-session quirk, not evidence that
ordinary authenticated Graph requests should be retried beyond the graph
client's one refresh-and-retry lifecycle boundary.

## Permanent Deletion Availability

Permanent deletion is `POST /items/{id}/permanentDelete`; a plain `DELETE /items/{id}` sends the item to the recycle bin. This section previously said permanent deletion is only supported on OneDrive for Business and SharePoint and that Personal always uses the recycle bin. A live probe on a OneDrive Personal account on 2026-10-25 got **204** from `permanentDelete` and the item was gone, so it works on Personal too. National clouds may not support permanent deletion regardless of account type.

## NFC/NFD Unicode Normalization

OneDrive **does** treat NFC and NFD forms of a name as the same name. This
section previously said the opposite. A live probe on a OneDrive Personal
account on 2026-10-25 uploaded `café.txt` in NFC form, then uploaded the NFD
form (`e` + U+0301) to the same parent: the second upload returned 200 with the
**first item's ID** and replaced its content.

Consequences for a sync client:

- Two local files whose names differ only in Unicode normalization (possible
  on Linux, where names are opaque bytes) are a true collision on OneDrive,
  just like a case-only difference. Uploading both would make the second
  overwrite the first.
- Collision and grouping keys must normalize (NFC, then full case folding; see
  "Case-Insensitive Namespace" above).
- A file created on macOS (APFS/HFS+ commonly hand back NFD names) maps onto
  the existing NFC remote item rather than creating a second one.

Whether OneDrive stores the uploaded byte form or rewrites names to NFC has not
been probed yet. The local scanner already NFC-normalizes the paths it records
(`internal/sync/scanner.go`).

## Read-Only Shared Items

Some shared files have permissions that allow viewing but not downloading. Attempting to download returns HTTP 403. The sync engine must handle this gracefully — it is not a transient error but a permanent permission constraint. Similarly, uploading modifications to read-only shared files fails with 403.
