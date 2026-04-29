package config

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-4.8.1
func TestLoad_UnknownKey_TopLevel(t *testing.T) {
	path := writeTestConfig(t, `unknown_section = "value"`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

// Validates: R-4.8.1
func TestLoad_UnknownKey_TypoInFlatKey(t *testing.T) {
	path := writeTestConfig(t, `transfer_worker = 8`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.Contains(t, err.Error(), "transfer_workers")
}

// Validates: R-4.8.1
func TestLoad_UnknownKey_TypoInFilter(t *testing.T) {
	path := writeTestConfig(t, `poll_interva = "5m"`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll_interval")
}

// Validates: R-4.8.1
func TestLoad_UnknownKey_NoSuggestion(t *testing.T) {
	path := writeTestConfig(t, `completely_unrelated_key = true`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.NotContains(t, err.Error(), "did you mean")
}

// Validates: R-4.8.1
func TestLoad_UnknownKeyInDriveSection(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
unknown_field = "value"
`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown key")
	assert.Contains(t, err.Error(), "personal:toni@outlook.com")
}

// Validates: R-4.8.1
func TestLoad_TypoInDriveSection_Suggestion(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
dispaly_name = "home"
`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean")
	assert.Contains(t, err.Error(), "display_name")
}

func TestLoad_DriveSection_ValidKeysPass(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"
paused = false
owner = "Alice Smith"
ignored_dirs = ["build"]
included_dirs = ["Documents"]
ignored_paths = ["*.tmp"]
ignore_dotfiles = true
ignore_junk_files = true
follow_symlinks = true
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)
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
		{"poll_interva", "poll_interval", 1},
		{"transfer_worker", "transfer_workers", 1},
		{"completely_different", "xyz", 19},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			assert.Equal(t, tt.expected, levenshtein(tt.a, tt.b))
		})
	}
}

func TestClosestMatch_Found(t *testing.T) {
	known := []string{"transfer_workers", "poll_interval", "display_name"}
	assert.Equal(t, "transfer_workers", closestMatch("transfer_worker", known))
	assert.Equal(t, "poll_interval", closestMatch("poll_interva", known))
}

func TestClosestMatch_NotFound(t *testing.T) {
	known := []string{"transfer_workers", "poll_interval"}
	assert.Empty(t, closestMatch("completely_unrelated", known))
}

func TestBuildGlobalKeyError_UnknownParent_SubField(t *testing.T) {
	// An unknown nested key should still return an error.
	err := buildGlobalKeyError("nonexistent_section.field")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestNewKnownGlobalKeysList_Sorted(t *testing.T) {
	// Verify the list is sorted for deterministic Levenshtein suggestions.
	assert.True(t, sort.StringsAreSorted(newKnownGlobalKeysList()),
		"newKnownGlobalKeysList() must be sorted")
}

func TestNewKnownDriveKeysList_Sorted(t *testing.T) {
	// Verify the list is sorted for deterministic Levenshtein suggestions.
	assert.True(t, sort.StringsAreSorted(newKnownDriveKeysList()),
		"newKnownDriveKeysList() must be sorted")
}
