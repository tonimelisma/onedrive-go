# Test Assurance Audit

Last updated: 2026-04-03

Purpose: build a spec-first model of the ideal test suite, then compare the real suite against that model to catch missing, weak, or nerfed tests before we start patching coverage.

Current phase: Phase 1. This document is being built from requirements, design docs, and code ownership only. Do not read test bodies for a workstream until its ideal model is written down here.

## How To Use This Document

1. Pick the highest-risk workstream that is still in `ideal-model` or `claims-mapping`.
2. Read the governing requirements and design docs first. Read package/file ownership next. Do not read test bodies yet.
3. Write down the ideal contract:
   - what behavior must be proven
   - which layer should prove it: unit, integration, e2e, stress, fuzz
   - what failure injection is mandatory
   - what bug or regression should definitely make a test fail
4. Only after the ideal contract is stable, map existing test claims:
   - test file names
   - `// Validates:` tags
   - package coverage shape
5. Only after claim mapping, read the actual test bodies and grade them:
   - `strong`
   - `weak`
   - `missing`
   - `blocked-by-planned`
6. Fix the weakest or highest-risk gap first.
7. Update this file every time a workstream moves phase or a new risk is discovered.

## Phase Legend

- `ideal-model`: ideal suite defined from spec/design only
- `claims-mapping`: existing test claims may be mapped, but bodies not yet audited
- `test-audit`: test bodies have been reviewed against the ideal contract
- `remediation`: concrete test or production fixes in flight
- `strong-enough`: current suite appears to meet the ideal contract for now
- `blocked-by-planned`: requirement is still planned or future, so only a placeholder is tracked

## Strength Rubric

- `strong`: test would fail for the obvious regression and exercises the contract at the right layer with meaningful assertions
- `weak`: test exists, but could be nerfed without detection because it only checks shallow output, only `no error`, over-mocks reality, or misses failure paths
- `missing`: no test appears to prove the contract
- `blocked-by-planned`: the requirement is not implemented yet, so only future ideal coverage is described

## Weak-Test Smell Checklist

- Asserts only `no error` or exit code without checking state changes or side effects
- Over-mocks collaborators so the real boundary behavior cannot fail
- Covers only the happy path for an I/O feature with no failure injection
- Verifies logs or strings but not the persisted state transition behind them
- Uses a golden file without semantic spot-checks for the critical fields
- Proves that something happened, but not that the wrong thing did not happen
- Does not include a kill switch: an obvious one-line production regression that would make the test fail
- Exercises a code path only indirectly when the requirement is actually about sequencing, durability, cancellation, or cleanup

## Audit Rules

- Organize by requirement and invariant first, not by current tests.
- Treat design invariants as first-class audit targets even when no single requirement line captures them.
- Distinguish missing coverage from unimplemented product work.
- Prefer one authoritative proof point per contract plus supporting lower-level tests, instead of many shallow duplicates.
- Add audit notes for production risks discovered during reading, even if test work comes later.

## Completeness Protocol

Use this section to reduce the chance that the audit misses an entire class of behavior.

### Sources Of Truth To Mine

Every workstream should be built from all relevant source types, not just requirements:

- Requirements:
  - user-visible product behavior
  - success-path command contracts
  - verification status claims already present in spec
- Technical design:
  - sequencing rules
  - ownership boundaries
  - state transitions
  - non-obvious rationale and constraints
- Code:
  - actual branch structure
  - error paths
  - cleanup behavior
  - concurrency and cancellation points
  - any implemented behavior not yet reflected cleanly in spec
- Reference docs:
  - API quirks
  - platform differences
  - external constraints that can create hidden regressions
- Existing test metadata:
  - `// Validates:` tags
  - test filenames
  - package test layout
- Historical signals:
  - archived design reviews
  - bug-fix comments in tests
  - TODO or `[planned]` notes that imply currently untested edges

### Discovery Passes

For each workstream, do all of these passes before calling the ideal model complete:

1. Requirement pass:
   - enumerate every directly governed requirement ID
   - break umbrella requirements into concrete observable contracts
2. Design invariant pass:
   - extract all “must happen before”, “must never”, “single owner”, “durable only”, “fail open/closed”, and “one source of truth” rules
3. Boundary pass:
   - CLI boundary
   - config boundary
   - filesystem boundary
   - network/API boundary
   - persistence boundary
   - signal/shutdown boundary
4. Lifecycle pass:
   - startup
   - steady state
   - reload/reconfigure
   - shutdown
   - crash and restart
   - cleanup after failure
5. Failure-mode pass:
   - transient failures
   - terminal failures
   - partial failures
   - malformed input
   - missing fields
   - timeout/cancellation
   - permission errors
   - disk/full resource exhaustion
6. Platform pass:
   - Linux vs macOS
   - case-sensitive vs case-insensitive filesystems
   - path and permission differences
7. Scale pass:
   - zero items
   - one item
   - many items
   - threshold edges
   - concurrency limits
   - long-running watch behavior
8. Observability pass:
   - logs
   - user-facing messages
   - JSON output
   - durable state visibility
9. Negative-space pass:
   - what must not happen
   - what must not be retried
   - what must not be deleted
   - what state must not be advanced
10. Kill-switch pass:
   - list 3 to 10 obvious regressions that should definitely break tests

### Coverage Dimensions Checklist

A workstream is not “fully modeled” until we have considered all applicable dimensions below:

- Happy path
- Boundary values
- Invalid input
- Retry and backoff
- Cancellation
- Cleanup and resource release
- Crash consistency
- Restart behavior
- Concurrency/races
- Idempotency
- Durability and persistence
- Security and containment
- Platform variance
- Human-readable output
- Machine-readable output
- External API quirks
- Performance-sensitive thresholds

### Code-Mining Checklist

When moving beyond spec into code, look for these structures because each often implies a test obligation:

- `switch` on status/error/action type
- retry loops and retry scheduling
- goroutine starts and channel ownership
- timer creation and timer reset logic
- `defer` cleanup paths
- transaction boundaries
- `os.Rename`, temp files, and atomic-write flows
- path sanitization and containment checks
- “fail open” / “fail closed” comments
- TODO, FIXME, and bug-number comments in tests or production code
- sentinel errors used in `errors.Is`

### Missing-Ideal Questions

Before marking a workstream complete, answer these:

- What contract is only described in code comments and nowhere in requirements?
- What contract is only described in a design rationale paragraph and nowhere else?
- What would break if we reordered lifecycle steps?
- What user data could be lost, duplicated, or hidden if this behavior regressed?
- What stale durable state could survive a crash and poison the next run?
- What platform-specific behavior could pass on one OS and fail on another?
- What requirement is marked verified but has weak or missing traceability?
- What is implemented in code but not claimed in the spec, or claimed in the spec but not wired in code?

### Stronger Row Schema

When a workstream starts getting deep, add this per-contract schema instead of relying only on bullets:

- Contract / invariant ID
- Source type:
  - requirement
  - design invariant
  - code-derived
  - external quirk
- Why it matters
- Code entry points
- Ideal proving layer
- Mandatory failure injections
- Kill switch
- Existing claim coverage
- Body-audit verdict
- Follow-up

### Confidence Rule

Do not say “we know everything we should ideally test” until:

- every workstream has completed the discovery passes above
- every high-risk workstream has at least one code-derived pass, not just spec-derived coverage
- every verified requirement has either a convincing proof path or an explicit audit note explaining the gap
- every design doc with operational invariants has had those invariants copied into this audit

## Gap Taxonomy

Use precise labels so we do not blur together very different problems.

- `missing-test`:
  - the ideal contract is implemented, but no convincing test exists
- `weak-test`:
  - a test exists, but an obvious regression could slip through
- `traceability-gap`:
  - coverage may exist, but the proof path is hard to locate because `// Validates:` tags or audit notes are missing
- `spec-drift`:
  - requirement or design status claims do not match the current code or audit evidence
- `implementation-gap`:
  - the requirement is claimed as implemented or verified, but the code path appears absent or only partially wired
- `planned-gap`:
  - the ideal contract is known, but the feature is still planned
- `benchmark-gap`:
  - the contract is about performance or scale and needs benchmark/profile evidence rather than ordinary correctness tests
- `env-gap`:
  - ideal coverage requires live credentials, OS-specific behavior, or stress infrastructure not yet available in the normal suite

## Evidence Tags

When writing future notes, use these tags inline so the basis of a claim stays obvious:

- `REQ`: requirement-derived
- `DESIGN`: technical design invariant or rationale
- `CODE`: production-code-derived behavior
- `REF`: external API or platform quirk
- `META`: test metadata only
- `BODY`: test body reviewed
- `E2E`: live or end-to-end evidence
- `SUSPECT`: likely gap, not yet proven

Example:

- `REQ+DESIGN`: one-shot execution must not return before results are drained
- `CODE+SUSPECT`: disable-upload-validation appears in config but no runtime wiring found yet
- `META`: many tests claim `R-2.10.*`, but bootstrap ordering is still not visibly tagged

## Ideal Proving Layers

Use the narrowest layer that can prove the contract honestly, then add higher layers only when they catch different failure classes.

- `unit`:
  - pure logic, branching, formatting, classification, small state transitions
- `component`:
  - a package plus realistic collaborators or fakes, useful for persistence, timer, and sequencing behavior
- `integration`:
  - multiple real packages together, usually including filesystem, DB, HTTP test server, or signal flow
- `e2e`:
  - full CLI or sync behavior through real entry points
- `live-e2e`:
  - OneDrive-backed or environment-backed proof against actual external behavior
- `stress`:
  - repeated or high-concurrency runs intended to expose flakiness and ownership bugs
- `race`:
  - correctness under `-race`, usually paired with concurrency-focused tests
- `fuzz`:
  - parser, normalization, and API-shape hardening where malformed input matters
- `benchmark`:
  - required for target or scale claims, not just performance curiosity

## Coverage Ledger

This is the operating dashboard for completeness, not a verdict of quality. A pass is only `done` when the workstream section contains the harvested results, not just a verbal claim.

| Workstream | Req | Design | Code | Ref | Meta | Body | Kill | Verified reconcile | Highest unresolved risk |
|---|---|---|---|---|---|---|---|---|---|
| W1 | done | done | partial | partial | done | done | partial | done | current actionable W1 top-ups are closed; only planned/deferred logging policy work remains |
| W2 | done | done | done | partial | done | done | partial | done | current actionable W2 top-ups are closed; only planned nil-guard / watch-cleanup hardening remains |
| W3 | done | done | partial | n/a | done | no | partial | no | big-delete and cross-drive proof paths unclear |
| W4 | done | done | done | n/a | done | done | partial | done | live crash/restart proof is still thinner than the now-reconciled store/CLI durable-row mutation coverage |
| W5 | done | done | done | partial | done | done | partial | done | upload-session traceability gap is closed; remaining W5 risk is broader worker-pool and future planned-transfer coverage |
| W6 | done | done | partial | partial | done | partial | partial | no | pagination and transport split still need body audit |
| W7 | done | done | done | partial | done | done | partial | done | business-account live `drive search` proof is still blocked by missing business test credentials; current personal/logout/list/shared live proof is closed |
| W8 | done | done | partial | n/a | done | no | no | no | validation-tier coverage may be over-claimed |
| W9 | done | done | partial | partial | done | no | no | no | shared-drive identity fallback details still need confirmation |

