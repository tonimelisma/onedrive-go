package config

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// LoadOutcome describes the domain class of a config read plus whether the
// caller must surface non-fatal warnings to the user.
type LoadOutcome struct {
	Class        errclass.Class
	WarningCount int
}

// ClassifyLoadOutcome converts config load results into the repository's
// shared failure model. Fatal parse/read errors stop the caller. Warnings are
// actionable but do not prevent informational commands from continuing.
func ClassifyLoadOutcome(err error, warnings []ConfigWarning) LoadOutcome {
	switch {
	case err != nil:
		return LoadOutcome{Class: errclass.ClassFatal, WarningCount: len(warnings)}
	case len(warnings) > 0:
		return LoadOutcome{Class: errclass.ClassActionable, WarningCount: len(warnings)}
	default:
		return LoadOutcome{Class: errclass.ClassSuccess}
	}
}

// ErrInvalidConfig marks a failure that only a change to the configuration
// can fix: the file could not be read or parsed, a value failed validation,
// or a drive selector matched nothing. Callers branch on it with errors.Is
// (the CLI exits 78, EX_CONFIG, so a service manager does not restart a run
// that would fail the same way). The wrapped error's text is unchanged.
var ErrInvalidConfig = errors.New("invalid configuration")

// invalidConfigError tags err as ErrInvalidConfig without changing its text
// or hiding its chain from errors.Is/As.
type invalidConfigError struct {
	err error
}

func (e *invalidConfigError) Error() string { return e.err.Error() }

func (e *invalidConfigError) Unwrap() error { return e.err }

func (e *invalidConfigError) Is(target error) bool { return target == ErrInvalidConfig }

// markInvalidConfig tags a config failure at the package's exported
// boundary. It is idempotent, so entrypoints that call one another do not
// stack tags.
func markInvalidConfig(err error) error {
	if err == nil || errors.Is(err, ErrInvalidConfig) {
		return err
	}

	return &invalidConfigError{err: err}
}
