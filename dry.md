# DRY Analysis: Redundant Validation in the Sync Pipeline

## Context

The sync engine validates file names, paths, and sizes at multiple layers.
This document maps every filtering layer, identifies true redundancies vs.
intentional complementary checks, and proposes a resolution aligned with
the engineering philosophy in CLAUDE.md.

## Complete Filter Inventory

### Layer 1 — Scanner (FullScan)

**Files:** `scanner.go:266,271`

During local filesystem walk, every entry is checked:
1. `isAlwaysExcluded(name)` — editor temps, partial downloads
2. `isValidOneDriveName(name)` — reserved names, invalid chars, length

**Failure mode:** Skip silently (`filepath.SkipDir` for dirs, continue for files). DEBUG log.

### Layer 2 — Watch Handlers (fsnotify)

**Files:** `observer_local_handlers.go:132,137,240`

Same two checks on every fsnotify event and when scanning contents of
newly created directories. **Not redundant with Layer 1** — Layer 1 runs
once at sync start; watch handlers process live events between scans.

### Layer 3 — Watch Setup

**File:** `observer_local.go:285`

Same checks during inotify watch registration. **Optimization layer** —
prevents registering watches on directories that will be filtered anyway.

### Layer 4 — Remote Observer

**File:** `observer_remote.go:398,405`

Same two checks on remote delta items. **Symmetry layer** — prevents
remote-only files (e.g., temp files uploaded via web UI) from entering
the planner. B-307 spec reference.

**Planned change (B-405):** Remove `isValidOneDriveName()` from this
layer. OneDrive enforces naming server-side; the client should not filter
downloads. `isAlwaysExcluded()` remains (OneDrive cannot enforce
client-side temp patterns).

### Layer 5 — Pre-Upload Validation

**File:** `upload_validation.go:80-110` (called from `engine.go:298,1180`)

`validateSingleUpload()` checks:
1. `isValidOneDriveName(path.Base(a.Path))` — **REDUNDANT** with Layer 1/2
2. `len(a.Path) > 400` — **UNIQUE** (only path-length check in codebase)
3. `a.View.Local.Size > 250 GB` — **UNIQUE** (only file-size check)

**Failure mode:** Remove action from plan, record `SyncFailure` (user-visible
via `issues list`). WARNING log.

### Layer 6 — Executor Delete Cleanup

**File:** `executor_delete.go:15-41`

`isDisposable()` calls `isAlwaysExcluded()` and `!isValidOneDriveName()`.
**Not a filter** — determines if files blocking a directory deletion can
be auto-removed. Different purpose entirely.

## Redundancy Assessment

| Check | Layer 1-4 (Observation) | Layer 5 (Upload Validation) | Redundant? |
|-------|------------------------|-----------------------------|------------|
| `isAlwaysExcluded()` | All 4 layers | **Not checked** | N/A (gap) |
| `isValidOneDriveName()` | All 4 layers | Checked (line 84) | **Yes** |
| Path length > 400 | Not checked | Checked (line 92) | No (unique) |
| File size > 250 GB | Not checked | Checked (line 101) | No (unique) |

### The Core Problem

`validateSingleUpload()` re-checks `isValidOneDriveName()` but does NOT
re-check `isAlwaysExcluded()`. This is **neither clean DRY nor consistent
defense-in-depth** — it is halfway between both approaches:

- If defense-in-depth is the intent, the omission of `isAlwaysExcluded()`
  is a gap.
- If DRY is the intent, the `isValidOneDriveName()` call is dead code
  (the scanner guarantees invalid names never become Actions).

## Contrast with Engineering Philosophy

| Principle | Says... | Implication |
|-----------|---------|-------------|
| "Extremely robust and full of defensive coding practices" | Keep redundant checks as safety nets | Supports defense-in-depth |
| "Prefer large, long-term solutions" | Design the right architecture | Suggests formalizing the layer contract |
| "Never settle for good enough for now" | The inconsistency is not acceptable | Fix it one way or the other |
| "No code is sacred" | If a better design exists, do it | Don't keep redundancy just because it exists |