Legend:

- `done`: harvested into this doc
- `partial`: started, but not enough to close
- `no`: not yet done
- `n/a`: not expected for this workstream

## Source Harvest Commands

These commands are meant to make future audit passes reproducible instead of intuition-driven.

Requirement and design harvesting:

```bash
rg -n '^\#\#? |^Implements:|^GOVERNS:' spec/design spec/requirements
rg -n '\[planned\]|\[verified\]|\[implemented\]|\[future\]' spec
```

Test traceability harvesting:

```bash
rg -n 'Validates:' -g '*_test.go'
rg --files | rg '(_test\.go$|e2e|testdata)'
```

Invariant and risk harvesting from code:

```bash
rg -n 'TODO|FIXME|BUG|panic\\(|errors\\.Is|context\\.With|go func|time\\.New|NewTicker|Retry-After|If-Match|os\\.Rename|CreateTemp|Close\\(' internal
rg -n 'fail open|fail closed|single owner|must not|invariant|atomic|durable|quiesc|drain' spec internal
```

Potential implementation-gap harvesting:

```bash
rg -n 'disable_download_validation|disable_upload_validation|transfer_workers|check_workers|MaxOneDriveFileSize|SimpleUploadMaxSize' .
rg -n 'TODO|planned' spec/design spec/requirements internal
```

Platform and environment harvesting:

```bash
rg -n 'linux|darwin|fsevents|inotify|case-insensitive|case-sensitive|permission|symlink|trash' internal spec
rg -n 'e2e|e2e_full|ONEDRIVE_ALLOWED_TEST_ACCOUNTS|ONEDRIVE_TEST_DRIVE' .
```

## Per-Workstream Exit Criteria

Do not move a workstream to `strong-enough` until all of these are true:

- The workstream section lists every applicable requirement and design invariant we know about.
- At least one code-derived pass has been done and documented.
- Every verified requirement in the governed docs has either:
  - a convincing proof path, or
  - an explicit gap label from the gap taxonomy.
- The section names the highest-risk missing kill switches.
- We can point to at least one ideal proving layer per major contract family.
- Cross-cutting concerns are covered where applicable:
  - cancellation
  - cleanup
  - crash/restart
  - observability
  - platform variance

## Cross-Cutting Contract Families

These should be checked for every workstream so we do not over-focus on happy-path functional behavior.

- Correctness:
  - the intended result happens
- Safety:
  - the wrong destructive result does not happen
- Durability:
  - state survives crashes and restarts correctly
- Recovery:
  - interrupted or degraded operations can resume or clear cleanly
- Containment:
  - writes, deletes, permissions, and paths stay inside allowed boundaries
- Coordination:
  - concurrent work, retries, and timers do not violate ownership rules
- Observability:
  - users and operators can understand what happened
- Compatibility-with-quirks:
  - external API and platform oddities are handled deliberately

## Verified-Claim Reconciliation

One explicit goal of this audit is to challenge every `[verified]` claim in the spec.

For each governed requirement marked `[verified]`, record one of:

- `proven`:
  - test path is known and believable
- `proven-but-weak`:
  - a test exists but likely would not catch an obvious nerf
- `traceability-gap`:
  - likely covered but hard to prove quickly
- `suspect-impl-gap`:
  - code path itself looks questionable
- `not-yet-audited`:
  - we have not read enough yet

When a design doc says `Implements: R-X [verified]`, the audit should verify both:

- the code appears to implement it
- a believable proof path exists

## Contract Template

Copy this block when a workstream needs finer-grained rows:

```text
Contract:
Source:
Why it matters:
Requirement IDs:
Design sections:
Code entry points:
Ideal proving layer:
Mandatory failure injection:
Negative-space assertion:
Kill switch:
Existing evidence:
Gap label:
Confidence:
Follow-up:
```

## Review Cadence

Use this cadence so the document keeps compounding instead of drifting:

1. After each new workstream pass, update the coverage ledger.
2. After each body audit, add at least one `Gap Taxonomy` label or a `proven`/`proven-but-weak` reconciliation note.
3. After each production or test fix, update both:
   - the affected workstream section
   - the highest unresolved risk column in the coverage ledger
4. When a spec status changes, update the audit in the same increment so spec drift does not accumulate.

## Workstream Backlog

| ID | Workstream | Governing docs | Primary code areas | Risk | Phase |
|---|---|---|---|---|---|
| W1 | Sync engine runtime, retry, and watch ownership invariants | `spec/requirements/sync.md`, `spec/requirements/non-functional.md`, `spec/design/sync-engine.md`, `spec/design/retry.md`, `spec/design/sync-control-plane.md`, `spec/design/sync-execution.md` | `internal/sync/`, `internal/multisync/`, `internal/syncdispatch/`, `internal/syncexec/` | Critical | `test-audit` |
| W2 | Observation correctness, filtering, collisions, normalization | `spec/requirements/sync.md`, `spec/requirements/non-functional.md`, `spec/design/sync-observation.md` | `internal/syncobserve/` | Critical | `claims-mapping` |
| W3 | Planner safety, conflict resolution, delete safety | `spec/requirements/sync.md`, `spec/requirements/non-functional.md`, `spec/design/sync-planning.md`, `spec/design/system.md` | `internal/syncplan/`, `internal/synctypes/` | Critical | `claims-mapping` |
| W4 | Sync store durability, issues lifecycle, verification/trash behavior | `spec/requirements/sync.md`, `spec/requirements/non-functional.md`, `spec/design/sync-store.md`, `spec/design/data-model.md` | `internal/syncstore/`, `internal/cli/issues.go`, `internal/cli/verify.go` | Critical | `claims-mapping` |
| W5 | Transfer manager, resumable transfer robustness, local disk safety | `spec/requirements/file-operations.md`, `spec/requirements/transfers.md`, `spec/requirements/non-functional.md`, `spec/design/drive-transfers.md` | `internal/driveops/`, `pkg/quickxorhash/`, `internal/cli/get.go`, `internal/cli/put.go` | Critical | `test-audit` |
| W6 | Graph client quirks, pagination, auth refresh, retry transport | `spec/requirements/file-operations.md`, `spec/requirements/drive-management.md`, `spec/requirements/non-functional.md`, `spec/design/graph-client.md`, `spec/design/retry.md` | `internal/graph/`, `internal/tokenfile/` | Critical | `claims-mapping` |
| W7 | CLI contracts, formatting, command independence from sync state | `spec/requirements/file-operations.md`, `spec/requirements/drive-management.md`, `spec/design/cli.md` | `internal/cli/`, `main.go` | Medium | `claims-mapping` |
| W8 | Configuration discovery, validation tiers, token resolution | `spec/requirements/configuration.md`, `spec/design/config.md` | `internal/config/` | Medium | `claims-mapping` |
| W9 | Drive identity and shared-drive discovery semantics | `spec/requirements/drive-management.md`, `spec/requirements/non-functional.md`, `spec/design/drive-identity.md` | `internal/driveid/`, `internal/cli/drive.go` | Medium | `claims-mapping` |

## First-Pass Risk Notes

These are not test results yet. They are likely weak spots inferred from requirements and design docs alone.

- Watch-loop sequencing and single-owner behavior are easy to nerf with refactors and hard to validate with shallow tests.
- Transfer resume robustness has explicit planned edge cases in the design doc: corrupt partial files, changed remote content, oversized partials.
- Observation has multiple planned hardening notes: nil field guards, buffer overflow verification, inotify setup cleanup, timestamp edge cases.
- Sync execution still calls out missing targeted `-race` stress tests for DepGraph, active scopes, Buffer, and WorkerPool.
- The system architecture doc explicitly calls out bare `assert.NoError` patterns and ignored `Close` errors as areas worth auditing for weak tests.

## Workstream Details

### W1. Sync Engine Runtime, Retry, And Watch Ownership Invariants

- Requirements and invariants:
  - `R-2.1`
  - `R-2.8.4`
  - `R-2.10.1` through `R-2.10.15`
  - `R-2.10.17` through `R-2.10.31`
  - `R-2.10.35` through `R-2.10.38`
  - `R-2.10.43`
  - `R-2.14.1` through `R-2.14.4`
  - `R-6.4.1` through `R-6.4.3`
  - `R-6.6.7`, `R-6.6.8`, `R-6.6.10`, `R-6.6.12`
  - `R-6.7.27`
  - `R-6.8.7`, `R-6.8.9`, `R-6.8.10`, `R-6.8.11`, `R-6.8.15`
  - Design-only runtime invariants from `sync-engine.md`: bootstrap ordering, one-shot completion barrier, trial failure isolation, trial success release, permission recheck release
- Ideal unit coverage:
  - `classifyResult()` proves exact routing for 401, 403, 404, 408, 412, 423, 429, 5xx, 507, `os.ErrPermission`, disk-full, and context cancellation
  - `ScopeKeyForStatus` and related routing prove shortcut-vs-own-drive-vs-account-vs-service scoping with empty `TargetDriveID` handled safely
  - timer math proves local timing defaults, `Retry-After` override, and per-scope caps
  - local permission classification proves directory-vs-file scope and auto-clear preconditions
  - shared-permission release logic proves fail-open behavior on inconclusive Graph checks
- Ideal integration coverage:
  - watch bootstrap uses one runtime pipeline and does not start observers until bootstrap work is quiescent
  - one-shot execution does not return before workers stop, results drain, and side effects are applied
  - persisted scope blocks survive restart with deadlines preserved for server-timed scopes
  - trial success releases held failures immediately and wakes retry processing without new external observation
  - trial failure extends only the active trial interval and does not re-enter normal result handling
  - scope-blocked work is durable in `sync_failures` instead of hidden only in memory
  - big-delete hold in watch mode blocks deletes while allowing non-delete work to continue
- Ideal e2e coverage:
  - watch startup with pre-existing divergence completes bootstrap before steady-state observation begins
  - `pause` and `resume` preserve accumulated work and do not lose changes
  - permission-denied subtree becomes download-only, then auto-recovers when permissions are restored
  - restart after mid-run failure rehydrates retryable work from durable state
- Mandatory failure injection:
  - fake worker results interleaving successes and failures across concurrent scopes
  - injected `Retry-After` on 429 and 503
  - injected local permission errors at both file and directory boundaries
  - restart with persisted `scope_blocks` plus `sync_failures`
  - context cancellation during active work and during cleanup
