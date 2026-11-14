package errclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.8.16
func TestClassString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		class Class
		want  string
	}{
		{name: "invalid_zero", class: ClassInvalid, want: "invalid"},
		{name: "success", class: ClassSuccess, want: "success"},
		{name: "shutdown", class: ClassShutdown, want: "shutdown"},
		{name: "superseded", class: ClassSuperseded, want: "superseded"},
		{name: "retryable", class: ClassRetryableTransient, want: "retryable transient"},
		{name: "scope_blocking", class: ClassScopeBlockingTransient, want: "scope-blocking transient"},
		{name: "actionable", class: ClassActionable, want: "actionable"},
		{name: "fatal", class: ClassFatal, want: "fatal"},
		{name: "unknown_value", class: Class(255), want: "invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.class.String())
		})
	}
}
