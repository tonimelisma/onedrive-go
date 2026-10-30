package loglevel

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.6.30
func TestNotice_SitsBetweenInfoAndWarn(t *testing.T) {
	t.Parallel()

	assert.Greater(t, Notice, slog.LevelInfo, "the log file at INFO keeps notices")
	assert.Less(t, Notice, slog.LevelWarn, "a notice is not a warning")
	assert.Equal(t, "NOTICE", Name(Notice))
	assert.Equal(t, "WARN", Name(slog.LevelWarn))
}

// Validates: R-6.6.30
func TestReplaceLevelName_PrintsNoticeInStructuredOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: ReplaceLevelName}))
	logger.Log(context.Background(), Notice, "OneDrive is reachable again")
	logger.Warn("still a warning")

	assert.Contains(t, buf.String(), `"level":"NOTICE"`)
	assert.Contains(t, buf.String(), `"level":"WARN"`)
	assert.NotContains(t, buf.String(), "INFO+2")
}
