package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.1, R-6.2.2
func TestRunVerifyDefaultRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyDefault,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            stdout,
		Stderr:            stderr,
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 9)
	assert.Equal(t, "gofumpt", runner.runCommands[0].name)
	assert.Equal(t, []string{"-w", "."}, runner.runCommands[0].args)
	assert.Equal(t, "goimports", runner.runCommands[1].name)
	assert.Equal(t, "golangci-lint", runner.runCommands[2].name)
	assert.Equal(t, "go", runner.runCommands[3].name)
	assert.Equal(t, []string{"build", "./..."}, runner.runCommands[3].args)
	assert.Equal(t, []string{"test", "-race", "-coverprofile=" + filepath.Join(repoRoot, "cover.out"), "./..."}, runner.runCommands[4].args)
	assert.Equal(t, "test", runner.runCommands[5].args[0])
	assert.Equal(t, "-c", runner.runCommands[5].args[1])
	assert.Equal(t, "-race", runner.runCommands[5].args[2])
	assert.Equal(t, "-tags=e2e e2e_full", runner.runCommands[5].args[3])
	assert.Equal(t, "-o", runner.runCommands[5].args[4])
	assert.Contains(t, filepath.Base(runner.runCommands[5].args[5]), strings.TrimSuffix(fullE2ECompileArtifactName, ".test"))
	assert.Equal(t, "./e2e", runner.runCommands[5].args[6])
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + authE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[6].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + fastE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[7].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[8].args)
	assertCommandHasEnvVar(t, runner.runCommands[6], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[6], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[6])
	assertCommandHasEnvVar(t, runner.runCommands[7], e2eRunFastFixturePreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[7], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[7], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandHasEnvVar(t, runner.runCommands[8], e2eSkipSuiteScrubEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[8], e2eRunAuthPreflightEnvVar+"=1")
	assertCommandLacksEnvVar(t, runner.runCommands[8], e2eRunFastFixturePreflightEnvVar+"=1")
	require.Len(t, runner.outputCommands, 1)
	assert.Contains(t, stdout.String(), "==> coverage")
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
	require.Len(t, runner.runCommands, 1)

	args := runner.runCommands[0].args
	require.Len(t, args, 7)
	assert.Equal(t, "-o", args[4])
	assert.NotEqual(t, filepath.Join(os.TempDir(), fullE2ECompileArtifactName), args[5])
	assert.Contains(t, filepath.Base(args[5]), strings.TrimSuffix(fullE2ECompileArtifactName, ".test"))
}

// Validates: R-6.2.1, R-6.2.2
func TestRunVerifyPublicRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            &bytes.Buffer{},
		Stderr:            &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 6)
	assert.Equal(t, "test", runner.runCommands[5].args[0])
	assert.Equal(t, "-c", runner.runCommands[5].args[1])
	assert.Equal(t, "-race", runner.runCommands[5].args[2])
	assert.Equal(t, "-tags=e2e e2e_full", runner.runCommands[5].args[3])
	assert.Equal(t, "-o", runner.runCommands[5].args[4])
	assert.Contains(t, filepath.Base(runner.runCommands[5].args[5]), strings.TrimSuffix(fullE2ECompileArtifactName, ".test"))
	assert.Equal(t, "./e2e", runner.runCommands[5].args[6])
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
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + authE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[0].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + fastE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[1].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[2].args)
	assert.Equal(t, []string{"test", "-tags=e2e e2e_full", "-run=" + fullE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[3].args)
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
	assert.True(t, os.IsNotExist(eventsErr))
	_, summaryErr := os.Stat(filepath.Join(logDir, e2eTimingSummaryFileName))
	assert.True(t, os.IsNotExist(summaryErr))
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
	assert.True(t, os.IsNotExist(eventsErr))
	_, summaryErr := os.Stat(filepath.Join(logDir, e2eTimingSummaryFileName))
	assert.True(t, os.IsNotExist(summaryErr))
	_, quirkEventsErr := os.Stat(filepath.Join(logDir, e2eQuirkEventsFileName))
	assert.True(t, os.IsNotExist(quirkEventsErr))
	_, quirkSummaryErr := os.Stat(filepath.Join(logDir, e2eQuirkSummaryFileName))
	assert.True(t, os.IsNotExist(quirkSummaryErr))
}

