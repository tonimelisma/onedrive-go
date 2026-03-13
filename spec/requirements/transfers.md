# R-5 Transfers

Download and upload transfer infrastructure shared by file operations and sync.

## R-5.1 Parallel Transfers [verified]

- R-5.1.1: The system shall use a flat pool of `transfer_workers` goroutines (default 8, range 4–64) for concurrent file operations (downloads, uploads, deletes, moves, conflict resolution). [verified]
- R-5.1.2: The system shall use a separate pool of `check_workers` goroutines (default 4, range 1–16) for parallel local QuickXorHash computation during filesystem scanning. [verified]

## R-5.2 Resumable Transfers [verified]

- R-5.2.1: When a file exceeds 4 MiB, the system shall use a resumable upload session. [verified]
- R-5.2.2: When a download is interrupted, the system shall resume via `.partial` files and HTTP Range requests on retry. [verified]
- R-5.2.3: Upload session state shall be persisted to disk and survive process restart. [verified]
- R-5.2.4: Interrupted transfers shall resume automatically on next sync. [verified]

## R-5.3 Upload Chunking [implemented]

- R-5.3.1: Files <= 4 MiB shall use simple PUT upload (single request). [verified]
- R-5.3.2: Files > 4 MiB shall use resumable upload sessions with configurable chunk size. [planned]
- R-5.3.3: Chunk size shall be a multiple of 320 KiB (API requirement). [verified]

## R-5.4 Bandwidth Limiting [future]

- R-5.4.1: The system shall support a global bandwidth limit (`bandwidth_limit`). [future]
- R-5.4.2: The system shall support time-of-day bandwidth scheduling (`bandwidth_schedule`). [future]

## R-5.5 Transfer Validation [verified]

- R-5.5.1: After download, the system shall verify hash and size against API metadata. [verified]
- R-5.5.2: After upload, the system shall verify the server-reported hash matches the local file. [verified]
- R-5.5.3: Validation shall be individually disableable via `disable_download_validation` and `disable_upload_validation`. [verified]

## R-5.6 Upload Session Robustness [implemented]

- R-5.6.1: When an upload fragment returns HTTP 416 (Range Not Satisfiable), the system shall query the session status endpoint for `nextExpectedRanges` before retrying. A network timeout during fragment upload can cause the server to receive partial data without the client ever getting an ACK. Naively retrying the same byte range fails with 416 because the server already has those bytes. The only recovery is to ask the session status endpoint where it actually stopped and resume from there. [planned]
- R-5.6.2: The system shall not use `If-Match` (eTag) on upload session creation. The eTag can change during session creation itself (server-side race), causing an immediate 412 Precondition Failed. Subsequent fragment uploads then cascade into 416 errors because the session is in a broken state. Omitting `If-Match` avoids the cascade entirely — conflict detection is handled at a higher level by the sync engine. [verified]
- R-5.6.3: When an upload session fails, the system shall cancel the session using `context.Background()` to prevent server-side quota leaks. A successful `CreateUploadSession` allocates server-side resources that persist for ~15 minutes and count against storage quotas. If the upload fails, the original context is likely already canceled (that's what caused the failure), so the cancel request must use a fresh `context.Background()` to ensure the cleanup HTTP call actually fires. [verified]
- R-5.6.4: On Business/SharePoint, the system shall include `fileSystemInfo` in the upload session creation request to set timestamps atomically and prevent double version creation. Without this, uploading a file and then PATCHing its timestamps in a separate request creates two version history entries per upload, doubling storage consumption. Including `fileSystemInfo` in the session creation sets timestamps atomically with the content in a single version. This is a documented API bug (onedrive-api-docs#877) with no server-side fix. [verified]
- R-5.6.5: After simple upload (files <= 4 MiB), the system shall perform a PATCH to `UpdateFileSystemInfo` to preserve the local modification timestamp. Simple upload (PUT to `/content`) sends raw binary with no way to include metadata in the same request, so the server stamps the file with the server receipt time. The post-upload PATCH restores the correct local mtime. This only affects simple uploads — resumable sessions use R-5.6.4 for atomic timestamps. [verified]
- R-5.6.6: Upload session fragment requests shall omit the Bearer token. Upload session URLs are pre-authenticated — the credential is embedded in the URL itself. Sending a Bearer token alongside is redundant and dangerous: if the OAuth token expires mid-upload (common for large files), the server rejects the expired Bearer token even though the pre-authenticated URL is still valid, causing spurious 401 errors hours into a multi-gigabyte transfer. [verified]
- R-5.6.7: Zero-byte files shall always use simple PUT upload. The upload session API requires at least one non-empty fragment — a zero-byte file cannot satisfy this constraint. Attempting to create a session for a zero-byte file either errors immediately or creates a session that can never complete. Simple PUT handles zero-byte files correctly as a degenerate case of a single empty request body. [verified]
- R-5.6.8: The system shall not re-query file metadata immediately after upload. After upload completion, server-side processing (virus scanning, indexing, SharePoint enrichment) can temporarily show incorrect size and hash values. The upload completion response itself contains the correct metadata. Querying the item endpoint immediately can return stale pre-processing values, causing false hash mismatches and unnecessary re-uploads on the next sync cycle. [verified]
- R-5.6.9: When uploading, the system shall pass `@microsoft.graph.conflictBehavior=replace` to overwrite the existing remote item. The sync engine resolves conflicts client-side via rename-aside (local conflicting version is renamed to `<name>.conflict-<timestamp>.<ext>`). By the time an upload reaches the API, the decision to overwrite has already been made. Without `replace`, the API defaults to `fail` or `rename`, which either errors out or creates a server-side duplicate that the sync engine doesn't expect. [verified]

## R-5.7 Upload Size Limits [verified]

- R-5.7.1: The system shall reject uploads exceeding the 250 GB maximum file size before attempting the transfer. [verified]

## R-5.8 iOS Media Handling [planned]

- R-5.8.1: When downloading iOS `.heic` files whose API-reported size/hash does not match the actual downloaded bytes (known API bug), the system shall log a warning and accept the download rather than failing validation. [planned]
