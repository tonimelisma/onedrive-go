package config

import "github.com/tonimelisma/onedrive-go/internal/errclass"

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
