# Documentation Architecture Migration

## Goal

Replace the current 45+ documentation files with a clean, normative documentation architecture of 28 files. The new docs describe the product as it **should** be — a coherent vision written from a blank slate. Any gap between vision and reality is tracked in requirements as a requirement with the appropriate status.

## Design Principles

1. **Normative, not descriptive.** Target docs describe the product vision, not its history. Written as if from a blank slate.
2. **No backwards compatibility.** The current documentation structure has zero influence on the target structure. We do not preserve names, locations, groupings, or conventions from the old docs. If migration cost shows up as a factor in any decision, that decision is wrong.
3. **One direction of causality.** Reference informs requirements, requirements inform design, design governs code and tests. Never backwards. When an agent discovers something during implementation (e.g., a new API bug), it files a BACKLOG item pointing to the appropriate upstream doc. A human routes the fix through the causal chain.
4. **Every fact lives in exactly one place.** No redundancy between docs.
5. **Optimized for agent context windows.** Docs are small, focused, and non-overlapping. An agent modifying one module reads exactly one design doc. The routing table in CLAUDE.md tells it which one. Agent performance degrades with instruction count — keep docs focused and CLAUDE.md under 150 lines.
6. **Traceability at every level.** Every design section traces to requirement IDs. Every requirement has a status. Design docs mirror requirement statuses. Gaps are visible, not hidden.
7. **Self-verification.** After implementing, agents produce a detailed report mapping each governing spec item to what was done, whether it was implemented in full, and any deviations.

## Causal Model

```
Reference > Requirements > Design (system > module) > Code + Tests
```

One direction. Downstream only. When you discover a new API bug in CI, it enters the system as **reference**, which may cause a **requirement** change, which flows into a **design** change, which changes **code** and **tests**.

- **Reference**: facts about the external world (API behavior, known bugs, competitors). No IDs — these are facts, not tracked items.
- **Requirements**: normative user-facing product vision. Hierarchically numbered. Each acceptance criterion has a status. EARS format.
- **Design**: normative technical vision. Each section traces to requirement IDs it fulfills and mirrors their status. Rationale sections explain key choices. Technical necessities (e.g., "normalize driveId casing") live here, not in requirements — they're implementation details derived from reference material.
- **Code + Tests**: implement and verify design.

## Target Architecture

All new docs live in `spec/` — a clean directory with no overlap with the old `docs/`.

```
spec/
  reference/
    graph-api-surface.md           # API endpoints, fields, authentication
    graph-api-quirks.md            # All bugs, inconsistencies, workarounds
    onedrive-sync-behavior.md      # Delta semantics, conflict behavior, edge cases
    onedrive-filtering.md          # External filtering behavior
    sync-ecosystem.md              # Competitors, IPC patterns, state models
    onedrive-glossary.md           # Domain vocabulary

  requirements/
    index.md                       # Capability list with summary and links
    file-operations.md             # R-1: ls, get, put, rm, mkdir, stat, cp, mv, recycle-bin
    sync.md                        # R-2: sync modes, watch, conflicts, filtering, crash recovery, pause/resume
    drive-management.md            # R-3: login, logout, whoami, drive types, multi-account, shared drives
    configuration.md               # R-4: TOML config, hot-reload, setup command, service install
    transfers.md                   # R-5: parallel transfers, resume, bandwidth, chunking
    non-functional.md              # R-6: performance targets, safety, observability, API quirks, migration

  design/
    system.md                      # Package structure, data flow, event-driven rationale
    data-model.md                  # Schema, state machines, concurrency
    graph-client.md                # GOVERNS: internal/graph/ + internal/tokenfile/
    config.md                      # GOVERNS: internal/config/
    drive-identity.md              # GOVERNS: internal/driveid/ + drive.go
    drive-transfers.md             # GOVERNS: internal/driveops/ + pkg/quickxorhash/ + get.go, put.go
    retry.md                       # GOVERNS: internal/retry/
    sync-observation.md            # GOVERNS: sync observers, scanner, buffer, shortcuts, permissions, inotify
    sync-planning.md               # GOVERNS: sync planner, types
    sync-execution.md              # GOVERNS: sync executor, worker, tracker, reconciler + status.go
    sync-engine.md                 # GOVERNS: sync engine, orchestrator, drive_runner + sync.go, sync_helpers.go
    sync-store.md                  # GOVERNS: sync baseline, store_interfaces, migrations, verify, trash + issues.go, verify.go
    cli.md                         # GOVERNS: root package CLI infrastructure + internal/logfile/

CLAUDE.md                          # Process (~150 lines) + routing table
BACKLOG.md                         # Restructured bugs and tasks
```

