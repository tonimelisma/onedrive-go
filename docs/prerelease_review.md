# Robustness Improvement Program — Investigation Plan

## Context

This is a pre-launch audit program structured as a series of increments. The codebase is functionally complete with clean architecture (no dependency cycles, no global mutable state, good interface design, ~35k lines of tests). The goal is to systematically surface and fix every category of issue — not through ad-hoc code review, but through structured methodologies that provide confidence of completeness.

**Guiding principle**: For each dimension, we define a *finite, enumerable* scope and a verification criterion that tells us when we're done. "Review the code" is not a methodology. "For every `filepath.Join(syncRoot, ...)` call, verify containment" is.

---

## Program Structure: 11 Increments

Each increment targets a specific failure class using a specific analysis technique. Ordered by severity (data integrity/security first) and dependency (concurrency before state machines, invariants before tests).

```
Inc 1:  Security — Path Containment & Input Validation          [CRITICAL]
Inc 2:  Resource Bounds & DoS Resistance                        [HIGH]
Inc 3:  Concurrency Correctness                                 [HIGH]
Inc 4:  State Machine Invariants & Protocol Correctness         [HIGH]
Inc 5:  Error Handling Completeness                             [MEDIUM]
Inc 6:  API Contract Fidelity & Defensive Parsing               [MEDIUM]
Inc 7:  Credential Safety & Secret Management                   [MEDIUM]
Inc 8:  Encapsulation & API Surface Hardening                   [MEDIUM]
Inc 9:  Test Completeness & Quality                             [MEDIUM]
Inc 10: Operational Resilience & Fault Injection                [MEDIUM]
Inc 11: Architecture Re-evaluation                              [STRATEGIC]
```

Dependencies: Inc 3 before Inc 4 (concurrency must be sound before state machine analysis). Inc 4+5 before Inc 9 (invariants and error paths inform test design). Inc 10 before Inc 11 (deep codebase knowledge from all prior increments informs architecture decisions). Inc 11 last — it's a strategic review informed by everything learned in 1-10.

---

## Increment 1: Security — Path Containment & Input Validation

**Methodology**: Data flow tracing. Start from every filesystem write point (`os.Create`, `os.MkdirAll`, `os.Rename`, `os.Remove`, `os.Chtimes`, `os.WriteFile`) and trace the path argument backwards to its source. Verify containment within syncRoot.

**Known issues**:
- `executor.go:111`, `executor_delete.go:17`, `executor_transfer.go:22,87`, `executor_conflict.go:24` — all use `filepath.Join(e.syncRoot, action.Path)` with no containment check. A crafted `action.Path` like `../../etc/passwd` escapes the root.
- `observer_remote.go:449-484` — `materializePath` builds paths from API response data (parent chain walking). No validation that the result stays within sync root boundaries.
- Scanner validates local filenames (`isValidOneDriveName`) but remote API responses are NOT validated through the same checks.

