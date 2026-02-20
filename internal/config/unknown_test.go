package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_UnknownKey_TopLevel(t *testing.T) {
	path := writeTestConfig(t, `
unknown_section = "value"
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestLoad_UnknownKey_InSection(t *testing.T) {
	//nolint:misspell // intentional typo to test unknown key detection
	path := writeTestConfig(t, "[transfers]\nparralel_downloads = 4\n")
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.Contains(t, err.Error(), "parallel_downloads")
}

func TestLoad_UnknownKey_TypoInFilter(t *testing.T) {
	path := writeTestConfig(t, `
[filter]
skip_file = ["*.tmp"]
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_files")
}

func TestLoad_UnknownKey_NoSuggestion(t *testing.T) {
	path := writeTestConfig(t, `
[filter]
completely_unrelated_key = true
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.NotContains(t, err.Error(), "did you mean")
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b     string
		expected int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"skip_file", "skip_files", 1},
		{"par" + "ralel_downloads", "parallel_downloads", 2},
		{"completely_different", "xyz", 19},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			assert.Equal(t, tt.expected, levenshtein(tt.a, tt.b))
		})
	}
}

func TestClosestMatch_Found(t *testing.T) {
	known := []string{"skip_files", "skip_dirs", "skip_dotfiles"}
	assert.Equal(t, "skip_files", closestMatch("skip_file", known))
	assert.Equal(t, "skip_dirs", closestMatch("skip_dir", known))
}

func TestClosestMatch_NotFound(t *testing.T) {
	known := []string{"skip_files", "skip_dirs"}
	assert.Equal(t, "", closestMatch("completely_unrelated", known))
}