After migration, the old `docs/` directory, `LEARNINGS.md`, and `MIGRATION.md` are deleted. Only `spec/`, `CLAUDE.md`, and `BACKLOG.md` remain.

### Complete GOVERNS Mapping

Every production `.go` file (99 total) is assigned to exactly one design doc. Test files, `e2e/`, and `testutil/` are exempt.

**system.md** — no GOVERNS (system-level, not module-level)

**data-model.md** — no GOVERNS (schema specification, not code)

**graph-client.md**
```
internal/graph/auth.go, internal/graph/client.go, internal/graph/delta.go,
internal/graph/download.go, internal/graph/drives.go, internal/graph/errors.go,
internal/graph/items.go, internal/graph/normalize.go, internal/graph/types.go,
internal/graph/upload.go, internal/tokenfile/tokenfile.go
```

**config.md**
```
internal/config/account.go, internal/config/config.go, internal/config/defaults.go,
internal/config/display_name.go, internal/config/drive.go, internal/config/drivemeta.go,
internal/config/env.go, internal/config/holder.go, internal/config/load.go,
internal/config/paths.go, internal/config/size.go, internal/config/token_resolution.go,
internal/config/toml_lines.go, internal/config/unknown.go, internal/config/validate.go,
internal/config/validate_drive.go, internal/config/write.go
```

**drive-identity.md**
```
internal/driveid/canonical.go, internal/driveid/id.go, internal/driveid/itemkey.go,
drive.go
```

**drive-transfers.md**
```
internal/driveops/cleanup.go, internal/driveops/doc.go, internal/driveops/hash.go,
internal/driveops/interfaces.go, internal/driveops/session.go,
internal/driveops/session_store.go, internal/driveops/stale_partials.go,
internal/driveops/transfer_manager.go, pkg/quickxorhash/quickxorhash.go,
get.go, put.go
```

**retry.md**
```
internal/retry/backoff.go, internal/retry/circuit.go, internal/retry/doc.go,
internal/retry/named.go, internal/retry/policy.go
```

**sync-observation.md**
```
internal/sync/observer_local.go, internal/sync/observer_local_handlers.go,
internal/sync/observer_remote.go, internal/sync/observer_shortcut.go,
internal/sync/scanner.go, internal/sync/buffer.go, internal/sync/shortcuts.go,
internal/sync/permissions.go, internal/sync/inotify_linux.go,
internal/sync/inotify_other.go
```

**sync-planning.md**
```
internal/sync/planner.go, internal/sync/types.go
```

**sync-execution.md**
```
internal/sync/executor.go, internal/sync/executor_conflict.go,
internal/sync/executor_delete.go, internal/sync/executor_transfer.go,
internal/sync/worker.go, internal/sync/tracker.go, internal/sync/reconciler.go,
internal/sync/upload_validation.go, internal/sync/compute_status.go,
status.go
```

**sync-engine.md**
```
internal/sync/engine.go, internal/sync/engine_shortcuts.go,
internal/sync/orchestrator.go, internal/sync/drive_runner.go,
sync.go, sync_helpers.go
```

**sync-store.md**
```
internal/sync/baseline.go, internal/sync/store_interfaces.go,
internal/sync/migrations.go, internal/sync/verify.go, internal/sync/trash.go,
issues.go, verify.go
```

**cli.md**
```
main.go, root.go, format.go, signal.go, pidfile.go, auth.go,
ls.go, rm.go, mkdir.go, mv.go, cp.go, stat.go, pause.go, resume.go,
recycle_bin.go, internal/logfile/logfile.go
```

**Exempt** (test infrastructure, not production code):
```
e2e/*.go, testutil/testenv.go
```

## Numbering and Traceability

### Requirement IDs

Three levels max: `R-{capability}.{feature}.{acceptance criterion}`

```
R-2       Capability: Sync
R-2.1     Feature: Bidirectional sync
R-2.1.1   AC: When a file exists in remote delta but not locally and not in baseline,
               the system shall download it to the local sync directory. [verified]
R-2.1.2   AC: When a file exists locally but not in remote delta and not in baseline,
               the system shall upload it. [verified]
```