- Kill switches that must fail tests:
  - start observers before bootstrap quiescence
  - return from one-shot execution before result side effects are drained
  - route trial failure through normal `processWorkerResult`
  - clear a scope but forget to wake the retrier
  - treat `os.ErrPermission` as a remote-scoped failure requiring drive identity
- Audit status:
  - initial body audit started
  - still far from full verified-claim reconciliation
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface is broad: `internal/sync/engine_*_test.go`, `internal/sync/permissions_test.go`, `internal/syncdispatch/{active_scopes,scope,dep_graph,delete_counter}_test.go`, `internal/multisync/{orchestrator,drive_runner}_test.go`, plus `e2e/sync*_test.go` and `e2e/orchestrator_e2e_test.go`
  - Explicit comment claims already exist for `R-2.1.1` through `R-2.1.6`, many `R-2.10.*` items, `R-2.14.1` through `R-2.14.4`, `R-6.8.7`, `R-6.8.9`, `R-6.8.10`, `R-6.8.11`, `R-6.8.15`, `R-6.6.10`, and `R-6.6.12`
  - No explicit claim surfaced yet for `R-2.8.4`, `R-6.4.2`, `R-6.4.3`, or `R-6.6.7`
  - Design-only invariants still look comment-invisible: bootstrap ordering, one-shot completion barrier, trial failure isolation, and retrier wakeup on scope release
  - First body-audit target inside W1 should be sequencing tests, not simple classifier tests, because that is where nerfs are hardest to spot from metadata alone
- Verified-claim reconciliation snapshot:

| Contract | Basis | Current evidence | Reconciliation | Gap label / note |
|---|---|---|---|---|
| Bootstrap must quiesce before local observer starts | `REQ+DESIGN+BODY` | `internal/sync/engine_phase0_test.go:44` proves bootstrap upload starts first and watcher creation is delayed | `proven` | Strong sequencing proof |
| Bootstrap must quiesce before remote polling starts | `REQ+DESIGN+BODY` | `internal/sync/engine_phase0_test.go:106` proves no second delta poll occurs until bootstrap download drains | `proven` | Strong sequencing proof |
| One-shot execution must not return before drain side effects finish | `DESIGN+BODY` | `internal/sync/engine_phase0_test.go:177` blocks permission resolution and asserts `executePlan` does not return early | `proven` | Important design-only invariant |
| Trial failure must stay isolated from normal result handling | `DESIGN+BODY` | `internal/sync/engine_phase0_test.go:269` and `internal/sync/engine_result_scope_test.go:531` show interval extension without scope clearing or re-detection | `proven` | Strong regression detector |
| Trial success must release held failures without new external observation | `REQ+DESIGN+BODY` | `internal/sync/engine_phase0_test.go:323` and `internal/sync/engine_result_scope_test.go:1061` show immediate re-dispatch after scope release | `proven` | Covers the core retry-path invariant |
| Scope release must kick the retrier immediately | `DESIGN+BODY` | `internal/sync/engine_single_owner_test.go:121` and `internal/sync/engine_watch_test.go:558` assert retry wakeup after release | `proven` | Good watch-loop ownership proof |
| Watch-mode big-delete hold must block deletes while allowing non-deletes | `REQ+DESIGN+BODY` | `internal/sync/engine_watch_test.go` now carries explicit `R-6.4.2` / `R-6.4.3` claims for threshold, mixed non-delete flow, and external-clear release behavior | `proven` | Traceability gap closed |
| Periodic full reconciliation in watch mode | `REQ+DESIGN+BODY` | `internal/sync/engine_reconcile_test.go` now drives the steady-state watch loop from `reconcileC`, proves async full reconciliation commits the token, and applies events back through the watch-owned buffer/result handoff | `proven` | Stronger body-level watch-loop proof for `R-2.8.4` |
| Aggregated warning logging behavior | `REQ+DESIGN+BODY+CODE` | `internal/sync/engine_result_scope_test.go` now proves `recordSkippedItems()` emits one WARN summary plus per-item DEBUG logs above threshold; this audit pass found and fixed the missing DEBUG-per-item production path in `engine_watch_reconcile.go` | `proven` | Real production bug fixed while reconciling `R-6.6.7` |
| Execution-time transient aggregation must group by `issue_type` without dropping report errors | `REQ+DESIGN+BODY+CODE` | `internal/sync/engine_result_scope_test.go` now proves `logFailureSummary()` aggregates by `issue_type`, keeps per-item DEBUG visibility above threshold, and `internal/sync/engine_run_once_test.go` proves `SyncReport.Errors` survives summary logging | `proven` | Real production bug fixed while reconciling `R-6.6.12` |

Key W1 gap notes:

- `META`: the most valuable sequencing guarantees are now directly discoverable enough for the audit ledger to treat the current W1 top-up tranche as closed.
- `BODY`: this pass closes the execution-time aggregation gap too: `R-6.6.12` now groups by `issue_type`, preserves per-item DEBUG detail, and no longer clears `SyncReport.Errors` as a side effect of logging.

### W2. Observation Correctness, Filtering, Collisions, And Normalization

- Requirements and invariants:
  - `R-2.1.2`
  - `R-2.4.1` through `R-2.4.3`, `R-2.4.6`, `R-2.4.7`
  - `R-2.11.1` through `R-2.11.5`
  - `R-2.12.1`, `R-2.12.2`
  - `R-2.13.1`
  - `R-6.7.1`, `R-6.7.3`, `R-6.7.5`, `R-6.7.19`, `R-6.7.20`, `R-6.7.24`
  - Design constraints and planned hardening from `sync-observation.md`: nil guards, buffer overflow verification, inotify setup cleanup
- Ideal unit coverage:
  - item conversion handles missing optional fields without panic
  - rename of a remote folder recalculates descendant paths correctly
  - zero-event delta page does not advance the token
  - filter cascade rejects dotfiles, marker patterns, configured symlink exclusions, vault items, and OneDrive-invalid filenames with actionable issue output where required
  - default symlink following dereferences both file and directory symlinks without infinite recursion
  - case-collision detection covers file/file, file/baseline, directory/directory, and N-way peer tracking on resolution
  - NFC normalization makes macOS NFD and NFC names compare equal
- Ideal integration coverage:
  - local observer coalesces bursty writes into correct buffered outcomes without losing the final state
  - remote observer handles paginated delta, duplicate items, 410 reset, and cross-drive shortcut parent chains
  - buffer overflow behavior is explicit and observable
  - partial inotify setup failure cleans up already-added watches
  - scanner permission interaction emits releasable failures instead of silent skips
- Ideal e2e coverage:
  - live watch mode catches local create, modify, delete, and rename with remote convergence
  - shared-folder observation differs correctly across Personal and Business/SharePoint drive types
  - case collisions block uploads until resolved and re-emit promptly on delete resolution
- Mandatory failure injection:
  - delta stream with reordered operations and duplicate IDs
  - missing `Name`, `ParentReference`, or `lastModifiedDateTime`
  - buffer overflow or flood conditions
  - symlink loops or unreadable directories
- Kill switches that must fail tests:
  - advance delta token on an empty delta response
  - drop descendant path recalculation on folder rename
  - silently skip invalid OneDrive filenames instead of recording an actionable issue
  - fail to re-emit colliding peers after one side is deleted
  - panic on sparse delta payloads
  - synthesize `time.Now()` for malformed remote timestamps and accidentally suppress a real follow-up hash/download decision
- Audit status:
  - ideal model drafted
  - note: planned requirements `R-2.4.4`, `R-2.4.5`, `R-6.7.15`, `R-6.7.21` stay out of the current strong/weak audit until implemented
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface is strong around observation mechanics: `scanner_test.go`, `observer_remote_test.go`, `buffer_test.go`, `observer_local_*_test.go`, `item_converter_test.go`, and `inotify*_test.go`
  - Explicit comment claims already exist for `R-2.1.2`, `R-2.11.1` through `R-2.11.5`, `R-2.12.1`, `R-2.12.2`, `R-2.13.1`, `R-6.7.1`, `R-6.7.3`, `R-6.7.5`, `R-6.7.20`, and `R-6.7.24`
  - No explicit claim surfaced yet for `R-2.4.1` through `R-2.4.3`, `R-2.4.6`, `R-2.4.7`, or `R-6.7.19`
  - Design hardening notes also look unclaimed from metadata: sparse payload nil-guarding, buffer overflow behavior, and inotify partial-watch cleanup
  - First body-audit target inside W2 should be naming/filtering coverage, because collisions and watch mechanics are visibly tagged while filter/naming guarantees are not
