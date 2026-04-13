# Performance Benchmarking

GOVERNS: cmd/devtool/main.go, internal/devtool/bench*.go, benchmarks/results/*.json, benchmarks/reports/*.md, spec/reference/performance-benchmarks.md, README.md (Representative Performance section)

Implements: R-6.1.6 [planned], R-6.1.7 [planned], R-6.1.8 [planned], R-6.1.9 [planned], R-6.1.10 [planned], R-6.10.6 [verified], R-6.10.14 [verified], R-6.10.15 [planned]

## Overview

The repository needs a first-class way to do three related but distinct jobs:

1. prove or falsify the performance targets in [non-functional.md](../requirements/non-functional.md)
2. help maintainers find real bottlenecks in representative sync workloads
3. publish honest, reproducible representative results for users without
   turning the README into an unsourced marketing claim

This design treats performance benchmarking as a repo-owned verification and
reporting surface, not as an ad hoc collection of one-off shell scripts or
GitHub Actions timings.

The existing production-visible instrumentation in `internal/perf`, the live
timing summaries written by the E2E harness, and the verifier's machine-readable
summary output are inputs to this design. They are not, by themselves, proof of
the product's CPU, memory, or throughput targets. Benchmarking must combine:

- external process truth such as wall time, CPU, and peak RSS
- repo-owned internal counters and phase timings that explain where time went
- controlled scenario definitions that can be rerun and compared over time

The implementation should stay deliberately small:

- one benchmark runner entrypoint
- one machine-readable result schema
- one generated report path from that schema
- one source of truth for internal performance counters
- one first representative scenario before broadening coverage

## Ownership Contract

- Owns: Named benchmark scenario definitions, benchmark result schema, benchmark report generation, benchmark publication policy, and the repo-owned `cmd/devtool` entrypoint that runs representative benchmark scenarios.
- Does Not Own: Production perf counter collection, sync-engine correctness, Graph transport semantics, or the user-facing `status --perf` / `perf capture` runtime surfaces.
- Source of Truth: The benchmark scenario registry plus the generated machine-readable benchmark result artifacts for each run.
- Allowed Side Effects: Creating benchmark fixtures under explicit temp/work directories, invoking repo-owned CLI flows, capturing machine/process metrics, writing benchmark result artifacts, and generating Markdown summary reports.
- Mutable Runtime Owner: The `cmd/devtool` benchmark runner owns mutable benchmark-run state for one invocation. Production runtime state remains owned by the product command or daemon being measured.
- Error Boundary: The benchmark runner translates fixture-setup, command-execution, environment, and artifact-write failures into benchmark-run outcomes. Product/runtime failures remain product failures and are captured as benchmark evidence rather than being hidden behind benchmark-specific retry or fallback behavior.

## Verified By

| Behavior | Evidence |
| --- | --- |
| The repo already has a machine-readable timing-summary foundation for live E2E timing windows rather than only human log output. | `TestE2ETimingRecorderWritesEventsAndAggregatedSummary`, `TestE2ETimingRecorderLoadsExistingEventsAcrossProcesses` |
| The verifier already owns machine-readable run summaries instead of forcing downstream tooling to scrape console text. | `TestRunVerifyWritesSummaryJSONOnSuccess`, `TestRunVerifyWritesSummaryJSONOnFailure` |
| Repo-owned diagnostic scenario tooling is already an accepted pattern under `cmd/devtool` for reproducible, named capture flows. | `TestRunWatchCaptureWritesJSON`, `TestLookupWatchCaptureScenarioStepOrder` |
| Repo-owned benchmarking now has a subject-aware `devtool bench` entrypoint with named scenarios, JSON result bundles, and deterministic summary aggregation. | `TestNewBenchCmdRequiresScenarioFlag`, `TestNewBenchCmdDefaultsSubjectToOnedriveGo`, `TestNewBenchCmdPassesFlagsThrough`, `TestRunBenchRequiresRepoRootAndScenario`, `TestLookupBenchRegistriesAreSortedAndIncludeBuiltins`, `TestRunBenchStartupEmptyConfigSucceeds` |
| The benchmark runner now owns one canonical live representative catch-up scenario with a checked-in deterministic fixture contract, stable denominator math, and fixture-failure reporting that stays inside benchmark results. | `TestLoadBenchLiveFixturePlanIsDeterministicAndSized`, `TestLoadBenchLiveFixturePlanMutationSelectionIsStable`, `TestPrepareBenchScenarioUsesPreparedRunnerAndCleanup`, `TestLiveCatchupScenarioMissingPrerequisitesReturnsFixtureFailure` |

## Objectives

- Prove the target requirements in `R-6.1.*` with named, repeatable evidence.
- Expose realistic bottlenecks in representative sync workloads instead of
  optimizing from intuition.
- Make the benchmark methodology explicit enough that future numbers are
  comparable across branches and dates.
- Publish a small set of representative results for users while keeping the
  authoritative evidence and methodology in repo-owned artifacts and docs.

## Non-Goals

- Replacing `go test -bench` microbenchmarks for hot leaf functions. Those
  still matter for package-local CPU/allocation diagnosis.
- Turning default `go run ./cmd/devtool verify default` into a performance
  benchmark lane.
- Treating GitHub Actions wall-clock runtime as product benchmark truth.
- Emitting durable benchmark history from production runtime state. Production
  logs remain operational telemetry; benchmark runs are explicit repo-owned
  exercises.

## Avoiding Duplicate Systems

This design intentionally rejects parallel benchmark subsystems.

- The benchmark runner must reuse `internal/perf` for product-owned counters
  instead of re-counting bytes, requests, or phase timings itself.
- Benchmark artifacts are the only source of truth for published benchmark
  numbers. Markdown reports and README snippets are derived views.
- The benchmark harness may reuse existing E2E fixture or timing helpers, but
  it must not create a second, mostly-overlapping fixture framework unless a
  real incompatibility forces that split.
- `devtool bench` is the single repo-owned benchmark entrypoint. Additional
  helper commands are acceptable only when they support that one entrypoint,
  not when they create a second benchmark UX.

## Ecosystem Lessons

The design intentionally copies the shape used by mature sync and backup tools
instead of inventing a benchmark story in isolation.

- `rclone` is a strong model for separating human progress, structured logs,
  machine-readable live stats, and opt-in profiling. We should copy that split,
  not just its counters.
- `Syncthing` is a strong model for serving different audiences through
  different surfaces: GUI/live status, REST, Prometheus metrics, event streams,
  and explicit profiling/debug support.
- `restic` is a strong model for documenting stable JSON output and for being
  explicit about the performance cost of user-facing conveniences such as
  progress estimation.
- `rsync` is a warning as much as a model: progress and ETA are useful for
  humans but can be misleading when the underlying algorithm is doing more than
  a simple byte stream copy.
- `hyperfine` is a strong model for benchmark mechanics: warmups, prepare
  hooks, multiple runs, outlier awareness, and exportable JSON/Markdown output.

The practical consequence is that this design should optimize first for
observability and measurement discipline, then for published benchmark tables.

## Audiences

Different consumers need different answers.

- End users care about startup time, catch-up time, no-op sync cost, idle watch
  resource usage, and whether progress is visible and believable.
- Power users care about machine-readable outputs, structured logs, and enough
  context to compare one run or configuration against another.
- Maintainers care about proof metrics plus bottleneck attribution: is the
  scenario limited by network, Graph throttling, hashing, planner work,
  filesystem I/O, or database work.
- README readers need only a few representative numbers, but those numbers must
  be tied back to the authoritative artifacts and methodology.

## Benchmark Shape

The benchmark system should stay as one maintained suite, one artifact format,
and one repo-owned runner. Controlled, live, and comparative workloads are
scenario modes within that one harness, not separate benchmark subsystems.

The first representative benchmark is intentionally a real OneDrive-backed
synthetic fixture. One long run through that scenario already yields external
proof metrics plus the internal attribution from `internal/perf`, which is
enough to find bottlenecks without maintaining several different benchmark
programs.

### Controlled Synthetic Scenarios

Controlled scenarios are the main regression and bottleneck-finding surface.
They use repo-owned local fixture construction plus mocked, recorded, or seeded
remote state so the workload is repeatable.

These scenarios are where we should prove planner, observer, store, executor,
and reconciliation scaling characteristics without network noise dominating the
result.

### Live Representative OneDrive Scenarios

Live scenarios use a real account and a named remote fixture to measure the
real user path. They are the truth surface for claims such as "a large
partially-local catch-up sync behaves like this on real OneDrive."

These scenarios are expected to be noisier than controlled runs. They belong in
explicit manual or scheduled lanes, not in required per-PR verification.

### Cross-App Comparisons

Cross-app comparisons run the same named fixture and starting state through
`onedrive-go` and another sync client on the same machine and network, with the
same measurement rules and documented caveats.

Cross-app comparisons are publication material, not merge gates. They exist to
ground external claims and to identify large product gaps worth investigating.

The first comparison target should be `abraunegg/onedrive`, because it is the
closest comparable OneDrive-focused sync client for full-sync behavior. The
second planned comparison target should be `rclone`, because it is the most
useful automatable CLI baseline for one-off and scripted sync-style operations.
Other tools may be added later once the core harness and fixture methodology
are stable.

## Scenario Taxonomy

Every benchmark scenario must have a stable, human-readable scenario ID. These
IDs are part of the publication contract and must not depend on incidental file
paths or machine names.

Initial benchmark families:

- `startup-empty-config`
- `watch-idle-30m`
- `sync-noop-reconcile-large`
- `sync-initial-10k-small-files`
- `sync-partial-local-catchup-100m`
- `sync-local-burst-1k-edits`
- `sync-remote-burst-1k-edits`
- `sync-large-files-10x1gb`

The first harness-validation scenario is `startup-empty-config`. The first
representative sync scenario is `sync-partial-local-catchup-100m`. It is a
real OneDrive-backed benchmark-owned fixture rooted at
`/benchmarks/sync-partial-local-catchup-100m`, generated from a checked-in
synthetic manifest that totals exactly 100 MiB across roughly 2.6K files. The
measured phase is intentionally a catch-up run, not an initial sync: the
scenario seeds remote state once per invocation, establishes a fresh local
baseline before each sample, applies deterministic local deletes and
truncations, and then measures the repairing `sync --download-only` pass.

## Scenario Contract

Every scenario must define an explicit execution contract. The benchmark runner
must not infer these rules ad hoc from the command being exercised.

Required scenario contract fields:

- subject-under-test ID
- scenario ID and benchmark class
- fixture setup procedure for local and remote state
- configuration profile used for the run
- warmup policy
- exact timer start condition
- exact timer stop condition
- success and convergence predicates
- cleanup/reset procedure between repetitions
- denominator fields recorded for the workload

The subject-under-test ID is required from the start so the same benchmark
artifact and reporting pipeline can measure `onedrive-go` and comparison apps
without inventing a second schema later.

The configuration profile must be explicit:

- `default-safe`: default shipping settings and safety invariants intact
- `tuned`: non-default but documented tuning values
- `comparative`: settings chosen to make a fair cross-app comparison

Public headline results should come from `default-safe` runs unless the report
clearly states otherwise.

The denominator fields are mandatory because raw timings are not comparable on
their own. At minimum, each scenario must record the number of files,
directories, changed items, changed bytes, expected transfers, and expected
deletes relevant to the run.

## Measurement Model

Benchmark evidence must separate proof metrics from explanation metrics.

### External Proof Metrics

These are the primary truth metrics used for target validation and public
reporting:

- wall-clock duration
- user CPU and system CPU
- peak RSS / max resident set size
- bytes read and written at the process level when available
- success/failure and correctness outcome

These metrics must come from the operating system or process runner, not only
from repo-internal counters.

### Internal Explanation Metrics

These metrics come from repo-owned instrumentation and explain why a scenario
behaved the way it did:

- observe/plan/execute/reconcile durations
- bytes transferred and item counts
- Graph request, retry, and status-class counts
- SQLite transaction counts and cumulative DB time
- transfer-manager timing and throughput
- watch-event counts where applicable

Internal metrics explain bottlenecks. They do not replace external proof
metrics when validating `R-6.1.*`.

### Headline Metrics For Public Reporting

The benchmark report and README summary should focus on a small stable set of
user-relevant metrics:

- startup time
- idle watch CPU and RSS
- no-op reconcile time on a large tree
- initial sync time for a representative file-count workload
- partial-local catch-up sync time
- local-change and remote-change convergence time

ETA should be treated as a live UX aid rather than a canonical benchmark metric.
Progress estimation is useful in the product surface, but it is too sensitive
to workload shape and algorithmic details to serve as a primary proof metric.

### Denominator And Attribution Metrics

Every benchmark artifact must also carry the workload size and attribution data
needed to interpret the headline numbers:

- file count and directory count
- changed item count and changed byte count
- total remote enumeration size where relevant
- transfer count, delete count, and conflict count where relevant
- phase timings and aggregate resource counters

## Repeatability Controls

Every benchmark run must record enough context to understand what was measured.

Required run metadata:

- benchmark class and scenario ID
- git commit
- benchmark runner version/schema version
- machine description
- OS and Go version
- account/fixture identity labels redacted to stable scenario-owned names
- warm or cold start/cache policy
- repeat count and aggregation rule
- whether the run used live OneDrive, mocked remote state, or cross-app
  comparison mode

Benchmark methodology must also state:

- how the local tree is seeded before the run
- how the remote tree is seeded before the run
- what counts as convergence or success
- what cleanup/reset happens between repetitions

The methodology must also state whether the scenario is intentionally measuring
warm-cache or cold-cache behavior. Those are different workloads and must not
be mixed into one published number.

## Measurement Mechanics

The benchmark runner should combine operating-system process measurement with
repo-owned artifacts.

- External proof metrics should come from process-level measurement such as
  `/usr/bin/time` on macOS or Linux equivalents, not from product logs alone.
- Internal explanation metrics should come from repo-owned perf, E2E timing,
  and scenario-specific artifacts.
- Controlled scenarios should support explicit warmup and reset hooks.
- Live representative scenarios must reset both local and remote fixture state
  between measured repetitions.
- Cross-app comparisons should use paired runs on the same machine and network,
  with alternating or randomized run order to reduce drift from transient
  conditions.

## Sample Size Guidance

Sample size depends on scenario cost and noise. The runner should not use one
fixed repetition count for every workload.

- Startup and other short controlled scenarios: `10-20` measured runs after a
  small number of warmups.
- Controlled end-to-end scenarios under roughly one minute: `5-10` measured
  runs.
- Multi-minute controlled sync scenarios: `3-5` measured runs, reported with a
  median plus spread.
- Live representative OneDrive scenarios: at least `3` paired runs before the
  result is considered publishable.
- Idle watch scenarios: at least `3` soak runs of meaningful duration, with
  steady-state and burst behavior reported separately when possible.

If variance remains high, the default response should be to stabilize the
fixture or split the scenario, not to silently inflate the sample size until a
number looks stable.

## Outputs And Publication

The benchmark harness must write one machine-readable result bundle per run and
should also support generated Markdown summaries from the same data.

Planned output layers:

- local run output under an explicit output directory such as
  `./.artifacts/benchmarks/` or `--output`
- `benchmarks/releases/<version>/results/*.json`: committed release-version
  benchmark artifacts
- `benchmarks/releases/<version>/reports/*.md`: committed release-version
  generated reports
- `spec/reference/performance-benchmarks.md`: curated methodology and current
  representative results
- `README.md`: a short "Representative Performance" section that links back to
  the full benchmark report and methodology

The README is a summary surface only. It must never be the sole location of the
numbers or the methodology behind them.

The README update is intentionally trivial once the artifacts and generated
report exist. The benchmark system should not grow special logic just for the
README.

Ordinary local benchmark runs do not need to be committed. The committed track
record lives at release granularity: for each released version, the repo should
contain the blessed machine-readable benchmark artifacts and generated reports
used to represent that release's performance.

Any published representative number must include or link to:

- machine profile
- run date
- run count or sample-size summary
- scenario ID
- subject-under-test ID
- configuration profile
- whether the run was controlled, live, or comparative
- a path or link to the fuller benchmark report

## Benchmark Runner Ownership

`cmd/devtool` owns benchmark orchestration and publication because benchmarking
is a repo verification/reporting concern, not a product CLI concern.

Planned responsibilities for the benchmark runner:

- subject adapter selection for `onedrive-go` and comparison apps
- scenario registry and fixture setup
- invocation of repo-owned CLI commands and/or E2E helpers
- external process metric capture
- collection of repo-owned timing/counter artifacts
- result normalization and report generation

The benchmark runner may reuse E2E fixture helpers or timing helpers, but the
benchmark suite is intentionally not just "run some E2E tests and time them."
Correctness E2E and benchmark reporting have different stability and reporting
requirements.

Cross-app support must be a compatibility requirement of the runner and the
artifact schema from v1. It does not need full comparison automation in the
first increment, but the core model must not assume that `onedrive-go` is the
only measurable subject.

### Delivered Benchmark Runner Slices

The implemented slices stay intentionally narrow: first the runner shape,
then one canonical live representative catch-up benchmark.

- `go run ./cmd/devtool bench --scenario <id>` is the repo-owned benchmark
  entrypoint.
- `--subject <id>` defaults to `onedrive-go`.
- `--runs <n>` and `--warmup <n>` override scenario defaults. `-1` means "use
  the scenario default"; `0` is an explicit zero-run override.
- `--json` emits the machine-readable result bundle to stdout.
- `--result-json <path>` writes the same bundle atomically to disk.

The implemented v1 subject adapter is `onedrive-go` only. It builds the repo
binary once per invocation, records repo/host metadata, and measures benchmark
samples against that built binary. Build time is not part of the measured
samples.

The implemented v1 scenario is `startup-empty-config`:

- class: `controlled`
- config profile: `default-safe`
- defaults: `runs=15`, `warmup=3`
- command under test: `onedrive-go --config <cfg> --verbose drive list --json`

For each sample, the scenario creates fresh temp `HOME`,
`XDG_CONFIG_HOME`, `XDG_DATA_HOME`, and `XDG_CACHE_HOME` roots and writes a
minimal config that enables info-level logging. Success requires exit code `0`
and a valid `drive list --json` payload with empty `configured` and `available`
arrays. The auth/degraded arrays follow the product CLI's existing JSON
contract and may be absent when empty.

The result bundle is subject-aware and records:

- result format version
- subject metadata
- scenario metadata
- environment metadata
- run metadata
- per-sample proof metrics
- per-sample `internal/perf.Snapshot` explanation metrics when available
- aggregate summary statistics

Per-sample proof metrics are stored in integer microseconds and bytes:

- wall-clock elapsed time
- exit code
- user CPU time
- system CPU time
- peak RSS
- stdout/stderr byte counts
- failure excerpt only when the sample failed

For this first scenario, explanation metrics come from the command's existing
performance summary log line captured on stderr. Skip-config commands such as
`drive list` do not switch into config-driven log-file routing, so the runner
must not assume that a configured `log_file` will exist for every scenario.

The implemented v1 representative sync scenario is
`sync-partial-local-catchup-100m`:

- class: `live`
- config profile: `default-safe`
- defaults: `runs=3`, `warmup=0`
- command under test: `onedrive-go sync --download-only`

The scenario keeps a benchmark-owned remote scope under
`/benchmarks/sync-partial-local-catchup-100m` and uses `sync_paths` so the
subject syncs only that subtree. The checked-in manifest defines four file
bands totaling exactly 100 MiB:

- `documents`: 1600 files × 256 B
- `metadata`: 900 files × 4 KiB
- `assets`: 160 files × 256 KiB
- `media`: 8 files × 7 MiB

Each measured sample follows the same three-stage contract:

1. run an unmeasured `sync --download-only` to establish a fresh local
   baseline and sync database
2. delete and truncate a deterministic subset of local files
3. measure a second `sync --download-only` until process exit, then verify the
   local tree matches the manifest exactly

Remote fixture setup is owned by scenario preparation, not by the measured
sample window. The runner resets the benchmark-owned remote scope, uploads the
canonical seed tree with `sync --upload-only`, waits for the scope to become
visible, and only then starts per-sample baseline and measured phases. Missing
live credentials, missing `.testdata`, missing allowlist entries, remote-reset
failures, and seed failures surface as `fixture_failed` benchmark outcomes
instead of opaque setup crashes.

The remote-scope visibility probe treats the parent `ls` output as a set of
exact entry names, not substring evidence. Sibling entries such as
`test-scope-backup` must not satisfy visibility for `test-scope`.

Per-sample runtime cleanup is part of the same benchmark outcome contract. If a
sample finishes its measured command successfully but teardown of the sample's
isolated runtime root fails, the runner downgrades that sample to
`fixture_failed` instead of silently discarding the cleanup error.

## Publication Rules

- Default local verification and required CI remain correctness-focused.
- Controlled benchmark scenarios may become optional local tooling and
  scheduled/manual CI lanes.
- Live representative scenarios and cross-app comparisons remain
  scheduled/manual by default.
- Cross-app comparison reports must document the compared app versions, machine,
  network assumptions, fixture state, and any feature mismatch caveats.
- Public benchmark summaries must be generated from benchmark artifacts, not
  copied in manually.
- Public README summaries must be intentionally sparse: a few representative
  numbers only, each tied to a machine, date, scenario ID, and full report.
- The repository should keep a committed release-by-release benchmark track
  record. That means each release may promote a curated set of benchmark
  artifacts and reports into `benchmarks/releases/<version>/`.

## Comparison Fairness Rules

Cross-app comparisons must compare like with like as closely as possible.

- The starting local and remote state must be equivalent for every app under
  test.
- Safety and correctness differences must be disclosed. A faster app that skips
  a safety invariant is not directly equivalent.
- Configuration differences that materially affect concurrency, hashing,
  verification, or deletion safety must be documented.
- If a competing app lacks a comparable mode, the report must say so instead of
  forcing an artificial equivalence.
- Comparison artifacts must record app version, command/config, machine, and
  run order.

## Increment Plan

This work is intentionally staged.

### Increment 1: Spec And Schema

- create the benchmark design doc
- add the missing non-functional requirements
- define scenario IDs, output schema shape, and publication rules

### Increment 2: Benchmark Runner Skeleton [delivered]

- add a repo-owned `cmd/devtool` benchmark entrypoint
- add scenario registration and JSON output plumbing
- make the result schema and runner subject-aware so they also work for
  cross-app measurement later
- record external process metrics and baseline run metadata
- define the scenario contract shape, denominator fields, and sample-size policy
- implement the harness-validation scenario `startup-empty-config`

### Increment 3: Canonical Live Catch-Up Benchmark [delivered]

- implement `sync-partial-local-catchup-100m`
- keep one benchmark harness and one artifact format instead of parallel
  benchmark subsystems
- add scenario-level prepare/cleanup support so live fixture setup happens once
  per invocation
- keep the scenario manual-only and outside default verification
- reuse existing `internal/perf` summaries for explanation metrics while the
  benchmark runner records external proof metrics

### Increment 4: Reporting Surface And Simple README Update

- add generated Markdown report output
- add `spec/reference/performance-benchmarks.md`
- add a small README summary section backed by generated artifacts
- include a representative-results table that cites machine, date, scenario,
  config profile, and run count

### Increment 5: Live And Comparative Runs

- add scheduled/manual live representative benchmark lanes
- add documented cross-app comparison methodology and initial comparison runs
- implement the first comparison subject adapter for `abraunegg/onedrive`
- implement the second comparison subject adapter for `rclone`
- add paired-run ordering and fixture-reset rules for comparative execution

## Open Design Constraints

- The benchmark runner must not create a second source of truth for runtime
  perf counters that already belong to `internal/perf`.
- Benchmark runs must keep secrets and account-specific identifiers out of
  committed artifacts.
- Large live-fixture scenarios need explicit reset/setup ownership so reruns do
  not accidentally inherit previous state and invalidate comparisons.
- Cross-app comparisons must compare like with like. If another app lacks a
  feature or safety invariant that `onedrive-go` enforces, the methodology must
  call that out instead of silently treating all elapsed time as equivalent.
