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
	"time"

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

// recordedBenchClock is a retry clock that records each wait and returns at
// once, moving its own time on by the wait, so a retry test takes no wall
// time and a deadline is reached by the waits alone.
type recordedBenchClock struct {
	at    time.Time
	waits []time.Duration
}

func (c *recordedBenchClock) clock() benchRetryClock {
	return benchRetryClock{
		now: func() time.Time { return c.at },
		wait: func(_ context.Context, d time.Duration) error {
			c.waits = append(c.waits, d)
			c.at = c.at.Add(d)

			return nil
		},
	}
}

// benchServerProblemStderr is what a command prints for a 5xx answer
// without --verbose: the CLI's error line alone.
const benchServerProblemStderr = cliErrorLinePrefix + cliServerProblemMessage + "\n"

// Validates: R-6.10.14
func TestResetBenchRemoteScopeRetriesUntilSuccessOrNotFound(t *testing.T) {
	t.Parallel()

	callCount := 0
	clock := &recordedBenchClock{}
	subject := preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			callCount++
			if callCount == 1 {
				return benchMeasuredProcess{Stderr: []byte(benchServerProblemStderr)}, assert.AnError
			}

			return benchMeasuredProcess{Stderr: []byte(benchMissingFolderStderr)}, assert.AnError
		},
		retry: clock.clock(),
	}

	err := resetBenchRemoteScope(t.Context(), subject, benchLiveCommandRuntime{}, "/benchmarks/test")
	require.NoError(t, err)
	assert.Equal(t, 2, callCount)
	assert.Equal(t, []time.Duration{benchPollBackoff(0)}, clock.waits, "one wait between the two attempts, on the injected clock")
}

// Validates: R-6.10.14
//
// A 5xx during the visibility probe is retried: the CLI says so in its own
// error line, the only line that names it without --verbose, which the
// benchmark never passes. The probe waits on the injected clock and then
// finds the scope.
func TestWaitForBenchRemoteScopeVisibleRetriesTheCLIsServerProblemLine(t *testing.T) {
	t.Parallel()

	callCount := 0
	clock := &recordedBenchClock{}
	subject := preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			callCount++
			if callCount <= 2 {
				return benchMeasuredProcess{Stderr: []byte(benchServerProblemStderr)}, assert.AnError
			}

			return benchMeasuredProcess{}, nil
		},
		retry: clock.clock(),
	}

	err := waitForBenchRemoteScopeVisible(t.Context(), subject, benchLiveCommandRuntime{}, "/benchmarks/test")
	require.NoError(t, err)
	assert.Equal(t, 3, callCount, "stat and ls saw the server problem, then stat found the scope")
	assert.Equal(t, []time.Duration{benchPollBackoff(0)}, clock.waits)
}

// Validates: R-6.10.14
//
// A scope that never appears ends the probe at its deadline, read from the
// injected clock, after waits that grow to their cap: no wall time passes.
func TestWaitForBenchRemoteScopeVisibleGivesUpAtItsDeadline(t *testing.T) {
	t.Parallel()

	clock := &recordedBenchClock{}
	subject := preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			return benchMeasuredProcess{Stderr: []byte(benchMissingFolderStderr)}, assert.AnError
		},
		retry: clock.clock(),
	}

	err := waitForBenchRemoteScopeVisible(t.Context(), subject, benchLiveCommandRuntime{}, "/benchmarks/test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out after "+benchLivePollTimeout.String())
	require.NotEmpty(t, clock.waits)
	assert.Equal(t, benchPollBackoff(0), clock.waits[0])
	assert.Equal(t, benchLivePollBackoffCap, clock.waits[len(clock.waits)-1], "the waits grow to their cap")
	total := time.Duration(0)
	for _, wait := range clock.waits {
		total += wait
	}
	assert.Greater(t, total, benchLivePollTimeout, "the probe waited past its deadline before it gave up")
}

// Validates: R-6.10.14
//
// The CLI lines the benchmark reads are the CLI's own: each sentence is in
// internal/cli/error_presentation.go, read as text, since devtool links no
// product package.
func TestBenchCLIErrorLinesFollowTheCLIsSource(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile(filepath.Join(repoRootForTest(t), "internal", "cli", "error_presentation.go"))
	require.NoError(t, err)
	assert.Contains(t, string(source), `"`+cliServerProblemMessage+`"`)
	assert.Contains(t, string(source), `"`+cliRemoteItemMissingSuffix+`"`)
	assert.Contains(t, string(source), `"Error: " + plain`)
}

