package multisync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	contextPerfUploadBytes = 512
	contextPerfWait        = 5 * time.Second
)

// cannedRoundTripper answers every request with an empty 200, so the
// production perf round tripper can be driven without a network.
type cannedRoundTripper struct{}

func (cannedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// recordContextPerfMetrics stands in for the work an engine does under its run
// context: one Graph request through the production perf round tripper, one
// store transaction and one upload. Each is recorded on whatever collector the
// context carries, which is how the transports, the sync store and the
// transfer manager record theirs.
func recordContextPerfMetrics(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://graph.invalid/perf", http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := perf.RoundTripper{Inner: cannedRoundTripper{}}.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		return fmt.Errorf("close response body: %w", closeErr)
	}

	collector := perf.FromContext(ctx)
	collector.RecordDBTransaction(time.Millisecond)
	collector.RecordTransfer(perf.TransferKindUpload, contextPerfUploadBytes, time.Millisecond)

	return nil
}

// assertContextPerfMetricsOnce asserts that snapshot holds exactly what one
// recordContextPerfMetrics call records: not zero (the metric went to another
// collector) and not two (it was counted at two levels of the same chain).
func assertContextPerfMetricsOnce(t *testing.T, owner string, snapshot *perf.Snapshot) {
	t.Helper()

	assert.Equal(t, 1, snapshot.HTTPRequestCount, "%s: HTTP requests", owner)
	assert.Equal(t, 1, snapshot.HTTPSuccessCount, "%s: HTTP successes", owner)
	assert.Equal(t, 1, snapshot.DBTransactionCount, "%s: DB transactions", owner)
	assert.Equal(t, 1, snapshot.UploadCount, "%s: uploads", owner)
	assert.Equal(t, int64(contextPerfUploadBytes), snapshot.UploadBytes, "%s: upload bytes", owner)
}

// assertContextPerfMetricsAtEveryLevel checks the three views a sync owner
// has: the mount's own snapshot, which `status --perf` shows; the owner's
// aggregate, which `GET /v1/perf` returns beside it; and the command collector
// the owner reports to, which feeds the performance log lines.
func assertContextPerfMetricsAtEveryLevel(
	t *testing.T,
	mountID string,
	mounts map[string]perf.Snapshot,
	perfRuntime *perf.Runtime,
	command *perf.Collector,
) {
	t.Helper()

	require.Contains(t, mounts, mountID)
	mount := mounts[mountID]
	assertContextPerfMetricsOnce(t, "mount", &mount)

	aggregate := perfRuntime.AggregateSnapshot()
	assertContextPerfMetricsOnce(t, "owner aggregate", &aggregate)

	commandSnapshot := command.Snapshot()
	assertContextPerfMetricsOnce(t, "command", &commandSnapshot)
}

// A watch runner's work must be counted for its mount. The HTTP transport,
// the sync store and the transfer manager record on the collector their
// context carries, so the runner's context has to carry the mount's collector;
// otherwise `status --perf` shows a mount with no requests, no transactions
// and no transfers while it is plainly syncing. The run starts under a context
// that already carries the command collector, as the CLI's does, so the test
// also proves the mount collector replacing it loses nothing and counts
// nothing twice.
//
// Validates: R-6.6.15
func TestOrchestrator_MountPerfIncludesContextRecordedMetrics(t *testing.T) {
	rd := testStandaloneMount(t, "personal:perf-watch-context@example.com", "PerfWatchContext")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	commandCollector := perf.NewCollector(nil)
	cfg.PerfParent = commandCollector

	orch := NewOrchestrator(cfg)

	recorded := make(chan error, 1)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				recorded <- recordContextPerfMetrics(ctx)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(perf.WithCollector(t.Context(), commandCollector))
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	select {
	case err := <-recorded:
		require.NoError(t, err)
	case <-time.After(contextPerfWait):
		require.Fail(t, "watch runner did not record in time")
	}

	// Read while the watch still runs, as `status --perf` does: a mount's
	// collector leaves the runtime when its runner exits.
	mounts := orch.perfRuntime.SnapshotByMount()

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(contextPerfWait):
		require.Fail(t, "RunWatch did not stop in time")
	}

	assertContextPerfMetricsAtEveryLevel(t, rd.CanonicalID.String(), mounts, orch.perfRuntime, commandCollector)
}

// The one-shot path runs each mount's pass through its own work function, so
// it needs the same wrap as the watch runner.
//
// Validates: R-6.6.15
func TestOrchestrator_OneShotMountPerfIncludesContextRecordedMetrics(t *testing.T) {
	rd := testStandaloneMount(t, "personal:perf-oneshot-context@example.com", "PerfOneShotContext")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	commandCollector := perf.NewCollector(nil)
	cfg.PerfParent = commandCollector

	orch := NewOrchestrator(cfg)

	// The one-shot owner drops a mount's collector when it closes that
	// mount's engine, so the per-mount view exists only while the pass runs,
	// which is when `status --perf` can read it. The pass captures it.
	mountsDuringPass := make(chan map[string]perf.Snapshot, 1)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(ctx context.Context, mode syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				if err := recordContextPerfMetrics(ctx); err != nil {
					return nil, err
				}
				mountsDuringPass <- orch.perfRuntime.SnapshotByMount()

				return &syncengine.Report{Mode: mode}, nil
			},
		}, nil
	}

	result := orch.RunOnce(
		perf.WithCollector(t.Context(), commandCollector),
		syncengine.SyncBidirectional,
		syncengine.RunOptions{},
	)
	require.Len(t, result.Reports, 1)
	require.NoError(t, result.Reports[0].Err)

	require.Len(t, mountsDuringPass, 1, "the pass ran once and captured the mount snapshots")
	mounts := <-mountsDuringPass

	assertContextPerfMetricsAtEveryLevel(t, rd.CanonicalID.String(), mounts, orch.perfRuntime, commandCollector)
}

// Validates: R-6.6.15, R-6.6.16
func TestOrchestrator_OneShotControlSocket_PerfStatusAndCapture(t *testing.T) {
	rd := testStandaloneMount(t, "personal:perf-control@example.com", "PerfControl")
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
	rd := testStandaloneMount(t, "personal:perf-invalid@example.com", "PerfInvalid")
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

// The owner resolves nothing against its own working directory: a relative
// capture path that reaches it is refused rather than written somewhere the
// requester never looks.
//
// Validates: R-6.6.16
func TestPerfCaptureOptionsFromRequest_RejectsRelativeOutputDir(t *testing.T) {
	_, err := perfCaptureOptionsFromRequest(synccontrol.OwnerModeWatch, &synccontrol.PerfCaptureRequest{
		DurationMS: 1000,
		OutputDir:  "relative/out",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be an absolute path")

	absolute := t.TempDir()
	opts, err := perfCaptureOptionsFromRequest(synccontrol.OwnerModeWatch, &synccontrol.PerfCaptureRequest{
		DurationMS: 1000,
		OutputDir:  absolute,
	})
	require.NoError(t, err)
	assert.Equal(t, absolute, opts.OutputDir)
}
