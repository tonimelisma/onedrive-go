package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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
	assert.Equal(t, errclass.ClassActionable, classifyCommandError(&syncstore.StateStoreIncompatibleError{
		Reason: syncstore.StateStoreIncompatibleReasonCorrupt,
	}), "a state store that needs the user is actionable")
	assert.Equal(t, errclass.ClassFatal, classifyCommandError(errors.New("boom")))
	assert.Equal(t, errclass.ClassFatal, classifyCommandError(errors.Join(testPanicError(t), context.Canceled)),
		"a recovered panic is fatal even beside the cancellation it caused")
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
		// Validates: R-6.6.31, R-2.5.9
		// A state store that needs a decision (a newer version wrote it, it
		// is damaged, nothing says which version wrote it) fails the same way
		// on every restart until someone acts.
		{
			name: "state store needs the user",
			err: newPresentedError("the saved sync state for this drive can't be used",
				&syncstore.StateStoreIncompatibleError{Reason: syncstore.StateStoreIncompatibleReasonNewerVersion}),
			want: exitStateStoreNeedsUser,
		},
		{
			name: "recovered panic beside a state store that needs the user",
			err: errors.Join(panicguard.ErrPanic,
				&syncstore.StateStoreIncompatibleError{Reason: syncstore.StateStoreIncompatibleReasonCorrupt}),
			want: exitFailure,
		},
		{name: "login declined is retryable", err: graph.ErrLoginDeclined, want: exitFailure},
		{name: "login code expired is retryable", err: graph.ErrLoginCodeExpired, want: exitFailure},
		{name: "throttled is transient", err: fmt.Errorf("get: %w", graph.ErrThrottled), want: exitFailure},
		{name: "server error is transient", err: graph.ErrServerError, want: exitFailure},
		{name: "not found", err: graph.ErrNotFound, want: exitFailure},
		{name: "sync left work", err: newPresentedError("sync left 1 file unsynced", errSyncLeftWork), want: exitFailure},
		{name: "plain", err: errors.New("boom"), want: exitFailure},
		{
			name: "text alone never classifies",
			err:  errors.New("graph: not logged in; invalid configuration"),
			want: exitFailure,
		},
		// Validates: R-6.3.9
		// A drive that did not stop ends the watch with 1, so a service
		// manager restarts it, and a user's stop that met one is no clean
		// interrupt either.
		{name: "stuck drive", err: fmt.Errorf("stop: %w", multisync.ErrSyncStuck), want: exitFailure},
		{
			name:          "stuck drive on a user stop",
			err:           fmt.Errorf("stop: %w", multisync.ErrSyncStuck),
			stopRequested: true,
			want:          exitFailure,
		},
		// Validates: R-6.3.10
		// A recovered panic exits 1, never Go's 2, also when it ended a stop
		// the user asked for, travels with the cancellation it caused, or is
		// joined with a failure that alone would exit 77, 78 or 65 (a one-shot
		// pass's drive error beside a stuck close, a helper's panic beside the
		// command's own error).
		{name: "recovered panic", err: fmt.Errorf("sync --watch: %w", panicguard.ErrPanic), want: exitFailure},
		{
			name:          "recovered panic on a user stop",
			err:           errors.Join(panicguard.ErrPanic, context.Canceled),
			stopRequested: true,
			want:          exitFailure,
		},
		{
			name: "recovered panic beside a sign-in failure",
			err:  errors.Join(fmt.Errorf("session: %w", graph.ErrNotLoggedIn), panicguard.ErrPanic),
			want: exitFailure,
		},
		{
			name: "recovered panic beside an invalid config",
			err:  errors.Join(panicguard.ErrPanic, fmt.Errorf("loading config: %w", config.ErrInvalidConfig)),
			want: exitFailure,
		},
		// Validates: R-6.8.26
		// A command whose token source could not save a refreshed sign-in
		// did its work, but the next run may need a new sign-in: exit 1. A
		// watch stopped while the save is failing exits 1 too, and a failure
		// beside it keeps its own status.
		{name: "refreshed sign-in not saved", err: withTokenSaveFailure(testTokenSaveError(), nil), want: exitFailure},
		{
			name:          "watch stopped with a sign-in not saved",
			err:           withTokenSaveFailure(testTokenSaveError(), nil),
			stopRequested: true,
			want:          exitFailure,
		},
		{
			name:          "command stopped with a sign-in not saved",
			err:           withTokenSaveFailure(testTokenSaveError(), context.Canceled),
			stopRequested: true,
			want:          exitFailure,
		},
		{
			name: "sign-in failure beside a sign-in not saved",
			err:  withTokenSaveFailure(testTokenSaveError(), fmt.Errorf("session: %w", graph.ErrNotLoggedIn)),
			want: exitSignInRequired,
		},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, commandExitCode(tt.err, tt.stopRequested), tt.name)
	}
}

