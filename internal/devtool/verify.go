package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type VerifyProfile string

const (
	defaultCoveragePattern           = "onedrive-go-cover.*"
	fullE2ECompileStepName           = "e2e-full compile"
	fullE2ECompileArtifactName       = "onedrive-go-e2e-full.test"
	authE2EPreflightPattern          = "^TestE2E_AuthPreflight_Fast$"
	fastE2EPreflightPattern          = "^TestE2E_FixturePreflight_Fast$"
	fullE2EPreflightPattern          = "^TestE2E_FixturePreflight_Full$"
	fullE2EFixturePreflight          = "TestE2E_FixturePreflight_Full"
	fullE2EPackageTimeout            = "60m"
	fastE2EPackageTimeout            = "10m"
	stressMultisyncTimeout           = "20m"
	stressSyncConcurrencyTimeout     = "40m"
	stressSyncTimeout                = "40m"
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
	UnitTimeout        time.Duration
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
	Stdout         io.Writer
	Stderr         io.Writer
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
	Error      string `json:"error,omitempty"`
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
	if err := runPRLaneVerification(ctx, runner, opts.RepoRoot, env, collector, stdout, stderr, plan, shard); err != nil {
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
		if err := remove(coverageFile); err != nil && !os.IsNotExist(err) {
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

	publicSteps := []verifyStep{{"format", runFormat}, {"lint", runLint}}
	publicSteps = append(publicSteps, repoRuleSteps()...)
	publicSteps = append(publicSteps, verifyStep{"build", runBuild})
	if err := runVerifySteps(ctx, runner, repoRoot, env, collector, stdout, stderr, publicSteps); err != nil {
		return err
	}

	if err := collector.runStep("unit tests", func() error {
		return runUnitTests(ctx, runner, repoRoot, env, coverageFile, unitTimeout, stdout, stderr)
	}); err != nil {
		return err
	}
	if err := collector.runStep("coverage", func() error {
		return runCoverageGate(ctx, runner, repoRoot, env, coverageFile, coverageThreshold, stdout)
	}); err != nil {
		return err
	}
	if err := collector.runStep(fullE2ECompileStepName, func() error {
		return runE2EFullCompileCheck(ctx, runner, repoRoot, env, stdout, stderr)
	}); err != nil {
		return err
	}

	return nil
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

	if plan.runE2E {
		if err := runE2E(ctx, runner, opts.RepoRoot, e2eEnv, collector, stdout, stderr, opts.ClassifyLiveQuirks); err != nil {
			return err
		}
	}

	if plan.runE2EFull {
		if err := runE2EFull(
			ctx,
			runner,
			opts.RepoRoot,
			e2eEnv,
			effectiveE2ELogDir,
			collector,
			stdout,
			stderr,
			opts.ClassifyLiveQuirks,
		); err != nil {
			return err
		}
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

func resolvedE2ELogDir(explicit string, plan verifyPlan) string {
	if explicit != "" {
		return explicit
	}
	if plan.runE2E || plan.runE2EFull {
		return filepath.Join(os.TempDir(), "e2e-debug-logs")
	}

	return ""
}

func runFormat(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> gofumpt\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "gofumpt", "-w", "."); err != nil {
		return fmt.Errorf("format gofumpt: %w", err)
	}

	if err := writeStatus(stdout, "==> goimports\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"goimports",
		"-local",
		"github.com/tonimelisma/onedrive-go",
		"-w",
		".",
	); err != nil {
		return fmt.Errorf("format goimports: %w", err)
	}

	return nil
}

func runLint(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> golangci-lint\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := assertLintVersionMatchesCI(ctx, runner, repoRoot, env); err != nil {
		return err
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "golangci-lint", "run", "--allow-parallel-runners"); err != nil {
		return fmt.Errorf("lint: %w", err)
	}

	return nil
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
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"go",
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
	); err != nil {
		return fmt.Errorf("unit tests: %w", err)
	}

	return nil
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
)

// unitTestTimeoutForCores scales the full tier's go test deadline to the cores
// the run may use. The race tier is CPU-bound, so a host with fewer cores than
// the reference needs proportionally longer: a fixed deadline that fits a CI
// runner fails a healthy run on a 2-core host. The multiplier is
// ceil(reference / cores), which is never below one, so more cores than the
// reference do not shorten the deadline a real hang is measured against.
func unitTestTimeoutForCores(cores int) time.Duration {
	if cores < 1 {
		cores = 1
	}

	// Integer ceiling of reference/cores.
	multiplier := (unitTestReferenceCores + cores - 1) / cores

	return unitTestBaseTimeout * time.Duration(multiplier)
}

// resolveUnitTestTimeout returns the explicit deadline (`--unit-timeout`) when
// one was given, else the deadline scaled to cores.
func resolveUnitTestTimeout(explicit time.Duration, cores int) time.Duration {
	if explicit > 0 {
		return explicit
	}

	return unitTestTimeoutForCores(cores)
}

// verifyStep is one named step of a verify profile, recorded in the summary
// under its name.
type verifyStep struct {
	name string
	run  func(context.Context, commandRunner, string, []string, io.Writer, io.Writer) error
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
		if err := runner.Run(
			ctx,
			repoRoot,
			env,
			stdout,
			stderr,
			"go",
			"test",
			"-tags=integration",
			"-race",
			"-v",
			"-timeout=5m",
			"./internal/graph/...",
		); err != nil {
			return fmt.Errorf("integration tests: %w", err)
		}

		return nil
	})
}

func writeStatus(w io.Writer, text string) error {
	_, err := io.WriteString(w, text)
	if err != nil {
		return fmt.Errorf("write status output: %w", err)
	}

	return nil
}
