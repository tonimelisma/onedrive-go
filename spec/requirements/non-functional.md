# R-6 Non-Functional

Performance targets, data integrity, process model, safety, observability, and packaging.

## R-6.1 Performance Targets [planned]

- R-6.1.1: Memory usage shall stay below 100 MB for 100K synced files. [planned]
- R-6.1.2: CPU usage in idle watch mode shall stay below 1%. [planned]
- R-6.1.3: Initial sync of 10K files shall complete in under 10 minutes. [planned]
- R-6.1.4: Startup time shall be under 1 second. [implemented]
- R-6.1.5: Binary size shall be under 20 MB (single static binary, no runtime dependencies). [implemented]

## R-6.2 Data Integrity [implemented]

- R-6.2.1: The system shall never silently lose or corrupt user data. [implemented]
- R-6.2.2: File operations (ls, get, put, rm, mkdir, stat, mv, cp) shall work independently of sync state — no sync database involved. [implemented]
- R-6.2.3: The system shall support configurable file permissions (`sync_file_permissions`) and directory permissions (`sync_dir_permissions`) for synced content. [implemented]

## R-6.3 Process Model [implemented]

- R-6.3.1: Only one sync process per configuration shall run at a time. [implemented]
- R-6.3.2: Status and query commands shall be concurrent-reader safe while sync is running. [implemented]
- R-6.3.3: The system shall enforce single-instance via PID file with advisory lock. [implemented]

## R-6.4 Safety [implemented]

- R-6.4.1: When a sync would delete more items than `big_delete_threshold` (default: 1000), the system shall abort and require `--force`. [implemented]
- R-6.4.2: When a sync would delete more than `big_delete_percentage` (default: 50%) of baseline items, the system shall abort. [implemented]
- R-6.4.3: Big-delete protection shall apply both globally and per-folder. [implemented]
- R-6.4.4: Remote deletions shall go to the OneDrive recycle bin by default (`use_recycle_bin`). [implemented]
- R-6.4.5: Local deletions triggered by remote changes shall go to OS trash on macOS (`use_local_trash`). [implemented]
- R-6.4.6: On Linux, local trash shall be opt-in (default off; servers/NAS typically lack XDG trash). [implemented]
- R-6.4.7: The system shall support configurable disk space reservation (`min_free_space`). [planned]

## R-6.5 Crash Recovery [implemented]

- R-6.5.1: The sync state store shall provide durable, transactional writes that survive process kill. [implemented]
- R-6.5.2: Every sync operation shall be atomic — incomplete operations shall not corrupt state. [implemented]
- R-6.5.3: On startup, the system shall detect items stuck in `syncing` state and reset them. [implemented]

## R-6.6 Observability [implemented]

- R-6.6.1: The system shall support dual-channel logging: console (stderr) and log file. [implemented]
- R-6.6.2: Console verbosity shall be controlled by `--quiet`, `--verbose`, and `--debug` flags. [implemented]
- R-6.6.3: Log file level shall be controlled independently by `log_level` in config. [implemented]
- R-6.6.4: The log file shall use structured JSON format. [implemented]
- R-6.6.5: The system shall support progress bars and color-coded transfer output. [future]
- R-6.6.6: The system shall support a TUI (terminal UI) for real-time status. [future]

## R-6.7 Technical Requirements [implemented]

Constraints derived from the OneDrive API that the system must satisfy for correctness. See [graph-api-quirks.md](../reference/graph-api-quirks.md) for the underlying API behaviors.

- R-6.7.1: The system shall handle delta operation reordering (deletions arriving after creations at the same path). [implemented]
- R-6.7.2: The system shall normalize driveId values across all endpoints to a canonical form. [implemented]
- R-6.7.3: The system shall track items by ID and reconstruct paths from parent chains. [implemented]
- R-6.7.4: The system shall detect server-side file modification after upload (SharePoint enrichment) and not re-upload. [implemented]
- R-6.7.5: The system shall handle HTTP 410 (delta token expiry) with full re-enumeration. [implemented]
- R-6.7.6: The system shall enforce upload chunk alignment to 320 KiB boundaries. [implemented]
- R-6.7.7: The system shall not compare hashes for deleted items. [implemented]

## R-6.8 Network Resilience [implemented]

- R-6.8.1: The system shall respect 429 (Too Many Requests) with Retry-After headers. [implemented]
- R-6.8.2: The system shall use exponential backoff with jitter for transient failures. [implemented]
- R-6.8.3: All transfers shall be resumable after network interruption. [implemented]

## R-6.9 Packaging [future]

- R-6.9.1: The system shall be distributable as a single static Go binary. [implemented]
- R-6.9.2: The system shall provide a Homebrew formula (macOS). [future]
- R-6.9.3: The system shall provide deb/rpm packages. [future]
- R-6.9.4: The system shall provide an AUR package. [future]
- R-6.9.5: The system shall provide a Docker image (Alpine-based, multi-arch). [future]
