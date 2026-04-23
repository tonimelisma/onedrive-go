package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
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
