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
	fullE2ECompileStepName           = "e2e-full compile"
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

	verifySummaryFilePerm = 0o600
	verifySummaryDirPerm  = 0o700

	verifySummaryStatusPass = "pass"
	verifySummaryStatusFail = "fail"
	// verifySummaryStatusNotRun is a step or bucket the profile did not start
	// because an earlier failure or a cancellation made it pointless.
	verifySummaryStatusNotRun = "not-run"
	// verifySummaryStatusSkipped is a step that ran and whose tests skipped,
	// so it verified nothing.
	verifySummaryStatusSkipped = "skipped"

	VerifyDefault     VerifyProfile = "default"
	VerifyPublic      VerifyProfile = "public"
	VerifyPR          VerifyProfile = "pr"
	VerifyQuick       VerifyProfile = "quick"
	VerifyLint        VerifyProfile = "lint"
	VerifyPlatform    VerifyProfile = "platform"
	VerifyE2E         VerifyProfile = "e2e"
	VerifyE2EFull     VerifyProfile = "e2e-full"
	VerifyIntegration VerifyProfile = "integration"
	VerifyStress      VerifyProfile = "stress"
	VerifyFailedTest  VerifyProfile = "failed-test"
)

// Go test deadlines of the live and stress profiles. Every go test those
// profiles start names its own, and the deadlines one CI job's profile can
// start, classified reruns included, sum with the job's setup to no more than
// that job's timeout-minutes (TestCIJobTimeoutsCoverEveryGoTestDeadline). A
// hung test then ends at go test's deadline, with a goroutine dump that names
// it and a verify summary, not at the job's, which leaves neither.
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
	integrationTimeout           = "5m"
	stressMultisyncTimeout       = "20m"
	stressSyncConcurrencyTimeout = "40m"
	stressSyncTimeout            = "40m"
)

type VerifyOptions struct {
	RepoRoot string
	// Profile is the profile to run. Empty means the caller named none: a plain
	// run then uses `default`, and the DoD pre-pr stage uses `pr`.
	Profile           VerifyProfile
	CoverageThreshold float64
	CoverageFile      string
	Shard             string
	// UnitTimeout is go test's per-package deadline in the full race tier.
	// Zero derives it from the cores this process may use.
	UnitTimeout time.Duration
	// ShortUnitTimeout is go test's per-package deadline in the short unit
	// tier. Zero derives it from the cores this process may use.
	ShortUnitTimeout   time.Duration
	E2ELogDir          string
	SummaryJSONPath    string
	ClassifyLiveQuirks bool
	DOD                bool
	DODStage           DODStage
	DODPR              int
	DODRecentMerged    int
	DODCommentManifest string
	DODWorktree        string
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
	Stdout      io.Writer
	Stderr      io.Writer
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
}

type StressSlowTestSummary struct {
	Package        string `json:"package,omitempty"`
	Test           string `json:"test"`
	Runs           int    `json:"runs"`
	TotalElapsedMS int64  `json:"total_elapsed_ms"`
	MaxElapsedMS   int64  `json:"max_elapsed_ms"`
}

type verifySummaryCollector struct {
	summary         VerifySummary
	stdout          io.Writer
	summaryJSONPath string
	e2eLogDir       string
	startedAt       time.Time
}

type fullE2EBucketKind string

const (
	fullE2EBucketKindParallelMisc      fullE2EBucketKind = "parallel_misc"
	fullE2EBucketKindSerialSync        fullE2EBucketKind = "serial_sync"
	fullE2EBucketKindSerialWatchShared fullE2EBucketKind = "serial_watch_shared"
)

type fullE2EBucketSpec struct {
	Name      string
	Kind      fullE2EBucketKind
	TestNames []string
	Parallel  int
	Timeout   string
}

type verifyPlan struct {
	runPublicChecks   bool
	runQuickChecks    bool
	runLint           bool
	runPlatformChecks bool
	runE2E            bool
	runE2EFull        bool
	runIntegration    bool
	runStress         bool
	runFailedTest     bool
	// checkFormat makes the format step list instead of rewrite (--check-format).
	checkFormat bool
}

// setCheckFormat applies --check-format, which only a plan with a format step
// takes.
func (p *verifyPlan) setCheckFormat(checkFormat bool) error {
	if checkFormat && !p.runPublicChecks && !p.runQuickChecks {
		return fmt.Errorf("--check-format applies to the profiles with a format step: default, public, pr and quick")
	}
	p.checkFormat = checkFormat

	return nil
}

func RunVerify(ctx context.Context, runner commandRunner, opts *VerifyOptions) (runErr error) {
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
		return fmt.Errorf("verify options are required")
	}

	profile := opts.Profile
	if profile == "" {
		profile = VerifyDefault
	}

	plan, err := resolveVerifyPlan(profile)
	if err != nil {
		return err
	}
	shard, err := resolveVerifyShard(plan, opts.Shard)
	if err != nil {
		return err
	}
	err = plan.setCheckFormat(opts.CheckFormat)
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
	collector := newVerifySummaryCollector(profile, stdout, opts.SummaryJSONPath, effectiveE2ELogDir)
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
		resolveUnitTestTimeout(opts.UnitTimeout, runtime.GOMAXPROCS(0)),
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
		resolveShortUnitTestTimeout(opts.ShortUnitTimeout, runtime.GOMAXPROCS(0)),
	); err != nil {
		return err
	}

	runErr = runOptionalVerification(ctx, runner, opts, env, collector, stdout, stderr, plan)

	return runErr
}

