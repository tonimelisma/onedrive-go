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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Command positions in the default verify profile. Named so that inserting a
// step is a one-line change here rather than a silent renumbering that shifts
// every downstream assertion onto the wrong command.
const (
	cmdFormatGofumpt      = 0
	cmdFormatGoimports    = 1
	cmdLint               = 2
	cmdBuild              = 3
	cmdBuildFirstPlatform = 4
)

// The cross-platform builds occupy supportedPlatformCount slots starting at
// cmdBuildFirstPlatform, so everything after them is expressed relative to
// that run rather than hardcoded.
const (
	cmdUnitTests        = cmdBuildFirstPlatform + supportedPlatformCount
	cmdE2EFullCompile   = cmdUnitTests + 1
	cmdTaggedVetE2E     = cmdE2EFullCompile + 1
	cmdTaggedVetInteg   = cmdTaggedVetE2E + 1
	cmdE2EAuthPreflight = cmdTaggedVetInteg + 1
	cmdE2EFastPreflight = cmdE2EAuthPreflight + 1
	cmdE2ESuite         = cmdE2EFastPreflight + 1

	verifyDefaultCommandCount = cmdE2ESuite + 1
)

// Validates: R-6.10.1
func TestRunVerifyDefaultRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
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
		Profile:      VerifyDefault,
		CoverageFile: filepath.Join(repoRoot, "cover.out"),
		UnitTimeout:  25 * time.Minute,
		Stdout:       stdout,
		Stderr:       stderr,
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, verifyDefaultCommandCount)
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
	require.Len(t, runner.outputCommands, 1)
	assert.Contains(t, stdout.String(), "==> coverage")
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
		{cores: 3, want: 20 * time.Minute},
		{cores: 4, want: 20 * time.Minute},
		{cores: 8, want: 20 * time.Minute},
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
	assert.Equal(t, 20*time.Minute, resolveUnitTestTimeout(0, 8))
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
	writeRequirementsBackedReadme(t, repoRoot)
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
		"golangci-lint run --allow-parallel-runners",
		"go build ./...",
		"go build ./... GOOS=linux",
		"go build ./... GOOS=darwin",
		"go build ./... GOOS=freebsd",
		"go test -race",
		"go test -c",
		"go vet -tags=e2e e2e_full",
		"go vet -tags=integration",
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

	repoRoot := t.TempDir()
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
	assert.Equal(t, fullE2EBucketCommandArgs(fullE2EParallelMiscBucket()), runner.runCommands[4].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fullE2ESerialSyncBucket()), runner.runCommands[5].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fullE2ESerialWatchSharedBucket()), runner.runCommands[6].args)
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
	repoRoot := t.TempDir()
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
	require.Len(t, runner.runCommands, 2+e2eFullCommandsBeforeBuckets-1+len(fullE2EBuckets()))
	assert.Empty(t, runner.combinedCommands)
	assert.Equal(t, runner.runCommands[0].args, runner.runCommands[1].args)
	assert.Contains(t, stdout, "LI-20260405-06")
	assert.Contains(t, stdout, "classified reruns:")
	assertVerifySummaryHasStep(t, summary, "auth preflight")
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

	_, stdout, err := runE2EFullForTest(t, runner, true)
	require.NoError(t, err)

	assert.Empty(t, runner.combinedCommands, "the classified fast suite streams through Run")
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+1+len(fullE2EBuckets()))
	assert.Equal(t, []string{"test", "-json", "-tags=e2e", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[2].args)
	assert.Equal(t,
		[]string{"test", "-tags=e2e", "-run=^TestE2E_Sync_DownloadOnly$", "-count=1", "-v", "-timeout=10m", "./e2e/..."},
		runner.runCommands[3].args)
	assert.Equal(t, fullE2EBucketJSONCommandArgs(fullE2ESerialWatchSharedBucket()), runner.runCommands[len(runner.runCommands)-1].args)
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
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(fullE2EBuckets()))
	assert.Equal(t,
		[]string{verifySummaryStatusPass, verifySummaryStatusPass, verifySummaryStatusPass},
		bucketStatuses(summary))
	fast := requireVerifySummaryStep(t, summary, "fast e2e")
	assert.Equal(t, verifySummaryStatusFail, fast.Status)
	assert.Equal(t, []string{"TestE2E_Sync_DeletePropagation"}, fast.FailedTests)
}

