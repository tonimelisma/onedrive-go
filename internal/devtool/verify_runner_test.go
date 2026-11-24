package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Command positions in the public verify profile. Named so that inserting a
// step is a one-line change here rather than a silent renumbering that shifts
// every downstream assertion onto the wrong command.
const (
	cmdFormatGofumpt      = 0
	cmdFormatGoimports    = 1
	cmdLint               = 2
	cmdBuild              = cmdLint + lintPassCount
	cmdBuildFirstPlatform = cmdBuild + 1
)

// lintPassCount is the number of golangci-lint passes the lint step runs.
const lintPassCount = 3

// The cross-platform builds occupy supportedPlatformCount slots starting at
// cmdBuildFirstPlatform, so everything after them is expressed relative to
// that run rather than hardcoded.
const (
	cmdUnitTests      = cmdBuildFirstPlatform + supportedPlatformCount
	cmdE2EFullCompile = cmdUnitTests + 1
	cmdTaggedVetE2E   = cmdE2EFullCompile + 1
	cmdTaggedVetInteg = cmdTaggedVetE2E + 1
	cmdTaggedVetRace  = cmdTaggedVetInteg + 1

	verifyPublicCommandCount = cmdTaggedVetRace + 1
)

// Command positions in the fast e2e verify profile: the two preflights, then
// the suite.
const (
	cmdE2EAuthPreflight = 0
	cmdE2EFastPreflight = 1
	cmdE2ESuite         = 2

	verifyE2ECommandCount = cmdE2ESuite + 1
)

// Validates: R-6.10.1
func TestRunVerifyPublicRunsTheUnitTierWithCoverage(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:     repoRoot,
		Profile:      VerifyPublic,
		CoverageFile: filepath.Join(repoRoot, "cover.out"),
		UnitTimeout:  25 * time.Minute,
		Stdout:       stdout,
		Stderr:       stderr,
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, verifyPublicCommandCount)
	assert.Equal(t, "gofumpt", runner.runCommands[cmdFormatGofumpt].name)
	assert.Equal(t, []string{"-w", "."}, runner.runCommands[cmdFormatGofumpt].args)
	assert.Equal(t, "goimports", runner.runCommands[cmdFormatGoimports].name)
	assert.Equal(t, "golangci-lint", runner.runCommands[cmdLint].name)
	assert.Equal(t, hostLintCommandArgs(), runner.runCommands[cmdLint].args)
	assert.Equal(t, "go", runner.runCommands[cmdBuild].name)
	assert.Equal(t, []string{"build", "./..."}, runner.runCommands[cmdBuild].args)

	for i, goos := range supportedPlatforms() {
		command := runner.runCommands[cmdBuildFirstPlatform+i]
		assert.Equal(t, []string{"build", "./..."}, command.args)
		assertCommandHasEnvVar(t, command, "GOOS="+goos)
	}

	// --unit-timeout reaches go test as given, whatever the host's core count,
	// and coverage counts a statement whichever package's tests execute it.
	assert.Equal(t,
		[]string{
			"test", "-race", "-timeout=25m0s", "-coverpkg=./...",
			"-coverprofile=" + filepath.Join(repoRoot, "cover.out"), "./...",
		},
		runner.runCommands[cmdUnitTests].args,
	)
	assert.Equal(t, "test", runner.runCommands[cmdE2EFullCompile].args[0])
	assert.Equal(t, "-c", runner.runCommands[cmdE2EFullCompile].args[1])
	assert.Equal(t, "-race", runner.runCommands[cmdE2EFullCompile].args[2])
	assert.Equal(t, "-tags=e2e e2e_full", runner.runCommands[cmdE2EFullCompile].args[3])
	assert.Equal(t, "-o", runner.runCommands[cmdE2EFullCompile].args[4])
	assert.Contains(t,
		filepath.Base(runner.runCommands[cmdE2EFullCompile].args[5]),
		strings.TrimSuffix(fullE2ECompileArtifactName, ".test"),
	)
	assert.Equal(t, "./e2e", runner.runCommands[cmdE2EFullCompile].args[6])
	require.Len(t, runner.outputCommands, 1)
	assert.Contains(t, stdout.String(), "==> coverage")
}

// Validates: R-6.10.1
func TestRunVerifyE2ERunsThePreflightsThenTheFastSuite(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:  t.TempDir(),
		Profile:   VerifyE2E,
		E2ELogDir: t.TempDir(),
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, verifyE2ECommandCount)
	assert.Equal(t,
		[]string{"test", "-tags=e2e", "-run=" + authE2EPreflightPattern, "-count=1", "-v", "-timeout=10m", "./e2e/..."},
		runner.runCommands[cmdE2EAuthPreflight].args,
	)
	assert.Equal(t,
		[]string{"test", "-tags=e2e", "-run=" + fastE2EPreflightPattern, "-count=1", "-v", "-timeout=10m", "./e2e/..."},
		runner.runCommands[cmdE2EFastPreflight].args,
	)
	assert.Equal(t,
		[]string{"test", "-tags=e2e", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."},
		runner.runCommands[cmdE2ESuite].args,
	)
	assertCommandHasEnvVar(t, runner.runCommands[cmdE2EAuthPreflight], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[cmdE2EAuthPreflight], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[cmdE2EAuthPreflight])
	assertCommandHasEnvVar(t, runner.runCommands[cmdE2EFastPreflight], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[cmdE2EFastPreflight], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[cmdE2EFastPreflight], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[cmdE2ESuite], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[cmdE2ESuite], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[cmdE2ESuite], e2eRunFastFixturePreflightEnvVar+"=1")
}

// Validates: R-6.10.22
//
// The 20-minute deadline of the full race tier was set on the 3-core macOS CI
// runner. A host with fewer cores is a developer's machine: internal/sync took
// 63 minutes there on 2 shared cores, and a deadline that fires on a healthy
// run makes the tier unusable. Below the reference the deadline is 180
// core-minutes divided by the cores. At the reference and above it stays, so
// neither CI runner's deadline moves past its job timeout.
func TestUnitTestTimeoutForCoresGivesSmallHostsACoreBudget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cores int
		want  time.Duration
	}{
		{cores: 0, want: 180 * time.Minute},
		{cores: 1, want: 180 * time.Minute},
		{cores: 2, want: 90 * time.Minute},
		{cores: 3, want: 30 * time.Minute},
		{cores: 4, want: 30 * time.Minute},
		{cores: 8, want: 30 * time.Minute},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, unitTestTimeoutForCores(tc.cores), "cores %d", tc.cores)
	}
}

