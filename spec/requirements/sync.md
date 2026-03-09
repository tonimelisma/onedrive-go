# R-2 Sync

Bidirectional file synchronization between a local directory and OneDrive.

## R-2.1 Sync Modes [implemented]

- R-2.1.1: When the user runs `sync`, the system shall perform one-shot bidirectional sync. [implemented]
- R-2.1.2: When `--watch` is passed, the system shall run continuously, detecting changes via filesystem events (inotify/FSEvents) and remote delta polling. [implemented]
- R-2.1.3: When `--download-only` is passed, the system shall only download remote changes. [implemented]
- R-2.1.4: When `--upload-only` is passed, the system shall only upload local changes. [implemented]
- R-2.1.5: When `--dry-run` is passed, the system shall preview operations without executing. [implemented]
- R-2.1.6: When `--full` is passed, the system shall perform full reconciliation (fresh delta enumeration + orphan detection). [implemented]

## R-2.2 Conflict Detection [implemented]

When the same file has been modified on both the local filesystem and OneDrive since the last successful sync, the system shall detect a conflict.

- R-2.2.1: The system shall use content hash comparison (QuickXorHash) against the baseline as the primary conflict signal. [implemented]
- R-2.2.2: The system shall use mtime as a fast-path optimization (skip hashing when timestamps match baseline). [implemented]

## R-2.3 Conflict Resolution [implemented]

- R-2.3.1: The default resolution shall preserve both versions: remote wins the original path, local version is renamed to `<name>.conflict-<timestamp>.<ext>`. [implemented]
- R-2.3.2: The system shall persistently record conflicts with metadata (path, timestamp, hashes, resolution status). [implemented]
- R-2.3.3: When the user runs `issues`, the system shall list all unresolved conflicts and failures. [implemented]
- R-2.3.4: When the user runs `issues resolve <path>`, the system shall allow resolution (keep-local, keep-remote, keep-both). [implemented]
- R-2.3.5: When the user runs `issues clear <path>`, the system shall dismiss a conflict. [implemented]
- R-2.3.6: When the user runs `issues retry <path>`, the system shall retry a failed item. [implemented]

## R-2.4 Filtering [implemented]

All filter settings are per-drive (no global filter defaults).

- R-2.4.1: When `skip_dotfiles = true`, the system shall exclude files and folders starting with `.`. [implemented]
- R-2.4.2: When `skip_dirs` is set, the system shall exclude matching directory names. [implemented]
- R-2.4.3: When `skip_files` is set, the system shall exclude matching file patterns. [implemented]
- R-2.4.4: When `max_file_size` is set, the system shall exclude files exceeding the limit. [implemented]
- R-2.4.5: When a directory contains a marker file (configurable name), the system shall exclude that directory. Whether the marker file supports gitignore-style patterns is TBD. [implemented]
- R-2.4.6: When `sync_paths` is set, the system shall sync only the specified paths. [planned]
- R-2.4.7: When `skip_symlinks = true`, the system shall exclude symlinks. Symlinked directories are always excluded from watch mode. [implemented]
- R-2.4.8: When an item belongs to the Personal Vault, the system shall exclude it by default. The `sync_vault` option enables vault sync for users who accept the auto-lock risk. [implemented]

## R-2.5 Crash Recovery [implemented]

- R-2.5.1: When the process is killed mid-sync, the next run shall resume cleanly from the last checkpoint. [implemented]
- R-2.5.2: The sync state store shall provide durable, transactional writes that survive process kill. [implemented]
- R-2.5.3: On startup, the system shall detect items stuck in `syncing` state and reset them for re-planning (reconciler). [implemented]

## R-2.6 Pause / Resume [implemented]

- R-2.6.1: When the user runs `pause [duration]`, the system shall stop data transfers while continuing to track changes. [implemented]
- R-2.6.2: When the user runs `resume`, the system shall resume transfers with a complete picture of accumulated changes. [implemented]

## R-2.7 Verification [implemented]

When the user runs `verify`, the system shall re-hash local files and compare against the baseline and remote state, reporting discrepancies.

## R-2.8 Watch Mode Behavior [implemented]

- R-2.8.1: The system shall reload `config.toml` on SIGHUP. [implemented]
- R-2.8.2: The system shall use a PID file with flock for single-instance enforcement. [implemented]
- R-2.8.3: The system shall support two-signal shutdown (first SIGINT = drain, second = force). [implemented]
- R-2.8.4: The system shall run periodic full reconciliation (default every 24 hours) to detect missed delta deletions. [implemented]
- R-2.8.5: The system shall support WebSocket subscription for near-instant remote change notification. [future]

## R-2.9 RPC / Control Socket [planned]

- R-2.9.1: When running `sync --watch`, the system shall expose a JSON-over-HTTP API on a Unix domain socket. [planned]
- R-2.9.2: The RPC API shall support polling (`GET /status`) and push (`GET /events` via SSE). [planned]
- R-2.9.3: GUI frontends shall connect to the control socket for real-time status, pause/resume, and conflict resolution. [planned]