- Body-audit notes from `internal/syncobserve/*_test.go`, `internal/sync/*_test.go`, and observation/config wiring:
  - Investigation outcome: the original W2 suspicion was real. `skip_dotfiles`, `skip_dirs`, and `skip_files` were marked verified in requirements/design, but the resolved config never reached `LocalObserver` or retry/trial single-path reconstruction.
  - Production fix: the sync engine now carries a resolved `LocalFilterConfig`, applies it to one-shot scans and watch mode via `LocalObserver.SetFilterConfig`, and threads the same filter state into retry/trial reconstruction through `ObserveSinglePathWithFilter`.
  - Regression coverage now exists at multiple layers:
    - `internal/syncobserve/filter_test.go` proves full-scan filtering, watch setup subtree exclusion, watch write suppression, and single-path retry/trial reconstruction semantics
    - `internal/sync/engine_filter_test.go` proves the engine suppresses uploads for configured exclusions instead of merely unit-testing the observer in isolation
    - `internal/cli/sync_helpers_test.go` proves `newSyncEngine` propagates resolved config into the engine instead of dropping it on the floor
  - Deeper W2 naming audit found two more real production gaps inside `R-2.11.*`: the broad internal `~...` exclusion swallowed OneDrive-reserved `~$...` names before they could become actionable invalid-filename skips, and SharePoint root-level `forms` was present in requirements/reference docs but observation had no drive-type rule context to enforce it.
  - Production fix: internal temp-file exclusion now keeps `~foo` / `.~foo` silent while letting `~$foo` flow into OneDrive name validation, and the engine now threads `LocalObservationRules` into full scans, watch mode, and retry/trial single-path reconstruction so SharePoint root-level `forms` is rejected consistently.
  - Regression coverage now also proves:
    - `internal/syncobserve/observer_local_test.go` records `~$...` and other OneDrive-invalid names as actionable skipped items instead of treating them as silent temp files
    - `internal/syncobserve/observer_local_test.go` and `internal/syncobserve/single_path_test.go` reject SharePoint root-level `forms` in both full-scan and retry/trial reconstruction paths
    - `internal/sync/engine_run_once_test.go` and `internal/cli/sync_helpers_test.go` prove the SharePoint-only rule survives engine runtime and CLI config wiring instead of living only in unit-level validation helpers
    - `internal/config/validate_test.go` now proves `skip_files` / `skip_dirs` no longer warn as unimplemented
  - Follow-up symlink audit outcome: `skip_symlinks` had the same shape of defect. The config/default existed, but `LocalFilterConfig` dropped the field and the observer hard-skipped symlinks regardless of configuration. That is now fixed.
  - New W2 symlink regression coverage proves:
    - default `skip_symlinks = false` follows file and directory symlinks during full scan and single-path reconstruction
    - `skip_symlinks = true` excludes symlink entries from full scan, watch setup, and single-path reconstruction
    - directory symlink cycles stop at the alias boundary instead of recursing forever
  - Follow-up W2 delete-semantic audit found another real prod bug: local silent exclusions could still resurface as synthetic deletes when the baseline already contained the path. That affected `skip_dotfiles`, `skip_dirs`, `skip_files`, and skipped symlink aliases, with an extra watch-mode hole where a skipped symlink could later emit `ChangeDelete` on remove.
  - Production fix: deletion detection now suppresses deletes for baseline paths that current local filters exclude, and watch mode retains skipped symlink alias knowledge so remove events and later safety scans stay silent instead of fabricating deletes.
  - Deeper W2 actionable-lifecycle audit found a retry/trial asymmetry: `recordRetryTrialSkippedItem()` already normalized zero `drive_id` rows to the engine drive when re-recording a skipped actionable issue, but the silent-resolution clear path deleted using the raw row drive. That could leave stale actionable rows behind for legacy or malformed zero-drive retry/trial failures.
  - Production fix: retry/trial failure maintenance now normalizes the drive identity symmetrically for both actionable upsert and resolved/silent clear paths.
  - Regression coverage now also proves:
    - reasonless retry/trial skips clear stale actionable rows even when the stored failure row carries zero `drive_id`
    - retry/trial replacement of one actionable scanner issue with another updates the existing row in place instead of accumulating stale state
  - Sparse-payload follow-up found another real remote-observation bug: the converter would emit malformed non-root events with empty `ItemID`, non-deleted items with empty `Name`, and delete events whose path could not be recovered from either the baseline or the delta payload itself. Those flowed into buffer/planner as empty-ID or empty-path remote changes.
  - Production fix: the remote converter now warns and skips those malformed sparse items, while still materializing deletes from the current delta payload when the item name and surviving parent chain are sufficient.
  - Regression coverage now proves:
    - `internal/syncobserve/observer_remote_test.go` skips remote items with empty `ItemID`
    - non-deleted remote items with empty `Name` are skipped instead of emitting empty-path creates/modifies
    - deletes with no recoverable path are skipped instead of emitting unmaterializable empty-path delete events
  - Sparse-timestamp follow-up found a second remote-normalization gap: the graph boundary treated empty, invalid, or out-of-range timestamps as `time.Now().UTC()`, which avoided panics but fabricated mtimes for sparse delta payloads and CLI output.
  - Production fix: graph timestamp parsing now preserves those values as unknown zero time, remote observation carries zero `Mtime` for sparse items, and CLI output renders zero timestamps as `unknown` in text and empty string in JSON instead of year-0001 or a fake current timestamp.
  - Regression coverage now proves:
    - `internal/graph/items_test.go` keeps empty, invalid, future, and delete-null timestamps at zero/unknown rather than replacing them
    - `internal/syncobserve/observer_remote_test.go` preserves zero timestamps through full delta conversion
    - `internal/cli/format_test.go`, `internal/cli/ls_test.go`, and `internal/cli/stat_test.go` render unknown timestamps sanely at the presentation boundary
  - Another W2 sparse-delta audit pass found a second live observation bug: non-delete delta updates can legally omit unchanged `name` and `parentReference`, but the converter still treated those fields as complete. That caused two bad outcomes: sparse updates with empty `name` were skipped entirely, and sparse renames with empty parent data were re-rooted to the sync root instead of staying in their baseline directory.
  - Production fix: item conversion now treats those omissions as partial updates when a baseline entry exists. Missing leaf names are recovered from `path.Base(existing.Path)`, missing parent context reuses the baseline directory, and moves with only one changed path component still materialize correctly.
  - Regression coverage now proves:
    - `internal/syncobserve/observer_remote_test.go` keeps sparse modify events on their existing path when both `name` and `parentReference` are omitted
    - sparse move events with only a new parent and no `name` still recover the baseline leaf
    - `internal/syncobserve/item_converter_test.go` proves shortcut-scoped sparse renames preserve the existing subdirectory instead of collapsing to the scope root
- Verified-claim reconciliation snapshot:

| Contract | Basis | Current evidence | Reconciliation | Gap label / note |
|---|---|---|---|---|
| `skip_dotfiles` excludes hidden files and directories from full-scan observation | `REQ+DESIGN+CODE+BODY` | `internal/syncobserve/filter_test.go` creates dotfile files/dirs and proves they do not produce `ChangeEvent`s or `SkippedItem`s | `proven` | Direct scanner/body proof added |
| `skip_dirs` excludes configured directory names from scan and watch setup | `REQ+DESIGN+CODE+BODY` | `internal/syncobserve/filter_test.go` proves `vendor/` is absent from full scans and does not receive watches in `AddWatchesRecursive` | `proven` | Covers both scan and watch-start behavior |
| `skip_files` excludes matching file globs from scan and watch write handling | `REQ+DESIGN+CODE+BODY` | `internal/syncobserve/filter_test.go` proves `*.log` files are omitted from full scans and suppressed on write events | `proven` | Direct file-glob regression coverage added |
| `skip_symlinks = false` follows symlink targets by default, while `skip_symlinks = true` excludes them | `REQ+DESIGN+CODE+BODY+REF` | `internal/syncobserve/observer_local_test.go`, `internal/syncobserve/filter_test.go`, `internal/syncobserve/single_path_test.go`, and `internal/syncobserve/observer_local_delete_test.go` now prove default follow, configured skip, watch setup parity, and cycle-stop behavior. External behavior was cross-checked against `abraunegg/onedrive` docs before implementation. | `proven` | Real prod gap fixed; now aligned with researched client behavior |
| Silent local exclusions must not later reappear as synthetic deletes | `REQ+DESIGN+CODE+BODY` | `internal/syncobserve/filter_test.go` now proves full scans suppress deletes for baseline paths hidden by `skip_dotfiles`, `skip_dirs`, `skip_files`, and `skip_symlinks`, including skipped symlink-directory descendants; `internal/syncobserve/observer_local_delete_test.go` proves skipped symlink remove events stay silent and remain suppressed through the next safety scan | `proven` | Real prod gap fixed in both scanner and watch mode |
| Retry/trial single-path reconstruction must honor the same local filters as normal observation | `DESIGN+CODE+BODY` | `internal/syncobserve/filter_test.go` proves `ObserveSinglePathWithFilter` resolves configured exclusions silently | `proven` | Important design-only invariant now covered |
| OneDrive-invalid local names, including `~$...` and SharePoint root-level `forms`, must surface as actionable skips instead of silent exclusions | `REQ+DESIGN+CODE+BODY+WEB` | `internal/syncobserve/observer_local_test.go`, `internal/syncobserve/single_path_test.go`, `internal/sync/engine_run_once_test.go`, and `internal/cli/sync_helpers_test.go` now prove invalid characters, reserved names/patterns, `~$...`, and SharePoint root `forms` all route through `SkippedItem` / actionable-failure handling rather than disappearing behind internal exclusions. Microsoft’s restrictions page is the governing external source for the `~$` and SharePoint-root `forms` rules. | `proven` | Two real prod gaps fixed: broad `~` exclusion and missing SharePoint drive-type validation context |
| Retry/trial actionable maintenance must normalize missing drive IDs for both upsert and clear paths | `REQ+DESIGN+CODE+BODY` | `internal/sync/engine_single_owner_test.go` now proves zero-drive retry/trial rows clear stale actionable failures against the engine drive and that same-path scanner-issue replacement updates the existing actionable row in place | `proven` | Real prod gap fixed in retry/trial failure maintenance |
| Remote observation must skip malformed sparse items it cannot identify or materialize safely | `DESIGN+CODE+BODY` | `internal/syncobserve/observer_remote_test.go` now proves remote items with empty `ItemID`, non-deleted items with empty `Name`, and delete entries without any recoverable path are warned and skipped instead of emitting empty-path or empty-ID events | `proven` | Real prod gap fixed in remote item conversion |
| Invalid or absent remote timestamps must remain unknown instead of being synthesized into current time | `REQ+DESIGN+CODE+BODY+WEB` | Official Graph delta docs describe sparse changed-property payloads, while `internal/graph/items_test.go`, `internal/syncobserve/observer_remote_test.go`, and CLI formatting tests now prove invalid, empty, future, and delete-null timestamps stay zero/unknown through parsing, observation, and presentation | `proven` | Real prod gap fixed at the graph boundary; spec now marks `R-6.7.16` and `R-6.7.26` verified |
| Sparse non-delete delta items must recover omitted unchanged path fields from the baseline | `REQ+DESIGN+CODE+BODY+WEB` | Microsoft’s delta overview documents that updated instances can be returned with only `id` plus the changed properties. `internal/syncobserve/observer_remote_test.go` and `internal/syncobserve/item_converter_test.go` now prove missing `name` and missing parent context are recovered from the baseline so sparse modifies and moves stay correctly rooted | `proven` | Real prod gap fixed in remote item conversion; spec now tracks `R-6.7.29` |
| Descendants must stay correctly rooted when a sparse parent in the same batch omits unchanged metadata | `DESIGN+CODE+BODY+WEB` | `internal/syncobserve/observer_remote_test.go` now proves a child follows a renamed parent whose `parentReference` was omitted, and `internal/syncobserve/item_converter_test.go` proves shortcut-scoped descendants keep a moved parent's recovered leaf name when the parent item omits `name` | `proven` | Real prod gap fixed in inflight parent recovery for sparse batches |
| Resolved config must reach the engine and observer instead of being dropped in CLI setup | `REQ+CODE+BODY` | `internal/cli/sync_helpers_test.go` plus `internal/sync/engine_filter_test.go` prove config propagation and end-to-end upload suppression | `proven` | Fixed production wiring gap |

Key W2 gap notes:

