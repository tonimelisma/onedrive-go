# Increment Metrics

Tracking quantitative metrics across increments to spot trends and improve process.

Detailed per-increment metrics from earlier phases archived in [docs/archive/metrics-phases-1-4.md](archive/metrics-phases-1-4.md).

---

## Trend Summary

| Increment | Agents | Top-ups | Deviations | Coverage Δ |
|-----------|:------:|:-------:|:----------:|:----------:|
| Phase 1 (1.7+1.8+2.2) | 3 | 0* | 3 | +0.4% |
| Phase 3 (3.3) | 2 | 0 | 1 | +0.8% (config) |
| Phase 3.5 (3.5.2) | 4 | 7 | 3 | -1.6% (total) |
| Phase 4.1 (3.5.2+4.1) | 6 | 2 | 2 | -2.5% (total) |
| Foundation Hardening | 3 | 1 | 0 | +0.4% (total) |
| Phase 4 Wave 1 (4.1-4.4+B-040) | 5 | 8 | 5 | +5.2% (total) |
| Phase 4 Wave 2 (4.5-4.6) | 2 | 1 | 4 | +2.5% (total) |
| Post-Wave 2 Top-Up | 2 | 0 | 2 | +0.0% (total) |
| Pre-4.7 Coverage Hardening | 2 | 1 | 2 | +1.1% (total) |
| Pre-4.7 Foundation Cleanup | 0 | 1 | 0 | 0.0% (no change) |
| Phase 4.8 Conflict Handler | 0 | 2 | 1 | +0.0% (sync 91.3%→91.2%) |

*\* Review not performed — process gap. Subsequent increments will have accurate top-up counts.*

---

## Latest Increment

### Phase 4.8: Conflict Handler — 2026-02-21

| Metric | Value |
|--------|-------|
| **Agent count** | 0 (orchestrator-only) |
| **Wave count** | 1 |
| **PR count** | 1 (#57) |
| **Coverage before** | 79.3% (total), 91.3% (sync) |
| **Coverage after** | 79.5% (total), 91.2% (sync) |
| **Top-up fix count** | 2 (dotfile handling for generateConflictPath; dogsled + exhaustive lint fixes) |
| **Agent deviation count** | 1 (Go's filepath.Ext behavior for dotfiles differs from plan assumption — required conflictStemExt helper) |
| **CI failures** | 0 |
| **Wall-clock time** | ~30 min |
| **New tests** | 17 (conflict_test.go) + updated reconciler/executor tests |
| **Lines changed** | +713 / -30 |

---

### Phase 4.7: Executor — 2026-02-20

| Metric | Value |
|--------|-------|
| **Agent count** | 0 (orchestrator-only) |
| **Wave count** | 1 |
| **PR count** | 1 (#56) |
| **Coverage before** | 78.5% (total), 91.7% (sync) |
| **Coverage after** | 79.3% (total), 91.3% (sync) |
| **Top-up fix count** | 4 (dupl→dispatchPhase refactor; removed unused graphChunkAlignment constant; misspell "cancelled"→"canceled"; errcheck Checkpoint handling) |
| **Agent deviation count** | 2 (GetItemByPath added to ExecutorStore — missed from plan; TransferClient interface fix was plan-known but confirmed needed) |
| **CI failures** | 0 |
| **Wall-clock time** | ~45 min |

---

### Pre-4.7 Foundation Cleanup — 2026-02-20

| Metric | Value |
|--------|-------|
| **Agent count** | 0 (orchestrator-only) |
| **Wave count** | 1 |
| **PR count** | 1 (#55) |
| **Coverage before** | 78.5% (total), 91.7% (sync) |
| **Coverage after** | 78.5% (total), 91.7% (sync) |
| **Top-up fix count** | 1 (safetyListErrorStore unused-embed linter fix) |
| **Agent deviation count** | 0 |
| **CI failures** | 0 |
| **Wall-clock time** | ~20 min |

---

### Pre-4.7 Coverage Hardening — 2026-02-20

| Metric | Value |
|--------|-------|
| **Agent count** | 2 |
| **Wave count** | 1 |
| **PR count** | 2 (#53, #54) |
| **Coverage before** | 77.4% (total), 92.6% (graph), 90.0% (sync), 94.6% (config) |
| **Coverage after** | 78.5% (total), 94.2% (graph), 91.7% (sync), 95.1% (config) |
| **Top-up fix count** | 1 (missing LEARNINGS.md entries from Agent B) |
| **Agent deviation count** | 2 (graph 95% target missed at 94.2%; Agent B renamed TestClose_AlreadyClosed → TestClose_ThenQuery) |
| **CI failures** | 0 |
| **Wall-clock time** | ~25 min |

---

## Template for Next Increment

```
### [Increment Name] — [Date]

| Metric | Value |
|--------|-------|
| **Agent count** | |
| **Wave count** | |
| **PR count** | |
| **Coverage before** | |
| **Coverage after** | |
| **Top-up fix count** | |
| **Agent deviation count** | |
| **CI failures** | |
| **Wall-clock time** | |
```