// testTokenSaveError is a token source's report of a refreshed sign-in it
// could not write.
func testTokenSaveError() error {
	return fmt.Errorf("%w to /tokens/sign-in.json; the next run may ask you to sign in again: disk full",
		graph.ErrTokenNotSaved)
}

// unsavedSignInSource is a token source whose refreshed sign-in could not be
// written; err is what its SaveError reports, nil once a write succeeded.
type unsavedSignInSource struct {
	err error
}

func (*unsavedSignInSource) Token(context.Context) (string, error) {
	return "made-up-access-token", nil
}

func (s *unsavedSignInSource) SaveError() error { return s.err }

// Validates: R-6.8.26
//
// After the command's work, main asks every runtime the command built for
// the sign-ins it could not save: a failing one is the command's error, with
// the file and the cause in its text, and one whose later write succeeded is
// no error. A watch stopped with one failing exits 1, not 0.
func TestCLIContext_TokenSaveFailureIsTheCommandsError(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{Logger: slog.New(slog.DiscardHandler)}
	source := &unsavedSignInSource{err: testTokenSaveError()}
	first := cc.newSessionRuntime(nil, cc.Logger)
	first.TokenSourceFn = func(string, *slog.Logger) (graph.TokenSource, error) { return source, nil }
	// The runtime a data command builds once its config resolves holds a
	// source for the same file: the failure is reported once.
	second := cc.newSessionRuntime(nil, cc.Logger)
	second.TokenSourceFn = first.TokenSourceFn
	for _, runtime := range []*driveops.SessionRuntime{first, second} {
		_, err := runtime.BootstrapClient("/tokens/sign-in.json")
		require.NoError(t, err)
	}

	err := withTokenSaveFailure(cc.tokenSaveFailure(), nil)
	require.ErrorIs(t, err, graph.ErrTokenNotSaved)
	assert.Equal(t, testTokenSaveError().Error(), err.Error())
	assert.Equal(t, exitFailure, commandExitCode(err, true))

	source.err = nil
	require.NoError(t, withTokenSaveFailure(cc.tokenSaveFailure(), nil))
	assert.Equal(t, exitSuccess, commandExitCode(withTokenSaveFailure(cc.tokenSaveFailure(), nil), true),
		"a failure a later save cleared changes nothing")
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
		errclass.ClassActionable: {
			err:  graph.ErrNotLoggedIn,
			want: []int{exitSignInRequired, exitConfigInvalid, exitStateStoreNeedsUser},
		},
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

// testPanicError is the error a boundary makes of a recovered panic.
func testPanicError(t *testing.T) error {
	t.Helper()

	return panicguard.Recovered(slog.New(slog.DiscardHandler), "in a test helper", "boom")
}

// Validates: R-6.3.10
//
// A helper goroutine of the command (the signal handler, the systemd
// watchdog, the performance logger) cancels the command with its panic as
// the cause. The panic replaces the cancellation it caused, or the nil of a
// command that finished regardless, and joins any other failure.
func TestWithCommandPanic_AHelpersPanicIsTheCommandsError(t *testing.T) {
	t.Parallel()

	panicErr := testPanicError(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(panicErr)

	require.ErrorIs(t, commandPanicCause(ctx), panicguard.ErrPanic)
	assert.Equal(t, panicErr, withCommandPanic(ctx, context.Canceled))
	assert.Equal(t, panicErr, withCommandPanic(ctx, nil))
	other := errors.New("upload failed")
	joined := withCommandPanic(ctx, other)
	require.ErrorIs(t, joined, panicguard.ErrPanic)
	require.ErrorIs(t, joined, other)
	assert.Equal(t, exitFailure, commandExitCode(withCommandPanic(ctx, context.Canceled), true),
		"a stop the panic caused is no stop the user asked for")

	stopped, stop := context.WithCancel(t.Context())
	stop()
	require.NoError(t, commandPanicCause(stopped))
	assert.Equal(t, context.Canceled, withCommandPanic(stopped, context.Canceled), "a plain stop stays a stop")
}

// panickingWriter is command output whose every write panics, as a bug in a
// command's output path would.
type panickingWriter struct{}

func (panickingWriter) Write([]byte) (int, error) {
	panic("output exploded")
}

// main recovers a panic on the command's own goroutine that nothing below
// recovered: one record with the stack in the log file, the error and the
// stack on stderr, and exit status 1. Unrecovered, it would end the process
// with Go's exit status 2 (the usage-error status) and nothing in the log
// file.
//
// Validates: R-6.3.10, R-6.6.31
func TestMainWithWriters_PanicExitsOne(t *testing.T) {
	setTestDriveHome(t)

	logPath := filepath.Join(t.TempDir(), "onedrive-go.log")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf("log_file = %q\n", logPath)), 0o600))

	var stderr bytes.Buffer
	exitCode, escaped := func() (code int, escaped any) {
		defer func() { escaped = recover() }()
		return mainWithWriters([]string{"--config", cfgPath, "drive", "list"}, panickingWriter{}, &stderr), nil
	}()

	require.Nil(t, escaped, "the panic escaped main")
	assert.Equal(t, exitFailure, exitCode)
	assert.Contains(t, stderr.String(), "internal error in onedrive-go: output exploded")
	assert.Contains(t, stderr.String(), "panickingWriter", "stderr carries the stack")
	record := logFileRecord(t, readLogFile(t, logPath), "Internal error in onedrive-go; this is a bug, please report it")
	assert.Equal(t, "output exploded", record["panic"])
	assert.Contains(t, record["stack"], "panickingWriter", "the log file carries the stack")
}