- `CODE+BODY`: the filter-config wiring gap was a real production defect, not a metadata illusion. It is now fixed with regression coverage at observer, engine, and CLI wiring layers.
- `CODE+BODY`: `skip_symlinks` was also a real runtime gap. The config/default existed, but local observation ignored it and always skipped symlinks. The observer now matches `abraunegg/onedrive`: default `false` follows symlink targets, `true` excludes them, and directory cycles stop at the alias boundary.
- `CODE+BODY`: silent local exclusions also had a delete-semantic bug. Filtered paths could still come back as fabricated `ChangeDelete` events when they already existed in the baseline, and skipped symlink removes could leak through watch mode. That is now fixed with explicit regression coverage.
- `CODE+BODY`: retry/trial actionable maintenance also had a real stale-row bug for zero-drive failures. Upsert already fell back to the engine drive, but clear did not, so a silent resolution could miss the stored row. That is now fixed with regression coverage.
- `CODE+BODY`: sparse remote payload handling also had a real conversion gap. Malformed remote items could still produce empty-ID or empty-path events and reach planner input. That is now fixed for the highest-risk identity/materialization cases with regression coverage, without regressing ordinary deleted-item path recovery from delta data.
- `CODE+BODY+WEB`: sparse remote timestamps also had a real normalization gap. Empty, invalid, and out-of-range timestamps were being rewritten to `time.Now()`, which hid malformed Graph payloads behind fake metadata. That is now fixed and covered through graph parsing, remote conversion, and CLI presentation tests.
- `CODE+BODY+WEB`: sparse non-delete delta items also had a real recovery gap. When Graph omitted unchanged path fields, the converter could skip valid updates or accidentally re-root them. That is now fixed with regression coverage for primary and shortcut scopes.
- `CODE+BODY+WEB`: deeper sparse parent-chain handling also had a real inflight-map gap. Parent items recovered omitted `name` / `parentReference` only at final classification time, so descendants in the same batch could still collapse out of their parent leaf or lose their grandparent chain. That is now fixed with primary and shortcut regression coverage.
- `CODE+BODY`: scanner-time `hash_panic` rows also had a real lifecycle gap. They were recorded as actionable skipped items, but healthy later scans did not include `hash_panic` in the stale-row auto-clear sweep, so one-off hash panics could linger forever. That is now fixed with regression coverage.
- `CODE+BODY+WEB`: filename validation/actionability also had two real gaps. OneDrive-reserved `~$...` names were being swallowed by the generic internal `~...` exclusion, and SharePoint root-level `forms` was specified but unenforced because observation lacked drive-type rule context. Both are now fixed with regression coverage and Microsoft-source alignment.
- `META`: the current W2 top-up tranche is closed. Remaining observation work is now the explicitly planned hardening list in `sync-observation.md` (`R-6.7.21` nil-guard sweep, partial-watch cleanup verification, overflow/drop instrumentation), not an unresolved body-audit suspicion.

### W3. Planner Safety, Conflict Resolution, And Delete Protection

- Requirements and invariants:
  - `R-2.2.1`, `R-2.2.2`
  - `R-2.3.1`
  - `R-6.2.1`, `R-6.2.2`, `R-6.2.5`
  - `R-6.4.1`, `R-6.4.2`, `R-6.4.3`
  - `R-6.7.7`
  - `R-2.14.2`
  - design matrix and cross-drive guard from `sync-planning.md`
- Ideal unit coverage:
  - file and folder decision matrices across unchanged, local-only, remote-only, both-changed, deleted, and conflict states
  - fast-path timestamp matching never suppresses a required hash comparison when baseline or mtime contract does not allow it
  - read-only subtree planning suppresses uploads, remote moves, and remote deletes while still permitting downloads
  - cross-drive moves are decomposed into safe operations instead of server-side move attempts
- Ideal integration coverage:
  - one-shot big-delete protection aborts the run and requires explicit `--force`
  - watch-mode big-delete hold pauses only delete actions while other work keeps flowing
  - conflict resolution preserves both versions at the correct paths with durable issue recording
  - planner never schedules a remote delete from local absence unless a baseline entry exists
  - planner never compares hashes for deleted items
- Ideal e2e coverage:
  - simultaneous local and remote edit produces rename-aside conflict behavior visible to the user
  - large accidental delete wave is blocked in one-shot and held in watch mode
  - shared read-only subtree still downloads remote changes
- Mandatory failure injection:
  - stale or absent baseline
  - mismatched hashes during local delete verification
  - cross-drive shortcut moves
  - interleaved deletions near big-delete threshold
- Kill switches that must fail tests:
  - allow a remote delete with no baseline record
  - compare hashes for deleted items and plan nonsense work
  - let watch-mode big-delete hold freeze the entire engine instead of deletes only
  - allow uploads inside a download-only permission subtree
  - attempt a server-side move across drives
- Audit status:
  - ideal model drafted
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface includes `internal/syncplan/{planner,planner_edge,planner_crossdrive,planner_cascade,planner_enrichment}_test.go`, `internal/syncexec/executor_test.go`, and several sync e2e suites
  - Explicit comment claims already exist for `R-2.2`, `R-2.3.1`, `R-6.2.1`, `R-6.4.1`, `R-6.7.7`, and `R-2.14.2`
  - No explicit claim surfaced yet for `R-6.2.2`, `R-6.2.5`, `R-6.4.2`, or `R-6.4.3`
  - The presence of `planner_crossdrive_test.go` is encouraging, but no matching `// Validates:` hit surfaced for the cross-drive move guard, so the body audit should confirm both the behavior and the missing traceability tag
  - Conflict handling appears split between planner and executor tests, which is another place where shallow “happy path only” testing can hide

### W4. Sync Store Durability, Issues Lifecycle, And Verification/Trash Behavior

- Requirements and invariants:
  - `R-2.3.2` through `R-2.3.10`
  - `R-2.5.1` through `R-2.5.4`
  - `R-2.7.1`
  - `R-2.10.1`, `R-2.10.2`, `R-2.10.4`, `R-2.10.22`, `R-2.10.33`, `R-2.10.34`, `R-2.10.41`
  - `R-2.15.1`
  - `R-6.4.4`, `R-6.4.5`
  - `R-6.6.11`
- Ideal unit coverage:
  - schema/formatter helpers prove stable scope-key representation and human-readable issue messages
  - issue grouping uses shortcut display names instead of opaque identifiers
  - verify output formats preserve mismatch semantics
- Ideal integration coverage:
  - transactional writes survive interruption and leave no half-committed state
  - reset of in-progress actions creates `sync_failures` bridge rows so crashed work is rediscoverable
  - success paths clear stale failure rows for download, delete, and move
  - scope release deletes `scope_blocks`, updates `sync_failures`, and keeps per-item failure counts
  - `issues clear` and `issues retry` mutate only the targeted durable rows
  - local trash and remote recycle-bin flows preserve user-recoverable deletion semantics
- Ideal e2e coverage:
  - `issues`, `issues clear`, `issues retry`, and `verify` reflect durable store state after real sync activity
  - crash/restart leaves the next run able to continue from store truth
- Mandatory failure injection:
  - simulated crash between observation and execution bookkeeping
  - stale failures whose underlying path has been renamed, moved, or deleted
  - shortcut-scoped failures needing display-time metadata lookup
- Kill switches that must fail tests:
  - advance delta checkpoint without persisting individual item failure state
  - leave stale `sync_failures` rows after successful delete or move
  - group shortcut-scoped issues under opaque scope keys instead of human names
  - clear one issue command and accidentally clear siblings in the same scope
- Audit status:
  - ideal model drafted
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface is rich: `internal/syncstore/{baseline,sync_failures,store_scope_blocks,commit_observation,trash,verify}_test.go`, `internal/cli/{issues,failure_display,verify,status}_test.go`, plus sync recovery and CLI e2e suites
  - Explicit comment claims already exist for `R-2.3.2` through `R-2.3.9`, `R-2.5`, `R-2.5.1`, `R-2.5.4`, `R-2.10.1`, `R-2.10.2`, `R-2.10.4`, `R-2.10.22`, `R-2.10.33`, `R-2.10.34`, `R-2.10.41`, `R-2.15.1`, and `R-6.4.5`
  - This audit increment adds explicit `// Validates:` claims for `R-2.3.10`, `R-2.7.1`, `R-6.4.4`, `R-6.6.11`, `R-2.10.1`, and `R-2.10.2`
  - This is a good example of why the audit matters: store mechanics appear well tagged, but user-facing JSON/reporting guarantees may be easier to weaken without tripping metadata-covered tests
  - First body-audit target inside W4 should be `issues`/`verify` output semantics and recycle-bin defaults, not just low-level store row mutation
- Body-audit notes from `internal/cli/{failure_display,issues,rm,status,verify}_test.go`, `internal/syncstore/inspector_test.go`, and the corresponding production paths:
  - `internal/cli/failure_display.go` had a real production bug: grouped shortcut-scoped quota failures selected copy by `issue_type` alone, so both text and JSON incorrectly said the user's own OneDrive storage was full. The fix now keys user-facing failure text by `(issue_type, scope_key)` and the regression tests prove the shortcut-owner-specific variant end to end.
  - The audit surfaced spec drift in `R-2.3.10`: the requirements text still said "unified conflicts and failures array", while the long-term design and implementation both use separate `conflicts`, `failure_groups`, and `held_deletes` arrays. The requirement now matches the actual contract, and JSON tests explicitly validate that schema.
  - `verify` had a deeper gap than traceability alone: mismatch ordering still depended on baseline map iteration, so the human-readable table could reshuffle rows nondeterministically. `internal/syncverify/verify.go` now sorts mismatches by path before formatting. Direct unit tests cover cancellation plus stat/rooted-path/hash error classification, caller-level service/root tests prove the `errVerifyMismatch` exit-1 contract without the generic `Error:` prefix, golden tests lock the human-readable table output, and the full-tag verify E2E now asserts exit code `1` plus decoded mismatch semantics for a tampered file.
  - `status` had a real production gap against `R-2.10.4`: the store already computed grouped visible issues with scope meaning, but `internal/cli/status.go` discarded that projection and rendered only totals. The fix now preserves grouped issue families through `querySyncState`, emits them structurally in `status --json`, and renders them textually with scope kind plus humanized scope label under each drive's sync state.
  - `rm` default delete semantics were implemented correctly, but the contract was weakly defended. Caller-level tests now prove ordinary `rm` resolves the item and issues the recycle-bin `DELETE`, and `--permanent` routes through `permanentDelete` for both Business and Personal drives. This audit originally suspected Personal-drive fallback, but Microsoft’s current `driveItem: permanentDelete` doc and the fast live-E2E suite both disproved that theory.
  - `issues clear` and `issues retry` did not reveal a production bug, but the previous single-path tests were too weak to kill an accidental prefix-wide mutation. The seeded CLI tests now include exact-path targets plus same-prefix siblings (`docs/CON` vs `docs/CON-copy`, `data/report.xlsx` vs `data/report.xlsx.bak`) and prove the single-path commands only mutate the targeted durable rows.
  - The remaining store-heavy W4 contracts turned out to be stronger than the earlier ledger suggested. `ResetFailure` and `ResetAllFailures` already reset failed `remote_state` rows back to the correct pending states while deleting the corresponding `sync_failures` rows, `ClearResolvedActionableFailures*` already proves stale actionable rows auto-clear by issue type plus the scanner's current path set, and `ResetInProgressStates_CreatesSyncFailures_*` already proves the crash-recovery bridge-row contract. The missing piece was body-level reconciliation and explicit traceability, not another production bug.
- Verified-claim reconciliation snapshot:

