package devtool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.14
func TestRunBenchRequiresRepoRootAndScenario(t *testing.T) {
	t.Parallel()

	err := RunBench(t.Context(), BenchOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing repo root")

	err = RunBench(t.Context(), BenchOptions{RepoRoot: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scenario")
}

// Validates: R-6.10.14
func TestLookupBenchRegistriesAreSortedAndIncludeBuiltins(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{DefaultBenchSubjectID}, BenchSubjectIDs())
	assert.Equal(t, []string{
		startupEmptyConfigScenarioID,
		syncPartialLocalCatchup100MID,
	}, BenchScenarioIDs())

	subject, err := LookupBenchSubject(DefaultBenchSubjectID)
	require.NoError(t, err)
	assert.Equal(t, DefaultBenchSubjectID, subject.ID)
	assert.Equal(t, benchSubjectKindRepoCLI, subject.Kind)

	scenario, err := LookupBenchScenario(startupEmptyConfigScenarioID)
	require.NoError(t, err)
	assert.Equal(t, startupEmptyConfigScenarioID, scenario.ID)
	assert.Equal(t, startupEmptyConfigDefaultRuns, scenario.DefaultRuns)
	assert.Equal(t, startupEmptyConfigDefaultWarmups, scenario.DefaultWarmup)

	liveScenario, err := LookupBenchScenario(syncPartialLocalCatchup100MID)
	require.NoError(t, err)
	assert.Equal(t, syncPartialLocalCatchup100MID, liveScenario.ID)
	assert.Equal(t, syncPartialLocalCatchup100MRuns, liveScenario.DefaultRuns)
	assert.Equal(t, syncPartialLocalCatchup100MWarm, liveScenario.DefaultWarmup)
	assert.Equal(t, "live", liveScenario.Class)
	assert.Equal(t, "default-safe", liveScenario.ConfigProfile)
	assert.Positive(t, liveScenario.Denominators.FileCount)
	assert.Positive(t, liveScenario.Denominators.DirectoryCount)
	assert.Positive(t, liveScenario.Denominators.ChangedItemCount)
	assert.Positive(t, liveScenario.Denominators.ChangedByteCount)
	assert.Positive(t, liveScenario.Denominators.ExpectedTransfers)
	assert.Zero(t, liveScenario.Denominators.ExpectedDeletes)
}

// Validates: R-6.10.14
func TestLookupBenchSubjectAndScenarioRejectUnknownIDs(t *testing.T) {
	t.Parallel()

	_, err := LookupBenchSubject("unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), DefaultBenchSubjectID)

	_, err = LookupBenchScenario("unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), startupEmptyConfigScenarioID)
	assert.Contains(t, err.Error(), syncPartialLocalCatchup100MID)
}

// Validates: R-6.10.14
func TestSummarizeBenchSamplesComputesExpectedStats(t *testing.T) {
	t.Parallel()

	summary := summarizeBenchSamples([]benchSample{
		{Phase: benchSamplePhaseWarmup, Status: BenchSampleSuccess},
		{Phase: benchSamplePhaseMeasured, Status: BenchSampleSuccess, ElapsedMicros: 10, PeakRSSBytes: 100, UserCPUMicros: 6, SystemCPUMicros: 2},
		{Phase: benchSamplePhaseMeasured, Status: BenchSampleSuccess, ElapsedMicros: 20, PeakRSSBytes: 300, UserCPUMicros: 8, SystemCPUMicros: 4},
		{Phase: benchSamplePhaseMeasured, Status: BenchSampleSuccess, ElapsedMicros: 30, PeakRSSBytes: 200, UserCPUMicros: 10, SystemCPUMicros: 6},
	})

	assert.Equal(t, BenchSampleSuccess, summary.Status)
	assert.Equal(t, 3, summary.SampleCount)
	assert.Equal(t, 3, summary.SuccessfulSampleCount)
	assert.Equal(t, 1, summary.WarmupCount)
	assert.EqualValues(t, 20, summary.ElapsedMicros.Mean)
	assert.EqualValues(t, 20, summary.ElapsedMicros.Median)
	assert.EqualValues(t, 10, summary.ElapsedMicros.Min)
	assert.EqualValues(t, 30, summary.ElapsedMicros.Max)
	assert.EqualValues(t, 200, summary.PeakRSSBytes.Median)
	assert.EqualValues(t, 300, summary.PeakRSSBytes.Max)
	assert.EqualValues(t, 8, summary.UserCPUMicros.Mean)
	assert.EqualValues(t, 4, summary.SystemCPUMicros.Mean)
}

// Validates: R-6.10.14
func TestWriteBenchResultJSONWritesExactPayload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "bench-result.json")
	payload := []byte("{\"result_version\":1}\n")

	require.NoError(t, writeBenchResultJSON(path, payload))

	data, err := readFile(path)
	require.NoError(t, err)
	assert.Equal(t, payload, data)
}

// Validates: R-6.10.14
func TestBenchSampleFailureClassificationHelpers(t *testing.T) {
	t.Parallel()

	base := benchSample{Phase: benchSamplePhaseMeasured, Iteration: 1}

	fixture := benchFixtureFailureSample(base, assert.AnError)
	assert.Equal(t, BenchSampleFixtureFailed, fixture.Status)
	assert.Contains(t, fixture.FailureExcerpt, assert.AnError.Error())

	subject := benchSubjectFailureSample(base, assert.AnError, []byte(""), []byte("stderr failure"))
	assert.Equal(t, BenchSampleSubjectFailed, subject.Status)
	assert.Contains(t, subject.FailureExcerpt, "stderr failure")

	invalid := benchInvalidSample(base, assert.AnError)
	assert.Equal(t, BenchSampleInvalid, invalid.Status)

	aborted := benchAbortedSample(base, assert.AnError)
	assert.Equal(t, BenchSampleAborted, aborted.Status)
}

// Validates: R-6.10.14
func TestFailureExcerptPrefersTailOfLongStructuredStreams(t *testing.T) {
	t.Parallel()

	startMarker := "start-of-stream-marker:"
	stderr := []byte(startMarker + strings.Repeat("prefix-", 96) + "terminal failure detail")

	excerpt := failureExcerpt(assert.AnError, nil, stderr)

	assert.Len(t, excerpt, benchFailureExcerptLimit)
	assert.True(t, strings.HasPrefix(excerpt, "..."))
	assert.Contains(t, excerpt, "terminal failure detail")
	assert.NotContains(t, excerpt, startMarker)
}

// Validates: R-6.10.14
func TestRunBenchStartupEmptyConfigSucceeds(t *testing.T) {
	var stdout bytes.Buffer

	err := RunBench(t.Context(), BenchOptions{
		RepoRoot: repoRootFromBenchTest(t),
		Scenario: startupEmptyConfigScenarioID,
		Runs:     1,
		Warmup:   0,
		JSON:     true,
		Stdout:   &stdout,
	})
	require.NoError(t, err)

	var result benchResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.Equal(t, benchSchemaVersion, result.ResultVersion)
	assert.Equal(t, DefaultBenchSubjectID, result.Subject.ID)
	assert.Equal(t, startupEmptyConfigScenarioID, result.Scenario.ID)
	assert.Equal(t, 1, result.Summary.SampleCount)
	require.Len(t, result.Samples, 1)
	assert.Equal(t, BenchSampleSuccess, result.Samples[0].Status)
	require.NotNil(t, result.Samples[0].PerfSummary)
	assert.Zero(t, result.Scenario.Denominators.FileCount)
	assert.Zero(t, result.Scenario.Denominators.DirectoryCount)
	assert.Zero(t, result.Scenario.Denominators.ChangedItemCount)
	assert.Zero(t, result.Scenario.Denominators.ChangedByteCount)
	assert.Zero(t, result.Scenario.Denominators.ExpectedTransfers)
	assert.Zero(t, result.Scenario.Denominators.ExpectedDeletes)
}

func repoRootFromBenchTest(t *testing.T) string {
	t.Helper()

	cwd, err := os.Getwd()
	require.NoError(t, err)

	return filepath.Clean(filepath.Join(cwd, "..", ".."))
}