func TestRunVerifyE2EFullClassifiesKnownWatchSharedQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	watchKey := goCommandKey(fullE2EBucketJSONCommandArgs(fullE2ESerialWatchSharedBucket()))
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
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(fullE2EBuckets())+1)
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
	assertVerifyBucketSummary(t, summary, fullE2ESerialWatchSharedBucket().Name, verifySummaryStatusPass)
}

func TestRunVerifyE2EFullDoesNotMaskUnknownWatchSharedFailure(t *testing.T) {
	t.Parallel()

	watchKey := goCommandKey(fullE2EBucketJSONCommandArgs(fullE2ESerialWatchSharedBucket()))
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
	require.Len(t, runner.runCommands, e2eFullCommandsBeforeBuckets+len(fullE2EBuckets()), "an unknown failure is not rerun")
	require.Len(t, summary.E2EFullBuckets, len(fullE2EBuckets()))
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
	writeRequirementsBackedReadme(t, repoRoot)
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
	assertVerifySummaryHasStep(t, summary, "e2e-full compile")
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
	collector := newVerifySummaryCollector(VerifyE2E, stdout, summaryPath, logDir)

	require.NoError(t, collector.finalize(nil))

	assert.Contains(t, stdout.String(), "quirk events: 2")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, 2, summary.QuirkEventCount)
}

// Validates: R-6.10.26
func TestRunVerifyWritesSummaryJSONOnFailure(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	bucketArgs := fullE2EBucketCommandArgs(fullE2ESerialSyncBucket())
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
	assertVerifySummaryHasStep(t, summary, "fast e2e")
	assertVerifyBucketSummary(t, summary, fullE2EParallelMiscBucket().Name, verifySummaryStatusPass)
	assertVerifyBucketSummary(t, summary, fullE2ESerialSyncBucket().Name, verifySummaryStatusFail)
	// The bucket after the failing one still ran.
	assertVerifyBucketSummary(t, summary, fullE2ESerialWatchSharedBucket().Name, verifySummaryStatusPass)
	assert.Contains(t, stdout.String(), "verify summary")
	assert.Contains(t, stdout.String(), fullE2ESerialSyncBucket().Name)
}

func TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	fullTests, err := discoverTaggedE2ETests(filepath.Join(repoRoot, "e2e"), "e2e && e2e_full")
	require.NoError(t, err)

	// The full fixture preflight runs on its own, ahead of the buckets.
	assigned := map[string]int{fullE2EFixturePreflight: 1}
	for _, bucket := range fullE2EBuckets() {
		for _, name := range bucket.TestNames {
			assigned[name]++
		}
	}

	for _, name := range fullTests {
		assert.Equalf(t, 1, assigned[name], "full-tag test %s must be assigned exactly once", name)
	}
	for name, count := range assigned {
		assert.Containsf(t, fullTests, name, "assigned full-tag test %s must exist", name)
		assert.Equalf(t, 1, count, "assigned full-tag test %s must appear once", name)
	}
}

func TestFastE2EExecutionManifestKeepsOnlySmokeLiveTests(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	fastTests, err := discoverTaggedE2ETests(filepath.Join(repoRoot, "e2e"), "e2e")
	require.NoError(t, err)

	assert.Contains(t, fastTests, "TestE2E_AuthPreflight_Fast")
	assert.Contains(t, fastTests, "TestE2E_FixturePreflight_Fast")
	assert.Contains(t, fastTests, "TestE2E_FileOpsSmokeCRUD")
	assert.Contains(t, fastTests, "TestE2E_Sync_UploadOnly")
	assert.Contains(t, fastTests, "TestE2E_Sync_DownloadOnly")
	assert.Contains(t, fastTests, "TestE2E_SyncWatch_WebsocketDisabledLongPollRegression")
	assert.Contains(t, fastTests, "TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount")
	assert.Contains(t, fastTests, "TestE2E_TrackedFilesNameNoTestAccount", "only the live jobs know the addresses it looks for")

	assert.NotContains(t, fastTests, "TestE2E_FileOps_Whoami")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_LsRoot")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_Mkdir")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_Put")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_LsFolder")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_Stat")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_Get")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_RmFile")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_RmSubfolder")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_RmPermanent")
	assert.NotContains(t, fastTests, "TestE2E_FileOps_Status")
	assert.NotContains(t, fastTests, "TestE2E_ErrorCases")
	assert.NotContains(t, fastTests, "TestE2E_JSONOutput")
	assert.NotContains(t, fastTests, "TestE2E_QuietFlag")
}

