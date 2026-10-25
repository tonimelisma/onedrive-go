package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
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
	assert.Equal(t, errclass.ClassActionable, classifyCommandError(fmt.Errorf("load: %w", config.ErrInvalidConfig)))
	assert.Equal(t, errclass.ClassFatal, classifyCommandError(errors.New("boom")))
}

// Validates: R-6.6.31, R-6.6.27
func TestCommandExitCode(t *testing.T) {
	t.Parallel()

	signInPresented := newPresentedError("Drive could not start syncing", fmt.Errorf("session: %w", graph.ErrNotLoggedIn))
	tests := []struct {
		name          string
		err           error
		stopRequested bool
		want          int
	}{
		{name: "success", err: nil, want: exitSuccess},
		{name: "user stop", err: context.Canceled, stopRequested: true, want: exitInterrupted},
		{name: "user stop with deadline cause", err: context.DeadlineExceeded, stopRequested: true, want: exitInterrupted},
		{name: "internal cancellation fails", err: context.Canceled, want: exitFailure},
		{name: "timeout fails", err: fmt.Errorf("list: %w", context.DeadlineExceeded), want: exitFailure},
		{name: "usage", err: &usageError{commandPath: "onedrive-go get", err: errors.New("bad args")}, want: exitUsage},
		{name: "no saved login", err: fmt.Errorf("session: %w", graph.ErrNotLoggedIn), want: exitSignInRequired},
		{name: "login rejected", err: fmt.Errorf("list: %w", graph.ErrUnauthorized), want: exitSignInRequired},
		{
			name: "login expired",
			err:  errors.Join(graph.ErrLoginExpired, graph.ErrUnauthorized),
			want: exitSignInRequired,
		},
		{name: "sign-in behind presented error", err: signInPresented, want: exitSignInRequired},
		{name: "config", err: fmt.Errorf("loading config: %w", config.ErrInvalidConfig), want: exitConfigInvalid},
		{name: "login declined is retryable", err: graph.ErrLoginDeclined, want: exitFailure},
		{name: "login code expired is retryable", err: graph.ErrLoginCodeExpired, want: exitFailure},
		{name: "throttled is transient", err: fmt.Errorf("get: %w", graph.ErrThrottled), want: exitFailure},
		{name: "server error is transient", err: graph.ErrServerError, want: exitFailure},
		{name: "not found", err: graph.ErrNotFound, want: exitFailure},
		{name: "plain", err: errors.New("boom"), want: exitFailure},
		{
			name: "text alone never classifies",
			err:  errors.New("graph: not logged in; invalid configuration"),
			want: exitFailure,
		},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, commandExitCode(tt.err, tt.stopRequested), tt.name)
	}
}

// Validates: R-6.6.31
func TestCommandExitCode_EveryFailureClassHasAStatus(t *testing.T) {
	t.Parallel()

	// One representative error per errclass the command classifier can
	// produce; each must land on a documented exit status.
	byClass := map[errclass.Class]struct {
		err  error
		want []int
	}{
		errclass.ClassSuccess:    {err: nil, want: []int{exitSuccess}},
		errclass.ClassShutdown:   {err: context.Canceled, want: []int{exitFailure, exitInterrupted}},
		errclass.ClassActionable: {err: graph.ErrNotLoggedIn, want: []int{exitSignInRequired, exitConfigInvalid}},
		errclass.ClassFatal:      {err: errors.New("boom"), want: []int{exitFailure}},
	}

	for class, tt := range byClass {
		assert.Equal(t, class, classifyCommandError(tt.err), class.String())
		assert.Contains(t, tt.want, commandExitCode(tt.err, false), class.String())
		assert.Contains(t, tt.want, commandExitCode(tt.err, true), class.String())
	}
}

// Validates: R-6.6.31
func TestMainWithWriters_ConfigErrorExits78(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("transfer_worker = 8\n"), 0o600))

	var stdout, stderr bytes.Buffer
	exitCode := mainWithWriters([]string{"--config", cfgPath, "ls", "/"}, &stdout, &stderr)

	assert.Equal(t, exitConfigInvalid, exitCode)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "unknown config key")
}

// Validates: R-6.6.31
func TestMainWithWriters_SignInRequiredExits77(t *testing.T) {
	setTestDriveHome(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `["personal:user@example.com"]` + "\n" +
		`sync_dir = "` + filepath.Join(dir, "OneDrive") + `"` + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	var stdout, stderr bytes.Buffer
	exitCode := mainWithWriters([]string{"--config", cfgPath, "ls", "/"}, &stdout, &stderr)

	assert.Equal(t, exitSignInRequired, exitCode, stderr.String())
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "onedrive-go login")
}
