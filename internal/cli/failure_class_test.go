package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/failures"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-6.8.16
func TestClassifyCommandError(t *testing.T) {
	t.Parallel()

	assert.Equal(t, failures.ClassSuccess, classifyCommandError(nil))
	assert.Equal(t, failures.ClassShutdown, classifyCommandError(context.Canceled))
	assert.Equal(t, failures.ClassShutdown, classifyCommandError(context.DeadlineExceeded))
	assert.Equal(t, failures.ClassActionable, classifyCommandError(graph.ErrNotLoggedIn))
	assert.Equal(t, failures.ClassFatal, classifyCommandError(errors.New("boom")))
}

// Validates: R-6.8.16
func TestCommandFailurePresentationForClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		class    failures.Class
		exitCode int
		reason   string
	}{
		{class: failures.ClassSuccess, exitCode: 0, reason: "completed successfully"},
		{class: failures.ClassShutdown, exitCode: 1, reason: "shutdown or cancellation"},
		{class: failures.ClassActionable, exitCode: 1, reason: "needs user action"},
		{class: failures.ClassFatal, exitCode: 1, reason: "failed fatally"},
	}

	for _, tt := range tests {
		presentation := commandFailurePresentationForClass(tt.class)
		assert.Equal(t, tt.exitCode, presentation.ExitCode)
		assert.Contains(t, presentation.Reason, tt.reason)
		assert.NotEmpty(t, presentation.Action)
	}
}
