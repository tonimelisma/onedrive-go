package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverCIDFiles_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	ids := discoverCIDFiles(dir, slog.Default())
	assert.Nil(t, ids)
}

func TestDiscoverCIDFiles_EmptyDirString(t *testing.T) {
	ids := discoverCIDFiles("", slog.Default())
	assert.Nil(t, ids)
}

func TestDiscoverCIDFiles_SortedOutput(t *testing.T) {
	dir := t.TempDir()

	// Write files in reverse alphabetical order — output should be sorted.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token_personal_z@example.com.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token_personal_a@example.com.json"), []byte(`{}`), 0o600))

	ids := discoverCIDFiles(dir, slog.Default())
	require.Len(t, ids, 2)
	assert.Equal(t, "personal:a@example.com", ids[0].String())
	assert.Equal(t, "personal:z@example.com", ids[1].String())
}

func TestDiscoverFilesForEmail_EmptyInputs(t *testing.T) {
	dir := t.TempDir()

	assert.Nil(t, discoverFilesForEmail("", "state_", ".db", "a@b.com", slog.Default()))
	assert.Nil(t, discoverFilesForEmail(dir, "state_", ".db", "", slog.Default()))
}

func TestDiscoverCIDFiles_ReadDirError(t *testing.T) {
	ids := discoverCIDFilesWithIO("/missing", slog.Default(), configIO{
		readManagedDir: func(path string) ([]os.DirEntry, error) {
			return nil, errors.New("boom")
		},
	})

	assert.Nil(t, ids)
}

func TestContainsEmailBoundary_EdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		fname  string
		needle string
		want   bool
	}{
		{"exact match at end", "state_personal_a@b.com.db", "_a@b.com", true},
		{"followed by underscore", "state_sharepoint_a@b.com_site_lib.db", "_a@b.com", true},
		{"substring collision", "state_personal_ba@b.com.db", "_a@b.com", false},
		{"no match at all", "state_personal_x@y.com.db", "_a@b.com", false},
		{"needle at very end (no suffix)", "account_personal_a@b.com", "_a@b.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containsEmailBoundary(tt.fname, tt.needle))
		})
	}
}
