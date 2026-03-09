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

OneDrive uses a case-insensitive, case-preserving namespace (consistent with Windows/NTFS). Two files named `README.md` and `Readme.md` cannot coexist in the same folder. On case-sensitive filesystems (Linux), the sync client must enforce this constraint:

- Before uploading, check for case-insensitive collisions at the target path
- When two local items would collide under case-insensitive rules, flag a conflict
- Path-based API queries perform case-insensitive matching

## Naming Restrictions

OneDrive enforces naming rules that differ from POSIX. The following are rejected:

- **Reserved names** (case-insensitive): `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9`
- **Reserved patterns**: names starting with `~$`, names containing `_vti_`, `forms` at root level (SharePoint)
- **Invalid characters**: `"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `|`
- **Trailing dot** (`.`): rejected in practice but **not documented** in Microsoft's official naming restrictions. Discovered via abraunegg/onedrive issue #2678.
- **Leading/trailing whitespace**: rejected

## File Size Constraints

- Maximum file size: 250 GB (all account types)
- Simple upload (PUT): files up to 4 MiB
- Upload session (resumable): files over 4 MiB, up to 250 GB
- Zero-byte files must use simple upload (upload sessions require at least one non-empty chunk)

## Conflict Behavior

The `@microsoft.graph.conflictBehavior` parameter controls name collisions on upload:
- `fail` — return an error
- `replace` — overwrite the existing item
- `rename` — auto-rename (e.g., `file (1).txt`)

Default for PUT is `replace`. This is a URL parameter, not a request body field.

## Upload Session Authentication

Upload session URLs include embedded authentication. Fragment uploads do **not** require a Bearer token. Sending a Bearer token with fragment uploads can cause spurious 401 errors when the token expires during a long upload — the pre-authenticated URL would have succeeded without it.

## Permanent Deletion Availability

`DELETE /items/{id}` with permanent deletion is only supported on OneDrive for Business and SharePoint. OneDrive Personal always sends items to the recycle bin. National clouds may not support permanent deletion regardless of account type.

## NFC/NFD Unicode Normalization

OneDrive does not normalize Unicode forms. A filename created on macOS (which uses NFD/decomposed form on APFS/HFS+) may appear as a different byte sequence than the same visually-identical name created on Linux (which typically uses NFC/composed form). A sync client on macOS must normalize filenames before comparison.

## Read-Only Shared Items

Some shared files have permissions that allow viewing but not downloading. Attempting to download returns HTTP 403. The sync engine must handle this gracefully — it is not a transient error but a permanent permission constraint. Similarly, uploading modifications to read-only shared files fails with 403.
