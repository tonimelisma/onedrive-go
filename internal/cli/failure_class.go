package cli

import (
	"context"
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Process exit statuses. They are a stable contract for scripts and service
// managers (R-6.6.31): 77 and 78 are the sysexits.h EX_NOPERM and EX_CONFIG
// values, and they mean "running again unchanged will fail the same way", so
// a systemd unit can stop restarting with RestartPreventExitStatus=77 78.
// Every other failure, transient or not, stays 1.
const (
	exitSuccess        = 0
	exitFailure        = 1
	exitUsage          = 2
	exitSignInRequired = 77
	exitConfigInvalid  = 78
	// exitInterrupted is the conventional status for a run stopped by SIGINT
	// (128 + 2), so shells and service managers can tell it from a failure.
	exitInterrupted = 130
)

// classifyCommandError maps a command's final error into the shared failure
// model (R-6.8.16). It branches only on sentinels and types, never on text.
func classifyCommandError(err error) errclass.Class {
	switch {
	case err == nil:
		return errclass.ClassSuccess
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errclass.ClassShutdown
	case isSignInRequired(err), errors.Is(err, config.ErrInvalidConfig):
		return errclass.ClassActionable
	default:
		return errclass.ClassFatal
	}
}

// isSignInRequired reports an error that only a new sign-in fixes: no saved
// login, or one Microsoft no longer accepts (ErrLoginExpired always travels
// with ErrUnauthorized). A sign-in the user canceled or let expire during
// `login` is not here: rerunning `login` is the fix, so it exits 1.
func isSignInRequired(err error) bool {
	return errors.Is(err, graph.ErrNotLoggedIn) || errors.Is(err, graph.ErrUnauthorized)
}

// commandExitCode is the process exit status for a command's final error.
// stopRequested is true when the user asked the run to stop (SIGINT or
// SIGTERM); only then does a cancellation exit 130 rather than fail.
func commandExitCode(err error, stopRequested bool) int {
	if err == nil {
		return exitSuccess
	}

	if stopRequested && classifyCommandError(err) == errclass.ClassShutdown {
		return exitInterrupted
	}

	var usage *usageError
	switch {
	case errors.As(err, &usage):
		return exitUsage
	case isSignInRequired(err):
		return exitSignInRequired
	case errors.Is(err, config.ErrInvalidConfig):
		return exitConfigInvalid
	default:
		return exitFailure
	}
}
