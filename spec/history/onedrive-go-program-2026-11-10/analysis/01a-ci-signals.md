# 01a — Make the test signals trustworthy: CI and devtool

Repository `~/src/onedrive-go`, branch `main`, commit `70301c0e`. Read-only analysis, 2026-11-10.
All paths are relative to the repository root unless absolute. No `go` command was run; every timing is taken from recorded CI logs.

Marks: **VERIFIED** = I read the code or the `gh` output that proves it. **SUSPECTED** = not proven; the sentence says what settles it.
GitHub evidence comes from read-only `gh` calls on 2026-11-10 and is cited by run or job ID.

Contents: 0 structure of `ci.yml` · 1 nightly visibility · 2 e2e buckets · 3 race on push · 4 seeds · 5 coverage · 6 stress target · 7 other untrustworthy signals · 8 pull requests · 9 open SUSPECTED points · 10 Not read

---

## 0. How `ci.yml` is structured (VERIFIED)

`.github/workflows/ci.yml` (511 lines) is the only workflow.

**Triggers** `ci.yml:3-12`: `push` to `main`, every `pull_request`, `workflow_dispatch`, and `schedule` `cron: '0 10 * * *'` (comment `:11`: "10:00 UTC: 03:00 Pacific daylight time, 02:00 standard time").

**Workflow concurrency** `ci.yml:14-17`:

```yaml
# Cancel stale PR runs; never cancel push, scheduled or manual runs.
concurrency:
  group: ci-${{ github.event_name == 'pull_request' && github.head_ref || github.sha }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

Push, schedule and dispatch runs on one commit share the group `ci-<sha>`.

**Permissions** `ci.yml:22-23`: workflow level `contents: read`. Only `integration` (`:252-254`) and `e2e` (`:359-361`) add `id-token: write`. No job has `issues:` or `statuses:`.

**Jobs**

| Job | Lines | Runs when (`if`) | Timeout | Notes |
|---|---|---|---|---|
| `verify-lint` | `:50-79` | PR or push `:53` | 15 `:52` | golangci-lint v2.11.3 `:65`, govulncheck `:72-76`, `devtool verify lint` `:79` |
| `verify-unit` | `:86-125` | PR or push `:98` | 20 `:97` | `fail-fast: false` `:88`; 2 OS x 3 shards `:90-93`; Linux `verify quick --shard` `:119`, macOS `verify platform --shard` `:125` |
| `verify-full` | `:129-174` | schedule or dispatch `:138` | 45 `:137` | `fail-fast: false` `:131`; `verify public --coverage-threshold` `:174` |
| `verify` | `:176-209` | `always()` `:188` | 5 | `needs: [verify-lint, verify-unit, verify-full]` `:187`; the required check |
| `stress` | `:211-235` | schedule or dispatch `:214` | 90 `:213` | `verify stress --summary-json` `:226`; `cache: false` `:223` |
| `integration` | `:246-351` | `vars.AZURE_CLIENT_ID != '' && github.event_name != 'push'` `:255` | 45 `:248` | concurrency `live-account-integration`, `cancel-in-progress: false` `:249-251` |
| `e2e` | `:353-510` | same condition `:362` | 90 `:355` | concurrency `live-account-e2e` `:356-358`; profile `e2e` on PR, `e2e-full` with `--classify-live-quirks` on schedule/dispatch `:443-454` |

**What runs on which event**

| Event | Jobs |
|---|---|
| push to `main` | `verify-lint`, six `verify-unit` legs, `verify` |
| pull request | the above plus `integration` and `e2e` (fast profile) |
| schedule, dispatch | two `verify-full` legs, `verify`, `stress`, `integration`, `e2e` (`e2e-full`) |

**`needs`**: only `verify` has one (`:187`). `stress`, `integration` and `e2e` are in nobody's `needs`.

**`continue-on-error`** appears twice, both on the first Azure login attempt: `:276` (step id `azure_login_integration` `:274`) and `:381` (step id `azure_login_e2e` `:379`). Each is followed by a retry step that runs only `if: steps.<id>.outcome == 'failure'` (`:283`, `:388`). They retry a login; they do not swallow a test failure. A second failed login fails the job through the retry step.

**`fail-fast: false`** at `:88` and `:131`: one failing matrix leg does not cancel the others.

**The aggregate** `ci.yml:189-209` treats `success` as ran, `skipped` as neutral, anything else as failure, and fails when nothing ran:

```yaml
          for result in "${LINT_RESULT}" "${UNIT_RESULT}" "${FULL_RESULT}"; do
            case "${result}" in
              success) ran=1 ;;
              skipped) ;;
              *) echo "::error::a verify job did not succeed (${result})"; exit 1 ;;
            esac
          done
```

No step sets any `ONEDRIVE_GO_*SEED*` variable, and no step opens an issue, posts a status or notifies anyone.

---

## 1. Nightly visibility

### 1.1 Current behaviour

**F1.1 The aggregate `verify` does not cover the nightly's live and stress jobs. VERIFIED.**
Its comment says "It waits on every tier" (`ci.yml:182`), but `needs` names three jobs (`:187`). On the scheduled runs of 2026-11-05, -06, -07, -09 and -10 (`42078523533`, `42259111116`, `42389550829`, `42692556240`, `42865435904`) the `e2e` job failed at "Run E2E tests" while `verify`, `stress`, both `verify-full` legs and `integration` succeeded.

**F1.2 Nothing reports a red nightly. VERIFIED.**
`ci.yml` has no issue, status or notification step. History: 30 scheduled runs from 2026-05-14 to 2026-06-13 all concluded `failure` on one head commit (`75f872de`), after which commit `2601169b` removed the schedule; commit `15a376a6` (#832, 2026-10-31) restored it. From 2026-11-01 to 2026-11-10: ten scheduled runs, eight `failure`, two `success` (`41880065754`, `42500173633`). The repository has no issue with "nightly" in its title and no `nightly-failure` label.

**F1.3 The nightly is invisible on the commit it ran on. VERIFIED for one commit.**
For `70301c0e`: the REST check-runs list has 20 entries, 8 from the scheduled suite (including `e2e failure`) and 12 from the push suite. The GraphQL `statusCheckRollup` is `SUCCESS` with 12 contexts, all from the push suite. The combined commit status has `total_count 0`. So the green tick on `main` says nothing about the nightly. The general rule (the rollup uses only one suite per workflow) is SUSPECTED; one commit was observed.

**F1.4 The README badge is unfiltered. VERIFIED.** `README.md:3` uses `…/actions/workflows/ci.yml/badge.svg` with no `event` or `branch` query, so it shows the latest run of any kind.

**F1.5 `devtool nightly check` is the only consumer, and it has holes. VERIFIED.**
- It runs only when an increment starts (`CLAUDE.md:248`), so a quiet week has no reader.
- `loadLatestNightlyRun` lists completed scheduled runs and accepts only `success` or `failure` (`internal/devtool/nightly.go:590-599`, `:609`). A newest run that concluded anything else is skipped and an older verdict is used, with no age check.
- `loadNightlyFailureKeys` counts only jobs with `Conclusion == "failure"` (`nightly.go:635`). `nightlyJob` has no `Status` field (`nightly.go:85-89`).
- `checkNightly` returns nil on "nightly: no finished scheduled run found; nothing to claim" (`nightly.go:151-153`), and a red run from which no key can be derived leaves nothing unclaimed, so the check passes (`nightly.go:155-161`, `:189-195`).
- `pruneNightlyClaims` deletes a `fixed` claim whose key is absent from the failing set once the fix commit is in the run (`nightly.go:377-381`). "Absent from the failing set" includes "the test never ran" (see F2.3).

**F1.6 The post-merge wait cannot tell a push run from a nightly. VERIFIED on code; the scenario is derived, not observed.**
`loadMainRunsForCommit` passes no `--event` (`internal/devtool/verify_dod.go:1358-1383`). `classifyPostMergeRuns` decides on the first run whose `HeadSHA` matches (`verify_dod.go:1394-1412`); `githubRun.Event` is decoded (`:116`) and read nowhere. `gh run list --commit 70301c0e…` lists the scheduled run `42865435904` (failure) before the push run `42781307017` (success). If a schedule or dispatch run exists on the merge commit while `verify --dod --stage post-merge` waits, the stage waits on (or fails on) the wrong run. The window today is the 1.5–3 minute push run; item 3 widens it to about 12 minutes.

**F1.7 Live jobs skip silently on the nightly when `vars.AZURE_CLIENT_ID` is empty. VERIFIED.** `ci.yml:255`, `:362`. A skipped job is not a failure anywhere, so a nightly without live coverage is green.

**F1.8 The nightly does not start at the documented time. VERIFIED.** The ten November scheduled runs were created between 16:05 and 20:23 UTC, six to ten hours after `0 10 * * *` (`ci.yml:11-12`, `spec/design/system.md:423`).

### 1.2 Why it is a problem

A test that fails only in the full tier (a data race, an exhaustive test, a live e2e test) reaches `main` by design (`CLAUDE.md:224`, `ci.yml:39-44`). The nightly is the only net. Today a red nightly changes nothing a person sees: the commit keeps its green tick (F1.3), no issue appears (F1.2), and the one automated reader runs only when someone starts new work (F1.5). The May–June record shows the result: thirty consecutive red nights, ended by deleting the schedule.

### 1.3 Proposed end state

**(a) One issue, opened or updated on red, closed on green. (b) A commit status `nightly` on the run's head commit.** Both written by one new subcommand, `devtool nightly report`, so the logic is unit-tested Go and shares its job and key derivation with `nightly check`.

**Workflow job** (new, at the end of `ci.yml`):

```yaml
  nightly-report:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    needs: [verify-full, verify, stress, integration, e2e]   # plus model-seeds after item 4
    if: always() && (github.event_name == 'schedule' || github.event_name == 'workflow_dispatch')
    permissions:
      contents: read     # checkout
      actions: read      # gh run view, job logs
      issues: write      # the nightly issue and its label
      statuses: write    # the commit status
    env:
      GH_TOKEN: ${{ github.token }}
      GH_REPO: ${{ github.repository }}
    steps:
      - uses: actions/checkout@<the pin the other jobs use>
      - uses: actions/setup-go@<the pin the other jobs use>
        with:
          go-version-file: go.mod
          cache: true
      - name: Report the nightly result
        env:
          DRY_RUN: ${{ github.event_name != 'schedule' && 'true' || 'false' }}
        run: go run ./cmd/devtool nightly report --run-id "${{ github.run_id }}" --dry-run="${DRY_RUN}"
