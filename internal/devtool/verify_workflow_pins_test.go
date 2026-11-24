package devtool

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.2
func TestWorkflowPinFindings_RefusesATagAndACheckoutThatKeepsCredentials(t *testing.T) {
	t.Parallel()

	const workflow = `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
      - uses: ./.github/actions/local
      - name: pinned
        uses: azure/login@a641126d1b8aa4d1fa005f4f92df94a3a4c4c906 # v3.1.0
`
	findings := workflowPinFindings(".github/workflows/ci.yml", workflow)

	assert.Equal(t, []string{
		".github/workflows/ci.yml:7: actions/checkout@v7.0.1 is not pinned to a commit SHA with a `# vX.Y.Z` comment",
		".github/workflows/ci.yml:7: actions/checkout does not set persist-credentials: false",
		".github/workflows/ci.yml:8: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e is not pinned to a commit SHA with a `# vX.Y.Z` comment",
		".github/workflows/ci.yml:9: actions/checkout does not set persist-credentials: false",
	}, findings)
}

// Validates: R-6.10.2
func TestWorkflowPinFindings_TheRepositoryWorkflowsArePinned(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootForTest(t)
	files, err := workflowFiles(repoRoot)
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, file := range files {
		content, readErr := readFile(filepath.Join(repoRoot, file))
		require.NoError(t, readErr)
		assert.Empty(t, workflowPinFindings(file, string(content)), file)
	}
}

// Validates: R-6.10.2
func TestRunWorkflowLint_RefusesAnUnpinnedAction(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeWorkflowFile(t, repoRoot, filepath.Base(ciWorkflowPath), `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7.0.1
`)

	err := runWorkflowLint(context.Background(), &fakeRunner{}, repoRoot, nil, io.Discard, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".github/workflows/ci.yml:7: actions/checkout@v7.0.1 is not pinned")
}