func TestRunVerifyE2EFullClassifiesKnownAuthPreflightQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	runner := &fakeRunner{
		runErrSequenceByKey: map[string][]error{
			"go test -tags=e2e -run=" + authE2EPreflightPattern + " -count=1 -v ./e2e/...": {
				assert.AnError,
			},
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:           repoRoot,
		Profile:            VerifyE2EFull,
		ClassifyLiveQuirks: true,
		Stdout:             stdout,
		Stderr:             &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 6)
	require.Len(t, runner.combinedCommands, 2)
	assert.Equal(t, runner.runCommands[0].args, runner.runCommands[1].args)
	assert.Contains(t, stdout.String(), "LI-20260405-06")
	assert.Contains(t, stdout.String(), "classified reruns:")
}

func TestRunVerifyE2EFullClassifiesKnownFastSuiteQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"go test -json -tags=e2e -v -parallel 5 -timeout=10m ./e2e/...": []byte(strings.Join([]string{
				`{"Time":"2026-04-08T00:00:00Z","Action":"run","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DownloadOnly"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DownloadOnly"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e"}`,
			}, "\n")),
		},
		combinedErrByKey: map[string]error{
			"go test -json -tags=e2e -v -parallel 5 -timeout=10m ./e2e/...": assert.AnError,
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:           repoRoot,
		Profile:            VerifyE2EFull,
		ClassifyLiveQuirks: true,
		Stdout:             stdout,
		Stderr:             &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.combinedCommands, 2)
	assert.Equal(t, []string{"test", "-json", "-tags=e2e", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.combinedCommands[0].args)
	assert.Equal(t, fullE2EBucketJSONCommandArgs(fullE2ESerialWatchSharedBucket()), runner.combinedCommands[1].args)
	require.Len(t, runner.runCommands, 6)
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=^TestE2E_Sync_DownloadOnly$", "-count=1", "-v", "./e2e/..."}, runner.runCommands[2].args)
	assert.Contains(t, stdout.String(), "LI-20260405-04")
	assert.Contains(t, stdout.String(), "classified reruns:")
}

func TestRunVerifyE2EFullDoesNotMaskUnknownFastSuiteFailure(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"go test -json -tags=e2e -v -parallel 5 -timeout=10m ./e2e/...": []byte(strings.Join([]string{
				`{"Time":"2026-04-08T00:00:00Z","Action":"run","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DeletePropagation"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DeletePropagation"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e"}`,
			}, "\n")),
		},
		combinedErrByKey: map[string]error{
			"go test -json -tags=e2e -v -parallel 5 -timeout=10m ./e2e/...": assert.AnError,
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:           repoRoot,
		Profile:            VerifyE2EFull,
		ClassifyLiveQuirks: true,
		Stdout:             &bytes.Buffer{},
		Stderr:             &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fast e2e")
	require.Len(t, runner.combinedCommands, 1)
	require.Len(t, runner.runCommands, 2)
}

