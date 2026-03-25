# Engine Event Loop Design Review

Comprehensive review of [engine-event-loop.md](file:///Users/tonimelisma/Development/onedrive-go/spec/design/engine-event-loop.md) against the codebase, requirements, and design specs.

---

## Overall Verdict

This is an **exceptionally well-reasoned design document** — one of the best refactoring plans I've reviewed. The defect analysis is grounded in actual code (with line numbers), the root cause analysis correctly identifies the two-goroutine architecture as the source of most problems, and the migration roadmap is carefully phased to minimize risk. The Appendix A (Design Provenance) is unusually honest about what was wrong in both predecessor plans.

That said, there are gaps, vagueness, risks, and edge cases the plan doesn't fully address. Below is a section-by-section breakdown.

---

## What I Agree With

### ✅ Core Thesis: Single Event Loop
The fundamental insight — that merging two goroutines eliminates the root cause of D1-D4 — is correct. The defects are real, verified against the codebase, and the single-goroutine approach structurally prevents their recurrence. This is the right architectural choice.

### ✅ Defect Inventory (Section 1)
Every defect is substantive and verified. The `admitAndDispatch` / `admitReady` duplication (D1) is the strongest motivation — it's a class of bug, not a single bug. D2 (semantically-wrong mutex sharing) is accurately characterized as a maintenance risk rather than a correctness bug. D3's ordering invariants are real and subtle. The inventory is honest about what's a defect vs what's a smell.

### ✅ Decision 1: No Async I/O for Permissions
This is the right call. The analysis is convincing: after eliminating `reobserve`, the remaining blocking calls are rare and bounded. The async alternative's complexity (2 result types, sealed interface, type-switch handler, deferred state updates, shutdown edge cases) vastly exceeds the cost of two bounded synchronous stalls. The "architecture doesn't preclude adding it later" point is correct and important.

### ✅ Decision 2: Eliminate reobserve
Deleting 85 lines of code that duplicates what the worker already does is a clear win. The ~200ms cost of the worker's first HTTP request is identical to reobserve's GET. The tradeoffs are honestly acknowledged (404-detection loss for fully-deleted scope items).

### ✅ Decision 4: Keep syncdispatch Mutexes
Defense-in-depth. The cost is ~100ns/call, invisible against a 2s debounce. No cross-package API changes. Correct decision.

### ✅ Decision 5: reconcileDoneCh
Clean, explicit, minimal. One buffered channel, one result struct. Far better than eventually-consistent shortcuts with a staleness window.

### ✅ Phase 2 (Preparatory Refactoring)
Brilliant sequencing. Phase 2 works entirely within the current two-goroutine architecture, reducing Phase 3's blast radius. If Phase 3 needs to be reverted, Phase 2's improvements stand independently. Each sub-phase (2a-2d) is independently deployable and testable.

### ✅ Appendix A
The Design Provenance section sets a high standard for transparency. Documenting what was rejected and why — especially "Points Where Both Plans Were Wrong" — is rare and valuable.

---

## Where I Disagree

### ❌ D9 Analysis: "Retrier hashes entire files synchronously" — misleading framing

The document says `runRetrierSweep` → `createEventFromDB` → `observeLocalFile` → `ComputeStableHash` blocks for "10+ seconds" on a 10GB file. While technically true, the framing implies this is a significant event loop stall.

**Reality check:** The retrier processes items in batches of `retryBatchSize = 1024`. Each item re-observed is typically either:
1. A small file (hash ≤10ms) — the vast majority of retries
2. Modified since last attempt (mtime changed) — O5's fast-path catches this
3. A large file — rare in retry scenarios

The O6 lazy hash fix is directionally correct (why hash when the worker re-hashes), but the document overstates the severity. A 10GB file in the retry queue is an unusual scenario — it typically means a large upload failed, and the retrier is re-checking whether to re-upload. The worker will re-hash anyway, so the fix eliminates redundant work, but the "10+ second block" scenario is unlikely in practice.

**My recommendation:** Keep O6 but reframe the motivation as "eliminating redundant work" rather than "preventing event loop stalls." The primary benefit is correctness (DRY), not performance.

### ❌ D5 Metrics: "3,633 lines with 88 methods" — not quite the right metric

The document says `engine.go` has 88 methods on `*Engine`. Looking at the outline, there are ~121 items total including standalone functions. While the file is certainly too large, the metric conflates file size with complexity. After the refactoring, many of these methods will be distributed across 12+ files — but the `*Engine` type will still have ~80+ methods. The file split addresses navigability but not the fundamental "god object" problem.

The plan acknowledges this in Non-Objectives ("Decompose the Engine struct into smaller types...separate work"). But it would be more honest to say: "The file split solves D5 (navigability) but does NOT solve the god object problem. Component extraction (Section 14) is the actual fix."

---

## Where Things Are Vague

### 🔶 The `admit()` function's interaction with `DepGraph.Register` / `WireDeps`

The plan shows `admit()` being called from two places:
1. `processBatch` → `dispatchActions` → `admit` (new actions)
2. `processResult` → `admit` (dependent actions from `depGraph.Complete`)

But Section 5 doesn't show how `dispatchBatchActions` changes. Currently, [dispatchBatchActions](file:///Users/tonimelisma/Development/onedrive-go/internal/sync/engine.go#L3256-L3309) does two-phase registration (Register + WireDeps), and WireDeps returns immediately-ready actions. How does `admit()` integrate with this flow?

Looking at the current code, `admitAndDispatch` is called with the ready actions from `WireDeps`. The plan's `admit()` replaces `admitAndDispatch`, returning `[]*TrackedAction` for the outbox. But `dispatchBatchActions` currently sends ready actions to `readyCh` via `admitAndDispatch`. After the refactoring, `dispatchBatchActions` would need to return `[]*TrackedAction` upward, which then gets appended to the outbox in `runEventLoop`.

This flow is implied but never spelled out. The `processBatch` method needs to return `[]*TrackedAction` (the plan mentions this at line 884), but the full chain from `processBatch` → `dispatchBatchActions` → `admit` → return to event loop isn't shown.

### 🔶 Bootstrap flow: how does initial dispatch integrate with the event loop?

The bootstrap code in Section 5 shows:

```go
dispatched := e.dispatchAndAdmit(ctx, plan)
return e.runEventLoop(ctx, &LoopConfig{...emptyCh: e.depGraph.WaitForEmpty()})
```

But `dispatchAndAdmit` returns `[]*TrackedAction`. How do these initial actions get to the workers? There's no outbox at this point — the event loop hasn't started yet. Does `dispatchAndAdmit` send directly to `readyCh`? That would be inconsistent with the event loop's outbox pattern. Does it seed the outbox? The plan doesn't clarify.

**My recommendation:** Show the complete bootstrap flow, including how the initial dispatched actions enter the worker pool before the event loop starts processing results.

### 🔶 `e.buf.Add(ev)` in `runTrialDispatch` — debounce delay for trials

The plan has trial dispatch sending events through the buffer:

```go
e.buf.Add(ev) // in runTrialDispatch
```

This means the trial event goes through the debounce window (2 seconds) before the planner sees it. For scope probes, this seems wasteful — you're adding 2 seconds of latency to every trial check. The current code (`reobserve` + `processTrialResult`) has no debounce delay.

**Is this intentional?** The plan doesn't discuss this latency tradeoff. For a scope that's been blocked for minutes, 2 seconds is noise. But it's a behavioral change from the current architecture that should be acknowledged.

### 🔶 `LoopConfig` channel semantics: who creates and closes these channels?

The plan shows `LoopConfig` with 10 channels but doesn't specify the lifecycle for each:
- Who creates `ready`? (Buffer's `FlushDebounced` output)
- Who closes `results`? (WorkerPool's `Stop`)
- Who closes `errs`? (observer count → all observers exit)
- What about `skippedCh`, `reconcileDoneCh`?

The current code has these lifecycles scattered across `initWatchInfra`, `startObservers`, and `RunWatch`. After the refactoring, the channel lifecycle document ([planned] in sync-execution.md) becomes more critical.

### 🔶 Error handling in `runEventLoop` return value

The event loop returns `error` but the shown code only returns `nil`. What about:
- Fatal errors from `processResult` (e.g., 401 Unauthorized)?
- Observer errors that should terminate the loop?
- DB errors during admission?

Currently, `resultFatal` is handled by recording the failure and continuing. The plan doesn't change this, but it's not explicit about whether the event loop should propagate fatal errors upward.

---

## Gaps

### 🕳️ Gap 1: `RunOnce` in Phase 4 is under-specified

Phase 4 says: "Refactor `RunOnce`'s `executePlan` to use `runEventLoop` with a one-shot `LoopConfig`. Delete `executePlan`'s separate drain goroutine spawn."

But `RunOnce` has fundamentally different semantics:
- It creates an **ephemeral** DepGraph, readyCh, and WorkerPool per call
- It waits for **all actions to complete** (not just emptyCh — it waits for `pool.Wait()`)
- It calls `pool.Stop()` → closes results channel → drain goroutine exits
- It then reads `resultStats()` for the report

With `runEventLoop`, the one-shot `LoopConfig` would have `results` set and all other channels nil. The loop would exit when `results` is closed (line 383: `if !ok { return nil }`). But the current `executePlan` calls `pool.Wait()` before `pool.Stop()`. With the event loop, the sequencing would be:
1. Call `runEventLoop` — blocks processing results
2. `pool.Wait()` — blocks until done (but this is in the event loop goroutine, so it can't also be outside it)

This needs a clear design. Does `pool.Stop()` happen outside the event loop? Does the event loop detect completion via `results` channel closure? The plan doesn't address this sequencing.

### 🕳️ Gap 2: `logFailureSummary` timing

Currently, `logFailureSummary()` is called at the end of `executePlan` — after all workers finish, after the drain goroutine exits. In the event loop, there's no "end of pass" concept for watch mode. The plan doesn't address when/how `logFailureSummary` is called in the new architecture.

For one-shot mode, it could be called when the event loop exits. For watch mode, it could be called at the end of each batch. But neither is specified.

### 🕳️ Gap 3: activeObs tracking for observer exit

The current [runWatchLoop](file:///Users/tonimelisma/Development/onedrive-go/internal/sync/engine.go#L1424-L1463) tracks `p.activeObs` and exits when all observers have died:

```go
case obsErr := <-p.errs:
    p.activeObs--
    if p.activeObs == 0 {
        return fmt.Errorf("sync: all observers exited")
    }
```

The plan's event loop has the `errs` case (line 418-421):
```go
case obsErr := <-c.errs:
    if e.handleObserverError(obsErr, c) {
        return nil
    }
```

But `handleObserverError` is never defined or specified. How does `activeObs` tracking work in the new architecture? Is it a field on `LoopConfig`? On `Engine`? The plan is silent on this.

### 🕳️ Gap 4: `postSyncHousekeeping` in watch mode

Currently called at the end of `bootstrapSync`. The plan's bootstrap code doesn't show where housekeeping runs. Does it run after the bootstrap event loop returns? Does the plan assume it's unchanged?

### 🕳️ Gap 5: `watchState` deletion and field migration

Phase 5 says "Remove `watchState` struct entirely. Fields that survive move to `Engine` (immutable config) or become event-loop-scoped locals."

But `watchState` has 17+ fields including `scopeGate`, `scopeState`, `buf`, `deleteCounter`, `lastDataVersion`, `remoteObs`, `localObs`, `lastPermRecheck`, `lastSummaryTotal`, `reconcileRunning`, `afterReconcileCommit`. The plan doesn't enumerate which fields move where. This is significant because:

1. Some fields are mode-specific (watch-only): `scopeGate`, `scopeState`, `buf`, `deleteCounter`
2. Some are lifecycle state: `remoteObs`, `localObs` (needed for `Close()`)
3. Some are timing state: `lastPermRecheck`, `lastSummaryTotal`

Moving 17 fields to different homes requires explicit enumeration. "Fields that survive" is too vague for a plan of this caliber.

### 🕳️ Gap 6: Relationship with TODO.md item #1 (synctypes scope logic moves)

TODO.md item #1 proposes moving `BlocksAction`, `ScopeKeyForStatus`, `IssueType`, `Humanize` from `synctypes` to `syncdispatch`. The event loop plan's `admit()` function calls `entry.scopeKey.BlocksAction(...)` (a method on `ScopeKey`). If item #1 moves this to a free function in `syncdispatch`, the `admit()` code would change to `syncdispatch.BlocksAction(entry.scopeKey, ...)`.

These two plans need to be sequenced. If item #1 goes first, the event loop plan's `admit()` pseudocode is stale. If the event loop goes first, item #1 needs to update the new code. The plan doesn't acknowledge this interaction.

### 🕳️ Gap 7: `watchPipeline` struct deletion (Phase 5) — but what is it?

Phase 5 says "Delete `watchPipeline` struct." But looking at the codebase, `watchPipeline` is referenced in [runWatchLoop](file:///Users/tonimelisma/Development/onedrive-go/internal/sync/engine.go#L1424) as `*watchPipeline`. The plan mentions it should be "replaced by `LoopConfig`" but doesn't show the struct definition or how its fields map to `LoopConfig` fields.

---

## Risks Not Fully Addressed

### ⚠️ Risk: 12-case select starvation under load

Risk 5 acknowledges select starvation but dismisses it with "simultaneous readiness is infrequent." This under-estimates the scenario:

During a reconciliation batch that produces 5,000+ events (24h reconciliation on a large drive), the buffer fills rapidly. The debounce fires, producing a large batch on `ready`. Processing this batch produces many actions that flow to workers. Workers complete rapidly (many are no-ops or fast operations), flooding `results`. Meanwhile, the retry timer fires (items from prior failures are due). The trial timer fires (a scope block trial is due). The reconciliation completes (`reconcileDoneCh` fires).

In this scenario, 5-6 select cases are simultaneously ready. Go's random selection means any one case might be delayed by 5-6 event loop iterations. The plan says "if profiling shows P99 result-processing latency exceeding 100ms, add an inner drain loop." But **this should be designed now, not deferred**. The inner drain pattern is:

```go
// After processing a batch, drain all pending results before returning to outer select
for {
    select {
    case r, ok := <-c.results:
        if !ok { return nil }
        dispatched := e.processResult(ctx, &r, c.bl)
        outbox = append(outbox, dispatched...)
    default:
        goto outerSelect
    }
}
```

Without this, the outbox can grow unboundedly during a burst — the event loop processes one result per select iteration while the outbox grows. The actor-with-outbox pattern mitigates deadlock but doesn't prevent outbox memory growth.

### ⚠️ Risk: Trial dispatch latency regression

As noted in the "vague" section, trial events now go through the 2-second debounce. This means:
1. Trial timer fires
2. `runTrialDispatch` creates event, calls `buf.Add(ev)`
3. 2 seconds of debounce
4. Planner sees the event
5. `admit` intercepts, marks as trial
6. Worker executes

Total latency: ~2.2 seconds minimum (vs ~0.2s with the current `reobserve` path). For a scope that's been blocked for 10 minutes, this is irrelevant. But for the initial trial (5-second interval), you're spending 40% of the interval in debounce. The second trial at 10s loses 20% to debounce. This compounds with the `time.Timer` reset overhead.

**Is this acceptable?** Probably, but the plan should explicitly acknowledge the latency delta and explain why it doesn't matter.

### ⚠️ Risk: `reconcileRunning` atomic stays

The plan eliminates all atomics except `reconcileRunning`. This atomic is a CAS guard accessed from two goroutines: the event loop (which triggers reconciliation) and the reconciliation goroutine (which sets it back to false). The plan says this is the only remaining cross-goroutine shared state.

But `reconcileRunning.Store(false)` in the reconciliation goroutine's `defer` happens BEFORE the send to `reconcileDoneCh`. There's a window where:
1. Reconciliation goroutine: completes work
2. `defer reconcileRunning.Store(false)` — sets to false
3. Event loop: `reconcileTick` fires, `reconcileRunning.CompareAndSwap(false, true)` succeeds — starts a NEW reconciliation before the event loop receives the previous result on `reconcileDoneCh`
4. Old reconciliation goroutine: sends result to `reconcileDoneCh`
5. New reconciliation goroutine: begins work, may race on DB writes

The plan's code shows `defer e.reconcileRunning.Store(false)` at the top of the goroutine, meaning it runs AFTER the channel send (defer is LIFO). Actually wait — the `defer` is before the `select` that sends, so in Go's defer stack ordering, `Store(false)` would run after the goroutine returns from the anonymous function... Let me re-read.

```go
go func() {
    defer e.reconcileRunning.Store(false)
    events, shortcuts, err := e.performReconciliation(ctx, bl)
    select {
    case e.reconcileDoneCh <- reconcileResult{...}:
    case <-ctx.Done():
    }
}()
```

The `defer` runs when the anonymous function returns — AFTER the select. So the ordering is: perform reconciliation → send result → Store(false). This is correct. The event loop won't start a new reconciliation until after the result is received AND the atomic is cleared. Since the send happens before the Store(false), and the event loop receives before checking CAS, the window is safe.

**Actually safe.** But worth a comment in the plan explaining why the defer ordering is critical.

### ⚠️ Risk: Test migration undercount

The plan estimates "~200 lines of changes across ~15 tests." But the `startDrainLoop` helper rewrite is non-trivial — it currently spawns a goroutine that calls `drainWorkerResults`. In the new architecture, tests need to either:
1. Call `runEventLoop` directly (requires constructing a full `LoopConfig`)
2. Create a test-specific helper that feeds events to the event loop

This is more than a "rewrite of a 17-line helper." It's a redesign of the test infrastructure for drain-loop-dependent tests. The plan acknowledges this implicitly but underestimates the effort.

---

## Edge Cases Not Fully Considered

### 🔸 Edge Case: Timer drain in concurrent select

The `armTrialTimer` code (Section 5) uses the canonical drain-before-reset pattern:
```go
if !e.trialTimer.Stop() {
    select {
    case <-e.trialTimer.C:
    default:
    }
}
e.trialTimer.Reset(delay)
```

This is correct for a single goroutine. But what if `armTrialTimer` is called from within a select case handler that was triggered BY the timer firing? Specifically:

1. `trialTimer.C` fires → event loop selects that case → calls `runTrialDispatch` → calls `armTrialTimer`
2. Inside `armTrialTimer`: `e.trialTimer.Stop()` returns false (timer already fired and was consumed by the select)
3. The drain `select { case <-e.trialTimer.C: default: }` takes the `default` case (channel already drained by the outer select)
4. `Reset(delay)` — correct

This works. But what about the retry timer? If `armRetryTimer` is called from `processResult` (which is called from the `results` case), there's no issue — the retry timer wasn't the trigger. But `armTrialTimer` IS called from `runTrialDispatch` which IS triggered by the trial timer. The drain-before-reset is safe here because the outer select already consumed the value. Worth a comment.

### 🔸 Edge Case: outbox ordering under scope admission

`admit()` returns actions in order. The outbox appends them. The event loop sends them one at a time via the outbox case. But what if:

1. Batch produces actions [A, B, C]
2. `admit()` returns [A, C] (B is scope-blocked)
3. outbox = [A, C]
4. Worker result for unrelated action D arrives → `processResult` returns [E, F]
5. outbox = [A, C, E, F]
6. Actions sent to workers in order: A, C, E, F

Is this ordering correct? The DepGraph handles dependencies — the outbox ordering is just "ready actions in order of discovery." There's no semantic requirement on outbox order. But this is a behavioral change from the current architecture where `admitAndDispatch` sends directly to `readyCh` (FIFO channel, order preserved). The plan should confirm that outbox ordering doesn't matter for correctness.

### 🔸 Edge Case: `observeLocalFile` behavior change with `skipHash`

O6 proposes `observeLocalFile` gains a `skipHash` parameter. This function is also called by the scanner and by `createEventFromDB`. The retrier path passes `skipHash=true`, producing an event with an empty hash. This event flows through the buffer and planner.

**What happens if the planner sees an event with an empty hash and a non-empty baseline hash?** From the plan: `"" != baseline.LocalHash → planner generates upload → correct`.

But what if `baseline.LocalHash` is also empty? (Zero-byte files have no hash.) Then `"" == "" → planner says "no change"`. Is this correct? If a zero-byte file's upload failed and is being retried, the planner would say "no change" and the retry would be silently dropped. The `isFailureResolved` check in the retrier should catch this case — but the plan doesn't analyze it.

### 🔸 Edge Case: `reconcileDoneCh` buffer of 1 — what if reconciliation completes while another is starting?

The plan says `reconcileDoneCh` is buffered(1). The CAS guard prevents overlapping reconciliations. But what if:

1. Reconciliation completes, sends result to `reconcileDoneCh` (buffer holds 1 item)
2. Event loop is busy processing a large batch — hasn't read `reconcileDoneCh` yet
3. Reconciliation tick fires — CAS succeeds (atomic was already cleared by defer)
4. Wait — no, the defer clears the atomic AFTER the send. So step 3 can't happen until after the send.

But: what if the SEND blocks? The send has a `ctx.Done()` fallback. If the context isn't canceled and the channel buffer is already full (from a previous unreceived result), the send blocks. But buffered(1) can only hold 1 item, and the CAS prevents a second reconciliation from starting until the first completes.

Actually, the CAS prevents a second reconciliation from starting AT ALL until `reconcileRunning.Store(false)` runs — which only happens after the send completes. So the channel can never have more than 1 item. The buffering of 1 is exactly right. 

---

## Downstream Impacts

### 📊 Impact on TODO.md items

| TODO Item | Impact | Action Needed |
|-----------|--------|---------------|
| #1 synctypes scope logic | `admit()` calls `BlocksAction()` as a method. If scope logic moves to `syncdispatch`, `admit()` needs updating. | Sequence this AFTER event loop, or update `admit()` pseudocode. |
| #4 synctypes imports | `EngineConfig` move to `internal/sync/` doesn't conflict. | No interaction. |
| #5 `NewBaselineForTest` | No interaction. | None. |
| #6 worker_test.go imports | No interaction. | None. |

### 📊 Impact on existing design docs

| Design Doc | Impact |
|------------|--------|
| [sync-engine.md](file:///Users/tonimelisma/Development/onedrive-go/spec/design/sync-engine.md) | Major rewrite. watchState, drain-loop retrier, trial dispatch sections all change. |
| [sync-execution.md](file:///Users/tonimelisma/Development/onedrive-go/spec/design/sync-execution.md) | Minor: note DepGraph/ScopeGate keep internal mutexes. |
| [sync-observation.md](file:///Users/tonimelisma/Development/onedrive-go/spec/design/sync-observation.md) | Minor: note permissionCache → plain map. |

### 📊 Impact on test infrastructure

The `startDrainLoop` helper is used by tests that need to feed worker results and observe engine state. After the refactoring, these tests need a new pattern. The plan should provide a `startEventLoop(t *testing.T, cfg *LoopConfig)` test helper that:

1. Creates a results channel
2. Populates a minimal `LoopConfig`
3. Runs `runEventLoop` in a goroutine
4. Returns a `func()` to stop it (close results channel)

This helper would replace `startDrainLoop` and be reusable across all test files that interact with the event loop.

---

## Improvement Suggestions

### 1. Add an inner drain loop for results (preemptive, not reactive)

Don't wait for P99 latency regression. The inner drain loop is ~5 lines, zero cost in the common case, and prevents outbox growth during bursts:

```go
case r, ok := <-c.results:
    if !ok { return nil }
    dispatched := e.processResult(ctx, &r, c.bl)
    outbox = append(outbox, dispatched...)
    // Drain any immediately available results to prevent outbox growth
    for {
        select {
        case r2, ok2 := <-c.results:
            if !ok2 { return nil }
            d2 := e.processResult(ctx, &r2, c.bl)
            outbox = append(outbox, d2...)
        default:
            break // no more immediately available
        }
    }
```

### 2. Explicitly document the trial debounce latency tradeoff

Add a paragraph to Decision 2 acknowledging the 2-second debounce penalty on trial dispatch and explaining why it's acceptable (scope blocks typically last 5+ minutes; the 2s is noise).

### 3. Enumerate watchState field migration in Phase 5

Don't say "fields that survive move to Engine." List each field and its destination.

### 4. Specify `handleObserverError` behavior

Define the function, including `activeObs` tracking logic.

### 5. Specify RunOnce integration (Phase 4) in more detail

Show the complete revised `RunOnce` flow with `runEventLoop`, including how the event loop detects completion and how `pool.Wait()`/`pool.Stop()` interact with the loop.

### 6. Add a `LoopConfig` invariant check

```go
func (c *LoopConfig) validate() {
    if c.results == nil { panic("LoopConfig: results channel is required") }
    if c.bl == nil { panic("LoopConfig: baseline is required") }
}
```

Compile-time prevention of misconfigured loops.

### 7. Consider `FlushImmediate` for trial events

Instead of going through the 2-second debounce, trial events could bypass it:
```go
e.buf.AddImmediate(ev) // no debounce for trial events
```

This would preserve the current trial dispatch latency (~200ms) without needing `reobserve`. But it requires a new buffer API.

---

## Summary Assessment

| Aspect | Rating | Notes |
|--------|--------|-------|
| Problem analysis | ⭐⭐⭐⭐⭐ | Every defect verified, root cause correctly identified |
| Solution design | ⭐⭐⭐⭐ | Correct approach, well-analyzed tradeoffs, some gaps in edge cases |
| Migration plan | ⭐⭐⭐⭐⭐ | Phasing is excellent — Phase 2 is independently valuable |
| Risk analysis | ⭐⭐⭐½ | Timer gotcha and pipeline overlap covered; select starvation underestimated |
| Completeness | ⭐⭐⭐½ | Bootstrap flow, RunOnce integration, watchState migration, observer error handling under-specified |
| Honesty | ⭐⭐⭐⭐⭐ | Appendix A sets a gold standard for design doc transparency |

**Overall: This plan is ready to execute with the gaps filled in.** The gaps are all tractable — none require rethinking the architecture. The core thesis is sound, the defect analysis is rigorous, and the phasing is exemplary.
