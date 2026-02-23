package config

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_UnknownKey_TopLevel(t *testing.T) {
	path := writeTestConfig(t, `unknown_section = "value"`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestLoad_UnknownKey_TypoInFlatKey(t *testing.T) {
	//nolint:misspell // intentional typo to test unknown key detection
	path := writeTestConfig(t, `parralel_downloads = 4`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.Contains(t, err.Error(), "parallel_downloads")
}

func TestLoad_UnknownKey_TypoInFilter(t *testing.T) {
	path := writeTestConfig(t, `skip_file = ["*.tmp"]`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_files")
}

func TestLoad_UnknownKey_NoSuggestion(t *testing.T) {
	path := writeTestConfig(t, `completely_unrelated_key = true`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
	assert.NotContains(t, err.Error(), "did you mean")
}

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

func TestLoad_TypoInDriveSection_Suggestion(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
aliaz = "home"
`)
	_, err := Load(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean")
	assert.Contains(t, err.Error(), "alias")
}

func TestLoad_DriveSection_ValidKeysPass(t *testing.T) {
	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
alias = "home"
enabled = true
remote_path = "/"
drive_id = "abc"
skip_dotfiles = true
skip_dirs = ["vendor"]
skip_files = ["*.log"]
poll_interval = "10m"
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

// --- Edge case: known parent with sub-field is not flagged ---

func TestBuildGlobalKeyError_KnownParent_SubField(t *testing.T) {
	// A nested key like "bandwidth_schedule.time" has a known parent,
	// so buildGlobalKeyError should return nil.
	err := buildGlobalKeyError("bandwidth_schedule.time")
	assert.Nil(t, err)
}

func TestBuildGlobalKeyError_UnknownParent_SubField(t *testing.T) {
	// An unknown nested key should still return an error.
	err := buildGlobalKeyError("nonexistent_section.field")
	assert.NotNil(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestKnownGlobalKeysList_Sorted(t *testing.T) {
	// Verify the list is sorted for deterministic Levenshtein suggestions.
	assert.True(t, sort.StringsAreSorted(knownGlobalKeysList),
		"knownGlobalKeysList must be sorted")
}

func TestKnownDriveKeysList_Sorted(t *testing.T) {
	// Verify the list is sorted for deterministic Levenshtein suggestions.
	assert.True(t, sort.StringsAreSorted(knownDriveKeysList),
		"knownDriveKeysList must be sorted")
}
