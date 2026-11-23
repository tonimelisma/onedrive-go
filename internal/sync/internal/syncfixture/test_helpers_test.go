package syncfixture

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// A run that returns once the awaited event is recorded hands its one result
// to the wait; the wait gives it back, so the caller's next read of the run's
// result does not wait on an empty channel.
func TestWaitUntilSeenOrExit_HandsBackAResultItTook(t *testing.T) {
	t.Parallel()

	recorder := NewDebugEventRecorder()
	recorder.Record(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventShutdownStarted})
	<-recorder.Events // the wait finds the event only in the history

	runErr := errors.New("the run's own result")
	done := make(chan error, 1)
	done <- runErr

	// The first look, before the wait, misses the event; every later one
	// finds it, as when the run records it just before it returns.
	looks := 0
	match := func(syncdomain.EngineDebugEvent) bool {
		looks++
		return looks > 1
	}

	result := recorder.WaitUntilSeenOrExit(t, done, match, "shutdown started")

	assert.Equal(t, runErr, ReceiveWithin(t, result, "the run's result the wait took"))
}
