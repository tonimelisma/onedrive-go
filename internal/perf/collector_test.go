package perf

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.6.14, R-6.6.15
func TestCollector_RollsUpChildMetricsIntoParent(t *testing.T) {
	parent := NewCollector(nil)
	child := NewCollector(parent)

	child.RecordHTTPRequest(200, 50*time.Millisecond, nil)
	child.RecordHTTPRetry(2 * time.Second)
	child.RecordTransfer(TransferKindDownload, 128, 75*time.Millisecond)
	child.RecordDBTransaction(10 * time.Millisecond)
	child.RecordObserve(3, 12*time.Millisecond)
	child.RecordPlan(4, 13*time.Millisecond)
	child.RecordExecute(4, 3, 1, 14*time.Millisecond)
	child.RecordReconcile(5, 15*time.Millisecond)
	child.RecordWatchBatch(6)
	child.SetResult("success")

	parentSnapshot := parent.Snapshot()
	childSnapshot := child.Snapshot()

	assert.Equal(t, 1, childSnapshot.HTTPRequestCount)
	assert.Equal(t, 1, parentSnapshot.HTTPRequestCount)
	assert.Equal(t, int64(128), parentSnapshot.DownloadBytes)
	assert.Equal(t, 4, parentSnapshot.ExecuteActionCount)
	assert.Equal(t, 6, parentSnapshot.WatchPathCount)
	assert.Equal(t, "success", childSnapshot.Result)
}

// Validates: R-6.6.14
func TestWithCollector_RoundTripRecordsRequestMetrics(t *testing.T) {
	collector := NewCollector(nil)
	ctx := WithCollector(context.Background(), collector)

	require.Same(t, collector, FromContext(ctx))

	collector.RecordHTTPRequest(503, 25*time.Millisecond, nil)
	collector.RecordHTTPRequest(0, 10*time.Millisecond, assert.AnError)

	snapshot := collector.Snapshot()
	assert.Equal(t, 2, snapshot.HTTPRequestCount)
	assert.Equal(t, 1, snapshot.HTTPServerErrorCount)
	assert.Equal(t, 1, snapshot.HTTPTransportErrors)
}
