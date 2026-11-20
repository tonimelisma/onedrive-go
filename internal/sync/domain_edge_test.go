package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAsciiLower validates the allocation-free ASCII lowering helper used by
// bundled junk matching.
func TestAsciiLower(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"already-lower.txt", "already-lower.txt"},
		{"ALLCAPS.TXT", "allcaps.txt"},
		{"MixedCase.Partial", "mixedcase.partial"},
		{"", ""},
		{"noext", "noext"},
		{"café.txt", "café.txt"},       // non-ASCII unchanged
		{"FILE.DB-WAL", "file.db-wal"}, // matches excluded suffix
		{"~$Document.docx", "~$document.docx"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			got := asciiLower(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}