Deeper nesting (e.g., Sync > Watch Mode > Local Observer > inotify) is flattened to 3 levels. "inotify limit detection" becomes an AC under the Watch Mode feature.

### Acceptance Criteria Format (EARS)

All acceptance criteria use EARS format: **"When [trigger/condition], the system shall [behavior]."**

This format is unambiguous, script-parseable, and maps directly to test assertions.

### Requirement Statuses

Each acceptance criterion (leaf-level requirement) has exactly one status:

| Status | Meaning |
|--------|---------|
| `planned` | Vision only. No design doc addresses this yet. |
| `designed` | A design doc section references this requirement ID and specifies how it works. |
| `implemented` | Code exists that implements the design. |
| `verified` | Tests exist and pass that verify this requirement. |

Statuses are tracked inline in requirements files next to each acceptance criterion. During migration, statuses are assigned based on current reality (most existing features jump straight to `implemented` or `verified`), not historical progression.

### Design Doc Status Mirroring

Design doc sections mirror the requirement statuses they implement:

```markdown
## Conflict Detection

Implements: R-2.1.3 [verified], R-2.1.4 [implemented]

When both remote and local have changes and baseline exists, the planner
compares hashes. If both differ from baseline, the planner emits ActionConflict.
```

This means an agent reading a design doc sees which parts are real and which are aspirational, without switching to requirements files.

### Traceability Chain

```
requirements/sync.md         spec/design/sync-planning.md        planner.go            planner_test.go
R-2.1.3 [verified]   --->   "Implements R-2.1.3 [verified]"  -->  (code)           -->  TestPlanner_EditEdit
```

- **Requirements → Design**: Design doc sections state which requirement IDs they implement and mirror the status.
- **Design → Code**: The `GOVERNS:` header maps design docs to code files.
- **Design → Tests**: Test names or comments reference the requirement or design section they verify.

To verify traceability:
- "Is R-2.1.3 designed?" → search design docs for `R-2.1.3`
- "Is R-2.1.3 implemented?" → check status in requirements and design docs
- "Is R-2.1.3 tested?" → find the test via the design doc's GOVERNS files

### Ongoing Traceability Enforcement

Add a CI check (or quality gate script) that verifies:
1. Every `.go` source file (non-test) appears in exactly one `GOVERNS:` list
2. Every requirement with status `designed`+ is referenced by at least one design doc section
3. Every `Implements:` line in a design doc references a valid requirement ID
4. Statuses in design docs match statuses in requirements files

## Document Templates

### Reference doc template

```markdown
# {Title}

{Purpose: what external knowledge this doc captures and why it matters.}

## {Topic}

{Facts. Present tense. No opinions, no design decisions.
Cite sources where possible (MS docs URL, CI run ID, observed behavior).
Workarounds may be stated as observed facts ("normalizing to 16 characters
resolves the truncation") but the decision to USE a workaround belongs
in the design doc.}
```

No IDs, no statuses. These are facts about the world. They change when the world changes.

### Requirements index template

```markdown
# Requirements

| Capability | File | ID Range |
|------------|------|----------|
| File Operations | [file-operations.md](file-operations.md) | R-1.* |
| Sync | [sync.md](sync.md) | R-2.* |
| Drive Management | [drive-management.md](drive-management.md) | R-3.* |
| Configuration | [configuration.md](configuration.md) | R-4.* |
| Transfers | [transfers.md](transfers.md) | R-5.* |
| Non-Functional | [non-functional.md](non-functional.md) | R-6.* |
```

### Requirements file template

```markdown
# R-{N} {Capability Name}

{One-sentence description of this capability.}

## R-{N}.1 {Feature Name}

{One-sentence description of this feature.}

- R-{N}.1.1: When {trigger/condition}, the system shall {behavior}. `[status]`
- R-{N}.1.2: When {trigger/condition}, the system shall {behavior}. `[status]`

## R-{N}.2 {Feature Name}

- R-{N}.2.1: When {trigger/condition}, the system shall {behavior}. `[status]`
```

Rules:
- Acceptance criteria are user-visible behaviors in EARS format.
- Technical necessities (e.g., "normalize driveId casing") are NOT requirements. They belong in design docs.
- Status is one of: `planned`, `designed`, `implemented`, `verified`.

