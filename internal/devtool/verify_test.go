package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type recordedCommand struct {
	cwd  string
	env  []string
	name string
	args []string
}

type fakeRunner struct {
	runCommands         []recordedCommand
	outputCommands      []recordedCommand
	combinedCommands    []recordedCommand
	outputs             map[string][]byte
	outputsByCWD        map[string]map[string][]byte
	runErr              error
	runErrByKey         map[string]error
	runErrSequenceByKey map[string][]error
	outputErr           error
	combinedOutputs     map[string][]byte
	combinedErr         error
	combinedErrByKey    map[string]error
}

const dependencyGraphFixtureFallbackGoVersion = "1.24.0"

func (f *fakeRunner) Run(
	_ context.Context,
	cwd string,
	env []string,
	_, _ io.Writer,
	name string,
	args ...string,
) error {
	f.runCommands = append(f.runCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})

	if err, ok := f.runErrByKey[name+" "+strings.Join(args, " ")]; ok {
		return err
	}
	if seq := f.runErrSequenceByKey[name+" "+strings.Join(args, " ")]; len(seq) > 0 {
		next := seq[0]
		f.runErrSequenceByKey[name+" "+strings.Join(args, " ")] = seq[1:]
		return next
	}

	return f.runErr
}

func (f *fakeRunner) Output(
	_ context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	f.outputCommands = append(f.outputCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})
	if f.outputErr != nil {
		return nil, f.outputErr
	}

	key := name + " " + strings.Join(args, " ")
	if byCWD, ok := f.outputsByCWD[cwd]; ok {
		if out, ok := byCWD[key]; ok {
			return out, nil
		}
	}

	return f.outputs[key], nil
}

func (f *fakeRunner) CombinedOutput(
	_ context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	f.combinedCommands = append(f.combinedCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})

	key := name + " " + strings.Join(args, " ")
	if err, ok := f.combinedErrByKey[key]; ok {
		return f.combinedOutputs[key], err
	}
	if f.combinedErr != nil {
		return f.combinedOutputs[key], f.combinedErr
	}

	return f.combinedOutputs[key], nil
}

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

	require.Len(t, runner.runCommands, 8)
	assert.Equal(t, "gofumpt", runner.runCommands[0].name)
	assert.Equal(t, []string{"-w", "."}, runner.runCommands[0].args)
	assert.Equal(t, "goimports", runner.runCommands[1].name)
	assert.Equal(t, "golangci-lint", runner.runCommands[2].name)
	assert.Equal(t, "go", runner.runCommands[3].name)
	assert.Equal(t, []string{"build", "./..."}, runner.runCommands[3].args)
	assert.Equal(t, []string{"test", "-race", "-coverprofile=" + filepath.Join(repoRoot, "cover.out"), "./..."}, runner.runCommands[4].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + authE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[5].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-run=" + fastE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[6].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-race", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[7].args)
	require.Len(t, runner.outputCommands, 1)
	assert.Contains(t, stdout.String(), "==> coverage")
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
	assert.Equal(t, []string{"test", "-tags=e2e", "-race", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[2].args)
	assert.Equal(t, []string{"test", "-tags=e2e e2e_full", "-run=" + fullE2EPreflightPattern, "-count=1", "-v", "./e2e/..."}, runner.runCommands[3].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fullE2EParallelMiscBucket()), runner.runCommands[4].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fullE2ESerialSyncBucket()), runner.runCommands[5].args)
	assert.Equal(t, fullE2EBucketCommandArgs(fullE2ESerialWatchSharedBucket()), runner.runCommands[6].args)
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
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[1])
	assertCommandLacksSkipSuiteScrubEnvVar(t, runner.runCommands[2])
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

	require.Len(t, runner.runCommands, 7)
	require.Len(t, runner.combinedCommands, 1)
	assert.Equal(t, runner.runCommands[0].args, runner.runCommands[1].args)
	assert.Contains(t, stdout.String(), "LI-20260405-06")
	assert.Contains(t, stdout.String(), "classified reruns:")
}