// Validates: R-6.10.22
func TestResolveUnitTestTimeoutPrefersTheExplicitValue(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 45*time.Minute, resolveUnitTestTimeout(45*time.Minute, 2))
	assert.Equal(t, 90*time.Minute, resolveUnitTestTimeout(0, 2))
	assert.Equal(t, 30*time.Minute, resolveUnitTestTimeout(0, 8))
}

func TestRunE2EFullCompileCheck_UsesUniqueArtifactPath(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}

	err := runE2EFullCompileCheck(
		context.Background(),
		runner,
		t.TempDir(),
		nil,
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	require.NoError(t, err)
	// compile, then one tagged vet per build-tagged source set
	require.Len(t, runner.runCommands, 1+taggedVetTargetCount)

	args := runner.runCommands[0].args
	require.Len(t, args, 7)
	assert.Equal(t, "-o", args[4])
	assert.NotEqual(t, filepath.Join(os.TempDir(), fullE2ECompileArtifactName), args[5])
	assert.Contains(t, filepath.Base(args[5]), strings.TrimSuffix(fullE2ECompileArtifactName, ".test"))
}

// Validates: R-6.10.1
func TestRunVerifyPublicRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:     repoRoot,
		Profile:      VerifyPublic,
		CoverageFile: filepath.Join(repoRoot, "cover.out"),
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
	})
	require.NoError(t, err)

	// Assert the whole sequence rather than one index, so a step added in the
	// middle shows up as a sequence change instead of silently shifting an
	// index-based assertion onto the wrong command.
	gotSequence := make([]string, 0, len(runner.runCommands))
	for _, command := range runner.runCommands {
		goos := ""
		for _, entry := range command.env {
			if strings.HasPrefix(entry, "GOOS=") {
				goos = " " + entry
			}
		}
		gotSequence = append(gotSequence, command.name+" "+strings.Join(command.args[:2], " ")+goos)
	}

	assert.Equal(t, []string{
		"gofumpt -w .",
		"goimports -local github.com/tonimelisma/onedrive-go",
		"golangci-lint run --allow-parallel-runners GOOS=linux",
		"golangci-lint run --allow-parallel-runners GOOS=darwin",
		"golangci-lint run --allow-parallel-runners GOOS=freebsd",
		"go build ./...",
		"go build ./... GOOS=linux",
		"go build ./... GOOS=darwin",
		"go build ./... GOOS=freebsd",
		"go test -race",
		"go test -c",
		"go vet -tags=e2e e2e_full",
		"go vet -tags=integration",
		"go vet -tags=race",
	}, gotSequence)

	e2eCompile := runner.runCommands[cmdE2EFullCompile]
	assert.Equal(t, "-race", e2eCompile.args[2])
	assert.Equal(t, "-tags=e2e e2e_full", e2eCompile.args[3])
	assert.Equal(t, "-o", e2eCompile.args[4])
	assert.Contains(t, filepath.Base(e2eCompile.args[5]), strings.TrimSuffix(fullE2ECompileArtifactName, ".test"))
	assert.Equal(t, "./e2e", e2eCompile.args[6])
}

func TestRunVerifyE2EFullRunsPreflightsBeforeSuites(t *testing.T) {
	t.Parallel()

	repoRoot := newFakeE2ERepo(t)
	runner := &fakeRunner{}
	stdout := &bytes.Buffer{}
	logDir := filepath.Join(repoRoot, "e2e-logs")

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:  repoRoot,
		Profile:   VerifyE2EFull,
		E2ELogDir: logDir,
		Stdout:    stdout,
		Stderr:    &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 7)
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + authE2EPreflightPattern, "-count=1", "-v", "-timeout=10m", "./e2e/..."}, runner.runCommands[0].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + fastE2EPreflightPattern, "-count=1", "-v", "-timeout=10m", "./e2e/..."}, runner.runCommands[1].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[2].args)
	assert.Equal(t, []string{"test", "-tags=e2e e2e_full", "-run=" + fullE2EPreflightPattern, "-count=1", "-v", "-timeout=10m", "./e2e/..."}, runner.runCommands[3].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fakeFullE2EBucket(t, fullE2EParallelMiscBucketName)), runner.runCommands[4].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fakeFullE2EBucket(t, fullE2ESerialSyncBucketName)), runner.runCommands[5].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fakeFullE2EBucket(t, fullE2ESerialWatchSharedBucketName)), runner.runCommands[6].args)
	assertCommandHasEnvVar(t, runner.runCommands[0], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[0], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[0])
	assertCommandHasEnvVar(t, runner.runCommands[1], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[1], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[1], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[2], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[3], "E2E_LOG_DIR="+logDir)
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[3])
	assertCommandHasEnvVar(t, runner.runCommands[4], "E2E_LOG_DIR="+logDir)
	assertCommandHasEnvVar(t, runner.runCommands[4], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[5], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[6], e2eSkipSuiteScrubEnvVar+"=1")
	assert.Contains(t, stdout.String(), "verify summary")
	assert.Contains(t, stdout.String(), "full-parallel-misc")
	assert.Contains(t, stdout.String(), "classified reruns: none")
}

func TestRunVerifyE2EFullWithoutExplicitLogDirNormalizesDefaultLogDir(t *testing.T) {
	repoRoot := newFakeE2ERepo(t)
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	logDir := filepath.Join(os.TempDir(), "e2e-debug-logs")
	require.NoError(t, os.MkdirAll(logDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, e2eTimingEventsFileName), []byte("events"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, e2eTimingSummaryFileName), []byte("summary"), 0o600))

	runner := &fakeRunner{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyE2EFull,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 7)
	for i := range runner.runCommands {
		assertCommandHasEnvVar(t, runner.runCommands[i], "E2E_LOG_DIR="+logDir)
	}
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[0])
	assertCommandHasEnvVar(t, runner.runCommands[0], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[1], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[1], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[2], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[4], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[5], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[6], e2eSkipSuiteScrubEnvVar+"=1")

	_, eventsErr := os.Stat(filepath.Join(logDir, e2eTimingEventsFileName))
	require.ErrorIs(t, eventsErr, os.ErrNotExist)
	_, summaryErr := os.Stat(filepath.Join(logDir, e2eTimingSummaryFileName))
	require.ErrorIs(t, summaryErr, os.ErrNotExist)
}

