package cli

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingHandler records the messages it received and can be made to fail.
type recordingHandler struct {
	failWith error
	records  []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler requires a value receiver for Record
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Message)

	return h.failWith
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// Validates: R-4.7.1
//
// The console and file handlers fail independently: a terminal goes away when
// output is piped into a command that exits early, a log file fills its disk.
// Returning at the first error let a broken console silently stop file
// logging, which is the half most worth keeping.
func TestMultiHandler_FailingHandlerDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()

	console := &recordingHandler{failWith: errors.New("broken pipe")}
	file := &recordingHandler{}

	handler := &multiHandler{handlers: []slog.Handler{console, file}}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "sync started", 0)
	err := handler.Handle(t.Context(), record)

	require.Error(t, err, "the failure is still reported")
	assert.Contains(t, err.Error(), "broken pipe")
	assert.Equal(t, []string{"sync started"}, file.records,
		"the file handler must still receive the record")
}

// Validates: R-4.7.1
func TestMultiHandler_ReportsEveryFailure(t *testing.T) {
	t.Parallel()

	first := &recordingHandler{failWith: errors.New("console gone")}
	second := &recordingHandler{failWith: errors.New("disk full")}

	handler := &multiHandler{handlers: []slog.Handler{first, second}}

	err := handler.Handle(t.Context(), slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "console gone")
	assert.Contains(t, err.Error(), "disk full",
		"a second failure must not be hidden by the first")
}

// Validates: R-4.7.1
func TestMultiHandler_AllSucceedingReportsNoError(t *testing.T) {
	t.Parallel()

	first := &recordingHandler{}
	second := &recordingHandler{}

	handler := &multiHandler{handlers: []slog.Handler{first, second}}

	require.NoError(t, handler.Handle(t.Context(), slog.NewRecord(time.Now(), slog.LevelInfo, "ok", 0)))
	assert.Equal(t, []string{"ok"}, first.records)
	assert.Equal(t, []string{"ok"}, second.records)
}
