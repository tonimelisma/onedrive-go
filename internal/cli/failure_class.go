package cli

import (
	"context"
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type commandFailurePresentation struct {
	Reason   string
	Action   string
	ExitCode int
}

func classifyCommandError(err error) errclass.Class {
	switch {
	case err == nil:
		return errclass.ClassSuccess
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errclass.ClassShutdown
	case errors.Is(err, graph.ErrNotLoggedIn), errors.Is(err, graph.ErrUnauthorized):
		return errclass.ClassActionable
	default:
		return errclass.ClassFatal
	}
}

func commandFailurePresentationForClass(class errclass.Class) commandFailurePresentation {
	switch class {
	case errclass.ClassInvalid:
		return commandFailurePresentation{
			Reason:   "the command failed because an invalid failure class escaped classification",
			Action:   "inspect the error output and fix the underlying issue before retrying",
			ExitCode: 1,
		}
	case errclass.ClassSuccess:
		return commandFailurePresentation{
			Reason:   "command completed successfully",
			Action:   "no action required",
			ExitCode: 0,
		}
	case errclass.ClassShutdown:
		return commandFailurePresentation{
			Reason:   "the command stopped because shutdown or cancellation was requested",
			Action:   "rerun the command if you still want the work completed",
			ExitCode: 1,
		}
	case errclass.ClassActionable:
		return commandFailurePresentation{
			Reason:   "the command needs user action before it can succeed",
			Action:   "follow the command's remediation hint and rerun it",
			ExitCode: 1,
		}
	case errclass.ClassRetryableTransient, errclass.ClassBlockScopeingTransient:
		return commandFailurePresentation{
			Reason:   "the command failed temporarily",
			Action:   "rerun the command after the underlying transient issue clears",
			ExitCode: 1,
		}
	case errclass.ClassFatal:
		return commandFailurePresentation{
			Reason:   "the command failed fatally",
			Action:   "inspect the error output and fix the underlying issue before retrying",
			ExitCode: 1,
		}
	}

	return commandFailurePresentation{
		Reason:   "the command failed fatally",
		Action:   "inspect the error output and fix the underlying issue before retrying",
		ExitCode: 1,
	}
}