func TestRunVerifyE2EWithoutExplicitLogDirNormalizesDefaultLogDir(t *testing.T) {
	repoRoot := t.TempDir()
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	logDir := filepath.Join(os.TempDir(), "e2e-debug-logs")
	require.NoError(t, os.MkdirAll(logDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, e2eTimingEventsFileName), []byte("events"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, e2eTimingSummaryFileName), []byte("summary"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, e2eQuirkEventsFileName), []byte("events"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, e2eQuirkSummaryFileName), []byte(`{"events":[]}`), 0o600))

	runner := &fakeRunner{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyE2E,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 3)
	for i := range runner.runCommands {
		assertCommandHasEnvVar(t, runner.runCommands[i], "E2E_LOG_DIR="+logDir)
	}
	assertCommandHasEnvVar(t, runner.runCommands[0], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[0])
	assertCommandHasEnvVar(t, runner.runCommands[1], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[1], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[2], e2eSkipSuiteScrubEnvVar+"=1")

	_, eventsErr := os.Stat(filepath.Join(logDir, e2eTimingEventsFileName))
	require.ErrorIs(t, eventsErr, os.ErrNotExist)
	_, summaryErr := os.Stat(filepath.Join(logDir, e2eTimingSummaryFileName))
	require.ErrorIs(t, summaryErr, os.ErrNotExist)
	_, quirkEventsErr := os.Stat(filepath.Join(logDir, e2eQuirkEventsFileName))
	require.ErrorIs(t, quirkEventsErr, os.ErrNotExist)
	_, quirkSummaryErr := os.Stat(filepath.Join(logDir, e2eQuirkSummaryFileName))
	require.ErrorIs(t, quirkSummaryErr, os.ErrNotExist)
}

func TestRunVerifyE2EFullClassifiesKnownAuthPreflightQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	authKey := goCommandKey(authE2EPreflightArgs())
	runner := &fakeRunner{
		runErrSequenceByKey: map[string][]error{authKey: {assert.AnError}},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != authKey {
				return passingGoTestOutput(args)
			}

			return rawFailLines(authPreflightTestName)
		},
	}

	summary, stdout, err := runE2EFullForTest(t, runner, true)
	require.NoError(t, err)

	// The rerun, then the fixture preflight, the fast suite, the full
	// preflight and the three buckets; every command streams through Run.
	require.Len(t, runner.runCommands, 2+e2eFullCommandsBeforeBuckets-1+len(fullE2EBucketShapes()))
	assert.Empty(t, runner.combinedCommands)
	assert.Equal(t, runner.runCommands[0].args, runner.runCommands[1].args)
	assert.Contains(t, stdout, "LI-20260405-06")
	assert.Contains(t, stdout, "classified reruns:")
	assertVerifySummaryHasStep(t, summary, authPreflightStepName)
}

func TestRunVerifyE2EFullClassifiesKnownFastSuiteQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	fastKey := goCommandKey(fastE2EJSONArgs())
	runner := &fakeRunner{
		runErrSequenceByKey: map[string][]error{fastKey: {assert.AnError}},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != fastKey {
				return passingGoTestOutput(args)
			}

			return jsonFailLines(fastDownloadTestName)
		},
	}

	summary, stdout, err := runE2EFullForTest(t, runner, true)
	require.NoError(t, err)

	require.Len(t, summary.ClassifiedReruns, 1)
	assert.Equal(t, string(fastE2EStepName), summary.ClassifiedReruns[0].Phase, "a rerun is recorded under its step's name")
	assert.Empty(t, runner.combinedCommands, "the classified fast suite streams through Run")
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+1+len(fullE2EBucketShapes()))
	assert.Equal(t, []string{"test", "-json", "-tags=e2e", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[2].args)
	assert.Equal(t,
		[]string{"test", "-tags=e2e", "-run=^TestE2E_Sync_DownloadOnly$", "-count=1", "-v", "-timeout=10m", "./e2e/..."},
		runner.runCommands[3].args)
	assert.Equal(t, fullE2EBucketJSONCommandArgs(fakeFullE2EBucket(t, fullE2ESerialWatchSharedBucketName)), runner.runCommands[len(runner.runCommands)-1].args)
	assert.Contains(t, stdout, "LI-20260405-04")
	assert.Contains(t, stdout, "classified reruns:")
}

// Validates: R-6.10.26
//
// An unknown fast-suite failure stays red, and no longer ends the profile: a
// fast test failing says nothing about the full buckets, which still run.
func TestRunVerifyE2EFullDoesNotMaskUnknownFastSuiteFailure(t *testing.T) {
	t.Parallel()

	fastKey := goCommandKey(fastE2EJSONArgs())
	runner := &fakeRunner{
		runErrByKey: map[string]error{fastKey: assert.AnError},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != fastKey {
				return passingGoTestOutput(args)
			}

			return jsonFailLines("TestE2E_Sync_DeletePropagation")
		},
	}

	summary, _, err := runE2EFullForTest(t, runner, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fast e2e")
	assert.Empty(t, runner.combinedCommands)
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(fullE2EBucketShapes()))
	assert.Equal(t,
		[]string{verifySummaryStatusPass, verifySummaryStatusPass, verifySummaryStatusPass},
		bucketStatuses(summary))
	fast := requireVerifySummaryStep(t, summary, fastE2EStepName)
	assert.Equal(t, verifySummaryStatusFail, fast.Status)
	assert.Equal(t, []string{"TestE2E_Sync_DeletePropagation"}, fast.FailedTests)
}

func TestRunVerifyE2EFullClassifiesKnownWatchSharedQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	watchKey := goCommandKey(fullE2EBucketJSONCommandArgs(fakeFullE2EBucket(t, fullE2ESerialWatchSharedBucketName)))
	runner := &fakeRunner{
		runErrSequenceByKey: map[string][]error{watchKey: {assert.AnError}},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != watchKey {
				return passingGoTestOutput(args)
			}

			return jsonFailLines(websocketRestartTestName)
		},
	}

	summary, stdout, err := runE2EFullForTest(t, runner, true)
	require.NoError(t, err)

	assert.Empty(t, runner.combinedCommands, "the classified bucket streams through Run")
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(fullE2EBucketShapes())+1)
	assert.Equal(t, []string{
		"test",
		"-tags=e2e e2e_full",
		"-race",
		"-run=^TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart$",
		"-count=1",
		"-v",
		"-parallel",
		"1",
		"-timeout=10m",
		"./e2e/...",
	}, runner.runCommands[len(runner.runCommands)-1].args)
	assert.Contains(t, stdout, "LI-20260405-03")
	assert.Contains(t, stdout, "classified reruns:")
	assertVerifyBucketSummary(t, summary, fullE2ESerialWatchSharedBucketName, verifySummaryStatusPass)
}

