package multisync

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// Validates: R-6.6.15, R-6.6.16
func TestOrchestrator_OneShotControlSocket_PerfStatusAndCapture(t *testing.T) {
	rd := testResolvedDrive(t, "personal:perf-control@example.com", "PerfControl")
	cfg := testOrchestratorConfig(t, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	orch := NewOrchestrator(cfg)

	orch.perfRuntime.Collector().RecordHTTPRequest(http.StatusOK, 15*time.Millisecond, nil)
	driveCollector := orch.perfRuntime.RegisterMount(rd.CanonicalID.String())
	driveCollector.RecordTransfer(perf.TransferKindUpload, 256, 20*time.Millisecond)
	driveCollector.RecordPlan(2, 5*time.Millisecond)

	control, err := orch.startControlServer(t.Context(), synccontrol.OwnerModeOneShot, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, control.Close(context.Background()))
	})

	client := controlTestClient(cfg.ControlSocketPath)

	statusReq, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		synccontrol.HTTPBaseURL+synccontrol.PathPerfStatus,
		http.NoBody,
	)
	require.NoError(t, err)

	// #nosec G704 -- fixed Unix-domain test socket client.
	statusResp, err := client.Do(statusReq)
	require.NoError(t, err)
	defer statusResp.Body.Close()
	require.Equal(t, http.StatusOK, statusResp.StatusCode)

	var status synccontrol.PerfStatusResponse
	require.NoError(t, json.NewDecoder(statusResp.Body).Decode(&status))
	assert.Equal(t, synccontrol.OwnerModeOneShot, status.OwnerMode)
	assert.Equal(t, 1, status.Aggregate.HTTPRequestCount)
	require.Contains(t, status.Mounts, rd.CanonicalID.String())
	assert.Equal(t, 1, status.Mounts[rd.CanonicalID.String()].UploadCount)
	assert.Equal(t, int64(256), status.Mounts[rd.CanonicalID.String()].UploadBytes)

	outputDir := filepath.Join(t.TempDir(), "capture-bundle")
	requestBody, err := json.Marshal(synccontrol.PerfCaptureRequest{
		DurationMS: 5,
		OutputDir:  outputDir,
		FullDetail: true,
	})
	require.NoError(t, err)

	captureReq, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		synccontrol.HTTPBaseURL+synccontrol.PathPerfCapture,
		bytes.NewReader(requestBody),
	)
	require.NoError(t, err)
	captureReq.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- fixed Unix-domain test socket client.
	captureResp, err := client.Do(captureReq)
	require.NoError(t, err)
	defer captureResp.Body.Close()
	require.Equal(t, http.StatusOK, captureResp.StatusCode)

	var capture synccontrol.PerfCaptureResponse
	require.NoError(t, json.NewDecoder(captureResp.Body).Decode(&capture))
	assert.Equal(t, synccontrol.OwnerModeOneShot, capture.OwnerMode)
	assert.FileExists(t, capture.Result.ManifestPath)
	assert.FileExists(t, capture.Result.CPUProfile)
	assert.FileExists(t, capture.Result.HeapProfile)
	assert.FileExists(t, capture.Result.BlockProfile)
	assert.FileExists(t, capture.Result.MutexProfile)
	assert.FileExists(t, capture.Result.GoroutineDump)
}

// Validates: R-6.6.16
func TestOrchestrator_OneShotControlSocket_PerfCaptureRejectsInvalidDuration(t *testing.T) {
	rd := testResolvedDrive(t, "personal:perf-invalid@example.com", "PerfInvalid")
	cfg := testOrchestratorConfig(t, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	orch := NewOrchestrator(cfg)

	control, err := orch.startControlServer(t.Context(), synccontrol.OwnerModeOneShot, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, control.Close(context.Background()))
	})

	client := controlTestClient(cfg.ControlSocketPath)
	requestBody, err := json.Marshal(synccontrol.PerfCaptureRequest{DurationMS: 0})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		synccontrol.HTTPBaseURL+synccontrol.PathPerfCapture,
		bytes.NewReader(requestBody),
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- fixed Unix-domain test socket client.
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var decoded synccontrol.MutationResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))
	assert.Equal(t, synccontrol.StatusError, decoded.Status)
	assert.Equal(t, synccontrol.ErrorInvalidRequest, decoded.Code)
	assert.Contains(t, decoded.Message, "greater than zero")
}
