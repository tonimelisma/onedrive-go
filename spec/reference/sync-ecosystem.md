# Sync Ecosystem

Key design patterns observed across file sync tools. For tool-specific documentation, consult each project directly.

## State Model Patterns

- **Path-based identity** (rclone bisync, rsync): Simple but breaks on rename/move — detected as delete + create. No move optimization possible.
- **ID-based identity** (Syncthing, Dropbox, this project): Stable across renames. Requires persistent ID-to-path mapping. OneDrive's delta API returns item IDs, making this the natural choice.
- **Three-way merge** (Syncthing, this project): Baseline (last known synced state) + local state + remote state. The baseline enables distinguishing "both sides changed" from "one side changed." Without a baseline, all differences are ambiguous.

## IPC Patterns for Daemon Control

- **Subprocess + stdout parsing** (OneDriveGUI → abraunegg/onedrive): Fragile, version-coupled, no structured queries. Avoid.
- **HTTP REST API** (rclone rc, Syncthing): Clean, language-agnostic, debuggable with curl. Both use localhost-only binding with token auth.
- **Unix domain socket** (systemd, Docker): Lower overhead than TCP, implicit auth via filesystem permissions. Natural fit for single-machine daemon.

## Conflict Resolution Patterns

- **Last-write-wins** (azcopy, rclone sync): No conflict detection. Timestamp comparison only. Data loss on simultaneous edits.
- **Rename-aside** (abraunegg/onedrive, Dropbox, this project): Preserve both versions by renaming the losing side. Safe but accumulates conflict copies.
- **Syncthing's approach**: Marks conflicts explicitly, lets user resolve. No automatic resolution. Most conservative.

## Upload Resume Patterns

- **Session-based resume** (OneDrive upload sessions, this project): Server provides a session URL with embedded auth. Client tracks byte offset. Sessions expire (~days).
- **Content-defined chunking** (restic, attic): Deduplication-friendly but not applicable to OneDrive's API (which requires sequential byte ranges).

## Lessons from Competitor Limitations

- rclone bisync has no persistent state across runs in its default mode — crashes can cause data loss. Persistent state (baseline) is essential.
- rsync has no delete propagation by default. Explicit `--delete` flag prevents accidental mass deletion. Big-delete threshold is a critical safety feature.
- abraunegg/onedrive's D-language curl dependency caused an entire class of platform-specific HTTP bugs. Using Go's `net/http` eliminates this.