| Contract | Basis | Current evidence | Reconciliation | Gap label / note |
|---|---|---|---|---|
| `issues --json` emits separate `conflicts`, `failure_groups`, and `held_deletes` arrays | `REQ+DESIGN+BODY` | `internal/cli/failure_display_test.go`, `internal/cli/issues_test.go`, and the updated `R-2.3.10` text in `spec/requirements/sync.md` all align on the split-array schema | `proven` | Prior spec drift fixed; current contract is now explicit |
| Shortcut-scoped quota failures use owner-specific reason/action text in both text and JSON output | `REQ+DESIGN+CODE+BODY` | `internal/cli/failure_display.go` now calls `synctypes.MessageForFailure`, and `internal/cli/failure_display_test.go` plus `internal/synctypes/failure_messages_test.go` prove the shortcut-specific copy | `proven` | Fixed real production bug |
| `status` preserves visible issue groups with scope context in both text and JSON output | `REQ+DESIGN+CODE+BODY` | `internal/syncstore/inspector.go` now keeps scope kind + humanized scope in `IssueSummary.Groups`, `internal/cli/status.go` preserves those groups in `sync_state.issue_groups`, and `internal/cli/status_test.go` plus `internal/syncstore/inspector_test.go` prove shortcut/account/file scope rendering | `proven` | Fixed real production gap that previously flattened grouped issue context into totals |
| `verify` text/JSON output and exit behavior preserve one deterministic mismatch set | `REQ+CODE+BODY+E2E` | `internal/syncverify/verify.go` now sorts mismatches by path; `internal/syncverify/verify_test.go`, `internal/cli/{verify,services,root}_test.go`, and `e2e/output_validation_e2e_test.go` prove deterministic ordering, classification, JSON decoding, and the exit-1/no-generic-Error contract | `proven` | Map-order gap closed and caller contract now explicit |
| Remote `rm` uses recycle-bin delete by default, with `--permanent` taking the permanent-delete path | `REQ+CODE+BODY` | `internal/cli/rm_test.go` now drives `runRm` through a real CLI session/provider seam and asserts `DELETE` versus `POST .../permanentDelete` behavior across both Business and Personal drive types | `proven` | Adjacent help-text drift fixed; permanent delete remains supported on Personal drives per current Graph docs and live E2E |
| `issues clear` and `issues retry` mutate only the targeted durable rows | `REQ+CODE+BODY` | `internal/cli/issues_test.go` now seeds exact-path targets plus same-prefix siblings and proves single-path clear/retry preserve adjacent durable rows instead of acting like prefix deletes | `proven` | Exact-path durable mutation now body-audited |
| Crash recovery and stale actionable cleanup reuse durable store truth | `REQ+BODY` | `internal/syncstore/baseline_test.go` proves `ResetInProgressStates` creates bridge `sync_failures` rows, `internal/syncstore/commit_observation_test.go` proves `ResetFailure` / `ResetAllFailures` transition failed rows back to pending and delete the matching failure row, and `internal/syncstore/sync_failures_test.go` proves `ClearResolvedActionableFailures*` removes only resolved rows of the targeted issue type | `proven` | Store behavior already matched the intended design; the audit gap was reconciliation, not production logic |

### W5. Transfer Manager, Resume Robustness, And Local Disk Safety

- Requirements and invariants:
  - `R-1.2`, `R-1.3`
  - `R-5.1.1`, `R-5.1.2`
  - `R-5.2.1` through `R-5.2.4`
  - `R-5.3.1`, `R-5.3.3`
  - `R-5.5.1` through `R-5.5.2`
  - `R-5.6.2` through `R-5.6.9`
  - `R-5.7.1`
  - `R-6.2.3`, `R-6.2.6`, `R-6.2.10`
  - `R-6.4.7`
  - `R-6.8.3`
- Ideal unit coverage:
  - upload mode selection: zero-byte, <= 4 MiB, > 4 MiB, over max size
  - chunk alignment is exact and rejects invalid configurable sizes once that feature exists
  - session cancellation uses `context.Background()` on failure
  - fragment requests omit Bearer tokens for pre-authenticated upload URLs
  - disk-space precheck distinguishes scope-level disk exhaustion from per-file insufficiency
- Ideal integration coverage:
  - interrupted download resumes from `.partial` using range requests and validates before final rename
  - interrupted upload resumes from persisted session state after process restart
  - successful download performs atomic write sequence and only renames after validation
  - simple upload performs timestamp preservation patch
  - resumable upload session includes `fileSystemInfo` for atomic timestamps
  - validation-disabled modes skip the right checks without silently skipping unrelated safety steps
- Ideal e2e coverage:
  - large file upload/download over the resumable threshold
  - zero-byte file upload
  - restart after interrupted transfer resumes instead of restarting from byte zero
  - disk threshold behavior blocks downloads cleanly
- Mandatory failure injection:
  - interrupted connections mid-transfer
  - hash mismatch after download
  - expired OAuth token during upload fragment dispatch
  - corrupt or oversized `.partial` file
  - remote file changed since partial download began
- Kill switches that must fail tests:
  - rename `.partial` into place before validation
  - send Bearer token on upload fragments
  - skip the post-simple-upload timestamp patch
  - leak upload sessions on failed transfer
  - treat low free space for one large file as a global download scope block when smaller files still fit
- Audit status:
  - first body audit completed for key transfer contracts
  - note: planned requirements `R-5.3.2`, `R-5.6.1`, `R-5.8.1` stay as future placeholders for now
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface includes `internal/driveops/{transfer_manager,session_store,session,stale_partials,disk_unix,cleanup,hash}_test.go`, `pkg/quickxorhash/quickxorhash_test.go`, `internal/cli/{get,put,root}_test.go`, plus transfer-related e2e suites
  - Explicit comment claims now exist for `R-1.2`, `R-1.2.4`, `R-1.3`, `R-1.3.4`, `R-5.2.1`, `R-5.2.2`, `R-5.2.3`, `R-5.2.4`, `R-5.3.1`, `R-5.3.3`, `R-5.5.1`, `R-5.5.2`, `R-5.6.2` through `R-5.6.9`, `R-5.7.1`, `R-6.2.3`, `R-6.2.6`, `R-6.2.10`, `R-6.4.7`, and `R-6.8.3`
  - The remaining explicit-tag gap inside W5 is now mostly `R-5.1.1` and `R-5.1.2`, which belong more naturally to sync/control-plane worker orchestration than upload-session robustness itself
  - The earlier loud metadata gap is closed for the subtle upload-session rules; the remaining W5 body work is no longer “are these rules tagged at all?” but “are the non-upload transfer and worker-pool contracts strong enough?”
- Body-audit notes from `internal/driveops/*_test.go` and `internal/graph/upload_test.go`:
  - The earlier metadata gap was partly misleading: nuanced upload-session rules are heavily covered in `internal/graph/upload_test.go`, including explicit tests for `R-5.6.2`, `R-5.6.3`, `R-5.6.4`, `R-5.6.5`, `R-5.6.7`, `R-5.6.8`, and `R-5.6.9`, plus chunk-auth behavior (`R-5.6.6`) and chunk alignment behavior
  - `internal/driveops/transfer_manager_test.go` does a solid job on download atomicity, resume behavior, hash mismatch handling, session-store resume, delete-on-resume-error, rename-failure preservation, and disk-space prechecks
  - The planned edge cases called out in `drive-transfers.md` now have direct body-level proof in `internal/driveops/transfer_manager_test.go`: corrupt partial bytes trigger hash-mismatch retry, changed remote content during resume forces a fresh re-download, and oversized partial state is discarded via the same retry path
  - Investigation outcome: the former `R-5.5.3` validation-disable requirement was not backed by any runtime path. The config keys existed only in parsing/tests, so the correct fix was to remove the dead config/spec surface instead of wiring a fake feature.
  - Investigation outcome: `R-5.7.1` was a real boundary gap. Sync observation already enforced the 250 GB limit, but direct file operations did not. `TransferManager.UploadFile` now rejects oversized files before hashing or any network transfer, and the branch has a regression test for that boundary.
  - Follow-up for W5:
    - upload-session traceability top-ups are closed; any future W5 work should focus on substantive uncovered worker-pool or live-transfer behavior, not missing `// Validates:` tags
- Verified-claim reconciliation snapshot:

| Contract | Basis | Current evidence | Reconciliation | Gap label / note |
|---|---|---|---|---|
| Fresh download uses `.partial`, verifies, then atomically renames | `REQ+DESIGN+BODY` | `internal/driveops/transfer_manager_test.go:137` and `:867` cover success and rename-failure preservation | `proven` | Strong atomic-write proof |
| Interrupted download resumes via `.partial` and range requests | `REQ+DESIGN+BODY` | `internal/driveops/transfer_manager_test.go:226`, `:268`, and `:1043` cover resume, fallback-to-fresh, and TOCTOU partial disappearance | `proven` | Good main-path resume coverage |
| Upload session state persists and is reused across retries/restarts | `REQ+DESIGN+BODY` | `internal/driveops/session_store_test.go` plus `internal/driveops/transfer_manager_test.go:500` and `:548` cover persisted resume and expired-session fallback | `proven` | Crosses storage and transfer layers well |
| Upload session creation must omit `If-Match` | `REQ+BODY` | `internal/graph/upload_test.go:1313` asserts no `If-Match` header on session creation | `proven` | Strong targeted regression test |
| Failed chunked upload must cancel the session even after caller cancellation | `REQ+DESIGN+BODY` | `internal/graph/upload_test.go:1338` proves cancel still fires after parent context cancellation | `proven` | Implementation uses detached timeout context, which satisfies the requirement intent |
| Upload-session chunk requests must omit Bearer tokens | `REQ+BODY` | `internal/graph/upload_test.go:313` asserts empty `Authorization` header for pre-auth chunk uploads | `proven` | Strong protocol-boundary test |
| Resumable-session request should include `fileSystemInfo` when mtime is known | `REQ+BODY` | `internal/graph/upload_test.go:546` and `:578` cover with/without `fileSystemInfo` | `proven` | Good API-shape proof |
| Simple upload must PATCH mtime afterward when needed | `REQ+BODY` | `internal/graph/upload_test.go:786`, `:854`, and `:886` cover patch, skip, and patch failure | `proven` | Strong contract proof |
| Zero-byte files must use simple upload, not sessions | `REQ+BODY` | `internal/graph/upload_test.go:1387` proves PUT `/content` path with empty body and no session creation | `proven` | Strong edge-case proof |
| Post-upload code must not re-query metadata immediately | `REQ+BODY` | `internal/graph/upload_test.go:1429` fails on unexpected GET after upload | `proven` | Strong anti-regression test |
| Direct upload rejects files over 250 GB before hashing or attempting transfer | `REQ+CODE+BODY` | `internal/driveops/transfer_manager.go` now rejects above `driveops.MaxOneDriveFileSize` before hashing or upload, and `internal/driveops/transfer_manager_test.go` proves hash and upload are both bypassed | `proven` | Fixed production gap and added explicit regression coverage |
| Resume edge cases: corrupt partial, changed remote content, oversized partial | `DESIGN+BODY` | `internal/driveops/transfer_manager_test.go` now covers corrupt partial bytes, stale remote content during resume, and oversized partial state via resume-then-fresh retry assertions | `proven` | Direct regression coverage added at the transfer-manager boundary |