func TestRunVerifyE2EFullDoesNotMaskUnknownWatchSharedFailure(t *testing.T) {
	t.Parallel()

	watchKey := goCommandKey(fullE2EBucketJSONCommandArgs(fakeFullE2EBucket(t, fullE2ESerialWatchSharedBucketName)))
	runner := &fakeRunner{
		runErrByKey: map[string]error{watchKey: assert.AnError},
		runStdoutFunc: func(args []string) string {
			if goCommandKey(args) != watchKey {
				return passingGoTestOutput(args)
			}

			return jsonFailLines("TestE2E_Orchestrator_WatchSimultaneous")
		},
	}

	summary, _, err := runE2EFullForTest(t, runner, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "full e2e")
	assert.Empty(t, runner.combinedCommands)
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(fullE2EBucketShapes()), "an unknown failure is not rerun")
	require.Len(t, summary.E2EFullBuckets, len(fullE2EBucketShapes()))
	assert.Equal(t, []string{"TestE2E_Orchestrator_WatchSimultaneous"}, summary.E2EFullBuckets[2].FailedTests)
}

func TestRunVerifyE2EStopsAfterFastPreflightFailure(t *testing.T) {
	t.Parallel()

	assertVerifyStopsAfterPreflightFailure(t, VerifyE2E, goCommandKey(fastE2EPreflightArgs()), 2, "preflight")
}

func TestRunVerifyE2EStopsAfterAuthPreflightFailure(t *testing.T) {
	t.Parallel()

	assertVerifyStopsAfterPreflightFailure(t, VerifyE2E, goCommandKey(authE2EPreflightArgs()), 1, "auth preflight")
}

func TestRunVerifyE2EFullStopsAfterFullPreflightFailure(t *testing.T) {
	t.Parallel()

	assertVerifyStopsAfterPreflightFailure(t, VerifyE2EFull, goCommandKey(fullE2EPreflightArgs()), 4, "preflight")
}

func TestRunVerifyWritesSummaryJSONOnSuccess(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyPublic,
		CoverageFile:    filepath.Join(repoRoot, "cover.out"),
		SummaryJSONPath: summaryPath,
		Stdout:          stdout,
		Stderr:          &bytes.Buffer{},
	})
	require.NoError(t, err)

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, string(VerifyPublic), summary.Profile)
	assert.Equal(t, verifySummaryStatusPass, summary.Status)
	assert.GreaterOrEqual(t, summary.TotalDurationMS, int64(0))
	assertVerifySummaryHasStep(t, summary, "format")
	assertVerifySummaryHasStep(t, summary, fullE2ECompileStepName)
	assert.Empty(t, summary.ClassifiedReruns)
	assert.Contains(t, stdout.String(), "verify summary")
	assert.Contains(t, stdout.String(), "classified reruns: none")
}

func TestVerifySummaryCollectorFinalizeReadsQuirkSummary(t *testing.T) {
	t.Parallel()

	logDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(logDir, e2eQuirkSummaryFileName),
		[]byte("{\"events\":[{\"phase\":\"cli_command\"},{\"phase\":\"auth_preflight\"}]}\n"),
		0o600,
	))

	stdout := &bytes.Buffer{}
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	collector := newVerifySummaryCollector(VerifyE2E, stdout, &bytes.Buffer{}, summaryPath, logDir)

	require.NoError(t, collector.finalize(nil))

	assert.Contains(t, stdout.String(), "quirk events: 2")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, 2, summary.QuirkEventCount)
}

// Validates: R-6.10.26, R-6.10.36
func TestRunVerifyWritesSummaryJSONOnFailure(t *testing.T) {
	t.Parallel()

	repoRoot := newFakeE2ERepo(t)
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	bucketArgs := fullE2EBucketCommandArgs(fakeFullE2EBucket(t, fullE2ESerialSyncBucketName))
	runner := &fakeRunner{
		runErrByKey: map[string]error{
			"go " + strings.Join(bucketArgs, " "): assert.AnError,
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyE2EFull,
		SummaryJSONPath: summaryPath,
		E2ELogDir:       filepath.Join(repoRoot, "e2e-logs"),
		Stdout:          stdout,
		Stderr:          &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "full e2e")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, string(VerifyE2EFull), summary.Profile)
	assert.Equal(t, verifySummaryStatusFail, summary.Status)
	assertVerifySummaryHasStep(t, summary, fastE2EStepName)
	assertVerifyBucketSummary(t, summary, fullE2EParallelMiscBucketName, verifySummaryStatusPass)
	assertVerifyBucketSummary(t, summary, fullE2ESerialSyncBucketName, verifySummaryStatusFail)
	// The bucket after the failing one still ran.
	assertVerifyBucketSummary(t, summary, fullE2ESerialWatchSharedBucketName, verifySummaryStatusPass)
	assert.Contains(t, stdout.String(), "verify summary")
	assert.Contains(t, stdout.String(), fullE2ESerialSyncBucketName)

	// The failed bucket kept its output and the summary names the file; the
	// buckets that passed left none (R-6.10.36).
	bucketLog := filepath.Join(repoRoot, defaultVerifyLogDirName,
		string(VerifyE2EFull)+"-"+string(fullE2ESerialSyncBucketName)+".log")
	for i := range summary.E2EFullBuckets {
		bucket := summary.E2EFullBuckets[i]
		if bucket.Name == string(fullE2ESerialSyncBucketName) {
			assert.Equal(t, bucketLog, bucket.LogPath)
			continue
		}
		assert.Empty(t, bucket.LogPath, bucket.Name)
	}
	logs, err := os.ReadDir(filepath.Join(repoRoot, defaultVerifyLogDirName))
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, filepath.Base(bucketLog), logs[0].Name())
}

// Validates: R-6.10.22
//
// The usage line names the verifier's profiles and nothing else: every name
// on it resolves to a plan, and every VerifyProfile constant the package
// declares is on it. A profile added without its name on the line, or a name
// left on the line after its profile went, fails here.
func TestVerifyProfileUsageNamesExactlyTheProfiles(t *testing.T) {
	t.Parallel()

	usage := strings.Split(VerifyProfileUsage, "|")
	for _, name := range usage {
		_, err := resolveVerifyPlan(VerifyProfile(name))
		require.NoError(t, err, "the usage line names %s, which resolves to no plan", name)
	}

	dir := filepath.Join(repoRootFromDevtoolPackage(t), "internal", "devtool")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	declared := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		ast.Inspect(file, func(node ast.Node) bool {
			spec, isSpec := node.(*ast.ValueSpec)
			if !isSpec || !isIdent(spec.Type, "VerifyProfile") {
				return true
			}
			for _, value := range spec.Values {
				lit, isLit := value.(*ast.BasicLit)
				require.True(t, isLit, "%s declares a VerifyProfile that is no string literal", entry.Name())
				name, unquoteErr := strconv.Unquote(lit.Value)
				require.NoError(t, unquoteErr)
				declared = append(declared, name)
			}

			return true
		})
	}
	assert.ElementsMatch(t, usage, declared, "the usage line and the VerifyProfile constants")
}

