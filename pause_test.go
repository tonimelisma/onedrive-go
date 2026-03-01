package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDuration_GoSyntax(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"90s", 90 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			d, err := parseDuration(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, d)
		})
	}
}

func TestParseDuration_DaySuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			d, err := parseDuration(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, d)
		})
	}
}

func TestParseDuration_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
	}{
		{""},
		{"abc"},
		{"-1h"},
		{"0m"},
		{"0d"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			_, err := parseDuration(tt.input)
			assert.Error(t, err)
		})
	}
}

func TestNewPauseCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newPauseCmd()
	assert.Equal(t, "pause [duration]", cmd.Use)
	assert.Equal(t, "true", cmd.Annotations[skipConfigAnnotation])
}