Key W5 gap notes:

- `CODE+DOC`: dead validation-disable toggles were removed rather than implemented, because they had no runtime contract and would only create a misleading escape hatch.
- `CODE+BODY`: direct `put` / transfer-layer 250 GB rejection is now explicitly implemented and regression-tested at the shared transfer boundary.
- `BODY`: W5 is stronger than the metadata first suggested because much of the subtle behavior lives in graph-layer tests, not just transfer-manager tests.
- `META`: the upload-session traceability gap is now closed. Remaining W5 work is either future-planned (`R-5.3.2`, `R-5.6.1`, `R-5.8.1`) or outside this specific upload-session tagging tranche (`R-5.1.*` worker-pool proof).

### W6. Graph Client Quirks, Pagination, Auth Refresh, And Retry Transport

- Requirements and invariants:
  - `R-1.1`, `R-1.4`, `R-1.5`, `R-1.6`, `R-1.7`, `R-1.8`
  - `R-3.1`
  - implemented portions of `R-6.7`
  - `R-6.8.1`, `R-6.8.2`, `R-6.8.4`, `R-6.8.6`, `R-6.8.8`, `R-6.8.14`
- Ideal unit coverage:
  - URL decoding and drive ID normalization are applied consistently across API surfaces
  - duplicate delta items keep the last occurrence per ID
  - item-type filtering excludes OneNote package items correctly
  - `Retry-After` parsing covers both 429 and 503
  - 401 refresh path preserves the original request semantics and returns fatal unauthorized on failure
- Ideal integration coverage:
  - paginated list and delta follow every `@odata.nextLink`
  - transient 404 `itemNotFound`, transient 403 after refresh, and 410 delta expiry all route correctly
  - sync callers use raw `http.DefaultTransport` while CLI callers use retry transport
  - copy polling continues until completion and handles non-terminal responses correctly
  - path-based queries validate that the returned item actually matches the requested path once `R-6.7.22` is implemented
- Ideal e2e coverage:
  - drive listing across account and shared-drive shapes
  - login/logout/token refresh flow for CLI commands
  - server-side copy and move behavior against live Graph API
- Mandatory failure injection:
  - paginated multi-page responses
  - malformed or sparse payload fields
  - expired token followed by refresh success and failure
  - retryable 429/503 with and without `Retry-After`
- Kill switches that must fail tests:
  - stop after the first page when `@odata.nextLink` exists
  - fail to decode URL-encoded item names
  - treat sync traffic as if retry transport were active
  - swallow refresh failure instead of returning fatal unauthorized
  - keep the first duplicate delta item instead of the last
- Audit status:
  - ideal model drafted
  - note: planned requirements `R-6.7.11`, `R-6.7.18`, `R-6.7.22`, `R-6.7.23` stay as future placeholders for now
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface is broad: `internal/graph/{client,items,drives,delta,download,upload,normalize,quirks,types,url_validation}_test.go`, `internal/tokenfile/tokenfile_test.go`, `internal/cli/{ls,stat,mv,cp,auth}_test.go`, and drive-list plus CLI e2e suites
  - Explicit comment claims already exist for `R-1.1`, `R-1.1.1`, `R-1.1.2`, `R-1.1.3`, `R-1.4`, `R-1.5`, `R-1.6`, `R-1.7`, `R-1.8`, `R-3.1`, `R-6.7.8`, `R-6.7.9`, `R-6.7.10`, `R-6.7.12`, `R-6.7.13`, `R-6.8.1`, `R-6.8.2`, `R-6.8.6`, `R-6.8.8`, and `R-6.8.14`
  - No explicit claim surfaced yet for `R-6.7.4` or `R-6.8.4`
  - `internal/tokenfile/tokenfile_test.go` exists, but no `// Validates:` hit surfaced in this pass, so auth/token durability traceability likely needs cleanup even if the tests are solid
  - First body-audit target inside W6 should be pagination and retry/auth boundary tests, because those can be partially covered while still missing the exact transport split or refresh failure semantics

### W7. CLI Contracts, Formatting, And Command Independence From Sync State

- Requirements and invariants:
  - `R-1.1.1`, `R-1.2.4`, `R-1.3.4`, `R-1.4.3`, `R-1.5.1`, `R-1.6.1`, `R-1.7.1`, `R-1.8.1`, `R-1.9.4`
  - `R-2.3.7`, `R-2.3.8`, `R-2.3.9`, `R-2.3.10`
  - `R-2.7.1`
  - `R-3.1.3`, `R-3.1.4`, `R-3.1.5`, `R-3.1.6`
  - `R-6.2.8`
  - `R-6.6.11`
- Ideal unit coverage:
  - formatting helpers prove stable text and JSON shape for each command
  - failure rendering proves plain-language reason and actionable remediation text
  - logout/purge display paths prove exactly which resources are removed and which are preserved
- Ideal integration coverage:
  - each CLI `RunE` path proves flag parsing, backend invocation, exit behavior, and emitted output
  - file operation commands prove independence from sync state and sync database availability
  - issues and verify commands prove JSON and human-readable output shapes from realistic store-backed inputs
- Ideal e2e coverage:
  - CLI commands produce machine-consumable JSON with the documented keys
  - auth/logout/purge flows work against real config and token files without collateral deletion
  - user-facing failure output is readable and stable under common failure classes
- Mandatory failure injection:
  - backend command errors
  - malformed or empty result sets
  - logged-out state and partial local credential state
  - sync store unavailable while file commands still run
- Kill switches that must fail tests:
  - a file command accidentally touches sync state to complete its work
  - a JSON output path silently drops a required field
  - `issues` output loses human-readable scope/action text
  - logout or purge deletes more state than the contract allows
- Audit status:
  - ideal model drafted
  - body audit done
  - live proof closed for currently available personal/shared test accounts
  - business-account live `drive search` proof explicitly blocked pending test credentials
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface is broad across `internal/cli/*_test.go` plus `e2e/cli_commands_e2e_test.go`, `e2e/output_validation_e2e_test.go`, and `e2e/w7_live_caller_proof_e2e_test.go`
  - Explicit comment claims already exist for many command outputs: `R-1.1.1`, `R-1.2.4`, `R-1.3.4`, `R-1.4.3`, `R-1.5.1`, `R-1.6`, `R-1.7.1`, `R-1.8.1`, `R-1.9`, `R-1.9.4`, `R-3.1.4`, `R-3.1.5`, `R-3.1.6`, `R-2.3.7`, `R-2.3.8`, `R-2.3.9`, `R-2.3.10`, `R-2.7.1`, `R-6.2.8`, and `R-6.6.11`
  - CLI output is likely one of the easier places for a test to look busy while only checking superficial strings, so the body audit here should favor semantic assertions over formatting-only checks
- Body-audit notes from `internal/cli/{auth,drive,services}_test.go`, `internal/cli/auth.go`, `internal/cli/drive.go`, and `internal/cli/account_catalog.go`:
  - `drive remove --purge` had a real production boundary bug: it reused `purgeSingleDrive`, which also deleted the account profile file even though `R-3.3.8` preserves the logged-in account and the CLI account read model depends on `account_*.json` for offline identity metadata. The fix narrows `purgeSingleDrive` to drive-owned state only; `logout --purge` still removes account profiles through the account-owned `purgeOrphanedFiles()` path.
  - New caller-level tests now prove both sides of that contract. `TestDriveService_RunRemove_PurgePreservesAccountProfile` proves drive purge removes config/state/metadata while preserving token + account profile and keeping the offline account catalog usable. `TestAuthService_RunLogout_PurgeRemovesAccountProfile` proves logout purge still removes token, state DB, metadata, and the account profile while leaving sync directories untouched.
  - The old helper-level `purgeSingleDrive` test was also too shallow to catch this regression. It now asserts the account profile survives drive-scoped purge instead of disappearing silently.
  - Direct file-command independence from sync-store health also had a real user-facing leak: the authenticated-success proof hook already treated auth-scope cleanup as best-effort, but it logged sync-store repair failures at `Warn`, so a successful `ls` or `rm` could still print sync-database warnings. The hook now logs repair failures at `Debug`, and caller-level `ls` / `rm` tests prove successful direct API commands stay quiet even when the discovered state DB is corrupt.
  - Plain logout caller coverage was thinner than purge coverage. `TestAuthService_RunLogout_PreservesOfflineState` now proves the non-purge boundary directly: token + config are removed, but the state DB, drive metadata, account profile, and sync directory survive, and logout also clears persisted `auth:account` scope blocks from the preserved state DB.
  - `whoami` had a real caller-boundary bug in drive matching. The authenticated path used `MatchDrive`, but it swallowed those errors and fell through to offline auth-required / not-logged-in behavior. Invalid `--drive` selectors and the multi-drive no-selector ambiguity could therefore report the wrong outcome. The fix only soft-skips matching when there are no configured drives at all; otherwise `whoami` now surfaces the real drive-selection error.
  - New caller-level `whoami` tests now prove both sides of that boundary: orphaned-profile-only runs still emit `accounts_requiring_auth` JSON after logout-style local state loss, while invalid or ambiguous drive selection returns the expected `MatchDrive` error.
  - `drive list` now has direct caller-level auth-required coverage rather than only helper and print-shape coverage. New text and JSON service tests prove that an invalid saved login on a configured drive marks the configured row as `required` and emits the corresponding `accounts_requiring_auth` entry with the right reason and retained state-database count.
  - `drive search` also had a real caller-boundary bug in auth projection. The service pulled auth-required business accounts only from configured catalog entries, so orphaned or token-discovered business accounts with invalid saved login fell through to the misleading "no business account found" error instead of surfacing `accounts_requiring_auth`. The fix makes `drive search` use the shared account catalog for all matching business accounts, regardless of whether the drive is configured.
  - New caller-level `drive search` tests now prove both text and JSON behavior for that boundary, including the `--account` filtered path: a business account with invalid saved login but no config still appears as an auth-required result with the retained state-database count.
  - `shared` did not show the same earlier auth-projection bug, but the new live E2E pass did expose a different real production gap: on the current personal recipient account, `search(q='*')` succeeded yet returned no usable shared-item identities, so the CLI rendered an empty shared list even though `sharedWithMe` still returned a shared item. The fix broadens the fallback rule so shared discovery now retries `sharedWithMe` when search succeeds but yields no item with both `remoteDriveID` and `remoteItemID`.
  - New caller-level `shared` tests now prove auth-required projection in both text and JSON for an unconfigured account with invalid saved login, including the `--account` filtered JSON path.
  - The live logout pass also exposed a second real caller-boundary bug: `whoami` correctly surfaced preserved orphaned profiles after plain logout, but `drive list` only projected `accounts_requiring_auth` for configured drives and silently dropped the same account. The fix makes `drive list` project auth-required accounts from the whole shared account catalog, so preserved post-logout profiles stay visible until re-login.
  - New fast E2E coverage now proves the live path end-to-end: plain logout removes the token/config section while preserving the offline account catalog that still appears in `whoami` and `drive list`, and `shared --json` on the current recipient account now returns live shared items instead of an empty list.
  - `drive search` text output was also slightly misleading when every matching business account required auth and no actual sites could be searched: it printed an empty "SharePoint sites matching ..." section. The text path now keeps the auth-required section and follows it with an explicit no-results message for searchable accounts only.
  - Remaining blocked lane: live `drive search` proof still needs a real business test account in `.env`/CI (`ONEDRIVE_TEST_DRIVE` or `ONEDRIVE_TEST_DRIVE_2` set to a `business:` canonical ID and included in `ONEDRIVE_ALLOWED_TEST_ACCOUNTS`) with searchable SharePoint content.