// Validates: R-6.10.14
//
// The benchmark reads a missing item from the CLI's own error line for it.
// The words "not found" elsewhere on stderr, a log line about a request the
// command retried or a failure that names something else, do not make a
// failed cleanup a success or a failed probe a wait.
func TestCLIReportsRemoteItemMissing_ReadsOnlyTheCLIErrorLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{name: "a folder rm -r or ls did not find", stderr: benchMissingFolderStderr, want: true},
		{name: "the drive root, which has no parent to list", stderr: "Error: / was not found on OneDrive.\n", want: true},
		{name: "a failure that recorded no path", stderr: "Error: the item was not found on OneDrive.", want: true},
		{
			name: "a retried not-found line before another failure",
			stderr: `time=2026-11-17T10:00:00Z level=DEBUG msg="retrying" body="itemNotFound: The resource could not be found."` + "\n" +
				"Error: OneDrive had a temporary server problem. Try again in a few minutes.\n",
			want: false,
		},
		{name: "another failure whose text says not found", stderr: `Error: drive section ["personal:a"] not found` + "\n", want: false},
		{name: "the sentence on a log line, not as the error", stderr: `level=WARN msg="/a.txt was not found on OneDrive."` + "\n", want: false},
		{name: "an error that goes on after the sentence", stderr: "Error: /a.txt was not found on OneDrive. And more.\n", want: false},
		{name: "no output", stderr: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, cliReportsRemoteItemMissing(tt.stderr))
		})
	}
}

// benchMissingFolderStderr is what `rm -r`, `stat` and `ls` print for a
// folder OneDrive does not have: the error line, then, under --verbose, its
// details.
const benchMissingFolderStderr = "Error: /benchmarks/test was not found on OneDrive. " +
	"Run 'onedrive-go ls /benchmarks' to see what is there.\n" +
	`Details: resolving "/benchmarks/test": graph: HTTP 404: itemNotFound` + "\n"

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
				return benchMeasuredProcess{Stderr: []byte(benchMissingFolderStderr)}, assert.AnError
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
	clock := &recordedBenchClock{}
	subject := preparedBenchSubject{
		measure: func(_ context.Context, spec benchCommandSpec) (benchMeasuredProcess, error) {
			callCount++
			switch callCount {
			case 1:
				assert.Equal(t, []string{"--config", "", "--drive", "", "stat", "--json", "/benchmarks/test"}, spec.Arg)
				return benchMeasuredProcess{Stderr: []byte(benchMissingFolderStderr)}, assert.AnError
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
		retry: clock.clock(),
	}

	err := waitForBenchRemoteScopeVisible(t.Context(), subject, benchLiveCommandRuntime{
		configPath: "",
		driveID:    "",
	}, "/benchmarks/test")
	require.NoError(t, err)
	assert.Equal(t, 3, callCount, "a sibling name should not satisfy scope visibility")
	assert.Equal(t, []time.Duration{benchPollBackoff(0)}, clock.waits)
}

// Validates: R-6.10.14
func TestClassifyBenchVisibilityErrorAllowsRetryableFamilies(t *testing.T) {
	t.Parallel()

	assert.NoError(t, classifyBenchVisibilityError([]byte(benchMissingFolderStderr), assert.AnError))
	assert.NoError(t, classifyBenchVisibilityError([]byte(benchServerProblemStderr), assert.AnError))
	assert.NoError(t, classifyBenchVisibilityError([]byte(benchServerProblemStderr+
		`Details: resolving "/benchmarks/test": graph: HTTP 503: serviceNotAvailable`+"\n"), assert.AnError),
		"under --verbose the error line still comes first")
	require.Error(t, classifyBenchVisibilityError([]byte("permission denied"), assert.AnError))
	require.Error(t, classifyBenchVisibilityError([]byte(`level=WARN msg="`+cliServerProblemMessage+`"`+"\n"), assert.AnError),
		"the sentence on a log line is not the CLI's error")
	require.Error(t, classifyBenchVisibilityError([]byte(benchServerProblemStderr[:len(benchServerProblemStderr)-1]+" And more.\n"),
		assert.AnError), "an error that goes on after the sentence is another error")
	require.Error(t, classifyBenchVisibilityError([]byte("Error: drive section not found\n"), assert.AnError),
		"a failure whose text says not found is not a missing item")
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
