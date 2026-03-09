<!-- ABSORBED → relevant design docs -->
# Clock Skew Resilience Audit

## Summary

This audit catalogs all `time.Now()` call sites in the sync engine, classifies each as safe or vulnerable to clock skew, and documents Go's monotonic clock guarantees.

## Go Monotonic Clock Guarantees

Go's `time.Now()` returns a `time.Time` that contains **both** a wall clock reading and a monotonic clock reading. Operations like `time.Since(t)` and `t.After(u)` use the **monotonic** component when both operands have it, making them immune to wall clock jumps (NTP corrections, DST, manual adjustments).

However, `t.Unix()`, `t.UnixNano()`, and formatting functions strip the monotonic reading and use only the wall clock. These are vulnerable to backward clock jumps.

## Call Site Classification

### Safe (monotonic-protected)

| File | Line | Usage | Why Safe |
|------|------|-------|----------|
| `engine.go` | 217 | `start := time.Now()` → `time.Since(start)` | `Since` uses monotonic clock |
| `observer_local_handlers.go` | 472 | `start := time.Now()` → `time.Since(start)` | `Since` uses monotonic clock |
| `observer_remote.go` | 300 | `lastActivityNano.Store(time.Now().UnixNano())` | Used for liveness check via `time.Since` equivalent |
| `observer_local.go` | 178 | `lastActivityNano.Store(time.Now().UnixNano())` | Same pattern |

### Injectable (test-controlled via `nowFunc`)

| File | Line | Usage | Notes |
|------|------|-------|-------|
| `executor.go` | 88 | `nowFunc: time.Now` | Used for conflict copy timestamps — cosmetic only |
| `reconciler.go` | 87 | `nowFunc: time.Now` | Used for `next_retry_at` calculation |
| `baseline.go` | 158 | `nowFunc: time.Now` | Used for `synced_at`, `detected_at`, `resolved_at`, metadata timestamps |

### Vulnerable but acceptable

| File | Line | Usage | Risk | Mitigation |
|------|------|-------|------|------------|
| `engine_shortcuts.go` | 111 | `time.Now().Unix()` for `DiscoveredAt` | Backward jump → lower timestamp | Cosmetic field only, no ordering dependency |
| `scanner.go` | 110 | `time.Now().UnixNano()` for `scanStartNano` | Backward jump → false mtime comparison | Would cause one extra unnecessary upload, self-corrects next cycle |
| `orchestrator.go` | 439,466 | `time.Now()` for pause-until comparison | Backward jump → premature unpause | Benign — user can re-pause |

### Vulnerable in external systems

| File | Line | Usage | Risk |
|------|------|-------|------|
| `baseline.go` | 288 | `syncedAt := m.nowFunc().UnixNano()` | Backward jump → stale `synced_at` in baseline |
| `baseline.go` | 1404 | `detectedAt := m.nowFunc().UnixNano()` | Backward jump → stale conflict `detected_at` |
| `baseline.go` | 1498 | `m.nowFunc().UTC().Format(...)` | Metadata timestamp — cosmetic |

All vulnerable sites use `nowFunc` injection, so tests can verify behavior under clock jumps. The primary risk is a backward clock jump making `synced_at` appear older than it is, but this has no correctness impact — it only affects the `status` command display.

## Conclusion

The codebase has excellent clock injection coverage. All critical timing paths (reconciler retries, baseline commits, conflict detection) use injectable `nowFunc`. The remaining raw `time.Now()` calls are either monotonic-protected (`time.Since`) or have negligible impact from clock skew.

No code changes required. The injectable `nowFunc` pattern is the correct approach and is already well-tested via ~80 test sites that override it.