| Contract / invariant | Evidence | Verdict | Notes |
|---|---|---|---|
| `drive remove --purge` preserves account-owned token/profile state while deleting only drive-owned config/state/metadata | `REQ+DESIGN+CODE+BODY` | `proven` | Fixed real production bug in `purgeSingleDrive`; caller-level service test now kills accidental over-deletion |
| `logout --purge` removes account-owned token/profile state in addition to drive-owned state | `REQ+DESIGN+BODY` | `proven` | Caller-level logout purge test now proves the destructive boundary separately from drive-scoped purge |
| plain `logout` removes token/config state while preserving state DBs, drive metadata, account profiles, and sync directories | `REQ+DESIGN+BODY` | `proven` | Caller-level logout test now proves the non-purge boundary and the preserved-state auth-scope clear path |
| `whoami` surfaces invalid or ambiguous drive selection instead of silently degrading to offline fallback output | `REQ+DESIGN+CODE+BODY` | `proven` | Fixed real production bug by soft-skipping authenticated lookup only when there are no configured drives |
| `whoami` still emits offline `accounts_requiring_auth` output when only orphaned local account state remains | `REQ+DESIGN+BODY` | `proven` | Caller-level JSON test now proves read-model orchestration, not just print helpers |
| `drive list` surfaces configured-drive auth-required state in both text and JSON output | `REQ+DESIGN+BODY` | `proven` | Caller-level tests now prove service-owned auth projection and emitted schema/section text |
| plain `logout` still leaves the preserved account visible in `drive list` `accounts_requiring_auth` output | `REQ+DESIGN+CODE+BODY` | `proven` | Fixed real production bug in `runList`: auth-required projection now uses the whole account catalog, not only configured drives |
| `drive search` surfaces auth-required business accounts from the shared catalog even when no matching business drive is configured | `REQ+DESIGN+CODE+BODY` | `proven` | Fixed real production bug in search auth projection; caller-level text and JSON tests now kill the misleading "no business account found" fallback for orphaned/token-discovered business accounts |
| `shared` surfaces auth-required accounts from the shared catalog in both text and JSON output | `REQ+DESIGN+CODE+BODY` | `proven` | Caller-level tests now prove `shared` reports unconfigured auth-required accounts and the service now reuses `accountReadModelService` instead of rebuilding the catalog inline |
| `shared` live discovery falls back when search succeeds but returns no usable shared-item identities | `REQ+DESIGN+CODE+BODY` | `proven` | Fixed real production bug from live recipient testing: `search(q='*')` returned no usable shared identities while `sharedWithMe` still returned items |
| direct API file commands remain successful and user-quiet when auth-proof cleanup cannot open a sync DB | `REQ+DESIGN+CODE+BODY` | `proven` | `authProofRecorder` now keeps proof-repair failures at debug level; caller-level `ls` and `rm` tests prove no user-visible sync-store warning leak |

### W8. Configuration Discovery, Validation Tiers, And Token Resolution

- Requirements and invariants:
  - `R-4.1`
  - `R-4.2`
  - `R-4.3`
  - `R-4.4`
  - `R-4.8.1` through `R-4.8.6`
  - `R-4.9.2`, `R-4.9.3`
  - `R-3.4.1`
  - `R-3.4.3`
  - `R-6.2.9`
- Ideal unit coverage:
  - path resolution, env overrides, display-name selection, token resolution, and size parsing
  - validation helpers prove exact rejection reasons for missing, invalid, or out-of-range config
  - write helpers preserve intended structure and unknown fields according to contract
- Ideal integration coverage:
  - load/write round-trips preserve semantic configuration state
  - validation tiers distinguish command classes correctly
  - sync config requires absolute and creatable `sync_dir`, while non-sync commands do not
  - token lookup follows the documented override chain without ambiguity
- Ideal e2e coverage:
  - CLI commands behave correctly with minimal valid config, fully populated config, and environment overrides
  - auto-created config files land in the expected locations with correct defaults
- Mandatory failure injection:
  - relative and non-creatable sync paths
  - missing drive sections
  - unknown fields and malformed TOML
  - conflicting override sources
- Kill switches that must fail tests:
  - config precedence order is inverted
  - write path drops required fields or silently rewrites unrelated content
  - sync validation accepts an invalid `sync_dir`
  - token resolution ignores an explicit override and falls back to stale state
- Audit status:
  - ideal model drafted
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface is rich across `internal/config/{load,write,validate,paths,env,holder,drive,token_resolution}_test.go` and related package tests
  - Explicit comment claims already exist for `R-4.1.1`, `R-4.1.2`, `R-4.1.3`, `R-4.2.1`, `R-4.2.2`, `R-4.3`, `R-4.4.1`, `R-4.4.2`, `R-4.8.4`, `R-4.8.6`, `R-3.4.1`, `R-3.4.3`, and `R-6.2.9`
  - No explicit claim surfaced yet for `R-4.8.1`, `R-4.8.2`, `R-4.8.3`, `R-4.9.2`, or `R-4.9.3`
  - This may partly be a traceability gap rather than a test gap, but the body audit should confirm whether validation-tier coverage is as complete as the design doc claims

### W9. Drive Identity And Shared-Drive Discovery Semantics

- Requirements and invariants:
  - `R-3.2`
  - `R-3.3`
  - `R-3.5`
  - `R-3.6.1`, `R-3.6.2`, `R-3.6.3`
  - `R-6.7.2`
  - planned placeholders: `R-3.6.4`, `R-3.6.5`
- Ideal unit coverage:
  - drive ID canonicalization and equality across casing/truncation variants
  - item-key construction and shared-drive identity extraction
  - drive selector matching and ambiguity handling
- Ideal integration coverage:
  - CLI drive listing and selection prove display naming, matching, and config interaction
  - shared-drive discovery handles shortcuts and shared-folder surfaces without identity drift
  - canonical IDs stay stable across all API entry points
- Ideal e2e coverage:
  - drive listing against live Personal and shared-drive setups
  - shared-folder sync and `drive` command workflows behave correctly with real Graph identities
- Mandatory failure injection:
  - inconsistent drive ID casing/truncation
  - ambiguous drive-name matches
  - shared item identity with partial metadata
- Kill switches that must fail tests:
  - two canonical-equivalent drive IDs compare unequal
  - drive selection accepts an ambiguous match
  - shared item display falls back to unstable or opaque identifiers when better identity exists
- Audit status:
  - ideal model drafted
- Claim mapping snapshot from filenames and `// Validates:` only:
  - Candidate test surface includes `internal/driveid/{canonical,id,itemkey,shared,edge}_test.go`, `internal/cli/drive_test.go`, and `e2e/drive_list*_test.go` plus shared-sync e2e coverage
  - Explicit comment claims already exist for `R-3.2.1` through `R-3.2.4`, `R-3.3.2` through `R-3.3.11`, `R-3.5.1`, `R-3.6.1`, `R-3.6.2`, `R-3.6.3`, and `R-6.7.2`
  - This currently looks like one of the healthiest traceability areas in the repo from metadata alone
  - Planned items `R-3.6.4` and `R-3.6.5` should stay parked until implemented

## Archived Recovery Reconciliation

- `2026-04-04`: reconciled the externally archived recovery artifacts that had been restored after the accidental cleanup:
  - `/Users/tonimelisma/Development/onedrive-go-stale-archive/refactor-remote-403-ephemeral-scope.bundle`
  - `/Users/tonimelisma/Development/onedrive-go-stale-archive/refactor-trial-result-policy-working.patch`
- `remote-403` conclusion:
  - the meaningful caller-level coverage from the archived branch is already present on `main` under the current account-catalog and auth-health shapes:
    - business-account search selection is now covered by `TestSearchableBusinessTokenIDs_*` in `internal/cli/drive_test.go`
    - drive-list auth-required projection is covered by `TestBuildConfiguredAuthRequirements_UsesOnlyAccountsNeedingAuth` and `TestAnnotateConfiguredDriveAuth_AndPrintSections` in `internal/cli/drive_test.go`
    - auth-health copy/merge behavior is covered by `TestMergeAuthRequirements_PrefersExistingFieldsAndSorts` and `TestAuthReasonTextAndAction_ReturnExpectedStrings` in `internal/cli/auth_health_test.go`
    - grouped status issue counting is covered by `TestQuerySyncState_CountsAuthAndRemoteBlockedScopesAsIssues` in `internal/cli/status_test.go`
  - the archived `issues_service` history-empty helper shape was superseded by later command/service refactors and was intentionally not replayed
- `trial-result-policy` conclusion:
  - the production refactor hunks in the archived dirty patch were older than the current `main` sync-engine architecture and would have reverted newer runtime ownership/result-routing work if replayed verbatim
  - the substantive behavioral proof from that patch is already on `main`:
    - unauthorized termination and trial-preserve routing tests are present in `internal/sync/engine_result_scope_test.go`
    - no-candidate / missing-state / skipped-candidate preserve semantics are present in `internal/sync/engine_single_owner_test.go`
    - scanner-resolved actionable cleanup coverage is present in `internal/sync/engine_result_scope_test.go`
    - governing retry/scope-preserve language is already present in `spec/design/retry.md`, `spec/design/sync-engine.md`, and `spec/requirements/sync.md`
- Reconciliation verdict:
  - no remaining production-code delta from the archived artifacts needed to be replayed onto `main`
  - the remaining useful integration work was traceability only, so the equivalent current tests were tagged directly and this note records the mapping for future audit/recovery work

## Next Moves

1. Continue W6 and W9 deep reconciliation:
   - pagination / transport-split body audit
   - shared-drive identity fallback details
2. When business-account credentials become available, add the blocked live W7 `drive search` proof:
   - set `ONEDRIVE_TEST_DRIVE` or `ONEDRIVE_TEST_DRIVE_2` to a `business:` canonical ID
   - include it in `ONEDRIVE_ALLOWED_TEST_ACCOUNTS`
   - ensure the account has searchable SharePoint content