### System design doc template

```markdown
# {Title}

{Purpose: what this doc defines at the system level.}

## {Section}

Implements: R-1.2.1 [verified], R-3.4.2 [implemented]

{Normative description. Present tense. "The system does X."}

### Rationale

{Why this approach was chosen over alternatives. Stable once written.}
```

### Module design doc template

```markdown
# {Module Name}

GOVERNS: {comma-separated list of Go source files (not test files)}

{Purpose: what this module does, in one paragraph.}

## Interface

Implements: R-1.2.1 [verified], R-2.1.3 [verified]

{Exported types, functions, and their contracts.
"Function X MUST do Y when Z."}

## Behavior

### {Behavior area}

Implements: R-2.3.1 [implemented]

{Detailed behavioral specification. State machines, decision matrices, algorithms.
"When the input is X and the state is Y, the output MUST be Z."}

### Rationale

{Why key design choices were made. Stable once written.}

## Constraints

{Hard platform limits (inotify watch count), API rate limits, known failure
modes the code must handle. NOT general tips or war stories.}
```

Rules:
- Every section that specifies behavior MUST list `Implements:` with requirement IDs and mirrored statuses.
- The `GOVERNS:` header lists every Go source file (not test files) this doc is authoritative for.
- Every Go source file in the repo appears in exactly one `GOVERNS:` list across all module design docs.
- Unimplemented sections are visible by their status markers (e.g., `R-2.5.1 [planned]`). An agent seeing `[planned]` knows not to depend on that feature existing.

### CLAUDE.md structure

```markdown
# onedrive-go

{One-paragraph project summary.}

## Routing Table

| When modifying... | Read first | Also consult |
|-------------------|-----------|--------------|
| `internal/graph/` | `spec/design/graph-client.md` | `spec/reference/graph-api-surface.md`, `spec/reference/graph-api-quirks.md` |
| `internal/sync/planner.go` | `spec/design/sync-planning.md` | `spec/reference/onedrive-sync-behavior.md` |
| ... | ... | ... |

| Working on capability... | Requirements file | Design docs |
|--------------------------|-------------------|-------------|
| R-1 File Operations | `spec/requirements/file-operations.md` | `spec/design/cli.md`, `spec/design/drive-transfers.md` |
| R-2 Sync | `spec/requirements/sync.md` | `spec/design/sync-*.md`, `spec/design/data-model.md` |
| ... | ... | ... |

## Process

{TDD workflow, quality gates, self-verification step, DoD.
Only rules the linter/CI can't enforce. ~150 lines total for entire file.}
```

The routing table has two dimensions:
- **By code path**: "modifying X? read Y" — for implementation work
- **By capability**: "working on R-2? read these" — for feature work

## CLAUDE.md Size Constraint

Target: ~150 lines. Achieve this by:
- Removing all conventions enforced by golangci-lint (line length, import ordering, formatting)
- Removing the architecture overview (now in `spec/design/system.md`)
- Removing the documentation index (now implicit in `spec/` directory structure)
- Keeping only: project summary, routing table, process workflow, quality gates, and rules an agent must know BEFORE writing code (e.g., "use testify not stdlib assertions", "comments explain why not what")

## Self-Verification Step

After implementing any change, the agent must re-read the governing design doc and produce a compliance report:

```
Spec Compliance Report:
- R-2.1.3 (conflict detection): FULLY IMPLEMENTED — planner emits ActionConflict
  when both hashes differ from baseline. Test: TestPlanner_EditEditConflict.
- R-2.1.4 (conflict resolution): PARTIALLY IMPLEMENTED — keep-both works,
  keep-local not yet wired. Filed BACKLOG item B-xxx.
- sync-planning.md §Filtering: NO CHANGE — not affected by this PR.
```

This is added to the development process in CLAUDE.md.

## Progress

| Phase | Status |
|-------|--------|
| Phase 0: Create structure and skeleton | **done** |
| Phase 1: Reference sources | **done** |
| Phase 2: Requirements sources | **done** |
| Phase 3: System design sources | **done** |
| Phase 4: Module design sources | **done** |
| Phase 5: Dissolve remaining files | pending |
| Phase 6: Finalize | pending |

## Migration Process

One PR for the entire migration. Process source docs upstream-first (reference → requirements → design). For each source doc: read it fully, distribute knowledge into appropriate target docs (reference facts → reference, requirements → requirements, design details → design), then mark it absorbed.

