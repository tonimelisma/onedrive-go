package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// Validates: R-6.6.16
func TestMainWithWriters_PerfCaptureJSON_ForOneShotOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	var captured synccontrol.PerfCaptureRequest
	expectedOutputDir := filepath.Join(t.TempDir(), "capture-bundle")

	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeOneShot}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == synccontrol.PathPerfCapture:
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if err := json.NewEncoder(w).Encode(synccontrol.PerfCaptureResponse{
				OwnerMode: synccontrol.OwnerModeOneShot,
				Result: perf.CaptureResult{
					OutputDir:     expectedOutputDir,
					ManifestPath:  filepath.Join(expectedOutputDir, "manifest.json"),
					CPUProfile:    filepath.Join(expectedOutputDir, "cpu.pprof"),
					HeapProfile:   filepath.Join(expectedOutputDir, "heap.pprof"),
					BlockProfile:  filepath.Join(expectedOutputDir, "block.pprof"),
					MutexProfile:  filepath.Join(expectedOutputDir, "mutex.pprof"),
					GoroutineDump: filepath.Join(expectedOutputDir, "goroutine.txt"),
					TracePath:     filepath.Join(expectedOutputDir, "trace.out"),
				},
			}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := mainWithWriters([]string{
		"--json",
		"perf",
		"capture",
		"--duration=5s",
		"--output", expectedOutputDir,
		"--trace",
		"--full-detail",
	}, &stdout, &stderr)
	require.Zero(t, exitCode)
	assert.Empty(t, stderr.String())

	assert.Equal(t, int64(5000), captured.DurationMS)
	assert.Equal(t, expectedOutputDir, captured.OutputDir)
	assert.True(t, captured.Trace)
	assert.True(t, captured.FullDetail)

	var decoded synccontrol.PerfCaptureResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	assert.Equal(t, synccontrol.OwnerModeOneShot, decoded.OwnerMode)
	assert.Equal(t, expectedOutputDir, decoded.Result.OutputDir)
	assert.NotEmpty(t, decoded.Result.TracePath)
}

// Validates: R-6.6.16
func TestMainWithWriters_PerfCaptureRejectsInvalidDuration(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := mainWithWriters([]string{
		"perf",
		"capture",
		"--duration=4s",
	}, &stdout, &stderr)

	assert.Equal(t, 1, exitCode)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "--duration must be between 5s and 1m0s")
}

// Validates: R-6.6.16
func TestMainWithWriters_PerfCaptureFailsWhenNoOwnerIsRunning(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := mainWithWriters([]string{
		"perf",
		"capture",
		"--duration=5s",
	}, &stdout, &stderr)

	assert.Equal(t, 1, exitCode)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "no active sync owner")
}
