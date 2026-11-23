package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.6.14
func TestSession_EmitsPeriodicUpdateAndSummary(t *testing.T) {
	t.Parallel()

	logBuf := &lockedBuffer{}
	logger := newTestJSONLogger(logBuf)

	session, _ := NewSession(context.Background(), logger, "command", "status", 5*time.Millisecond, nil)
	session.Collector().AddCommandItems(3)
	session.Collector().RecordHTTPRequest(200, 12*time.Millisecond, nil)

	require.Eventually(t, func() bool {
		return strings.Contains(logBuf.String(), "performance update")
	}, 250*time.Millisecond, 5*time.Millisecond)

	session.Complete(nil)

	lines := splitLogLines(t, logBuf.String())
	require.GreaterOrEqual(t, len(lines), 2)

	update := decodeLogLine(t, lines[0])
	summary := decodeLogLine(t, lines[len(lines)-1])

	assert.Equal(t, "performance update", update["msg"])
	assert.Equal(t, "command", update["perf_kind"])
	assert.Equal(t, "status", update["perf_name"])
	assert.NotContains(t, update, "started_at")
	assert.NotContains(t, update, "updated_at")

	assert.Equal(t, "performance summary", summary["msg"])
	assert.Equal(t, "success", summary["result"])
	assert.EqualValues(t, 3, summary["command_items"])
	assert.EqualValues(t, 1, summary["http_requests"])
}

// A panic in the periodic update ends the command: the context NewSession
// returned is canceled with the panic as its cause, which the CLI turns into
// exit status 1, and the summary is still written when the command completes.
//
// Validates: R-6.3.10, R-6.6.14
func TestSession_UpdatePanicEndsTheCommandWithThePanic(t *testing.T) {
	t.Parallel()

	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	logBuf := &lockedBuffer{}
	logger := slog.New(synctest.PanicOnMessage{Message: "performance update", Next: slog.NewJSONHandler(logBuf, nil)})
	session, ctx := NewSession(context.Background(), logger, "command", "status", time.Millisecond, nil)

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the update's panic did not end the command")
	}
	require.ErrorIs(t, context.Cause(ctx), panicguard.ErrPanic)
	require.ErrorContains(t, context.Cause(ctx), "log call exploded")

	session.Complete(nil)
	assert.Contains(t, logBuf.String(), "Internal error in the performance logger")
	assert.Contains(t, logBuf.String(), "performance summary", "the summary is still written")
	require.ErrorIs(t, context.Cause(ctx), panicguard.ErrPanic, "completing keeps the panic as the cause")
}

// panicOnUpdateAfter is a slog.Handler that panics on the first periodic
// update once gate has closed, as a bug in the update's log call would, and
// drops every other record.
type panicOnUpdateAfter struct {
	gate <-chan struct{}
}

func (panicOnUpdateAfter) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler fixes the by-value Record parameter.
func (h panicOnUpdateAfter) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "performance update" {
		<-h.gate
		panic("log call exploded")
	}

	return nil
}

func (h panicOnUpdateAfter) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h panicOnUpdateAfter) WithGroup(string) slog.Handler { return h }

