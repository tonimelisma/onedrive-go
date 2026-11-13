package panicguard

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// onlyRecord is the one record capture holds.
func onlyRecord(t *testing.T, capture *synctest.LogCapture) synctest.CapturedRecord {
	t.Helper()

	records := capture.Records()
	require.Len(t, records, 1, "a recovered panic is logged once")

	return records[0]
}

// panicAndRecover panics with value on this goroutine and returns what the
// boundary made of it.
func panicAndRecover(logger *slog.Logger, value any, attrs ...slog.Attr) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Recovered(logger, "in the test helper", r, attrs...)
		}
	}()

	explodingHelper(value)

	return nil
}

func explodingHelper(value any) {
	panic(value)
}

// Validates: R-6.3.10, R-6.3.8
//
// One record at Error carries the caller's attributes, the panic value and
// the stack of the goroutine that panicked, which still holds the frame that
// raised it; the error wraps ErrPanic and names where it was caught.
func TestRecovered_LogsTheValueAndTheStackAtErrorAndWrapsErrPanic(t *testing.T) {
	t.Parallel()

	capture, logger := synctest.NewLogCapture()
	err := panicAndRecover(logger, "boom", slog.String("drive", "Work"))

	require.ErrorIs(t, err, ErrPanic)
	assert.Equal(t,
		"internal error in the test helper: boom. This is a bug; please report it, with the log file if you have one",
		err.Error())

	record := onlyRecord(t, capture)
	assert.Equal(t, slog.LevelError, record.Level)
	assert.Equal(t, "Internal error in the test helper; this is a bug, please report it", record.Message)
	assert.Equal(t, "Work", record.Attrs["drive"])
	assert.Equal(t, "boom", record.Attrs["panic"])
	assert.Contains(t, record.Attrs["stack"], "explodingHelper", "the stack is the panicking goroutine's, taken before it unwound")
	assert.Equal(t, record.Attrs["stack"], Stack(err), "the error carries the stack it logged")
}

// Validates: R-6.3.10
//
// A boundary that has no logger of its own still leaves a record: a panic is
// never silent.
func TestRecovered_WithoutALoggerLogsThroughTheDefault(t *testing.T) {
	// Not parallel: replaces the process-wide default logger.
	capture, logger := synctest.NewLogCapture()
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	err := panicAndRecover(nil, errors.New("nil map"))

	require.ErrorIs(t, err, ErrPanic)
	record := onlyRecord(t, capture)
	assert.Equal(t, "nil map", record.Attrs["panic"])
	assert.Contains(t, record.Attrs["stack"], "explodingHelper")
}

// Validates: R-6.3.10
//
// The CLI prints the stack under the error that ends a command, however many
// layers wrapped the panic's error on its way there.
func TestStack_FindsThePanicInsideAWrappedError(t *testing.T) {
	t.Parallel()

	err := panicAndRecover(slog.New(slog.DiscardHandler), 42)
	wrapped := errors.Join(errors.New("another failure"), fmt.Errorf("sync --watch: %w", err))

	assert.Contains(t, Stack(wrapped), "explodingHelper")
	require.ErrorIs(t, wrapped, ErrPanic)
	assert.Empty(t, Stack(errors.New("plain failure")), "an error that carries no panic has no stack")
	assert.Empty(t, Stack(nil))
}
