package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.14
func TestLoadBenchLiveFixturePlanIsDeterministicAndSized(t *testing.T) {
	t.Parallel()

	planA, err := loadSyncPartialLocalCatchup100MFixturePlan()
	require.NoError(t, err)

	planB, err := loadSyncPartialLocalCatchup100MFixturePlan()
	require.NoError(t, err)

	assert.Equal(t, planA, planB)
	assert.Equal(t, syncPartialLocalCatchup100MID, planA.Manifest.ScenarioID)
	assert.Equal(t, "benchmarks/sync-partial-local-catchup-100m", planA.ScopeRootRelativePath)
	assert.Equal(t, 2668, planA.Denominators.FileCount)
	assert.Equal(t, 113, planA.Denominators.DirectoryCount)
	assert.Equal(t, int64(100<<20), sumBenchLiveFixtureBytes(planA.Files))
	assert.Equal(t, sumBenchLiveFixtureBytes(planA.Files), int64(100<<20))
	assert.Len(t, planA.Directories, planA.Denominators.DirectoryCount)
	assert.Equal(t, 192, planA.Denominators.ChangedItemCount)
	assert.Equal(t, 192, planA.Denominators.ExpectedTransfers)
	assert.Zero(t, planA.Denominators.ExpectedDeletes)
}

// Validates: R-6.10.14
func TestLoadBenchLiveFixturePlanMutationSelectionIsStable(t *testing.T) {
	t.Parallel()

	plan, err := loadSyncPartialLocalCatchup100MFixturePlan()
	require.NoError(t, err)

	assert.Len(t, plan.Mutations.Deletes, 96)
	assert.Len(t, plan.Mutations.Truncates, 96)

	deleted := make(map[string]struct{}, len(plan.Mutations.Deletes))
	for _, entry := range plan.Mutations.Deletes {
		deleted[entry.File.RelativePath] = struct{}{}
	}
	for _, entry := range plan.Mutations.Truncates {
		_, overlap := deleted[entry.File.RelativePath]
		assert.False(t, overlap, "truncate list should not overlap deletes")
		assert.Zero(t, entry.TruncateToBytes)
	}

	assert.Equal(t, "assets/tier-00/set-00/assets-00000.bin", plan.Mutations.Deletes[0].File.RelativePath)
	assert.Equal(t, "assets/tier-00/set-00/assets-00007.bin", plan.Mutations.Truncates[0].File.RelativePath)
}

// Validates: R-6.10.14
func TestPerturbBenchLiveFixtureAppliesDeletesAndTruncates(t *testing.T) {
	t.Parallel()

	scopeRoot := t.TempDir()
	files := []benchLiveFileEntry{
		{RelativePath: "docs/a.txt", SizeBytes: 64},
		{RelativePath: "docs/b.txt", SizeBytes: 64},
		{RelativePath: "media/c.bin", SizeBytes: 128},
	}
	require.NoError(t, materializeBenchLiveFixture(scopeRoot, files))

	err := perturbBenchLiveFixture(scopeRoot, benchLiveMutationPlan{
		Deletes: []benchLiveMutationEntry{
			{File: files[0]},
		},
		Truncates: []benchLiveMutationEntry{
			{File: files[2], TruncateToBytes: 0},
		},
	})
	require.NoError(t, err)

	_, err = stat(filepath.Join(scopeRoot, "docs", "a.txt"))
	require.Error(t, err)

	info, err := stat(filepath.Join(scopeRoot, "media", "c.bin"))
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

// Validates: R-6.10.14
func TestPrepareBenchScenarioUsesPreparedRunnerAndCleanup(t *testing.T) {
	t.Parallel()

	cleanupCalled := false
	definition := benchScenarioDefinition{
		Spec: BenchScenarioSpec{ID: "prepared"},
		Prepare: func(context.Context, benchScenarioPrepareRequest) (preparedBenchScenario, error) {
			return preparedBenchScenario{
				run: func(_ context.Context, _ preparedBenchSubject, phase benchSamplePhase, iteration int) benchSample {
					return benchSample{
						Iteration: iteration,
						Phase:     phase,
						Status:    BenchSampleSuccess,
					}
				},
				cleanup: func() error {
					cleanupCalled = true
					return nil
				},
			}, nil
		},
	}

	prepared, err := prepareBenchScenario(t.Context(), &definition, t.TempDir(), preparedBenchSubject{}, "")
	require.NoError(t, err)

	samples := collectBenchSamples(t.Context(), prepared.run, preparedBenchSubject{}, 0, 1, nil)
	require.Len(t, samples, 1)
	assert.Equal(t, BenchSampleSuccess, samples[0].Status)

	warnBenchCleanup(io.Discard, "scenario", prepared.cleanup)
	assert.True(t, cleanupCalled)
}

// Validates: R-6.10.14
func TestLiveCatchupScenarioMissingPrerequisitesReturnsFixtureFailure(t *testing.T) {
	t.Setenv("ONEDRIVE_TEST_DRIVE", "")
	t.Setenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS", "")
	t.Setenv("ONEDRIVE_TEST_DRIVE_2", "")

	scenario, err := lookupSyncPartialLocalCatchup100MScenario()
	require.NoError(t, err)

	prepared, err := prepareBenchScenario(t.Context(), &scenario, t.TempDir(), preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			return benchMeasuredProcess{}, nil
		},
	}, "")
	require.NoError(t, err)

	sample := prepared.run(t.Context(), preparedBenchSubject{}, benchSamplePhaseMeasured, 1)
	assert.Equal(t, BenchSampleFixtureFailed, sample.Status)
	assert.Contains(t, sample.FailureExcerpt, "ONEDRIVE_TEST_DRIVE not set")

	var stderr bytes.Buffer
	warnBenchCleanup(&stderr, "scenario", prepared.cleanup)
	assert.Empty(t, stderr.String())
}

