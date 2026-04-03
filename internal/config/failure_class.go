package config

import "github.com/tonimelisma/onedrive-go/internal/failures"

// LoadOutcome describes the domain class of a config read plus whether the
// caller must surface non-fatal warnings to the user.
type LoadOutcome struct {
	Class        failures.Class
	WarningCount int
}

// ClassifyLoadOutcome converts config load results into the repository's
// shared failure model. Fatal parse/read errors stop the caller. Warnings are
// actionable but do not prevent informational commands from continuing.
func ClassifyLoadOutcome(err error, warnings []ConfigWarning) LoadOutcome {
	switch {
	case err != nil:
		return LoadOutcome{Class: failures.ClassFatal, WarningCount: len(warnings)}
	case len(warnings) > 0:
		return LoadOutcome{Class: failures.ClassActionable, WarningCount: len(warnings)}
	default:
		return LoadOutcome{Class: failures.ClassSuccess}
	}
}