func TestRunVerifyE2EFullClassifiesKnownFastSuiteQuirkAfterSuccessfulRerun(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"go test -json -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...": []byte(strings.Join([]string{
				`{"Time":"2026-04-08T00:00:00Z","Action":"run","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DownloadOnly"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DownloadOnly"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e"}`,
			}, "\n")),
		},
		combinedErrByKey: map[string]error{
			"go test -json -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...": assert.AnError,
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

	require.Len(t, runner.combinedCommands, 1)
	assert.Equal(t, []string{"test", "-json", "-tags=e2e", "-race", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.combinedCommands[0].args)
	require.Len(t, runner.runCommands, 7)
	assert.Equal(t, []string{"test", "-tags=e2e", "-race", "-run=^TestE2E_Sync_DownloadOnly$", "-count=1", "-v", "./e2e/..."}, runner.runCommands[2].args)
	assert.Contains(t, stdout.String(), "LI-20260405-04")
	assert.Contains(t, stdout.String(), "classified reruns:")
}

func TestRunVerifyE2EFullDoesNotMaskUnknownFastSuiteFailure(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"go test -json -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...": []byte(strings.Join([]string{
				`{"Time":"2026-04-08T00:00:00Z","Action":"run","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DeletePropagation"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e","Test":"TestE2E_Sync_DeletePropagation"}`,
				`{"Time":"2026-04-08T00:00:01Z","Action":"fail","Package":"github.com/tonimelisma/onedrive-go/e2e"}`,
			}, "\n")),
		},
		combinedErrByKey: map[string]error{
			"go test -json -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...": assert.AnError,
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
	assertVerifySummaryHasStep(t, summary, "format", verifySummaryStatusPass)
	assertVerifySummaryHasStep(t, summary, "repo consistency", verifySummaryStatusPass)
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
	assertVerifySummaryHasStep(t, summary, "fast e2e", verifySummaryStatusPass)
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

func readVerifySummaryFile(t *testing.T, path string, summary *VerifySummary) {
	t.Helper()

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, summary))
}

func assertVerifySummaryHasStep(t *testing.T, summary VerifySummary, name string, status string) {
	t.Helper()

	for _, step := range summary.Steps {
		if step.Name != name {
			continue
		}

		assert.Equal(t, status, step.Status)
		return
	}

	require.Failf(t, "missing summary step", "step %q not found", name)
}

func assertVerifyBucketSummary(t *testing.T, summary VerifySummary, name string, status string) {
	t.Helper()

	for _, bucket := range summary.E2EFullBuckets {
		if bucket.Name != name {
			continue
		}

		assert.Equal(t, status, bucket.Status)
		return
	}

	require.Failf(t, "missing bucket summary", "bucket %q not found", name)
}

func assertCommandHasEnvVar(t *testing.T, cmd recordedCommand, want string) {
	t.Helper()
	assert.Contains(t, cmd.env, want)
}

func assertCommandLacksSkipSuiteScrubEnvVar(t *testing.T, cmd recordedCommand) {
	t.Helper()
	assert.NotContains(t, cmd.env, e2eSkipSuiteScrubEnvVar+"=1")
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
		"./internal/sync",
	}, runner.runCommands[1].args)
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

// Validates: R-6.10.6
func TestRunRepoConsistencyChecksFailsWithoutOwnershipContract(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "cli.md"),
		[]byte("# CLI\n\nGOVERNS: internal/cli/*.go\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ownership Contract")
	assert.Contains(t, err.Error(), "cli.md")
}

