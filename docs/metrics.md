# Increment Metrics

Tracking quantitative metrics across increments to spot trends and calibrate planning.

---

## Increment History

### Phase 1 Completion (1.7 + 1.8 + 2.2) — 2026-02-19

*Note: This increment predates the metrics framework. Data reconstructed from git log and agent outputs. Some fields are approximate.*

| Metric | Value |
|--------|-------|
| **Planned LOC** | ~1180 |
| **Actual LOC** | ~1170 (root.go 73, auth.go 166, format.go 80, format_test.go 111, files.go 493, e2e_test.go 188, items.go +60) |
| **Estimation accuracy** | 99% |
| **Agent count** | 3 |
| **Wave count** | 2 (Wave 1: 2 parallel, Wave 2: 1 sequential) |
| **PR count** | 3 (#15, #16, #17) |
| **Coverage before** | 74.0% (total), 92.2% (graph) |
| **Coverage after** | 74.4% (total), 92.3% (graph) |
| **Top-up fix count** | 0 (review was not performed — process gap identified) |
| **Agent deviation count** | 3 (gochecknoinits, dupl helpers, mousetrap dep) |
| **CI failures** | 0 |
| **Wall-clock time** | ~2 hours |

#### Per-Agent Breakdown

| Agent | Planned LOC | Actual LOC | Tests | Coverage | Deviations | LEARNINGS |
|-------|:-----------:|:----------:|:-----:|:--------:|:----------:|:---------:|
| A (graph paths) | 120 | ~120 | 5 | 92.3% | 1 (dupl helpers) | 0 (protocol gap) |
| B (CLI auth) | 600 | 430 | 6 | n/a (root pkg) | 2 (gochecknoinits, mousetrap) | 0 (protocol gap) |
| C (file ops + E2E) | 460 | 620 | 9 | n/a (root pkg + e2e) | 0 | 0 (protocol gap) |

#### Agent Scorecards (Retrospective — assessed after the fact)

**Agent A (Graph path methods)**

| Criterion | Rating | Notes |
|-----------|:------:|-------|
| Correctness | 5 | Clean implementation, proper error handling |
| Test quality | 4 | Good coverage, could add more edge cases |
| Code style | 5 | Good refactoring to shared helpers |
| Logging | 4 | Reuses existing logging from parent functions |
| Documentation | 2 | No LEARNINGS.md update |
| Plan adherence | 4 | One deviation (dupl), well-handled |
| Communication | 3 | Reported what was built but no observations |

**Agent B (CLI auth)**

| Criterion | Rating | Notes |
|-----------|:------:|-------|
| Correctness | 5 | Clean, proper stderr/stdout separation |
| Test quality | 4 | format_test.go solid, no auth command tests |
| Code style | 5 | Good constructor pattern, clean structure |
| Logging | 3 | CLI commands don't log (acceptable for Phase 1) |
| Documentation | 2 | No LEARNINGS.md update |
| Plan adherence | 4 | Two deviations, both improvements |
| Communication | 3 | Good summary, no process/architecture observations |

**Agent C (File ops + E2E)**

| Criterion | Rating | Notes |
|-----------|:------:|-------|
| Correctness | 5 | All commands work, E2E proves it |
| Test quality | 4 | E2E comprehensive, no unit tests for CLI functions |
| Code style | 4 | Clean, one unused helper (runCLIExpectError) |
| Logging | 3 | CLI commands don't log (acceptable for Phase 1) |
| Documentation | 2 | No LEARNINGS.md update |
| Plan adherence | 5 | Followed plan closely |
| Communication | 3 | Good summary, limited observations |

---

## Trend Summary

| Increment | Planned | Actual | Accuracy | Agents | Top-ups | Deviations | Coverage Δ |
|-----------|:-------:|:------:|:--------:|:------:|:-------:|:----------:|:----------:|
| Phase 1 (1.7+1.8+2.2) | 1180 | 1170 | 99% | 3 | 0* | 3 | +0.4% |

*\* Review not performed — process gap. Subsequent increments will have accurate top-up counts.*