// discoverTaggedE2ETests lists, in name order, the top-level Test functions
// of the e2e test files whose build constraint is exactly buildExpression.
func discoverTaggedE2ETests(e2eDir string, buildExpression string) ([]string, error) {
	entries, err := os.ReadDir(e2eDir)
	if err != nil {
		return nil, fmt.Errorf("read e2e dir: %w", err)
	}

	tests := make([]string, 0)
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		path := filepath.Join(e2eDir, entry.Name())
		data, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if !hasBuildExpression(string(data), buildExpression) {
			continue
		}

		file, err := parser.ParseFile(fset, path, data, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}

			tests = append(tests, fn.Name.Name)
		}
	}

	slices.Sort(tests)
	return tests, nil
}

// hasBuildExpression reports whether source's leading comment block carries
// a //go:build line equal to buildExpression.
func hasBuildExpression(source string, buildExpression string) bool {
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "//") {
			break
		}
		if !strings.HasPrefix(line, "//go:build ") {
			continue
		}

		return strings.TrimSpace(strings.TrimPrefix(line, "//go:build ")) == buildExpression
	}

	return false
}

func TestFullE2EBucketsOwnDemotedFastTests(t *testing.T) {
	t.Parallel()

	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_DriveList_HappyPath_Text")
	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_Shared_FileDiscoveryAndSelectorReadCommands")
	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_Logout_PreservesOfflineAccountCatalog")
	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_ErrorCases")
	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_JSONOutput")
	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_QuietFlag")
	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_RoundTrip")
	assert.Contains(t, fullE2EParallelMiscTestNames(), "TestE2E_Status_LiveOverlay_ConfigTolerance")
	assert.NotContains(t, fullE2EParallelMiscTestNames(), "TestE2E_Whoami_ConfigTolerance")

	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_DryRun")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_InternalBaselineVerification")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_Conflicts")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_DriveRemoveAndReAdd")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_DirectionalModes_PreserveEditEditConflict")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges")
	assert.Contains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges")
	assert.NotContains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile")
	assert.NotContains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload")
	assert.NotContains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_ResolveAll")
	assert.NotContains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_DeleteSafetyThreshold")
	assert.NotContains(t, fullE2ESerialSyncTestNames(), "TestE2E_Sync_ResolveDryRun")

	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_SyncWatch_WebsocketStartupSmoke")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_ExplicitStandaloneSharedFolderRemainsConfiguredDrive")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_RenameReusesChildMountState")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_MoveReusesChildMountState")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_WritableUploadSyncsToSharedTarget")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_ReadOnlyBlockedUploadStatus")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget")
	assert.Contains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot")
	assert.NotContains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Resolve_WithWatchDaemonExecutesQueuedIntent")
	assert.NotContains(t, fullE2ESerialWatchSharedTestNames(), "TestE2E_Resolve_DeletesWithWatchDaemon")
}

