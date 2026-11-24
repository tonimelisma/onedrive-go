package devtool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type VerifyProfile string

const (
	defaultCoveragePattern           = "onedrive-go-cover.*"
	fullE2ECompileArtifactName       = "onedrive-go-e2e-full.test"
	authPreflightTestName            = "TestE2E_AuthPreflight_Fast"
	authE2EPreflightPattern          = "^" + authPreflightTestName + "$"
	fastE2EPreflightPattern          = "^TestE2E_FixturePreflight_Fast$"
	fullE2EFixturePreflight          = "TestE2E_FixturePreflight_Full"
	fullE2EPreflightPattern          = "^" + fullE2EFixturePreflight + "$"
	stressSlowTestLimit              = 5
	authPreflightIncidentID          = "LI-20260405-06"
	fastDownloadIncidentID           = "LI-20260405-04"
	fastDownloadTestName             = "TestE2E_Sync_DownloadOnly"
	websocketRestartIncidentID       = "LI-20260405-03"
	websocketRestartTestName         = "TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart"
	e2eSkipSuiteScrubEnvVar          = "ONEDRIVE_E2E_SKIP_SUITE_SCRUB"
	e2eRunAuthPreflightEnvVar        = "ONEDRIVE_E2E_RUN_AUTH_PREFLIGHT"
	e2eRunFastFixturePreflightEnvVar = "ONEDRIVE_E2E_RUN_FAST_FIXTURE_PREFLIGHT"
	e2eTimingEventsFileName          = "timing-events.jsonl"
	e2eTimingSummaryFileName         = "timing-summary.json"
	e2eQuirkEventsFileName           = "quirk-events.jsonl"
	e2eQuirkSummaryFileName          = "quirk-summary.json"

	fullE2EParallelMiscParallel = 5
	fullE2ESerialParallel       = 1
	fastE2EParallel             = 5

	verifySummaryStatusPass = "pass"
	verifySummaryStatusFail = "fail"
	// verifySummaryStatusNotRun is a step or bucket the profile did not start
	// because an earlier failure or a cancellation made it pointless.
	verifySummaryStatusNotRun = "not-run"
	// verifySummaryStatusSkipped is a step that ran and whose tests skipped,
	// so it verified nothing.
	verifySummaryStatusSkipped = "skipped"

	VerifyPublic      VerifyProfile = "public"
	VerifyPR          VerifyProfile = "pr"
	VerifyQuick       VerifyProfile = "quick"
	VerifyLint        VerifyProfile = "lint"
	VerifyPlatform    VerifyProfile = "platform"
	VerifyE2E         VerifyProfile = "e2e"
	VerifyE2EFull     VerifyProfile = "e2e-full"
	VerifyIntegration VerifyProfile = "integration"
	VerifyStress      VerifyProfile = "stress"
	VerifyRace        VerifyProfile = "race"
	VerifySeeds       VerifyProfile = "seeds"
	VerifyFailedTest  VerifyProfile = "failed-test"
)

// Go test deadlines of the live profiles; the race and seeds profiles keep
// theirs beside their steps (verify_race.go, verify_seeds.go), the stress
// lane's repeated steps derive theirs from a budget for one pass
// (stressRepeatedStep.timeout), its race runs of the sync tree take the race
// profile's once per run (stressSyncRaceTimeout), and the unit tiers', the
// race profile's and the linter's scale with the cores (deadlineForCores).
// Every go test a profile starts names its own, and the deadlines one CI
// job's profile can start, classified reruns included, sum with the job's
// setup to no more than that job's timeout-minutes
// (TestCIJobTimeoutsCoverEveryGoTestDeadline). A hung test then ends at go
// test's deadline, with a goroutine dump that names it and a verify summary,
// not at the job's, which leaves neither.
const (
	// e2ePreflightTimeout bounds each preflight. The auth preflight's own
	// budget is 90 seconds per endpoint per drive.
	e2ePreflightTimeout   = "10m"
	fastE2EPackageTimeout = "10m"
	// The bucket deadlines sit well above the recorded nightly durations
	// (about 2, 15 and 16 minutes in November 2026), so a slow night passes
	// and a hang fails at its own deadline.
	fullE2EParallelMiscTimeout      = "15m"
	fullE2ESerialSyncTimeout        = "40m"
	fullE2ESerialWatchSharedTimeout = "40m"
	// classifiedRerunTimeout bounds the one-test rerun of a known live quirk.
	classifiedRerunTimeout = "10m"
	// integrationTimeout bounds the integration tier, whose runs take about
	// two minutes.
	integrationTimeout = "5m"
)