// Validates: R-6.10.14
func TestBenchScopeRootRelativePathValidatesInputs(t *testing.T) {
	t.Parallel()

	relative, err := benchScopeRootRelativePath("/benchmarks/sync-partial-local-catchup-100m")
	require.NoError(t, err)
	assert.Equal(t, "benchmarks/sync-partial-local-catchup-100m", relative)

	_, err = benchScopeRootRelativePath("/")
	require.Error(t, err)

	_, err = benchScopeRootRelativePath("relative/path")
	require.Error(t, err)
}

// Validates: R-6.10.14
func TestCreateBenchLiveRuntimeCopiesCredentialsAndWritesConfig(t *testing.T) {
	t.Parallel()

	workRoot := t.TempDir()
	credentialDir := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(credentialDir, "token_personal_user@example.com.json"),
		[]byte(`{"token":"x"}`),
	))

	runtime, err := createBenchLiveRuntime(
		workRoot,
		credentialDir,
		"personal:user@example.com",
		"",
	)
	require.NoError(t, err)

	dataRoot := filepath.Join(benchRuntimeEnvValue(runtime.env, "XDG_DATA_HOME"), "onedrive-go")
	_, err = stat(filepath.Join(dataRoot, "token_personal_user@example.com.json"))
	require.NoError(t, err)
	configBody, err := readFile(runtime.configPath)
	require.NoError(t, err)
	assert.Contains(t, string(configBody), `["personal:user@example.com"]`)
	assert.Contains(t, string(configBody), runtime.syncDir)
}

// Validates: R-6.10.14
func TestResetBenchRemoteScopeRetriesUntilSuccessOrNotFound(t *testing.T) {
	t.Parallel()

	callCount := 0
	subject := preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			callCount++
			if callCount == 1 {
				return benchMeasuredProcess{Stderr: []byte("graph: HTTP 503")}, assert.AnError
			}

			return benchMeasuredProcess{Stderr: []byte("not found")}, assert.AnError
		},
	}

	err := resetBenchRemoteScope(t.Context(), subject, benchLiveCommandRuntime{}, "/benchmarks/test")
	require.NoError(t, err)
	assert.Equal(t, 2, callCount)
}

// Validates: R-6.10.14
func TestWaitForBenchRemoteScopeVisibleRetriesTransientFailures(t *testing.T) {
	t.Parallel()

	callSpecs := make([][]string, 0, 4)
	subject := preparedBenchSubject{
		measure: func(_ context.Context, spec benchCommandSpec) (benchMeasuredProcess, error) {
			callSpecs = append(callSpecs, append([]string(nil), spec.Arg...))
			switch len(callSpecs) {
			case 1:
				assert.Equal(t, []string{"--config", "", "--drive", "", "stat", "--json", "/benchmarks/test"}, spec.Arg)
				return benchMeasuredProcess{Stderr: []byte("not found")}, assert.AnError
			case 2:
				assert.Equal(t, []string{"--config", "", "--drive", "", "ls", "/benchmarks"}, spec.Arg)
				return benchMeasuredProcess{Stdout: []byte("sync-partial-local-catchup-100m\nbenchmarks\ntest")}, nil
			default:
				require.Failf(t, "unexpected extra call", "spec.Arg=%v", spec.Arg)
				return benchMeasuredProcess{}, nil
			}
		},
	}

	err := waitForBenchRemoteScopeVisible(t.Context(), subject, benchLiveCommandRuntime{
		configPath: "",
		driveID:    "",
	}, "/benchmarks/test")
	require.NoError(t, err)
	assert.Len(t, callSpecs, 2)
}

