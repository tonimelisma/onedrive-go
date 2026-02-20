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
| Phase 3 (3.3) | 550 | 550 | 100% | 2 | 0 | 1 | +0.8% (config) |

*\* Review not performed — process gap. Subsequent increments will have accurate top-up counts.*

---

### Phase 3: Config Completion (3.3) — 2026-02-19

| Metric | Value |
|--------|-------|
| **Planned LOC** | ~550 |
| **Actual LOC** | ~550 (Wave 1: ~350 new, Wave 2: ~200 new, plus ~340 moved via file extraction) |
| **Estimation accuracy** | ~100% |
| **Agent count** | 2 (sequential waves) |
| **Wave count** | 2 (Wave 1: config enhance, Wave 2: CLI integration) |
| **PR count** | 2 (#19, #20) |
| **Coverage before** | 73.4% (total), 94.8% (config) |
| **Coverage after** | 73.4% (total), 95.6% (config) |
| **Top-up fix count** | 0 |
| **Agent deviation count** | 1 (errWriter pattern for show.go — improvement over plan) |
| **CI failures** | 0 |
| **Wall-clock time** | ~1.5 hours |

#### Per-Agent Breakdown

| Agent | Planned LOC | Actual LOC | Tests | Coverage | Deviations | LEARNINGS |
|-------|:-----------:|:----------:|:-----:|:--------:|:----------:|:---------:|
| Wave 1 (Config enhance) | 350 | ~350 | ~15 | 95.6% (config) | 1 (errWriter) | 0 |
| Wave 2 (CLI integration) | 200 | ~200 | 0 (manual) | n/a (root pkg) | 0 | 0 |

#### Agent Scorecards

**Wave 1 Agent (Config package enhancements)**

| Criterion | Rating | Notes |
|-----------|:------:|-------|
| Correctness | 5 | All code correct, no bugs found in review |
| Test quality | 5 | 95.6% coverage, error paths tested, table-driven |
| Code style | 5 | Clean errWriter pattern, consistent formatting |
| Logging | N/A | Config package doesn't do I/O logging |
| Documentation | 5 | Comprehensive godoc on all exported symbols |
| Plan adherence | 4 | One deviation (errWriter) — improvement |
| Communication | 4 | Clear summary with coverage numbers |

**Wave 2 Agent (CLI integration)**

| Criterion | Rating | Notes |
|-----------|:------:|-------|
| Correctness | 5 | PersistentPreRunE, config show, --config all work |
| Test quality | 3 | No unit tests (CLI wiring), manual verification only |
| Code style | 5 | Clean, minimal, follows existing patterns |
| Logging | 4 | buildLogger() properly respects config + CLI flags |
| Documentation | 3 | No new godoc (thin wiring code) |
| Plan adherence | 5 | Followed plan exactly |
| Communication | 4 | Clear summary |

#### Orchestrator Self-Assessment

| Criterion | Rating | Notes |
|-----------|:------:|-------|
| Planning quality | 5 | Two-wave plan with zero conflicts, accurate estimates |
| Agent prompt clarity | 4 | Agents executed correctly with minimal deviation |
| Review thoroughness | 5 | Line-by-line review of all new/modified files |
| Top-up effectiveness | N/A | No top-ups needed |
| Documentation updates | 5 | All docs updated: roadmap, LEARNINGS, BACKLOG, metrics, CLAUDE.md |
| Escalation discipline | 5 | No non-trivial decisions made autonomously |
