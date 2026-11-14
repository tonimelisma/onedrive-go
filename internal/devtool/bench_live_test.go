package devtool

import (
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
func TestBenchScopeRootRelativePathValidatesInputs(t *testing.T) {
	t.Parallel()

	relative, err := benchScopeRootRelativePath("/benchmarks/catchup-scope")
	require.NoError(t, err)
	assert.Equal(t, "benchmarks/catchup-scope", relative)

	_, err = benchScopeRootRelativePath("/")
	require.Error(t, err)

	_, err = benchScopeRootRelativePath("relative/path")
	require.Error(t, err)
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
				return benchMeasuredProcess{Stdout: []byte("other-scope\nbenchmarks\ntest")}, nil
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
//
// A sample that succeeded but leaves a runtime root it cannot remove is a
// fixture failure; one that already failed keeps its own classification.
func TestFinalizeBenchLiveSampleRuntimeReportsARootItCannotRemove(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}

	parent := t.TempDir()
	runtimeRoot := filepath.Join(parent, "runtime")
	require.NoError(t, mkdirAll(filepath.Join(runtimeRoot, "sync-root"), 0o700))
	//nolint:gosec // directories need execute bits; the test makes the parent read-only on purpose.
	require.NoError(t, os.Chmod(parent, 0o500))
	t.Cleanup(func() {
		//nolint:gosec // directories need execute bits; 0700 restores the private test directory.
		assert.NoError(t, os.Chmod(parent, 0o700))
	})

	succeeded := benchSample{Phase: benchSamplePhaseMeasured, Iteration: 1, Status: BenchSampleSuccess}
	result := finalizeBenchLiveSampleRuntime(runtimeRoot, succeeded)
	assert.Equal(t, BenchSampleFixtureFailed, result.Status)
	assert.Contains(t, result.FailureExcerpt, "cleanup sample runtime")

	failed := benchSample{Phase: benchSamplePhaseMeasured, Iteration: 1, Status: BenchSampleInvalid}
	assert.Equal(t, failed, finalizeBenchLiveSampleRuntime(runtimeRoot, failed))
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
