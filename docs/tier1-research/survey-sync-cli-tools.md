# Survey of File Sync CLI Tools

A comprehensive survey of file synchronization and backup CLI tools, documenting features, patterns, innovations, and gaps across the landscape. This informs our sync engine design by identifying what is table stakes, what is innovative, and what is missing.

---

## 1. azcopy (Microsoft)

**What it is:** Microsoft's official CLI for copying data to/from Azure Blob Storage and Azure Files. Written in Go. Open source.

### Core Commands
- `azcopy copy` -- copy files/blobs between local and Azure, or between Azure endpoints
- `azcopy sync` -- one-way sync (source to destination), comparing by name + last-modified or MD5 hash
- `azcopy list` -- list blobs/files in a container
- `azcopy remove` -- delete blobs/files
- `azcopy jobs list/show/resume/clean` -- manage transfer jobs
- `azcopy bench` -- run upload/download performance benchmarks
- `azcopy login` -- authenticate via Azure AD, SAS tokens, or service principals

### Sync Model
- **One-way only** (source to destination). Not bidirectional.
- Compares by file name + last-modified timestamp (default) or MD5 hash (`--compare-hash`).
- `--mirror-mode=true` overwrites all destination files regardless of timestamps.
- `--delete-destination=true` removes destination files not present at source.
- `--dry-run` previews operations without executing.

### Conflict Handling
- No conflict detection. Last-write-wins by timestamp comparison. If source is newer (or `--mirror-mode`), it overwrites. No merge, no rename, no user prompt.

### Filtering
- `--include-pattern` / `--exclude-pattern` -- glob patterns on file names
- `--include-path` / `--exclude-path` -- specific path prefixes
- `--include-after` / `--include-before` -- date-based filtering
- `--exclude-regex` -- regular expression patterns
- Performance warning: filtering on large item counts can be very slow (scanning overhead).

### Performance
- **Parallel transfers** via `AZCOPY_CONCURRENCY_VALUE` env var (concurrent HTTP requests).
- **Chunk-based uploads** -- files split into chunks, uploaded in parallel.
- **Server-to-server copy** -- Azure-to-Azure copies bypass client bandwidth entirely.
- `--cap-mbps` -- throttle bandwidth.
- `--block-size-mb` -- control chunk size for block blobs.
- No built-in compression. Data transferred as-is.

### Configuration
- Primarily CLI flags and environment variables. No config file.
- Job plan files stored in `~/.azcopy/` for resume capability.
- Structured log files per job.

### Notable Innovations
- **Job resume** -- every transfer is a job with a plan file; failed/paused jobs can be resumed from where they left off with `azcopy jobs resume`.
- **Benchmark mode** -- generates test data in memory, uploads to destination, measures throughput, deletes test data. Built-in performance testing.
- **Server-side copy** -- Azure-to-Azure transfers happen server-side, zero client bandwidth.
- Written in Go (relevant: same language as our project).

### Limitations
- Azure-only. Cannot talk to S3, GCS, or other clouds.
- One-way sync only. No bidirectional.
- No daemon/watch mode. Must be invoked each time.
- No compression.
- Filtering can be slow on large datasets.
- No conflict resolution beyond timestamp comparison.

---

## 2. AWS CLI s3 sync (Amazon)

**What it is:** Amazon's official CLI for S3 operations. The `aws s3 sync` command synchronizes directories to/from S3.

### Core Commands
- `aws s3 sync` -- sync directories (local-to-S3, S3-to-local, S3-to-S3)
- `aws s3 cp` -- copy files (supports `--recursive`)
- `aws s3 mv` -- move files
- `aws s3 rm` -- remove files
- `aws s3 ls` -- list buckets/objects
- `aws s3 mb/rb` -- make/remove buckets

### Sync Model
- **One-way only**. Syncs from source to destination.
- By default, compares file size + last-modified timestamp. Transfers only if source is newer or size differs.
- `--size-only` -- compare by size only, ignore timestamps.
- `--exact-timestamps` -- require exact timestamp match (useful for S3-to-S3).
- `--no-overwrite` -- never overwrite existing destination files.
- `--delete` -- remove destination files not present at source.
- `--dryrun` -- preview without executing.

### Conflict Handling
- No conflict detection. Overwrites based on size/timestamp comparison. No merge, no rename.

### Filtering
- `--include` / `--exclude` -- glob patterns.
- **Important caveat**: by default all files are included. `--include` only re-includes files previously excluded. Must use `--exclude '*'` first, then `--include '*.txt'` to select specific extensions.
- Patterns evaluated in order; last match wins.

