package devtool

import (
	"bytes"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.1
func TestRunWatchCaptureRequiresScenario(t *testing.T) {
	t.Parallel()

	err := RunWatchCapture(t.Context(), WatchCaptureOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scenario")
}

// Validates: R-6.2.1
func TestWatchCaptureScenarioNamesSorted(t *testing.T) {
	t.Parallel()

	names := WatchCaptureScenarioNames()
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	assert.Equal(t, sorted, names)
	assert.Contains(t, names, "marker_create")
}

// Validates: R-6.2.1
func TestRunWatchCaptureWritesJSON(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := RunWatchCapture(t.Context(), WatchCaptureOptions{
		Scenario: "marker_create",
		JSON:     true,
		Settle:   100 * time.Millisecond,
		Stdout:   &stdout,
	})
	require.NoError(t, err)

	var records []WatchCaptureRecord
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &records))
	require.NotEmpty(t, records)

	for _, record := range records {
		assert.Equal(t, "marker_create", record.Scenario)
		assert.Equal(t, "create_marker", record.Step)
		assert.NotEmpty(t, record.Path)
		assert.NotZero(t, record.OpBits)
		assert.NotEmpty(t, record.OpNames)
		assert.GreaterOrEqual(t, record.TimeOffsetMicros, int64(0))
	}
}

// Validates: R-6.2.1
func TestLookupWatchCaptureScenarioStepOrder(t *testing.T) {
	t.Parallel()

	scenario, err := LookupWatchCaptureScenario("marker_parent_rename")
	require.NoError(t, err)

	assert.Equal(t, []string{"rename_parent"}, scenario.StepNames())
}