// A panic on the command's own goroutine completes the command's
// performance session: the log file holds exactly one summary, and it reads
// "error", as for a helper goroutine's panic.
//
// Validates: R-6.3.10
func TestMainWithWriters_APanicOnTheCommandsGoroutineWritesOnePerformanceSummary(t *testing.T) {
	setTestDriveHome(t)

	logPath := filepath.Join(t.TempDir(), "onedrive-go.log")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf("log_file = %q\n", logPath)), 0o600))

	var stderr bytes.Buffer
	exitCode, escaped := func() (code int, escaped any) {
		defer func() { escaped = recover() }()
		return mainWithWriters([]string{"--config", cfgPath, "drive", "list"}, panickingWriter{}, &stderr), nil
	}()

	require.Nil(t, escaped, "the panic escaped main")
	assert.Equal(t, exitFailure, exitCode)
	contents := readLogFile(t, logPath)
	assert.Equal(t, 1, strings.Count(contents, `"msg":"performance summary"`), contents)
	record := logFileRecord(t, contents, "performance summary")
	assert.Equal(t, "error", record["result"])
	assert.Equal(t, "onedrive-go drive list", record["perf_name"])
}

// panicCarriedBy finds the panic in a join, returns a bare panic as itself,
// and returns nil for an error that carries none.
//
// Validates: R-6.3.10
func TestPanicCarriedBy_FindsThePanicInAJoin(t *testing.T) {
	t.Parallel()

	recovered := testPanicError(t)
	other := errors.New("watch failed")

	assert.Same(t, recovered, panicCarriedBy(errors.Join(other, recovered)))
	assert.Same(t, recovered, panicCarriedBy(recovered))
	assert.NoError(t, panicCarriedBy(other))
	assert.NoError(t, panicCarriedBy(nil))
}