func assertVerifyStopsAfterPreflightFailure(
	t *testing.T,
	profile VerifyProfile,
	commandKey string,
	expectedCommands int,
	errorText string,
) {
	t.Helper()

	repoRoot := newFakeE2ERepo(t)
	runner := &fakeRunner{
		runErrByKey: map[string]error{
			commandKey: assert.AnError,
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  profile,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), errorText)
	require.Len(t, runner.runCommands, expectedCommands)
}

// Validates: R-6.10.8
func TestRunVerifyStressRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	runner := &fakeRunner{}

	// The wants below are literals for a CI runner's cores, so a changed
	// count or deadline shows up here whatever host runs the test.
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyStress,
		Cores:    unitTestReferenceCores,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	// The watch-ordering step is intentionally absent: it targeted
	// TestWatchOrderingStress_, which no test in the repo has ever defined, so
	// it reported success while running nothing. Every step streams through
	// Run; none captures its output until it exits.
	require.Len(t, runner.runCommands, 3)
	assert.Empty(t, runner.combinedCommands)

	// The multisync step runs every test of its package; the sync step runs
	// its declared members.
	repeated := stressRepeatedSteps()
	assert.Equal(t, "go", runner.runCommands[0].name)
	assert.Equal(t, []string{
		"test",
		"-json",
		"-race",
		"-count=50",
		"-shuffle=on",
		"-timeout=25m0s",
		"./internal/multisync",
	}, runner.runCommands[0].args)

	// Both sync steps name the tree, not the one directory, so a package
	// extracted from internal/sync stays under stress.
	assert.Equal(t, "go", runner.runCommands[1].name)
	assert.Equal(t, []string{
		"test",
		"-json",
		"-race",
		"-count=50",
		"-shuffle=on",
		"-timeout=1h15m0s",
		"-run", "^(" + strings.Join(repeated[1].members(), "|") + ")$",
		"./internal/sync/...",
	}, runner.runCommands[1].args)

	assert.Equal(t, "go", runner.runCommands[2].name)
	assert.Equal(t, []string{
		"test",
		"-json",
		"-race",
		"-count=3",
		"-shuffle=on",
		"-timeout=1h30m0s",
		"./internal/sync/...",
	}, runner.runCommands[2].args)
}

// Validates: R-6.10.8, R-6.10.26
//
// The stress lane's race runs of the sync tree are one go test whose -count,
// step name and deadline all carry stressSyncRaceRuns: the deadline is that
// many times the race profile's deadline for one run on the same cores, so a
// suite that grows moves both lanes' budgets together instead of ending the
// night at a fixed number. The two repeated steps take the deadlines of their
// own pass budgets on the same cores. Every want is a literal: three runs at
// the race profile's 30 minutes on a runner and at its 120 on 2 cores.
func TestRunVerifyStressSyncRaceStepTakesTheRaceDeadlineOncePerRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		cores     int
		deadline  string
		repeating []string
	}{
		{cores: 2, deadline: "-timeout=6h0m0s", repeating: []string{"-timeout=56m15s", "-timeout=7h30m0s"}},
		{cores: 4, deadline: "-timeout=1h30m0s", repeating: []string{"-timeout=25m0s", "-timeout=1h15m0s"}},
	} {
		t.Run(strconv.Itoa(tc.cores)+" cores", func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{}
			summaryPath := filepath.Join(t.TempDir(), "stress-summary.json")
			stdout := &bytes.Buffer{}
			require.NoError(t, RunVerify(context.Background(), runner, &VerifyOptions{
				RepoRoot:        t.TempDir(),
				Profile:         VerifyStress,
				Cores:           tc.cores,
				SummaryJSONPath: summaryPath,
				Stdout:          stdout,
				Stderr:          &bytes.Buffer{},
			}))

			require.Len(t, runner.runCommands, 3)
			assert.Contains(t, runner.runCommands[0].args, tc.repeating[0])
			assert.Contains(t, runner.runCommands[1].args, tc.repeating[1])
			assert.Equal(t, []string{"test", "-json", "-race", "-count=3", "-shuffle=on", tc.deadline, syncTestTree},
				runner.runCommands[2].args)
			assert.Contains(t, stdout.String(),
				"==> go test -json -race -count=3 -shuffle=on "+tc.deadline+" "+syncTestTree+"\n")

			var summary VerifySummary
			readVerifySummaryFile(t, summaryPath, &summary)
			assert.Equal(t, verifySummaryStatusPass, requireVerifySummaryStep(t, summary, stressSyncRaceStepName).Status)
		})
	}
}

// Validates: R-6.10.22
//
// The race profile is one go test of the whole sync tree under the race
// detector: unshortened, so the exhaustive tests run too, never from go
// test's cache, with the deadline of the cores this process may use unless
// --race-timeout names one, streamed, and recorded as one step of the
// summary.
func TestRunVerifyRaceRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		explicit time.Duration
		deadline time.Duration
	}{
		"the deadline of the cores": {deadline: raceSyncTimeoutForCores(runtime.GOMAXPROCS(0))},
		"--race-timeout":            {explicit: 75 * time.Minute, deadline: 75 * time.Minute},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{}
			summaryPath := filepath.Join(t.TempDir(), "race-summary.json")
			err := RunVerify(context.Background(), runner, &VerifyOptions{
				RepoRoot:        t.TempDir(),
				Profile:         VerifyRace,
				RaceTimeout:     tc.explicit,
				SummaryJSONPath: summaryPath,
				Stdout:          &bytes.Buffer{},
				Stderr:          &bytes.Buffer{},
			})
			require.NoError(t, err)

			require.Len(t, runner.runCommands, 1)
			assert.Empty(t, runner.combinedCommands)
			assert.Equal(t, "go", runner.runCommands[0].name)
			assert.Equal(t, []string{"test", "-race", "-count=1", "-timeout=" + tc.deadline.String(), "./internal/sync/..."},
				runner.runCommands[0].args)

			var summary VerifySummary
			readVerifySummaryFile(t, summaryPath, &summary)
			assert.Equal(t, string(VerifyRace), summary.Profile)
			assert.Equal(t, verifySummaryStatusPass, requireVerifySummaryStep(t, summary, raceStepName).Status)
		})
	}
}

// The race run of the sync tree, with the job's setup, took 12.2 and 15.8
// minutes in CI's verify-race job on 2026-11-15, so at the runners' core
// count and above its deadline is 30 minutes. Below it the deadline is 240
// core-minutes divided by the cores: one run took 87.1 minutes on a shared,
// loaded 2-core host. A host that reports no cores counts as one.
//
// Validates: R-6.10.22
func TestRaceSyncTimeoutForCoresGivesSmallHostsACoreBudget(t *testing.T) {
	t.Parallel()

	for _, cores := range []int{3, 4, 8} {
		assert.Equal(t, 30*time.Minute, raceSyncTimeoutForCores(cores), "cores %d", cores)
	}

	assert.Equal(t, 120*time.Minute, raceSyncTimeoutForCores(2))
	assert.Equal(t, 240*time.Minute, raceSyncTimeoutForCores(1))
	assert.Equal(t, 240*time.Minute, raceSyncTimeoutForCores(0))
	assert.Equal(t, 50*time.Minute, resolveRaceSyncTimeout(50*time.Minute, 2), "an explicit deadline wins")
	assert.Equal(t, 120*time.Minute, resolveRaceSyncTimeout(0, 2))
	assert.Equal(t, 30*time.Minute, resolveRaceSyncTimeout(0, 8))
}