Source docs are NOT deleted during processing — they are marked with `<!-- ABSORBED -->` at the top. All deletions happen in Phase 6. This provides a rollback path if the migration goes wrong partway through.

### Phase 0: Create structure and skeleton

1. Create `spec/reference/`, `spec/requirements/`, `spec/design/` directories
2. Create every target doc as a placeholder with its template header (title, purpose statement, empty sections)
3. Create the requirements capability skeleton in `spec/requirements/index.md`:
   - R-1 File Operations
   - R-2 Sync
   - R-3 Drive Management
   - R-4 Configuration
   - R-5 Transfers
   - R-6 Non-Functional
4. Pre-read `LEARNINGS.md` and tag each entry with its target doc (reference, which design doc, or CLAUDE.md). This avoids needing to patch design docs later when LEARNINGS.md is processed in Phase 5.
5. Commit: `docs: create spec/ placeholder structure`

### Phase 1: Reference sources

Process into `spec/reference/`. Within this phase, steps targeting different files can run in parallel. Steps targeting the same file must run sequentially.

When a source doc contains requirements or design content alongside reference facts, split: reference facts → reference doc, requirements → requirements file, design details → design doc placeholder.

| Step | Source | Primary target | Split to |
|------|--------|----------------|----------|
| 1.1 | `docs/tier1-research/domain-glossary.md` | `onedrive-glossary.md` | |
| 1.2 | `docs/tier1-research/api-analysis.md` | `graph-api-surface.md` | requirements → `requirements/` |
| 1.3 | `docs/tier1-research/api-item-field-matrix.md` | `graph-api-surface.md` | |
| 1.4 | `docs/tier1-research/issues-graph-api-bugs.md` | `graph-api-quirks.md` | |
| 1.5 | `docs/tier1-research/issues-api-inconsistencies.md` | `graph-api-quirks.md` | |
| 1.6 | `docs/tier1-research/issues-common-bugs.md` | `graph-api-quirks.md` | design lessons → design placeholders |
| 1.7 | `docs/design/ci_issues.md` | `graph-api-quirks.md` | constraints → design placeholders |
| 1.8 | `docs/tier1-research/ref-sync-algorithm.md` | `onedrive-sync-behavior.md` | |
| 1.9 | `docs/tier1-research/ref-conflict-scenarios.md` | `onedrive-sync-behavior.md` | |
| 1.10 | `docs/tier1-research/ref-edge-cases.md` | `onedrive-sync-behavior.md` | |
| 1.11 | `docs/tier1-research/ref-filtering-rules.md` | `onedrive-filtering.md` | |
| 1.12 | `docs/tier1-research/ref-config-inventory.md` | `onedrive-filtering.md` | |
| 1.13 | `docs/tier1-research/survey-sync-cli-tools.md` | `sync-ecosystem.md` | |
| 1.14 | `docs/tier1-research/survey-gui-ipc-patterns.md` | `sync-ecosystem.md` | |
| 1.15 | `docs/tier1-research/survey-sync-state-models.md` | `sync-ecosystem.md` | |
| 1.16 | `docs/tier1-research/issues-feature-requests.md` | `requirements/` | feature requests → requirements with status |
| 1.17 | `docs/tier1-research/README.md` | delete | index file, no content |

**End-of-phase check**: read each reference doc written in this phase. Verify it's coherent, non-redundant, and contains only facts (no design decisions, no opinions).

### Phase 2: Requirements sources

Process into `spec/requirements/`. When source docs contain design or implementation details, split those into design doc placeholders.

| Step | Source | Primary target | Split to |
|------|--------|----------------|----------|
| 2.1 | `docs/design/prd.md` | `requirements/*` | CLI details → `design/cli.md`, implementation details → relevant design docs |
| 2.2 | `docs/roadmap.md` | `requirements/*` | status markers for each requirement |
| 2.3 | `docs/design/observability.md` | `requirements/non-functional.md` | design content → `design/sync-engine.md` |

**End-of-phase check**: read `requirements/index.md` and each requirements file. Verify every AC uses EARS format, has a status, and contains no implementation details.

### Phase 3: System design sources

Process into `spec/design/system.md` and `spec/design/data-model.md`.

