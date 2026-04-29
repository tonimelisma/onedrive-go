package errclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.8.16
func TestClassStringAndValidity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		class   Class
		want    string
		isValid bool
	}{
		{name: "invalid_zero", class: ClassInvalid, want: "invalid", isValid: false},
		{name: "success", class: ClassSuccess, want: "success", isValid: true},
		{name: "shutdown", class: ClassShutdown, want: "shutdown", isValid: true},
		{name: "superseded", class: ClassSuperseded, want: "superseded", isValid: true},
		{name: "retryable", class: ClassRetryableTransient, want: "retryable transient", isValid: true},
		{name: "scope_blocking", class: ClassBlockScopeingTransient, want: "scope-blocking transient", isValid: true},
		{name: "actionable", class: ClassActionable, want: "actionable", isValid: true},
		{name: "fatal", class: ClassFatal, want: "fatal", isValid: true},
		{name: "unknown_value", class: Class(255), want: "invalid", isValid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.class.String())
			assert.Equal(t, tt.isValid, tt.class.Valid())
		})
	}
}
