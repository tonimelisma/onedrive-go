// Package failures defines the repository's canonical runtime failure classes.
// Boundaries may add local context, but they should not invent a second class
// vocabulary once a result has been translated here.
package failures

const (
	classInvalidString = "invalid"
	logOwnerNoneString = "none"
)

// Class is the executable source of truth for the cross-cutting failure model
// documented in spec/design/error-model.md.
type Class uint8

const (
	ClassInvalid Class = iota
	ClassSuccess
	ClassShutdown
	ClassRetryableTransient
	ClassScopeBlockingTransient
	ClassActionable
	ClassFatal
)

// Valid reports whether the class is one of the documented runtime classes.
func (c Class) Valid() bool {
	return c >= ClassSuccess && c <= ClassFatal
}

func (c Class) String() string {
	switch c {
	case ClassInvalid:
		return classInvalidString
	case ClassSuccess:
		return "success"
	case ClassShutdown:
		return "shutdown"
	case ClassRetryableTransient:
		return "retryable transient"
	case ClassScopeBlockingTransient:
		return "scope-blocking transient"
	case ClassActionable:
		return "actionable"
	case ClassFatal:
		return "fatal"
	}

	return classInvalidString
}

// LogOwner identifies which boundary owns logging and presentation for a
// classified failure.
type LogOwner uint8

const (
	LogOwnerNone LogOwner = iota
	LogOwnerConfig
	LogOwnerCLI
	LogOwnerSync
)

func (o LogOwner) String() string {
	switch o {
	case LogOwnerNone:
		return logOwnerNoneString
	case LogOwnerConfig:
		return "config"
	case LogOwnerCLI:
		return "cli"
	case LogOwnerSync:
		return "sync"
	}

	return logOwnerNoneString
}
