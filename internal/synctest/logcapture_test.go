package synctest

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.3.10
func TestLogCapture_KeepsEveryRecordWithItsAttributes(t *testing.T) {
	t.Parallel()

	capture, logger := NewLogCapture()
	logger.Debug("first", slog.String("drive", "Work"))
	logger.Error("second record", slog.Int("count", 2))

	records := capture.Records()
	require.Len(t, records, 2)
	assert.Equal(t, CapturedRecord{Level: slog.LevelDebug, Message: "first", Attrs: map[string]string{"drive": "Work"}}, records[0])
	assert.Equal(t, map[string]string{"count": "2"}, records[1].Attrs)
	assert.Equal(t, []CapturedRecord{records[1]}, capture.AtLeast(slog.LevelWarn), "a console at Warn shows only the Error")
	assert.True(t, capture.Contains("second"))
	assert.False(t, capture.Contains("third"))
}

// Validates: R-6.3.10
func TestPanicOnMessage_PanicsOnlyOnItsMessage(t *testing.T) {
	t.Parallel()

	capture, next := NewLogCapture()
	logger := slog.New(PanicOnMessage{Message: "boom", Next: next.Handler()}).With(slog.String("drive", "Work"))

	logger.Info("calm")
	assert.PanicsWithValue(t, "log call exploded", func() { logger.Info("boom") })
	assert.True(t, capture.Contains("calm"), "every other record reaches the next handler")
	assert.False(t, capture.Contains("boom"))
}