```

A job-level `permissions` block replaces the workflow-level one for that job, so `contents: read` must be repeated. Only `schedule` events write; a manual dispatch prints what it would write. That is also how the design is tested before the first scheduled run.

**Go, `internal/devtool/nightly.go`** (existing file):

```go
const NightlyActionReport NightlyAction = "report"   // beside :44-45

type NightlyOptions struct {                         // :48-57 gains
	RunID  int64 // report: the workflow run to report on
	DryRun bool  // report: print the writes, perform none
}
// nightlyJob (:85-89) gains Status, the "status" field of `gh run view --json jobs`.
// RunNightly's switch (:119-134) gains the report case.
```

`loadNightlyFailureKeys` (`nightly.go:619-666`) is split so `check` and `report` share one derivation:

```go
func loadNightlyJobs(ctx context.Context, runner commandRunner, repoRoot string, runID int64) ([]nightlyJob, error)
func nightlyJobVerdict(job nightlyJob) nightlyVerdict
func nightlyKeysForJobs(ctx context.Context, runner commandRunner, repoRoot string, jobs []nightlyJob) ([]nightlyJobReport, error)
```

Verdict rule, one function used by both subcommands:

| Verdict | Condition |
|---|---|
| `passed` | completed, conclusion `success` |
| `not-run` | completed, conclusion `skipped` |
| `failed` | completed, any other conclusion except `cancelled` (so `timed_out` counts) |
| `no-verdict` | not completed, or `cancelled` |

**Go, new file `internal/devtool/nightly_report.go`**:

```go
const (
	nightlyIssueLabel    = "nightly-failure"
	nightlyIssueTitle    = "Nightly CI is failing"
	nightlyStatusContext = "nightly"
	nightlyKeysMarker    = "<!-- nightly-keys: %s -->" // sorted, comma-separated
	nightlyIssueMaxTests = 50                          // per job, then "and N more"
)

// nightlyRequiredJobs are the ci.yml job keys a scheduled run must pass.
// A matrix leg matches by the prefix name + " (".
var nightlyRequiredJobs = []string{"verify-full", "verify", "stress", "integration", "e2e"}

type nightlyJobReport struct {
	Name    string
	Verdict nightlyVerdict
	Tests   []string // failed top-level tests; empty when the log names none or is unreadable
	Keys    []string // the claim keys nightly check derives for this job
}

type nightlyReport struct {
	RunID   int64
	RunURL  string
	HeadSHA string
	Jobs    []nightlyJobReport
}

func buildNightlyReport(ctx context.Context, runner commandRunner, repoRoot string, runID int64) (nightlyReport, error)
func (r nightlyReport) state() string          // "success", "failure" or "error"
func (r nightlyReport) keys() []string
func renderNightlyIssueBody(r nightlyReport) string
func reportNightly(ctx context.Context, runner commandRunner, opts NightlyOptions) error
```

`state()`: `failure` when any required job is `failed`; otherwise `error` when any required job is `not-run` or `no-verdict` (this makes F1.7 red); otherwise `success`. `buildNightlyReport` ignores the `nightly-report` job itself by evaluating only `nightlyRequiredJobs`.

Commands `reportNightly` issues (all through the existing `commandRunner`, which has no stdin, so bodies go by `--body`):

| Situation | Commands |
|---|---|
| always | `gh run view <id> --json databaseId,conclusion,headSha,createdAt,url,jobs`; per failed job `gh api repos/{owner}/{repo}/actions/jobs/<id>/logs` (as `nightly.go:644-647`) |
| always | `gh api -X POST repos/{owner}/{repo}/statuses/<headSha> -f state=<state> -f context=nightly -f target_url=<run url> -f description=<n failed jobs>` |
| not green, no open issue | `gh label create nightly-failure --force …`, then `gh issue create --title … --label nightly-failure --body …` |
| not green, open issue | `gh issue edit <n> --body …`; and `gh issue comment <n> --body …` only when the key set in the marker differs from the new one |
| green, open issue | `gh issue close <n> --comment "Nightly <url> passed on <sha>."` |
| find the issue | `gh issue list --label nightly-failure --state open --json number,body --limit 1` |

The issue body carries: run URL, head commit, one row per required job with its verdict and failed tests, the claim keys (the same strings `nightly check` prints), replay lines (item 4), and the keys marker. When a job log cannot be read, the row lists the job with "tests unknown: log unreadable"; the job-level verdict never depends on logs.

**CLI** `cmd/devtool/main.go:55-111`: a third subcommand `report` with `--run-id` (required) and `--dry-run`; `cmd.AddCommand(check, release, report)` at `:108`.

**Changes to `nightly check`** (`nightly.go:139-197`, `:589-614`):
1. The newest completed scheduled run decides. When its conclusion is neither `success` nor `failure`, or it is older than a new constant `nightlyStaleAfter = 48 * time.Hour` (measured with `opts.Now`), the failing set is the single key `job-nightly-no-verdict`.
2. A run that concluded `failure` but yields zero keys yields `job-nightly-unattributed` instead of passing.
3. Keys come from `failed` jobs under the shared verdict rule (so a `timed_out` job counts).
4. Prune soundness: `nightlyClaim` (`nightly.go:59-68`) gains `Job string` with tag `json:"job,omitempty"`, set at claim time to the producing job's name. A `fixed` claim is deleted as confirmed only when that job's verdict is `passed` in a run containing the fix commit. A claim with no `Job` is confirmed only by a run whose every required job passed.

**Post-merge and concurrency:**
- `loadMainRunsForCommit` adds `"--event", "push"` (`verify_dod.go:1358-1383`), and `classifyPostMergeRuns` skips runs whose `Event` is not `push` (`:1394-1412`).
- `ci.yml:16` becomes `group: ci-${{ github.event_name }}-${{ github.event_name == 'pull_request' && github.head_ref || github.sha }}` so a dispatch and a schedule run on one commit do not queue behind, or cancel, each other. (GitHub keeps one pending run per group, as the file's own comment at `:244-245` says for the live groups.)

**Badge:** `README.md:3` gains `?branch=main&event=schedule`, and a second badge may show the push lane. SUSPECTED that the badge endpoint honours `event`; GitHub's badge documentation settles it.

**What GitHub offers a scheduled workflow on a user-owned repository** (the repository is public and user-owned; `gh api` shows the default workflow token permission is `read` and branch protection requires contexts `integration`, `e2e`, `verify` with `strict: false`):
- A scheduled run creates check runs on the tip of the default branch, but they do not reach the commit rollup (F1.3). A commit status written with `statuses: write` is the available way to mark the commit. SUSPECTED that such a status joins `statusCheckRollup`; one rollup query after the first scheduled run settles it.
- The status marks one commit. If `main` moves during the night, the tip has no `nightly` status. The issue and the filtered badge are the commit-independent signals, so both parts are needed.
- `nightly` must not be added to the required contexts: a PR head never receives it.

### 1.4 Tests to write first

`internal/devtool/nightly_test.go`. The fake remote `claimRemote` (`:37-50`, `newClaimRemote` `:52`) dispatches commands in `Output` (`:108`) and `CombinedOutput` (`:144`) and rejects anything else as "unexpected command". Extend it with an in-memory issue list, label set and status list for `gh issue list|create|edit|comment|close`, `gh label create` and `gh api -X POST …/statuses/…`. Reuse `failingNightlyRemote` (`:174`), `runNightlyCheck` (`:188`), `nightlyTestNow` (`:170`).

New tests, each with `// Validates: R-6.10.24` on the line above (`CLAUDE.md:209`):
- `TestNightlyReportOpensIssueOnFirstRedRun`
- `TestNightlyReportUpdatesOpenIssueWithoutDuplicating` (body replaced; comment only when keys change)
- `TestNightlyReportClosesIssueOnGreenRun`
- `TestNightlyReportPostsCommitStatus` (table: success, failure, error)
- `TestNightlyReportTreatsSkippedRequiredJobAsError`
- `TestNightlyReportIgnoresItsOwnJob`
- `TestNightlyReportDryRunWritesNothing`
- `TestNightlyRequiredJobsExistInWorkflow` — reads `.github/workflows/ci.yml` and asserts each name is a job key and is in the `needs` of `nightly-report`. Precedent: `pinnedLintVersion` reads the workflow by regular expression (`internal/devtool/verify_lint_version.go:13-17`, `:67-85`); `go.mod:34` has YAML only as an indirect dependency, so a regular expression is the consistent choice.

