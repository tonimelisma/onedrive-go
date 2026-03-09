# Drive Transfers

GOVERNS: internal/driveops/cleanup.go, internal/driveops/doc.go, internal/driveops/hash.go, internal/driveops/interfaces.go, internal/driveops/session.go, internal/driveops/session_store.go, internal/driveops/stale_partials.go, internal/driveops/transfer_manager.go, pkg/quickxorhash/quickxorhash.go, get.go, put.go

Implements: R-5.1 [implemented], R-5.2 [implemented], R-5.3 [implemented], R-5.5 [implemented], R-1.2 [implemented], R-1.3 [implemented]

## TransferManager

Unified download/upload manager shared by both CLI file operations and the sync engine. Handles resume, hash verification, and cleanup.

### Download

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

QuickXorHash computation for local files (`hash.go`). The `pkg/quickxorhash/` package implements the algorithm (vendored from rclone, BSD-0 license).

## Cleanup

`cleanup.go` removes stale `.partial` files and expired upload sessions on startup. `stale_partials.go` detects orphaned partial files from interrupted downloads.

### Rationale: Per-Side Hashes

SharePoint enrichment silently modifies files after upload (hash/size change). Per-side hash baselines (`local_hash`, `remote_hash`) handle this natively: the planner compares new hashes against the correct side's baseline. No special code paths, no false conflicts from enrichment.
