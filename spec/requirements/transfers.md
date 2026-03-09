# R-5 Transfers

Download and upload transfer infrastructure shared by file operations and sync.

## R-5.1 Parallel Transfers [verified]

- R-5.1.1: The system shall use configurable concurrent transfer workers (`transfer_workers`, default: `runtime.NumCPU()`, minimum 4). [verified]
- R-5.1.2: The system shall use a separate pool of check workers for local hash computation (`check_workers`). [verified]

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

## R-5.6 Upload Session Robustness [planned]

- R-5.6.1: When an upload fragment returns HTTP 416 (Range Not Satisfiable), the system shall query the session status endpoint for `nextExpectedRanges` before retrying. [planned]
- R-5.6.2: The system shall not use `If-Match` (eTag) on upload session creation, to prevent 412/416 cascading failures. [planned]
- R-5.6.3: When an upload session fails, the system shall cancel the session using `context.Background()` to prevent server-side quota leaks. [planned]
- R-5.6.4: On Business/SharePoint, the system shall include `fileSystemInfo` in the upload session creation request to set timestamps atomically and prevent double version creation. [planned]
- R-5.6.5: After simple upload (files <= 4 MiB), the system shall perform a PATCH to `UpdateFileSystemInfo` to preserve the local modification timestamp. [planned]
- R-5.6.6: Upload session fragment requests shall omit the Bearer token (pre-authenticated URLs embed their own credentials), preventing spurious 401 errors on long transfers. [planned]
- R-5.6.7: Zero-byte files shall always use simple PUT upload, since upload sessions require at least one non-empty chunk. [planned]
- R-5.6.8: The system shall not re-query file metadata immediately after upload, as properties (size, hash) may be temporarily in flux during server-side processing. [planned]
- R-5.6.9: When uploading, the system shall pass `@microsoft.graph.conflictBehavior=replace` to overwrite the existing item, consistent with the sync engine's local conflict resolution via rename-aside. [planned]

## R-5.7 Upload Size Limits [planned]

- R-5.7.1: The system shall reject uploads exceeding the 250 GB maximum file size before attempting the transfer. [planned]

## R-5.8 iOS Media Handling [planned]

- R-5.8.1: When downloading iOS `.heic` files whose API-reported size/hash does not match the actual downloaded bytes (known API bug), the system shall log a warning and accept the download rather than failing validation. [planned]
