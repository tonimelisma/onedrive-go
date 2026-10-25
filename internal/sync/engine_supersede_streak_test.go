package sync

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
