# System Architecture

GOVERNS: internal/synctree/*.go, internal/localtrash/*.go, internal/fsroot/*.go, internal/localpath/*.go, internal/perf/*.go, internal/devtool/*.go, internal/errclass/*.go, internal/sharedref/*.go, internal/authstate/*.go, internal/synctest/*.go, internal/namekey/*.go, internal/panicguard/*.go

Implements: R-6.2.2 [verified], R-6.3.1 [verified], R-6.6.17 [verified], R-6.10.1 [verified], R-6.10.2 [verified], R-6.10.5 [verified], R-6.10.7 [verified], R-6.10.8 [verified], R-6.10.9 [verified], R-6.10.11 [verified], R-6.10.12 [verified], R-6.10.13 [verified], R-6.10.16 [verified], R-6.10.17 [verified], R-6.10.21 [verified], R-6.10.22 [verified], R-6.10.26 [verified], R-6.10.27 [verified], R-6.3.10 [verified]

## Verified By

| Behavior | Evidence |
| --- | --- |
| Package boundaries and privileged-call discipline remain documented in the owning design docs and exercised by package-level behavior tests. | `cmd/devtool/main_test.go`, `spec/design/sync-control-plane.md`, `spec/design/sync-engine.md`, `spec/design/graph-client.md`, `spec/design/drive-transfers.md` |
| The product continues to split multi-mount control, single mounted content-root sync, Graph I/O, and rooted filesystem access into explicit authority boundaries. | `spec/design/sync-control-plane.md`, `spec/design/sync-engine.md`, `spec/design/graph-client.md`, `spec/design/drive-transfers.md` |
| Public README status stays requirements-backed and does not reintroduce stale phase-roadmap language. | `internal/devtool/verify_readme_test.go` |
| The full live suite runs every bucket after one fails, lists every bucket (`not-run` after a preflight failure) and names the failed tests; `public` and `stress` run every step; every go test deadline a CI job's profile can start fits inside the job's timeout (R-6.10.26). | `TestRunVerifyE2EFullRunsEveryBucketAfterOneFails`, `TestRunVerifyE2EFullReportsEveryFailingBucket`, `TestRunVerifyE2EFullRecordsBucketsNotRunAfterPreflightFailure`, `TestRunVerifyE2EFullDoesNotMaskUnknownFastSuiteFailure`, `TestRunVerifyWritesSummaryJSONOnFailure`, `TestRunVerifyPublicRunsUnitTestsAfterLintFails`, `TestRunVerifyPublicSkipsOnlyTheCoverageGateAfterUnitTestsFail`, `TestRunVerifyStressRunsEveryStepAfterOneFails`, `TestCIJobTimeoutsCoverEveryGoTestDeadline` |
| A check that did not run is not recorded as passed: `--check-format` fails on unformatted files, the auth preflight is rerun only for its own test failing and a failed rerun reports its own error, a skipped auth preflight is recorded `skipped`, and the `failed-test` rerun keeps `-race` and the shuffle seed (R-6.10.26). | `TestRunVerifyCheckFormatFailsOnUnformattedFiles`, `TestCheckFormatPassesOnAFormattedTree`, `TestRunVerifyRejectsCheckFormatWithoutAFormatStep`, `TestRunVerifyE2EFullDoesNotRerunAuthPreflightWithoutItsTestFailing`, `TestRunVerifyE2EFullReportsTheAuthPreflightRerunFailure`, `TestRunVerifyRecordsASkippedAuthPreflightAsSkipped`, `TestRunVerifyFailedTestKeepsRaceAndTheShuffleSeed`, `TestRunFailedTestVerification_PatternThatSelectsNothingFails` |
| One parser reads every go test log devtool consumes, raw or `-json`, with or without the timestamps and byte-order mark of a GitHub Actions job log: it decodes only lines that are `-json` events, settles a raw `--- FAIL` line at its package's result line, read only in go test's exact form (`ok`, two spaces, a tab and the package; `FAIL`, a tab and the package, `[build failed]` and `[setup failed]` included), and drops it when the package passed, names a package that failed without a test, and names each test a timeout panic lists as running, which no classified rerun may answer (R-6.10.23, R-6.10.26). | `TestParseGoTestLog_TimestampedRawLines`, `TestParseGoTestLog_BraceInARawLineIsNotJSON`, `TestParseGoTestLog_TimeoutPanicNamesTheRunningTest`, `TestParseGoTestLog_PackageFailuresAndPassingPackages`, `TestParseGoTestLog_StrayOkLineDoesNotCloseARawPackage`, `TestRunVerifyE2EFullDoesNotRerunAuthPreflightWithoutItsTestFailing` |
| The DoD stages and the nightly check repeat a GitHub request that GitHub answered with an HTTP 5xx, whose connection dropped, or that never reached GitHub, with backoff and at most four attempts, then fail naming the request and the answer; other failures, rate limits among them, are final at once, a review reply is posted once, and a rerun refused as already running after a 5xx counts as started once the run reads so (R-6.10.16, R-6.10.23). | `TestGHClient_RetriesOnlyTransientFailures`, `TestGHClient_RetriesARequestThatNeverReachedGitHub`, `TestGHClient_RateLimitsAreFinal`, `TestGHClient_RefusalAfterA5xxMeansTheRequestTookEffect`, `TestGHClient_ContextEndingDuringTheBackoffEndsTheCall`, `TestNightlyCheck_RetriesAJobLog5xxThenSucceeds`, `TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound`, `TestWaitForPRCI_RepeatsAPollGitHubFailed`, `TestWaitForPRCI_RerunGitHubStartedDespiteA5xxIsWaitedFor`, `TestWaitForPRCI_RefusedRerunOfAFinishedRunFails` |
| The DoD stall window counts only the jobs that run for the awaited event, reads the e2e job's per-event timeout, gives a check that waits in its job's concurrency group one whole run of that group, and refuses a job condition, timeout or concurrency group it cannot evaluate (R-6.10.16). | `TestCIStallWindow_PostMergeUsesOnlyPushJobs`, `TestCIStallWindow_PullRequestUsesOnlyPullRequestJobs`, `TestCIWorkflow_RejectsAnExpressionItDoesNotKnow`, `TestCILiveJobsPullRequestTimeoutsAreTheirNeed`, `TestWaitForPRCI_CheckQueuedInItsGroupWaitsOneRunOfTheGroup`, `TestWaitForPRCI_RunningCheckStallsAtTheEventsWindow` |
| The live CI jobs keep the test drives and the account allowlist out of job-level `env` and receive them in the step that runs `scripts/ci-mask-test-drives.sh`, which only the checkout precedes and which masks every spelling of them, exports the names the credential steps use and maps the shortcut fixtures to drives by variable name; an artifact is redacted before it is uploaded, the e2e suite names drives by role in test names, file names, messages and artifacts, no tracked file spells a test account, nothing guesses a shortcut's parent drive, and the allowlist check names variables, never values (R-6.10.27). | `TestCIWorkflowSetsNoTestDriveInJobEnv`, `TestCIMaskScript_MasksEverySpellingAndExportsTheNames`, `TestCIMaskScript_RefusesAShortcutMappingToAnotherVariable`, `TestDriveRoles_RecordedLinesHoldNoConfiguredDrive`, `TestE2ERecordersRecordRolesNotDrives`, `TestE2E_TrackedFilesNameNoTestAccount`, `TestFindTestAccounts_NamesFileLineAndRoleNeverTheAddress`, `TestLoadLiveTestConfig_DefaultShortcutFixtures`, `TestLoadDotEnvFile_ReturnsErrors`, `TestCheckDriveAllowed_NamesTheVariablesAndNoValue`, `TestAllowlistIncludes` |
| A go test step of any profile in which no package ran a test fails instead of reporting success (a `-run` pattern that selects nothing, a tree of `[no test files]`, a binary that never built), the integration tier runs only the `TestIntegration_...` tests that the test-convention check holds every integration-tagged test to, and every stress step streams its output (R-6.10.17, R-6.10.26). | `TestRunVerifyPublicFailsAUnitTierThatRanNoTest`, `TestRunVerifyQuickFailsAShardThatRanNoTest`, `TestRunVerifyIntegrationRunsOnlyTheIntegrationTests`, `TestRunTestConventionsRequiresTheIntegrationPrefixInIntegrationFiles`, `TestFindTestConventionViolations_RealRepoIsClean`, `TestAssertTestsMatched_RejectsEmptyRun`, `TestAssertTestsMatched_RejectsEmptyRunInJSONOutput`, `TestAssertTestsMatched_AcceptsRealRun`, `TestAssertTestsMatched_RejectsATreeWithNoTestFiles`, `TestAssertTestsMatched_ABinaryThatNeverRanRanNoTest`, `TestRunVerifyStressRunsExpectedSteps`, `TestRunVerifyStressFailsAStepWhosePackagesHaveNoTests`, `TestRunVerifyStressStreamsTheRepeatStep`, `TestRunVerifyE2EFullFailsASuiteThatRanNoTests` |
| The lint profile lints every workflow file with the pinned actionlint, its shellcheck and pyflakes integrations off, and names each finding's file and line; the benchmark builds its subject with the toolchain `go.mod` pins (R-6.10.2). | `TestVerifyLint_RefusesAnInvalidWorkflowFile`, `TestRunVerifyLintRunsThePinnedActionlintOverEveryWorkflow`, `TestRunWorkflowLint_NamesEveryFinding`, `TestRunWorkflowLint_ACompileErrorIsNotAFinding`, `TestRunWorkflowLint_TreeWithoutWorkflowsRunsNothing`, `TestPrepareBenchSubject_BuildsWithThePinnedToolchain` |
| Requirement traceability refuses an ID that two requirement lines declare, naming both lines (R-6.10.12). | `TestRequirementTraceability_RefusesAnIDDefinedTwice`, `TestFindRequirementViolations_RealRepoIsClean` |
| Design-doc ownership, routing-table paths, and test-citation claims cannot drift from the tree without failing verification. | `TestSpecGoverns_RejectsDeadRoutingTableReference`, `TestSpecGoverns_AcceptsResolvableRoutingTable`, `TestSpecGoverns_MissingRoutingTableIsNotAViolation`, `TestSpecGoverns_RejectsUngovernedProductionFile`, `TestSpecGoverns_RejectsPatternMatchingNothing`, `TestSpecGoverns_RejectsEquallySpecificDoubleOwnership`, `TestSpecGoverns_MoreSpecificPatternWinsOverBroadGlob`, `TestSpecGoverns_RejectsCitationOfMissingTest`, `TestSpecGoverns_SkipsProseGovernsEntries`, `TestSpecGoverns_AllowsOverlappingPatternsWithinOneDoc`, `TestSpecGoverns_IgnoresTestFiles`, `TestSpecGoverns_AcceptsFullyGovernedTree`, `TestSpecGoverns_AcceptsCitationOfExistingTest` |
| A provider read-only window is reported as an untestable live lane rather than a false failure, and is not retried as propagation lag. | `TestIsTransientDrivesDiscoveryError_ServiceReadOnlyIsNotTokenPropagation`, `TestIsTransientDrivesDiscoveryError_PlainAccessDeniedStillRetries`, `TestServiceReadOnly_IsClassifiedAsProviderUnavailable`, `TestServiceReadOnly_OrdinaryForbiddenIsNotProviderUnavailable`, `TestMostSpecificErrorCode_PrefersInnerCode`, `TestMostSpecificErrorCode_FallsBackToOuterCode`, `TestMostSpecificErrorCode_HandlesUnparseableBody` |
| Dependency vulnerabilities are caught by tooling rather than by review, including in the forked oauth2 credential path. | `.github/workflows/ci.yml` (govulncheck step), `.github/dependabot.yml` |
| Every platform claimed in README.md compiles on every verify run, and the darwin runtime is exercised by its own CI leg. | `TestRunVerifyDefaultRunsExpectedSteps`, `TestRunVerifyPublicRunsExpectedSteps`, `.github/workflows/ci.yml` |
| Test assertion style and sleep discipline are enforced by repo verification, not only by contributor convention. | `internal/devtool/verify_test_conventions_test.go` |
| No goroutine of the product can end the process with Go's exit status 2: every goroutine start (a go statement, a group's `Go` call, or the callback of a `time.AfterFunc` or `context.AfterFunc` call) begins with deferred calls that include a recover handing the value to `panicguard.Recovered`, every recover does, a start the scan cannot resolve to a function of its package (another package's function, an expression it cannot name) is a finding, and the scans reject what they police; `Recovered` logs one Error record with the panicking goroutine's stack and returns an error wrapping `ErrPanic` that carries the stack (R-6.3.10, R-6.3.8). A test whose subject panics on a goroutine of its own runs it in a child test binary (`synctest.RunInChild`), so a missing boundary fails the test instead of the package's run. | `TestEveryGoroutineHasAPanicBoundary`, `TestPanicRecoverySitesCaptureTheStack`, `TestBoundaryScans_RejectWhatTheyPolice`, `TestRecovered_LogsTheValueAndTheStackAtErrorAndWrapsErrPanic`, `TestRecovered_WithoutALoggerLogsThroughTheDefault`, `TestStack_FindsThePanicInsideAWrappedError`, `TestRunInChild_AnEscapedGoroutinePanicEndsOnlyTheChild`, `TestRunInChild_APassingChildPasses`, `TestLogCapture_KeepsEveryRecordWithItsAttributes`, `TestPanicOnMessage_PanicsOnlyOnItsMessage` |
| `localpath.AtomicWrite` and `fsroot.Root.AtomicWrite` fsync the directory the rename landed in, report a sync failure, and tolerate only a filesystem that cannot sync a directory (R-6.5.4). | `TestAtomicWrite_SyncsTargetDirectory`, `TestAtomicWrite_ReportsDirectorySyncFailure`, `TestAtomicWrite_ToleratesUnsupportedDirectorySync`, `TestRoot_AtomicWrite_SyncsTargetDirectory`, `TestRoot_AtomicWrite_ReportsDirectorySyncFailure`, `TestRoot_AtomicWrite_ToleratesUnsupportedDirectorySync` |
| `synctree.Root.Spelling` finds an entry by its spelling on disk when a stored NFC path differs from it in Unicode form only: the exact path first, then one listing per missing component; an ambiguous match, a missing parent and an unusable path resolve nothing, and `SpellingOfParent` never looks up the new entry's own name (R-2.13.2). | `TestRoot_SpellingFindsTheNameOnDisk`, `TestRoot_SpellingReturnsUnusablePathsAsGiven`, `TestRoot_SpellingOfParentResolvesOnlyTheParents`, `TestRoot_SpellingAddressesTheEntryForRootedOperations` |
| Sync-tree mutation cannot escape the configured sync root through a symlinked ancestor, and a containment failure is never reported as absence. | `TestRoot_Rename_ThroughSymlinkedAncestorFailsAndPreservesTarget`, `TestRoot_MkdirAll_ThroughSymlinkedAncestorFails`, `TestRoot_RemoveAll_ThroughSymlinkedAncestorFails`, `TestRoot_Remove_ThroughSymlinkedAncestorFails`, `TestRoot_Chtimes_ThroughSymlinkedAncestorFails`, `TestRoot_Lstat_ThroughSymlinkedAncestorIsNotReportedAsNotExist`, `TestRoot_Stat_ThroughSymlinkedAncestorIsNotReportedAsNotExist`, `TestRoot_OpenFile_ThroughSymlinkedAncestorFails`, `TestRoot_NormalizeNotExist_DepthOnePathTerminates`, `TestRoot_MutationsWithinRootStillSucceed`, `TestRoot_MutationsThroughInRootSymlinkSucceed`, `TestRoot_ValidateNoSymlinkAncestors_NamesLinkAndNonFolderSeparately` |
| Managed-state I/O cannot leave its folder through a symlinked parent, and a failure reads as absence only when the name is absent: never for an escape, and never for a create refused for permission, space or a read-only mount (R-6.4.10). | `TestRoot_Remove_RefusesSymlinkedParent`, `TestRoot_Rename_RefusesSymlinkedParent`, `TestRoot_AtomicWrite_RefusesSymlinkedParent`, `TestRoot_Stat_RefusesSymlinkedParent`, `TestRoot_OpenFile_CreateFailureKeepsItsErrno`, `TestRoot_OpenFile_PermissionDeniedIsNotNotExist`, `TestRoot_NameBelowAFileIsNotExist`, `TestRoot_RootThatIsAFileHasNothingBeneathIt` |

## Package Structure

Implements: R-6.1.4 [target], R-6.1.5 [target], R-6.9.1 [verified]

```
.                             Root package (thin process entrypoint)
cmd/
  devtool/                    Repo-owned developer-tooling entrypoint
internal/
  authstate/                  Shared auth-health vocabulary and user-facing auth copy
  cli/                        Cobra command tree, CLI bootstrap, output formatting, command handlers

  config/                     TOML config, drive sections, XDG paths, override chain
  devtool/                    Verifier, benchmark, worktree, and cleanup-audit helpers
  driveid/                    Type-safe drive identity: ID, CanonicalID (leaf, stdlib-only)
  driveops/                   Authenticated drive access: sessions, transfers, hashing
  errclass/                   Shared runtime failure class enum (leaf, stdlib-only)
  fsroot/                     Root-bound managed-state file capabilities
  graph/                      Graph API client: auth, Graph normalization, items CRUD, delta, transfers
  graphtransport/             Graph-facing HTTP transport profile construction
  localpath/                  Explicit arbitrary-local-path boundary helpers
  logfile/                    Log file creation and retention
  multisync/                  Multi-mount sync control plane and watch reload
  namekey/                    OneDrive's name-equivalence key (leaf, x/text only)
  panicguard/                 A recovered panic as one log record with its stack and one error (leaf, stdlib-only)
  perf/                       Command-scoped and live sync performance instrumentation plus capture bundles
  retry/                      Retry policies, exponential backoff with jitter, retry transport
  sharedref/                  Shared item selector parsing and formatting
  sync/                       Single mounted content-root sync engine, durable SQLite sync state, and read-only inspection
  synccontrol/                JSON-over-HTTP Unix-socket protocol shared by CLI and multisync owner
  synctest/                   Shared sync-package test helpers (test-only)
  synctree/                   Root-bound sync runtime filesystem capability
  syncverify/                 Baseline-versus-local sync verification helpers
  tokenfile/                  Pure OAuth token file I/O (leaf, stdlib + oauth2 only)
pkg/
  quickxorhash/               QuickXorHash algorithm (vendored from rclone, BSD-0)
e2e/                          E2E test suite (not production code)
scripts/                      Developer and CI-supporting helper scripts
testutil/                     Shared test helpers (not production code)
spec/                         Requirements, design docs, and reference docs
```

## Dependency Rules

```
root pkg → internal/cli/ → internal/graphtransport/ → internal/retry/
                         → internal/driveops/  → internal/graph/ → pkg/*
                         → internal/errclass/
                         → internal/perf/
                         → internal/multisync/ → internal/perf/
                                              → internal/sync/ → internal/driveops/
                         → internal/config/  → internal/driveid/
```

- No cycles. `driveops` does NOT import `sync`. `graph` does NOT import `config`.
- `multisync` owns multi-mount lifecycle; `sync` owns the single mounted
  content-root runtime.
- `authstate`, `driveid`, `errclass`, `localpath`, `loglevel`, `namekey`, `panicguard`, `sdnotify` and `sharedref` are leaf packages (no internal imports), every package that is one today, held to it by the `leaf-internal` depguard rule. Three packages have exactly one internal dependency and are held to that one by a rule of their own: `tokenfile` imports only `fsroot`, `retry` only `perf`, and `fsroot` only `localpath` (for `IsDirSyncUnsupported`).
- `panicguard` is the one way a recovered panic is logged and turned into an
  error (R-6.3.10). Every goroutine product code starts recovers at its
  boundary: the deferred calls a goroutine's function begins with include a
  recover that hands the value to `panicguard.Recovered`, which logs it once
  at Error with the panicking goroutine's stack and returns an error wrapping
  `panicguard.ErrPanic`. What follows is the boundary's own decision: a
  drive's runtime fails that drive (R-6.3.8, sync-engine.md and
  sync-control-plane.md), a helper the watch cannot run without ends the
  watch, a CLI helper cancels the command with the panic as the cause
  (cli.md, Exit Status), and a one-shot helper reports its unit's failure.
  `TestEveryGoroutineHasAPanicBoundary` and
  `TestPanicRecoverySitesCaptureTheStack` scan `main.go` and every package
  under `internal/` and `pkg/` except the developer tool and the test
  helpers, so a new package is covered without editing them; the goroutines
  that need no boundary are named in the test with the reason, and an entry
  no goroutine uses fails it. The CLI, the Graph client, `perf`,
  `multisync` and `sync` import it.
- `namekey` is the single authority for "are these two names in one folder the
  same OneDrive name". It is shared because both sides of the boundary ask:
  `graph` (path verification, created-item lookups), `driveops` (upload
  readback, CLI path walks) and `sync` (collision detection, the baseline
  sibling index, case-variant ordering). Its rule is reconstructed from live
  probes, not a Unicode standard form (OneDrive behaves like a collation),
  and every probed pair, including the 500-pair systematic probe M-1 kept in
  `internal/namekey/testdata`, is a test case
  (`TestKey_MatchesProbedOneDriveCollisions`, `TestKey_MatchesTheSystematicProbe`, R-2.12.3).
- Both `graph` and `config` import `tokenfile` for token file I/O.
- Callers pass token paths to `graph` — no config coupling.
- `graphtransport` owns Graph-facing HTTP transport profile construction. `driveops` owns the session runtime that reuses those profiles and chooses between bootstrap, interactive, and sync paths.

## SQLite-Backed Sync Pipeline

The sync engine is now snapshot-first:

- observation produces dirty signals and current observation facts
- SQLite persists current local/remote snapshots, baseline, observation
  problems, retry work, block scopes, and refresh cadence
- SQLite computes structural diff and reconciliation outcomes
- Go builds the current actionable set, executes it, and publishes baseline

```
Remote Observer ──┐
                  ├──→ Dirty Buffer ──→ refresh snapshots ──────────────┐
Local Observer  ──┘         │                                           │
                      debounce/wake                                     │

Remote Obs ───────────────→ remote_state ──┐                            │
Local Scan/Obs ───────────→ local_state  ──┼──→ comparison_state ──→ reconciliation_state
Baseline ─────────────────→ baseline     ──┘                            │
                                                                         │
observation_issues / retry_work / block_scopes / observation_state ─────┤
                                                                         ↓
                                                          Go actionable-set planner
                                                                         ↓
                                                            final scope admission
                                                                         ↓
                                                               Executor / workers
                                                                         ↓
                                                                  baseline publish
```

### Design Principles

1. **Durable truth lives in SQLite.** `local_state`, `remote_state`,
   `baseline`, `observation_issues`, `retry_work`, `block_scopes`, and
   `observation_state` are the durable authority surfaces. Observation and
   execution rebuild working state from those tables rather than inventing a
   second durable coordinator.
2. **Dirty buffering is runtime scheduling only.** The runtime uses a buffer to
   coalesce dirty signals and wake replans, not to define sync truth.
3. **SQLite owns structural diff and reconciliation.**
   `comparison_state` and `reconciliation_state` compute the latest
   snapshot-vs-baseline truth from SQLite, while Go owns actionable-set
   construction, durable retry/blocker reconciliation, dependency ordering, and
   final admission on the ready frontier.
4. **Watch-primary.** `sync --watch` is the primary runtime mode. All components serve both one-shot and watch modes.
5. **Interface-driven testability.** All I/O (filesystem, network, database) is behind interfaces.
6. **Boundary-owned error translation.** Each boundary normalizes the failures it understands once, and downstream layers consume that shared contract. See [error-model.md](error-model.md).

### Rationale

The current design deliberately keeps durable truth and executable planning
separate. Current and synced truth are explicit in SQLite, while the
worker-dispatching actionable set remains runtime-owned in Go. This preserves
the tested executor and dependency-graph runtime while snapshot persistence,
retry/state ownership, and
latest-plan materialization move into durable SQLite surfaces.

## Module Design Docs

For detailed module design, see:

| Module | Design Doc |
|--------|-----------|
| Graph API client and HTTP runtime | [graph-client.md](graph-client.md) |
| Configuration | [config.md](config.md) |
| Drive identity | [drive-identity.md](drive-identity.md) |
| Drive transfers | [drive-transfers.md](drive-transfers.md) |
| Retry infrastructure | [retry.md](retry.md) |
| Sync observation | [sync-observation.md](sync-observation.md) |
| Sync planning | [sync-planning.md](sync-planning.md) |
| Sync execution | [sync-execution.md](sync-execution.md) |
| Sync engine | [sync-engine.md](sync-engine.md) |
| Sync control plane | [sync-control-plane.md](sync-control-plane.md) |
| Sync store | [sync-store.md](sync-store.md) |
| Data model (schema) | [data-model.md](data-model.md) |
| Error model | [error-model.md](error-model.md) |
| Threat model | [threat-model.md](threat-model.md) |
| Degraded mode | [degraded-mode.md](degraded-mode.md) |
| Performance benchmarking | [performance-benchmarking.md](performance-benchmarking.md) |
| CLI commands | [cli.md](cli.md) |

## Cross-Cutting Reference Docs

| Topic | Reference |
|--------|-----------|
| New developer onboarding | [../reference/developer-onboarding.md](../reference/developer-onboarding.md) |
| Graph API quirks | [../reference/graph-api-quirks.md](../reference/graph-api-quirks.md) |
| OneDrive sync behavior | [../reference/onedrive-sync-behavior.md](../reference/onedrive-sync-behavior.md) |
| OneDrive glossary | [../reference/onedrive-glossary.md](../reference/onedrive-glossary.md) |
| Live incidents ledger | [../reference/live-incidents.md](../reference/live-incidents.md) |
| Live recurring families | [../reference/live-recurring-families.md](../reference/live-recurring-families.md) |
| Sync ecosystem patterns | [../reference/sync-ecosystem.md](../reference/sync-ecosystem.md) |

For live CI / E2E / integration debugging, open
[live-incidents.md](../reference/live-incidents.md) first for chronology and
evidence, then use
[live-recurring-families.md](../reference/live-recurring-families.md) as the
compact current-policy companion for recurring families.

## Source Of Truth Map

| Concern | Owner |
|---------|-------|
| Runtime failure class | `internal/errclass` + `internal/sync/engine_result_classify.go` |
| Sync-domain condition key and stored-condition grouping | `internal/sync/condition_keys.go` + `internal/sync/condition_projection.go` |
| Status title/reason/action rendering | `internal/cli/status_condition_descriptors.go` |
| Durable sync status facts | `internal/sync` SQLite tables (`observation_issues`, `retry_work`, `block_scopes`) plus catalog-owned account auth state |
| Account/auth presentation | `internal/authstate` vocabulary projected through `internal/cli/account_view_snapshot.go` |
| Read-only status snapshot | `internal/sync/store_inspect.go` |
| Production perf counters, live snapshots, and capture bundles | `internal/perf` with session/control-plane ownership split across `internal/cli` and `internal/multisync` |
| Durable mutation rules | `internal/sync` writable store APIs (`CommitMutation`, scope/failure helpers) plus engine-owned scope/result flow |

## Production Performance Instrumentation

`internal/perf` is the repo-owned measurement boundary for production-visible
performance data.

- `internal/perf` owns command/session collectors, aggregate counter rollups,
  live per-drive snapshots, and opt-in capture bundle creation.
- The perf snapshot includes stale-work counters by owning boundary
  (admission, worker-start local truth, worker-start remote truth, live local
  precondition, live remote precondition, and pending replan retirement), local
  observation commit/recovery counters, and replan-phase worker-idle timing.
- `internal/perf.Runtime` is the single owner of drive registration and
  capture admission, so perf capture does not route through a second
  extra runtime object just to gate one in-flight bundle.
- `internal/cli` owns command-session lifetime and human-visible rendering:
  final/periodic log summaries, `status --perf`, and the `perf capture`
  command.
- `internal/multisync` owns live sync-owner registration and the control-socket
  surfaces that expose live snapshots and trigger captures.
- Two recording routes exist. The engine records its own phases and counters
  (observe, plan, execute, refresh, watch batches, stale work, local
  observation, replan idle) on the collector it was constructed with. HTTP
  requests and retries, store transactions and transfers are recorded on the
  collector the call's context carries (`perf.FromContext`); a context that
  carries none records nothing. `perf.WithCollector` replaces the context's
  collector rather than adding one, and every collector forwards each record to
  its parent, so the rule for an owner is to carry the most specific collector
  in the context: `internal/multisync` puts each mount's collector in that
  mount's run context, and the record reaches the mount snapshot, the owner
  aggregate and the command collector once each.
- Logs are the durable history. `status --perf` and the control socket are
  live-owner views only and intentionally do not invent a new persistent perf
  database.
- Always-on production logs carry aggregate counters and timings only. Rich
  per-drive detail is reserved for explicit capture bundles.

## Verification Policy

Static verification is a first-class architectural constraint, not a best-effort hygiene step.

- `golangci-lint` runs with `default: none`; every enabled linter is an explicit policy choice.
- `depguard` enforces architectural import boundaries, and `forbidigo` bans raw filesystem `os.*` calls in production/runtime packages outside the explicit boundary packages (`fsroot`, `synctree`, `localpath`). Repo tooling under `internal/devtool` is allowed to use stdlib filesystem calls directly because it does not participate in product runtime authority. depguard matches a rule's `files` patterns against each file's absolute path, anchored, and golangci-lint rewrites nothing in them but `${base-path}` and `${config-path}`, so every path-scoped rule names its files from `**/`. Written relative to the repository, the leaf-package, `graph`-no-`config`, `driveops`-no-`sync` and `multisync`-no-`graph` rules matched nothing and were off without a word, which is how `tokenfile` and `retry` stopped being the leaves the list said they were; a temporary internal import in a listed leaf now fails lint, and a test fails on any pattern that cannot match (`TestGolangciConfig_DepguardFilePatternsMatchAbsolutePaths`). The leaf list holds every package that imports no internal package. `tokenfile`, `retry` and `fsroot`, each with one internal dependency, have one rule apiece in depguard's lax mode: it denies the module's `internal/` prefix and allows the one dependency by its exact path, and in lax mode an allow entry that matches with a longer prefix than the deny wins, so that import alone passes. A temporary import of another internal package fails lint in each, naming its rule. A rule names its packages by folder, so a renamed or removed package would leave its pattern matching nothing; a test fails on any `**/<folder>/*.go` pattern whose folder holds no non-test Go file (`TestGolangciConfig_DepguardPackagePatternsNameExistingPackages`).
- The devtool output-boundary check prevents production packages from bypassing the CLI-owned output contract with package-level `slog.Debug`/`Info`/`Warn`/`Error`, `log.Print*`, or direct `os.Stdout`/`os.Stderr` writes. The explicit product exceptions are `internal/cli/root.go` and `internal/cli/format.go`, which own the command output/status writers.
- Test-time synchronization and assertion style are verified by `cmd/devtool`.
  Direct `t.Fatal`, `t.Fatalf`, `t.Error`, and `t.Errorf` are banned in tests
  in favor of testify `assert`/`require`. Direct `time.Sleep` is banned in
  test code, so unit and integration tests use explicit readiness handshakes or
  injected clocks/timers instead of wall-clock waits. The narrow exception is
  the centralized live E2E helper that waits on external process or provider
  propagation and carries an explicit `nolint` justification. Every test of a
  file whose `//go:build` constraint names the `integration` tag, `TestMain`
  aside, is named `TestIntegration_...`, the prefix the integration tier
  selects, so a new integration test under another name fails the check
  instead of never running. The tag counts on either side of `&&` and `||`
  and not under a negation: a `!integration` file is built exactly when the
  tier is off (`TestFileBuildConstraintNames_FollowsNegationAndBothOperands`).
- A test's wait for an event that must come (a debug event, an observer
  batch, a pass result, a goroutine's return) is bounded by one named wait,
  long enough for a loaded two-core host under the race detector: `eventWait`
  (one minute) in `internal/sync`. A wait returns as soon as its event
  arrives, so the bound decides only how long a broken test hangs; ten-second
  bounds failed correct watch tests under load. A wait whose expiry is the
  passing path, because it shows that something does not happen, stays short
  and is written at its use. A test never waits unboundedly for a watch or
  loop to return, whether it runs in the background or is the test's own
  call. It starts one in a goroutine only through `runInBackground`
  (`runWatchInBackground` for `RunWatch`), whose cleanup cancels it and waits
  within `eventWait`, so a failing test never leaves one running into the
  next, and reads the result with `receiveWithin` or `stopWatch`; its own
  call of one goes through `runWithin`. Each fails naming what it waited for.
  A call whose state makes it return at once (a canceled context, a
  quiescent phase, a signal already queued) stays a plain call. Inside a
  `testing/synctest` bubble a blocked receive is reported as a deadlock
  instead, and virtual waits stay as they are.
- The chaos model (`TestModel_ChaosGraphConverges`) logs each seed's fault
  summary from a test cleanup, so a failing seed prints it. Its one `-race`
  seed is chosen to draw both faults of a create that took effect, a lost
  create answer and a session that timed out once the empty item existed, and
  the run asserts that it still does. The large-file session client
  (`scenarioSessionClient`) passes through the chaos layer too
  (`TestChaosGraph_SessionRouteDrawsFaults`).
- `nolintlint` requires both a specific linter name and a short justification. Unused exclusions are surfaced with `linters.exclusions.warn-unused`.
- Inline suppressions are reserved for the small documented exception set where the code is correct and the linter cannot express that shape cleanly: interface-mandated receiver shapes, validated subprocess/request dispatch that the linter cannot follow across helper boundaries, non-cryptographic jitter, `driver.Valuer` SQL `NULL` semantics, fixed placeholder SQL, and intentional test fixtures/mocks.
- `cmd/devtool` is the single repo-owned verification and worktree-bootstrap entry point. `go run ./cmd/devtool verify` exposes explicit profiles: `public` (format, lint, output-boundary check, test-convention check, README status check, build, race+coverage measured across packages with `-coverpkg=./...`, the coverage gate, and a compile-only `e2e_full` package guard), `default` (the `public` profile plus fast E2E), `quick` (format, the same repository rules, the host and cross-platform builds, the tagged vets, then `go test -short` without the race detector or a coverage profile; `--shard k/n` runs one slice), `lint` (golangci-lint, then the workflow lint), `platform` (the host build plus the same short tier, with `--shard`), `pr` (lint, then quick: what the required PR check runs on Linux, in one local command, and the DoD `pre-pr` stage's default), `e2e`, `e2e-full`, `integration` (the integration tests only: `-run=^TestIntegration_` over `./internal/graph/...` with the `integration` tag; graph's untagged unit tests belong to the unit tiers, and the test-convention check holds every test of an `integration`-tagged file to that prefix, `TestRunVerifyIntegrationRunsOnlyTheIntegrationTests`, `TestRunTestConventionsRequiresTheIntegrationPrefixInIntegrationFiles`), `stress`, and `failed-test`. With no profile argument a plain `verify` runs `default`. The full tier's `go test -race` deadline is 20 minutes on a host with at least the 3 cores of the macOS CI runner it was set on, and 180 core-minutes divided by `GOMAXPROCS` below that (90 minutes on 2 cores, 180 on one). The small-host budget is measured, not scaled from the 20 minutes: a host that small is a developer's machine, slower per core than a CI runner and busy with other work, and `internal/sync` took 63 minutes there under `-race` with `-coverpkg` on 2 shared cores, where twice the base deadline had ended a healthy run as a hang. The CI runners keep the deadline their job timeouts sit above; `--unit-timeout` overrides it (`TestUnitTestTimeoutForCoresGivesSmallHostsACoreBudget`). The short tier's `go test -short` deadline follows the same rule with its own numbers: 10 minutes at 3 cores or more, and 60 core-minutes divided by `GOMAXPROCS` below that (30 minutes on 2 cores, 60 on one). That budget is measured too: on 2 shared cores `internal/sync`'s short tier passed in 2 to 8 minutes, and ran into the 10 minutes inside a pre-PR gate with no test hung, the same tests taking 2.5 times as long when other work shared the cores. `--short-unit-timeout` overrides it (`TestShortUnitTestTimeoutForCoresGivesSmallHostsACoreBudget`, `TestRunVerifyShortTierDeadlineComesFromTheCoresByDefault`), and the test that holds every CI job timeout above its go test deadlines passes both tiers' CI-runner deadlines explicitly, so it reads the same on any host. The coverage gate's number is written once, in requirement R-6.10.4: `requiredCoverage` (`internal/devtool/verify_coverage.go`) reads it from `spec/requirements/non-functional.md` when `--coverage-threshold` is left at 0, its default, and fails when that line states no percentage or more than one; a positive value overrides it and a negative one reports the total without gating, and neither reads the requirement (`TestRequiredCoverageReadsRequirement`, `TestRequiredCoverageRejectsUnusableRequirement`, `TestRunVerifyZeroCoverageThresholdUsesRequirement`, `TestRunVerifyCoverageThresholdFailure`, `TestRunVerifyNegativeCoverageThresholdReportsWithoutGating`). Devtool and CI each used to carry their own number, and the two disagreed. The total is measured with `-coverpkg=./...`, so a statement counts when any package's tests execute it, not only its own package's. Every command a profile starts runs with `GOTOOLCHAIN` set to `go.mod`'s `toolchain` directive (its `go` directive when there is none) unless the caller set `GOTOOLCHAIN` itself, so a host whose system Go is newer builds, tests and lints with the same toolchain as CI (`internal/devtool/toolchain.go`, `TestRunVerifyPassesThePinnedToolchainToCommands`, `TestRunVerifyKeepsAnExplicitGOTOOLCHAIN`). `quick`, `lint` and `platform` are the per-PR CI lane (R-6.10.22): `-short` skips the tests marked exhaustive through `skipInShort` in `internal/sync` (the randomized model and its replay, chaos and crash variants, the reverse-dispatch re-run of every RunOnce scenario, the reconcile matrix, the planner model, multi-pass refresh variants), which the full profiles run. Shards select top-level tests by name length modulo the shard count (`^(.{n})*.{k}$`): a stable hash that needs no test list, spreads every family across the shards, and partitions all names so the shards of one count run every test exactly once (`TestVerifyShardPatternsPartitionEveryName`); a shard whose pattern matches nothing in a small package is expected, so the empty-run guard fails a shard only when no package ran one of its tests (`TestRunVerifyQuickFailsAShardThatRanNoTest`). Shortcut ownership boundaries are protected by narrow APIs, behavior tests, and manual drift sweeps during refactor increments rather than a brittle verifier-time custom checker. The stress profile is intentionally targeted, not "repeat every test in every package": it repeats `./internal/multisync` under `-race -count=50 -shuffle=on -timeout=20m`, the watch, runtime, shutdown, worker and concurrency tests of the `./internal/sync/...` tree (`stressSyncConcurrencyPattern`) under `-race -count=50 -shuffle=on -timeout=40m`, and the whole `./internal/sync/...` tree under `-race -count=3 -shuffle=on -timeout=40m`. Both sync steps name the tree, not the directory, so a package split out of `internal/sync` stays under stress instead of silently leaving the lane (`TestRunVerifyStressRunsExpectedSteps`). Every stress step runs through the empty-run guard and its tee: over a tree a package without a selected test is expected, so a step fails when no package of its target ran one, with a `-run` pattern or without (`TestRunVerifyStressFailsWhenConcurrencyPatternMatchesNothing`, `TestRunVerifyStressFailsAStepWhosePackagesHaveNoTests`), and its output reaches the log as go test prints it, so a hang on CI shows every line up to it and a job killed meanwhile keeps them (`TestRunVerifyStressStreamsTheRepeatStep`). One race iteration of the whole sync tree takes about six minutes on a CI runner since the WS9 harness and per-scenario invariant checks, so fifty repetitions of it no longer fit any budget; repetition goes where the 1-in-50 flakes have come from. The whole-tree stress step executes via `go test -json`, and once it exits the verifier extracts, from the output the tee captured, the slowest completed tests, keyed by package and test so same-named tests of two packages stay apart (`TestSummarizeStressSlowTestsKeysByPackage`), into the same repo-owned summary surface instead of relying on raw timeout dumps alone. The `failed-test` profile reads `--failed-log <path>`, extracts the last failed Go package/test from raw or `go test -json` output (a downloaded job log with its timestamps included, and a test a timeout panic lists as running) through the one go test log parser, `internal/devtool/go_test_log.go`, and reruns only that package/test before any broader profile rerun, through the no-tests-matched rule, so a rerun of a test renamed since the failing run fails instead of passing on nothing (`TestRunFailedTestVerification_PatternThatSelectsNothingFails`), with `-race` and with `-shuffle=<seed>` when the log shows the seed the failing package printed (`-test.shuffle <seed>`): the full tier runs under the race detector and the stress lane shuffles, so a rerun without either can pass on a failure only they produce (`TestRunVerifyFailedTestKeepsRaceAndTheShuffleSeed`). The verifier also exposes `--classify-live-quirks` for manual live verification: it reruns a tiny documented registry of known provider-consistency failures once and only downgrades them when the rerun passes. `--summary-json <path>` writes a machine-readable end-of-run summary with per-phase durations, per-bucket full-suite results (`not-run` buckets included, with the reason), the failed top-level tests of every failed step and bucket, quirk-event count, any classified reruns, and any recorded stress slow-test summaries; steps and buckets carry `pass`, `fail`, `skipped` (ran, and its tests skipped) or `not-run`. The `public` and `stress` profiles run every step whatever an earlier one returned and join the failures, because the nightly is their one run and stopping would hide the other steps for a day; only the coverage gate waits for a passing unit-test step, since a failed run leaves a partial profile (`TestRunVerifyPublicRunsUnitTestsAfterLintFails`, `TestRunVerifyPublicSkipsOnlyTheCoverageGateAfterUnitTestsFail`, `TestRunVerifyStressRunsEveryStepAfterOneFails`). The format step rewrites the tree, which cannot fail; `--check-format` instead lists what `gofumpt` and `goimports` would change and fails when anything would, and CI passes it to `verify quick` and `verify public` (`TestRunVerifyCheckFormatFailsOnUnformattedFiles`); profiles without a format step refuse the flag.
- The compile-only `e2e_full` guard uses a per-run temporary output path rather
  than a fixed shared filename in `os.TempDir()`, so parallel verifier runs do
  not serialize or fail on cross-process artifact collisions.
- The verifier implementation is split by concrete ownership inside `internal/devtool`: `verify.go` owns profile orchestration and shared runners, `verify_quick.go` owns the per-PR lane (the quick, lint, platform and pr profiles and test sharding), `toolchain.go` owns the Go toolchain pin read from `go.mod`, `verify_dod.go` owns the DoD stages and `verify_dod_ci_wait.go` their two CI waits, `verify_summary.go` owns end-of-run summaries, `verify_e2e.go` owns fast/full live-suite orchestration, `verify_stress.go` owns stress profiles, `verify_coverage.go` owns the coverage gate and the reading of its number from R-6.10.4, `verify_test_match.go` owns the empty-run guard of the go test steps and the tee they stream through, and `verify_workflow_lint.go` owns the workflow lint. This split is structural only; `go run ./cmd/devtool verify ...` remains the single behavior contract and developer entrypoint.
- Verifier tests mirror that ownership split instead of one monolithic test file: `verify_runner_test.go` covers profile orchestration and summaries, `verify_quick_test.go` covers the per-PR lane and sharding, `verify_coverage_test.go` covers the coverage gate and its requirement, and `verify_test_helpers_test.go` owns the shared fake runner.
- `go run ./cmd/devtool verify --dod --stage <stage>` layers repo-owned Definition of Done automation onto the same verification entrypoint. The staged flow is explicit: `start` fetches recent merged-PR review threads into a local ignored manifest for manual classification, `pre-pr` runs the selected local verification profile, `pr` when none is named (`verify default --dod --stage pre-pr` keeps the full tier as the gate; `TestDODPrePRRunsThePRProfileByDefault`, `TestDODPrePRKeepsTheFullTierSelectable`), and proves the branch contains `origin/main`, `pre-merge` waits for required PR CI, posts templated evidence-bearing replies for every classified unresolved thread, resolves those threads, and rechecks unresolved thread state, and `post-merge` wraps `gh pr merge`, handles the known multi-worktree `main` checkout quirk by verifying server-side merge state, fast-forwards the root `main`, removes the increment worktree/branch, reruns cleanup audit, and waits for the push-to-`main` CI run of the merge commit to succeed; a skipped push run is a failure, since every push to `main` runs the quick lane. Review-thread reads page through GitHub's `reviewThreads` connection before enforcing zero unresolved threads, and post-merge cleanup tolerates already-completed merge/worktree/branch deletion steps so reruns can finish the audit tail. Before `git worktree remove`, cleanup removes exactly what `.worktreeinclude` put into the worktree, read from the root checkout's file: an `@` entry only while it is a symlink, a copied entry only while it is a regular file. Anything else at an entry's path (a real directory where the `.testdata` symlink belongs, say) fails the stage naming the path and is never deleted, and the root checkout itself is refused (`TestRemoveBootstrapEntriesRemovesOnlyWhatBootstrapCreated`, `TestRemoveBootstrapEntriesRefusesARealDirectoryAtASymlinkEntry`, `TestRemoveBootstrapEntriesRefusesTheRootCheckout`, `TestDODCleanupRemovesBootstrapEntriesBeforeTheWorktree`).
- The two DoD CI waits (`pre-merge` on the pull request's required checks, `post-merge` on the merge commit's push run) end on no progress, not on a fixed deadline: queued live jobs legitimately wait behind another pull request's run. Each poll fingerprints the state of every check, required or not; the wait fails when the fingerprint has not changed for the stall window, and the message names the checks still pending (`TestWaitForPRCI_ProgressExtendsTheWait`, `TestWaitForPRCI_StallEndsTheWaitNamingThePendingChecks`, `TestCIStallWindow_IsTheLongestJobTimeoutPlusAMargin`). The window is the longest `timeout-minutes` among the jobs of `ci.yml` that run for the awaited event, `pull_request` before merge and `push` after it, plus 10 minutes: 30 minutes for a push to `main`, which runs only the PR lane, and 50 for a pull request today, set by the `e2e` job's 40. It is read from the workflow (`internal/devtool/ci_workflow.go`), so there is no second copy of any number: each job's `if:` is evaluated for the event in the forms the workflow uses (`always()`, `github.event_name` compared with `==` or `!=`, and a repository-variable comparison, counted as true because assuming the live jobs are configured only lengthens a window), and each timeout is a number or exactly `${{ github.event_name == '<event>' && N || M }}`, which the `e2e` job uses for its fast tier on a pull request and its full tier otherwise. Any other condition or expression, and a job without a timeout, fails the stage before it waits, so a new job cannot silently leave the window (`TestCIStallWindow_PostMergeUsesOnlyPushJobs`, `TestCIStallWindow_PullRequestUsesOnlyPullRequestJobs`, `TestCIWorkflow_RejectsAnExpressionItDoesNotKnow`). A required check that has not started, whose job sits in a concurrency group (`concurrency: <name>` or a map of `group:` and `cancel-in-progress:`, a plain name; any other form fails loudly), can wait without any state change for one whole run of that group from another event, a pull request's `e2e` behind the nightly's `e2e-full`. While such a check has not started, the window is the longest timeout any job of its group has on any event, plus the margin (190 minutes for `e2e` today); once it starts, the state change restarts the window and the event's own applies (`TestWaitForPRCI_CheckQueuedInItsGroupWaitsOneRunOfTheGroup`, `TestWaitForPRCI_RunningCheckStallsAtTheEventsWindow`). Pull request #883 showed which status the rollup gives each kind of waiting check: while a dispatch run's live jobs ran on the same branch, its `integration` and `e2e` checks, held by their concurrency group, read `PENDING` with no conclusion, the checks waiting only for a runner (`verify-lint` and the six `verify-unit` shards) read `QUEUED`, and the jobs skipped by their `if:` read `COMPLETED` with conclusion `SKIPPED`. The rule is unchanged: every status but `IN_PROGRESS` and `COMPLETED`, and a required check not yet reported, counts as not started, and both observed waiting statuses fall under it (`TestWaitForPRCI_CheckQueuedInItsGroupWaitsOneRunOfTheGroup` runs a `PENDING` and a `QUEUED` check). Every `gh` request of the waits and of the other DoD stages goes through one client (`internal/devtool/gh_retry.go`): a request GitHub answered with an HTTP 5xx, whose connection dropped, or that never reached GitHub (an i/o or TLS handshake timeout, a refused connection, a failed name lookup, an unreachable network) is repeated after 5, 10 and 20 seconds, at most four attempts in all, and then fails with the request's name and GitHub's answer; any other failure is final at once. A rate limit (a 403 secondary limit, a 429) is final too, because a repeat within seconds only extends it; the stage is run again later. A review reply is posted once, because a 5xx can arrive after GitHub posted it, and `gh pr merge` is attempted once and then checked against the server's merge state. A rerun request is repeated, because GitHub refuses to rerun a run that is running rather than start a second attempt; that refusal ("This workflow is already running") after a 5xx means GitHub started the rerun anyway, so the run is read again and the wait goes on when it reads in progress, and fails when it still reads completed (`TestGHClient_RetriesOnlyTransientFailures`, `TestGHClient_RetriesARequestThatNeverReachedGitHub`, `TestGHClient_RateLimitsAreFinal`, `TestGHClient_RefusalAfterA5xxMeansTheRequestTookEffect`, `TestWaitForPRCI_RepeatsAPollGitHubFailed`, `TestWaitForPRCI_RerunGitHubStartedDespiteA5xxIsWaitedFor`, `TestWaitForPRCI_RefusedRerunOfAFinishedRunFails`). `--ci-timeout` is a hard cap that ends even a wait that keeps changing; its default is one stall window for the first attempt and one per allowed rerun, plus the longest window of a check waiting in its concurrency group (`TestWaitForPRCI_HardCapEndsAWaitThatKeepsChanging`, `TestNewCIWaitConfig`, `TestCIStallWindow_PullRequestUsesOnlyPullRequestJobs`). A rerun of the same check lists a second entry beside the first, so checks are reduced to the newest `startedAt` per workflow and name, and a failure outranks checks still pending (`TestClassifyPRChecks_UsesTheNewestRunOfARepeatedCheck`, `TestClassifyPRChecks_FailureOutranksPending`). A required check that concluded `failure` or `cancelled` is examined through its run (`gh run view <id> --json conclusion,headSha,jobs,status,updatedAt,url`, the id taken from the check's details URL): a job other than the aggregate `verify` that failed or timed out fails the wait at once and names the job; otherwise, once the run has completed and only if it is a run of the head commit (of the merge commit after merge), its cancelled jobs are rerun with `gh run rerun <id> --failed`, at most `--ci-max-reruns` times per wait (default 2), after which the wait fails naming the cancelled jobs and the run. A cancelled run of a superseded commit is never rerun; the head commit's own run decides, under the same stall window. The run's `updatedAt` at rerun time is remembered, so a run that still shows it has not started the rerun and is not rerun twice (`TestWaitForPRCI_CancelledShardIsRerunOnceAndTheWaitThenSucceeds`, `TestWaitForPRCI_ThirdCancellationFails`, `TestWaitForPRCI_CancelledRunOfAnOlderCommitIsNotRerun`, `TestWaitForPRCI_RealJobFailureIsNotRerun`, `TestWaitForPRCI_WaitsForTheRunToFinishBeforeRerunning`, `TestWaitForPostMergePushCI_CancelledPushRunIsRerunOnce`). Skipped and timed-out checks are not rerun (`TestClassifyPRChecks_OnlyFailedOrCancelledChecksNameARunToExamine`).
- Nightly failures are owned by increments, not watched by a person (R-6.10.23, `internal/devtool/nightly.go`). The `start` stage, and `go run ./cmd/devtool nightly check` by itself, reads the latest completed scheduled run of `ci.yml` on `main` and derives one key per failure: the top-level test that a failed job's log names through the one go test log parser (a `--- FAIL:` line behind the job log's timestamps, a go-test JSON fail event, or a test the timeout panic lists as running), or `job-<name>` when the log names no test (the aggregate `verify` job is skipped when another job failed). Its run list, run view and job-log reads go through the retrying `gh` client, so a transient 5xx no longer ends the `start` stage (`TestNightlyCheck_RetriesAJobLog5xxThenSucceeds`, `TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound`). Ownership is a claim ref on GitHub, `refs/nightly-claims/<key>`: one commit on the empty tree whose subject is a JSON record (state `claimed` or `fixed`, owner branch, host, run, time, fix commit). Every transition is a compare-and-swap push (`git push --force-with-lease=<ref>:<value last read>`), which git rejects atomically when the ref has moved, so of two agents that read the same nightly at the same moment exactly one creates the claim and the other reads it and reports the failure as held elsewhere. A claim is a 24-hour lease renewed by every check from its owner; an expired one is taken over by the next increment that checks, by the same swap, so a dead session cannot park a failure. `pre-merge` refuses a PR whose body does not name every key its branch holds (`nightly release` hands one back), and `post-merge` swaps each of the branch's claims to `fixed` with the merge commit. A `fixed` claim keeps other increments off the failure until a nightly contains the fix: if the key still fails in a run whose head contains the fix commit, the fix did not hold and the next check claims it again; once a run containing it passes, the ref is deleted. Claim refs are outside `refs/heads` and `refs/tags`, so they trigger no CI and appear in no branch list.
- DoD PR-comment actionability is intentionally not inferred by tooling. The manifest records GitHub's thread state (`isResolved`, `isOutdated`, path, line, and comments), while the agent or human must label each unresolved thread as `fixed`, `already_fixed`, or `non_actionable` with the required evidence fields. The tool enforces reply clarity and resolution, not semantic judgment.
- `go run ./cmd/devtool cleanup-audit` is the read-only git-state cleanup classifier. It fetches/prunes `origin`, then classifies local worktrees, local branches, and remote branches as `safe_remove`, `keep_attached`, `keep_dirty`, `keep_unmerged`, or `keep_main` without deleting anything.
- `go run ./cmd/devtool bench --scenario <name> [--subject <id>] [--runs N]
  [--warmup N] [--json] [--result-json <path>]` is the repo-owned benchmark
  runner entrypoint. The delivered slices measure the
  `startup-empty-config` controlled scenario and the manual
  `sync-bidirectional-catchup-100m` live representative scenario against the
  built `onedrive-go` binary, emit a subject-aware JSON result bundle, and keep
  release report publication in the dedicated benchmarking design. The subject
  is built with the toolchain `go.mod` pins, through the `withPinnedToolchain`
  every verify profile uses, so a host whose Go is newer measures the binary
  CI builds (`TestPrepareBenchSubject_BuildsWithThePinnedToolchain`).
- Observation coordination is intentionally single-root. `internal/sync/engine_primary_root.go`
  owns the primary remote-root plan for the configured drive, while
  `internal/sync/engine_run_once.go`, `internal/sync/engine_watch.go`,
  `internal/sync/engine_watch_observation.go`, and
  `internal/sync/engine_watch_maintenance.go` execute that one root in
  one-shot, watch, and full-reconcile paths. Shared-root fallback policy lives
  alongside that execution rather than behind a separate session/phase
  framework, so the runtime reflects the real product model directly.
- `go run ./cmd/devtool worktree add --path <path> --branch <branch>` is the canonical way to create new worktrees from `origin/main`. It runs `git fetch origin main` first and stops if the fetch fails, so the base is the current `origin/main`, not the last one fetched (`TestAddWorktreeFetchesOriginMainBeforeCreatingTheWorktree`, `TestAddWorktreeStopsWhenTheFetchFails`). It applies `.worktreeinclude` immediately so the new worktree is ready for fast E2E and local development. An entry the root checkout lacks, such as `.testdata` on a host without live-test credentials, is skipped with a notice on stderr; the live suites then fail their own preflight (`TestBootstrapWorktreeSkipsIncludeSourceMissingFromRoot`).
- `cmd/devtool` is exercised both through package-level command assembly tests and black-box process tests for `verify` profile parsing plus `worktree bootstrap/add` semantics, including explicit `--source-root` selection. The documented developer entrypoint and the shipped binary must stay aligned.
- Fast E2E is mandatory in the default local `default` profile. The harness loads typed live-test config from exported env plus root `.env` and durable `.testdata/fixtures.env` defaults; verification does not silently skip fast E2E based on exported shell variables.
- `verify e2e` is intentionally a minimal live smoke lane: verifier-owned auth preflight, verifier-owned fast fixture preflight, `TestE2E_FileOpsSmokeCRUD`, `TestE2E_Sync_UploadOnly`, `TestE2E_Sync_DownloadOnly`, `TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount`, and `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression`, plus any non-live helper tests that happen to share the package.
- Repo verification regression-tests that contract via tagged-test discovery: `internal/devtool/verify_runner_test.go` asserts the `//go:build e2e` set still contains only the intended smoke live tests while the demoted live coverage remains full-only.
- The dedicated fast preflight tests are verifier-owned entrypoints, not part of the main fast suite contract. The verifier sets `ONEDRIVE_E2E_RUN_AUTH_PREFLIGHT=1` only for `TestE2E_AuthPreflight_Fast` and `ONEDRIVE_E2E_RUN_FAST_FIXTURE_PREFLIGHT=1` only for `TestE2E_FixturePreflight_Fast`, so the subsequent `go test -tags=e2e` pass does not silently rerun them.
- The fast auth preflight owns the one scrub of live suite artifacts. Later fast invocations set `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1`, so the fast fixture preflight and the main smoke suite reuse the already-scrubbed drive state instead of redoing the remote cleanup work.
- The per-PR fast E2E lane no longer uses `-race`. Race-heavy coverage stays in the default unit/public verification, stress, and `e2e-full` profiles where the extra compile and startup cost is acceptable.
- Required verification compiles the tagged `e2e_full` package with `go test -c -race -tags='e2e e2e_full' ./e2e` before any live suite runs. The compile-only `-c` form is intentional: the E2E package owns a live `TestMain`, so `go test -run=^$` would still execute credential validation and remote scrub side effects instead of acting as a pure build guard.
- Shared-file fixture resolution is identity-based, not recipient-slot-based. If
  more than one configured test account can open the same raw share link, the
  harness accepts that fixture as long as every matching configured recipient
  resolves to the same remote drive/item identity and then picks a stable
  recipient candidate deterministically.
- Known shared-folder fixture visibility through `drive list --json` is
  eventual because the command depends on best-effort
  `GET /me/drive/root/search(q='*')` discovery. Full-suite tests that validate
  exact shared selectors or compare successive shared-discovery outputs must
  poll across one ordinary read-only propagation window and skip that
  assertion if Graph never exposes the same known fixture on that run, rather
  than treating one empty search pass as a product regression. Provider-only
  skips carry a stable `provider_skip` reason such as
  `drive_list_shared_selector_omitted`, `shared_folder_name_omitted`,
  `shared_discovery_gateway`, or `shortcut_root_placeholder_omitted`.
- Shortcut fixture coverage uses stable manually-created placeholders rather
  than trying to create shortcuts through Graph. The durable fixture contract in
  `.testdata/fixtures.env` names the writable shortcut parent drive, writable
  shortcut alias, sharer email, and sentinel path; the read-only shortcut parent
  drive, alias, sharer email, and sentinel path; plus the standalone shared
  folder name used to prove explicit shared-folder drives remain standalone.
  The fixture folder names, sentinel paths and the standalone folder name
  have defaults (the `default*Name` and `defaultShortcutSentinelPath`
  constants in `testutil/testenv.go`); the two shortcut parent drives do not.
  The repository names no test account, so nothing guesses which configured
  drive holds a shortcut: they come only from
  `ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE` and
  `ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE`, and a run that sets neither
  fails the shortcut tests with a message naming both. In CI the e2e job's
  masking step states the mapping by variable name, never by value: the
  read-only shortcut lives in drive-1 (`ONEDRIVE_TEST_DRIVE`) and the writable
  one in drive-2 (`ONEDRIVE_TEST_DRIVE_2`), and `scripts/ci-mask-test-drives.sh`
  exports both fixture variables from those drives. Exported variables win
  over `.testdata/fixtures.env`, so a value the vault file also sets cannot
  conflict. The current canonical manual fixture set is: the writable shortcut
  folder owned by drive-1's account and shortcut-added in drive-2's root, the
  read-only folder owned by drive-2's account and shortcut-added in drive-1's
  root, and the standalone folder owned by drive-2's account and added as an
  explicit `shared:` drive for standalone shared folder coverage.
  Full shortcut rename/move tests mutate only the manual shortcut placeholder,
  restore its original alias and parent path in cleanup, and validate local
  projection/state reuse through sync output and status. They do not depend on
  provider-specific direct traversal through `/shortcut` in the parent drive.
  If the parent root listing decodes successfully and direct shared-target
  sentinel download succeeds after the shorter provider-omission proof window,
  the helper can classify a missing root placeholder immediately instead of
  spending the full shortcut fixture propagation budget.
  Provider-backed shortcut alias deletion/manual-discard E2E tests are
  intentionally out of scope: Microsoft Graph does not document a reliable
  create/recreate API for these shortcuts, so deleting a manually seeded CI
  placeholder would make the fixture unrecoverable. Delete, final-drain, and
  manual-discard behavior stays covered by fake-Graph and unit/integration
  tests.
- Full-suite sync tests that stress concurrent uploads must assert eventual
  sync convergence, not single-pass perfection. A one-shot `sync --upload-only`
  run may legitimately persist retryable transient `retry_work` when one exact
  upload hits a live 5xx service outage, and the next sync pass is the
  contract-owned recovery path.
- Every one-shot `sync` in a live test whose assertions read only the end state
  goes through a converge helper in `e2e/e2e_test.go`. Both helpers run their
  passes through a `syncRunner`, the drive selection of the pass: the default
  drive, one named drive (`...ForDrive`), or every configured drive in one pass
  (`...AllDrives`). `runSyncThroughTransientFailures` repeats a must-succeed
  pass while it recorded retry work and left work behind, and returns the last
  pass's summary and every pass's debug log; it does nothing for delta lag.
  `requireSyncEventuallyConverges` repeats a pass until the caller's predicate
  holds, for state that reaches the client only through delta;
  `syncPassFinishedWork` lets that predicate also require that the pass left no
  work. A sync whose proof is the pass itself (an `Uploaded` or `Up to date`
  line, a conflict, a dry run, an idempotence check, a denied write it must
  record, a resume of a killed transfer) stays one call with a
  `Single pass by design:` comment saying why: a converge loop around it would
  hide a regression where a pass that should finish in one go needs two.
- Before any test runs, `TestMain` syncs each suite drive download-only into a
  throwaway folder and records what the account itself leaves unsynced
  (`recordStandingSyncConditions`, `testutil.StandingSyncConditions`). A shared
  folder whose owner took it away is not synced on any pass and the sync
  command rightly exits 1 on each (R-6.6.28); no test made that condition and
  no test can clear it. `runCLICore` then gives a test the pass it would have
  seen without it (`settleStandingSyncConditions`): a pass that exited 1 and
  left only recorded mounts, each with no more items than recorded, comes back
  as the output the command prints without them and no error, and the test's
  debug log and the quirk summary (`standing_sync_condition`) say so. A pass
  that left anything else, one more item or another mount, is returned as the
  command printed it. The baseline's notice names a drive by role and gives
  counts only, and what the passes printed goes to the suite debug log
  `standing-baseline`. A baseline pass that leaves items on a line naming no
  mount stops the suite, because no later pass could tell them from a test's
  own left work.
- The manual full E2E suite is layered on top of the fast suite. Its files use `//go:build e2e && e2e_full`, so the canonical invocation is the verifier's `e2e-full` profile, which sets both tags, preserves the fast-then-full ordering, and runs the full shared-folder fixture preflight before the `e2e_full` buckets. A fast-suite test failure does not stop the full buckets, which still run and whose failures join it; a preflight failure (auth, fast fixture or full fixture, marked by `errE2EPreflight`) stops the lane, because the account or the fixtures are unusable, and the summary then lists every bucket as `not-run` with the reason (`TestRunVerifyE2EFullDoesNotMaskUnknownFastSuiteFailure`, `TestRunVerifyE2EFullRecordsBucketsNotRunAfterPreflightFailure`).
- `e2e_full` owns the demoted live coverage that no longer runs on every PR: drive-list catalog checks, shared-file discovery checks, richer caller-proof/logout coverage, extended direct file-operation coverage, current condition/status rendering, slower sync state/recovery checks, engine-owned conflict protection, richer shortcut child-mount checks (`TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount`, `TestE2E_Shortcut_ExplicitStandaloneSharedFolderRemainsConfiguredDrive`, `TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible`, `TestE2E_Shortcut_RenameReusesChildMountState`, `TestE2E_Shortcut_MoveReusesChildMountState`, `TestE2E_Shortcut_WritableUploadSyncsToSharedTarget`, `TestE2E_Shortcut_ReadOnlyBlockedUploadStatus`, `TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes`, `TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget`, and `TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot`), and websocket startup smoke. Recurring live E2E does not delete manual shortcut placeholders because Microsoft does not document a supported Graph create/recreate contract for Add-to-OneDrive shortcuts; local delete lifecycle remains covered by fake-Graph/unit integration tests. When Graph keeps the shared target reachable but omits the parent Add-to-OneDrive placeholder for the full shortcut fixture budget, shortcut E2Es skip because there is no root placeholder for the product path to observe.
- `e2e_full` runs as three explicit verifier-owned buckets instead of one monolithic package invocation: `full-parallel-misc` (`-parallel 5` for tests already marked safe), `full-serial-sync`, and `full-serial-watch-shared`. The verifier executes those buckets sequentially after the single full-suite preflight so only the vetted misc bucket regains concurrency, and it runs every bucket whatever an earlier one returned: a failure in one says nothing about the others, and stopping at it hid their state for the night and let a fix claimed for a test in a later bucket read as confirmed although the test never ran. The bucket failures are joined, each bucket's failed top-level tests are named in the summary, and a cancellation records the buckets it kept from starting as `not-run` (`TestRunVerifyE2EFullRunsEveryBucketAfterOneFails`, `TestRunVerifyE2EFullReportsEveryFailingBucket`). Every bucket and the classified paths stream their output through a tee instead of capturing it until the command exits, so a job killed mid-bucket keeps every line printed before. Manual full-suite coverage names current product workflows only: live file operations, sync convergence, engine-owned conflict protection, shortcut child mounts, and websocket startup smoke.
- The one-time full-suite preflight still owns the global live-drive artifact scrub. Bucketed `go test` runs set `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1` so they reuse that cleaned fixture state instead of re-scrubbing both configured drives before every bucket process.
- When `verify e2e` or `verify e2e-full` runs without an explicit `--e2e-log-dir`, the verifier resolves the repo-owned default temp log dir once up front, scrubs stale timing and quirk artifacts there, and passes that same `E2E_LOG_DIR` through the fast suite, the full-suite preflight, and every full bucket so live diagnostics stay in one place.
- Every go test the live and stress profiles start names its own deadline: each preflight `10m`, the fast suite `10m`, the buckets `15m` (`full-parallel-misc`), `40m` (`full-serial-sync`) and `40m` (`full-serial-watch-shared`), well above their recorded nightly durations of about 2, 15 and 16 minutes, and each one-test classified rerun `10m`. Because every bucket now runs, the deadlines add up, and the deadlines one CI job's profile can start, every classified rerun included, plus ten minutes of setup must fit inside that job's `timeout-minutes`: the `e2e` job's 180 on the schedule and manual dispatch and the `stress` job's 120 (20m + 40m + 40m) are set from that sum, and `TestCIJobTimeoutsCoverEveryGoTestDeadline` reads them from `ci.yml` for the event each profile runs on (with `verify-unit`, `verify-full` and `integration`) and fails when a deadline outgrows its job. On a pull request the `e2e` job runs the fast tier, three 10-minute deadlines, so its timeout there is 40 minutes, and the `integration` job's one 5-minute deadline makes its 15 (its runs take about 2 minutes): exactly each need, because they set the pull request's DoD stall window, and `TestCILiveJobsPullRequestTimeoutsAreTheirNeed` keeps them from drifting looser. When the job's timeout wins instead, the log carries no goroutine dump naming the hung test and devtool never writes its summary.
- Websocket-specific E2E timing starts only after `observer_started(remote)`, and the timed mutation must target a subtree that bootstrap already materialized locally. Waiting merely for the socket connection is not enough on initial startup or daemon restart, and mixing the timed wake assertion with fresh parent-folder creation reintroduces the same delta-lag race the websocket harness is supposed to isolate from.
- Managed shortcut child projections are mount-root engines and intentionally emit a `websocket_fallback` debug event with note `mount_root`; the parent drive's primary-root watcher can still establish socket.io. Websocket smoke tests treat that child fallback as expected and only fail on primary-root endpoint/connect/fallback failures.
- Historical isolated shared-folder mount-root fixtures did not become ready just because owner-drive `stat /folder` succeeded. The helper must also wait for `ls /` against the derived mount-root drive to succeed before upload-only or watch tests start; otherwise Graph can still return `404 itemNotFound` on the mount-root children route for that same fresh parent.
- Mount-root remote read helpers must re-establish that same `ls /` boundary before and after known route-lag failures. Owner-drive path visibility or an earlier successful mount-root listing does not prove later exact-path `stat`, `get`, or other mount-root read pollers will avoid the documented `items/{rootID}/children` `404 itemNotFound` family.
- A live test that waits for a file a running `sync --watch` daemon uploads waits for the file's size or content, never for its name alone. A new file is created empty under its final name and filled by a second request ([drive-transfers.md](drive-transfers.md), "New files and their placeholder"), so `stat` and `ls` show the name while the file has 0 bytes, and a test that stops the daemon, or edits, deletes or overwrites the file, on the name alone can act between the two requests and leave the empty item behind (LI-20261110-01). `waitForRemoteStatSize` and `waitForRemoteStatSizeForDrive` wait until `stat --json` reports the file's size, which for a new file changes only when its content commits (`TestRemoteStatSizeReady`); `waitForRemoteFileContent` waits until a `get` yields exactly the expected bytes, and is the wait when the test compares content next or the upload replaces an existing file, whose old content stays until the new commits. Name-only waits remain for files written by a command that has already exited (`put`, `cp`, a one-shot `sync`), which has no upload in flight.
- `--classify-live-quirks` is intentionally narrow. Today it covers `LI-20260405-04` (fast `TestE2E_Sync_DownloadOnly`), `LI-20260405-06` (strict auth preflight `/me/drives` propagation lag), and `LI-20260405-03` (the isolated `full-serial-watch-shared` recurrence of `TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart`). Each rerun answers only its incident's shape: the auth preflight is rerun only when its output shows `TestE2E_AuthPreflight_Fast` itself failing, not after a build failure, an empty pattern or a timeout (`TestRunVerifyE2EFullDoesNotRerunAuthPreflightWithoutItsTestFailing`). Unknown failures, or repeated failures after the one rerun, stay red, and a failed rerun reports its own error, carrying the first attempt's (`TestRunVerifyE2EFullReportsTheAuthPreflightRerunFailure`).
- PR `e2e` and nightly or manual `e2e-full` runs emit repo-owned verifier artifacts: the `--summary-json` report, the shared E2E `timing-summary.json` file, and the shared E2E `quirk-summary.json` file. The timing summary aggregates remote-write visibility, delete-disappearance, scope-transition, and sync-convergence wait windows across the fast suite, the full preflight, and all full buckets. The quirk summary records repo-owned live-provider evidence observed by the harness itself: CLI-emitted `graph.QuirkEvidence`, strict auth-preflight retry decisions, and fixture-helper recurrence classifications emitted by the helper that actually observed the recurrence.
- CI uses `--classify-live-quirks` only for the nightly and manual `e2e-full` runs. Required per-PR verification remains strict, with one narrow and deliberate exception: a **provider read-only window** skips the live lane instead of failing it.
- The exception exists because red has a meaning. A failing required check asserts that the change under test is broken, and when Microsoft answers `403` with an inner `serviceReadOnly` on a read unrelated to the diff, that assertion is false. Authentication still works and the drive is still readable; only the service is refusing. Reporting "the suite could not run" is the accurate outcome, and it is strictly more informative than a red check the reader has to go disprove. The skip is keyed on that one explicit Graph code and nothing else — every other live failure stays red, including ordinary `accessDenied`. The verifier records a skipped auth preflight as `skipped`, never `pass`, and prints one line that names LI-20260823-01 and no drive (`TestRunVerifyRecordsASkippedAuthPreflightAsSkipped`); the steps after it run as before.
- Retry policy follows the same reasoning. `serviceReadOnly` is excluded from the `drives-token-propagation` quirk family: that family exists for post-token projection lag, which clears in seconds, and spending its full budget on a read-only window that has been observed lasting hours only converts a decided failure into a slow one (LI-20260823-01).
- Every go test step of every profile fails when no package of its target ran a test (`runGoTestMatching`, `internal/devtool/verify_test_match.go`): the race and short unit tiers and their shards, the integration tier, the stress steps, the live preflights, suites and buckets, the classified reruns and the `failed-test` rerun. The one go test devtool starts outside it is the compile-only `go test -c` of the `e2e_full` package, which runs no test by design. `go test` exits 0 when a `-run` pattern selects nothing and when its packages have no test files, so a step whose target was renamed, deleted or emptied keeps reporting success while verifying nothing. The stress profile carried exactly that for months: a run pattern targeting watch-ordering stress probes that no file has ever defined, reported as `ok`. The guard tees the step's output, streaming it as it is printed, and passes the step only when a line shows a test that ran: a package result line without the `[no tests to run]` marker (`ok`, or `FAIL` for a binary that ran; one that reports `[build failed]` or `[setup failed]` ran nothing), a verbose `--- PASS` or `--- FAIL` line, or a `-json` event naming a test that passed, failed or skipped. The marker alone cannot decide: over a `./pkg/...` tree every package without a match prints it, and a tree of `[no test files]`, a build failure in `-json` output or an empty output prints none. One package or a tree, the rule is the same (`TestAssertTestsMatched_AcceptsTreeWithSomeEmptyPackages`, `TestAssertTestsMatched_RejectsTreeWhereNothingRan`, `TestAssertTestsMatched_RejectsATreeWithNoTestFiles`, `TestAssertTestsMatched_ABinaryThatNeverRanRanNoTest`, `TestAssertTestsMatched_AcceptsVerboseAndFailedPackages`, `TestAssertTestsMatched_HandlesJSONOutput`, `TestRunVerifyE2EFullFailsASuiteThatRanNoTests`). When the command failed as well, both errors are reported, the empty run first. The guard is looser than a per-package one: a step whose tests still run in one package passes however many other packages lost theirs. The unit tiers keep their output as before: the guard tees it to the same stdout and stderr, and the coverage gate reads the profile file, not the output (`TestRunVerifyPublicFailsAUnitTierThatRanNoTest`, `TestRunVerifyQuickFailsAShardThatRanNoTest`). The devtool tests' fake runner prints what go test prints for a passing run of its arguments (`passingGoTestOutput`), so a profile test meets the guard as a real run does.
- Build-tagged sources are vetted, not merely compiled. `go vet ./...` never sees `e2e`, `e2e_full`, or `integration` code, so the verifier runs `go vet` against each tagged set alongside the compile guard. Without it, a vet finding in live-test code could surface only on a live run — the slowest and least available lane in the repo.
- The `defer`-close rule in CLAUDE.md is enforced, not merely stated. `errcheck` previously skipped every source line containing `defer `, which switched off exactly the check that matters: `Close` flushes buffers, so a dropped error on a written file is silent data loss. Production code now carries a justified `//nolint:errcheck` at each reader-side close (response bodies, SQL cursors and statements, read-only file and rooted handles), so a new close on a written file fails lint by default. Test files keep a documented blanket exemption because they are not the data-integrity boundary.
- `internal/sync` exports only what other packages use. Its top-level exported surface is 66 identifiers, down from 217; the removed 151 were package-private in practice and, being exported, were invisible to `unused` and `unparam`. Narrowing them immediately surfaced four dead types and a production function whose only callers were its own tests.
- Exactly three identifiers stay exported without an external consumer: `Action`, `ItemType`, and `ProtectedRoot`. Each is a core domain type whose name is also the natural local-variable name throughout the package, so unexporting them produces pervasive `govet` shadowing rather than clarity. This is a documented exception, not an oversight.
- `unparam` deliberately keeps `check-exported: false`. It cannot see callers in other packages, so enabling it reports exported functions as unused that other packages do use. The real coverage gap it implies is over-export inside `internal/sync`, which is addressed by narrowing that package's surface rather than by a flag that produces false positives.
- Atomic managed-file writes sync twice, and the second one is the easy one to
  forget. Syncing the temp file makes its contents durable; the rename that
  gives those contents their final name is a directory update, and that update
  is not durable until the directory itself is synced. Skipping it leaves a
  window where a crash loses the rename even though every byte reached the
  disk -- the exact outcome the write-temp-then-rename pattern exists to rule
  out. A directory sync that fails is reported rather than swallowed: the
  contents are already visible, but success would claim a guarantee the
  filesystem refused, and rewriting the same bytes is safe to retry. This
  holds for both atomic writers: `fsroot.Root.AtomicWrite` for managed state
  (config, tokens, upload sessions) and `localpath.AtomicWrite` for arbitrary
  paths (trash records). Both tolerate the one case where the sync cannot
  happen, a filesystem that answers `EINVAL`/`ENOTSUP`/`EOPNOTSUPP` to an
  fsync on a directory (FUSE and SMB mounts, home folders among them),
  through `localpath.IsDirSyncUnsupported`, which is the single definition
  of that errno set for every caller that syncs a directory after a rename
  (`driveops.publishPartial`, the trash's cross-filesystem copy and restore,
  both atomic writers). Without it every config and token write on such a
  folder failed. `fsroot` imports `localpath` for that one predicate rather
  than keeping a copy or moving it into a package of its own: `localpath`
  imports no internal package, `synctree` already imports it for the same
  test, and a second copy of the errno set is the drift a single definition
  exists to prevent. `fsroot` syncs the directory through its rooted handle.
  The seams are `localpath`'s unexported `atomicWrite` with an injected
  directory sync and `fsroot`'s `syncDir` op, because an fsync has no
  observable effect to test (`TestAtomicWrite_SyncsTargetDirectory`,
  `TestAtomicWrite_ReportsDirectorySyncFailure`,
  `TestAtomicWrite_ToleratesUnsupportedDirectorySync`,
  `TestRoot_AtomicWrite_SyncsTargetDirectory`,
  `TestRoot_AtomicWrite_ReportsDirectorySyncFailure`,
  `TestRoot_AtomicWrite_ToleratesUnsupportedDirectorySync`).
- The lint version is checked, not just pinned. CI installs a pinned
  golangci-lint, but nothing compared that pin against the version a developer
  runs locally, and a different version is a different lint policy rather than
  a cosmetic difference. Local verification then passes on rules CI does not
  have, or misses rules it does, and the disagreement only surfaces after a
  push -- which is exactly how a `noctx` rule absent from an older local build
  failed CI on a branch that had just verified clean. `devtool verify` now
  reads the pin out of the workflow, so the two cannot drift apart through a
  second copy of the number, and refuses to lint against a different version.
  It also refuses a linter built with another Go minor release than `go.mod`'s
  toolchain (`built with goX.Y` in `golangci-lint --version`): a linter built
  with a newer Go cannot read the pinned toolchain's export data and fails
  every typecheck. Both refusals print the install command with
  `GOTOOLCHAIN=<pinned toolchain>` and a `GOBIN` of the `PATH` directory that
  holds the linter that ran, since installing anywhere else leaves the stale
  binary first on `PATH`
  (`TestAssertLintVersionMatchesCI_BuiltWithAnotherGoPrintsTheRebuildCommand`,
  `TestAssertLintVersionMatchesCI_BuiltWithThePinnedMinorPasses`).
- The workflow files are linted, not left for GitHub to parse. No Go test
  reads `.github/workflows` as GitHub does, so a workflow GitHub refuses to
  load (a `run:` written as a plain scalar that holds `: `, which YAML reads
  as a mapping inside the scalar) passed every test and every profile. The
  lint profile's second step, `workflow lint`
  (`internal/devtool/verify_workflow_lint.go`), runs the pinned actionlint over
  every `.yml` and `.yaml` file there:
  `go run github.com/rhysd/actionlint/cmd/actionlint@<version> -shellcheck= -pyflakes= -oneline <files>`.
  `go run` with a version fetches the module once, checks it against the
  checksum database and caches the build, outside the repository's module
  graph, so neither CI nor a developer host installs anything for it and
  `go.mod` carries no tool dependency; the version is the one constant
  `actionlintModule`. shellcheck and pyflakes are off because actionlint uses
  them whenever they are on `PATH`, and a runner that happened to have one
  would lint by another policy. The step runs under a five-minute deadline
  (the first run's download and build take under a minute) and its error
  names each finding's file and line (`TestVerifyLint_RefusesAnInvalidWorkflowFile`,
  which runs the pinned actionlint itself and so needs the module proxy the
  first time, `TestRunVerifyLintRunsThePinnedActionlintOverEveryWorkflow`,
  `TestRunWorkflowLint_NamesEveryFinding`). A finding is a line that starts
  with one of the workflow files the lint was given: a Go compile error has
  the same `<file>:<line>:<column>:` shape, and a failed build of actionlint
  itself returns the command's error alone
  (`TestRunWorkflowLint_ACompileErrorIsNotAFinding`). A tree without workflow
  files has nothing to lint (`TestRunWorkflowLint_TreeWithoutWorkflowsRunsNothing`).
- `synctree` error text names paths inside the sync root relative to it
  (`stating docs/a.txt: ...`), matching the rest of the user-facing output
  (cli-output-guidelines §5); only the sync folder itself is named by its full
  path. Each operation names its path once (`nameForMessage`); the absolute
  path is used only for the not-exist/unsafe-path re-check (`absForCheck`).
- Path containment failures carry `synctree.ErrUnsafePath`. They are the
  definition of an unsafe path, and callers classify on that sentinel to tell a
  permanent path problem from a transient one: shortcut-root planning treats it
  as blocked and needing the user, and everything else as unavailable and worth
  retrying. Without the sentinel a path that escapes the root read as merely
  unavailable and was retried forever instead of surfacing. The executor's
  translation to its own escape sentinel matched on message text before this,
  which is fragile across a package boundary and could only recognize the two
  phrasings it happened to know.
- Synctree refusals are `treeRefusalError` values: `errors.Is` matches
  `ErrUnsafePath` or `ErrUnsupportedTreeEntry`, but the error has no `Unwrap`
  and its message is the detail alone ("a/b is a symbolic link", "parent
  folder a is not a folder"). Wrapping the sentinel with `%w` made the bare
  sentinel the innermost error, and the sync console, which reports the
  innermost error as the reason, printed "sync tree path is unsafe" and
  dropped the part that named the path. Evidence:
  `TestRootRefusals_KeepSentinelAndShowTheDetailAsInnermostError`.
- Requirement traceability is checked in both directions, and the reverse
  direction is the one that matters. Forward: a `Validates:` or `Implements:`
  reference must resolve to a declared requirement, so a renamed requirement
  cannot leave citations pointing at nothing. Reverse: a requirement marked
  `[verified]` must have evidence -- a test citing it, or a design doc
  implementing it or its group. A verified status with neither is a claim about
  behavior nobody demonstrated, and unlike a dangling citation it reads as
  proof. The rule found five such claims, two of which described a status
  projection with no test touching it at all. Unique: one requirement line
  declares each ID, heading or criterion. Two lines with one ID make every
  citation of it ambiguous, and the status of either can stand in for the
  other's; two requirements once shared R-2.8.12 and every check passed. The
  check names each line that declares a duplicated ID
  (`TestRequirementTraceability_RefusesAnIDDefinedTwice`).
- Every mutex states what it guards, and `devtool verify` fails when one does
  not. Which data a mutex protects is never inferable from the declaration:
  field order suggests an answer that is frequently wrong. The rule was written
  after finding `activeScopesMu` sitting directly above three fields while
  guarding only the first -- the two below it are owned by the single runtime
  goroutine and never taken under that lock. A reader who trusted the layout
  would have added a fourth field expecting protection that was never there.
- The documentation model is machine-checked, not conventional. `devtool verify` fails when a production Go file has no governing design doc, when two docs claim one file at equal pattern specificity, when a `GOVERNS:` pattern matches nothing, when a design doc cites a test that does not exist, or when CLAUDE.md's routing table points at a path that no longer resolves. The last rule matters most: a requirement marked verified on the strength of a renamed or deleted test is a false claim, and only tooling catches that reliably. A companion rule governs historical provenance: `B-NNN` identifiers refer to a backlog that no longer exists, so every one cited in Go code must be registered in `spec/reference/retired-backlog-ids.md`. Verification fails when code introduces an unregistered identifier, embeds one in a Go name, or the registry keeps an entry nothing cites any more. That keeps a dead vocabulary explicitly labelled as historical instead of letting it read as a live cross-reference, and stops it from spreading.
- Dependency risk is monitored, not assumed. `govulncheck` runs on every CI verify and fails the build on a vulnerability this code actually reaches; Dependabot proposes weekly Go-module and GitHub-Actions updates. Both matter more than usual here because the product holds OAuth refresh tokens and because token refresh depends on a fork of `golang.org/x/oauth2` that will not receive upstream advisories automatically.
- CI is two tiers (R-6.10.22), and the PR lane is parallel by construction. On every pull request and every push to `main`, `verify-lint` (golangci-lint and govulncheck) runs beside `verify-unit`, a matrix of Linux `verify quick --shard k/n` jobs and macOS `verify platform --shard k/n` jobs, so the critical path is the slowest single job, not the sum; the shard list in `ci.yml` is the one place the shard count lives. The full tier runs on the nightly schedule (`0 10 * * *`, 02:00 or 03:00 Pacific) and on manual dispatch: `verify-full` runs `verify public` on both platforms with the race detector and every exhaustive test, `stress` runs the race repetition profile, and the live `e2e` job runs `e2e-full`. The trade-off is deliberate and documented in CLAUDE.md and CONTRIBUTING.md: a data race, a coverage drop or an exhaustive-tier failure can reach `main` and is caught by the next nightly run. The local pre-PR gate matches the PR lane: `verify --dod --stage pre-pr` runs the `pr` profile by default, because the full race tier does not fit a 2-core host's budget, and `verify default` remains the full local tier for a host that can finish it. Push to `main` is no longer skipped for squash merges: branch protection no longer requires a PR branch to be up to date, so a stale-branch merge can break `main`, and the push run of the quick lane catches it within minutes; the DoD post-merge stage waits for that run. The live jobs never run on push to `main` (their shared accounts serialize runs, the PR run exercised the change, and the nightly covers `main`).
- Every platform README.md claims is verified, not assumed. Both the PR lane and the full tier run on `ubuntu-latest` and `macos-latest`, because the tree carries darwin-only behavior (filesystem identity, the non-Linux watcher path) that a Linux-only lane cannot exercise. A tiny aggregate `verify` job gates on `verify-lint`, `verify-unit` and `verify-full`. The indirection is load-bearing: branch protection requires a check named exactly `verify`, and a matrix reports its legs as `verify-unit (ubuntu-latest, 1/3)` and so on, so pointing protection at the matrix directly means the required name never reports and every PR stays blocked. The aggregate keeps the required name stable no matter how many jobs and shards the lane grows, treats a job skipped by its own event condition as success (the full tier on a PR, the PR lane on the schedule), and fails a run in which nothing ran. The coverage gate is enforced on the Linux full-tier leg only, at the number R-6.10.4 states (the workflow carries no copy of it), and reported without gating on macOS, since a secondary platform compiles a different build-tagged file set and would legitimately land on a different percentage. The macOS leg still runs the unit tier with `-coverpkg=./...` and `-coverprofile`, which instrument every package in every test binary, and that costs time on the slower runner, so it was measured: on the full-tier run 43130170152 (manual dispatch on 2026-11-12 UTC, head 3003e3ee) the macOS `verify-full` job took 22 min 25 s and the Ubuntu one 11 min 17 s, of a 45-minute timeout; the macOS unit step took 16 min 46 s on 3 cores, its slowest package, `internal/sync`, 852.5 s of go test's 20-minute per-binary deadline (5 min 48 s to spare), against 9 min 55 s and 506.4 s on Ubuntu's 4 cores (the earlier run 43103299254 took 19 min 6 s and 14 min 56 s as jobs). The deadline applies to each test binary, so no package came within five minutes of it and the coverage flags stay on both legs; a macOS leg within five minutes of that deadline is the point at which a negative threshold should skip `-coverprofile` and `-coverpkg`. On top of that, `runBuild` cross-compiles every supported GOOS on every `quick` and `public` run, so a platform claim cannot decay into a build failure that only a user discovers.
- CI keeps the expensive runtime stress lane out of the required per-PR path, and runs it nightly and on manual dispatch so race-heavy watch/runtime regressions surface within a day. Stress runs upload the verifier `--summary-json` artifact so timeout growth and slowest-test evidence remain visible even when the package step stays green; the `verify-full` legs upload theirs too, one per platform. The live jobs print only variable names and file counts while they fetch credentials, never a drive identifier, a secret name or a token file name, because the repository's logs are public (R-6.10.27). The drive identifiers and the account allowlist are not in job-level `env`, whose values GitHub prints in every step's header: each live job receives them in the step right after its checkout, each as `${{ secrets.NAME || vars.NAME }}` so that a secret of the variable's name, if one is created, wins and is masked by GitHub everywhere. That step runs `scripts/ci-mask-test-drives.sh`, the one copy of the logic for both jobs: it masks every spelling of them the job, the tests or Graph URLs in debug output can print (each identifier and allowlist entry, the account address inside it, the vault-secret and token-file spellings, and the lower-cased and percent-encoded form of each, `@` as `%40` and `:` as `%3A`); lists those spellings in `$RUNNER_TEMP/onedrive-test-masks`; and exports the values, and each drive's Key Vault secret name (`<NAME>_VAULT_SECRET`) and token file name (`<NAME>_TOKEN_FILE`), through `$GITHUB_ENV`, printing nothing itself, so the credential steps derive no spelling. It keeps to bash 3.2, because the devtool tests run it on both CI platforms (`TestCIMaskScript_MasksEverySpellingAndExportsTheNames`). With repository variables that one step's header still shows the values once per job; a repository secret of the same name removes that last occurrence. Masking covers the log only. The e2e recorders write every configured drive by role (`drive-1`, `drive-2`, and `drive-1-account` for its address) through one helper, `testutil.DriveRoles`, that the timing and quirk summaries and every debug-log chunk go through; a debug log's file name is the test name redacted before it is sanitized, the auth preflight's subtests are named by role, the suite's own failure messages and log lines write drives through the same helper, and TestMain names the second drive's variable, not the drive (`TestDriveRoles_RecordedLinesHoldNoConfiguredDrive`, `TestE2ERecordersRecordRolesNotDrives`). No tracked file spells a test account: `TestE2E_TrackedFilesNameNoTestAccount`, in the quick live tier because only the live jobs know the addresses, lists the tracked files with `git ls-files` and fails on any line that holds a configured drive's or an allowlist entry's address in any case or percent-encoded, naming the file, the line and the account's role (`testutil.FindTestAccounts`, `TestFindTestAccounts_NamesFileLineAndRoleNeverTheAddress`); as a second layer the `e2e` job replaces every listed spelling with `***` in `/tmp/e2e-debug-logs` before uploading its artifacts, and uploads nothing from outside that directory: the verifier's `--summary-json` file is written there too. A failure message that quotes the CLI's own output is not rewritten by the suite; it reaches only the job log, which the masks cover. The test-account allowlist is read by one function, `testutil.CheckDriveAllowed`, which the live suites and the live benchmark share and whose error names the variables, never a value (`TestCheckDriveAllowed_NamesTheVariablesAndNoValue`); the `.env` files are read by one reader too, `testutil.LoadDotEnvFile`, which returns its errors to the live benchmark and to `LoadLiveTestConfig` and exits only in the thin harness wrappers `LoadDotEnv` and `LoadTestEnv` (`TestLoadDotEnvFile_ReturnsErrors`). `TestCIWorkflowSetsNoTestDriveInJobEnv` holds the workflow to all of this. Each `verify-unit` and `verify-full` leg prints its runner's core count, which the test tiers' durations and the full tier's deadline formula depend on.
- Managed repo-state files use `internal/fsroot` root capabilities (R-6.4.10).
  Every `fsroot` call beneath the root -- open, stat, directory listing,
  removal, rename, an atomic write's temp file, subdirectories and directory
  sync, and the absence check that maps a failure onto `os.ErrNotExist` --
  resolves its name through an open `os.Root` descriptor.
  The name check in `cleanRelative` is lexical, so it cannot see a symlinked
  directory below the root; until the mutations were rooted too, such a link
  redirected a remove, rename, temp file or mkdir outside the root while reads
  were refused. The root directory itself is the one directory created by
  path (`Root.MkdirAll`): it is the anchor every handle opens on, and `Open`
  works before it exists. Its own path may be a symlink (a config folder
  linked into a dotfiles checkout), and links that stay inside the root are
  followed. `os.Root` has no `CreateTemp`, so a temp file is claimed with
  `OpenFile(O_RDWR|O_CREATE|O_EXCL, 0600)` on a random name, retried on
  `os.ErrExist` at most `tempNameAttempts` times; `O_EXCL` never opens an
  existing entry, so a name another writer holds or a symlink planted at it is
  left alone (`TestRoot_Remove_RefusesSymlinkedParent`,
  `TestRoot_Rename_RefusesSymlinkedParent`,
  `TestRoot_AtomicWrite_RefusesSymlinkedParent`,
  `TestRoot_Stat_RefusesSymlinkedParent`,
  `TestRoot_AtomicWrite_RefusesDirectoryCreationBelowSymlinkedParent`,
  `TestRoot_CreateTemp_RefusesSymlinkedDir`,
  `TestRoot_AtomicWrite_FollowsSymlinksThatStayInside`,
  `TestRoot_AtomicWrite_TempNameCollisionRetries`,
  `TestRoot_CreateTemp_StopsAfterBoundedNameCollisions`). Temp files are
  created only inside an atomic write; the package exports no temp-file
  creation of its own.
  A failure reads as `os.ErrNotExist` only when the name is absent: the
  failure must itself say not-exist or not-a-directory, and a rooted lstat of
  the name must agree. Before, any failure on a name that turned out absent
  read as "does not exist", and a create always targets an absent name, so a
  log file in a folder the user cannot write failed with "file does not
  exist" instead of "permission denied", and a full disk or a read-only mount
  read the same way. The lstat is still needed because ENOTDIR also answers
  an operation that wanted a directory and found a file, and that name is
  there; a name below a file is absent. `synctree` applies the same errno
  guard to the sync tree (below) (`TestRoot_OpenFile_CreateFailureKeepsItsErrno`,
  `TestRoot_OpenFile_PermissionDeniedIsNotNotExist`,
  `TestRoot_AtomicWrite_PermissionDeniedIsNotNotExist`,
  `TestRoot_NameBelowAFileIsNotExist`).
  The root follows the same rule. A root that is missing, or whose path runs
  through a file, is not there, and nor is anything beneath it. A root that is
  something other than a directory (a file, a FIFO, a device) is there, so
  listing it fails with ENOTDIR as listing any file does, but every name
  beneath it is absent. A stat of the root's path decides this before
  anything opens the root (`checkRootIsDirectory`), never after: `os.OpenRoot`
  opens the path for reading, which blocks forever on a FIFO, and it reports a
  root that is a regular file with a bare "not a directory" error carrying no
  errno. An open that fails after the stat found a directory keeps its own
  error. Before, that bare error reached callers as neither absent nor
  ENOTDIR, and the CLI re-derived it by stat'ing each managed file's parent,
  which still missed a file two folders up
  (`TestRoot_RootThatIsAFileHasNothingBeneathIt`,
  `TestRoot_RootThatIsAFIFOAnswersWithoutOpeningIt`,
  `TestManagedPathExists_DistinguishesAbsentFromUnreadable`).
  An atomic write creates the root and every missing folder above and below
  it with its `dirPerm`, so its callers do not create the root first
  (`TestRoot_AtomicWrite_CreatesMissingFoldersWithDirPerm`).
  The sync state database is the one managed file that does not go through
  `fsroot`. SQLite opens it, and creates its `-wal` and `-shm` files next to
  it, by path inside its own file layer, and the `database/sql` driver takes a
  file name, never a descriptor opened through an `os.Root`; only the
  creation of its folder is rooted (`openSyncStoreWithDriver` in
  `internal/sync/store.go`). That is why R-6.4.10 names config, tokens,
  upload sessions and logs and leaves the database out. Redirecting the
  database would take a symlink planted in the user's own data folder, and
  write access there already reaches the token beside it.
- Sync-runtime filesystem operations under one configured sync root use `internal/synctree`. Managed shortcut projection roots use opt-in `synctree` helpers for no-symlink ancestor validation, safe component creation, root-contained path-state checks, rooted renames, empty-target replacement, byte-identical tree comparison, no-follow tree removal, and platform device/inode identity capture for same-parent shortcut alias rename detection; ordinary content sync symlink policy remains owned by the existing sync observation/execution rules.
- Every `synctree` filesystem call — read, write, and destroy alike — resolves its name through an open `os.Root` descriptor. Mutating operations are part of the rooted `rootHandle` surface specifically because they are the ones that destroy data when containment is skipped: a lexically validated absolute path handed to package-level `os.Rename` follows a symlinked ancestor out of the sync root and silently replaces whatever sits at the destination. `synctree` therefore performs no mutation from an absolute path: the one absolute-path entry point, `RemoveAbs`, converts through `Rel` (which rejects anything outside the root) before delegating to the rooted `Remove`. Its injectable ops take a rooted handle plus a rooted-relative name, so a future call site cannot reintroduce the escape.
- `synctree` offers three escalating strictness tiers, and callers must pick deliberately: rooted operations (`MkdirAll`, `Remove`, `RemoveAll`, `Rename`, `Chtimes`) contain the path but follow symlinks that stay inside the root; `*NoFollow` variants (`MkdirAllNoFollow`, `RemoveEmptyDirNoFollow`, `ValidateTreeNoFollow`, `DirEmptyNoFollow`) additionally reject symlinks even when they resolve inside the root; and `ValidateNoSymlinkAncestors` is the standalone ancestor assertion used where the mutation itself happens outside `synctree`. No `synctree` operation removes a tree it has not checked: `RemoveAll` checks nothing, so sync calls it only for bundled junk a folder delete found disposable all the way down, and a user's tree leaves by the verified local delete, one entry at a time (`TestProductCode_RemovesATreeOnlyByTheVerifiedLocalDelete`). Its refusal names which problem it found ("parent folder a is a symbolic link" or "parent folder a is not a folder"), since a symlinked ancestor is the everyday case under `follow_symlinks`.
- `synctree` never follows a link out of the sync root, for reads as well as writes. Reading content through a followed alias (R-2.4.6) is a sync observation/execution policy, not a `synctree` capability: observation walks and the upload source precondition stat followed paths through `localpath`, and every local mutation stays on the rooted capability.
- `RemoveEmptyDirNoFollow` performs its final removal with a rooted `unlinkat(AT_REMOVEDIR)` against the parent descriptor rather than `os.Root.Remove`. `os.Root` exposes no directories-only primitive and its `Remove` unlinks regular files, so using it would delete a file that replaced the directory after the emptiness check. The explicit `AT_REMOVEDIR` call keeps both containment and the directories-only contract that makes the final syscall the race guard.
- `synctree` error normalization is a safety boundary. `normalizeNotExist` maps rooted failures onto `os.ErrNotExist` so callers can branch on absence, but it first asserts that no ancestor is a symlink. The unrooted `lstat` it consults follows symlinks, so for a path beneath a symlinked ancestor it answers about a location outside the sync root; left unguarded it reports "this path escapes the root" as "nothing is here", which callers read as permission to create. Only a positive `ErrUnsafePath` determination overrides the caller's original error. An errno other than `ENOENT`/`ENOTDIR` is kept as is: a create always targets a missing name, and "does not exist" in place of `ENOSPC`, `EROFS` or `EACCES` hid a full disk from R-2.10.60 (`TestRoot_CreateFailureKeepsItsErrno`, `TestErrnoShim_DownloadFailuresAreClassifiedThenHeal`).
- `synctree.Root.Spelling` is the one place that turns a stored path into the spelling on disk (R-2.13.2). Sync stores paths in NFC; a byte-exact filesystem (ext4, Btrfs, XFS) answers ENOENT for the NFC spelling of an NFD-named entry, so callers resolve an action's path once and address the entry by the result. The exact path is tried first with one rooted lstat, so the common case costs nothing more; only a missing path is walked component by component, and a missing component is looked up in its parent's listing for the single entry with the same NFC form. A component nothing matches ends the walk and keeps the caller's spelling for it and everything below, which is how a new entry gets its stored name inside resolved parents. Two matches are a local name collision (R-2.12.3) and resolve nothing. `SpellingOfParent` resolves only the parents, so creating many entries in one folder never lists that folder per entry. Neither returns an error: an unusable path comes back as given and the operation that follows reports why. The rooted operations themselves do not resolve on their own: an absence check (the `-wal` sidecar probe, a new download's target) would then list its folder every time, and `Abs` paths handed to `localpath` readers would still bypass it. The emulated case-insensitive volume folds both forms like macOS, which is why only real-filesystem tests cover this.
- `synctree.Root.Interpose` is the filesystem seam for tests (D-094): it wraps the `Handle` every rooted operation opens, and the engine applies it through the nil-in-production `engineInputs.interposeSyncTree`. `internal/synctest` provides the shims: `CaseInsensitive` (a case-insensitive, normalization-insensitive, case-preserving volume; a case variant is the stored entry's inode, as a hard link would be), `ErrnoShim` (rule-based errno injection by operation and path) and `IdentityShim` (an entry reports a chosen inode number and birth time through every stat, listing and walk, which is how inode reuse is reproduced: the kernel cannot be made to reuse an inode on demand) (`TestCaseInsensitive_VariantNamesAreTheStoredEntry`, `TestCaseInsensitive_RenameRespellsOrReplaces`, `TestErrnoShim_FailsMatchingOperationsUntilHealed`, `TestCaseInsensitiveShim_RemoteCaseOnlyRenameConverges`, `TestIdentityShim_ReportsTheRuledIdentityEverywhere`).
- `synctree.FileIdentity` is (device, inode, birth time), and `synctree.SameIdentity` is the one Go definition of "the same entry": equal device and inode, and equal birth times whenever both are known (R-2.17.1). The birth time comes from `statx(2)` `STATX_BTIME` on Linux, read by path after each rooted stat and for each walked or listed entry and kept only when the mask bit is set and statx names the same device and inode (`withBirthTime`), and from `st_birthtimespec` on macOS, FreeBSD and NetBSD; elsewhere it is unknown. A FileInfo may state its own birth time (`BirthTimeReporter`), which the shims use. Because Linux FileInfo values are wrapped to carry it, `synctree.SameFile` replaces `os.SameFile` for anything the sync tree returned (`TestSameIdentity_BirthTimeDecidesWhenBothSidesKnowIt`, `TestIdentityFromFileInfo_EveryReadReportsTheSameBirthTime`, `TestIdentityFromFileInfo_LinuxBirthTimeIsStatxBtime`). Reads outside the seam see the real filesystem: upload sources, content hashing and followed symlinks go through `localpath` by absolute path (R-2.4.6), and the read-only probe reads `statfs` directly, so a shim cannot make a folder read-only.
- Arbitrary local file paths outside those rooted domains use `internal/localpath` as the explicit trust boundary. `localpath.AtomicWrite` is the default arbitrary-path writer when replace-in-place semantics matter. `localpath.WriteDisposableFile` is reserved for disposable operational artifacts such as perf capture manifests; durable config, token, state, or sync-tree content writers must not use it.
- A folder the client creates is one of three kinds, and its mode follows the kind (R-5.10.2). A folder in a user's tree (anything sync creates below the sync folder, shortcut folders and their parents included; a folder `get` downloads into and a missing parent of a `get` target; a missing parent of a file restored from the trash) asks for `localpath.UserTreeDirPerm`, 0777, and the kernel applies the umask, as for the files in it (0666 minus the umask). The sync folder the client creates (`cli/sync_dir.go`) is private, 0700: it is the container the client makes on its own initiative. Managed state (config, sign-ins, sync state, engine locks, logs, trash entries, upload sessions, the control socket, performance captures, detached copies) keeps the mode its package names. An existing folder is never chmodded. Every folder below a sync folder is a user-tree folder, so `synctree.Root.MkdirAll` and `MkdirAllNoFollow` take no mode: the sync tree asks for `localpath.UserTreeDirPerm` itself, `MkdirAll` through its `rootOps.mkdirAll` seam like the tree's other rooted mutations, and sync code cannot pass another mode (`TestRoot_CreatedFoldersFollowTheUmask`, `TestRoot_MkdirAll_FailureInjection`). `localpath.UserTreeDirPerm` is the one home of the user-tree mode: `TestFolderModes_EveryCreatedFolderNamesAClassifiedMode` parses every production file (devtool excepted) and fails on a folder-creating call (`Mkdir`, `MkdirAll`, `MkdirAllNoFollow`, and every function or struct-field function that passes its own parameter on as such a mode, found to a fixed point, which brings in `AtomicWrite`, the other writers that create parents and the sync tree's seam) whose mode is a literal, a computed value or a constant missing from its table of classified constants, and on a classified constant no call uses; `TestFolderModes_OneHomeForTheUserTreeMode` keeps that table to one user-tree constant; `TestFolderModes_TheSyncTreeCreatesOnlyUserTreeFolders` fails when a method of `synctree.Root` takes a folder mode from its caller, or when `Root.MkdirAll` or `ensureNoFollowDirectory` (which makes each folder of `MkdirAllNoFollow`) asks for anything but the user-tree constant or creates nothing. The scan has no type information: it resolves a call by its name, its argument count and the packages its file imports, and checks every argument an ambiguous call could take as a mode. It cannot tell a folder outside the sync tree that belongs to a user's tree but is created with a managed-state or sync-folder constant from a correct one (a restore's missing parents used to get the trash's own constant, and the scan could not have caught it); it does not see a folder made by a call it does not know as one (`syscall.Mkdir`, `unix.Mkdirat`, a function value passed around, a wrapper reachable only through an unnamed function literal); and inside `internal/synctree` it holds only the two functions above, so a new folder-creating function there that asks for a classified managed-state constant would pass (the detached-copy folder in the trash legitimately does). `os.MkdirTemp` takes no mode (0700) and is outside the scan.
- A test that needs a umask runs in a child test binary: `synctest.RunUnderUmask` starts the binary again for that one test with the umask in its environment, and the child sets it before the test's work. The umask belongs to the whole process, so setting it in a binary running tests in parallel would change what every other test creates (`TestRunUnderUmask_RunsTheTestUnderTheUmask`).
- Append-only log file creation in `internal/logfile` is the single documented exception to atomic replace-style writes. It is allowed because the log is an append-only operational artifact, not an authoritative config/state file.
- `fsroot.Root` and `synctree.Root` carry unexported injectable ops so managed-state and sync-runtime I/O failure paths can be tested deterministically without package-level test hooks. `fsroot`'s ops are `openRoot`, `tempSuffix` and `syncDir`: no hook takes an absolute path (`syncDir` is handed the directory already opened through the rooted handle), so failure injection substitutes a fake rooted handle (`TestRoot_AtomicWrite_FailureInjection`, `TestRoot_AtomicWrite_ReportsDirectorySyncFailure`, `TestRoot_ReadFile_ReportsReadAndCloseFailures`, `TestRoot_Open_ReportsRootAndFileCloseFailures`), the directory sync's answer is injected through `syncDir` (`TestRoot_AtomicWrite_ToleratesUnsupportedDirectorySync`), and the steps of writing the temp file are tested on a fake file (`TestWriteTempFile_ReportsEachStepFailure`). `synctree`'s `rootOps.syncDir` is the directory sync a detached file's cross-filesystem copy performs on the trash entry it lands in (`spec/design/sync-execution.md` → local trash); `localtrash.Trash` carries the same kind of seam (`syncDir`, `renameNoReplace`) for its restore path.

- Family layering inside `internal/sync` is machine-checked. Each sync design doc declares a `LAYER:` rank beside its `GOVERNS:` line, and `devtool verify` fails when a file owned by a ranked doc reads a package-scope symbol owned by a higher-ranked doc. The compiler cannot help here -- every sync family compiles into one package, so a reference from the store up into the engine is as legal to it as any other -- but the families are real, each doc already declares which files it owns, and the direction between them is a design decision worth keeping true.

  The declared order is `sync-domain` (0), `sync-store` (1), `sync-planning` and `sync-observation` (2), `sync-execution` (3), `sync-engine` (4). Same-rank references are permitted because two families can be genuinely mutually recursive without either sitting above the other.

  `sync-shortcut` is deliberately unranked. Its shell is written as `*Engine` methods that call Graph and mutate the filesystem, so the family still reads seven engine-owned symbols; ranking it requires inverting that dependency behind an interface the shortcut family declares. A doc without a `LAYER:` line is not checked in either direction, which makes the set of unranked families the remaining work -- visible in the docs, and only shrinking. That is a more honest instrument than an allowlist of individual exceptions, which grows quietly.

## Planned Improvements

- Split `internal/sync` by authority boundary. At 32k LOC across ~130 production files it owns unrelated reasons to change, which this doc's own guidance calls a god package. **The blocker is measured, not suspected, and it is the same at every granularity tried.** [planned]

  Candidate slices and their coupling to the rest of the package, counted as distinct top-level symbols crossing each direction:

  | Candidate slice | Files | LOC | needs from rest | rest needs from it |
  | --- | --- | --- | --- | --- |
  | `planner*` | 4 | 1,025 | 66 | 12 |
  | `executor*` + `worker*` | 10 | 3,362 | 65 | 22 |
  | `shortcut*` | 13 | 3,672 | 42 | 27 |
  | `scope*` | 5 | 925 | 38 | 29 |
  | `store*` | 15 | 3,911 | 76 | 38 |
  | `observer*` + `scanner*` | 13 | 5,771 | 100 | 47 |
  | `engine*` | 38 | 8,605 | 213 | 39 |

  Every slice is bidirectionally coupled, so none is a leaf and none can be extracted as a file move. The inbound direction is usually a legitimate interface — what the rest needs from `planner*`, for example, is `planner`, `newPlanner`, `plannerMountContext`, and the `plannerVisible*` helpers, which is exactly the surface a planner package would export. The outbound direction is the problem: each slice consumes a large shared vocabulary (`Action`, `ScopeKey`, `ItemType`, `BaselineEntry`, `pathView`, the `*Row` types) that lives at the centre of the package.

  So the prerequisite for **any** split is extracting that vocabulary into its own package, giving a hypothetical `syncdomain` <- `syncstore`/`syncplanner`/... <- `sync` (names used for illustration only; no such packages exist, and the earlier packages of similar names were collapsed into `internal/sync`). That is a design decision rather than a refactor, and it has a real cost: a vocabulary package risks being the "fake intermediate boundary" this doc warns against. It is defensible only if the domain types keep their invariant-preserving behavior and the package owns the sync domain model rather than acting as a DTO bag. Two further constraints: 14 test files reach into store internals (`rawDB()`, direct `m.db`) from `package sync` and must be re-homed, and cross-package tests will need explicit seams, which pushes back against the export-surface reduction already done.

  Concretely: `store_types.go` declares row types the rest of the package references 133 times, while the store's own method signatures need ten domain types back. That specific pair is a hard import cycle today.

- Watch-ordering stress probes. The stress profile previously claimed to run watch-ordering stress probes in `internal/sync` behind a dedicated run pattern; no such tests, and no `stress`-tagged test of any kind, has ever existed in the repo, so the step verified nothing and the claim has been removed rather than left standing. Deterministic watch-ordering probes are still worth writing — the `sync race x50` step surfaced a roughly 1-in-50 shutdown flake that isolated runs do not reproduce. One concrete instance has now been observed and recorded: `TestRunWatch_FallbackSleepHonorsCancellation` failing with "fallback jitter sleep did not start" under `-race -count=50 -run 'TestRunWatch|TestWatchRuntime|TestOneShot|Shutdown|Drain'`. It did not reproduce in two further 50-iteration runs of the same pattern, nor in 20 isolated iterations, which is the signature the entry describes. The failing wait is for the degraded local-observer fallback to begin its jittered sleep after the manual clock advances, so the probe to write is one that pins the ordering between the fallback ticker firing and the sleep starting, rather than waiting on a signal. [planned]
- Resource consumption guarantees documented per component. [planned]
- Representative benchmark scenarios, reporting, and publication policy are defined in [performance-benchmarking.md](performance-benchmarking.md). The repo-owned `devtool bench` runner, the `startup-empty-config` harness-validation scenario, and the manual `sync-bidirectional-catchup-100m` live representative scenario are implemented; fixture-slot preparation, release artifact promotion, and published benchmark reports remain planned. [planned]
- Production ignored-return audit complete: invariant-bearing results such as `depGraph.Complete` and dispatch admission booleans are handled explicitly, baseline/account metadata lookups now use explicit helpers or `found` checks, and the remaining ignored returns are limited to explicit value-only helper drops plus documented impossible-error builder writes. [verified]
- Bare `assert.NoError` audit complete: remaining sites are intentional cleanup, test-plumbing, or pure error-contract assertions, while weak CLI happy-path tests now assert rendered text, JSON shape, and preserved config/state. [verified]
- Write-path `Close` audit complete: sync-store close failures now propagate on internal baseline verification helpers and explicit sync-state reset flows, while read-only inspector close paths remain best-effort debug logged by design. [verified]
- Direct command/workflow coverage across the flattened `internal/cli/` command flows remains above the current target. [verified]
