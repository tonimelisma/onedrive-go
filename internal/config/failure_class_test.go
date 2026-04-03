package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/failures"
)

// Validates: R-6.8.16
func TestClassifyLoadOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		warnings []ConfigWarning
		want     failures.Class
	}{
		{name: "fatal_error", err: errors.New("bad config"), want: failures.ClassFatal},
		{name: "warnings_are_actionable", warnings: []ConfigWarning{{Message: "bad key"}}, want: failures.ClassActionable},
		{name: "clean_success", want: failures.ClassSuccess},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outcome := ClassifyLoadOutcome(tt.err, tt.warnings)
			assert.Equal(t, tt.want, outcome.Class)
			assert.Equal(t, len(tt.warnings), outcome.WarningCount)
		})
	}
}
