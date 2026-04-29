// Package errclass defines the repository's canonical runtime failure classes,
// documented in spec/design/error-model.md. Boundary packages consume this
// shared vocabulary rather than re-declaring local enums.
package errclass

const (
	classInvalidString = "invalid"
)

// Class is the executable source of truth for the cross-cutting failure model
// documented in spec/design/error-model.md.
type Class uint8

const (
	ClassInvalid Class = iota
	ClassSuccess
	ClassShutdown
	ClassSuperseded
	ClassRetryableTransient
	ClassBlockScopeingTransient
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
	case ClassSuperseded:
		return "superseded"
	case ClassRetryableTransient:
		return "retryable transient"
	case ClassBlockScopeingTransient:
		return "scope-blocking transient"
	case ClassActionable:
		return "actionable"
	case ClassFatal:
		return "fatal"
	}

	return classInvalidString
}