// Validates: R-6.10.22, R-6.10.26
//
// A sync tree that lost its tests verifies nothing under the race detector
// either: the race step fails instead of passing on `[no test files]`.
func TestRunVerifyRaceFailsWhenNoPackageRanATest(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{runStdoutFunc: func([]string) string {
		return "?   \tgithub.com/tonimelisma/onedrive-go/internal/sync\t[no test files]\n"
	}}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyRace,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.ErrorContains(t, err, string(raceStepName)+" matched no tests")
}

// Validates: R-6.10.8, R-6.10.26
//
// A stress step without a -run pattern verifies nothing too when its packages
// lost their tests: go test passes a tree of `[no test files]`.
func TestRunVerifyStressFailsAStepWhosePackagesHaveNoTests(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if slices.Contains(args, "./internal/multisync") {
				return "?   \tgithub.com/tonimelisma/onedrive-go/internal/multisync\t[no test files]\n"
			}

			return passingGoTestOutput(args)
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyStress,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.ErrorContains(t, err, string(stressMultisyncRaceStepName)+" matched no tests")
	assert.NotContains(t, err.Error(), string(stressSyncRaceStepName)+" matched", "the other steps ran a test")
	require.Len(t, runner.runCommands, 3, "every step still runs")
}

// Validates: R-6.10.1, R-6.10.8
//
// go test exits 0 when a -run pattern matches nothing, so the sync step would
// stay green after every declared member was renamed away. Over the tree, a
// package without a member is fine; no member run anywhere fails the step,
// and the profile reports it once its other steps have run.
func TestRunVerifyStressFailsWhenNoMemberOfTheSyncStepRan(t *testing.T) {
	t.Parallel()

	syncArgs := stressRepeatedSteps()[1].command(runtime.GOMAXPROCS(0)).args
	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if !slices.Equal(args, syncArgs) {
				return passingGoTestOutput(args)
			}

			return strings.Join([]string{
				"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync\t0.3s [no tests to run]",
				"ok  \tgithub.com/tonimelisma/onedrive-go/internal/sync/planner\t0.1s [no tests to run]",
				"",
			}, "\n")
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyStress,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(stressSyncConcurrencyRaceStepName)+" matched no tests")
	assert.Len(t, runner.runCommands, 3, "the step after the one that verified nothing still runs")
}

// Validates: R-6.10.8
//
// The whole-tree step runs a tree, so two packages can each hold a test of
// one name. Keyed by name alone their timings would merge into one entry
// that belongs to neither.
func TestSummarizeStressSlowTestsKeysByPackage(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestShutdown","Elapsed":2.0}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync/planner","Test":"TestShutdown","Elapsed":0.5}`,
		`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestShutdown","Elapsed":1.0}`,
	}, "\n")

	slowTests := summarizeStressSlowTests(output, stressSlowTestLimit)

	require.Len(t, slowTests, 2)
	assert.Equal(t, StressSlowTestSummary{
		Package:        "github.com/tonimelisma/onedrive-go/internal/sync",
		Test:           "TestShutdown",
		Runs:           2,
		TotalElapsedMS: 3000,
		MaxElapsedMS:   2000,
	}, slowTests[0])
	assert.Equal(t, StressSlowTestSummary{
		Package:        "github.com/tonimelisma/onedrive-go/internal/sync/planner",
		Test:           "TestShutdown",
		Runs:           1,
		TotalElapsedMS: 500,
		MaxElapsedMS:   500,
	}, slowTests[1])

	var printed bytes.Buffer
	require.NoError(t, writeStressSlowTestSummary(&printed, slowTests))
	assert.Contains(t, printed.String(),
		"github.com/tonimelisma/onedrive-go/internal/sync/planner TestShutdown runs=1 total=500ms max=500ms")
}

// Validates: R-6.10.8
func TestRunVerifyStressWritesSlowTestSummaryToStdoutAndJSON(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	summaryPath := filepath.Join(t.TempDir(), "stress-verify-summary.json")
	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if !slices.Equal(args, stressSyncRaceCommand(runtime.GOMAXPROCS(0)).args) {
				return passingGoTestOutput(args)
			}

			return strings.Join([]string{
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestFast","Elapsed":0.125}`,
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestSlow","Elapsed":1.5}`,
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestSlow","Elapsed":1.0}`,
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestMedium","Elapsed":0.75}`,
			}, "\n")
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyStress,
		SummaryJSONPath: summaryPath,
		Stdout:          stdout,
		Stderr:          &bytes.Buffer{},
	})
	require.NoError(t, err)

	assert.Contains(t, stdout.String(), "slowest completed race tests")
	assert.Contains(t, stdout.String(), "github.com/tonimelisma/onedrive-go/internal/sync TestSlow runs=2 total=2.5s max=1.5s")
	assert.Contains(t, stdout.String(), string(stressSyncRaceStepName)+": pass")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	step := requireVerifySummaryStep(t, summary, stressSyncRaceStepName)
	require.Len(t, step.SlowTests, 3)
	assert.Equal(t, "github.com/tonimelisma/onedrive-go/internal/sync", step.SlowTests[0].Package)
	assert.Equal(t, "TestSlow", step.SlowTests[0].Test)
	assert.Equal(t, 2, step.SlowTests[0].Runs)
	assert.EqualValues(t, 2500, step.SlowTests[0].TotalElapsedMS)
	assert.EqualValues(t, 1500, step.SlowTests[0].MaxElapsedMS)
}

// streamCheckRunner is the fake runner of a stress run whose step with
// stepArgs prints its output and then waits, before it returns, until the
// test has seen that output on stdout: only a step that streams its output
// gets past it.
type streamCheckRunner struct {
	*fakeRunner
	stepArgs []string
	// printed is closed once the step printed its output.
	printed chan struct{}
	// seen is closed by the test once the output reached stdout.
	seen chan struct{}
}