// A request the update's panic interrupts carries the panic, the cause of
// the cancel, in place of context.Canceled. It reads as stopped, as under any
// other stop, while the command's summary still reads as an error.
//
// Validates: R-6.3.10, R-6.8.29
func TestSession_UpdatePanicStopsTheRequestInFlight(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	arrived := make(chan struct{})
	var arrivedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrivedOnce.Do(func() { close(arrived) })
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	session, ctx := NewSession(context.Background(), slog.New(panicOnUpdateAfter{gate: arrived}),
		"command", "status", time.Millisecond, nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	client := &http.Client{Transport: RoundTripper{Inner: &http.Transport{}}}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Do(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}

	require.ErrorIs(t, err, context.Canceled, "the interrupted request reads as stopped")
	require.ErrorIs(t, err, panicguard.ErrPanic, "and still carries the panic")
	assert.Equal(t, 1, session.Collector().Snapshot().HTTPRequestCount)

	summaryBuf := &lockedBuffer{}
	session.SetLogger(newTestJSONLogger(summaryBuf))
	session.Complete(err)
	summary := decodeLogLine(t, strings.TrimSpace(summaryBuf.String()))
	assert.Equal(t, "error", summary["result"], "a command a panic ended is no clean stop")
}

// Validates: R-6.6.14
func TestSession_CompleteReleasesTheCommandContext(t *testing.T) {
	t.Parallel()

	session, ctx := NewSession(t.Context(), slog.New(slog.DiscardHandler), "command", "status", 0, nil)
	require.NoError(t, ctx.Err(), "the command context lives until the command completes")

	session.Complete(nil)

	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.ErrorIs(t, context.Cause(ctx), context.Canceled, "a clean command ends without a cause")
}

// sessionEventWait bounds a test's wait for a session's goroutine.
const sessionEventWait = 5 * time.Second

// The periodic updates end when the command's context ends, also without
// Complete: a command whose own goroutine panicked never reaches Complete,
// and its updates would otherwise go on until the process exits. Complete
// after that still writes the summary, once, and returns.
//
// Validates: R-6.6.14
func TestSession_PeriodicUpdatesEndWithTheCommandContext(t *testing.T) {
	t.Parallel()

	logBuf := &lockedBuffer{}
	parent, cancel := context.WithCancel(t.Context())
	session, _ := NewSession(parent, newTestJSONLogger(logBuf), "command", "status", time.Hour, nil)

	cancel()

	select {
	case <-session.doneCh:
	case <-time.After(sessionEventWait):
		require.FailNow(t, "the periodic updates outlived the command's context")
	}

	session.Complete(context.Canceled)
	session.Complete(nil)
	lines := splitLogLines(t, logBuf.String())
	require.Len(t, lines, 1, "the summary is written once")
	summary := decodeLogLine(t, lines[0])
	assert.Equal(t, "performance summary", summary["msg"])
	assert.Equal(t, "canceled", summary["result"])
}

// Validates: R-6.6.16
func TestRuntimeCapture_DefaultManifestOmitsDriveSnapshots(t *testing.T) {
	runtime := NewRuntime(nil)
	runtime.Collector().RecordHTTPRequest(200, time.Millisecond, nil)
	runtime.RegisterMount("personal:alice@example.com").RecordTransfer(TransferKindUpload, 1, time.Millisecond)
	outputDir := filepath.Join(t.TempDir(), "bundle")

	result, err := runtime.Capture(t.Context(), CaptureOptions{
		Duration:  10 * time.Millisecond,
		OutputDir: outputDir,
		OwnerMode: "watch",
	})
	require.NoError(t, err)
	assert.FileExists(t, result.ManifestPath)
	assert.FileExists(t, result.CPUProfile)
	assert.FileExists(t, result.HeapProfile)
	assert.FileExists(t, result.BlockProfile)
	assert.FileExists(t, result.MutexProfile)
	assert.FileExists(t, result.GoroutineDump)

	manifest := readCaptureManifest(t, result.ManifestPath)
	assert.Equal(t, "watch", manifest.OwnerMode)
	assert.Equal(t, 1, manifest.ManagedDriveCount)
	assert.Equal(t, 1, manifest.Aggregate.HTTPRequestCount)
	assert.Nil(t, manifest.DriveSnapshots)
}

// Validates: R-6.6.16
func TestRuntimeCapture_FullDetailManifestIncludesDriveSnapshots(t *testing.T) {
	runtime := NewRuntime(nil)
	runtime.Collector().RecordDBTransaction(time.Millisecond)
	runtime.RegisterMount("personal:bob@example.com").RecordTransfer(TransferKindDownload, 2, time.Millisecond)
	outputDir := filepath.Join(t.TempDir(), "bundle")

	result, err := runtime.Capture(t.Context(), CaptureOptions{
		Duration:   10 * time.Millisecond,
		OutputDir:  outputDir,
		Trace:      true,
		FullDetail: true,
		OwnerMode:  "oneshot",
	})
	require.NoError(t, err)
	assert.FileExists(t, result.TracePath)

	manifest := readCaptureManifest(t, result.ManifestPath)
	require.NotNil(t, manifest.DriveSnapshots)
	require.Contains(t, manifest.DriveSnapshots, "personal:bob@example.com")
	assert.Equal(t, 1, manifest.DriveSnapshots["personal:bob@example.com"].DownloadCount)
	assert.Equal(t, int64(2), manifest.DriveSnapshots["personal:bob@example.com"].DownloadBytes)
}

// Validates: R-6.6.14
func TestSnapshotAttrs_UsesActionableActionField(t *testing.T) {
	t.Parallel()

	attrs := SnapshotAttrs(&Snapshot{
		CommandBytes:          512,
		PlanRunCount:          1,
		ActionableActionCount: 4,
		PlanTimeMS:            25,
	})

	attrMap := make(map[string]any, len(attrs))
	for i := range attrs {
		attrMap[attrs[i].Key] = attrs[i].Value.Any()
	}

	assert.Equal(t, int64(512), attrMap["command_bytes"])
	assert.Equal(t, int64(1), attrMap["plan_runs"])
	assert.Equal(t, int64(4), attrMap["actionable_actions"])
	assert.Equal(t, int64(25), attrMap["plan_time_ms"])
}

// Validates: R-6.6.17
func TestSnapshotAttrs_IncludesStaleWorkAndObservationCounters(t *testing.T) {
	t.Parallel()

	attrs := SnapshotAttrs(&Snapshot{
		SupersededEngineAdmissionCount:               1,
		SupersededWorkerStartLocalTruthCount:         2,
		SupersededWorkerStartRemoteTruthCount:        3,
		SupersededLiveLocalPreconditionCount:         4,
		SupersededLiveRemotePreconditionCount:        5,
		SupersededPendingReplanRetirementCount:       6,
		LocalObservationScopedCommitCount:            7,
		LocalObservationScopedUpsertCount:            8,
		LocalObservationPrefixDeleteCount:            10,
		LocalObservationFullSnapshotReplacementCount: 11,
		LocalObservationSuspectDroppedEventsCount:    12,
		ReplanIdleWaitingDrainMS:                     13,
		ReplanIdleLocalRefreshMS:                     14,
		ReplanIdlePlanningMS:                         15,
		ReplanIdleRuntimeInstallMS:                   16,
	})

	attrMap := make(map[string]any, len(attrs))
	for i := range attrs {
		attrMap[attrs[i].Key] = attrs[i].Value.Any()
	}

	assert.Equal(t, int64(1), attrMap["superseded_engine_admission"])
	assert.Equal(t, int64(2), attrMap["superseded_worker_start_local_truth"])
	assert.Equal(t, int64(3), attrMap["superseded_worker_start_remote_truth"])
	assert.Equal(t, int64(4), attrMap["superseded_live_local_precondition"])
	assert.Equal(t, int64(5), attrMap["superseded_live_remote_precondition"])
	assert.Equal(t, int64(6), attrMap["superseded_pending_replan_retirement"])
	assert.Equal(t, int64(7), attrMap["local_obs_scoped_commits"])
	assert.Equal(t, int64(8), attrMap["local_obs_scoped_upserts"])
	assert.NotContains(t, attrMap, "local_obs_exact_deletes")
	assert.Equal(t, int64(10), attrMap["local_obs_prefix_deletes"])
	assert.Equal(t, int64(11), attrMap["local_obs_full_snapshot_replacements"])
	assert.Equal(t, int64(12), attrMap["local_obs_suspect_dropped_events"])
	assert.Equal(t, int64(13), attrMap["replan_idle_waiting_drain_ms"])
	assert.Equal(t, int64(14), attrMap["replan_idle_local_refresh_ms"])
	assert.Equal(t, int64(15), attrMap["replan_idle_planning_ms"])
	assert.Equal(t, int64(16), attrMap["replan_idle_runtime_install_ms"])
}

func newTestJSONLogger(buf *lockedBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

func splitLogLines(t *testing.T, raw string) []string {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(raw), "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			filtered = append(filtered, line)
		}
	}

	return filtered
}

func decodeLogLine(t *testing.T, line string) map[string]any {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &decoded))
	return decoded
}

func readCaptureManifest(t *testing.T, path string) captureManifest {
	t.Helper()

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)

	var manifest captureManifest
	require.NoError(t, json.Unmarshal(data, &manifest))

	return manifest
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.Buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("write test log buffer: %w", err)
	}

	return n, nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.Buffer.String()
}
