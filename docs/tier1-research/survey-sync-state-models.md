# Survey: Sync State Database Models

A comprehensive survey of how file synchronization tools model their sync state, covering storage engines, per-file metadata, identity models, change detection, conflict detection, move/rename detection, and crash recovery.

## 1. rclone bisync

### Storage Engine

Flat listing files stored as text/JSON in a working directory (default: `~/.cache/rclone/bisync/` on Linux). Listing files are named based on Path1 and Path2 arguments, e.g., `path_to_local_tree..remote_subdir.lst`. Backup copies of listings are maintained for crash recovery. Filter file integrity is tracked via companion `.md5` files containing MD5 hashes.

### Per-File Fields

Each listing entry stores the fields available through rclone's `ObjectInfo` interface:

| Field | Type | Description |
|-------|------|-------------|
| Remote | string | Relative path from sync root |
| Size | int64 | File size in bytes |
| ModTime | time.Time | Modification timestamp |
| Hash | string | Content checksum (type depends on backend: MD5, SHA-1, Dropbox content hash, etc.) |
| ID | string | Backend-specific object ID (when available) |
| MimeType | string | MIME type (when available) |

The `--compare` flag controls which fields are actually used for comparison: `size`, `modtime`, `checksum`, or any combination thereof. The default inherits sync's standard comparison: size + modtime.

### Identity Model

Files are identified **by path** (the `Remote` field, relative to the sync root). There is no persistent file ID tracking across renames. Each listing is a flat map from path to metadata.

### Change Detection

Snapshot-based delta comparison between runs:

1. At startup, new listings are generated for both Path1 and Path2.
2. Current listings are diffed against the prior run's saved listings.
3. Files are classified as: **New**, **Newer**, **Older**, or **Deleted**.
4. The `--compare` flag determines which fields drive classification (size, modtime, checksum, or combinations).
5. The `--slow-hash-sync-only` optimization computes checksums only for the subset of files where modtime or size already changed, avoiding full-tree hashing.

### Conflict Detection

A conflict occurs when a file is "new or changed on both sides (relative to the prior run) AND is not currently identical on both sides." Before declaring a conflict, bisync performs an equality check using the same comparison function as `rclone check`. This catches false positives where both sides were edited to the same result. Conflicts are resolved by renaming one copy with a conflict suffix; `--conflict-resolve` controls which side wins (newer, older, larger, smaller, path1, path2).

### Move/Rename Detection

Standard bisync does **not** detect renames. A renamed directory appears as "all files deleted, then new files created." As of rclone v1.66, the `--track-renames` flag enables rename detection based on content hashing, but it cannot be used during `--resync`. The documentation advises renaming on both sides simultaneously as the most efficient workaround.

### Checkpoint/Recovery

Multi-layered recovery mechanism:

- **Lock files**: Created at run start (`.lck` extension with PID). Prevents concurrent runs.
- **Backup listings**: With `--recover`, one backup listing is always maintained representing the last known successful sync state.
- **Error state markers**: On critical errors, listings are renamed to `.lst-err`, blocking future runs and forcing `--resync`.
- **Graceful shutdown**: SIGINT/Ctrl+C triggers a clean shutdown with up to 60 seconds to save state.
- **Resync**: One-time initialization/recovery. `--resync-mode` (path1, path2, newer, older, larger, smaller) determines winner when files differ on both sides.