type VerifyOptions struct {
	RepoRoot string
	// Profile is the profile to run. Empty means the caller named none: a plain
	// run and the DoD pre-pr stage then use `pr`, the local pre-PR gate.
	Profile           VerifyProfile
	CoverageThreshold float64
	CoverageFile      string
	Shard             string
	// LintPass names the one lint pass to run (--lint-pass), a matrix leg of
	// the CI lint job; empty runs every pass.
	LintPass string
	// UnitTimeout is go test's per-package deadline in the full race tier.
	// Zero derives it from Cores.
	UnitTimeout time.Duration
	// ShortUnitTimeout is go test's per-package deadline in the short unit
	// tier. Zero derives it from Cores.
	ShortUnitTimeout time.Duration
	// RaceTimeout is go test's deadline in the race profile. Zero derives it
	// from Cores.
	RaceTimeout time.Duration
	// Cores is the core count the go test deadlines that scale with the cores
	// are derived for: the unit tiers', the race profile's and the stress
	// lane's race runs of the sync tree. Zero or less is the cores this
	// process may use; a test sets it to derive a CI runner's go test
	// deadlines on any host.
	Cores           int
	E2ELogDir       string
	SummaryJSONPath string
	// VerifyLogDir is where a failed go test step keeps its full output
	// (--verify-log-dir). Empty is .verify-logs under RepoRoot.
	VerifyLogDir       string
	ClassifyLiveQuirks bool
	DOD                bool
	DODStage           DODStage
	DODPR              int
	DODRecentMerged    int
	DODCommentManifest string
	DODBranch          string
	// DODCITimeout is the hard cap on one DoD CI wait. Zero allows one stall
	// window for the first attempt and one for every rerun.
	DODCITimeout time.Duration
	// DODCIMaxReruns is how many times one DoD CI wait may rerun the canceled
	// jobs of the head commit's run. Zero reruns nothing.
	DODCIMaxReruns int
	FailedTestLog  string
	// CheckFormat makes the format step list the files the formatters would
	// change and fail when there are any, instead of rewriting them.
	CheckFormat bool
	// SeedStart is the seeds profile's first seed (`--seed-start`). Zero
	// derives it from the UTC date of Now.
	SeedStart uint64
	// Now is the clock of the seeds profile's date; nil is the real clock.
	Now func() time.Time
	// AsUser names the user the go test steps run as when verify runs as
	// root on Linux (`--as-user`, default from DEVTOOL_TEST_USER). Empty
	// runs them as root, after one warning.
	AsUser string
	Stdout io.Writer
	Stderr io.Writer

	// testUserHost stands in for the host's user database, tools and
	// effective uid in tests; nil reads the real host.
	testUserHost *testUserHost
}

type VerifySummary struct {
	Profile          string                   `json:"profile"`
	Status           string                   `json:"status"`
	TotalDurationMS  int64                    `json:"total_duration_ms"`
	Steps            []VerifyStepSummary      `json:"steps"`
	QuirkEventCount  int                      `json:"quirk_event_count"`
	ClassifiedReruns []ClassifiedRerunSummary `json:"classified_reruns,omitempty"`
	E2EFullBuckets   []E2EBucketSummary       `json:"e2e_full_buckets,omitempty"`
}

type VerifyStepSummary struct {
	Name       string                  `json:"name"`
	Status     string                  `json:"status"`
	DurationMS int64                   `json:"duration_ms"`
	Error      string                  `json:"error,omitempty"`
	SlowTests  []StressSlowTestSummary `json:"slow_tests,omitempty"`
	// FailedTests names the top-level tests of a failed step.
	FailedTests []string `json:"failed_tests,omitempty"`
	// LogPath is the file that kept the full output of a failed go test step.
	LogPath string `json:"log_path,omitempty"`
}

type ClassifiedRerunSummary struct {
	IncidentID   string `json:"incident_id"`
	Phase        string `json:"phase"`
	Trigger      string `json:"trigger"`
	RerunCommand string `json:"rerun_command"`
	DurationMS   int64  `json:"duration_ms"`
	Status       string `json:"status"`
}