| Step | Source | Primary target | Split to |
|------|--------|----------------|----------|
| 3.1 | `docs/design/architecture.md` | `design/system.md` | verify package structure against actual code |
| 3.2 | `docs/design/event-driven-rationale.md` | `design/system.md` | rationale section; strip historical narration |
| 3.3 | `docs/design/data-model.md` | `design/data-model.md` | verify schema against actual migration SQL |
| 3.4 | `docs/design/remote-state-separation.md` | `design/data-model.md` + `design/sync-store.md` | schema → data-model, interfaces → sync-store |
| 3.5 | `docs/design/decisions.md` | rationale sections in relevant design docs | each decision → rationale section where it belongs |

**End-of-phase check**: verify system.md package structure matches actual code. Verify data-model.md schema matches actual migration SQL.

### Phase 4: Module design sources

Process into `spec/design/*.md` module docs. Each source doc may contribute to multiple targets — split by content type. Incorporate tagged LEARNINGS.md entries from Phase 0 pre-read.

| Step | Source | Primary target | Split to |
|------|--------|----------------|----------|
| 4.1 | `docs/design/configuration.md` | `design/config.md` | |
| 4.2 | `docs/design/accounts.md` | `design/drive-identity.md` | |
| 4.3 | `docs/design/MULTIDRIVE.md` | `design/drive-identity.md` | consolidate with 4.2, no redundancy |
| 4.4 | `docs/design/driveops.md` | `design/drive-transfers.md` | |
| 4.5 | `docs/design/sharepoint-enrichment.md` | `design/drive-transfers.md` | rationale section for per-side hashes |
| 4.6 | `docs/design/retry-architecture.md` | `design/retry.md` | |
| 4.7 | `docs/design/retry-transition-requirements.md` | `design/retry.md` | merge with 4.6; requirements → `requirements/` |
| 4.8 | `docs/design/failures.md` | `design/retry.md` + `design/sync-execution.md` | API facts → `reference/graph-api-quirks.md` |
| 4.9 | `docs/design/sync-algorithm.md` | `design/sync-observation.md` + `design/sync-planning.md` + `design/sync-execution.md` + `design/sync-engine.md` | largest split — each section to the module that governs that code |
| 4.10 | `docs/design/concurrent-execution.md` | `design/sync-execution.md` | |
| 4.11 | `docs/design/filtering-conflicts.md` | `design/sync-planning.md` | unresolved items → `requirements/` with status |
| 4.12 | `docs/design/test-strategy.md` | `CLAUDE.md` scratch notes | process and conventions only |
| 4.13 | `docs/design/E2E.md` | `CLAUDE.md` scratch notes | E2E infrastructure, test isolation |
| 4.14 | `docs/design/clock-skew-audit.md` | relevant design docs | findings → constraints sections |

**End-of-phase check**: for each module design doc, verify the `GOVERNS:` file list matches actual files in the codebase. Verify every `Implements:` line references a valid requirement ID.

### Phase 5: Dissolve remaining files

| Step | Source | Target | Notes |
|------|--------|--------|-------|
| 5.1 | `LEARNINGS.md` | distribute per Phase 0 tagging | API facts → reference. Design constraints → module doc constraints sections. Process conventions → CLAUDE.md. |
| 5.2 | `docs/design/cleanup.md` | delete | personal notes |
| 5.3 | `docs/design/legacy-sequential-architecture.md` | delete | historical only |
| 5.4 | `docs/archive/backlog-v1.md` | delete | |
| 5.5 | `docs/archive/learnings-phases-1-3.md` | delete | |
| 5.6 | `docs/archive/migration-v1.md` | delete | |
| 5.7 | `docs/archive/prerelease_review.md` | `BACKLOG.md` | unaddressed audit items → backlog entries |
| 5.8 | Restructure `BACKLOG.md` | `BACKLOG.md` | remove closed items, update doc references to `spec/` paths, remove old phase numbers, align with requirement IDs where applicable |

### Phase 6: Finalize

