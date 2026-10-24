package synccontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A relative --output must mean the directory the user typed it in, which
// only the sending process knows; encoding resolves it there.
//
// Validates: R-6.6.16
func TestPerfCaptureRequest_MarshalJSONResolvesRelativeOutputDir(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)

	encoded, err := json.Marshal(PerfCaptureRequest{DurationMS: 1000, OutputDir: "captures/out", Trace: true})
	require.NoError(t, err)

	var decoded PerfCaptureRequest
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, filepath.Join(wd, "captures", "out"), decoded.OutputDir)
	assert.EqualValues(t, 1000, decoded.DurationMS)
	assert.True(t, decoded.Trace)
}

// Validates: R-6.6.16
func TestPerfCaptureRequest_MarshalJSONKeepsAbsoluteAndEmptyOutputDir(t *testing.T) {
	absolute := t.TempDir()
	for _, outputDir := range []string{absolute, ""} {
		encoded, err := json.Marshal(PerfCaptureRequest{DurationMS: 5, OutputDir: outputDir})
		require.NoError(t, err)

		var decoded PerfCaptureRequest
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		assert.Equal(t, outputDir, decoded.OutputDir)
	}
}