func resolveVerifyPlan(profile VerifyProfile) (verifyPlan, error) {
	switch profile {
	case VerifyDefault:
		return verifyPlan{
			runPublicChecks: true,
			runE2E:          true,
		}, nil
	case VerifyPublic:
		return verifyPlan{
			runPublicChecks: true,
		}, nil
	case VerifyPR:
		return verifyPlan{
			runLint:        true,
			runQuickChecks: true,
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
	case VerifyFailedTest:
		return verifyPlan{
			runFailedTest: true,
		}, nil
	default:
		return verifyPlan{}, fmt.Errorf("usage: devtool verify [%s]", verifyProfileUsage)
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
	publicSteps := []verifyStep{{"format", formatStep(plan.checkFormat)}}
	publicSteps = append(publicSteps, lintSteps()...)
	publicSteps = append(publicSteps, repoRuleSteps()...)
	publicSteps = append(publicSteps, verifyStep{"build", runBuild})
	stepsErr := runEveryVerifyStep(ctx, runner, repoRoot, env, collector, stdout, stderr, publicSteps)

	unitErr := collector.runStep("unit tests", func() error {
		return runUnitTests(ctx, runner, repoRoot, env, coverageFile, unitTimeout, stdout, stderr)
	})
	var coverageErr error
	if unitErr == nil {
		coverageErr = collector.runStep("coverage", func() error {
			return runCoverageGate(ctx, runner, repoRoot, env, coverageFile, coverageThreshold, stdout)
		})
	} else {
		// A failed run leaves a partial coverage profile, and gating it would
		// add a second failure that is only the first one's shadow.
		collector.recordStepNotRun("coverage", "unit tests failed")
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
		if err := runStress(ctx, runner, opts.RepoRoot, env, collector, stdout, stderr); err != nil {
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
// buckets. A fast-suite test failure does not stop e2e-full: it says nothing
// about the buckets, and the night's record has to show every failure. A
// preflight failure does, because the account or the fixtures are unusable;
// the buckets are then recorded as not run.
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
	var fastErr error
	if plan.runE2E {
		fastErr = runE2E(ctx, runner, opts.RepoRoot, e2eEnv, collector, stdout, stderr, opts.ClassifyLiveQuirks)
		if fastErr != nil && (!plan.runE2EFull || errors.Is(fastErr, errE2EPreflight)) {
			if plan.runE2EFull {
				collector.recordBucketsNotRun(fullE2EBuckets(), "fast e2e preflight failed")
			}

			return fastErr
		}
	}
	if !plan.runE2EFull {
		return nil
	}

	fullErr := runE2EFull(ctx, runner, opts.RepoRoot, e2eEnv, e2eLogDir, collector, stdout, stderr, opts.ClassifyLiveQuirks)

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

func runLint(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> golangci-lint\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := assertLintVersionMatchesCI(ctx, runner, repoRoot, env); err != nil {
		return err
	}
	args := lintCommandArgs(runtime.GOMAXPROCS(0))
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, lintBinaryName, args...); err != nil {
		return fmt.Errorf("lint: %w", err)
	}

	return nil
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
	// with at least unitTestReferenceCores cores.
	unitTestBaseTimeout = 20 * time.Minute

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
// go test, the linter) for the cores the run may use. A deadline that fits a
// CI runner fails a healthy run on a 2-core host, so a host below the
// reference gets the step's small-host budget divided by its cores. A host at
// the reference or above keeps the base deadline: more cores do not shorten
// the deadline a real hang is measured against, and the CI job timeouts are
// set above it.
func deadlineForCores(cores int, base, smallHostCoreBudget time.Duration) time.Duration {
	if cores < 1 {
		cores = 1
	}
	if cores >= unitTestReferenceCores {
		return base
	}

	return smallHostCoreBudget / time.Duration(cores)
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

// verifyStep is one named step of a verify profile, recorded in the summary
// under its name.
type verifyStep struct {
	name string
	run  func(context.Context, commandRunner, string, []string, io.Writer, io.Writer) error
}

// lintSteps are the lint profile's steps, which every profile that lints runs:
// golangci-lint over the Go code, then actionlint over the workflow files.
func lintSteps() []verifyStep {
	return []verifyStep{{"lint", runLint}, {"workflow lint", runWorkflowLint}}
}

// repoRuleSteps are the repository's static rules: the output-boundary,
// test-convention, README, spec-ownership, layering, backlog-id, mutex-doc
// and traceability checks. They take seconds, so every profile that builds
// the tree runs them.
func repoRuleSteps() []verifyStep {
	return []verifyStep{
		{"output boundaries", runOutputBoundaries},
		{"test conventions", runTestConventions},
		{"README status", runReadmeStatus},
		{"spec governs", runSpecGoverns},
		{"layering", runLayering},
		{"retired backlog ids", runRetiredBacklogIDs},
		{"mutex ownership docs", runMutexDocs},
		{"requirement traceability", runRequirementTraceability},
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

func runIntegration(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return collector.runStep("integration", func() error {
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