**Deliverables**:
1. Path containment guard function (post-join `filepath.Rel` check), applied at every filesystem write point
2. Remote filename validation (reject `..`, `/`, `\`, null bytes, control chars) in `classifyAndConvert`
3. Symlink detection at executor write points (prevent following symlinks that escape syncRoot)
4. Adversarial input tests for each guard
5. Written threat model document

**Verification**: Enumerate all `os.*` calls in production code. For each one, confirm the path argument passes through the containment guard. This is a finite checklist.

**Key files**: `internal/sync/executor.go`, `executor_delete.go`, `executor_transfer.go`, `executor_conflict.go`, `observer_remote.go`, `scanner.go`

---

## Increment 2: Resource Bounds & DoS Resistance

**Methodology**: Boundary analysis. For each function accepting external input, determine worst-case resource consumption (memory, file descriptors, CPU, disk). Verify all allocations are bounded.

**Known issues**:
- `graph/client.go:149,323` — `io.ReadAll(resp.Body)` on error responses, no size limit. Malicious server → OOM.
- `root.go:120` — Transfer HTTP client has `Timeout: 0`, relies on context. Stalled connection without context cancellation hangs forever.
- `observer_remote.go` — `maxObserverPages = 10000` × items per page = unbounded total item accumulation.
- `buffer.go` — `defaultBufferMaxPaths = 100_000` caps path count but not events-per-path.

**Deliverables**:
1. `io.LimitReader` on all error body reads (cap at e.g. 64 KiB)
2. Per-transfer timeout or connection-level deadline
3. Total item cap during delta enumeration
4. Per-path event cap in Buffer
5. Documentation of resource consumption guarantees for each component

**Verification**: Grep for `io.ReadAll`, `append`, `make(map`. For each, confirm a documented upper bound exists.

**Key files**: `internal/graph/client.go`, `internal/graph/upload.go`, `internal/sync/buffer.go`, `internal/sync/observer_remote.go`, `root.go`

---

## Increment 3: Concurrency Correctness

**Methodology**: Lock ordering analysis + channel lifecycle analysis. Enumerate all mutexes, document their acquisition order, verify no cycle. For channels, document create/close/read/write for each.

**Known issues**:
- `tracker.go:154` — Nested lock `mu` → `cyclesMu`. Verified safe but ordering contract is undocumented.
- `types.go ForEachPath` — Holds read lock during callback. Any callback that calls a Baseline write method → deadlock. Callers must be audited.
- `baseline.go Load` — Not mutex-guarded. Concurrent `Load` calls race on `m.baseline = b`. Currently prevented by architecture but is a latent bug.
- `buffer.go:119-126` — Lock released before notify channel send. Possible lost notification (benign for debounce but should be verified).

**Deliverables**:
1. Lock ordering contract document (every mutex, documented hierarchy)
2. Channel lifecycle document (every channel, who creates/closes/reads/writes)
3. Audit all `ForEachPath` callers for re-entrancy safety
4. `sync.Once` or mutex on `SyncStore.Load`
5. Targeted `-race` stress tests for DepTracker, Buffer, WorkerPool

**Verification**: Run full test suite with `-race` under CPU contention. Custom stress tests that hammer concurrent operations on shared state.

**Key files**: `internal/sync/tracker.go`, `internal/sync/types.go`, `internal/sync/baseline.go`, `internal/sync/buffer.go`, `internal/sync/worker.go`

---

## Increment 4: State Machine Invariants & Protocol Correctness

**Methodology**: Formal invariant specification. State the invariants each component must maintain. Verify each state transition preserves them. Check for invariant violations that only manifest in specific action sequences.

**Known issues**:
- Zero-byte files map to `NULL` in SQLite baseline — can't distinguish from "size unknown". Affects change detection for empty files.
- `conflictCopyPath` uses second-precision timestamps — two conflicts in the same second for the same file collide.
- `buildDependencies` must produce a DAG. If it produces a cycle, DepTracker deadlocks. No cycle detection exists.
- `applySingleOutcome` default case returns nil for unknown ActionTypes — silently drops outcomes.
- Delta token committed only after full cycle success. Verify this holds in watch mode with `CycleDone`.

**Deliverables**:
1. Written invariant specifications: baseline consistency, delta token lifecycle, action plan DAG property, conflict resolution atomicity
2. Cycle detection in `buildDependencies` (runtime assertion + test)
3. Sub-second uniqueness in `conflictCopyPath`
4. Fix zero-byte NULL conflation in baseline (use `sql.NullInt64{Valid: true, Int64: 0}` for zero)
5. Explicit error for unknown ActionType in `applySingleOutcome`
6. Property-based tests for planner with random input combinations

**Verification**: Write tests that exercise every invariant boundary. Use `testing/quick` or manual generators for planner property tests.

**Key files**: `internal/sync/baseline.go`, `internal/sync/planner.go`, `internal/sync/tracker.go`, `internal/sync/executor.go`, `internal/sync/executor_conflict.go`, `internal/sync/engine.go`

---

## Increment 5: Error Handling Completeness

**Methodology**: Error path enumeration. For each function, enumerate all error sources and trace propagation. Verify errors are handled, wrapped with `%w`, and never silently swallowed.

**Known issues**:
- 42+ bare `assert.NoError` final assertions in tests — could mask incorrect results.
- `upload.go:209,296` — `body, _ := io.ReadAll(resp.Body)` explicitly ignores read error.
- Deferred close errors on write paths are universally ignored.
- Scanner hash phase lacks panic recovery (unlike worker pool).

**Deliverables**:
1. Audit all `_ = ...` patterns in production code
2. Add value assertions after bare `assert.NoError` calls
3. Error return from deferred `Close` on write paths
4. Panic recovery in scanner hash phase
5. Documentation of error handling conventions (update LEARNINGS.md)

**Verification**: For each production function with >1 error path, verify at least one test exercises each path.

**Key files**: All production files. Focus on `internal/graph/upload.go`, `internal/sync/scanner.go`, `internal/driveops/transfer_manager.go`

---

## Increment 6: API Contract Fidelity & Defensive Parsing

**Methodology**: Fuzz testing of API response parsing. Generate valid-but-unexpected JSON responses and verify graceful handling.

**Known issues**:
- Delta items with missing `Name`, `ParentReference`, or `File`/`Folder` facets — are all accessed without nil guards?
- ETag format assumptions (opaque strings per RFC 7232 — what about quoted ETags?).
- `UploadSession.UploadURL` received from API with no URL validation (scheme, host).
- `NextExpectedRanges` parsing for upload resume — malformed ranges?

**Deliverables**:
1. Fuzz tests for `graph.Item` JSON parsing with missing/extra/malformed fields
2. Nil guards on all Item field accesses in observer_remote.go
3. Upload URL validation (HTTPS scheme, Microsoft domain)
4. NFC normalization idempotency test
5. Documentation of all Graph API response shape assumptions

**Verification**: Use `testing/quick` or manual fuzzing — no panics from any random JSON payload.

**Key files**: `internal/graph/types.go`, `internal/graph/items.go`, `internal/graph/upload.go`, `internal/graph/delta.go`, `internal/sync/observer_remote.go`

---

## Increment 7: Credential Safety & Secret Management

**Methodology**: Taint analysis. Track every variable holding a secret (token, pre-auth URL) from source to all sinks. Verify no secret reaches a log, error message, or world-readable file.

**Known issues**:
- `UploadURL` is a plain `string` without `LogValuer` protection (unlike `DownloadURL` which has it).
- `SessionRecord.SessionURL` stores pre-authenticated URL as plain string in JSON file.
- `GraphError.Message` includes API error body — could contain tokens in edge cases.

**Deliverables**:
1. `UploadURL` type with `slog.LogValuer` (matching `DownloadURL` pattern)
2. Audit all `slog.*` calls for potential secret leakage
3. Audit all error message strings for embedded secrets
4. Test that captures log output during a sync cycle and verifies no tokens/pre-auth URLs appear

**Verification**: Grep all `slog.*` calls. For each, verify no argument could contain a secret.

**Key files**: `internal/graph/types.go`, `internal/graph/upload.go`, `internal/graph/client.go`, `internal/driveops/session_store.go`

---

## Increment 8: Encapsulation & API Surface Hardening

**Methodology**: API surface minimization. For each exported symbol, determine if it must be exported. For mutable exported state, determine if it should be a function or unexported.

**Known issues**:
- `tokenfile.RequiredMetaKeys` — exported mutable `[]string` var. Any caller can modify it.
- `Baseline.ByPath`/`ByID` — exported mutable maps (documented for test convenience but production footgun).
- `graph.Client.Do`/`DoWithHeaders` — appear unused outside `graph/` package. Could be unexported.
- `sync` imports `graph` directly for error classification — coupling. Could be decoupled via error classifier interface.

**Deliverables**:
1. Convert exported mutable `var` slices to functions returning copies (or unexport)
2. Unexport `Baseline.ByPath`/`ByID`, provide test-only setup helpers
3. Unexport `Do`/`DoWithHeaders` if confirmed unused externally
4. Evaluate decoupling `sync` → `graph` error dependency via interface
5. Documentation of each package's public API contract

**Verification**: `go vet` + `golangci-lint` with strict export rules. Manual audit of every exported symbol.

**Key files**: `internal/tokenfile/tokenfile.go`, `internal/sync/types.go`, `internal/graph/client.go`, `internal/graph/auth.go`

---

## Increment 9: Test Completeness & Quality

**Methodology**: Coverage gap analysis + mutation testing. Identify untested code paths. For tested paths, verify tests would catch real bugs (not just "no error" checks).

**Known issues**:
- `migrations.go` has zero dedicated tests.
- 42+ bare `assert.NoError` final assertions (from Inc 5, may carry forward).
- No tests for: circular parent references in planner, dependency graph cycles, buffer overflow behavior, transfer manager resume with corrupt `.partial` files.
- Root package coverage at ~47% — CLI layer has thin unit tests.

**Deliverables**:
1. Migration test suite (fresh DB, intermediate versions, interrupted migration)
2. Planner property tests (random inputs, verify DAG invariant)
3. Buffer overflow test with drop metric verification
4. Transfer manager resume edge case tests (corrupt partial, changed remote, oversized partial)
5. Root package unit test expansion (target 60%+)

**Verification**: Run mutation testing to verify test quality. Measure whether tests detect injected bugs.

**Key files**: `internal/sync/migrations.go`, `internal/sync/planner.go`, `internal/sync/buffer.go`, `internal/driveops/transfer_manager.go`, root package test files

---

## Increment 10: Operational Resilience & Fault Injection

**Methodology**: Systematic fault injection. For every external dependency (filesystem, network, API, database), enumerate failure modes and verify graceful degradation.

**Known issues**:
- Disk full during baseline commit — SQLite transaction fails, but is in-memory cache consistent?
- Network partition during upload session — session resume after expiry (48h TTL).
- SIGTERM during active worker pool — graceful shutdown under active transfers.
- Clock skew — which comparisons are time-sensitive?
- inotify partial watch setup failure — are already-added watches cleaned up?

**Deliverables**:
1. Fault injection tests for: disk full (baseline commit, file download, token save), network partition (upload session resume), API throttling (sustained 429)
2. Graceful shutdown test under active worker pool
3. Clock skew resilience audit (document all time-sensitive comparisons)
4. inotify partial-watch cleanup verification
5. Documentation of degraded-mode behavior guarantees

**Verification**: Use injected error functions to simulate each failure. Verify no crashes, no data corruption, no goroutine leaks.

**Key files**: `internal/sync/baseline.go`, `internal/driveops/transfer_manager.go`, `internal/sync/engine.go`, `internal/sync/worker.go`, `internal/sync/observer_local.go`

---

## Increment 11: Architecture Re-evaluation

**Methodology**: Blank-slate design exercise. Armed with deep knowledge from increments 1-10, evaluate whether the current architecture is the one we'd build from scratch. Analyze structural decisions against actual complexity discovered during the audit.

**Key questions to answer**:

1. **`internal/sync/` at 8k lines in one package** — The "Pragmatic Flat" architecture puts Engine, Planner, Executor, Baseline, Buffer, DepTracker, WorkerPool, and both Observers in a single package. During increments 1-10 we'll have deep knowledge of the coupling between these types. Is the coupling genuinely tight enough to justify one package, or are there clean seam lines for sub-packages (e.g., `sync/plan`, `sync/exec`, `sync/observe`, `sync/baseline`)?

2. **`sync` → `graph` coupling** — The sync package imports `graph` directly for error classification (`graph.GraphError`, `graph.ErrThrottled`, etc.) and type references (`graph.Item`, `graph.DeltaPage`). Should these be decoupled via interfaces? Or is the coupling acceptable given that `graph` is the sole data source?

3. **Baseline storage abstraction** — The baseline is tightly coupled to SQLite via raw SQL. Should there be a storage interface (`BaselineStore`) that abstracts the persistence layer? This would enable testing with in-memory stores and potential future migration to different backends.

4. **Transfer architecture** — `TransferManager` in `driveops` is shared between CLI and sync engine. Is this the right boundary? Should transfers be a standalone package?

5. **Configuration flow** — `config.Holder` is shared between `SessionProvider` and `Orchestrator` with `sync.RWMutex`. Is there a cleaner way to propagate config changes (e.g., config reload channels)?

6. **CLI structure** — The root package contains all CLI commands as individual files. At 21 files / 4k lines, is this scaling well? Should commands be grouped (file-ops, sync-ops, account-ops)?

7. **Error type hierarchy** — `graph.GraphError` → sentinel errors → `sync` error classification. Is the two-tier classification the right design, or should there be a unified error taxonomy?

**Deliverables**:
1. Architecture assessment document — for each question above, a recommendation with rationale
2. Concrete refactoring proposals (if any) with effort estimates
3. Updated architecture diagram if changes are proposed
4. Prioritized list of structural changes, noting which are worth doing now vs. deferring

**Verification**: Any proposed changes must be validated against the invariants established in increments 3-4 and the test suite expanded in increment 9.

**Key files**: `internal/sync/` (all files), `internal/graph/errors.go`, `internal/driveops/`, `internal/config/holder.go`, root package

---

## Additional Dimensions (Not Full Increments)

These are addressed within the increments above or as ongoing concerns:

- **Performance profiling** (memory/CPU at scale) — can be a follow-up program after correctness is established
- **Dependency audit** (CVEs in third-party deps) — standard supply-chain concern, `govulncheck` as part of CI
- **Documentation completeness** — addressed incrementally within each increment's deliverables
- **No sync_dir overlap validation** between drives — address in Inc 4 (invariants) or Inc 6 (config validation)

---

## Execution Model

Each increment follows the standard CLAUDE.md development process: claim work → read context → worktree → TDD → code review checklist → Definition of Done. Each increment produces:
1. Code fixes (with tests written first)
2. Documentation (invariants, contracts, threat models)
3. A completion report with findings, fixes, and remaining concerns

Estimated scope: 11 increments. Increments 1-10 are fix-oriented (each a focused session). Increment 11 is strategic (architecture re-evaluation, informed by deep knowledge from 1-10). Some increments (1, 3, 4) are heavier; others (7, 8) are lighter. The program can be paused and resumed at any increment boundary.