// Validates: R-6.10.6
func TestRunRepoConsistencyChecksFailsWithoutOwnershipContractBullet(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "cli.md"),
		[]byte(strings.Join([]string{
			"# CLI",
			"",
			"GOVERNS: internal/cli/*.go",
			"",
			"## Ownership Contract",
			"- Owns: CLI entrypoints",
			"- Does Not Own: sync runtime",
			"- Source of Truth: Cobra command definitions",
			"- Allowed Side Effects: config I/O and stdout",
			"- Mutable Runtime Owner: process-local command execution",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Error Boundary")
	assert.Contains(t, err.Error(), "cli.md")
}

// Validates: R-6.10.5
func TestEnsureInternalDependencyGraphGuardrailsPassesMinimalValidGraph(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeDependencyGraphModule(t, repoRoot)
	writeDependencyGraphPackage(t, repoRoot, "driveid")
	writeDependencyGraphPackage(t, repoRoot, "synctypes", internalPackagePrefix+"driveid")
	writeDependencyGraphPackage(t, repoRoot, "sync")
	writeDependencyGraphPackage(t, repoRoot, "syncstore")
	writeDependencyGraphPackage(t, repoRoot, "cli")
	writeDependencyGraphPackage(t, repoRoot, "multisync")

	require.NoError(t, ensureInternalDependencyGraphGuardrails(repoRoot))
}

// Validates: R-6.10.5
func TestEnsureInternalDependencyGraphGuardrailsFailsOnForbiddenEdge(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeDependencyGraphModule(t, repoRoot)
	writeDependencyGraphPackage(t, repoRoot, "driveid")
	writeDependencyGraphPackage(t, repoRoot, "synctypes", internalPackagePrefix+"driveid")
	writeDependencyGraphPackage(t, repoRoot, "sync")
	writeDependencyGraphPackage(t, repoRoot, "syncstore")
	writeDependencyGraphPackage(t, repoRoot, "multisync")
	writeDependencyGraphPackage(t, repoRoot, "cli", internalPackagePrefix+"synctypes")

	err := ensureInternalDependencyGraphGuardrails(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden internal import edge detected")
	assert.Contains(t, err.Error(), internalPackagePrefix+"cli")
	assert.Contains(t, err.Error(), internalPackagePrefix+"synctypes")
}

// Validates: R-6.10.5
func TestEnsureInternalDependencyGraphGuardrailsFailsWhenSynctypesImportsExtraInternalPackage(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeDependencyGraphModule(t, repoRoot)
	writeDependencyGraphPackage(t, repoRoot, "driveid")
	writeDependencyGraphPackage(t, repoRoot, "sync")
	writeDependencyGraphPackage(t, repoRoot, "synctypes", internalPackagePrefix+"driveid", internalPackagePrefix+"sync")

	err := ensureInternalDependencyGraphGuardrails(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synctypes may only import internal/driveid")
	assert.Contains(t, err.Error(), internalPackagePrefix+"sync")
}

// Validates: R-6.10.5
func TestEnsureInternalDependencyGraphGuardrailsFailsOnPackageCountLimit(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeDependencyGraphModule(t, repoRoot)
	for i := 0; i <= internalPackageLimit; i++ {
		writeDependencyGraphPackage(t, repoRoot, fmt.Sprintf("pkg%02d", i))
	}

	err := ensureInternalDependencyGraphGuardrails(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal package graph exceeds limit")
	assert.Contains(t, err.Error(), "28 packages")
}

// Validates: R-6.10.5
func TestEnsureInternalDependencyGraphGuardrailsFailsOnImportEdgeLimit(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeDependencyGraphModule(t, repoRoot)
	for i := 0; i < 14; i++ {
		name := fmt.Sprintf("pkg%02d", i)
		var imports []string
		for j := i + 1; j < 14; j++ {
			imports = append(imports, internalPackagePrefix+fmt.Sprintf("pkg%02d", j))
		}
		writeDependencyGraphPackage(t, repoRoot, name, imports...)
	}

	err := ensureInternalDependencyGraphGuardrails(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal package graph exceeds limit")
	assert.Contains(t, err.Error(), "91 import edges")
}

// Validates: R-6.10.7
func TestRunRepoConsistencyChecksFailsWithoutCrossCuttingDesignDocReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "system.md"),
		[]byte(strings.Join([]string{
			"# System",
			"",
			"## Design Docs",
			"- [error-model.md](error-model.md)",
			"- [degraded-mode.md](degraded-mode.md)",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threat-model.md")
	assert.Contains(t, err.Error(), "system.md")
}

// Validates: R-6.10.7
func TestRunRepoConsistencyChecksFailsWithoutCrossCuttingEvidenceSection(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "error-model.md"),
		[]byte("# Error Model\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Verified By")
	assert.Contains(t, err.Error(), "error-model.md")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnMalformedValidatesReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "internal", "bad_trace_test.go"),
		[]byte(strings.Join([]string{
			"package internal",
			"",
			"import \"testing\"",
			"",
			"// Validates: D-6",
			"func TestBadTrace(t *testing.T) {}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed Validates reference")
	assert.Contains(t, err.Error(), "bad_trace_test.go")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnUnknownRequirementReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "internal", "unknown_trace_test.go"),
		[]byte(strings.Join([]string{
			"package internal",
			"",
			"import \"testing\"",
			"",
			"// Validates: R-9.9.9",
			"func TestUnknownTrace(t *testing.T) {}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown requirement ID R-9.9.9")
	assert.Contains(t, err.Error(), "unknown_trace_test.go")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnBrokenRecurringIncidentPromotedDocAnchor(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "reference", "live-incidents.md"),
		[]byte(strings.Join([]string{
			"# Live Incidents",
			"",
			"## LI-TEST-01: Sample recurring incident",
			"",
			"Recurring: yes",
			"Promoted docs: [graph-api-quirks.md#missing-anchor](graph-api-quirks.md#missing-anchor)",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing anchor #missing-anchor")
	assert.Contains(t, err.Error(), "LI-TEST-01")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnMalformedImplementsReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "sync-engine.md"),
		[]byte(strings.Join([]string{
			"# Sync Engine",
			"",
			"Implements: R-6.10.13 [verified], D-6 [verified]",
			"",
			"## Verified By",
			"",
			"| Behavior | Evidence |",
			"| --- | --- |",
			"| sample | TestFixtureEvidence |",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed Implements reference")
	assert.Contains(t, err.Error(), "sync-engine.md")
}

// Validates: R-6.10.13
func TestRunRepoConsistencyChecksFailsWhenExpandedGovernedDocMissingEvidence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		title string
	}{
		{name: "sync-control-plane.md", title: "Sync Control Plane"},
		{name: "sync-store.md", title: "Sync Store"},
		{name: "sync-observation.md", title: "Sync Observation"},
		{name: "config.md", title: "Configuration"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repoRoot := t.TempDir()
			writeRepoConsistencyFixtures(t, repoRoot)

			require.NoError(t, os.WriteFile(
				filepath.Join(repoRoot, "spec", "design", tt.name),
				[]byte(strings.Join([]string{
					"# " + tt.title,
					"",
					"GOVERNS: internal/example/*.go",
					"",
					"## Ownership Contract",
					"- Owns: sample",
					"- Does Not Own: sample",
					"- Source of Truth: sample",
					"- Allowed Side Effects: sample",
					"- Mutable Runtime Owner: sample",
					"- Error Boundary: sample",
					"",
				}, "\n")),
				0o600,
			))

			err := runRepoConsistencyChecks(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.name)
			assert.Contains(t, err.Error(), "## Verified By")
		})
	}
}

// Validates: R-6.10.13
func TestRunRepoConsistencyChecksFailsWhenEvidenceDocReferencesUnknownTest(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsUnknownEvidenceTest(t, "cli.md", []string{
		"# CLI",
		"",
		"GOVERNS: internal/cli/*.go",
		"",
		"## Ownership Contract",
		"- Owns: CLI entrypoints",
		"- Does Not Own: sync runtime",
		"- Source of Truth: Cobra command definitions",
		"- Allowed Side Effects: config I/O and stdout",
		"- Mutable Runtime Owner: process-local command execution",
		"- Error Boundary: CLI error rendering",
		"",
		"## Verified By",
		"",
		"| Behavior | Evidence |",
		"| --- | --- |",
		"| sample | TestMissingEvidence |",
		"",
	}, "TestMissingEvidence")
}

// Validates: R-6.10.13
func TestRunRepoConsistencyChecksFailsWhenExpandedGovernedDocReferencesUnknownTest(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsUnknownEvidenceTest(t, "sync-store.md", []string{
		"# Sync Store",
		"",
		"GOVERNS: internal/syncstore/*.go",
		"",
		"## Ownership Contract",
		"- Owns: sample",
		"- Does Not Own: sample",
		"- Source of Truth: sample",
		"- Allowed Side Effects: sample",
		"- Mutable Runtime Owner: sample",
		"- Error Boundary: sample",
		"",
		"## Verified By",
		"",
		"| Behavior | Evidence |",
		"| --- | --- |",
		"| sample | TestMissingStoreEvidence |",
		"",
	}, "TestMissingStoreEvidence")
}

// Validates: R-6.10.7
func TestRunRepoConsistencyChecksFailsWithoutDegradedModeIDColumn(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "degraded-mode.md"),
		[]byte("# Degraded Mode\n\n| Failure | Evidence |\n| --- | --- |\n| sample | tests |\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "| ID |")
	assert.Contains(t, err.Error(), "degraded-mode.md")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnHTTPClientDoOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	badDir := filepath.Join(repoRoot, "internal", "bad")
	require.NoError(t, os.MkdirAll(badDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(badDir, "bad_http.go"),
		[]byte(strings.Join([]string{
			"package bad",
			"",
			"import \"net/http\"",
			"",
			"type wrapper struct {",
			"\tclient *http.Client",
			"}",
			"",
			"func do(req *http.Request, w wrapper) (*http.Response, error) {",
			"\treturn w.client.Do(req)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http.Client.Do")
	assert.Contains(t, err.Error(), "bad_http.go")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksAllowsApprovedHTTPClientDoBoundary(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	graphDir := filepath.Join(repoRoot, "internal", "graph")
	require.NoError(t, os.MkdirAll(graphDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(graphDir, "client_preauth.go"),
		[]byte(strings.Join([]string{
			"package graph",
			"",
			"import \"net/http\"",
			"",
			"type client struct {",
			"\thttpClient *http.Client",
			"}",
			"",
			"func (c *client) do(req *http.Request) (*http.Response, error) {",
			"\treturn c.httpClient.Do(req)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	require.NoError(t, runRepoConsistencyChecks(repoRoot))
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnCLIProcessGlobalOutput(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	cliDir := filepath.Join(repoRoot, "internal", "cli")
	require.NoError(t, os.MkdirAll(cliDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(cliDir, "bad_output.go"),
		[]byte(strings.Join([]string{
			"package cli",
			"",
			"import (",
			"\t\"fmt\"",
			"\t\"os\"",
			")",
			"",
			"func badOutput() {",
			"\tfmt.Fprintln(os.Stderr, \"oops\")",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cli output boundary violation")
	assert.Contains(t, err.Error(), "bad_output.go")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnExecCommandContextOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_exec.go", []string{
		"package bad",
		"",
		"import (",
		"\t\"context\"",
		"\t\"os/exec\"",
		")",
		"",
		"func run(ctx context.Context) error {",
		"\treturn exec.CommandContext(ctx, \"echo\", \"nope\").Run()",
		"}",
		"",
	}, "exec.CommandContext")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnExecCommandOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_exec_command.go", []string{
		"package bad",
		"",
		"import \"os/exec\"",
		"",
		"func run() error {",
		"\treturn exec.Command(\"echo\", \"nope\").Run()",
		"}",
		"",
	}, "exec.Command")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnSQLOpenOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_sql.go", []string{
		"package bad",
		"",
		"import \"database/sql\"",
		"",
		"func open() (*sql.DB, error) {",
		"\treturn sql.Open(\"sqlite\", \"file:test.db\")",
		"}",
		"",
	}, "sql.Open")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnSignalNotifyOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_signal.go", []string{
		"package bad",
		"",
		"import (",
		"\t\"os\"",
		"\t\"os/signal\"",
		")",
		"",
		"func watch(ch chan os.Signal) {",
		"\tsignal.Notify(ch)",
		"}",
		"",
	}, "signal.Notify")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnSignalStopOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_signal_stop.go", []string{
		"package bad",
		"",
		"import (",
		"\t\"os\"",
		"\t\"os/signal\"",
		")",
		"",
		"func watch(ch chan os.Signal) {",
		"\tsignal.Stop(ch)",
		"}",
		"",
	}, "signal.Stop")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnOSExitOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_exit.go", []string{
		"package bad",
		"",
		"import \"os\"",
		"",
		"func exitNow() {",
		"\tos.Exit(1)",
		"}",
		"",
	}, "os.Exit")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksIgnoresTestSupportOSExitOutsideProductionRoots(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	testutilDir := filepath.Join(repoRoot, "testutil")
	require.NoError(t, os.MkdirAll(testutilDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(testutilDir, "testenv.go"),
		[]byte(strings.Join([]string{
			"package testutil",
			"",
			"import \"os\"",
			"",
			"func fatal() {",
			"\tos.Exit(1)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.NoError(t, err)
}

func assertRepoConsistencyRejectsPrivilegedCall(
	t *testing.T,
	filename string,
	source []string,
	want string,
) {
	t.Helper()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	badDir := filepath.Join(repoRoot, "internal", "bad")
	require.NoError(t, os.MkdirAll(badDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(badDir, filename),
		[]byte(strings.Join(source, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), want)
	assert.Contains(t, err.Error(), filename)
}

func assertRepoConsistencyRejectsUnknownEvidenceTest(
	t *testing.T,
	docName string,
	docLines []string,
	missingTest string,
) {
	t.Helper()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", docName),
		[]byte(strings.Join(docLines, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown test function "+missingTest)
	assert.Contains(t, err.Error(), docName)
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnRawOSFilesystemCallInGuardedPackage(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	syncDir := filepath.Join(repoRoot, "internal", "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(syncDir, "bad_fs.go"),
		[]byte(strings.Join([]string{
			"package sync",
			"",
			"import \"os\"",
			"",
			"func badFS(path string) error {",
			"\t_, err := os.Stat(path)",
			"\treturn err",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw os filesystem call detected")
	assert.Contains(t, err.Error(), "bad_fs.go")
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnStaleFilterSemanticsWording(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	requirementsDir := filepath.Join(repoRoot, "spec", "requirements")
	require.NoError(t, os.MkdirAll(requirementsDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(requirementsDir, "sync.md"),
		[]byte("Filter settings are per-drive only.\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale filter semantics wording")
	assert.Contains(t, err.Error(), "sync.md")
}

func TestRunRepoConsistencyChecksFailsOnUnknownActiveDocCLIExample(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "cli.md"),
		[]byte(strings.Join([]string{
			"# CLI",
			"",
			"GOVERNS: internal/cli/*.go",
			"",
			"## Ownership Contract",
			"- Owns: CLI entrypoints",
			"- Does Not Own: sync runtime",
			"- Source of Truth: Cobra command definitions",
			"- Allowed Side Effects: config I/O and stdout",
			"- Mutable Runtime Owner: process-local command execution",
			"- Error Boundary: CLI error rendering",
			"",
			"## Verified By",
			"",
			"| Behavior | Evidence |",
			"| --- | --- |",
			"| stale | TestFixtureEvidence |",
			"",
			"Run `onedrive-go madeup`.",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid documented CLI example")
	assert.Contains(t, err.Error(), "madeup")
	assert.Contains(t, err.Error(), "cli.md")
}

func TestRunRepoConsistencyChecksPassesCurrentActiveDocCLIExamples(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, runRepoConsistencyChecks(repoRoot))
}

func writeRepoConsistencyFixtures(t *testing.T, repoRoot string) {
	t.Helper()

	writeRepoConsistencyDirectories(t, repoRoot)
	writeRepoConsistencyRequirements(t, repoRoot)
	writeRepoConsistencyDesignDocs(t, repoRoot)
	writeRepoConsistencyReferenceDocs(t, repoRoot)
	writeRepoConsistencyCodeFixtures(t, repoRoot)
}

func writeDependencyGraphModule(t *testing.T, repoRoot string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, "internal"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "go.mod"),
		[]byte(fmt.Sprintf("module github.com/tonimelisma/onedrive-go\n\ngo %s\n", dependencyGraphFixtureGoVersion())),
		0o600,
	))
}

func dependencyGraphFixtureGoVersion() string {
	version := strings.TrimPrefix(runtime.Version(), "go")
	if version == runtime.Version() || version == "" {
		return dependencyGraphFixtureFallbackGoVersion
	}

	for _, r := range version {
		if (r < '0' || r > '9') && r != '.' {
			return dependencyGraphFixtureFallbackGoVersion
		}
	}

	return version
}

func writeDependencyGraphPackage(t *testing.T, repoRoot string, name string, imports ...string) {
	t.Helper()

	dir := filepath.Join(repoRoot, "internal", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))

	lines := []string{"package " + name}
	if len(imports) > 0 {
		lines = append(lines, "", "import (")
		for _, path := range imports {
			lines = append(lines, fmt.Sprintf("\t_ %q", path))
		}
		lines = append(lines, ")")
	}
	lines = append(lines, "")

	require.NoError(t, os.WriteFile(
		filepath.Join(dir, name+".go"),
		[]byte(strings.Join(lines, "\n")),
		0o600,
	))
}

func writeRepoConsistencyDirectories(t *testing.T, repoRoot string) {
	t.Helper()

	for _, dir := range []string{
		filepath.Join(repoRoot, "e2e"),
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "spec", "reference"),
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, ".github", "workflows"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
	}

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "go.mod"),
		[]byte(fmt.Sprintf("module github.com/tonimelisma/onedrive-go\n\ngo %s\n", dependencyGraphFixtureGoVersion())),
		0o600,
	))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "CLAUDE.md"), []byte("clean\n"), 0o600))
}

func writeRepoConsistencyRequirements(t *testing.T, repoRoot string) {
	t.Helper()

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "requirements", "non-functional.md"),
		[]byte(strings.Join([]string{
			"# R-6 Non-Functional",
			"",
			"## R-6.2 Data Integrity [verified]",
			"- R-6.2.1: sample [verified]",
			"- R-6.2.2: sample [verified]",
			"",
			"## R-6.10 Verification [verified]",
			"- R-6.10.5: sample [verified]",
			"- R-6.10.6: sample [verified]",
			"- R-6.10.7: sample [verified]",
			"- R-6.10.11: sample [verified]",
			"- R-6.10.12: sample [verified]",
			"- R-6.10.13: sample [verified]",
			"",
		}, "\n")),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "requirements", "sync.md"),
		[]byte(strings.Join([]string{
			"# R-2 Sync",
			"",
			"## R-2.8 Watch Mode Behavior [verified]",
			"- R-2.8.3: sample [verified]",
			"",
			"## R-2.10 Failure Management [verified]",
			"- R-2.10.41: sample [verified]",
			"",
		}, "\n")),
		0o600,
	))
}

func writeRepoConsistencyDesignDocs(t *testing.T, repoRoot string) {
	t.Helper()

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "system.md"),
		[]byte(strings.Join([]string{
			"# System",
			"",
			"## Design Docs",
			"- [error-model.md](error-model.md)",
			"- [threat-model.md](threat-model.md)",
			"- [degraded-mode.md](degraded-mode.md)",
			"",
		}, "\n")),
		0o600,
	))
	for _, doc := range repoConsistencyDesignDocFixtures() {
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "spec", "design", doc.name), []byte(doc.content), 0o600))
	}
}

func writeRepoConsistencyReferenceDocs(t *testing.T, repoRoot string) {
	t.Helper()

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "reference", "graph-api-quirks.md"),
		[]byte(strings.Join([]string{
			"# Graph API Quirks",
			"",
			`<a id="fixture-quirk"></a>`,
			"### Fixture Quirk",
			"",
			"Sample quirk.",
			"",
		}, "\n")),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "reference", "live-incidents.md"),
		[]byte(strings.Join([]string{
			"# Live Incidents",
			"",
			"## LI-TEST-01: Sample recurring incident",
			"",
			"Recurring: yes",
			"Promoted docs: [graph-api-quirks.md#fixture-quirk](graph-api-quirks.md#fixture-quirk)",
			"",
		}, "\n")),
		0o600,
	))
}

func repoConsistencyDesignDocFixtures() []struct {
	name    string
	content string
} {
	return []struct {
		name    string
		content string
	}{
		{
			name:    "error-model.md",
			content: "# Error Model\n\n## Verified By\n\n| Boundary | Evidence |\n| --- | --- |\n| sample | TestFixtureEvidence |\n",
		},
		{
			name:    "threat-model.md",
			content: "# Threat Model\n\n## Mitigation Evidence\n\n| Mitigation | Evidence |\n| --- | --- |\n| sample | TestFixtureEvidence |\n",
		},
		{
			name:    "degraded-mode.md",
			content: "# Degraded Mode\n\n| ID | Failure | Evidence |\n| --- | --- | --- |\n| DM-1 | sample | TestFixtureEvidence |\n",
		},
		repoConsistencyBehaviorDocFixture("cli.md", "CLI", "internal/cli/*.go", []string{
			"- Owns: CLI entrypoints",
			"- Does Not Own: sync runtime",
			"- Source of Truth: Cobra command definitions",
			"- Allowed Side Effects: config I/O and stdout",
			"- Mutable Runtime Owner: process-local command execution",
			"- Error Boundary: CLI error rendering",
		}, []string{
			"Run `onedrive-go status`.",
			"Run `onedrive-go --drive <id> resolve deletes` after reviewing held deletes.",
			"Run `onedrive-go --drive <id> resolve local <target>` to queue a specific conflict choice.",
			"Run 'onedrive-go --drive <id> recover' when the state DB is damaged.",
		}),
		{
			name: "sync-engine.md",
			content: strings.Join([]string{
				"# Sync Engine",
				"",
				"## Verified By",
				"",
				"| Behavior | Evidence |",
				"| --- | --- |",
				"| sample | TestFixtureEvidence |",
				"",
			}, "\n"),
		},
		repoConsistencyBehaviorDocFixture("sync-execution.md", "Sync Execution", "internal/sync/*.go", []string{
			"- Owns: action execution",
			"- Does Not Own: planning",
			"- Source of Truth: executor config",
			"- Allowed Side Effects: transfer execution",
			"- Mutable Runtime Owner: worker pool",
			"- Error Boundary: worker results",
		}, nil),
		repoConsistencyBehaviorDocFixture("sync-control-plane.md", "Sync Control Plane", "internal/multisync/*.go", []string{
			"- Owns: multi-drive lifecycle",
			"- Does Not Own: single-drive execution",
			"- Source of Truth: config holder",
			"- Allowed Side Effects: orchestrator startup",
			"- Mutable Runtime Owner: watch orchestrator",
			"- Error Boundary: drive reports",
		}, nil),
		repoConsistencyBehaviorDocFixture("sync-store.md", "Sync Store", "internal/syncstore/*.go", []string{
			"- Owns: sqlite sync state",
			"- Does Not Own: graph calls",
			"- Source of Truth: sqlite rows",
			"- Allowed Side Effects: sqlite reads and writes",
			"- Mutable Runtime Owner: sync store handles",
			"- Error Boundary: persisted failure facts",
		}, nil),
		repoConsistencyBehaviorDocFixture("sync-observation.md", "Sync Observation", "internal/sync/*.go", []string{
			"- Owns: change observation",
			"- Does Not Own: planning",
			"- Source of Truth: local and remote observation inputs",
			"- Allowed Side Effects: filesystem and graph observation",
			"- Mutable Runtime Owner: observers and buffer",
			"- Error Boundary: change events and skipped items",
		}, nil),
		repoConsistencyBehaviorDocFixture("config.md", "Configuration", "internal/config/*.go", []string{
			"- Owns: config loading",
			"- Does Not Own: graph calls",
			"- Source of Truth: resolved config snapshot",
			"- Allowed Side Effects: config and metadata IO",
			"- Mutable Runtime Owner: config holder",
			"- Error Boundary: load and validation outcomes",
		}, nil),
	}
}

func repoConsistencyBehaviorDocFixture(
	name string,
	title string,
	governs string,
	ownership []string,
	examples []string,
) struct {
	name    string
	content string
} {
	lines := []string{
		"# " + title,
		"",
		"GOVERNS: " + governs,
		"",
		"## Ownership Contract",
	}
	lines = append(lines, ownership...)
	lines = append(lines,
		"",
		"## Verified By",
		"",
		"| Behavior | Evidence |",
		"| --- | --- |",
		"| sample | TestFixtureEvidence |",
		"",
	)
	lines = append(lines, examples...)
	if len(examples) > 0 {
		lines = append(lines, "")
	}

	return struct {
		name    string
		content string
	}{
		name:    name,
		content: strings.Join(lines, "\n"),
	}
}

func writeRepoConsistencyCodeFixtures(t *testing.T, repoRoot string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "internal", "clean.go"), []byte("package internal\n"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "internal", "fixture_test.go"),
		[]byte(strings.Join([]string{
			"package internal",
			"",
			"import \"testing\"",
			"",
			"// Validates: R-6.2.1",
			"func TestFixtureEvidence(t *testing.T) {}",
			"",
		}, "\n")),
		0o600,
	))
}

// Ensure the fake runner still satisfies the commandRunner contract if the
// signatures evolve.
var _ commandRunner = (*fakeRunner)(nil)