### Performance
- **Multithreaded** -- multiple concurrent requests in flight by default.
- `max_concurrent_requests` -- configurable concurrency (default varies by CLI version).
- **Multipart upload/download** -- automatic for files above threshold.
- `multipart_threshold` -- default 8 MB. Files above this size use multipart.
- `multipart_chunksize` -- default 8 MB, minimum 5 MB.
- `max_queue_size` -- controls memory usage for pending transfers.
- No built-in compression.

### Configuration
- `~/.aws/config` and `~/.aws/credentials` files.
- S3-specific settings nested under `[profile]` > `s3` key.
- Environment variables (`AWS_*`).
- `aws configure set` for programmatic config.

### Notable Innovations
- **Automatic multipart** -- seamlessly switches to multipart upload/download based on file size with no user intervention.
- **S3-to-S3 server-side copy** -- cross-region or cross-account copies without downloading.
- Deep integration with AWS IAM for fine-grained access control.

### Limitations
- AWS S3 only. Single-cloud tool.
- One-way sync only.
- No daemon/watch mode.
- No compression.
- No conflict resolution.
- Include/exclude semantics are counterintuitive (must exclude first, then include).
- Does not preserve custom metadata during sync (use MinIO Client for that).

---

## 3. restic

**What it is:** A modern backup program written in Go. Not a sync tool per se, but a snapshot-based backup tool with deduplication and encryption.

### Core Commands
- `restic init` -- create a new encrypted repository
- `restic backup` -- create a snapshot
- `restic restore` -- restore files from a snapshot
- `restic snapshots` -- list snapshots
- `restic diff` -- compare two snapshots
- `restic mount` -- FUSE-mount a repository for browsing
- `restic forget` -- remove snapshots by retention policy
- `restic prune` -- remove unreferenced data from repository
- `restic check` -- verify repository integrity
- `restic find` / `restic ls` -- search/list files in snapshots
- `restic copy` -- copy snapshots between repositories
- `restic stats` -- show repository statistics

### Sync Model
- **Snapshot-based backup** (push only). No sync, no bidirectional.
- Each `restic backup` creates an immutable snapshot.
- Deduplication ensures only changed chunks are stored.
- Restore is a separate operation; no continuous sync.

### Conflict Handling
- N/A. Backup tool. Multiple snapshots coexist; no overwrites, no conflicts.

### Filtering
- `--exclude` / `--iexclude` (case-insensitive) -- glob patterns.
- `--exclude-file` / `--iexclude-file` -- read patterns from file.
- `--exclude-if-present` -- skip directories containing a marker file.
- `--exclude-larger-than` -- skip files above a size limit.
- Negation patterns with `!` prefix (gitignore-style).

### Performance
- **Content-defined chunking** -- variable-size chunks using rolling hash (Rabin fingerprint). Only new chunks stored.
- `--read-concurrency` -- parallel file reading (default 2).
- Compression: `--compression auto|off|max` (repository format v2).
- **Files cache** -- in-memory hash tables for fast change detection on subsequent backups.
- Single-threaded core, but I/O parallelism via read concurrency.

### Configuration
- Primarily CLI flags and environment variables (`RESTIC_REPOSITORY`, `RESTIC_PASSWORD`, `RESTIC_COMPRESSION`, etc.).
- No config file. Profiles are typically managed via wrapper scripts.

### Notable Innovations
- **Content-defined chunking with dedup** -- variable-size chunks mean insertions/deletions at the start of a file don't invalidate all subsequent chunks. Far more efficient than fixed-size chunking.
- **Client-side encryption** -- AES-256 in counter mode with Poly1305-AES. All data encrypted before leaving the client.
- **FUSE mount for browsing** -- mount any snapshot as a read-only filesystem. Browse and selectively restore.
- **Integrity verification** -- `restic check --read-data` verifies every pack file's integrity.
- **Backend agnostic** -- works with local disk, SFTP, S3, B2, Azure, GCS, REST server, or any rclone remote.
- Written in Go (relevant: same language as our project).

### Limitations
- Not a sync tool. Cannot do bidirectional or continuous sync.
- Single-threaded core (parallelism limited to I/O).
- No native cloud push -- relies on backends for cloud storage.
- FUSE mount only on Linux/macOS/FreeBSD.

---

## 4. Syncthing

**What it is:** A peer-to-peer continuous file synchronization program. Runs as a daemon with a web UI; CLI is secondary.

### Core Commands
- `syncthing` -- run the daemon
- `syncthing cli` -- command-line interface for configuration
- Configuration primarily via XML file or web UI / REST API.
- No traditional CLI "sync" command -- it runs continuously.

### Sync Model
- **Continuous bidirectional peer-to-peer sync** (daemon mode).
- **Folder types**: Send-Receive (full bidirectional), Send-Only, Receive-Only.
- **Block Exchange Protocol (BEP)** -- custom protocol, not HTTP-based.
- Files split into blocks (128 KiB to 16 MiB depending on file size).
- Blocks can be sourced from any connected peer that has them.
- No central server. Peers discover each other via relay servers or local discovery.