func (r *streamCheckRunner) Run(
	ctx context.Context,
	cwd string,
	env []string,
	stdout, stderr io.Writer,
	name string,
	args ...string,
) error {
	if !slices.Equal(args, r.stepArgs) {
		return r.fakeRunner.Run(ctx, cwd, env, stdout, stderr, name, args...)
	}
	if _, err := io.WriteString(stdout, passingGoTestOutput(args)); err != nil {
		return fmt.Errorf("write fake stdout: %w", err)
	}
	close(r.printed)

	select {
	case <-r.seen:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for the test to read stdout: %w", ctx.Err())
	}
}

// Validates: R-6.10.8, R-6.10.26
//
// The whole-tree step captured its output until go test exited, so on CI a
// hang in it printed nothing until its deadline, and a job killed meanwhile
// left no test output at all. It streams like every other step: the text of
// each package's result reaches the log while the step still runs.
func TestRunVerifyStressStreamsTheRepeatStep(t *testing.T) {
	t.Parallel()

	runner := &streamCheckRunner{
		fakeRunner: &fakeRunner{},
		stepArgs:   stressSyncRaceCommand(runtime.GOMAXPROCS(0)).args,
		printed:    make(chan struct{}),
		seen:       make(chan struct{}),
	}
	var stdout outputCapture
	verified := make(chan error, 1)
	go func() {
		verified <- RunVerify(t.Context(), runner, &VerifyOptions{
			RepoRoot: t.TempDir(),
			Profile:  VerifyStress,
			Stdout:   &stdout,
			Stderr:   io.Discard,
		})
	}()

	select {
	case <-runner.printed:
	case err := <-verified:
		require.Failf(t, "the whole-tree step did not stream", "verify returned (%v) without running the step through Run", err)
	}
	assert.Contains(t, stdout.String(), rawGoTestPackagePassPrefix+fakeGoTestPackage(runner.stepArgs),
		"the step's output is on stdout while the step still runs")
	close(runner.seen)
	require.NoError(t, <-verified)
	assert.Contains(t, stdout.String(), "slowest completed race tests", "the step keeps its slow-test summary")
}

// Validates: R-6.10.26
//
// The integration tier ran every test of ./internal/graph/..., the untagged
// unit tests included, so it passed although no integration test ran. It runs
// the integration tests only, and fails when none of them ran.
func TestRunVerifyIntegrationRunsOnlyTheIntegrationTests(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	require.NoError(t, RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyIntegration,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	}))
	require.Len(t, runner.runCommands, 1)
	assert.Equal(t, []string{
		"test", "-tags=integration", "-race", "-v", "-run=^TestIntegration_", "-timeout=5m", "./internal/graph/...",
	}, runner.runCommands[0].args)

	empty := &fakeRunner{runStdoutFunc: func([]string) string {
		return "testing: warning: no tests to run\nPASS\nok  \t" + repoModulePath + "/internal/graph\t0.021s [no tests to run]\n"
	}}
	err := RunVerify(context.Background(), empty, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyIntegration,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.ErrorContains(t, err, "integration tests matched no tests")
}

func TestRunVerifyFailedTestRerunsLastJSONFailureOnly(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	logPath := filepath.Join(repoRoot, "ci.log")
	require.NoError(t, os.WriteFile(logPath, []byte(strings.Join([]string{
		`{"Action":"fail","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestOld"}`,
		`2026-04-30T20:00:00Z {"Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Status_JSONShape"}`,
		`{"Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e"}`,
	}, "\n")), 0o600))

	runner := &fakeRunner{runStdoutFunc: goTestReportsPass("TestE2E_Status_JSONShape")}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:      repoRoot,
		Profile:       VerifyFailedTest,
		FailedTestLog: logPath,
		Stdout:        &bytes.Buffer{},
		Stderr:        &bytes.Buffer{},
	})

	require.NoError(t, err)
	require.Len(t, runner.runCommands, 1)
	assert.Equal(t, "go", runner.runCommands[0].name)
	assert.Equal(t, []string{
		"test",
		"-tags=e2e e2e_full",
		"-race",
		"-run=^TestE2E_Status_JSONShape$",
		"-count=1",
		"-v",
		"-parallel",
		"1",
		"-timeout=15m",
		"./e2e/...",
	}, runner.runCommands[0].args)
}

func TestRunVerifyFailedTestRerunsLastRawPackageAndSubtest(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	logPath := filepath.Join(repoRoot, "ci.log")
	require.NoError(t, os.WriteFile(logPath, []byte(
		"--- FAIL: TestWatchLoop/closed_scheduler (0.00s)\n"+
			"FAIL\tgithub.com/tonimelisma/onedrive-go/internal/sync\t0.123s",
	), 0o600))

	runner := &fakeRunner{runStdoutFunc: goTestReportsPass("TestWatchLoop/closed_scheduler")}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:      repoRoot,
		Profile:       VerifyFailedTest,
		FailedTestLog: logPath,
		Stdout:        &bytes.Buffer{},
		Stderr:        &bytes.Buffer{},
	})

	require.NoError(t, err)
	require.Len(t, runner.runCommands, 1)
	assert.Equal(t, []string{
		"test",
		"-race",
		"-run=^TestWatchLoop$/^closed_scheduler$",
		"-count=1",
		"-v",
		"./internal/sync",
	}, runner.runCommands[0].args)
}

// Validates: R-6.10.26
//
// The full tier runs under the race detector and the stress lane shuffles test
// order. A rerun without either can pass on a failure only they produce, and
// would report a race or an order dependence as fixed.
func TestRunVerifyFailedTestKeepsRaceAndTheShuffleSeed(t *testing.T) {
	t.Parallel()

	const syncPackage = "github.com/tonimelisma/onedrive-go/internal/sync"
	const multisyncPackage = "github.com/tonimelisma/onedrive-go/internal/multisync"

	cases := []struct {
		name string
		log  string
	}{
		{
			name: "raw output",
			log: strings.Join([]string{
				"-test.shuffle 111",
				"ok  \t" + multisyncPackage + "\t1.0s",
				"-test.shuffle 1731340000000000000",
				"--- FAIL: TestWatchLoop (0.01s)",
				"FAIL\t" + syncPackage + "\t0.123s",
			}, "\n"),
		},
		{
			name: "json output",
			log: strings.Join([]string{
				`{"Action":"output","Package":"` + multisyncPackage + `","Output":"-test.shuffle 111\n"}`,
				`{"Action":"output","Package":"` + syncPackage + `","Output":"-test.shuffle 1731340000000000000\n"}`,
				`{"Action":"fail","Package":"` + syncPackage + `","Test":"TestWatchLoop"}`,
				`{"Action":"fail","Package":"` + syncPackage + `"}`,
				`{"Action":"pass","Package":"` + multisyncPackage + `"}`,
			}, "\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoRoot := t.TempDir()
			logPath := filepath.Join(repoRoot, "ci.log")
			require.NoError(t, os.WriteFile(logPath, []byte(tc.log), 0o600))

			runner := &fakeRunner{runStdoutFunc: goTestReportsPass("TestWatchLoop")}
			require.NoError(t, RunVerify(context.Background(), runner, &VerifyOptions{
				RepoRoot:      repoRoot,
				Profile:       VerifyFailedTest,
				FailedTestLog: logPath,
				Stdout:        &bytes.Buffer{},
				Stderr:        &bytes.Buffer{},
			}))

			require.Len(t, runner.runCommands, 1)
			assert.Equal(t, []string{
				"test",
				"-race",
				"-shuffle=1731340000000000000",
				"-run=^TestWatchLoop$",
				"-count=1",
				"-v",
				"./internal/sync",
			}, runner.runCommands[0].args)
		})
	}
}