Under `// Validates: R-6.10.23`:
- `TestNightlyCheckCountsTimedOutJobAsFailure`
- `TestNightlyCheckClaimsNoVerdictWhenNewestRunWasCancelled`
- `TestNightlyCheckClaimsNoVerdictWhenNewestRunIsStale`
- `TestNightlyCheckFailsOnRedRunWithoutKeys`
- extend `TestNightlyCheckPrunesConfirmedFixesAndExpiredStaleClaims` (`:362`) with a `fixed` claim whose job did not pass: it must survive.

No existing test covers a `cancelled` or `timed_out` run or job (the thirteen tests are at `nightly_test.go:203`–`:433`).

CLI: extend `TestNewNightlyCmdPassesFlagsThrough` (`cmd/devtool/main_test.go:637`, `// Validates: R-6.10.23` at `:636`) for `report --run-id --dry-run`.

Post-merge: a test beside the existing `classifyPostMergeRuns` tests that a scheduled run listed first is ignored. (I did not open that test file; see "Not read".)

### 1.5 Spec and docs

- New **R-6.10.24** in `spec/requirements/non-functional.md` after `:202`: a scheduled run that does not pass every required job shall open or update one issue naming the failing jobs and tests, the issue shall close on the next passing scheduled run, and every scheduled run shall post a commit status `nightly` on its head commit; a required job that did not run is not a pass.
- Edit **R-6.10.23** (`non-functional.md:202`): a fix is confirmed only by a run in which the failing job passed; a missing or stale verdict is itself a claimable failure.
- `spec/design/system.md`: `:255` (claims paragraph, "once a run containing it passes, the ref is deleted"), `:423`–`:424` (tiers and the aggregate), `:252` (file ownership: add `nightly_report.go`), `:254` (post-merge waits for the *push* run). `system.md:3` already governs `internal/devtool/*.go` by glob.
- `CLAUDE.md:224`, `:248`; `ci.yml:25-48` comment; `README.md:3`.
- R-6.10.19 (`non-functional.md:199`) requires evidence for `[verified]`: the `Validates:` tests above supply it.

### 1.6 Risks

- SUSPECTED: a completed job's log is readable through the API while its run is still in progress. The dispatch dry run settles it. If not, the fallback is a second workflow triggered by `workflow_run` (`types: [completed]`, filtered on `github.event.workflow_run.event == 'schedule'`) that runs the same command; it also reports a run that was cancelled as a whole. Job verdicts do not depend on this either way.
- SUSPECTED: `issues: write` on a job block is granted when the repository default is `read`, and it covers `gh label create`. The first dispatch dry run cannot show it (it writes nothing); the first scheduled run does. `id-token: write` at `ci.yml:252-254` shows job-level elevation works in this repository. Fallback for the label: the owner creates it once.
- SUSPECTED: the conclusion GitHub gives a job that hits `timeout-minutes`. The design is safe either way: `timed_out` is `failed`, `cancelled` is `no-verdict`, and both make the night red.
- SUSPECTED: whether the owner's notification settings deliver an issue created by the Actions bot. Not answerable from the repository.
- A failing `nightly-report` job appears to `nightly check` as `job-nightly-report`. That is intended.
- More claim keys per night once item 2 lands; `devtool nightly release` exists for keys an increment gives up (`cmd/devtool/main.go:103-106`).

---

## 2. The e2e job stops at the first failing bucket

### 2.1 Current behaviour (VERIFIED)

`internal/devtool/verify_e2e.go:697-720`:

```go
	buckets := fullE2EBuckets()
	for i := range buckets {
		bucket := buckets[i]
		if err := collector.runBucket(bucket, func() error {
			…
			return runFullE2EBucket(ctx, runner, repoRoot, bucketEnv, bucket, stdout, stderr)
		}); err != nil {
			return err
		}
	}
```

The return at `:718-720` ends the profile at the first failing bucket. Two more stop points precede it: the full fixture preflight (`:693-695`), and the fast suite, whose error returns before `runE2EFull` is called (`internal/devtool/verify.go:394-398`, full suite at `:400-414`).

The three buckets are `full-parallel-misc`, `full-serial-sync`, `full-serial-watch-shared` (`verify_e2e.go:20-56`), run in that order.

The summary JSON records a status and an error string per bucket and no test names (`E2EBucketSummary`, `verify.go:116-125`). A bucket that never ran has no entry.

Tests pin the behaviour: `TestRunVerifyE2EFullDoesNotMaskUnknownFastSuiteFailure` (`internal/devtool/verify_runner_test.go:391-419`) asserts exactly two `runCommands` after a fast-suite failure (`:418`); `TestRunVerifyWritesSummaryJSONOnFailure` (`:576-608`) fails the serial-sync bucket and asserts only that bucket's status.

### 2.2 Why it is a problem

Recorded nights:
- 2026-11-09 (job `129415823673`): `full-parallel-misc` failed on `TestE2E_RapidFileChurn` and `TestE2E_InvalidFilenameRejection`; the job ended after 7m1.763s; the two serial buckets never ran.
- 2026-11-05 (job `127425083910`): `full-serial-sync` failed on `TestE2E_Sync_FolderMovesBothWays`; ended after 21m47.63s; the watch bucket never ran.
- 2026-11-10 (job `130002404209`): all three ran; the watch bucket failed on `TestE2E_SyncWatch_PauseResume`; 37m21.307s.

**F2.1** A failure in bucket 1 hides the state of buckets 2 and 3 for that night. Fixing it reveals the next one a day later.
**F2.2** It contradicts the repository's own rule, `CLAUDE.md:138`: "Never stop-on-first-error for bulk operations — collect, report, continue".
**F2.3** It makes the claim pruning unsound: a test in bucket 3 with a `fixed` claim is "absent from the failing set" on a night when bucket 1 failed, so `pruneNightlyClaims` deletes its claim as confirmed although it never ran (`nightly.go:377-381`). VERIFIED on code; no such deletion was observed.

### 2.3 Proposed end state

`runE2EFull` (`verify_e2e.go:676-724`):

```go
	var bucketErrs []error
	for i := range buckets {
		bucket := buckets[i]
		if ctx.Err() != nil {
			collector.recordBucketNotRun(bucket, "run cancelled")
			continue
		}
		if err := collector.runBucket(bucket, func() ([]string, error) { … }); err != nil {
			bucketErrs = append(bucketErrs, fmt.Errorf("bucket %s: %w", bucket.Name, err))
		}
	}
	return errors.Join(bucketErrs...)
```

- A preflight failure still stops everything (the account or fixture is unusable); `runE2EFull` records all three buckets as `not-run` first, so the summary always lists three.
- `runOptionalVerification` (`verify.go:375-433`) under `e2e-full`: a fast-suite *test* failure is kept, the full buckets still run, and the joined error is returned. A fast *preflight* failure still stops. To tell them apart, the preflight functions wrap with a sentinel `errE2EPreflight` (`fmt.Errorf("fast e2e auth preflight: %w: %w", errE2EPreflight, err)`) checked by `errors.Is`. This is a stated choice; the narrower alternative is to continue only across buckets.

Summary (`verify.go:98-125`, `:52-53`):

```go
const verifySummaryStatusNotRun = "not-run"

type E2EBucketSummary struct {
	…
	FailedTests []string `json:"failed_tests,omitempty"`
}
type VerifyStepSummary struct {
	…
	FailedTests []string `json:"failed_tests,omitempty"`   // used by "fast e2e"
}
```

`verify_summary.go`: `runBucket(bucket fullE2EBucketSpec, fn func() ([]string, error)) error` (today `:51-71` takes `func() error`), and new `recordBucketNotRun(bucket fullE2EBucketSpec, reason string)`. `renderText` (`:133-174`) prints the failed tests under each bucket line.

Collecting the names: the bucket and fast-suite commands tee their stdout into a buffer the way `runGoTestMatching` already does (`verify_test_match.go:34-37`). Raw output goes through `failedTopLevelTests` (`nightly.go:668-690`); the `-json` output of the classification paths goes through `failedGoTestsFromJSON` (`verify_e2e.go:791-812`), reduced to top-level names.

Streaming: both classification paths call `runner.CombinedOutput` and write the output only after the command returns (`verify_e2e.go:521-524`, `:853-856`). On the nightly the watch bucket runs this way (`bucketSupportsClassifiedRerun`, `:743-745`), so a job-level timeout during it leaves no test output in the log. Replace with `runner.Run` writing to a tee.