| Step | Task |
|------|------|
| 6.1 | Write `CLAUDE.md`: project summary + routing table (both dimensions) + process + self-verification step. Target ~150 lines. Strip all linter-enforced conventions. |
| 6.2 | Update `.worktreeinclude` if it references `docs/` paths. |
| 6.3 | Search codebase for references to old doc paths (comments, scripts) and update to `spec/` paths. |
| 6.4 | Update auto-memory `MEMORY.md` to reflect new doc paths and conventions. |
| 6.5 | Delete all source docs (remove `<!-- ABSORBED -->` files), `docs/` directory, `LEARNINGS.md`. |
| 6.6 | **Verify GOVERNS coverage**: every `.go` source file (non-test) appears in exactly one `GOVERNS:` list. Script this. |
| 6.7 | **Verify requirement traceability**: every requirement with status `designed`+ is referenced by at least one design doc `Implements:` line. Every `planned` requirement is NOT referenced (hasn't been designed yet). |
| 6.8 | **Verify design-to-requirement links**: every `Implements:` line references a valid requirement ID that exists in `spec/requirements/`. Statuses in design docs match statuses in requirements files. |
| 6.9 | **Verify no orphan docs**: no `.md` files exist outside `spec/`, `CLAUDE.md`, `BACKLOG.md`, and `MIGRATION.md`. |
| 6.10 | **Verify doc sizes**: no design doc exceeds ~500 lines. Flag any that do for potential splitting. |
| 6.11 | Delete `MIGRATION.md`. |

## Migration Definition of Done

The migration is complete when:
1. All Phase 6 verification checks pass
2. `docs/` directory no longer exists
3. `LEARNINGS.md` no longer exists
4. `MIGRATION.md` no longer exists
5. Every Go source file is governed by exactly one design doc
6. Every requirement has a status and uses EARS format
7. Every design section has `Implements:` with mirrored statuses
8. CLAUDE.md is under 150 lines
9. BACKLOG.md has no references to old doc paths or phase numbers
10. `.worktreeinclude` and code comments reference `spec/` paths

## Writing Rules

1. **Present tense, normative.** "The planner selects ActionDownload when..." not "We decided to make the planner select..." or "In Phase 5.7 we added..."
2. **No phase numbers.** No "Phase 5.7," no "v2," no version history.
3. **No migration language.** No "previously," "we used to," "replaced by," "legacy."
4. **No hedging for implemented features.** "The system does X" not "the system should do X." For unimplemented features, the `[planned]` status marker communicates aspiration — the prose is still normative.
5. **Technical necessities in design, not requirements.** "Normalize driveId casing" belongs in `design/graph-client.md`, not requirements — it's derived from reference material.
6. **When in doubt, check code.** If a source doc claims something, verify against actual code before writing it into a target doc.
7. **One fact, one place.** If the same information could go in two docs, pick the one that owns it causally (upstream wins) and reference it from the other.
8. **Constraints are specific.** The constraints section in module docs contains: hard platform limits, API rate limits, known failure modes the code must handle. NOT general tips, war stories, or aspirational goals.

## Repeatable Process for Delivering an Increment

This replaces the current development workflow in CLAUDE.md after migration.

### 1. Pick work

Check `spec/requirements/` for acceptance criteria with status `planned` or `designed`. Check `BACKLOG.md` for bugs. Claim it.

### 2. Read

CLAUDE.md routing table tells you which docs to read:
- The requirements file for the acceptance criteria you're implementing
- The design doc(s) governing the modules you'll change
- Reference docs if touching Graph API or OneDrive behavior

### 3. Branch

Create a branch: `<type>/<task-name>`.

### 4. Develop (TDD)

Red → green → refactor. No exceptions.

### 5. Update docs

Mandatory, not optional:
- **Design doc**: update the module doc(s) you touched. New behavior → new spec section. Changed behavior → updated spec. New constraint discovered → constraints section.
- **Requirements**: if you completed a feature, update status (`implemented` → `verified` once tests pass). Mirror status in the design doc `Implements:` line.
- **Reference**: if you discovered a new API quirk, file a BACKLOG item to update the relevant reference doc upstream. Do NOT update reference docs directly during implementation — route through the causal chain.
- **BACKLOG.md**: if you found issues, file them.

### 6. Self-verify

Re-read the governing design doc. Produce a compliance report listing each spec item, whether it was implemented in full, partially, or not at all, and how it was implemented. Include in the PR description.

### 7. Quality gates

Format, build, test, lint, coverage, E2E, traceability check.

### 8. Ship

Push, PR, CI green, merge.

### Verification rule

A PR that changes code without updating the governing design doc is incomplete. A PR that marks a requirement `verified` without a test verifying the acceptance criteria is incomplete. A PR without a self-verification report is incomplete.