type E2EBucketSummary struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	RunPattern string `json:"run_pattern"`
	Parallel   int    `json:"parallel"`
	Timeout    string `json:"timeout"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	// Error is the failure of a failed bucket, or why a bucket did not run.
	Error string `json:"error,omitempty"`
	// FailedTests names the top-level tests of a failed bucket.
	FailedTests []string `json:"failed_tests,omitempty"`
	// LogPath is the file that kept the full output of a failed bucket.
	LogPath string `json:"log_path,omitempty"`
}

type StressSlowTestSummary struct {
	Package        string `json:"package,omitempty"`
	Test           string `json:"test"`
	Runs           int    `json:"runs"`
	TotalElapsedMS int64  `json:"total_elapsed_ms"`
	MaxElapsedMS   int64  `json:"max_elapsed_ms"`
}

type verifySummaryCollector struct {
	summary VerifySummary
	stdout  io.Writer
	// stderr takes the marker line of every step that fails.
	stderr          io.Writer
	summaryJSONPath string
	e2eLogDir       string
	startedAt       time.Time
	// stepLogs keeps the output of every go test step; nil keeps none.
	stepLogs *stepLogs
}

type fullE2EBucketKind string

const (
	fullE2EBucketKindParallelMisc      fullE2EBucketKind = "parallel_misc"
	fullE2EBucketKindSerialSync        fullE2EBucketKind = "serial_sync"
	fullE2EBucketKindSerialWatchShared fullE2EBucketKind = "serial_watch_shared"
)

type fullE2EBucketSpec struct {
	Name      verifyStepName
	Kind      fullE2EBucketKind
	TestNames []string
	Parallel  int
	Timeout   string
}

type verifyPlan struct {
	runPublicChecks bool
	runQuickChecks  bool
	runLint         bool
	// lintChangedOnly lints only the packages changed against origin/main and
	// their direct importers (lintChangedPackages): the pr profile's local gate.
	lintChangedOnly   bool
	runPlatformChecks bool
	runE2E            bool
	runE2EFull        bool
	runIntegration    bool
	runStress         bool
	runRace           bool
	runSeeds          bool
	runFailedTest     bool
	// checkFormat makes the format step list instead of rewrite (--check-format).
	checkFormat bool
	// lintPasses are the golangci-lint passes of the lint step: every pass,
	// or the one --lint-pass names.
	lintPasses []lintPass
}

// setCheckFormat applies --check-format, which only a plan with a format step
// takes.
func (p *verifyPlan) setCheckFormat(checkFormat bool) error {
	if checkFormat && !p.runPublicChecks && !p.runQuickChecks {
		return errors.New("--check-format applies to the profiles with a format step: public, pr and quick")
	}
	p.checkFormat = checkFormat

	return nil
}

func RunVerify(ctx context.Context, runner commandRunner, opts *VerifyOptions) (runErr error) {
	if opts != nil {
		host := defaultTestUserHost()
		if opts.testUserHost != nil {
			host = *opts.testUserHost
		}
		_, stderr := resolveVerifyWriters(opts)
		switched, err := withTestUser(runner, opts.AsUser, host, stderr)
		if err != nil {
			return fmt.Errorf("choose test user: %w", err)
		}
		runner = switched
	}

	if opts != nil && opts.DOD {
		if err := RunVerifyDOD(ctx, runner, opts); err != nil {
			return fmt.Errorf("run dod verify: %w", err)
		}

		return nil
	}

	return runVerifyProfile(ctx, runner, opts)
}

func runVerifyProfile(ctx context.Context, runner commandRunner, opts *VerifyOptions) error {
	return runVerifyProfileWithEnv(ctx, runner, opts, os.Environ())
}

// runVerifyProfileWithEnv runs one verify profile with baseEnv as the
// environment of every command it starts, plus the Go toolchain go.mod pins
// when baseEnv does not choose one itself.
func runVerifyProfileWithEnv(
	ctx context.Context, runner commandRunner, opts *VerifyOptions, baseEnv []string,
) (runErr error) {
	if opts == nil {
		return errors.New("verify options are required")
	}

	profile := opts.Profile
	if profile == "" {
		profile = VerifyPR
	}

	plan, err := resolveVerifyPlan(profile)
	if err != nil {
		return err
	}
	shard, err := applyVerifyFlags(&plan, opts)
	if err != nil {
		return err
	}
	toolchain, err := pinnedGoToolchain(opts.RepoRoot)
	if err != nil {
		return err
	}
	env := withPinnedToolchain(baseEnv, toolchain)

	stdout, stderr := resolveVerifyWriters(opts)
	effectiveE2ELogDir := resolvedE2ELogDir(opts.E2ELogDir, plan)
	collector := newVerifySummaryCollector(profile, stdout, stderr, opts.SummaryJSONPath, effectiveE2ELogDir)
	stdout, stderr = collector.keepStepLogs(
		newStepLogs(resolveVerifyLogDir(opts.VerifyLogDir, opts.RepoRoot), profile, shard), stdout, stderr,
	)
	defer func() {
		if finalizeErr := collector.finalize(runErr); finalizeErr != nil {
			if runErr == nil {
				runErr = finalizeErr
			} else {
				runErr = errors.Join(runErr, finalizeErr)
			}
		}
	}()
	coverageThreshold, err := resolveCoverageThreshold(plan, opts.RepoRoot, opts.CoverageThreshold)
	if err != nil {
		return err
	}

	coverageFile, cleanup, err := prepareCoverageFile(plan, opts.CoverageFile)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil && runErr == nil {
			runErr = cleanupErr
		}
	}()

	if err := runPublicVerification(
		ctx,
		runner,
		opts.RepoRoot,
		env,
		collector,
		stdout,
		stderr,
		coverageFile,
		coverageThreshold,
		resolveUnitTestTimeout(opts.UnitTimeout, verifyCores(opts)),
		plan,
	); err != nil {
		return err
	}
	if err := runPRLaneVerification(
		ctx,
		runner,
		opts.RepoRoot,
		env,
		collector,
		stdout,
		stderr,
		plan,
		shard,
		resolveShortUnitTestTimeout(opts.ShortUnitTimeout, verifyCores(opts)),
	); err != nil {
		return err
	}

	runErr = runOptionalVerification(ctx, runner, opts, env, collector, stdout, stderr, plan)

	return runErr
}

// applyVerifyFlags checks the flags that only some profiles take against the
// plan and applies them: --shard, --seed-start, --lint-pass and
// --check-format.
func applyVerifyFlags(plan *verifyPlan, opts *VerifyOptions) (verifyShard, error) {
	shard, err := resolveVerifyShard(*plan, opts.Shard)
	if err != nil {
		return verifyShard{}, err
	}
	if opts.LintPass != "" && !plan.runLint && !plan.runPublicChecks {
		return verifyShard{}, errors.New("--lint-pass applies to the profiles that lint: public, pr and lint")
	}
	if plan.lintPasses, err = resolveLintPasses(opts.LintPass); err != nil {
		return verifyShard{}, err
	}
	if opts.SeedStart != 0 && !plan.runSeeds {
		return verifyShard{}, errors.New("--seed-start applies to the seeds profile only")
	}

	return shard, plan.setCheckFormat(opts.CheckFormat)
}

func resolveVerifyPlan(profile VerifyProfile) (verifyPlan, error) {
	switch profile {
	case VerifyPublic:
		return verifyPlan{
			runPublicChecks: true,
		}, nil
	case VerifyPR:
		return verifyPlan{
			runLint:         true,
			lintChangedOnly: true,
			runQuickChecks:  true,
		}, nil
	case VerifyQuick:
		return verifyPlan{
			runQuickChecks: true,
		}, nil
	case VerifyLint:
		return verifyPlan{
			runLint: true,
		}, nil
	case VerifyPlatform:
		return verifyPlan{
			runPlatformChecks: true,
		}, nil
	case VerifyE2E:
		return verifyPlan{
			runE2E: true,
		}, nil
	case VerifyE2EFull:
		return verifyPlan{
			runE2E:     true,
			runE2EFull: true,
		}, nil
	case VerifyIntegration:
		return verifyPlan{
			runIntegration: true,
		}, nil
	case VerifyStress:
		return verifyPlan{
			runStress: true,
		}, nil
	case VerifyRace:
		return verifyPlan{
			runRace: true,
		}, nil
	case VerifySeeds:
		return verifyPlan{
			runSeeds: true,
		}, nil
	case VerifyFailedTest:
		return verifyPlan{
			runFailedTest: true,
		}, nil
	default:
		return verifyPlan{}, fmt.Errorf("usage: devtool verify [%s]", VerifyProfileUsage)
	}
}

func resolveVerifyWriters(opts *VerifyOptions) (io.Writer, io.Writer) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	return stdout, stderr
}

func prepareCoverageFile(plan verifyPlan, coverageFile string) (string, func() error, error) {
	if !plan.runPublicChecks || coverageFile != "" {
		return coverageFile, func() error { return nil }, nil
	}

	f, err := createTemp(os.TempDir(), defaultCoveragePattern)
	if err != nil {
		return "", nil, fmt.Errorf("create coverage file: %w", err)
	}

	coverageFile = f.Name()
	if err := f.Close(); err != nil {
		return "", nil, fmt.Errorf("close coverage file: %w", err)
	}

	return coverageFile, func() error {
		if err := remove(coverageFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove coverage file %s: %w", coverageFile, err)
		}

		return nil
	}, nil
}

func runPublicVerification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	coverageFile string,
	coverageThreshold float64,
	unitTimeout time.Duration,
	plan verifyPlan,
) error {
	if !plan.runPublicChecks {
		return nil
	}

	// The full tier runs every step and reports every failure. A lint or rule
	// failure says nothing about the tests, and on the nightly, the one run of
	// this tier, stopping there would hide the tests' state for a day.
	publicSteps := []verifyStep{{name: formatStepName, run: formatStep(plan.checkFormat)}}
	publicSteps = append(publicSteps, lintSteps(plan.lintPasses)...)
	publicSteps = append(publicSteps, repoRuleSteps()...)
	publicSteps = append(publicSteps, verifyStep{name: buildStepName, run: runBuild})
	stepsErr := runEveryVerifyStep(ctx, runner, repoRoot, env, collector, stdout, stderr, publicSteps)

	unitErr := collector.runStep(unitTestsStepName, func() error {
		return runUnitTests(ctx, runner, repoRoot, env, coverageFile, unitTimeout, stdout, stderr)
	})
	var coverageErr error
	if unitErr == nil {
		coverageErr = collector.runStep(coverageStepName, func() error {
			return runCoverageGate(ctx, runner, repoRoot, env, coverageFile, coverageThreshold, stdout)
		})
	} else {
		// A failed run leaves a partial coverage profile, and gating it would
		// add a second failure that is only the first one's shadow.
		collector.recordStepNotRun(coverageStepName, "unit tests failed")
	}
	compileErr := collector.runStep(fullE2ECompileStepName, func() error {
		return runE2EFullCompileCheck(ctx, runner, repoRoot, env, stdout, stderr)
	})

	return errors.Join(stepsErr, unitErr, coverageErr, compileErr)
}

func runOptionalVerification(
	ctx context.Context,
	runner commandRunner,
	opts *VerifyOptions,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	plan verifyPlan,
) error {
	e2eEnv := env
	effectiveE2ELogDir := resolvedE2ELogDir(opts.E2ELogDir, plan)
	if effectiveE2ELogDir != "" {
		e2eEnv = append([]string{}, env...)
		e2eEnv = append(e2eEnv, "E2E_LOG_DIR="+effectiveE2ELogDir)
		if err := resetE2EArtifacts(effectiveE2ELogDir); err != nil {
			return err
		}
	}

	if err := runLiveVerification(ctx, runner, opts, e2eEnv, effectiveE2ELogDir, collector, stdout, stderr, plan); err != nil {
		return err
	}

	if plan.runIntegration {
		if err := runIntegration(ctx, runner, opts.RepoRoot, env, collector, stdout, stderr); err != nil {
			return err
		}
	}
	if plan.runStress {
		if err := runStress(ctx, runner, opts.RepoRoot, env, verifyCores(opts), collector, stdout, stderr); err != nil {
			return err
		}
	}
	if plan.runRace {
		raceTimeout := resolveRaceSyncTimeout(opts.RaceTimeout, verifyCores(opts))
		if err := runRaceVerification(ctx, runner, opts.RepoRoot, env, raceTimeout, collector, stdout, stderr); err != nil {
			return err
		}
	}
	if plan.runSeeds {
		if err := runSeedsVerification(ctx, runner, opts.RepoRoot, env, opts, collector, stdout, stderr); err != nil {
			return err
		}
	}
	if plan.runFailedTest {
		if err := runFailedTestVerification(ctx, runner, opts.RepoRoot, env, collector, stdout, stderr, opts.FailedTestLog); err != nil {
			return err
		}
	}

	return nil
}

// runLiveVerification runs the fast live suite and, under e2e-full, the full
// buckets. Under e2e-full it first discovers the full tier, before any live
// call, and every later step uses the buckets that one discovery built; a
// test it cannot place stops the profile there. A fast-suite test failure
// does not stop e2e-full: it says nothing about the buckets, and the night's
// record has to show every failure. A preflight failure does, because the
// account or the fixtures are unusable; the buckets are then recorded as not
// run.
func runLiveVerification(
	ctx context.Context,
	runner commandRunner,
	opts *VerifyOptions,
	e2eEnv []string,
	e2eLogDir string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	plan verifyPlan,
) error {
	var buckets []fullE2EBucketSpec
	if plan.runE2EFull {
		var discoverErr error
		if buckets, discoverErr = runFullE2EDiscovery(collector, opts.RepoRoot, stdout); discoverErr != nil {
			return discoverErr
		}
	}

	var fastErr error
	if plan.runE2E {
		fastErr = runE2E(ctx, runner, opts.RepoRoot, e2eEnv, collector, stdout, stderr, opts.ClassifyLiveQuirks)
		if fastErr != nil && (!plan.runE2EFull || errors.Is(fastErr, errE2EPreflight)) {
			if plan.runE2EFull {
				collector.recordBucketsNotRun(buckets, "fast e2e preflight failed")
			}

			return fastErr
		}
	}
	if !plan.runE2EFull {
		return nil
	}

	fullErr := runE2EFull(ctx, runner, opts.RepoRoot, e2eEnv, e2eLogDir, buckets, collector, stdout, stderr, opts.ClassifyLiveQuirks)

	return errors.Join(fastErr, fullErr)
}

func resolvedE2ELogDir(explicit string, plan verifyPlan) string {
	if explicit != "" {
		return explicit
	}
	if plan.runE2E || plan.runE2EFull {
		return filepath.Join(os.TempDir(), "e2e-debug-logs")
	}

	return ""
}

// formatterCommand is one formatter the format step runs over the tree.
type formatterCommand struct {
	name string
	// args come before the mode flag (-w rewrites, -l lists) and the tree.
	args []string
}

func formatterCommands() []formatterCommand {
	return []formatterCommand{
		{name: "gofumpt"},
		{name: "goimports", args: []string{"-local", repoModulePath}},
	}
}

func (f formatterCommand) argsWithMode(mode string) []string {
	return append(append([]string{}, f.args...), mode, ".")
}

// formatStepName names the format step.
const formatStepName verifyStepName = "format"

// formatStep is the format step of a profile: rewrite the tree, or, with
// --check-format, only check it.
func formatStep(checkOnly bool) func(context.Context, commandRunner, string, []string, io.Writer, io.Writer) error {
	if checkOnly {
		return checkFormat
	}

	return runFormat
}

func runFormat(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	for _, formatter := range formatterCommands() {
		if err := writeStatus(stdout, "==> "+formatter.name+"\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if err := runner.Run(ctx, repoRoot, env, stdout, stderr, formatter.name, formatter.argsWithMode("-w")...); err != nil {
			return fmt.Errorf("format %s: %w", formatter.name, err)
		}
	}

	return nil
}

// checkFormat lists the files each formatter would change, changes none, and
// fails when there are any. A step that rewrites the checkout and returns nil
// can never fail, so CI passes --check-format.
func checkFormat(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	var unformatted []string
	for _, formatter := range formatterCommands() {
		if err := writeStatus(stdout, "==> "+formatter.name+" -l\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}

		var listed bytes.Buffer
		err := runner.Run(ctx, repoRoot, env, io.MultiWriter(stdout, &listed), stderr, formatter.name, formatter.argsWithMode("-l")...)
		if err != nil {
			return fmt.Errorf("check format %s: %w", formatter.name, err)
		}
		for _, file := range strings.Split(listed.String(), "\n") {
			if file = strings.TrimSpace(file); file != "" {
				unformatted = append(unformatted, formatter.name+" "+file)
			}
		}
	}
	if len(unformatted) == 0 {
		return nil
	}

	return fmt.Errorf(
		"check format: the formatters would change %s (verify without --check-format rewrites them)",
		strings.Join(unformatted, ", "),
	)
}

// lintCommandArgs is golangci-lint's command line for the cores the run may
// use. The deadline is passed here, over the `run.timeout` of .golangci.yml,
// because a configuration file cannot follow the host: the fixed value that
// fits a CI runner ends a healthy whole-repository run on a 2-core host that
// other work shares.
func lintCommandArgs(cores int) []string {
	return []string{"run", "--allow-parallel-runners", "--timeout=" + lintTimeoutForCores(cores).String()}
}

// supportedPlatformCount is the number of GOOS values README.md claims support
// for. It is a constant so callers can size things against it; the array
// return type below makes the two impossible to drift apart.
const supportedPlatformCount = 3

// supportedPlatforms returns the GOOS values README.md claims support for.
// Every one is cross-compiled on every verify run so a platform claim cannot
// rot into a build failure that only a user discovers.
func supportedPlatforms() [supportedPlatformCount]string {
	return [supportedPlatformCount]string{"linux", "darwin", "freebsd"}
}

// buildStepName names the build step: the host build, then the build for
// every supported platform.
const buildStepName verifyStepName = "build"

func runBuild(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> go build\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", "build", "./..."); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	return runCrossPlatformBuild(ctx, runner, repoRoot, env, stdout, stderr)
}

func runCrossPlatformBuild(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
) error {
	for _, goos := range supportedPlatforms() {
		if err := writeStatus(stdout, "==> go build GOOS="+goos+"\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}

		platformEnv := append(append([]string{}, env...), "GOOS="+goos, "GOARCH=amd64")
		if err := runner.Run(ctx, repoRoot, platformEnv, stdout, stderr, "go", "build", "./..."); err != nil {
			return fmt.Errorf("build GOOS=%s: %w", goos, err)
		}
	}

	return nil
}

// unitTestsStepName names the full unit tier's step.
const unitTestsStepName verifyStepName = "unit-tests"

func runUnitTests(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	coverageFile string,
	timeout time.Duration,
	stdout, stderr io.Writer,
) error {
	if err := writeStatus(stdout, "==> go test -race -coverpkg=./... -coverprofile\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	// Through the empty-run guard like every go test step: a tier in which no
	// package ran a test fails, instead of handing the coverage gate a profile
	// of nothing. The coverage gate reads the profile file, not this output.
	_, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "unit tests",
		"test",
		"-race",
		// Explicit, not go test's 10-minute default: internal/sync under
		// -race on a CI macOS runner came within seconds of the default and
		// then past it. A deadline with headroom fails a real hang, not a
		// slow runner.
		"-timeout="+timeout.String(),
		// Every package's tests count toward every package's statements.
		// Without it a statement counts only when a test of its own package
		// runs it, so code another package's tests exercise reads as
		// uncovered and the total understates what the suite proves.
		"-coverpkg=./...",
		"-coverprofile="+coverageFile,
		"./...",
	)

	return err
}

const (
	// unitTestBaseTimeout is go test's per-package deadline in the full verify
	// profiles (race detector on, every exhaustive test running) on a host
	// with at least unitTestReferenceCores cores. It was 20 minutes until
	// internal/sync outgrew it on the Linux nightly runner.
	unitTestBaseTimeout = 30 * time.Minute

	// unitTestReferenceCores is the core count the base deadline was set on:
	// GitHub's macOS runner, which has 3 cores and is where internal/sync
	// under -race outgrew go test's 10-minute default. The verify-full job's
	// timeout-minutes sits above the base deadline; a larger reference would
	// push the macOS deadline past it and let the job, not go test, end a
	// hang, without the goroutine dump.
	unitTestReferenceCores = 3

	// unitTestSmallHostCoreBudget is the deadline of a host with fewer cores
	// than the reference, as core time: the deadline is the budget divided by
	// the cores. It is measured, not scaled from the base: a host that small
	// is a developer's machine, slower per core than a CI runner and doing
	// other work beside the run. On a shared 2-core host internal/sync took
	// 63 minutes under -race with -coverpkg, 126 core-minutes, where twice the
	// base deadline (40 minutes) had ended it as a hang. 180 leaves that run
	// about 40% of headroom: 90 minutes on 2 cores, 180 on one.
	unitTestSmallHostCoreBudget = 180 * time.Minute

	// shortUnitTestBaseTimeout is go test's per-package deadline in the short
	// unit tier (no race detector, no exhaustive tests) on a host with at
	// least unitTestReferenceCores cores: a package that takes ten minutes
	// there is hung, not slow.
	shortUnitTestBaseTimeout = 10 * time.Minute

	// shortUnitTestSmallHostCoreBudget is the short tier's deadline on a host
	// with fewer cores than the reference, as core time. It is measured, like
	// the race tier's: on a shared 2-core host internal/sync's short tier
	// passed in 2 to 8 minutes and ran into the 10-minute deadline in a
	// pre-PR gate with no test hung, the same tests taking 2.5 times as long
	// when other work shared the cores. The budget gives that host three
	// times the run that failed: 30 minutes on 2 cores, 60 on one.
	shortUnitTestSmallHostCoreBudget = 60 * time.Minute

	// lintBaseTimeout is golangci-lint's deadline on a host with at least
	// unitTestReferenceCores cores. It is the `run.timeout` .golangci.yml
	// holds for a linter started by hand, and a test keeps the two equal.
	lintBaseTimeout = 15 * time.Minute

	// lintSmallHostCoreBudget is the linter's deadline on a host with fewer
	// cores than the reference, as core time. It is measured, like the unit
	// tiers': on a shared 2-core host the whole-repository lint reported no
	// issue and then ran into the 15-minute deadline in a pre-PR gate, at 16.6
	// minutes, while other builds shared the cores. The budget gives that host
	// three times the deadline that failed: 45 minutes on 2 cores, 90 on one.
	lintSmallHostCoreBudget = 90 * time.Minute
)

// deadlineForCores is the deadline of a CPU-bound verify step (a unit tier's
// go test, the race profile's, the linter) for the cores the run may use. A
// deadline that fits a CI runner fails a healthy run on a 2-core host, so a
// host below the reference gets the step's small-host budget divided by its
// cores. A host at the reference or above keeps the base deadline: more cores
// do not shorten the deadline a real hang is measured against, and the CI job
// timeouts are set above it.
func deadlineForCores(cores int, base, smallHostCoreBudget time.Duration) time.Duration {
	if cores < 1 {
		cores = 1
	}
	if cores >= unitTestReferenceCores {
		return base
	}

	return smallHostCoreBudget / time.Duration(cores)
}

// verifyCores is the core count a run's core-scaled go test deadlines are
// derived for: opts.Cores when it is above zero, else the cores this process
// may use.
func verifyCores(opts *VerifyOptions) int {
	if opts.Cores > 0 {
		return opts.Cores
	}

	return runtime.GOMAXPROCS(0)
}

// unitTestTimeoutForCores is the full race tier's go test deadline for the
// cores the run may use.
func unitTestTimeoutForCores(cores int) time.Duration {
	return deadlineForCores(cores, unitTestBaseTimeout, unitTestSmallHostCoreBudget)
}

// shortUnitTestTimeoutForCores is the short unit tier's go test deadline for
// the cores the run may use.
func shortUnitTestTimeoutForCores(cores int) time.Duration {
	return deadlineForCores(cores, shortUnitTestBaseTimeout, shortUnitTestSmallHostCoreBudget)
}

// lintTimeoutForCores is golangci-lint's deadline for the cores the run may
// use.
func lintTimeoutForCores(cores int) time.Duration {
	return deadlineForCores(cores, lintBaseTimeout, lintSmallHostCoreBudget)
}

// resolveUnitTestTimeout returns the explicit deadline (`--unit-timeout`) when
// one was given, else the race tier's deadline for the cores.
func resolveUnitTestTimeout(explicit time.Duration, cores int) time.Duration {
	if explicit > 0 {
		return explicit
	}

	return unitTestTimeoutForCores(cores)
}

// resolveShortUnitTestTimeout returns the explicit deadline
// (`--short-unit-timeout`) when one was given, else the short tier's deadline
// for the cores.
func resolveShortUnitTestTimeout(explicit time.Duration, cores int) time.Duration {
	if explicit > 0 {
		return explicit
	}

	return shortUnitTestTimeoutForCores(cores)
}

// verifyStepName is the stable name of a step a verify profile runs, a
// bucket of the full live suite included: lower-case words joined by
// hyphens, a constant declared beside its step, never built from a path, a
// package list, a count or a command line. The summary records a step under
// it, a failed step's marker line names it, and the nightly reader keys a
// failed job by it, so a step's name is part of a nightly key.
type verifyStepName string

// verifyStepNames is the closed set of step names: every constant of type
// verifyStepName, each once. TestVerifyProfilesNameEveryStepOnceFromTheClosedSet
// holds it to the constants the package declares and, both ways, to the
// steps the profiles run; the nightly reader takes a marker line only for a
// name in it.
func verifyStepNames() []verifyStepName {
	return []verifyStepName{
		formatStepName,
		lintStepName,
		workflowLintStepName,
		outputBoundariesStepName,
		testConventionsStepName,
		specGovernsStepName,
		retiredBacklogIDsStepName,
		mutexOwnershipDocsStepName,
		requirementTraceabilityStepName,
		buildStepName,
		hostBuildStepName,
		taggedVetStepName,
		unitTestsStepName,
		coverageStepName,
		fullE2ECompileStepName,
		shortUnitTestsStepName,
		fullE2EDiscoveryStepName,
		authPreflightStepName,
		fastFixturePreflightStepName,
		fastE2EStepName,
		fullFixturePreflightStepName,
		fullE2EParallelMiscBucketName,
		fullE2ESerialSyncBucketName,
		fullE2ESerialWatchSharedBucketName,
		integrationStepName,
		stressMultisyncRaceStepName,
		stressSyncConcurrencyRaceStepName,
		stressSyncRaceStepName,
		raceStepName,
		seedsStepName,
		failedTestRerunStepName,
	}
}

// runsGoTest reports whether the step runs go test, whose failed tests name
// the step's failure in its log. A step that runs no go test (the format,
// lint and rule checks, the builds, the tagged vet, the coverage gate, the
// compile of the full live suite and the discovery of its tests) fails in a
// way no test names, so the nightly reader keys its marker whenever a log
// holds it. The switch names every step of the set, so a new step must say
// which it is (TestVerifyStepNamesSayWhetherTheyRunGoTest); a name outside
// the set runs none.
func (name verifyStepName) runsGoTest() bool {
	switch name {
	case unitTestsStepName, shortUnitTestsStepName, authPreflightStepName, fastFixturePreflightStepName,
		fastE2EStepName, fullFixturePreflightStepName, fullE2EParallelMiscBucketName, fullE2ESerialSyncBucketName,
		fullE2ESerialWatchSharedBucketName, integrationStepName, stressMultisyncRaceStepName,
		stressSyncConcurrencyRaceStepName, stressSyncRaceStepName, raceStepName, seedsStepName, failedTestRerunStepName:
		return true
	case formatStepName, lintStepName, workflowLintStepName, outputBoundariesStepName, testConventionsStepName,
		specGovernsStepName, retiredBacklogIDsStepName,
		mutexOwnershipDocsStepName, requirementTraceabilityStepName, buildStepName, hostBuildStepName,
		taggedVetStepName, coverageStepName, fullE2ECompileStepName, fullE2EDiscoveryStepName:
		return false
	}

	return false
}

// verifyStep is one named step of a verify profile, recorded in the summary
// under its name.
type verifyStep struct {
	name verifyStepName
	run  func(context.Context, commandRunner, string, []string, io.Writer, io.Writer) error
}

// The lint profile's steps.
const (
	lintStepName         verifyStepName = "lint"
	workflowLintStepName verifyStepName = "workflow-lint"
)

// lintSteps are the lint profile's steps, which every profile that lints runs:
// golangci-lint over the Go code in each of passes, then actionlint over the
// workflow files.
func lintSteps(passes []lintPass) []verifyStep {
	return []verifyStep{
		{name: lintStepName, run: lintStepRun(passes, lintWholeModule)},
		{name: workflowLintStepName, run: runWorkflowLint},
	}
}

// changedLintSteps are lintSteps with golangci-lint over the changed packages
// only (lintChangedPackages), the pr profile's.
func changedLintSteps(passes []lintPass) []verifyStep {
	return []verifyStep{
		{name: lintStepName, run: lintStepRun(passes, lintChangedPackages)},
		{name: workflowLintStepName, run: runWorkflowLint},
	}
}

// The repository rules' steps.
const (
	outputBoundariesStepName        verifyStepName = "output-boundaries"
	testConventionsStepName         verifyStepName = "test-conventions"
	specGovernsStepName             verifyStepName = "spec-governs"
	retiredBacklogIDsStepName       verifyStepName = "retired-backlog-ids"
	mutexOwnershipDocsStepName      verifyStepName = "mutex-ownership-docs"
	requirementTraceabilityStepName verifyStepName = "requirement-traceability"
)

// repoRuleSteps are the repository's static rules that lint cannot state: the
// flattened-error-attribute, integration-test-name, spec-ownership,
// backlog-id, mutex-doc and traceability checks. They take seconds, so every
// profile that builds the tree runs them.
func repoRuleSteps() []verifyStep {
	return []verifyStep{
		{name: outputBoundariesStepName, run: runOutputBoundaries},
		{name: testConventionsStepName, run: runTestConventions},
		{name: specGovernsStepName, run: runSpecGoverns},
		{name: retiredBacklogIDsStepName, run: runRetiredBacklogIDs},
		{name: mutexOwnershipDocsStepName, run: runMutexDocs},
		{name: requirementTraceabilityStepName, run: runRequirementTraceability},
	}
}

func runVerifySteps(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	steps []verifyStep,
) error {
	for _, step := range steps {
		if err := collector.runStep(step.name, func() error {
			return step.run(ctx, runner, repoRoot, env, stdout, stderr)
		}); err != nil {
			return err
		}
	}

	return nil
}

// runEveryVerifyStep runs every step whatever an earlier one returned, and
// joins their failures. After a cancellation the remaining steps are recorded
// as not run instead of being started against a dead context.
func runEveryVerifyStep(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	steps []verifyStep,
) error {
	var stepErrs []error
	for _, step := range steps {
		if ctx.Err() != nil {
			collector.recordStepNotRun(step.name, "run canceled")
			continue
		}
		if err := collector.runStep(step.name, func() error {
			return step.run(ctx, runner, repoRoot, env, stdout, stderr)
		}); err != nil {
			stepErrs = append(stepErrs, err)
		}
	}
	if err := ctx.Err(); err != nil {
		stepErrs = append(stepErrs, fmt.Errorf("verify steps: %w", err))
	}

	return errors.Join(stepErrs...)
}

// The integration tier's build tag and the name prefix of its tests. The tier
// runs `-run=^TestIntegration_`, and the test-convention check holds every
// test of a file built with the tag to the prefix, so the pattern cannot
// silently leave a new integration test out.
const (
	integrationBuildTag   = "integration"
	integrationTestPrefix = "TestIntegration_"
)

// integrationStepName names the integration tier's one step.
const integrationStepName verifyStepName = "integration"

func runIntegration(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return collector.runStep(integrationStepName, func() error {
		if err := writeStatus(stdout, "==> go test -tags=integration\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}

		// The integration tests only: graph's untagged unit tests belong to
		// the unit tiers, and running them here would pass the tier when no
		// integration test ran. The test-convention check keeps every test of
		// an integration-tagged file under the prefix this pattern selects.
		_, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "integration tests",
			"test",
			"-tags="+integrationBuildTag,
			"-race",
			"-v",
			"-run=^"+integrationTestPrefix,
			"-timeout="+integrationTimeout,
			"./internal/graph/...",
		)

		return err
	})
}

func writeStatus(w io.Writer, text string) error {
	_, err := io.WriteString(w, text)
	if err != nil {
		return fmt.Errorf("write status output: %w", err)
	}

	return nil
}

// TierDeadlineDefaults are the go test deadlines of the full race tier, the
// short tier and the race profile on a host with the CI runners' cores, for
// the command line's help to state the values the verifier uses instead of
// copies of them.
func TierDeadlineDefaults() (unit, short, race time.Duration) {
	return unitTestBaseTimeout, shortUnitTestBaseTimeout, raceSyncBaseTimeout
}
