package sync

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.9
func TestRecordSupersede_WarnsOnceWhenTheSameActionKeepsGoingStale(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	flow := &engineFlow{
		deps:             runtimeDeps{logger: slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))},
		supersedeStreaks: make(map[string]int),
	}
	move := &actionCompletion{ActionType: ActionRemoteMove, OldPath: "movesrc", Path: "movedst", ErrMsg: "remote item changed"}

	for range supersedeStreakWarnThreshold + 2 {
		flow.recordSupersede(move)
	}
	logs := logBuf.String()
	assert.Equal(t, supersedeStreakWarnThreshold+2, strings.Count(logs, "action superseded; replanning"),
		"every supersede is visible at Debug")
	assert.Equal(t, 1, strings.Count(logs, "level=WARN"), "the stall warns once per streak")

	flow.clearSupersedeStreak(move)
	logBuf.Reset()
	for range supersedeStreakWarnThreshold - 1 {
		flow.recordSupersede(move)
	}
	assert.NotContains(t, logBuf.String(), "level=WARN", "success resets the streak")
}

// Validates: R-2.8.11
//
// A wedged action's backoff doubles from the base to a cap (D-059d).
func TestSupersedeParkDelay_DoublesToACap(t *testing.T) {
	t.Parallel()

	assert.Equal(t, supersedeParkBaseDelay, supersedeParkDelay(0))
	assert.Equal(t, 2*supersedeParkBaseDelay, supersedeParkDelay(1))
	assert.Equal(t, supersedeParkMaxDelay, supersedeParkDelay(40))
}

// Validates: R-2.8.9, R-2.8.11
//
// Ordinary supersedes keep no retry work; the same action superseded
// supersedeParkThreshold times in a row is parked in retry_work with a future
// retry time, which admission honors, so watch mode stops replanning it hot
// (D-059d).
func TestApplySupersededCompletion_ParksTheSameActionAfterAStreak(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	signals := &recordingSignals{}
	flow := &engineFlow{
		deps:             runtimeDeps{store: store, logger: testLogger(t)},
		supersedeStreaks: make(map[string]int),
		signals:          signals,
	}
	flow.retries.rowsByKey = map[RetryWorkKey]RetryWorkRow{}
	stale := &actionCompletion{ActionType: ActionRemoteMove, OldPath: "movesrc", Path: "movedst", ErrMsg: "remote item changed"}

	for range supersedeParkThreshold - 1 {
		require.NoError(t, flow.applySupersededCompletion(t.Context(), nil, stale, "test"))
	}
	rows, err := store.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows, "below the threshold a supersede keeps no retry work")

	require.NoError(t, flow.applySupersededCompletion(t.Context(), nil, stale, "test"))
	rows, err = store.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Greater(t, rows[0].NextRetryAt, time.Now().UnixNano(), "the wedged action waits for its backoff")
	assert.Positive(t, signals.retry, "watch arms the retry timer for the parked action")
}