**Sources**: [rclone bisync docs](https://rclone.org/bisync/), [rclone fs package](https://pkg.go.dev/github.com/rclone/rclone/fs), [bisync issue #5675](https://github.com/rclone/rclone/issues/5675)

---

## 2. Syncthing

### Storage Engine

**SQLite** (since v2.0, released August 2025; previously LevelDB). The database is stored in Syncthing's config directory. Schema version is tracked and auto-migrated on upgrade (current schema version: 14). WAL journal mode is used for concurrent read/write access and crash safety.

The SQLite schema uses normalized tables with indirection for large data:

| Table | Purpose |
|-------|---------|
| `files` | Core file metadata: sequence, size, modified, name_idx, version_idx, blocklist_hash, deleted, local_flags, device_idx |
| `file_names` | Normalized file name storage (idx, name) |
| `fileinfos` | Serialized protobuf data (fiprotobuf) with sequence |
| `blocklists` | Block list data keyed by blocklist_hash, storing serialized protobuf |

For large files (>3 blocks), blocks are stored separately and referenced by hash. Version vectors with >10 entries are similarly indirected.

### Per-File Fields

The `FileInfo` protobuf message defines per-file metadata:

| Field | Type | Description |
|-------|------|-------------|
| name | string | Path relative to folder root (UTF-8 NFC, `/` separator) |
| type | enum | FILE, DIRECTORY, or SYMLINK_FILE/SYMLINK_DIRECTORY |
| size | int64 | File size in bytes |
| permissions | uint32 | Unix permission bits |
| modified_s | int64 | Modification time (seconds since Unix epoch) |
| modified_ns | int32 | Modification time nanosecond fraction |
| modified_by | uint64 | Short ID of the device that last modified the file |
| deleted | bool | Whether the file has been deleted |
| invalid | bool | Whether the file is invalid/unavailable |
| no_permissions | bool | Set when permission data is unavailable |
| version | VersionVector | Vector of (device_short_id, counter) pairs |
| sequence | int64 | Device-local monotonic clock at last local DB update |
| block_size | int32 | Size of individual blocks |
| blocks | []BlockInfo | List of (offset, size, hash) per block |
| blocks_hash | bytes | Hash of entire block list for quick comparison |
| symlink_target | string | Target path for symlinks |
| local_flags | uint32 | Local-only flags (not sent over protocol) |

Each `BlockInfo` contains: `offset` (int64), `size` (int32), `hash` (bytes, SHA-256).

### Identity Model

Files are identified by the tuple **(folder ID + relative path)**. There is no persistent inode or server-side ID tracking. The protocol specification states: "The combination of folder and name uniquely identifies each file in a cluster."

### Change Detection

Dual detection mechanism:

1. **Filesystem watcher** (inotify/FSEvents): Real-time notifications of local changes (enabled by default).
2. **Full scan**: Periodic rescan (default: hourly). During rescan, files are checked for changes to modification time, size, or permission bits. If a change is detected, the file is rehashed block-by-block to compute a new block list.
3. **Remote changes**: Received via the Block Exchange Protocol (BEP) from peer devices. Version vectors determine whether a remote change supersedes the local state.

### Conflict Detection

Conflicts are detected via **version vectors**. Each device maintains a counter in the version vector, incremented on each modification. When two devices modify the same file concurrently, their version vectors are incomparable (neither dominates the other), indicating a conflict.

Resolution: The file with the **older** modification time is renamed to `<filename>.sync-conflict-<date>-<time>-<modifiedBy>.<ext>`. If modification times are equal, the device with the larger device ID value loses. The conflict copy is then propagated to all devices as a normal file. For modification-vs-deletion conflicts, the deletion wins and the modified version is preserved as a conflict copy.

### Move/Rename Detection

Syncthing does **not** detect moves or renames. A rename appears as a deletion plus a creation. The receiving side reconstructs the "new" file block-by-block from the blocks of the "deleted" file (since blocks are content-addressed), avoiding retransmission over the network. However, for large directory renames, this can still be slow because every file must be re-scanned and blocks must be reassembled.

### Checkpoint/Recovery

- SQLite with WAL mode provides crash-safe transactions.
- The `sequence` field (device-local monotonic counter) enables resumption from the last known sync point.
- Syncthing can detect and rebuild from a corrupted or missing database by performing a full rescan of all files and re-exchanging state with peers.

**Sources**: [Syncthing BEP v1 spec](https://docs.syncthing.net/specs/bep-v1.html), [Syncthing database system (DeepWiki)](https://deepwiki.com/syncthing/syncthing/2.3-database-system), [Syncthing sync docs](https://docs.syncthing.net/users/syncing.html), [SQLite migration issue #9954](https://github.com/syncthing/syncthing/issues/9954)

---

## 3. Unison

### Storage Engine

**OCaml-serialized binary files** stored in `~/.unison/` (or the directory specified by `-root`). Archive files are named `arNNNNNNNNN` (hash-based naming). The format is tied to Unison's version (though since v2.52, no longer dependent on OCaml compiler version). Archives are human-unreadable binary blobs.

A separate fingerprint cache (`fpcache`) accelerates update detection by caching content fingerprints keyed by file metadata.

### Per-File Fields

The archive is a recursive tree structure defined in OCaml with four variants:

```ocaml
type archive =
  | ArchiveDir  of Props.t * archive NameMap.t
  | ArchiveFile of Props.t * Os.fullfingerprint * Fileinfo.stamp * Osx.ressStamp
  | ArchiveSymlink of string
  | NoArchive
```

**Props.t** (file properties):

| Field | Type | Description |
|-------|------|-------------|
| perm | Perm.t | File permissions |
| uid | Uid.t | User ID (owner) |
| gid | Gid.t | Group ID |
| time | Time.t | Modification time |
| length | Filesize.t | File size |
| typeCreator | TypeCreator.t | macOS type/creator codes |
| ctime | CTime.t | Change time (metadata change) |
| xattr | Xattr.t | Extended attributes |
| acl | ACL.t | Access control lists |

**ArchiveFile** additional fields:

| Field | Type | Description |
|-------|------|-------------|
| fullfingerprint | Os.fullfingerprint | Content fingerprint (MD5 or similar hash of full file contents) |
| stamp | Fileinfo.stamp | Fast-check stamp (InodeStamp, CtimeStamp, NoStamp, or RescanStamp) |
| ressStamp | Osx.ressStamp | macOS resource fork stamp |

**Fileinfo.stamp** variants: `InodeStamp` (inode number), `CtimeStamp` (change time), `NoStamp` (no fast check available), `RescanStamp` (force rescan).

### Identity Model

Files are identified **by path** within the replica tree. The archive is a tree structure keyed by path components (via `NameMap.t`). There is no inode or server-side ID tracking beyond the fast-check stamp.

### Change Detection

Two-phase detection:

1. **Fast check** (when `fastcheck=true`, the default on Unix): Compare the file's current inode number, modification time, and size against the archived stamp. If these match, the file is assumed unchanged. This avoids reading file contents.
2. **Full fingerprint check**: For files that may have changed (fast check failed or `fastcheck=false`), compute a content fingerprint (hash of entire file contents) and compare against the archived fingerprint.

The `dataClearlyUnchanged` function in `fpcache.ml` implements the fast check, comparing current `Props` and `stamp` against cached values.

### Conflict Detection

Unison compares each replica's current state against the shared archive (the last-known-synchronized state). If a file was modified on **both** replicas relative to the archive, it is a conflict. Unison also checks for "false conflicts" where both sides were changed to the same content (identical fingerprints).

The reconciliation algorithm presents conflicts to the user interactively for resolution. In batch mode, conflicts can be resolved by preference rules (e.g., `prefer=newer`).

### Move/Rename Detection

Unison does **not** detect renames. A rename appears as a deletion at the old path and a creation at the new path. Both changes are propagated independently.

### Checkpoint/Recovery

- Archives are updated atomically: a new archive is written to a temporary file, then renamed over the old one.
- Lock files prevent concurrent Unison instances from corrupting archives. If lock files are orphaned (e.g., after a crash), the `ignorelocks` preference skips them.
- If archives are corrupted or deleted, Unison starts from scratch, treating all files as new (conservative approach). This is always safe but may re-propagate files.
- Temporary files during transfer use `.unison.tmp` extension; files older than 30 days are considered safe to delete.

**Sources**: [Unison formal specification (PDF)](https://www.cis.upenn.edu/~bcpierce/papers/unisonspec.pdf), [Unison GitHub: update.ml](https://github.com/bcpierce00/unison/blob/master/src/update.ml), [Unison GitHub: props.ml](https://github.com/bcpierce00/unison/blob/master/src/props.ml), [Unison GitHub: fileinfo.ml](https://github.com/bcpierce00/unison/blob/master/src/fileinfo.ml)

---

## 4. rsync

### Storage Engine

rsync has **no persistent state database**. It is a stateless transfer tool. All change detection happens at runtime by comparing source and destination metadata and/or contents. No data is stored between runs.

### Per-File Fields (Runtime)

During transfer, rsync tracks per-file:

| Field | Description |
|-------|-------------|
| Path | Relative file path |
| Size | File size |
| ModTime | Modification timestamp |
| Permissions | Unix permission bits |
| Owner/Group | UID/GID |
| Checksum | MD5 (or xxHash in rsync 3.2+) whole-file checksum |
| Block checksums | Per-block rolling checksum (Adler-32 variant) + strong checksum (MD5/xxHash) |

### Identity Model

Files are identified **by path**. rsync builds a file list from source and destination, matched by relative path.

### Change Detection

The "quick check" algorithm (default) compares:

1. **Size**: If sizes differ, the file has changed.
2. **Modification time**: If mtimes differ, the file has changed.

With `--checksum`, rsync computes whole-file checksums on both sides and compares them, ignoring size/mtime. With `--size-only`, only size is compared.

For the delta transfer algorithm (reducing bytes sent over the wire):

1. The receiver splits its copy into fixed-size blocks and computes two checksums per block: a weak rolling checksum (Adler-32 variant) and a strong checksum (MD5/xxHash).
2. These checksums are sent to the sender.
3. The sender scans its file byte-by-byte using the rolling checksum to find matching blocks at arbitrary offsets. Matching candidates are verified with the strong checksum.
4. Only non-matching data (deltas) are transmitted.

### Conflict Detection

rsync has **no conflict detection**. It is a one-directional tool: source overwrites destination. The `--update` flag skips files that are newer on the destination, providing a basic guard.

### Move/Rename Detection

Not supported in standard rsync. The `--fuzzy` flag attempts to find renamed files on the destination by looking for files with similar names or content in the same directory, avoiding retransmission.

### Checkpoint/Recovery

- `--partial`: Keep partially transferred files for resume.
- `--partial-dir`: Store partial files in a separate directory.
- Transfers use temporary files (`.filename.XXXXXX`) that are renamed into place on completion, providing atomic file updates.
- `--append` resumes interrupted transfers by appending to existing files.

**Sources**: [rsync algorithm (Tridgell & Mackerras)](https://www.cs.tufts.edu/~nr/rsync.html), [How rsync works](https://rsync.samba.org/how-rsync-works.html), [rsync man page](https://man7.org/linux/man-pages/man1/rsync.1.html)

---

## 5. Dropbox (Nucleus)

### Storage Engine

**SQLite** (`nucleus.sqlite3`) on the local client. Additionally, encrypted SQLite databases (`filecache.dbx`, `config.dbx`) store supplementary metadata. The DBX files are encrypted with the SQLite Encryption Extension (SEE), using DPAPI-derived keys (on Windows).

Server-side, metadata is stored in a separate metaserver database, with a **Server File Journal (SFJ)** that acts as an append-only log of all edits, indexed by namespace ID.

### Per-File Fields

Client-side (reconstructed from forensic analysis and published documentation):

| Field | Description |
|-------|-------------|
| file_id | Globally unique file identifier (persistent across renames/moves) |
| path | File path relative to Dropbox root |
| size | File size |
| mtime | Local modification time |
| ctime | Creation time |
| content_hash | Dropbox content hash (SHA-256 of concatenated 4 MiB block SHA-256 hashes) |
| blocklist | Ordered list of block hashes identifying file contents |
| server_path | Canonical path on the server |

Server-side SFJ entries contain: namespace_id, journal_index (monotonic), filename, latest journal entry reference.

**Dropbox content hash algorithm**: File is split into 4 MiB blocks. SHA-256 of each block is computed. All block hashes are concatenated and SHA-256'd again. This produces a single 256-bit hash that can be computed incrementally and verified per-block.

### Identity Model

In the **Nucleus** engine, files are identified by a **globally unique file ID**, not by path. This was a fundamental change from the legacy "Sync Engine Classic" which keyed nodes by full path. ID-based identity means:

- Renames are cheap metadata operations (just update the path mapping).
- Shared folders and files maintain stable identity.
- No transient duplication or disappearance during renames.

### Change Detection

**Three-tree model**:

1. **Remote Tree**: Latest server state of the user's Dropbox.
2. **Local Tree**: Last observed state of the user's Dropbox on disk.
3. **Synced Tree**: Last known fully-synced state (the merge base).

Changes are detected by diffing each tree against the Synced Tree:
- `remote != synced` means a remote change occurred.
- `local != synced` means a local change occurred.
- If only one side changed, the change is propagated.
- If both sides changed, it is a conflict.

Server-side changes are received via a **cursor-based journal polling** mechanism: the client sends its last-known cursor position and receives all SFJ entries since that point.

Local changes are detected via filesystem watchers.

### Conflict Detection

Implicit in the three-tree model. When both `local != synced` and `remote != synced` for the same file, and the local and remote states differ from each other, it is a conflict. Dropbox renames the losing version with a "conflicted copy" suffix containing the device name and date.

### Move/Rename Detection

Nucleus handles renames natively because files are tracked by ID, not path. A rename on one side is simply a metadata update to the path associated with a file ID. The Synced Tree's node (keyed by ID) is updated with the new path. No content retransmission is needed.

### Checkpoint/Recovery

- SQLite provides transactional guarantees for the local database.
- The cursor-based journal mechanism allows the client to resume from any interruption by replaying journal entries from its last cursor position.
- Zero-crash policy: the client is designed to recover from every known error condition.
- Rust's type system is used to "design away invalid states" at compile time.
- The **planner** algorithm converges the three trees incrementally, producing a series of idempotent operations that can be safely retried after interruption.

**Sources**: [Rewriting the heart of our sync engine (Dropbox)](https://dropbox.tech/infrastructure/rewriting-the-heart-of-our-sync-engine), [Testing sync at Dropbox](https://dropbox.tech/infrastructure/-testing-our-new-sync-engine), [Streaming File Synchronization (Dropbox 2014)](https://dropbox.tech/infrastructure/streaming-file-synchronization), [Dropbox content hash docs](https://www.dropbox.com/developers/reference/content-hash), [Dropbox forensic artifacts](https://dereknewton.com/2011/04/forensic-artifacts-dropbox/)

---

## 6. git

### Storage Engine

**Custom binary format** stored at `.git/index` (the "index" or "staging area"). The file uses a compact binary encoding with a `DIRC` (DIRectory Cache) signature and format version number (versions 2, 3, or 4). Version 4 adds prefix compression for path names. Multiple optional extensions add supplementary functionality. The split index extension (`link`) allows sharing a base index across worktrees.

### Per-File Fields

Each index entry stores extensive stat(2) data:

| Field | Size | Description |
|-------|------|-------------|
| ctime_s | 32-bit | Last metadata change time (seconds) |
| ctime_ns | 32-bit | Last metadata change time (nanoseconds) |
| mtime_s | 32-bit | Last data change time (seconds) |
| mtime_ns | 32-bit | Last data change time (nanoseconds) |
| dev | 32-bit | Device ID |
| ino | 32-bit | Inode number |
| mode | 32-bit | Object type (4 bits) + unused (3 bits) + Unix permissions (9 bits) |
| uid | 32-bit | User ID |
| gid | 32-bit | Group ID |
| size | 32-bit | File size (truncated to 32-bit) |
| object_name | 160/256-bit | SHA-1 or SHA-256 hash of blob object |
| flags | 16-bit | assume-valid (1 bit) + extended (1 bit) + merge stage (2 bits) + name length (12 bits) |
| extended_flags | 16-bit | (v3+) skip-worktree, intent-to-add |
| path | variable | NUL-terminated relative path (v4: prefix-compressed) |

**Extensions**:

| Extension | Signature | Purpose |
|-----------|-----------|---------|
| Cache Tree | `TREE` | Pre-computed tree objects for unchanged directories |
| Resolve Undo | `REUC` | Higher-stage entries from resolved conflicts |
| Split Index | `link` | Changes to apply on a shared base index |
| Untracked Cache | `UNTR` | Cached untracked file lists |
| FS Monitor | `FSMN` | Integration with filesystem monitoring (watchman) |
| Index Entry Offset Table | `IEOT` | Multi-threaded index loading |
| Sparse Directory | `sdir` | Sparse checkout directory summaries |

### Identity Model

Files are identified by **path** (relative to repository root) and **content hash** (SHA-1 or SHA-256 of the blob object). The combination of path + blob hash uniquely identifies a versioned file. The device ID and inode number are stored but used only for fast change detection, not for identity.

### Change Detection

The "stat cache" approach:

1. Compare the file's current stat(2) data (dev, ino, mtime, ctime, size, uid, gid, mode) against the cached values in the index.
2. If **all** stat fields match, the file is assumed unchanged (no need to hash the contents).
3. If any stat field differs, the file is re-hashed and compared against the stored blob hash.

**Racily clean problem**: If a file is modified within the same second as the index was last written, the mtime comparison is ambiguous. Git handles this by "smudging" the cached size to zero for entries with mtime equal to the index file's mtime, forcing a content comparison on the next check.

**FS Monitor extension**: Integrates with external filesystem watchers (e.g., Watchman) to track which files changed since the last check, avoiding full stat() scans.

### Conflict Detection

The 2-bit **merge stage** field in entry flags:
- Stage 0: Normal (no conflict)
- Stage 1: Common ancestor version
- Stage 2: "Ours" version
- Stage 3: "Theirs" version

During a merge, conflicted files have entries at stages 1-3 simultaneously. The `REUC` (Resolve Undo) extension preserves higher-stage entries after conflict resolution, enabling `git checkout --conflict`.

### Move/Rename Detection

Git does **not** track renames explicitly. Instead, `git diff` and `git log` use heuristic rename detection at display time: if a deletion and an addition have sufficiently similar content (above a configurable similarity threshold, default 50%), they are reported as a rename. This is purely a post-hoc analysis, not tracked in the index.

### Checkpoint/Recovery

- **Lock file**: `.git/index.lock` is created atomically before writing. If it already exists, the operation fails (preventing concurrent modifications). Orphaned lock files after crashes must be manually removed.
- **Atomic update**: The new index is written to `.git/index.lock` and then renamed to `.git/index` on completion. This ensures the index is always in a consistent state.
- **No WAL or journaling**: The index is a single file overwritten atomically. There is no incremental update mechanism (the split index extension provides partial support).

**Sources**: [git index-format docs](https://git-scm.com/docs/index-format), [racy-git docs](https://git-scm.com/docs/racy-git/2.0.5), [Git Internals (MSDN)](https://learn.microsoft.com/en-us/archive/msdn-magazine/2017/august/devops-git-internals-architecture-and-index-files)

---

## Cross-Tool Comparison

### Storage Engine Summary

| Tool | Engine | Format | Persistent? |
|------|--------|--------|-------------|
| rclone bisync | Flat files | Text/JSON listing files + MD5 checksums | Yes |
| Syncthing | SQLite (v2.0+) | Normalized relational tables + protobuf blobs | Yes |
| Unison | Custom binary | OCaml-serialized archive trees | Yes |
| rsync | None | N/A (stateless) | No |
| Dropbox | SQLite | nucleus.sqlite3 + encrypted .dbx files | Yes |
| git | Custom binary | `.git/index` with DIRC header + extensions | Yes |

### Per-File Metadata Comparison

| Metadata | rclone | Syncthing | Unison | rsync | Dropbox | git |
|----------|--------|-----------|--------|-------|---------|-----|
| Path | Y | Y | Y (tree key) | Y | Y | Y |
| Size | Y | Y | Y | Y | Y | Y (32-bit) |
| ModTime | Y | Y (ns) | Y | Y | Y | Y (ns) |
| Content hash | Optional | Y (per-block SHA-256) | Y (full-file) | Y (at runtime) | Y (block-based SHA-256) | Y (SHA-1/256) |
| Permissions | N | Y | Y | Y | N | Y (partial) |
| Owner (uid/gid) | N | N | Y | Y | N | Y |
| Inode | N | N | Y (stamp) | N | N | Y |
| Device ID | N | N | N | N | N | Y |
| CTime | N | N | Y | N | Y | Y (ns) |
| Extended attrs | N | N | Y | Y (with -X) | N | N |
| ACLs | N | N | Y | Y (with -A) | N | N |
| Block list | N | Y | N | Y (runtime) | Y | N |
| Version vector | N | Y | N | N | N | N |
| Server/file ID | Backend-specific | N | N | N | Y | N |
| Symlink target | N | Y | Y | Y | N | Y (gitlink) |
| Deleted flag | N | Y | N | N | N | N |
| Merge stage | N | N | N | N | N | Y |

### Identity Model Comparison

| Tool | Primary Identity | Stable Across Renames? | Content-Addressed? |
|------|-----------------|----------------------|-------------------|
| rclone bisync | Path | No | No (optional hash comparison) |
| Syncthing | Folder + Path | No | Yes (block hashes) |
| Unison | Path (tree position) | No | Yes (fingerprint) |
| rsync | Path | No | Partial (--fuzzy) |
| Dropbox (Nucleus) | **File ID** | **Yes** | Yes (content hash) |
| git | Path + blob hash | No (heuristic detection) | Yes (blob SHA) |

### Change Detection Comparison

| Tool | Primary Method | Fast Path | Full Verification |
|------|---------------|-----------|-------------------|
| rclone bisync | Diff listings between runs | Size + modtime | Checksum comparison |
| Syncthing | FS watcher + periodic scan | Modtime + size + perms | Block-level rehash |
| Unison | Diff replica vs archive | Inode + modtime + size (fastcheck) | Full file fingerprint |
| rsync | Compare source vs destination | Size + modtime (quick check) | Whole-file checksum |
| Dropbox | Three-tree diff | FS watcher events | Content hash comparison |
| git | Stat cache comparison | All stat(2) fields | Blob hash comparison |

### Conflict Detection Comparison

| Tool | Mechanism | Resolution |
|------|-----------|------------|
| rclone bisync | Changed-on-both-sides check with equality verification | Configurable: rename loser with conflict suffix |
| Syncthing | Version vector comparison (incomparable vectors = conflict) | Rename older file with `.sync-conflict-*` suffix |
| Unison | Both replicas changed vs archive, different fingerprints | Interactive user choice (or batch preferences) |
| rsync | None (unidirectional) | Source always wins |
| Dropbox | Both trees differ from synced tree, differ from each other | Rename loser as "conflicted copy" |
| git | Merge stage 1/2/3 entries in index | User resolves manually; rerere for automation |

### Move/Rename Detection Comparison

| Tool | Detection Method | Avoids Retransmission? |
|------|-----------------|----------------------|
| rclone bisync | `--track-renames` (content hash matching) | Yes (when enabled) |
| Syncthing | None (delete + create), but blocks are content-addressed | Partially (no network retransmit, but local reassembly) |
| Unison | None (delete + create) | No |
| rsync | `--fuzzy` (name/content similarity heuristic) | Yes (when enabled) |
| Dropbox (Nucleus) | **Native (file ID tracking)** | **Yes (metadata-only update)** |
| git | Heuristic (similarity threshold) at display time | N/A (not a transfer tool) |

---

## Key Patterns and Best Practices

### Pattern 1: Three-State Comparison (The "Merge Base")

The most robust sync tools maintain a **third reference state** representing the last known synchronized state. Both Dropbox (Synced Tree), Unison (archive), and rclone bisync (prior listings) implement this pattern. Change detection becomes:

```
local_change  = (current_local  != last_synced)
remote_change = (current_remote != last_synced)
conflict      = local_change AND remote_change AND (current_local != current_remote)
```

This is fundamentally the same model as a three-way merge in version control. Without a merge base, the system cannot distinguish "A changed and B didn't" from "B changed and A didn't" -- it can only see that the two sides differ.

**Takeaway**: A sync state database must store the last-known-synced state for every tracked file. This is non-negotiable for correct bidirectional sync.

### Pattern 2: Fast Check Before Content Hash

Every tool with persistent state implements a two-tier change detection strategy:

1. **Fast check**: Compare cheap metadata (size, mtime, inode) against cached values. If identical, assume no change.
2. **Slow check**: Compute content hash only when the fast check indicates a possible change.

This is essential for performance at scale. Full-tree hashing is prohibitively expensive for large file sets. The fast check reduces the set of files that need hashing to only those that actually changed.

**Takeaway**: Store enough stat(2) metadata (at minimum: size + mtime; ideally also inode) to enable fast change detection without hashing.

### Pattern 3: ID-Based vs Path-Based Identity

The single biggest architectural decision is whether to track files by path or by a stable ID:

- **Path-based** (rclone, Syncthing, Unison, rsync, git): Simple, universal, works on any filesystem. But renames are destructive (delete + create), causing unnecessary retransmission and potential data loss during conflict resolution.
- **ID-based** (Dropbox Nucleus): Renames are cheap metadata operations. Shared resources maintain stable identity. But requires a server that assigns and tracks IDs (or local inode tracking, which is fragile).

For a cloud sync tool like OneDrive, the server provides stable item IDs, making ID-based tracking both feasible and advantageous. However, local filesystems do not provide stable IDs (inodes can be reused after deletion), so local-side identity typically falls back to path-based with heuristic rename detection.

**Takeaway**: Use server-provided item IDs as the primary identity key. Use path as the human-readable key and for local filesystem mapping. Detect renames by correlating "deleted ID at old path" with "same ID appeared at new path."

### Pattern 4: Block-Level Deduplication

Both Syncthing and Dropbox split files into blocks and hash each block independently:

- Enables block-level deduplication (identical blocks are not stored/transferred twice).
- Enables efficient delta transfers (only changed blocks are sent).
- The block hash list itself becomes a content identifier.

Dropbox's approach (4 MiB blocks, SHA-256 of concatenated block hashes) is particularly clean for verification: you can verify any individual block without downloading the whole file.

**Takeaway**: Block-level hashing is valuable for large files but adds complexity. For a Graph API client where the API does not support block-level operations (only whole-file upload/download for files under 250 MiB), block-level hashing may not be worth the implementation cost. Whole-file content hashing (QuickXorHash for OneDrive) is sufficient.

### Pattern 5: Atomic State Updates

Every tool uses some form of atomic update to prevent corruption:

| Tool | Mechanism |
|------|-----------|
| rclone bisync | Write new listing, rename into place; backup listings for recovery |
| Syncthing | SQLite transactions with WAL |
| Unison | Write to temp file, atomic rename |
| git | Write to `.git/index.lock`, atomic rename to `.git/index` |
| Dropbox | SQLite transactions |

**Takeaway**: SQLite with WAL mode is the industry-standard choice for sync state databases. It provides ACID transactions, crash recovery, concurrent access, and is widely tested. This aligns with the project's existing decision to use SQLite.

### Pattern 6: The Racily-Clean Problem

Git's "racily clean" problem illustrates a fundamental challenge: if a file is modified within the same timestamp granularity as the index update, the modification is invisible to fast checks. Solutions:

- **git**: Smudge the cached size to zero, forcing a content comparison next time.
- **Unison**: Uses inode numbers as an additional fast-check signal.
- **Syncthing**: Uses nanosecond-precision timestamps plus inode-equivalent metadata.

**Takeaway**: Store timestamps at the highest available precision (nanoseconds). Consider storing inode numbers as an additional change detection signal. Always have a fallback to content hashing when metadata is ambiguous.

### Pattern 7: Cursor-Based Remote Change Tracking

Both Dropbox (SFJ cursor) and OneDrive (delta query cursor) use a cursor/token mechanism for efficient remote change detection:

1. Client stores a cursor representing its last-known server state.
2. On each sync, client sends the cursor and receives only changes since that point.
3. New cursor is stored after processing changes.

This is far more efficient than listing all remote files every time. The cursor is a critical piece of sync state that must be persisted reliably.

**Takeaway**: The delta query token from Microsoft Graph API is a first-class piece of sync state. Store it in the database alongside file metadata. Handle HTTP 410 (token expired) by falling back to full enumeration.

---

## Summary: Recommended Data Model for onedrive-go

Based on this survey, the recommended sync state model for onedrive-go should:

1. **Use SQLite with WAL mode** as the storage engine (industry consensus, matches PRD decision).
2. **Maintain three states per file**: local observed, remote observed, last synced. This enables correct bidirectional conflict detection.
3. **Use server-provided item IDs** as the primary key for tracking files. This enables native rename detection and stable identity.
4. **Store comprehensive per-file metadata**: item ID, path, parent ID, size, mtime (nanosecond precision), content hash (QuickXorHash + cTag), eTag, inode (for local fast check), and delta cursor.
5. **Implement two-tier change detection**: fast check (size + mtime + inode) followed by content hash verification only when fast check indicates a change.
6. **Persist the delta query cursor** as a first-class database field, enabling efficient incremental remote change detection.
7. **Track file type** (file, directory, symlink, remote item) to handle each type appropriately.
8. **Include a deleted/tombstone flag** rather than immediately removing entries, enabling rename detection and conflict resolution.
9. **Use atomic transactions** for all state updates, ensuring the database is never in an inconsistent state after a crash.
10. **Design for move detection** by correlating item ID appearances and disappearances across paths between sync runs.