### Conflict Handling
- **Rename-based**: When both sides modify the same file, the version with the older mtime is renamed to `<file>.sync-conflict-<date>-<time>-<deviceID>.<ext>`.
- If mtimes are equal, the device with the larger device ID "loses" and gets renamed.
- `maxConflicts` config: how many conflict copies to keep (default 10, -1 = unlimited, 0 = disabled).
- No automatic merge. User must manually resolve by choosing which version to keep.

### Filtering
- `.stignore` file in each synced folder root.
- Glob patterns, one per line. First matching pattern wins.
- Supports `!` negation, `//` comments.
- `#include` directive to include patterns from another file.
- Patterns are per-folder, not global.

### Performance
- **Block-level dedup** -- only changed blocks transferred.
- **Multi-peer parallelism** -- blocks fetched from all available peers simultaneously.
- **Rename detection** -- renamed files are not retransmitted.
- Optional compression (per-connection, configurable).
- All data encrypted in transit (TLS 1.3).

### Configuration
- XML config file (`~/.local/state/syncthing/config.xml` or similar).
- Web UI on port 8384.
- REST API for programmatic control.
- Per-folder and per-device settings.

### Notable Innovations
- **Untrusted/encrypted devices** -- a device can store encrypted data without being able to read it (XChaCha20-Poly1305 + AES-SIV). Enables cloud relay without trusting the server.
- **File versioning** -- built-in support for trash can, simple, staggered, or external versioning. Keeps old versions of modified/deleted files.
- **Multi-peer block sourcing** -- can download different blocks of the same file from different peers simultaneously.
- **No central server** -- fully decentralized. Relay servers only for NAT traversal.
- **Conflict copy retention** -- configurable number of conflict copies to keep.

### Limitations
- Not a cloud storage client. Cannot sync to S3, Azure, OneDrive, etc.
- Requires Syncthing running on all peers.
- CLI is secondary to the web UI; most configuration done via web/API.
- No selective sync (all-or-nothing per folder, unless using ignore patterns).
- No true conflict merge -- just renames conflicting files.

---

## 5. Unison

**What it is:** A bidirectional file synchronizer for Unix and Windows. Written in OCaml. Academic pedigree (UPenn).

### Core Commands
- `unison <profile>` -- run sync using a named profile
- `unison <root1> <root2>` -- sync two directories
- Key flags: `-batch` (non-interactive), `-auto` (accept non-conflicting), `-repeat watch` (continuous mode)

### Sync Model
- **True bidirectional sync**. The gold standard for bidirectional file synchronization.
- Maintains a local archive of the previous state of both replicas. Compares current state against archive to detect changes on each side.
- Three-way merge: previous state, replica A, replica B.
- Can operate over SSH, direct socket, or locally.
- `-repeat watch` -- continuous mode with filesystem monitoring (inotify/fsevents).
- `-repeat N` -- poll every N seconds.

### Conflict Handling
- **Interactive by default**: presents each conflict to the user for resolution.
- `-batch` mode skips conflicts (leaves them unresolved).
- `-auto` mode accepts all non-conflicting changes automatically.
- `copyonconflict` -- makes backup copies of files that would be overwritten during conflict resolution.
- `merge` / `mergebatch` -- invoke external merge tools (e.g., diff3) for automatic conflict resolution.
- `prefer` preference -- automatically prefer one side in conflicts (e.g., `prefer = newer`).

### Filtering
- `ignore` preference -- glob patterns to skip files/directories.
- `ignorenot` -- exception to ignore (but cannot override a parent directory ignore).
- `path` preference -- explicitly specify which paths to sync (positive selection).
- `follow` -- follow symlinks matching pattern.
- `immutable` -- mark directories as immutable to skip change detection (performance optimization).