The current state violates "never settle for good enough for now."

## Options

### Option A: Consistent Defense-in-Depth

Add `isAlwaysExcluded()` to `validateSingleUpload()` so the pre-execution
layer is a complete mirror of the observation layer. Accept the DRY
violation as a deliberate architectural choice. Document it.

- **Pro:** Every layer is self-sufficient. If someone refactors the scanner
  and forgets a check, the upload validator catches it. Failure mode is
  user-visible (`issues list`) rather than silent.
- **Con:** Two layers doing the same name check. Maintaining both requires
  discipline.
- **Effort:** Tiny — add one function call + tests.

### Option B: Trust the Pipeline (Strict DRY)

Remove `isValidOneDriveName()` from `validateSingleUpload()`. The
observation layer is the single source of truth for "what enters the sync
system." The upload validator only checks things the observation layer
*cannot* know: full path length and file size.

Also execute B-405: remove `isValidOneDriveName()` from the remote
observer.

- **Pro:** Clean separation of concerns. Each layer has a unique
  responsibility. No dead code.
- **Con:** If a bug in the scanner lets an invalid name through, it hits
  the Graph API and produces a cryptic 400 error instead of a clean
  `issues` entry.
- **Effort:** Small — remove one call, update tests, execute B-405.

### Option C: Formalize Two-Tier Validation (Recommended)

Define an explicit contract:

- **Tier 1 — Observation filter** (`shouldObserve(name) bool`): Fast,
  filename-only. Runs in scanner + all observers. Shared implementation,
  called from 4+ sites (already the case). Decides what enters the
  pipeline.
- **Tier 2 — Pre-execution validator** (`validateAction(action) []Issue`):
  Thorough, operates on full Action context (path, size, permissions).
  Catches structural issues impossible to detect at observation time.
  Returns structured issues for user visibility.

Changes:
1. Remove `isValidOneDriveName()` from `validateSingleUpload()` — Tier 1's
   job, not Tier 2's.
2. Document the two-tier contract explicitly in `sync-observation.md` and
   `sync-execution.md`.
3. Execute B-405: remove `isValidOneDriveName()` from remote observer.
4. Align with R-2.11.5 (`ScanResult{Events, Skipped}`) so Tier 1
   rejections become visible too.

- **Pro:** Clean architecture, explicit contract, each tier has unique
  responsibility, no dead code, no inconsistency. Aligns with "prefer
  large, long-term solutions."
- **Con:** Loses the safety net for invalid names at the pre-execution
  layer (acceptable if Tier 1 is well-tested, which it is — 30+ unit
  tests cover the observation filters).
- **Effort:** Medium — code changes are small, documentation is the main
  work.

### Option D: Single Filter Pipeline Object

Create a `SyncFilter` struct that owns all filtering rules. Each layer
calls `SyncFilter.Accept(entry)` which returns `(ok, *Rejection)`. The
filter is configured once and injected into scanner, observers, and
engine.

- **Pro:** Single source of truth for all filtering rules. Adding a new
  filter rule is a one-line change.
- **Con:** Significant refactor for what amounts to a few shared function
  calls. The current shared functions already achieve most of this without
  the abstraction overhead.
- **Effort:** Large — new type, dependency injection into 4+ components.

## Recommendation

**Option C** is the sweet spot. It resolves the inconsistency, respects
DRY, and does not over-engineer. The observation-layer filtering (Layers
1-4) is already well-structured — shared functions called from
appropriate sites. The only change is removing the redundant
`isValidOneDriveName()` from the upload validator and formalizing the
tier contract in documentation.

Option D is overkill — the current shared functions already serve as a
centralized filter implementation; wrapping them in a struct adds
abstraction without new capability.
