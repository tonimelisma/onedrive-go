package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSize_ValidInputs(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"0", 0},
		{"", 0},
		{"1024", 1024},
		{"1KB", 1000},
		{"1KiB", 1024},
		{"10MB", 10_000_000},
		{"10MiB", 10_485_760},
		{"1GB", 1_000_000_000},
		{"1GiB", 1_073_741_824},
		{"50GB", 50_000_000_000},
		{"1TB", 1_000_000_000_000},
		{"100B", 100},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := parseSize(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseSize_InvalidInputs(t *testing.T) {
	for _, input := range []string{"abc", "MB", "-1"} {
		t.Run(input, func(t *testing.T) {
			_, err := parseSize(input)
			assert.Error(t, err)
		})
	}
}
