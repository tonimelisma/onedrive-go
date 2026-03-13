# Drive Transfers

GOVERNS: internal/driveops/cleanup.go, internal/driveops/doc.go, internal/driveops/hash.go, internal/driveops/interfaces.go, internal/driveops/session.go, internal/driveops/session_store.go, internal/driveops/stale_partials.go, internal/driveops/transfer_manager.go, pkg/quickxorhash/quickxorhash.go, get.go, put.go

Implements: R-5.1 [verified], R-5.2 [verified], R-5.3 [implemented], R-5.5 [verified], R-1.2 [verified], R-1.3 [verified], R-5.6 [implemented], R-5.7 [verified], R-5.8 [planned], R-6.8.3 [verified]

## TransferManager

Unified download/upload manager shared by both CLI file operations and the sync engine. Handles resume, hash verification, and cleanup.

### Download

Implements: R-6.2.3 [verified]

1. Create `.partial` file in target directory
2. If `.partial` exists with content, resume via HTTP Range request
3. Stream response body, computing QuickXorHash incrementally
4. Verify hash and size against API metadata
5. Atomic rename `.partial` → final path

### Upload

1. Files ≤ 4 MiB: simple PUT (single request)
2. Files > 4 MiB: create resumable upload session, upload in chunks (320 KiB aligned)
3. Verify server-reported hash matches local file after upload

## SessionStore

File-based upload session persistence. Each session is a JSON file in the data directory containing the upload URL, byte offset, file hash, and expiry. Atomic writes (write-to-temp + rename). On resume, local file hash is recomputed — if it differs from the stored hash, the session is discarded.

## Transfer Interfaces

Two required interfaces (`Downloader`, `Uploader`) and two optional interfaces (`RangeDownloader`, `SessionUploader`) type-asserted at runtime. This allows the graph client to support resume without requiring all implementations to.

## Hash Utilities

QuickXorHash computation for local files (`hash.go`). The `pkg/quickxorhash/` package implements the algorithm (vendored from rclone, BSD-0 license). When a remote file lacks a hash (common on Business/SharePoint), a fallback chain is attempted: QuickXorHash → SHA256 → SHA1. `HashVerified` is set to false when the remote hash is empty.

## Cleanup

`cleanup.go` removes stale `.partial` files and expired upload sessions on startup. `stale_partials.go` detects orphaned partial files from interrupted downloads.

## Design Constraints

- `Upload()` accepts `io.ReaderAt` (not `io.Reader`): enables retry-safe uploads without re-opening the file. `io.NewSectionReader` creates independent readers for each chunk.
- Guard `.partial` file cleanup with `ctx.Err() == nil`: a 3.9 GB partial of a 4 GB download should survive Ctrl-C for resume. Only intentional deletions (hash mismatch) should remove partials.
- Per-transfer timeout or connection-level deadline for `transferHTTPClient()`: `Timeout: 0` relies on context; a stalled connection without context cancellation hangs indefinitely. [planned]
- Transfer manager resume edge case tests: corrupt partial file, changed remote content, oversized partial. [planned]

### Rationale: Per-Side Hashes

SharePoint enrichment silently modifies files after upload (hash/size change). Per-side hash baselines (`local_hash`, `remote_hash`) handle this natively: the planner compares new hashes against the correct side's baseline. No special code paths, no false conflicts from enrichment.