func assertVerifyStopsAfterPreflightFailure(
	t *testing.T,
	profile VerifyProfile,
	commandKey string,
	expectedCommands int,
	errorText string,
) {
	t.Helper()

	repoRoot := t.TempDir()
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

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyStress,
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

	assert.Equal(t, "go", runner.runCommands[0].name)
	assert.Equal(t, []string{
		"test",
		"-race",
		"-count=50",
		"-shuffle=on",
		"-timeout=20m",
		"./internal/multisync",
	}, runner.runCommands[0].args)

	// Both sync steps name the tree, not the one directory, so a package
	// extracted from internal/sync stays under stress.
	assert.Equal(t, "go", runner.runCommands[1].name)
	assert.Equal(t, []string{
		"test",
		"-race",
		"-count=50",
		"-shuffle=on",
		"-timeout=40m",
		"-run", stressSyncConcurrencyPattern,
		"./internal/sync/...",
	}, runner.runCommands[1].args)

	assert.Equal(t, "go", runner.runCommands[2].name)
	assert.Equal(t, []string{
		"test",
		"-json",
		"-race",
		"-count=3",
		"-shuffle=on",
		"-timeout=40m",
		"./internal/sync/...",
	}, runner.runCommands[2].args)
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
	require.ErrorContains(t, err, "multisync race x50 matched no tests")
	assert.NotContains(t, err.Error(), "sync race x3 matched", "the other steps ran a test")
	require.Len(t, runner.runCommands, 3, "every step still runs")
}

// Validates: R-6.10.1, R-6.10.8
//
// go test exits 0 when a -run pattern matches nothing, so the x50 step would
// stay green after a rename took every concurrency test out of the pattern.
// Over the tree, a package without a match is fine; no match anywhere fails
// the step, and the profile reports it once its other steps have run.
func TestRunVerifyStressFailsWhenConcurrencyPatternMatchesNothing(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if !slices.Contains(args, "-run") {
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
	assert.Contains(t, err.Error(), "sync concurrency race x50 matched no tests")
	assert.Len(t, runner.runCommands, 3, "the step after the one that verified nothing still runs")
}

// Validates: R-6.10.8
//
// The x3 step runs a tree, so two packages can each hold a test of one name.
// Keyed by name alone their timings would merge into one entry that belongs
// to neither.
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
			if goCommandKey(args) != "go test -json -race -count=3 -shuffle=on -timeout=40m ./internal/sync/..." {
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

	assert.Contains(t, stdout.String(), "slowest completed sync race tests")
	assert.Contains(t, stdout.String(), "github.com/tonimelisma/onedrive-go/internal/sync TestSlow runs=2 total=2.5s max=1.5s")
	assert.Contains(t, stdout.String(), "sync race x3: pass")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	step := requireVerifySummaryStep(t, summary, "sync race x3")
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
// The x3 step captured its output until go test exited, so on CI a hang in it
// printed nothing for forty minutes, and a job killed meanwhile left no test
// output at all. It streams like every other step.
func TestRunVerifyStressStreamsTheRepeatStep(t *testing.T) {
	t.Parallel()

	runner := &streamCheckRunner{
		fakeRunner: &fakeRunner{},
		stepArgs: []string{
			"test", "-json", "-race", "-count=3", "-shuffle=on", "-timeout=" + stressSyncTimeout, stressSyncTree,
		},
		printed: make(chan struct{}),
		seen:    make(chan struct{}),
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
		require.Failf(t, "the x3 step did not stream", "verify returned (%v) without running the step through Run", err)
	}
	assert.Contains(t, stdout.String(), fakePassingTestName, "the step's output is on stdout while the step still runs")
	close(runner.seen)
	require.NoError(t, <-verified)
	assert.Contains(t, stdout.String(), "slowest completed sync race tests", "the step keeps its slow-test summary")
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

	runner := &fakeRunner{}
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

	runner := &fakeRunner{}
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

			runner := &fakeRunner{}
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

func writeRequirementsBackedReadme(t *testing.T, repoRoot string) {
	t.Helper()

	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "README.md"),
		[]byte("# Project\n\n## Status\n\nActive development. See spec/requirements/index.md for capability status.\n"),
	))
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
	require.NoError(t, atomicWrite(summaryPath, append(data, '\n'), 0o600, 0o700, ".verify-summary-*.tmp"))

	var actual VerifySummary
	readVerifySummaryFile(t, summaryPath, &actual)
	assert.Equal(t, expected, actual)
}
