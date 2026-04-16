package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type VerifyProfile string

const (
	defaultCoverageThreshold         = 75.5
	defaultCoveragePattern           = "onedrive-go-cover.*"
	authE2EPreflightPattern          = "^TestE2E_AuthPreflight_Fast$"
	fastE2EPreflightPattern          = "^TestE2E_FixturePreflight_Fast$"
	fullE2EPreflightPattern          = "^TestE2E_FixturePreflight_Full$"
	fullE2EFixturePreflight          = "TestE2E_FixturePreflight_Full"
	fullE2EPackageTimeout            = "60m"
	fastE2EPackageTimeout            = "10m"
	stressWatchOrderingTimeout       = "20m"
	stressMultisyncTimeout           = "20m"
	stressSyncTimeout                = "30m"
	stressSlowTestLimit              = 5
	authPreflightIncidentID          = "LI-20260405-06"
	fastDownloadIncidentID           = "LI-20260405-04"
	fastDownloadTestName             = "TestE2E_Sync_DownloadOnly"
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
	VerifyE2E         VerifyProfile = "e2e"
	VerifyE2EFull     VerifyProfile = "e2e-full"
	VerifyIntegration VerifyProfile = "integration"
	VerifyStress      VerifyProfile = "stress"
)

type VerifyOptions struct {
	RepoRoot           string
	Profile            VerifyProfile
	CoverageThreshold  float64
	CoverageFile       string
	E2ELogDir          string
	SummaryJSONPath    string
	ClassifyLiveQuirks bool
	Stdout             io.Writer
	Stderr             io.Writer
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
	runPublicChecks bool
	runE2E          bool
	runE2EFull      bool
	runIntegration  bool
	runStress       bool
}

func RunVerify(ctx context.Context, runner commandRunner, opts *VerifyOptions) (runErr error) {
	if opts == nil {
		return fmt.Errorf("verify options are required")
	}

	plan, err := resolveVerifyPlan(opts.Profile)
	if err != nil {
		return err
	}

	stdout, stderr := resolveVerifyWriters(opts)
	effectiveE2ELogDir := resolvedE2ELogDir(opts.E2ELogDir, plan)
	collector := newVerifySummaryCollector(opts.Profile, stdout, opts.SummaryJSONPath, effectiveE2ELogDir)
	defer func() {
		if finalizeErr := collector.finalize(runErr); finalizeErr != nil {
			if runErr == nil {
				runErr = finalizeErr
			} else {
				runErr = errors.Join(runErr, finalizeErr)
			}
		}
	}()
	coverageThreshold := opts.CoverageThreshold
	if coverageThreshold == 0 {
		coverageThreshold = defaultCoverageThreshold
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

	env := os.Environ()
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
		plan,
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
	default:
		return verifyPlan{}, fmt.Errorf("usage: devtool verify [default|public|e2e|e2e-full|integration|stress]")
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
	plan verifyPlan,
) error {
	if !plan.runPublicChecks {
		return nil
	}

	publicSteps := []func(context.Context, commandRunner, string, []string, io.Writer, io.Writer) error{
		runFormat,
		runLint,
		runBuild,
	}
	publicStepNames := []string{"format", "lint", "build"}
	for i, step := range publicSteps {
		if err := collector.runStep(publicStepNames[i], func() error {
			return step(ctx, runner, repoRoot, env, stdout, stderr)
		}); err != nil {
			return err
		}
	}

	if err := collector.runStep("unit tests", func() error {
		return runUnitTests(ctx, runner, repoRoot, env, coverageFile, stdout, stderr)
	}); err != nil {
		return err
	}
	if err := collector.runStep("coverage", func() error {
		return runCoverageGate(ctx, runner, repoRoot, env, coverageFile, coverageThreshold, stdout)
	}); err != nil {
		return err
	}
	if err := collector.runStep("repo consistency", func() error {
		return runRepoConsistencyChecks(repoRoot)
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
		if err := runE2EFull(ctx, runner, opts.RepoRoot, e2eEnv, effectiveE2ELogDir, collector, stdout, stderr); err != nil {
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
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "golangci-lint", "run", "--allow-parallel-runners"); err != nil {
		return fmt.Errorf("lint: %w", err)
	}

	return nil
}

func runBuild(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> go build\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", "build", "./..."); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	return nil
}

func runUnitTests(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	coverageFile string,
	stdout, stderr io.Writer,
) error {
	if err := writeStatus(stdout, "==> go test -race -coverprofile\n"); err != nil {
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
		"-coverprofile="+coverageFile,
		"./...",
	); err != nil {
		return fmt.Errorf("unit tests: %w", err)
	}

	return nil
}

func runCoverageGate(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	coverageFile string,
	coverageThreshold float64,
	stdout io.Writer,
) error {
	if err := writeStatus(stdout, "==> coverage\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	report, err := runner.Output(ctx, repoRoot, env, "go", "tool", "cover", "-func="+coverageFile)
	if err != nil {
		return fmt.Errorf("coverage report: %w", err)
	}

	if writeErr := writeStatus(stdout, string(report)); writeErr != nil {
		return fmt.Errorf("write coverage report: %w", writeErr)
	}

	coverageTotal, err := parseCoverageTotal(string(report))
	if err != nil {
		return err
	}

	if coverageTotal < coverageThreshold {
		return fmt.Errorf("coverage gate failed: %.1f%% < %.1f%%", coverageTotal, coverageThreshold)
	}

	return nil
}

func parseCoverageTotal(report string) (float64, error) {
	for _, line := range strings.Split(report, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "total:" {
			continue
		}

		value := strings.TrimSuffix(fields[2], "%")
		coverageTotal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("parse coverage total %q: %w", value, err)
		}

		return coverageTotal, nil
	}

	return 0, fmt.Errorf("parse coverage total: total line not found")
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