Timeouts: the Go deadlines are fast 10m plus 3 x 60m (`verify.go:26-27`; every bucket uses `fullE2EPackageTimeout`, `verify_e2e.go:26`, `:36`, `:46`), 190 minutes against a 90-minute job (`ci.yml:355`). With all buckets always running, the sum must fit. Per-bucket constants, for example `fullE2EParallelMiscTimeout = "15m"`, `fullE2ESerialSyncTimeout = "40m"`, `fullE2EWatchSharedTimeout = "40m"` (recorded durations 134 s, 908 s, about 948 s), and `timeout-minutes: 120` for the job. A new test reads `timeout-minutes` of the `e2e` and `stress` jobs from `ci.yml` and asserts the sum of the Go deadlines is below it (same workflow-reading precedent as 1.4).

**`devtool nightly check`:** it derives keys from job logs, not from the summary (`nightly.go:644-657`), so it needs no code change and now sees every failing test of the night. SUSPECTED: that `failedTopLevelTests` matches `--- FAIL:` when it sits inside a `-json` output line (the watch bucket's log on the nightly). Reading `nightly.go:668-690` settles it; if it does not match, the watch bucket's failures surface only as `job-e2e` today, which is a second reason to stream raw output.

### 2.4 Tests to write first

`internal/devtool/verify_runner_test.go`, with `fakeRunner` (`verify_test_helpers_test.go:21-35`; keys are `name + " " + args`, failures injected by `runErrByKey` and `combinedErrByKey`) and the helpers `readVerifySummaryFile` (`:119`), `assertVerifySummaryHasStep` (`:127`), `assertVerifyBucketSummary` (`:155`):
- New `TestRunVerifyE2EFullRunsEveryBucketAfterOneFails`: fail bucket 1; assert three bucket commands ran, the error names bucket 1, the summary has `fail`, `pass`, `pass`.
- New `TestRunVerifyE2EFullReportsEveryFailingBucket`: fail buckets 1 and 3; assert both names in the error and `failed_tests` in the summary.
- New `TestRunVerifyE2EFullRecordsBucketsNotRunAfterPreflightFailure`.
- Change `TestRunVerifyE2EFullDoesNotMaskUnknownFastSuiteFailure` (`:391-419`): still an error containing "fast e2e", but the bucket commands now run (the count at `:418` changes).
- Change `TestRunVerifyWritesSummaryJSONOnFailure` (`:576-608`): the later bucket is `pass`, not absent.
- `TestRunVerifyE2EFullDoesNotMaskUnknownWatchSharedFailure` (`:468-497`) pins `runCommands` length 5 (`:496`); it changes if the classification path moves from `CombinedOutput` to `Run`.
- The e2e tests at `:204`–`:576` carry no `// Validates:` comment; new ones cite the requirement chosen in 2.5.

### 2.5 Spec and docs

No requirement mentions buckets or `--summary-json`. Either a new **R-6.10.26** ("the full live suite shall run every bucket after a bucket fails and report every failure") or one sentence added to R-6.10.22 (`non-functional.md:200`); a new ID is cleaner because tests can cite it. Design doc: `system.md:333` ("executes those buckets sequentially after the single full-suite preflight"), `:336` ("gives every full bucket the same `60m` timeout budget"), `:342` (summary artifacts), `:248`; `CLAUDE.md:226` ("60-min bucket timeout").

### 2.6 Risks

- A red night costs the full run (about 37 minutes recorded) on the shared `live-account-e2e` group, which PR e2e jobs also use (`ci.yml:356-358`).
- A broken account state after bucket 1 can cascade into buckets 2 and 3 and inflate the key count. The preflight stop limits this.

---

## 3. `-race` on the sync tree on every push to `main`

### 3.1 Current behaviour (VERIFIED)

A push to `main` runs `verify-lint`, six `verify-unit` legs and `verify` (`ci.yml:53`, `:98`). The unit legs run `runShortUnitTests`, whose arguments are (`internal/devtool/verify_quick.go:151-157`):

```go
	args := []string{"test", "-short", "-timeout=" + shortUnitTestTimeout}
```

plus an optional `-run=<shard>` and `./...`. There is no `-race`. The file says so itself (`verify_quick.go:90-91`): "The race detector, the coverage gate, the compile guard and the exhaustive tests stay in default and public, which the local gate and nightly CI run." R-6.10.22 states the same (`non-functional.md:200`). `-short` also skips every seeded model test (`skipInShort`, `internal/sync/test_helpers_test.go:356`; called at `engine_model_based_test.go:94`, `planner_model_test.go:670`, `chaos_remote_test.go:477`).

So **no PR or push lane uses the race detector**, and no PR or push lane runs the model tests. The only race runs in CI are nightly: `verify public` (`verify.go:550-559`), stress, `integration` (`verify.go:687-700`) and the e2e commands.

Recorded race timings for `internal/sync` (run `42865435904`):

| Where | Time |
|---|---|
| Linux `verify-full`, `go test -race -coverprofile ./...` | `internal/sync 639.743s`; job total 13m42.224s |
| macOS `verify-full` | `internal/sync 891.790s` of the 1200 s deadline; job total 21m45.386s |
| stress, `-race -count=3` whole package | about 25 minutes, about 8.4 minutes per iteration |
| push runs on `main` | 1.5–3 minutes in total |

A `-race` run of `./internal/sync/...` alone, without coverage, is not recorded. From the Linux figure it is plausibly at or below about 11 minutes. SUSPECTED; one run of the new job settles it.

### 3.2 Why it is a problem

A data race merged at 10:00 is found by the nightly, which in November started six to ten hours late (F1.8), and is attributed to nobody: the post-merge stage that could have blamed the merge passed in three minutes. With several increments merging per day, the nightly then fails for all of them.

### 3.3 Proposed end state

**Devtool.** New profile and file:

```go
// internal/devtool/verify.go, profile constants (:55-64)
VerifyRace VerifyProfile = "race"

// internal/devtool/verify_race.go (new)
const raceSyncTimeout = "20m"   // go test's default is 10m; the recorded run takes 640 s

// runRaceVerification runs the sync tree under the race detector, unshortened.
func runRaceVerification(ctx context.Context, runner commandRunner, repoRoot string,
	env []string, collector *verifySummaryCollector, stdout, stderr io.Writer) error
// command: go test -race -timeout=20m ./internal/sync/...
```

Unshortened, so the model tests run with their fixed race seeds (item 4). `verifyPlan` (`verify.go:158-168`) gains `runRace`; `resolveVerifyPlan` (`:247-294`) maps the profile; `verifyProfileUsage` (`verify_quick.go:12`) and the cobra `Use` string (`cmd/devtool/main.go:133`) list it.

**Workflow.**

```yaml
  # The race detector on the sync tree for every commit that reaches main, so a
  # race is caught on the commit that introduced it and the post-merge stage
  # of that increment waits for it.
  verify-race:
    runs-on: ubuntu-latest
    timeout-minutes: 25      # above the 20-minute go test deadline plus the build
    if: github.event_name == 'push'
    steps:
      - checkout, setup-go (cache: true) as in verify-unit
      - name: Verify race
        run: go run ./cmd/devtool verify race
```

`verify` gains it: `needs: [verify-lint, verify-unit, verify-full, verify-race]` (`ci.yml:187`), `RACE_RESULT` in `env` (`:192-194`) and in the loop (`:199`). On a PR it is `skipped`, which the loop treats as neutral.

Effect on the DoD: the push run grows from about 3 to about 12 minutes; `verify --dod --stage post-merge` waits for it (`system.md:254`) within `DefaultDODCITimeout = 30 * time.Minute` (`verify_dod.go:19`). Ship the push-event filter from 1.3 before or with this job, because the longer run widens the window in F1.6.

If 12 minutes is too long, the existing shard machinery can split it: `resolveVerifyShard` restricts `--shard` to quick and platform (`verify_quick.go:68-77`, message at `:73`) and would need to admit `race`. Each leg pays the race build again; the gain is SUSPECTED until measured.

### 3.4 Tests to write first

- `TestRunVerifyRaceRunsExpectedSteps` in `verify_runner_test.go`, modelled on `TestRunVerifyStressRunsExpectedSteps` (`:742-793`): assert the exact argument list, with `// Validates: R-6.10.22`.
- `cmd/devtool/main_test.go`: the profile is accepted (beside `TestDevtoolBinary_VerifyRejectsUnknownProfile`, `:471`).
- The workflow test from 1.4 can also assert that `verify-race` is in the aggregate's `needs`.

### 3.5 Spec and docs

R-6.10.22 (`non-functional.md:200`) says the per-PR lane runs "without the race detector"; add: every push to `main` additionally runs the sync tree under the race detector. `system.md:248` (profiles), `:252` (file ownership), `:423`–`:424`; `ci.yml:25-48` (the sentence "A data race … can therefore reach main and be caught by the next nightly run" at `:39-44` narrows to races outside the sync tree); `CLAUDE.md:224`; `CONTRIBUTING.md:58-62`; `verify_quick.go:90-91` comment.

### 3.6 Risks

- A flaky race-only test now fails post-merge stages. That is the existing 1-in-50 class the stress lane exists for (`verify_stress.go:50-55`); it becomes visible sooner.
- The macOS timing does not apply; the job is Linux only.

---

## 4. Model-test seeds

### 4.1 Current behaviour (VERIFIED)

Three seeded families, all in `internal/sync`:

| Family | Tests | Single | Count | Start | Default / under `-race` |
|---|---|---|---|---|---|
| engine | `TestRunOnce_RandomizedBidirectionalEditsConverge` (`engine_model_based_test.go:93`) | `ONEDRIVE_GO_MODEL_SEED` `:69` | `ONEDRIVE_GO_MODEL_SEEDS` `:70` | `ONEDRIVE_GO_MODEL_SEED_START` `:71` | 12 `:63` / 2 `:65` |
| engine crash | `TestModel_CrashAtSeededPointsRecovers` (`:142`) | same | same | same | 4 `:64` / 1 (`:162-166`) |
| planner | `TestPlannerModel_RandomTriplesConverge` (`planner_model_test.go:669`) | `ONEDRIVE_GO_PLANNER_SEED` `:36` | `ONEDRIVE_GO_PLANNER_SEEDS` `:37` | **none** | 8 `:33` / 1 (`:648-652`) |
| chaos | `TestModel_ChaosGraphConverges` (`chaos_remote_test.go:476`) | `ONEDRIVE_GO_MODEL_SEED` (`:499-501`) | `ONEDRIVE_GO_CHAOS_SEEDS` `:54` | **none** | 2 `:55` / 1 (`:503-505`) |

Derivation, engine (`engine_model_based_test.go:173-202`):

```go
	if raw := os.Getenv(modelSeedEnv); raw != "" {
		seed, err := strconv.ParseUint(raw, 10, 64)
		require.NoError(t, err, modelSeedEnv)
		return []uint64{seed}
	}
	count := modelDefaultSeeds
	if raceDetectorEnabled {
		count = modelRaceSeeds
	}
	…
	start := uint64(1)
	if raw := os.Getenv(modelSeedStartEnv); raw != "" { … start = parsed }
	seeds := make([]uint64, count)
	for i := range seeds {
		seeds[i] = start + uint64(i)
	}
```

Planner (`planner_model_test.go:641-666`) and chaos (`chaos_remote_test.go:496-517`) repeat the loop by hand with `seeds[i] = uint64(i + 1)`: always from 1.

`raceDetectorEnabled` is a build-tagged constant (`race_enabled_test.go:8`, `race_disabled_test.go:6`). Under race the batch count also drops from 6 to 2 (`engine_model_based_test.go:57`, `:60`; chaos `chaos_remote_test.go:484-486`), so **the same seed does different work with and without `-race`**.

How a failure prints its seed:
- Subtest name `seed=N` (`engine_model_based_test.go:98`, `planner_model_test.go:674`, `chaos_remote_test.go:481`).
- Engine: `failureContext()` (`engine_model_based_test.go:425-428`): `"seed %d (rerun with %s=%d); operations:\n  %s"`.
- Planner: a cleanup log `"planner model seed %d (rerun with %s=%d):\n  %s"` (`planner_model_test.go:689-693`) and a second formatter (`:706-708`).
- Chaos: `t.Logf("seed %d faults: %s", …)` (`chaos_remote_test.go:489`); no rerun hint of its own.

None prints the test name, the package or whether `-race` was on.

**F4.1 CI always runs the same seeds. VERIFIED.** Every CI command that is not `-short` is `-race` (item 3.1), and no workflow step sets a seed variable. So every night runs engine seeds 1–2, crash seed 1, planner seed 1, chaos seed 1. The 12- and 8-seed defaults run in no CI job and no devtool profile. The comment at `engine_model_based_test.go:38-40` ("The default run is modelDefaultSeeds, cheap enough for every CI run; a nightly or local search sets the count") describes something no job does.

**F4.2 The automated readers drop the seed. VERIFIED.** `failedTopLevelTests` cuts the name at the first `/` (`nightly.go:682`), so `seed=7` is lost and the claim key is the test name. `failed-test` reruns a non-e2e failure with `test [-run=<exact>] -count=1 -v <pkg>` (`internal/devtool/verify_failed.go:165-170`): no `-race`, no seed variable.

**F4.3 Bad counts pass silently. VERIFIED.** A count of `0` makes zero subtests and the test passes; a negative count panics in `make` (`engine_model_based_test.go:197`, `planner_model_test.go:660`).

### 4.2 Why it is a problem

The model tests exist to search; CI searches five fixed points forever. Rotating seeds fixes that but creates a new hazard that must be designed out first: with a different seed each night, a real failure on Monday is followed by a green Tuesday. The single nightly issue (item 1) would close, and `pruneNightlyClaims` would confirm a "fix" nobody made. And because the seed's meaning depends on `-race` and on the commit ("adding operations renumbers what a seed does", `engine_model_based_test.go:40-42`), a bare number is not a replay.

### 4.3 Proposed end state

**(1) One seed helper** in `internal/sync/test_helpers_test.go`, replacing the three hand-rolled loops:

```go
type seedEnv struct {
	single string // one seed; overrides count and start
	count  string // number of consecutive seeds
	start  string // first seed, default 1
}

// seedsFromEnv returns the seeds a model test runs. A count below 1 fails the test.
func seedsFromEnv(t *testing.T, env seedEnv, defaultCount int) []uint64
```

New variables `ONEDRIVE_GO_PLANNER_SEED_START` and `ONEDRIVE_GO_CHAOS_SEED_START`. `modelSeeds`, `plannerModelSeeds` and `chaosSeeds` become one-line callers that pick the default by `raceDetectorEnabled`. `modelCrashSeeds` keeps its limit rule (`engine_model_based_test.go:156-171`): when a count is set it runs all of them.

**(2) One replay line**, also in `test_helpers_test.go`:

```go
// logSeedReplayOnFailure registers a cleanup that, when the subtest failed, logs
//   seed-replay: <ENV>=<seed> go test [-race] -run '^<TopLevelTest>$' ./internal/sync
// The text after "seed-replay: " is a shell command.
func logSeedReplayOnFailure(t *testing.T, envName string, seed uint64)
```

Called first in each `seed=N` subtest of the four tests. `-race` is printed when `raceDetectorEnabled`. The top-level name is `t.Name()` cut at `/`. The test owns the mapping from test to variable, so devtool needs no table. The existing messages (`:425-428`, `:689-693`, `:706-708`) stay for the operation log.

**(3) Devtool parses it.** New file `internal/devtool/seed_replay.go`:

```go
type seedReplay struct {
	Env     string
	Seed    uint64
	Test    string
	Package string
	Race    bool
}

// seedReplaysFromLog finds seed-replay lines in raw or -json go test output.
func seedReplaysFromLog(log string) []seedReplay
func (r seedReplay) command() string
func (r seedReplay) key() string   // "seed-" + env + "-" + seed, through sanitizeNightlyKey
```

The package pattern must be a tight character class (`\./[\w./-]+`) so that inside a `-json` line it does not swallow the `\n"}` tail. Users: `nightly report` (issue body), `nightly check` (keys), and `failed-test`, where `failedGoTestRerunArgs` (`verify_failed.go:151-171`) sets the variable and `-race` when the failed log carries a replay line for the target test.

**(4) Rotation in a dedicated, non-race lane.** Breadth belongs to non-race runs (`engine_model_based_test.go:50-54`: "4 seeds measured 204 s" under race on 2 cores). `verify-full`, stress and the push race job keep their fixed seeds, so their red stays sticky and attributable.

```go
// internal/devtool/verify.go
VerifySeeds VerifyProfile = "seeds"
// VerifyOptions gains: Now func() time.Time; SeedStart uint64 (0 = derive from the date)

// internal/devtool/verify_seeds.go (new)
const (
	nightlySeedStride   = 1000   // start advances this much per UTC day; >= every count below
	seedsEngineCount    = 12     // the tests' own defaults; raise only after a recorded timing
	seedsPlannerCount   = 8
	seedsChaosCount     = 2
	seedsTimeout        = "30m"
	seedsRunPattern     = "^(TestRunOnce_RandomizedBidirectionalEditsConverge|TestModel_CrashAtSeededPointsRecovers|TestPlannerModel_RandomTriplesConverge|TestModel_ChaosGraphConverges)$"
)
var nightlySeedEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// nightlySeedStart returns 1 + whole UTC days since the epoch * nightlySeedStride.
func nightlySeedStart(now time.Time) uint64
func runSeedsVerification(ctx context.Context, runner commandRunner, repoRoot string,
	env []string, opts VerifyOptions, collector *verifySummaryCollector, stdout, stderr io.Writer) error
```

It prints `==> model seeds start=<n> engine=12 planner=8 chaos=2` and runs `go test -timeout=30m -run <pattern> ./internal/sync/...` with the three start and three count variables set, through the tree-aware empty-match guard of item 6. `--seed-start <n>` on the command line reproduces a night.

```yaml
  model-seeds:
    runs-on: ubuntu-latest
    timeout-minutes: 40
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
    steps:
      - checkout, setup-go
      - run: go run ./cmd/devtool verify seeds --summary-json "${RUNNER_TEMP}/seeds-verify-summary.json"
      - upload the summary (if: always()), as stress does at ci.yml:228-235
```

It joins `nightlyRequiredJobs` and the `needs` of `nightly-report`.

**(5) Sticky failures.** Do not merge rotation before this exists.
- `nightly report` opens **one issue per failing seed**, label `seed-failure`, titled with the key, body = the replay command, the head commit and the run URL. It never closes these. The fixing PR closes it and, per the file's own rule (`engine_model_based_test.go:40-42`), adds the seed as a named scenario test.
- `nightly check` adds to its failing set the key of every open `seed-failure` issue (`gh issue list --label seed-failure --state open --json number,title`), and for the `model-seeds` job derives `seed-…` keys from replay lines instead of test-name keys, falling back to `job-model-seeds`.
- A seed claim then lives exactly as long as its issue: the existing `fixed` and prune logic applies once the issue closes.

### 4.4 Tests to write first

- `internal/sync/test_helpers_test.go` neighbours: `TestSeedsFromEnv` (table with `t.Setenv`: default, single, count, start, start+count; count 0 and negative are asserted through a fake `testing.TB` or by checking the validation function directly). `// Validates: R-6.10.25`.
- `TestModel_SameSeedReplaysIdentically` (`engine_model_based_test.go:109`) already holds the replay promise; no change.
- `internal/devtool/seed_replay_test.go`: `TestSeedReplaysFromLog` (raw line, `-json` line, race and non-race, two families in one log), `TestSeedReplayCommandRoundTrips`.
- `verify_runner_test.go`: `TestRunVerifySeedsSetsRotatedStart` (injected clock; assert the env through `assertCommandHasEnvVar`, `verify_test_helpers_test.go:170`), `TestNightlySeedStrideCoversEveryCount`, `TestSeedsRunPatternNamesExistingTests` (reads `internal/sync/*_test.go`; precedent `TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce`, `verify_runner_test.go:610`).
- `verify_runner_test.go:872-900` (failed-test, non-e2e): add a case with a replay line asserting the variable and `-race`.
- `nightly_test.go`: `TestNightlyReportOpensOneIssuePerFailingSeed`, `TestNightlyReportNeverClosesSeedIssues`, `TestNightlyCheckClaimsOpenSeedIssues`.

### 4.5 Spec and docs

New **R-6.10.25**: the nightly shall run the seeded model tests on a seed range that advances every day; a failing seed shall be reported with a command that replays it and shall stay open until fixed. `system.md:248`, `:252`, `:423`. Correct the comment at `engine_model_based_test.go:36-42` and add the start variables to `planner_model_test.go:27-30` and `chaos_remote_test.go:470-475`.

### 4.6 Risks

- Non-race time per seed is not recorded (SUSPECTED; the first dispatch run of `model-seeds` settles it). That is why the counts start at the tests' own defaults.
- A replay holds only at the failing commit (`engine_model_based_test.go:40-42`); the issue must carry the commit, and it does.
- More open issues. Each is a real, reproducible failure.

---

## 5. Coverage gates disagree

### 5.1 Current behaviour (VERIFIED)

How it is computed: `runUnitTests` runs (`internal/devtool/verify.go:550-559`, status line `:541`)

```
go test -race -timeout=20m -coverprofile=<repo>/cover.out ./...
```

with no `-coverpkg` and no `-covermode` (neither string occurs in `internal/devtool`, `cmd/devtool` or `.github`). `runCoverageGate` (`verify.go:615-654`) runs `go tool cover -func=<file>` (`:627`), `parseCoverageTotal` (`:656-673`) reads the `total:` line, a negative threshold prints "coverage gate disabled (total %.1f%%)" (`:645-647`), otherwise it fails with `coverage gate failed: %.1f%% < %.1f%%` (`:649-651`).

Every place a number lives:

| Place | Value |
|---|---|
| `internal/devtool/verify.go:18` `defaultCoverageThreshold` | 75.5 |
| `cmd/devtool/main.go:15` second `defaultCoverageThreshold` (flag default, `:171-176`) | 75.5 |
| `.github/workflows/ci.yml:173` (Linux `'76.0'`, macOS `'-1'`), used at `:174` | 76.0 |
| `spec/requirements/non-functional.md:183` R-6.10.4 | 76.0 |
| tests: `verify_runner_test.go:61`, `:161`, `:533`, `:941` | 76.0 |
| tests: `cmd/devtool/main_test.go:45` (asserts the constant), `:69`/`:88` | constant, 80.5 |

**F5.1** Local `verify default` gates at 75.5; the nightly gates at 76.0. A change that lands at 75.8% passes the documented local gate (`CLAUDE.md:224`: "`verify default` remains the gate") and fails the nightly a day later.
**F5.2** `--coverage-threshold 0` silently becomes 75.5: `if coverageThreshold == 0 { coverageThreshold = defaultCoverageThreshold }` (`verify.go:208-211`).
**F5.3** Without `-coverpkg`, a statement counts only when a test in its own package executes it. The recorded total is 81.0% on Linux and 80.8% on macOS (run `42865435904`), five points above the gate, so the gate does not bind.

### 5.2 Proposed end state: the number exists once

Following the repository's own precedent ("read from the workflow so the two cannot drift apart through a second copy of the number", `verify_lint_version.go:28-30`), the number lives **only in R-6.10.4**.

- Delete `defaultCoverageThreshold` at `verify.go:18` and at `cmd/devtool/main.go:15`.
- New file `internal/devtool/verify_coverage.go` (the gate functions `runCoverageGate` and `parseCoverageTotal` move here from `verify.go:615-673`):

```go
const requirementsPath = "spec/requirements/non-functional.md"

// requiredCoverage reads the percentage R-6.10.4 states.
func requiredCoverage(repoRoot string) (float64, error)
```

- `runVerifyProfile` (`verify.go:208-211`): `0` means "the requirement's number" and calls `requiredCoverage`; a positive value overrides (tests); negative means "do not gate". The flag default at `cmd/devtool/main.go:171-176` becomes `0`, with the help text saying so. This also resolves F5.2 by making the meaning of 0 explicit.
- `runUnitTests` adds `"-coverpkg=./..."` (`verify.go:550-559`) and the status line (`:541`) says so.
- `ci.yml:167-174` carries no number:

```yaml
      - name: Verify public
        env:
          COVERAGE_ARGS: ${{ matrix.os != 'ubuntu-latest' && '--coverage-threshold -1' || '' }}
        run: go run ./cmd/devtool verify public ${COVERAGE_ARGS}
```

(The condition is written with `!=` on purpose: `cond && '' || 'x'` yields `'x'` when the condition is true, because the empty string is falsy.)

- R-6.10.4 is rewritten: "Total statement coverage, measured over all packages with `-coverpkg=./...`, shall not drop below N% in the verification gate; this line is the only place the number is stated."

**The number N must be re-decided in the same PR.** With `-coverpkg=./...` a statement counts when any package's tests execute it, so the total can only rise from 81.0%. By how much is SUSPECTED; one `workflow_dispatch` run of the branch (the macOS leg already prints the total without gating) settles it. Set N from that measurement, not from 76.0.

Fallback if parsing prose for a runtime value is unwanted: keep one exported constant in `internal/devtool`, used by `cmd/devtool`, and add a repo rule to `repoRuleSteps` (`verify.go:582-593`) asserting R-6.10.4 states the same number. Two copies, mechanically tied.

### 5.3 Every place that changes

`internal/devtool/verify.go:18`, `:208-211`, `:541`, `:550-559`, `:615-673` (move); `cmd/devtool/main.go:15`, `:171-176`; `.github/workflows/ci.yml:167-174`; `spec/requirements/non-functional.md:183`; `internal/devtool/verify_runner_test.go:61`, `:83` (the pinned unit-test argument list), `:161`, `:533`, `:941`, and the three coverage tests at `:917`, `:926`, `:954`; `cmd/devtool/main_test.go:45`; `spec/design/system.md:424` (Linux-only gate), `:248`, `:252`; `CONTRIBUTING.md:47`; `CLAUDE.md:342` ("PR CI does not gate coverage; the nightly `verify public` does") stays true.

### 5.4 Tests to write first

- `TestRequiredCoverageReadsRequirement` (reads the real requirements file from the repository root; fails when the line has no percentage). `// Validates: R-6.10.4`.
- `TestRunVerifyZeroCoverageThresholdUsesRequirement`.
- Update the argument assertion at `verify_runner_test.go:83` to include `-coverpkg=./...`.
- `TestRunVerifyCoverageThresholdFailure` (`:926`) and `TestRunVerifyNegativeCoverageThresholdReportsWithoutGating` (`:954`) keep their shape.
- `TestNewVerifyCmdDefaultsToDefaultProfile` (`cmd/devtool/main_test.go:25`, assertion `:45`) expects 0.

### 5.5 Risks

- `-coverpkg=./...` instruments every package in every test binary. The macOS leg already spends 891.8 s of its 1200 s deadline in `internal/sync`. If the dispatch run shows it over, make the non-gating leg not measure at all (negative threshold = no `-coverprofile`), which changes `TestRunVerifyNegativeCoverageThresholdReportsWithoutGating` and the flag help. SUSPECTED cost; the same dispatch run settles it.
- The local `verify default` on the 2-core host gets slower by the same mechanism.

---

## 6. Stress lane targets `./internal/sync` only

### 6.1 Current behaviour (VERIFIED)

The four production lines, all in `internal/devtool/verify_stress.go`:

| Line | Text |
|---|---|
| `:75` | `"==> go test -race -count=50 -shuffle=on -timeout=%s -run %s ./internal/sync\n"` |
| `:85` | `"./internal/sync",` (arguments of step "sync concurrency race x50", `:78-86`) |
| `:91` | `"==> go test -json -race -count=3 -shuffle=on -timeout=%s ./internal/sync\n"` |
| `:101` | `"./internal/sync",` (arguments of step "sync race x3", `:94-102`) |

These are the only production references to `./internal/sync` in `internal/devtool`, `cmd/devtool` and `.github`. Comments that describe the target: `verify_stress.go:14-17`, `:50-55`.

Tests that pin them, all in `internal/devtool/verify_runner_test.go`:

| Line | Test | Pin |
|---|---|---|
| `:780` | `TestRunVerifyStressRunsExpectedSteps` (`:742-793`, `// Validates: R-6.10.8` at `:741`) | `"./internal/sync"` in the x50 arguments |
| `:791` | same | `"./internal/sync"` in the x3 arguments |
| `:802` | `TestRunVerifyStressWritesSlowTestSummaryToStdoutAndJSON` (`:795-833`) | fake-runner key `"go test -json -race -count=3 -shuffle=on -timeout=40m ./internal/sync"` |

(`:898` also contains `"./internal/sync"`, but it is a rerun package parsed from a failed log in the `failed-test` tests, unrelated.)

Docs: `spec/design/system.md:248` ("all of `./internal/sync` under `-race -count=3 -shuffle=on -timeout=40m`"), `:425`.

`internal/sync` has only `testdata/` sub-directories today, so the two spellings select the same tests **today**.

### 6.2 Why it is a problem

The planned split of `internal/sync` into sub-packages moves tests out of the directory the stress lane names. The lane then runs fewer tests each time a package is extracted and stays green. The x50 step has a second way to go empty:

**F6.1 The `-run` step is unguarded. VERIFIED.** `runStressCommand` calls plain `runner.Run` (`verify_stress.go:128-144`, call at `:139`), not `runGoTestMatching`. The guard's own comment records that this already happened: "That is how the stress profile's watch-ordering step passed for months against tests that do not exist. Every targeted step in this package routes through here" (`internal/devtool/verify_test_match.go:18-22`); `system.md:346` repeats the claim. Its callers are three e2e steps only (`verify_e2e.go:600`, `:654`, `:942`).

**F6.2 The guard cannot be used on a tree as it is. VERIFIED.** `assertTestsMatched` fails on any occurrence of the marker (`verify_test_match.go:56-65`, `strings.Contains` at `:57`). With `./internal/sync/...` and a `-run` pattern, any sub-package without a matching test prints the marker and the step would be falsely red.

**F6.3 The slow-test summary keys by test name only. VERIFIED.** `goTestJSONEvent` has `Action`, `Test`, `Elapsed` (`verify_stress.go:36-40`); totals are keyed `totals[event.Test]` (`:198-201`). Two sub-packages with a same-named test would be merged.

### 6.3 Proposed end state

- `verify_stress.go:75`, `:85`, `:91`, `:101`: `./internal/sync/...`. Update the comments at `:14-17` and `:50-55`.
- `internal/multisync` (`:60`, `:69`) has no sub-packages; making it `./internal/multisync/...` is a symmetric one-line choice.
- New in `verify_test_match.go`, leaving the strict single-package guard as it is for the e2e callers:

```go
// runGoTestMatchingTree is runGoTestMatching for a ./pkg/... target: packages
// without a matching test are expected, a run in which no package ran one is not.
func runGoTestMatchingTree(ctx context.Context, runner commandRunner, repoRoot string,
	env []string, stdout, stderr io.Writer, stepDescription string, args ...string) error

// assertTestsMatchedInTree fails when the marker is present and no package ran a test.
func assertTestsMatchedInTree(output string, stepDescription string) error
```

  "Ran a test" means: a raw line starting `ok  \t` without the marker, a line starting `--- PASS`, `--- FAIL` or `FAIL\t`, or a `-json` event with a non-empty `Test` and action `pass` or `fail`.
- `runStressCommand` (`:128-144`) routes a step with a `-run` argument through `runGoTestMatchingTree`.
- `goTestJSONEvent` gains `Package` and `Output`; `StressSlowTestSummary` (`verify.go:127-132`) gains `Package string` with tag `json:"package,omitempty"`; `summarizeStressSlowTests` (`verify_stress.go:177-237`) keys by package and test; `writeStressSlowTestSummary` (`:239-264`, format at `:252`) prints the package before the test name. The pinned substring `"TestSlow runs=2 total=2.5s max=1.5s"` (`verify_runner_test.go:822`) still matches, and the fake events there already carry a `Package` field (`:803-806`).

### 6.4 Tests to write first

- `verify_runner_test.go:780`, `:791`, `:802`: expect `./internal/sync/...`.
- `internal/devtool/verify_test_match_test.go` (existing tests at `:16`, `:28`, `:39`, each `// Validates: R-6.10.1`): add `TestAssertTestsMatchedInTree_AcceptsTreeWithSomeEmptyPackages`, `…_RejectsTreeWhereNothingRan`, `…_HandlesJSONOutput`.
- `TestRunVerifyStressFailsWhenConcurrencyPatternMatchesNothing` (fake runner writes only marker lines for the x50 command).
- `TestSummarizeStressSlowTestsKeysByPackage`.

### 6.5 Spec and docs

`system.md:248`, `:425`, `:346` (the "every targeted step" sentence becomes true). R-6.10.8 (`non-functional.md:187`) needs no change.

### 6.6 Risks

- The tree-aware guard is weaker than a per-package one: if the watch tests are renamed so that only one sub-package still matches `Cancel`, the step passes with less content. Pattern selection is inherently loose; the mitigation is the x3 whole-tree step, which has no pattern.

---

## 7. Anything else the files prove

Each row: what, where, mark, and the smallest fix.

| # | Finding | Evidence | Mark | Fix |
|---|---|---|---|---|
| a | **Go deadlines exceed job deadlines.** Stress: 20m + 40m + 40m = 100 minutes of `go test` deadline in a 90-minute job. e2e-full: 190 in 90 (item 2). The file states the opposite intent for the other lanes: "so the test deadline, not the job, ends a slow run". When the job deadline wins there is no goroutine dump and no summary JSON, since `finalize` writes it only if it runs. | `verify.go:28-30`; `ci.yml:213`, `:355`, `:95-96`, `:135-136`; `verify_summary.go:92-131` | VERIFIED (recorded stress totals: 36–54 min) | per-step constants whose sum fits; the workflow-reading test of 2.3 |
| b | **The format step cannot fail.** `runFormat` runs `gofumpt -w .` and `goimports -w .` and returns nil whether or not files changed. `verify public` formats before it lints, in one checkout, so the nightly cannot be red on format. | `verify.go:446-473` (`:450`, `:457-468`), `:349`; quick `verify_quick.go:104-107` | VERIFIED. Whether the separate `verify-lint` job catches it through the formatters enabled in `.golangci.yml:14-21` is SUSPECTED; a PR with one mis-formatted line settles it | a `--check-format` option that lists instead of writing and fails on output, passed by `ci.yml` |
| c | **Stale timing in the stress comment and design doc**: "about six minutes"; recorded 639.7 s, about 8.4 min per stress iteration, 891.8 s on macOS (74% of the 20-minute deadline, with the comment at `verify.go:553-556` already noting macOS came within seconds of the default). | `verify_stress.go:50-52`; `system.md:248`; run `42865435904` | VERIFIED | correct the text; watch the macOS margin when item 5 lands |
| d | **`public` and `stress` stop at the first failing step.** Lint failure skips the unit tests; a failing multisync x50 skips both sync steps. | `verify.go:352-354`, `:595-613`; `verify_stress.go:107-123` (`:111-113`, `:120-122`) | VERIFIED | stress: run all three steps, `errors.Join`; public: run unit tests even when lint or a repo rule fails |
| e | **The auth-preflight rerun has no cause check.** Under `--classify-live-quirks`, any auth preflight failure is rerun once and a passing rerun returns nil, labelled with incident `LI-20260405-06`. The design text says only "a tiny documented registry of known provider-consistency failures" is downgraded. On a failed rerun the returned error wraps the first attempt's error, not the rerun's. | `verify_e2e.go:600-635` (rerun `:610`, `return nil` `:623-633`, `:635`); `verify.go:22`, `:32`; `system.md:248` | VERIFIED | match the first failure against `authE2EPreflightPattern` before rerunning, as `classifyFastE2EQuirk` does for the suite (`verify_e2e.go:747-764`) |
| f | **`failed-test` reruns non-e2e failures without `-race` and without the seed.** A race-only or seed-only failure "passes" on rerun. | `verify_failed.go:165-170` | VERIFIED | item 4 (3) |
| g | `--coverage-threshold 0` becomes the default. | `verify.go:208-211` | VERIFIED | item 5 |
| h | **Stale pointer** in the auth-failure guidance: "See CLAUDE.md 'Test Credentials' section"; the heading is "Test credential pipeline". | `ci.yml:510`; `CLAUDE.md:228` | VERIFIED | fix the text |
| i | Live jobs skip silently on the nightly when the Azure variable is empty. | `ci.yml:255`, `:362` | VERIFIED | item 1: `not-run` on a required job is red |
| j | Nightly starts 6–10 hours after its documented time. | `ci.yml:11-12`; `system.md:423`; November run creation times | VERIFIED | document the observed window; moving the cron off minute 0 may help (SUSPECTED; a week of runs settles it). The 48-hour staleness rule in item 1 tolerates the delay |
| k | Push, schedule and dispatch share the group `ci-<sha>`. | `ci.yml:16` | VERIFIED; the cancellation scenario is derived | item 1 |
| l | **A skipped auth preflight is recorded as a pass.** The test calls `t.Skipf("provider read-only window: …")`; no devtool or workflow code looks for skips; the design doc says the window is "reported as an untestable live lane". | `e2e/auth_preflight_e2e_test.go:57`, `:69-84`; `system.md:16`, `:343` | doc-versus-verifier gap VERIFIED; what the later steps do in such a window SUSPECTED (a recorded run during a window settles it) | detect `--- SKIP:` in the teed preflight output, record status `skipped` in the summary and print one greppable line |
| m | `verify-full` writes no summary JSON; stress and e2e do. | `ci.yml:174` against `:226`, `:449` | VERIFIED | pass `--summary-json` and upload it |
| n | `nightly check` passes on a red run with no derivable key and on "no finished scheduled run". | `nightly.go:151-153`, `:155-161`, `:189-195` | VERIFIED | item 1 |
| o | `pruneNightlyClaims` confirms a fix for a test that did not run. | `nightly.go:377-381` | VERIFIED | item 1 |
| p | Post-merge classification ignores the event. | `verify_dod.go:1358-1412` | VERIFIED | item 1 |
| q | The seed comment describes a run no job performs. | `engine_model_based_test.go:38-40` | VERIFIED | item 4 |
| r | The commit rollup on `main` excludes the nightly's check runs. | F1.3 | VERIFIED for one commit | item 1 |
| s | Output of the classification paths and of the stress x3 step is buffered until the command returns, so a job-level timeout loses it. | `verify_e2e.go:521-524`, `:853-856`; `verify_stress.go:158-159` | VERIFIED | stream through a tee (item 2) |
| t | Outside the brief, one line: three steps echo drive identifiers or secret names into the logs of a public repository. | `ci.yml:315`, `:328`, `:432` | VERIFIED | mask or drop the echo |

---

## 8. Pull requests, in dependency order

File counts are estimates.

| # | Pull request | Depends on | Files (about) |
|---|---|---|---|
| 1 | **Stress targets the tree; tree-aware empty-match guard; slow tests keyed by package** (item 6; 7c text). `verify_stress.go`, `verify_test_match.go`, `verify_test_match_test.go`, `verify.go` (summary struct), `verify_runner_test.go`, `system.md` | none | 6 |
| 2 | **One coverage number, `-coverpkg=./...`** (item 5; 7g). `verify.go`, new `verify_coverage.go` and its test, `cmd/devtool/main.go`, `main_test.go`, `verify_runner_test.go`, `ci.yml`, `non-functional.md`, `system.md`, `CONTRIBUTING.md`. Needs one dispatch run of the branch to choose N | none | 10 |
| 3 | **e2e runs every bucket; failed tests and `not-run` in the summary; streamed output; deadline invariant** (item 2; 7a, 7d for stress, 7s). `verify_e2e.go`, `verify_summary.go`, `verify.go`, `verify_stress.go`, `verify_runner_test.go`, `ci.yml`, `system.md`, `non-functional.md`, `CLAUDE.md` | none (touches `verify_stress.go` after PR 1) | 9 |
| 4 | **Nightly soundness**: shared job verdict, no-verdict and unattributed keys, staleness, sound prune, post-merge push filter, concurrency group (items 1.3 second half; 7n, 7o, 7p, 7k). `nightly.go`, `nightly_test.go`, `verify_dod.go` and its test, `ci.yml`, `non-functional.md`, `system.md` | none | 7 |
| 5 | **`nightly report`, the `nightly-report` job, R-6.10.24, badge** (item 1; 7i, 7m, 7r). New `nightly_report.go` and test additions in `nightly_test.go`, `nightly.go`, `cmd/devtool/main.go`, `main_test.go`, `ci.yml`, `README.md`, `non-functional.md`, `system.md`, `CLAUDE.md` | 4; better after 3 | 10 |
| 6 | **`verify-race` on push** (item 3). New `verify_race.go`, `verify.go`, `verify_quick.go`, `cmd/devtool/main.go`, `verify_runner_test.go`, `main_test.go`, `ci.yml`, `non-functional.md`, `system.md`, `CLAUDE.md`, `CONTRIBUTING.md` | the push filter from 4 | 11 |
| 7 | **Seed helper, start variables, `seed-replay:` line, `failed-test` replay** (item 4 parts 1–3; 7f, 7q). `internal/sync/test_helpers_test.go`, the three seed test files, new `internal/devtool/seed_replay.go` and test, `verify_failed.go`, `verify_runner_test.go` | none | 9 |
| 8 | **`seeds` profile, `model-seeds` job, per-seed issues, R-6.10.25** (item 4 parts 4–5). New `verify_seeds.go`, `verify.go`, `nightly.go`, `nightly_report.go`, `nightly_test.go`, `verify_runner_test.go`, `cmd/devtool/main.go`, `ci.yml`, `non-functional.md`, `system.md` | 1 (tree guard), 5, 7 | 10 |

Small fixes ride with the PR that touches the file: 7h (`ci.yml:510`) with any `ci.yml` PR; 7e (auth-preflight cause check) and 7l (skip detection) with PR 3; 7b (format check) with PR 2 or on its own; 7t on its own.

PRs 1, 2, 3, 4 and 7 are independent of each other. Only one `devtool verify` should run at a time on the 2-core host, and at most two PRs should be pushed at once because the live-account jobs share one concurrency group (`ci.yml:244-245`).

---

## 9. SUSPECTED points and what settles each

| Point | Settled by |
|---|---|
| The rollup takes only one suite per workflow (F1.3) | a second commit with both a push and a scheduled run |
| A completed job's log is readable while its run is in progress | the first `workflow_dispatch` dry run of `nightly-report` |
| Job-level `issues: write` / `statuses: write` work with the default `read`; `issues: write` covers label creation | the first scheduled run of `nightly-report` |
| A commit status joins `statusCheckRollup` | one rollup query after that run |
| The badge honours `event=schedule` | GitHub's badge documentation |
| Conclusion of a job that hits `timeout-minutes` | documentation or one observed timeout (the design is safe either way) |
| Bot-created issues notify the owner | the owner's notification settings |
| `failedTopLevelTests` matches `--- FAIL:` inside `-json` lines | reading `internal/devtool/nightly.go:668-690` |
| Race time of `./internal/sync/...` alone; gain from sharding | one run of `verify-race` |
| Non-race time per model seed | one dispatch run of `model-seeds` |
| The total with `-coverpkg=./...`, and its cost on macOS | one dispatch run of the coverage branch |
| `golangci-lint run` reports formatter diffs in `verify-lint` | a PR with one mis-formatted line |
| Behaviour of later e2e steps in a provider read-only window | a recorded run during a window |
| Moving the cron minute shortens the start delay | a week of runs |
| GitHub's own failure e-mail for scheduled runs reaches the owner | not answerable from the repository |

---

## 10. Not read

- `internal/devtool/verify_dod.go` outside `:19`, `:27`, `:108-123`, `:189`, `:241`, `:741`, `:769`, `:1318-1415`; its test file (the existing `classifyPostMergeRuns` tests are named from the function, not read).
- The implementations behind `repoRuleSteps` (only the list at `verify.go:582-593`).
- The `e2e/` tree apart from `e2e/auth_preflight_e2e_test.go:57-84`; whether `./e2e/...` has sub-packages (this is why the strict single-package guard is left unchanged for the e2e callers in 6.3).
- How the tests in `verify_runner_test.go` build their `repoRoot` (relevant to the fallback in 5.2).
- `sanitizeNightlyKey` (`nightly.go:693-704`) was read earlier in the investigation; its exact character rules are not restated here, so the seed key format in 4.3 must be checked against it.
- `scripts/`, `Makefile`-style wrappers and any non-devtool entry point.
- No `go` command, test or build was run; nothing was measured locally.
- GitHub behaviour that a read-only caller cannot observe (section 9).

Citations taken from a read earlier in this investigation and not re-opened in the final pass (the content was read; a line number may be off by a few lines): `spec/design/system.md:341`-area wording on classified reruns (cited here as `:248` instead), `.golangci.yml:14-21`, `internal/devtool/runner.go:15-19` (the `commandRunner` interface: `Run`, `Output`, `CombinedOutput`), `e2e/auth_preflight_e2e_test.go:57`, `:69-84`, `ci.yml:273-288`, `:315`, `:328-351`, `:432`, `:443-510`, `internal/devtool/verify_summary.go` line ranges, `internal/devtool/verify_failed.go` line ranges, `internal/devtool/nightly.go:589-707`.