### Performance
- **rsync-like delta transfer** -- only changed blocks of files are transmitted.
- Compression of transfers (similar to rsync's algorithm).
- `immutable` directories skip scanning entirely.
- `fastcheck` -- use file size + mtime instead of content hash for change detection (faster but less safe).
- Single-threaded. No parallel transfers.

### Configuration
- **Profile files** in `~/.unison/<profilename>.prf`.
- Rich preference system with dozens of options.
- Can include other profiles.
- CLI flags override profile settings.

### Notable Innovations
- **Three-way merge algorithm** -- the canonical approach to bidirectional sync. Maintains archive of previous synchronized state to distinguish "changed on A" from "changed on B" from "changed on both."
- **External merge tool integration** -- can invoke diff3, emacs, or custom merge scripts for automatic conflict resolution.
- **Academic rigor** -- formal specification of synchronization semantics. Proven correct.
- **Continuous watch mode** -- `-repeat watch` with filesystem monitoring for near-real-time sync.
- **`copyonconflict`** -- safety net for automated conflict resolution.

### Limitations
- **Version compatibility** -- both sides must run the same major version of Unison. Version mismatches cause failures. This is the most-cited pain point.
- Single-threaded. No parallel file transfers.
- No cloud storage support. Local or SSH only.
- No encryption at rest. Relies on SSH for transport encryption.
- No deduplication.
- OCaml dependency can be challenging to build/install.

---

## 6. rsync

**What it is:** The classic Unix file transfer and synchronization tool. Written in C. The baseline against which all sync tools are measured.

### Core Commands
- `rsync [options] source destination` -- the only command. Everything via flags.
- Key flags: `-a` (archive), `-v` (verbose), `-z` (compress), `-P` (progress + partial), `--delete`, `-n` (dry run)

### Sync Model
- **One-way only** (source to destination). Not bidirectional.
- Delta transfer algorithm: computes rolling checksums of destination file blocks, sends only differing blocks from source.
- `--delete` -- remove destination files not present at source.
- `--update` -- skip files newer on destination.
- `--ignore-existing` -- skip files that already exist on destination.

### Conflict Handling
- None. One-way tool. Source always wins. Use `--backup` to keep overwritten files.

### Filtering
- `--include` / `--exclude` -- glob patterns evaluated in order.
- `--include-from` / `--exclude-from` -- read patterns from file.
- `--filter` -- combined filter rules (more flexible than include/exclude).
- `.rsync-filter` files -- per-directory filter rules (similar to .gitignore).
- `--cvs-exclude` -- ignore version control directories.
- Patterns support `*`, `**`, `?`, `[...]`, and directory-only suffix `/`.

### Performance
- **Delta transfer algorithm** -- the rsync algorithm. Only changed blocks transferred. This is rsync's defining innovation.
- Compression: `-z` flag, supports zstd, lz4, zlib.
- **Single-threaded** by default. C implementation, very fast for single streams.
- `--whole-file` -- skip delta calculation for scenarios where full transfer is faster.
- `--checksum` -- compare by checksum instead of mtime+size (slower but more reliable).
- Parallel transfers possible via `--multi-thread-streams` or external wrappers (`parallel`, `xargs`).

### Configuration
- No config file. All via CLI flags.
- Common patterns wrapped in shell scripts or cron jobs.
- `rsyncd.conf` for daemon mode (serving files over rsync protocol).

### Notable Innovations
- **The rsync algorithm itself** -- rolling checksum + strong checksum for efficient delta transfer. Published in 1996, still the foundation of most sync tools.
- **Per-directory filter files** -- `.rsync-filter` in each directory, inherited by subdirectories.
- **`--backup` and `--backup-dir`** -- keep copies of overwritten/deleted files.
- **`--partial` and `--partial-dir`** -- resume interrupted transfers.
- **Daemon mode** -- can serve files over the rsync protocol (port 873).

### Limitations
- **One-way only**. No bidirectional sync.
- Single-threaded core.
- No encryption (relies on SSH wrapper). No encryption at rest.
- No deduplication across runs.
- No cloud storage support. Local or SSH only.
- No watch/daemon mode for continuous sync (must use cron or external watcher).
- No checksumming of destination after transfer (must use `--checksum` which recalculates everything).

---

## 7. Nextcloud CLI (nextcloudcmd)

**What it is:** Command-line client bundled with the Nextcloud desktop client. Performs one-shot sync against a Nextcloud/ownCloud server.

### Core Commands
- `nextcloudcmd <local-dir> <server-url>` -- sync a local directory with a Nextcloud server.
- That is essentially the only command. Single purpose.

### Sync Model
- **Bidirectional sync** -- propagates changes in both directions.
- **One-shot execution** -- runs once and exits. No daemon mode, no filesystem watching.
- Must be invoked repeatedly (e.g., via cron) for continuous sync.
- `--max-sync-retries N` -- retry failed syncs (default 3).

### Conflict Handling
- Same as Nextcloud desktop client: rename-based conflict files.
- Limited documentation on CLI-specific conflict behavior.

### Filtering
- `--exclude <file>` -- specify an exclude list file.
- `--unsyncedfolders <file>` -- selective sync (exclude specific server folders).
- `-h` flag to sync hidden files.
- Requires a `sync-exclude.lst` file (installed with the desktop client or specified explicitly).

### Performance
- Inherits desktop client's transfer engine.
- No documented parallelism controls.
- No chunking configuration exposed via CLI.
- No compression controls.

### Configuration
- CLI flags only. No config file.
- Authentication via `--user`/`--password` or netrc.
- `--non-interactive` for scripted use.
- `--trust` to trust self-signed certificates.

### Notable Innovations
- Very few. It is a thin CLI wrapper around the desktop sync engine.

### Limitations
- Nextcloud/ownCloud only. Cannot sync to any other cloud service.
- One-shot only. No daemon, no watch mode.
- Very limited CLI options. Most configuration must be done via the desktop client.
- Requires the desktop client to be installed (shared binary).
- Sparse documentation.
- No parallel transfer controls, no chunking config, no bandwidth throttling.

---

## 8. dbxcli (Dropbox)

**What it is:** Official Dropbox command-line client. Written in Go. Open source. More of a file manager than a sync tool.

### Core Commands
- `dbxcli ls` -- list files
- `dbxcli get <remote> [local]` -- download a file
- `dbxcli put <local> <remote>` -- upload a file
- `dbxcli cp` / `dbxcli mv` -- copy/move files on Dropbox
- `dbxcli mkdir` -- create directory
- `dbxcli rm` -- delete files
- `dbxcli search` -- search for files
- `dbxcli revs` -- list file revisions
- `dbxcli restore` -- restore a file to a previous revision
- `dbxcli du` -- display usage information
- `dbxcli team` -- team management (add/remove members, list groups)

### Sync Model
- **No sync command at all**. This is a file management tool, not a sync tool.
- Individual file upload/download only.
- No directory sync, no delta detection, no continuous mode.

### Conflict Handling
- N/A. No sync = no conflicts. Individual file operations only.

### Filtering
- N/A. No directory operations beyond `ls`.

### Performance
- No documented parallelism, chunking, or compression features.
- Simple sequential file operations.

### Configuration
- OAuth2 authentication. Tokens stored in `~/.config/dbxcli/auth.json`.
- No config file beyond auth.

### Notable Innovations
- `revs` and `restore` -- access Dropbox's built-in file versioning from CLI.
- Team management commands for Dropbox Business.
- Written in Go.

### Limitations
- **Not a sync tool**. No directory sync, no delta transfer, no watch mode.
- No bulk operations beyond individual file get/put.
- No filtering, no parallelism, no bandwidth control.
- Appears minimally maintained (last significant updates are old).

---

## 9. borgbackup (Borg)

**What it is:** A deduplicating backup program with compression and authenticated encryption. Written in Python/Cython.

### Core Commands
- `borg init` -- create an encrypted repository
- `borg create` -- create a backup archive
- `borg extract` -- restore files from an archive
- `borg list` -- list archives or files in an archive
- `borg diff` -- compare two archives
- `borg mount` -- FUSE-mount a repository
- `borg prune` -- delete old archives by retention policy
- `borg compact` -- free space in repository
- `borg check` -- verify repository integrity
- `borg key` -- manage encryption keys
- `borg info` -- show archive/repository info

### Sync Model
- **Snapshot-based backup** (push only). Not a sync tool.
- Each `borg create` produces an archive (snapshot).
- Deduplication ensures only new/changed chunks are stored.

### Conflict Handling
- N/A. Backup tool. Archives are immutable. No conflicts possible.

### Filtering
- `--exclude` -- glob/shell/regex patterns.
- `--exclude-from` -- read patterns from file.
- `--pattern` / `--patterns-from` -- combined include (`+`) and exclude (`-`) patterns with prefixes.
- `--exclude-if-present` -- skip directories containing a marker file (e.g., `.nobackup`).
- Multiple pattern styles: fnmatch, shell, regex, path prefix, path full.
- O(1) hashtable-based pattern matching -- huge pattern lists have minimal performance impact.
- Negation-aware: `!` prefix for non-recursive exclusion.

### Performance
- **Content-defined chunking** -- variable-size chunks using Buzhash rolling hash. Only unique chunks stored.
- Compression: lz4 (default, fast), zstd, zlib, lzma (configurable per-archive).
- Client-side encryption: AES-OCB or ChaCha20-Poly1305.
- **Single-threaded core** historically. Borg 2.0 adds parallel repo access from the same client.
- Files cache for fast change detection on subsequent backups.

### Configuration
- Environment variables (`BORG_REPO`, `BORG_PASSPHRASE`, etc.).
- No config file. Wrapper scripts and cron are the norm.
- Repository-level settings (encryption, compression defaults).

### Notable Innovations
- **O(1) pattern matching** -- can have thousands of exclude patterns with negligible performance impact. Most tools slow down linearly with pattern count.
- **FUSE mount** -- browse any archive as a filesystem.
- **Borg 2.0: rclone integration** -- native rclone backend enables 70+ cloud storage services.
- **Borg 2.0: parallel repo access** -- multiple `borg create` processes can write to the same repo concurrently.
- **Authenticated encryption** -- data integrity verified cryptographically, not just checksums.
- **`--exclude-if-present`** -- powerful directory-level exclusion based on marker files.

### Limitations
- Not a sync tool. Backup/restore only.
- Historically single-threaded (Borg 2.0 improves this).
- No native cloud support (Borg 1.x requires SSHFS/NFS; Borg 2.0 adds rclone backend).
- Python/Cython -- heavier than Go-based tools.
- Repository format not compatible between major versions (1.x vs 2.x).

---

## 10. Other Notable Tools

### duplicity
- **What**: Encrypted incremental backup using librsync + GPG + tar.
- **Model**: Full backup + incremental chain. Push-only.
- **Encryption**: GPG-based (symmetric or public key).
- **Backends**: SSH, FTP, S3, GCS, Azure, WebDAV, rclone, and many more.
- **Filtering**: `--include`/`--exclude` with shell globs, regex, and `ignorecase:` prefix. `--files-from` for explicit file lists.
- **Performance weakness**: Exclude pattern matching is O(n) per file -- 40 patterns add ~60 seconds to a 1700-file backup. Linear degradation.
- **Innovation**: GPG integration for zero-trust encrypted backups to untrusted storage.
- **Limitation**: Slow for large exclude lists. Incremental chains can become fragile. No deduplication.

### Mutagen
- **What**: Real-time bidirectional file sync for remote development. Written in Go.
- **Model**: Continuous daemon-based bidirectional sync. Designed for dev workflows (local IDE to remote server/container).
- **Modes**: `two-way-safe` (default, no data loss), `two-way-resolved` (alpha wins conflicts), `one-way-safe`, `one-way-replica` (exact mirror).
- **Conflict handling**: In `two-way-safe`, conflicts are detected and reported via `mutagen sync list`. In `two-way-resolved`, alpha side automatically wins.
- **Filtering**: Git-style ignore patterns (`.mutagen.yml` or `--ignore` flag). `--ignore-vcs` to skip VCS directories. Per-session and global ignores.
- **Performance**: rsync-like differential transfers, low-latency filesystem watching, very fast propagation of small changes.
- **Transports**: Local, SSH, Docker containers.
- **Innovation**: Purpose-built for container-based development. Multiple sync modes with clear data-loss semantics. YAML configuration via `.mutagen.yml`.
- **Limitation**: Not designed for cloud storage. Focused on dev workflow, not general-purpose sync.

### lsyncd (Live Syncing Daemon)
- **What**: Daemon that watches for filesystem changes (inotify/fsevents) and triggers rsync.
- **Model**: One-way, event-driven, near-real-time. Aggregates events over a configurable delay window, then spawns rsync.
- **Configuration**: Lua configuration file. Highly customizable.
- **Performance**: Only needs to be installed on source. Uses rsync for actual transfer (delta algorithm).
- **Modes**: `default.rsync` (local), `default.rsyncssh` (remote via SSH).
- **Innovation**: Event aggregation -- collates filesystem events for several seconds before syncing, avoiding thrashing. Lua scripting for custom behaviors.
- **Limitation**: One-way only. Linux/macOS only. Depends on rsync. No cloud support. No conflict handling.

### MinIO Client (mc)
- **What**: CLI for S3-compatible object storage. Written in Go.
- **Commands**: `mc ls`, `mc cp`, `mc mirror`, `mc diff`, `mc find`, `mc watch`, `mc admin`.
- **Sync**: `mc mirror` with `--watch` for continuous one-way sync. `--overwrite --remove` for exact mirroring.
- **Innovation**: Preserves custom metadata during copy (`-a` flag) -- something AWS CLI and rclone do not do. Real-time watch mode for mirror.
- **Limitation**: S3-compatible storage only. One-way mirror only.

### Duplicacy
- **What**: Cloud backup tool with lock-free deduplication. CLI is free; GUI is commercial.
- **Model**: Snapshot-based backup. Push-only.
- **Innovation**: **Lock-free deduplication** -- no locking required for concurrent backups from multiple machines to the same repository. Each chunk stored as an independent file named by its hash. Two-step fossil collection algorithm for safe garbage collection without locks.
- **Backends**: S3, GCS, Azure, Dropbox, B2, Google Drive, OneDrive, Hubic.
- **Limitation**: Backup only, not sync. CLI free for personal use; commercial license $50/machine/year.

---

## Cross-Tool Analysis

### What Is Table Stakes

These features appear in essentially every serious sync/backup CLI tool and should be considered mandatory:

| Feature | Prevalence | Notes |
|---------|-----------|-------|
| One-way sync/copy | Universal | Every tool supports source-to-destination transfer |
| Include/exclude filtering | Universal | Glob patterns at minimum |
| Dry-run/preview | Near-universal | `--dry-run` or `-n` flag |
| Delete at destination | Near-universal | `--delete` flag to mirror source |
| Timestamp-based comparison | Universal | mtime + size as change detection baseline |
| Checksum comparison option | Common | MD5, SHA-256, or tool-specific hash |
| Resume interrupted transfers | Common | Most tools handle partial transfers |
| SSH transport | Common | For remote sync tools |
| Progress reporting | Universal | Some form of progress indication |

### What Differentiates Good From Great

| Feature | Tools | Impact |
|---------|-------|--------|
| Content-defined chunking | restic, borg | Dramatically better dedup for modified files |
| Delta transfer (rsync algorithm) | rsync, unison, mutagen, lsyncd | Bandwidth efficiency for large file modifications |
| Client-side encryption | restic, borg, syncthing, duplicity | Zero-trust storage |
| FUSE mount for browsing | restic, borg | Intuitive restore UX |
| Bidirectional sync | unison, syncthing, mutagen, rclone bisync | The hard problem |
| Continuous/watch mode | syncthing, mutagen, lsyncd, unison | Near-real-time sync without cron |
| Job resume with plan files | azcopy | Robust recovery from failures |
| Server-side copy | azcopy, AWS CLI | Zero client bandwidth for cloud-to-cloud |
| Parallel transfers | azcopy, AWS CLI, syncthing | Throughput on high-bandwidth links |

### Conflict Handling Approaches

This is the most varied area across tools. Approaches observed:

| Approach | Tools | Description |
|----------|-------|-------------|
| No conflicts (one-way) | rsync, azcopy, AWS CLI, lsyncd, mc | Source always wins. No detection needed. |
| Rename conflicting file | syncthing, nextcloudcmd | Loser renamed with `.sync-conflict-<timestamp>` suffix. User resolves manually. |
| Interactive resolution | unison | Present each conflict to user. Accept/reject per file. |
| Configurable winner | mutagen, rclone bisync | Alpha wins, newer wins, larger wins, etc. |
| External merge tool | unison | Invoke diff3/custom script for content-level merge. |
| Safety copy + auto-resolve | unison (`copyonconflict`) | Keep backup, then auto-resolve. Best of both worlds. |
| Snapshot coexistence | restic, borg, duplicacy | No conflicts because snapshots are immutable. |

**Key insight**: No tool in the landscape does automatic content-level merge for binary files. Text file merge is only supported by unison (via external tools). Most tools either avoid the problem (one-way) or use simple heuristics (timestamp, rename).

### Filtering Capabilities Compared

| Feature | rsync | azcopy | AWS CLI | syncthing | unison | restic | borg |
|---------|-------|--------|---------|-----------|--------|--------|------|
| Glob patterns | Yes | Yes | Yes | Yes | Yes | Yes | Yes |
| Regex patterns | Via filter | `--exclude-regex` | No | No | No | No | Yes |
| Pattern file | Yes | No | No | `.stignore` | Via profile | `--exclude-file` | `--exclude-from` |
| Per-directory rules | `.rsync-filter` | No | No | `.stignore` | No | No | No |
| Negation | Yes | No | No | `!` prefix | `ignorenot` | `!` prefix | `!` prefix |
| Size-based | No | No | `--size-only` | No | No | `--exclude-larger-than` | No |
| Date-based | No | `--include-after` | No | No | No | No | No |
| Marker file exclusion | No | No | No | No | No | `--exclude-if-present` | `--exclude-if-present` |
| Case-insensitive | No | No | No | No | No | `--iexclude` | No |
| Include+exclude combined | Yes | Separate | Counter-intuitive | First-match | Separate | Exclude-only | `+`/`-` prefixes |

**Key insight**: Borg's O(1) hashtable-based pattern matching is a standout. Most tools degrade linearly with pattern count. For a sync engine that may have thousands of user-defined rules, pattern matching performance matters.

### Performance Features Compared

| Feature | rsync | azcopy | AWS CLI | syncthing | unison | restic | borg | mutagen |
|---------|-------|--------|---------|-----------|--------|--------|------|---------|
| Delta transfer | Yes | No | No | Block-level | Yes | No | No | Yes |
| Parallel transfers | Limited | Yes | Yes | Yes (peers) | No | Limited | No | No |
| Chunked upload | No | Yes | Auto | Yes | No | Yes | Yes | No |
| Compression | zstd/lz4/zlib | No | No | Optional | Yes | auto/off/max | lz4/zstd/zlib/lzma | No |
| Deduplication | No | No | No | Block-level | No | Content-defined | Content-defined | No |
| Bandwidth throttle | `--bwlimit` | `--cap-mbps` | No | Per-device | No | `--limit-upload/download` | No | No |
| Server-side copy | No | Yes | Yes | No | No | No | No | No |
| Filesystem watching | No | No | No | Yes | `-repeat watch` | No | No | Yes |

### Configuration Approaches

| Approach | Tools | Pros | Cons |
|----------|-------|------|------|
| CLI flags only | rsync, azcopy | Simple, scriptable | Verbose commands, no persistence |
| Environment variables | azcopy, restic, borg | CI/CD friendly | Not discoverable |
| Config file (INI/TOML) | AWS CLI | Persistent, human-readable | Another file to manage |
| Config file (XML) | syncthing | Rich structure | Verbose, hard to hand-edit |
| Config file (YAML) | mutagen | Modern, human-readable | Indentation-sensitive |
| Config file (Lua) | lsyncd | Programmable | Learning curve |
| Profile files | unison | Named configs, composable | Custom format |
| Auth token file | dbxcli | Automatic | Security concern if unprotected |

### What Is Innovative

1. **Unison's three-way merge** -- maintaining archive state to distinguish "changed on A" vs "changed on B" is the correct approach to bidirectional sync. Our sync engine should use this pattern (with SQLite instead of files).

2. **Syncthing's untrusted device encryption** -- storing encrypted data on untrusted peers without them being able to read or deduplicate it. XChaCha20-Poly1305 + AES-SIV.

3. **Borg's O(1) pattern matching** -- hashtable-based pattern matching that does not degrade with pattern count. Critical for large rule sets.

4. **azcopy's job management** -- plan files, resume, structured logs per job. Enterprise-grade reliability for long-running transfers.

5. **Mutagen's sync modes with clear data-loss semantics** -- four modes (`two-way-safe`, `two-way-resolved`, `one-way-safe`, `one-way-replica`) with explicit documentation of when data loss can occur.

6. **rclone bisync's conflict resolution options** -- configurable winner (newer, larger, path1, path2), configurable loser action (rename with suffix, delete, numbered copies). Most flexible conflict handling in the landscape.

7. **Content-defined chunking** (restic, borg) -- variable-size chunks via rolling hash mean insertions don't invalidate subsequent chunks. Superior to fixed-size blocks.

8. **lsyncd's event aggregation** -- collating filesystem events before syncing prevents thrashing from rapid changes.

9. **Duplicacy's lock-free deduplication** -- concurrent backups from multiple machines without locking. Elegant solution to the multi-writer problem.

### What Is Missing

Gaps in the current landscape that represent opportunities:

1. **No tool handles cloud API quirks gracefully** -- every cloud has API inconsistencies (truncated IDs, case normalization, server-side file modification). No tool documents or handles these systematically.

2. **No bidirectional cloud sync CLI with proper conflict handling** -- rclone bisync exists but is explicitly labeled experimental. No production-grade bidirectional CLI sync to cloud storage exists.

3. **Poor observability** -- most tools have primitive logging. No structured metrics, no dashboards, no alerting integration. azcopy's job logs are the closest to enterprise-grade.

4. **No selective/on-demand sync in CLI tools** -- virtual filesystem / placeholder files are only in GUI clients (OneDrive, Dropbox). No CLI tool offers this.

5. **No content-aware conflict resolution** -- no tool attempts to merge conflicts at the content level for common file types (text, JSON, etc.) without external tools.

6. **No incremental state tracking with database** -- most tools use flat files (timestamps, file lists) or the filesystem itself for state. SQLite-based state tracking is rare (rclone uses a simple listing file; Unison uses custom binary archives).

7. **Bandwidth-aware scheduling** -- no tool adapts transfer parallelism or scheduling based on available bandwidth. All use static configuration.

8. **Cross-platform filesystem normalization** -- Unicode normalization (NFC/NFD), case sensitivity handling, and path length limits are handled inconsistently or not at all.

---

## Implications for Our Sync Engine

Based on this survey, our OneDrive sync engine should:

1. **Use Unison's three-way merge approach** -- SQLite archive of previous synced state to detect changes on each side independently.

2. **Implement configurable conflict resolution** -- follow Mutagen's model of named modes with clear data-loss semantics (safe, alpha-wins, one-way-safe, mirror).

3. **Use content-defined chunking for uploads** -- leverage OneDrive's upload session API with variable-size chunks.

4. **Implement azcopy-style job management** -- plan files, resume capability, structured per-job logs.

5. **Use borg-style pattern matching** -- O(1) or near-O(1) filtering for large rule sets.

6. **Support filesystem watching** -- inotify/fsevents for near-real-time sync, with lsyncd-style event aggregation to avoid thrashing.

7. **Handle cloud API quirks as first-class concerns** -- our Tier 1 research documents these extensively; no other tool does this systematically.

8. **Provide clear sync modes** -- at minimum: bidirectional-safe, bidirectional-prefer-local, bidirectional-prefer-remote, one-way-up, one-way-down, mirror.

9. **Use SQLite for state** -- not flat files, not custom binary formats. Queryable, transactional, crash-safe.

10. **Implement proper observability** -- structured logging, sync statistics, configurable verbosity, machine-readable output for monitoring integration.
