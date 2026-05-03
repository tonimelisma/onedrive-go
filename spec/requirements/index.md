# Requirements

Product vision: a fast, safe, well-tested CLI OneDrive client for Linux and macOS. Unix-style file operations plus robust bidirectional sync with conflict tracking. MIT licensed.

## Target Platforms

- **Linux** (primary): x86_64, ARM64. inotify for filesystem monitoring.
- **macOS** (primary): x86_64, ARM64 (Apple Silicon). FSEvents for filesystem monitoring.
- **Windows**: Explicit non-goal (Microsoft ships a native client).

## Non-Goals

1. Multi-cloud backends (OneDrive only)
2. GUI application (CLI tool; GUI frontends connect via control socket)
3. Client-side encryption (use rclone crypt, Cryptomator, or OS encryption)
4. Mobile platforms
5. Web UI / dashboard

## Capabilities

| ID | Capability | File | Status |
|----|------------|------|--------|
| R-1 | File Operations | [file-operations.md](file-operations.md) | implemented |
| R-2 | Sync | [sync.md](sync.md) | implemented (partial: RPC planned) |
| R-3 | Drive Management | [drive-management.md](drive-management.md) | implemented |
| R-4 | Configuration | [configuration.md](configuration.md) | implemented |
| R-5 | Transfers | [transfers.md](transfers.md) | implemented |
| R-6 | Non-Functional | [non-functional.md](non-functional.md) | partial |

## Status Legend

- `future` — not yet designed
- `planned` — vision only, no design doc addresses this yet
- `designed` — a design doc section specifies how it works
- `implemented` — code exists that implements the design
- `verified` — tests exist and pass that verify this requirement
- `cancelled` — requirement ID is retained for traceability, but the capability is no longer planned
- `target` — quantitative goal to be measured (performance benchmarks)

Status promotion rules:

- Promote to `designed` only when a governing design doc names the requirement
  in `Implements:` and describes the behavior, owner, or constraint.
- Promote to `implemented` only when production code implements the designed
  behavior and the governing design doc points to the owning package, file
  family, or runtime boundary.
- Promote to `verified` only when checked-in tests, a verifier rule, or
  live/E2E evidence validates the requirement, and the evidence is named in the
  governing design doc's `Verified By` table or adjacent evidence text.
