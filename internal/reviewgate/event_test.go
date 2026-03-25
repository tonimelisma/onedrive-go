package reviewgate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.3.5
func TestLoadPullRequest(t *testing.T) {
	eventFile := filepath.Join(t.TempDir(), "event.json")
	eventBody := `{
		"repository": {"full_name": "fallback/repo"},
		"pull_request": {
			"number": 326,
			"draft": false,
			"changed_files": 17,
			"head": {"sha": "abc123"},
			"base": {"repo": {"full_name": "tonimelisma/onedrive-go"}}
		}
	}`

	err := os.WriteFile(eventFile, []byte(eventBody), 0o600)
	require.NoError(t, err)

	pullRequest, err := LoadPullRequest(eventFile, "")

	require.NoError(t, err)
	assert.Equal(t, PullRequest{
		Repository:       "tonimelisma/onedrive-go",
		Number:           326,
		Draft:            false,
		HeadSHA:          "abc123",
		ChangedFileCount: 17,
	}, pullRequest)
}
