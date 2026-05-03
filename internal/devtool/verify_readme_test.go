package devtool

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.1
func TestRunReadmeStatusRejectsStaleRoadmapLanguage(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "README.md"),
		[]byte("# Project\n\n## Status\n\n**Active development** — Phases 1-3.5 complete. Working CLI. Phase 4 v2: event-driven sync engine rewrite in progress.\n"),
	))

	stdout := &bytes.Buffer{}
	err := runReadmeStatus(context.Background(), &fakeRunner{}, repoRoot, nil, stdout, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "README status check failed")
	assert.Contains(t, err.Error(), "Phases ")
	assert.Contains(t, err.Error(), "Phase ")
	assert.Contains(t, err.Error(), "complete. Working CLI")
	assert.Contains(t, err.Error(), "rewrite in progress")
	assert.Contains(t, stdout.String(), "==> README status")
}

// Validates: R-6.10.1
func TestRunReadmeStatusRejectsStaleRoadmapLanguageCaseInsensitively(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "README.md"),
		[]byte("# Project\n\n## Status\n\nActive development. phase 4 v2: event-driven sync engine rewrite In Progress.\n"),
	))

	err := runReadmeStatus(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "README status check failed")
	assert.Contains(t, err.Error(), "Phase ")
	assert.Contains(t, err.Error(), "rewrite in progress")
}

// Validates: R-6.10.1
func TestRunReadmeStatusRejectsMissingReadme(t *testing.T) {
	t.Parallel()

	err := runReadmeStatus(context.Background(), &fakeRunner{}, t.TempDir(), nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "README status check failed: README.md is missing")
}

// Validates: R-6.10.1
func TestRunReadmeStatusRejectsMissingStatusSection(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "README.md"),
		[]byte("# Project\n\nActive development. See spec/requirements/index.md for capability status.\n"),
	))

	err := runReadmeStatus(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "README status check failed: README.md is missing a ## Status section")
}

// Validates: R-6.10.1
func TestRunReadmeStatusIgnoresStaleRoadmapLanguageOutsideStatusSection(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "README.md"),
		[]byte("# Project\n\n## Status\n\nActive development. See spec/requirements/index.md for capability status.\n\n## Development Notes\n\nHistorical phase notes moved to requirements.\n"),
	))

	err := runReadmeStatus(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)
}

// Validates: R-6.10.1
func TestRunReadmeStatusAllowsRequirementsBackedStatus(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "README.md"),
		[]byte("# Project\n\n## Status\n\nActive development. See spec/requirements/index.md for capability status.\n"),
	))

	err := runReadmeStatus(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)
}