func TestRunVerifyE2EFullClassifiesKnownWatchSharedQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	watchSharedJSONArgs := fullE2EBucketJSONCommandArgs(fullE2ESerialWatchSharedBucket())
	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"go " + strings.Join(watchSharedJSONArgs, " "): []byte(strings.Join([]string{
				`{"Time":"2026-04-22T00:00:00Z","Action":"run","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart"}`,
				`{"Time":"2026-04-22T00:01:30Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart"}`,
				`{"Time":"2026-04-22T00:01:30Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e"}`,
			}, "\n")),
		},
		combinedErrByKey: map[string]error{
			"go " + strings.Join(watchSharedJSONArgs, " "): assert.AnError,
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:           repoRoot,
		Profile:            VerifyE2EFull,
		ClassifyLiveQuirks: true,
		Stdout:             stdout,
		Stderr:             &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.combinedCommands, 2)
	assert.Equal(t, watchSharedJSONArgs, runner.combinedCommands[1].args)
	require.Len(t, runner.runCommands, 6)
	assert.Equal(t, []string{
		"test",
		"-tags=e2e e2e_full",
		"-race",
		"-run=^TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart$",
		"-count=1",
		"-v",
		"-parallel",
		"1",
		"-timeout=60m",
		"./e2e/...",
	}, runner.runCommands[5].args)
	assert.Contains(t, stdout.String(), "LI-20260405-03")
	assert.Contains(t, stdout.String(), "classified reruns:")
}

func TestRunVerifyE2EFullDoesNotMaskUnknownWatchSharedFailure(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	watchSharedJSONArgs := fullE2EBucketJSONCommandArgs(fullE2ESerialWatchSharedBucket())
	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"go " + strings.Join(watchSharedJSONArgs, " "): []byte(strings.Join([]string{
				`{"Time":"2026-04-22T00:00:00Z","Action":"run","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Orchestrator_WatchSimultaneous"}`,
				`{"Time":"2026-04-22T00:01:30Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Orchestrator_WatchSimultaneous"}`,
				`{"Time":"2026-04-22T00:01:30Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e"}`,
			}, "\n")),
		},
		combinedErrByKey: map[string]error{
			"go " + strings.Join(watchSharedJSONArgs, " "): assert.AnError,
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:           repoRoot,
		Profile:            VerifyE2EFull,
		ClassifyLiveQuirks: true,
		Stdout:             &bytes.Buffer{},
		Stderr:             &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "full e2e")
	require.Len(t, runner.combinedCommands, 2)
	require.Len(t, runner.runCommands, 5)
}

func TestRunVerifyE2EStopsAfterFastPreflightFailure(t *testing.T) {
	t.Parallel()

	assertVerifyStopsAfterPreflightFailure(t, VerifyE2E, "go test -tags=e2e -run="+fastE2EPreflightPattern+" -count=1 -v ./e2e/...", 2, "preflight")
}

func TestRunVerifyE2EStopsAfterAuthPreflightFailure(t *testing.T) {
	t.Parallel()

	assertVerifyStopsAfterPreflightFailure(t, VerifyE2E, "go test -tags=e2e -run="+authE2EPreflightPattern+" -count=1 -v ./e2e/...", 1, "auth preflight")
}

func TestRunVerifyE2EFullStopsAfterFullPreflightFailure(t *testing.T) {
	t.Parallel()

	assertVerifyStopsAfterPreflightFailure(t, VerifyE2EFull, "go test -tags=e2e e2e_full -run="+fullE2EPreflightPattern+" -count=1 -v ./e2e/...", 4, "preflight")
}

func TestRunVerifyWritesSummaryJSONOnSuccess(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		SummaryJSONPath:   summaryPath,
		Stdout:            stdout,
		Stderr:            &bytes.Buffer{},
	})
	require.NoError(t, err)

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, string(VerifyPublic), summary.Profile)
	assert.Equal(t, verifySummaryStatusPass, summary.Status)
	assert.GreaterOrEqual(t, summary.TotalDurationMS, int64(0))
	assertVerifySummaryHasStep(t, summary, "format")
	assertVerifySummaryHasStep(t, summary, "repo consistency")
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
	assertVerifyBucketSummary(t, summary, fullE2ESerialSyncBucket().Name, verifySummaryStatusFail)
	assert.Contains(t, stdout.String(), "verify summary")
	assert.Contains(t, stdout.String(), fullE2ESerialSyncBucket().Name)
}

func TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	fullTests, err := discoverTaggedE2ETests(filepath.Join(repoRoot, "e2e"), "e2e && e2e_full")
	require.NoError(t, err)

	assigned := make(map[string]int)
	for _, name := range fullE2EStandaloneTests() {
		assigned[name]++
	}
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

// Validates: R-6.2.1
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

	require.Len(t, runner.runCommands, 2)
	require.Len(t, runner.combinedCommands, 1)

	assert.Equal(t, "go", runner.runCommands[0].name)
	assert.Equal(t, []string{
		"test",
		"-tags=stress",
		"-race",
		"-count=50",
		"-timeout=20m",
		"-run",
		"TestWatchOrderingStress_",
		"./internal/sync",
	}, runner.runCommands[0].args)

	assert.Equal(t, "go", runner.runCommands[1].name)
	assert.Equal(t, []string{
		"test",
		"-race",
		"-count=50",
		"-timeout=20m",
		"./internal/multisync",
	}, runner.runCommands[1].args)

	assert.Equal(t, "go", runner.combinedCommands[0].name)
	assert.Equal(t, []string{
		"test",
		"-json",
		"-race",
		"-count=50",
		"-timeout=30m",
		"./internal/sync",
	}, runner.combinedCommands[0].args)
}

func TestRunVerifyStressWritesSlowTestSummaryToStdoutAndJSON(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	summaryPath := filepath.Join(t.TempDir(), "stress-verify-summary.json")
	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"go test -json -race -count=50 -timeout=30m ./internal/sync": []byte(strings.Join([]string{
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestFast","Elapsed":0.125}`,
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestSlow","Elapsed":1.5}`,
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestSlow","Elapsed":1.0}`,
				`{"Action":"pass","Package":"github.com/tonimelisma/onedrive-go/internal/sync","Test":"TestMedium","Elapsed":0.75}`,
			}, "\n")),
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
	assert.Contains(t, stdout.String(), "TestSlow runs=2 total=2.5s max=1.5s")
	assert.Contains(t, stdout.String(), "sync race x50: pass")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	step := requireVerifySummaryStep(t, summary, "sync race x50")
	require.Len(t, step.SlowTests, 3)
	assert.Equal(t, "TestSlow", step.SlowTests[0].Test)
	assert.Equal(t, 2, step.SlowTests[0].Runs)
	assert.EqualValues(t, 2500, step.SlowTests[0].TotalElapsedMS)
	assert.EqualValues(t, 1500, step.SlowTests[0].MaxElapsedMS)
}

// Validates: R-6.2.1
func TestRunVerifyFailsOnForbiddenRepoPattern(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)
	badDir := filepath.Join(repoRoot, "internal", "bad")
	require.NoError(t, os.MkdirAll(badDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(badDir, "bad.go"),
		[]byte(strings.Join([]string{
			"package bad",
			"",
			"func forbiddenPattern() string {",
			"\treturn \"graph.MustNewClient(nil, nil)\"",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            &bytes.Buffer{},
		Stderr:            &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MustNewClient")
}

// Validates: R-6.2.1
func TestParseCoverageTotal(t *testing.T) {
	t.Parallel()

	total, err := parseCoverageTotal("pkg/foo\t80.0%\ntotal:\t(statements)\t76.2%\n")
	require.NoError(t, err)
	assert.InDelta(t, 76.2, total, 0.001)
}

// Validates: R-6.2.1
func TestRunVerifyCoverageThresholdFailure(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t75.9%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            &bytes.Buffer{},
		Stderr:            &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage gate failed")
}

// Validates: R-6.2.1
func TestResolveVerifyPlanRejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	_, err := resolveVerifyPlan("weird")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage")
}

func TestRunRepoConsistencyChecksPassesCurrentActiveDocCLIExamples(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, runRepoConsistencyChecks(repoRoot))
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
