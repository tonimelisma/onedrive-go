package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-6.8.16
func TestClassifyCommandError(t *testing.T) {
	t.Parallel()

	assert.Equal(t, errclass.ClassSuccess, classifyCommandError(nil))
	assert.Equal(t, errclass.ClassShutdown, classifyCommandError(context.Canceled))
	assert.Equal(t, errclass.ClassShutdown, classifyCommandError(context.DeadlineExceeded))
	assert.Equal(t, errclass.ClassActionable, classifyCommandError(graph.ErrNotLoggedIn))
	assert.Equal(t, errclass.ClassActionable, classifyCommandError(graph.ErrUnauthorized))
	assert.Equal(t, errclass.ClassFatal, classifyCommandError(errors.New("boom")))
}

// Validates: R-6.8.16
func TestCommandFailurePresentationForClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		class    errclass.Class
		exitCode int
		reason   string
	}{
		{class: errclass.ClassInvalid, exitCode: 1, reason: "invalid failure class"},
		{class: errclass.ClassSuccess, exitCode: 0, reason: "completed successfully"},
		{class: errclass.ClassShutdown, exitCode: 1, reason: "shutdown or cancellation"},
		{class: errclass.ClassActionable, exitCode: 1, reason: "needs user action"},
		{class: errclass.ClassRetryableTransient, exitCode: 1, reason: "failed temporarily"},
		{class: errclass.ClassScopeBlockingTransient, exitCode: 1, reason: "failed temporarily"},
		{class: errclass.ClassFatal, exitCode: 1, reason: "failed fatally"},
	}

	for _, tt := range tests {
		presentation := commandFailurePresentationForClass(tt.class)
		assert.Equal(t, tt.exitCode, presentation.ExitCode)
		assert.Contains(t, presentation.Reason, tt.reason)
		assert.NotEmpty(t, presentation.Action)
	}

	presentation := commandFailurePresentationForClass(errclass.Class(255))
	assert.Equal(t, 1, presentation.ExitCode)
	assert.Contains(t, presentation.Reason, "failed fatally")
}