// Validates: R-6.10.14
func TestWaitForBenchRemoteScopeVisibleRequiresExactScopeNameMatch(t *testing.T) {
	t.Parallel()

	callCount := 0
	subject := preparedBenchSubject{
		measure: func(_ context.Context, spec benchCommandSpec) (benchMeasuredProcess, error) {
			callCount++
			switch callCount {
			case 1:
				assert.Equal(t, []string{"--config", "", "--drive", "", "stat", "--json", "/benchmarks/test"}, spec.Arg)
				return benchMeasuredProcess{Stderr: []byte("not found")}, assert.AnError
			case 2:
				assert.Equal(t, []string{"--config", "", "--drive", "", "ls", "/benchmarks"}, spec.Arg)
				return benchMeasuredProcess{Stdout: []byte("test-backup\nother-entry\n")}, nil
			case 3:
				assert.Equal(t, []string{"--config", "", "--drive", "", "stat", "--json", "/benchmarks/test"}, spec.Arg)
				return benchMeasuredProcess{}, nil
			default:
				require.Failf(t, "unexpected extra call", "spec.Arg=%v", spec.Arg)
				return benchMeasuredProcess{}, nil
			}
		},
	}

	err := waitForBenchRemoteScopeVisible(t.Context(), subject, benchLiveCommandRuntime{
		configPath: "",
		driveID:    "",
	}, "/benchmarks/test")
	require.NoError(t, err)
	assert.Equal(t, 3, callCount, "a sibling name should not satisfy scope visibility")
}

// Validates: R-6.10.14
func TestClassifyBenchVisibilityErrorAllowsRetryableFamilies(t *testing.T) {
	t.Parallel()

	assert.NoError(t, classifyBenchVisibilityError([]byte("not found"), assert.AnError))
	assert.NoError(t, classifyBenchVisibilityError([]byte("graph: HTTP 503"), assert.AnError))
	require.Error(t, classifyBenchVisibilityError([]byte("permission denied"), assert.AnError))
}

// Validates: R-6.10.14
func TestPrepareAndMeasureCatchupSampleRestoresFixtureAndReadsPerfSummary(t *testing.T) {
	t.Parallel()

	workRoot := t.TempDir()
	scopeRelativePath := "benchmarks/test-scope"
	fixture := &benchLiveFixturePlan{
		ScopeRootRelativePath: scopeRelativePath,
		Files: []benchLiveFileEntry{
			{RelativePath: "docs/a.txt", SizeBytes: 64},
			{RelativePath: "docs/b.txt", SizeBytes: 64},
		},
		Directories: []string{"docs"},
		Mutations: benchLiveMutationPlan{
			Deletes: []benchLiveMutationEntry{
				{File: benchLiveFileEntry{RelativePath: "docs/a.txt", SizeBytes: 64}},
			},
			Truncates: []benchLiveMutationEntry{
				{File: benchLiveFileEntry{RelativePath: "docs/b.txt", SizeBytes: 64}, TruncateToBytes: 0},
			},
		},
	}

	runtimeRoot := filepath.Join(workRoot, "runtime")
	syncRoot := filepath.Join(runtimeRoot, "sync-root")
	scopeRoot := filepath.Join(syncRoot, filepath.FromSlash(scopeRelativePath))
	require.NoError(t, materializeBenchLiveFixture(scopeRoot, fixture.Files))

	logPath := filepath.Join(runtimeRoot, "bench.log")
	require.NoError(t, mkdirAll(runtimeRoot, 0o700))
	require.NoError(t, writeFile(logPath, []byte{}))

	state := &benchLiveScenarioState{
		fixture: *fixture,
	}
	baselineCalls := 0
	subject := preparedBenchSubject{
		measure: func(_ context.Context, _ benchCommandSpec) (benchMeasuredProcess, error) {
			baselineCalls++
			if baselineCalls == 1 {
				return benchMeasuredProcess{}, nil
			}

			require.NoError(t, materializeBenchLiveFixture(scopeRoot, fixture.Files))
			require.NoError(t, writeBenchLivePerfLog(logPath, &benchPerfSummary{
				DurationMS:      42,
				Result:          "success",
				DownloadCount:   2,
				DownloadBytes:   128,
				RefreshRunCount: 1,
			}))
			return benchMeasuredProcess{
				ElapsedMicros: 1500,
				PeakRSSBytes:  8192,
			}, nil
		},
	}

	runtime := benchLiveCommandRuntime{
		rootDir: runtimeRoot,
		syncDir: syncRoot,
		logPath: logPath,
	}
	sample := benchSample{Phase: benchSamplePhaseMeasured, Iteration: 1, Status: BenchSampleSuccess}

	preparedSample, ok := state.prepareMeasuredCatchup(t.Context(), subject, runtime, scopeRoot, sample)
	require.True(t, ok)

	result := state.measureCatchupSample(t.Context(), subject, runtime, scopeRoot, preparedSample)
	assert.Equal(t, BenchSampleSuccess, result.Status)
	require.NotNil(t, result.PerfSummary)
	assert.EqualValues(t, 42, result.PerfSummary.DurationMS)
	assert.Equal(t, 2, result.PerfSummary.DownloadCount)
	assert.EqualValues(t, 1500, result.ElapsedMicros)
}