// Validates: R-6.10.25, R-6.10.26
//
// A failing seed is rerun as its replay line says: with the seed's variable
// set and the race detector exactly when the failing run had it, since
// without it some families do other work for the same seed. Of two failing
// seeds, the rerun takes the one of the failure the log settled last. The
// rerun runs under the dispatch order the line names and under none when it
// names none, whatever the caller's environment holds, and under a deadline
// of its own.
func TestRunVerifyFailedTestRerunsAFailingSeedAsItsReplayLineSays(t *testing.T) {
	t.Parallel()

	const chaosTest = "TestModel_ChaosGraphConverges"
	cases := map[string]struct {
		replays []string
		order   string
		want    []string
	}{
		"without the race detector": {
			replays: []string{
				"ONEDRIVE_GO_MODEL_SEED=7001 go test -run '^" + chaosTest + "$' ./internal/sync",
				"ONEDRIVE_GO_MODEL_SEED=9 go test -run '^" + chaosTest + "$' ./internal/sync",
			},
			want: []string{"test", "-run=^" + chaosTest + "$", "-count=1", "-v", "-timeout=30m", "./internal/sync"},
		},
		"with the race detector": {
			replays: []string{"ONEDRIVE_GO_MODEL_SEED=7001 go test -race -run '^" + chaosTest + "$' ./internal/sync"},
			want:    []string{"test", "-race", "-run=^" + chaosTest + "$", "-count=1", "-v", "-timeout=30m", "./internal/sync"},
		},
		"with a dispatch order": {
			replays: []string{
				"ONEDRIVE_GO_MODEL_SEED=7001 ONEDRIVE_GO_DISPATCH_ORDER=reverse go test -race -run '^" + chaosTest + "$' ./internal/sync",
			},
			order: "reverse",
			want:  []string{"test", "-race", "-run=^" + chaosTest + "$", "-count=1", "-v", "-timeout=30m", "./internal/sync"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lines := []string{}
			for _, replay := range tc.replays {
				lines = append(lines, "        test_helpers_test.go:480: "+seedReplayPrefix+replay)
			}
			lines = append(lines,
				"    --- FAIL: "+chaosTest+"/seed=9 (4.00s)",
				"    --- FAIL: "+chaosTest+"/seed=7001 (4.00s)",
				"FAIL\t"+repoModulePath+"/internal/sync\t9.0s")
			repoRoot := t.TempDir()
			logPath := filepath.Join(repoRoot, "ci.log")
			require.NoError(t, os.WriteFile(logPath, []byte(strings.Join(lines, "\n")), 0o600))

			runner := &fakeRunner{runStdoutFunc: goTestReportsPass(chaosTest + "/seed=7001")}
			var stdout bytes.Buffer
			require.NoError(t, RunVerify(context.Background(), runner, &VerifyOptions{
				RepoRoot:      repoRoot,
				Profile:       VerifyFailedTest,
				FailedTestLog: logPath,
				Stdout:        &stdout,
				Stderr:        &bytes.Buffer{},
			}))

			require.Len(t, runner.runCommands, 1)
			assert.Equal(t, tc.want, runner.runCommands[0].args)
			assertCommandHasEnvVar(t, runner.runCommands[0], "ONEDRIVE_GO_MODEL_SEED=7001")
			assertCommandHasEnvVar(t, runner.runCommands[0], seedsDispatchOrderEnv+"="+tc.order)
			if tc.order != "" {
				assertCommandLacksEnvVar(t, runner.runCommands[0], seedsDispatchOrderEnv+"=")
			}
			assert.Contains(t, stdout.String(),
				"==> ONEDRIVE_GO_MODEL_SEED=7001 "+seedsDispatchOrderEnv+"="+tc.order+" go test")
		})
	}
}

// Validates: R-6.10.26
//
// go test exits 0 when -run selects nothing, so the rerun of a test renamed
// or deleted since the failing run used to report success while running
// nothing.
func TestRunFailedTestVerification_PatternThatSelectsNothingFails(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	logPath := filepath.Join(repoRoot, "ci.log")
	require.NoError(t, os.WriteFile(logPath, []byte(
		"--- FAIL: TestRenamedSince (0.01s)\nFAIL\t"+testSyncPackage+"\t0.101s\n",
	), 0o600))

	runner := &fakeRunner{runStdoutFunc: func([]string) string {
		return "testing: warning: no tests to run\nPASS\nok  \t" + testSyncPackage + "\t0.011s [no tests to run]\n"
	}}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:      repoRoot,
		Profile:       VerifyFailedTest,
		FailedTestLog: logPath,
		Stdout:        &bytes.Buffer{},
		Stderr:        &bytes.Buffer{},
	})

	require.ErrorContains(t, err, "matched no tests")
	require.Len(t, runner.runCommands, 1)
	assert.Contains(t, runner.runCommands[0].args, "-run=^TestRenamedSince$")
}

func TestRunVerifyFailedTestRequiresLogPath(t *testing.T) {
	t.Parallel()

	err := RunVerify(context.Background(), &fakeRunner{}, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyFailedTest,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--failed-log is required")
}

// Validates: R-6.10.1
func TestResolveVerifyPlanRejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	_, err := resolveVerifyPlan("weird")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage")
}

func TestWriteVerifySummaryRoundTripsJSON(t *testing.T) {
	t.Parallel()

	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	expected := VerifySummary{
		Profile:         string(VerifyPublic),
		Status:          verifySummaryStatusPass,
		TotalDurationMS: 1234,
		Steps: []VerifyStepSummary{
			{Name: "format", Status: verifySummaryStatusPass, DurationMS: 10},
		},
		QuirkEventCount: 2,
	}

	data, err := json.Marshal(expected)
	require.NoError(t, err)
	require.NoError(t, atomicWrite(summaryPath, append(data, '\n'), ".verify-summary-*.tmp"))

	var actual VerifySummary
	readVerifySummaryFile(t, summaryPath, &actual)
	assert.Equal(t, expected, actual)
}