// Validates: R-6.10.14
func TestRunSampleReturnsFixtureFailureWhenRuntimeCleanupFails(t *testing.T) {
	repoRoot := t.TempDir()
	credentialDir := filepath.Join(repoRoot, ".testdata")
	require.NoError(t, mkdirAll(credentialDir, 0o700))
	require.NoError(t, writeFile(
		filepath.Join(credentialDir, "token_personal_user@example.com.json"),
		[]byte(`{"token":"x"}`),
	))
	t.Setenv("ONEDRIVE_TEST_DRIVE", "personal:user@example.com")
	t.Setenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS", "personal:user@example.com")
	t.Setenv("ONEDRIVE_TEST_DRIVE_2", "")

	fixture := benchLiveFixturePlan{
		Manifest: benchLiveFixtureManifest{
			RemoteScopePath: "/benchmarks/test-scope",
		},
		ScopeRootRelativePath: "benchmarks/test-scope",
		Files: []benchLiveFileEntry{
			{RelativePath: "docs/a.txt", SizeBytes: 64},
		},
		Directories: []string{"docs"},
		Mutations: benchLiveMutationPlan{
			Deletes: []benchLiveMutationEntry{
				{File: benchLiveFileEntry{RelativePath: "docs/a.txt", SizeBytes: 64}},
			},
		},
	}

	state := &benchLiveScenarioState{
		repoRoot: repoRoot,
		fixture:  fixture,
		workRoot: t.TempDir(),
	}
	state.setupOnce.Do(func() {})

	var runtimeRoot string
	callCount := 0
	subject := preparedBenchSubject{
		measure: func(_ context.Context, spec benchCommandSpec) (benchMeasuredProcess, error) {
			callCount++
			runtimeRoot = spec.CWD
			scopeRoot := filepath.Join(spec.CWD, "sync-root", filepath.FromSlash(fixture.ScopeRootRelativePath))
			logPath := filepath.Join(spec.CWD, "bench.log")

			require.Equal(
				t,
				[]string{"--config", filepath.Join(spec.CWD, "config.toml"), "--drive", "personal:user@example.com", "sync", "--download-only"},
				spec.Arg,
			)

			if callCount == 1 {
				require.NoError(t, materializeBenchLiveFixture(scopeRoot, fixture.Files))
				return benchMeasuredProcess{}, nil
			}
			require.Equal(t, 2, callCount, "runSample should only measure baseline and catchup once each")

			require.NoError(t, materializeBenchLiveFixture(scopeRoot, fixture.Files))
			require.NoError(t, writeBenchLivePerfLog(logPath, &benchPerfSummary{
				DurationMS:      42,
				Result:          "success",
				DownloadCount:   1,
				DownloadBytes:   64,
				RefreshRunCount: 1,
			}))
			//nolint:gosec // directories need execute bits; this test intentionally makes the directory read-only.
			require.NoError(t, os.Chmod(spec.CWD, 0o500))

			return benchMeasuredProcess{
				ElapsedMicros: 1234,
				PeakRSSBytes:  8192,
			}, nil
		},
	}

	sample := state.runSample(t.Context(), subject, benchSamplePhaseMeasured, 1)
	assert.Equal(t, BenchSampleFixtureFailed, sample.Status)
	assert.Contains(t, sample.FailureExcerpt, "cleanup sample runtime")

	if runtimeRoot != "" {
		if _, err := stat(runtimeRoot); err == nil {
			//nolint:gosec // directory permissions require execute bits; 0700 is the intended private runtime root.
			require.NoError(t, os.Chmod(runtimeRoot, 0o700))
			require.NoError(t, removeAll(runtimeRoot))
		}
	}
}

func sumBenchLiveFixtureBytes(files []benchLiveFileEntry) int64 {
	var total int64
	for _, entry := range files {
		total += entry.SizeBytes
	}

	return total
}

func writeBenchLivePerfLog(path string, snapshot *benchPerfSummary) error {
	record := struct {
		Msg string `json:"msg"`
		benchPerfSummary
	}{
		Msg:              "performance summary",
		benchPerfSummary: *snapshot,
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal perf log record: %w", err)
	}

	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write perf log file %s: %w", path, err)
	}

	return nil
}

func benchRuntimeEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}

	return ""
}
