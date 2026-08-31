package devtool

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionRunner reports a fixed `golangci-lint --version` output.
type versionRunner struct {
	output string
	err    error
}

func (r versionRunner) Run(
	_ context.Context, _ string, _ []string, _, _ io.Writer, _ string, _ ...string,
) error {
	return r.err
}

func (r versionRunner) Output(
	_ context.Context, _ string, _ []string, _ string, _ ...string,
) ([]byte, error) {
	return []byte(r.output), r.err
}

func (r versionRunner) CombinedOutput(
	_ context.Context, _ string, _ []string, _ string, _ ...string,
) ([]byte, error) {
	return []byte(r.output), r.err
}

func writeWorkflowFixture(t *testing.T, workflow string) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ciWorkflowPath), []byte(workflow), 0o600))

	return root
}

const workflowFixture = `jobs:
  verify:
    steps:
      - name: Install golangci-lint
        uses: golangci/golangci-lint-action@v9
        with:
          version: v2.11.3
          install-only: true
`

// Validates: R-6.10.2
//
// A different golangci-lint version is a different lint policy. Local
// verification then passes on rules CI does not have, or misses rules it does,
// and the disagreement only surfaces after a push.
func TestAssertLintVersionMatchesCI_MismatchIsReported(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, workflowFixture)
	runner := versionRunner{output: "golangci-lint has version 2.10.1 built with go1.26.0\n"}

	err := assertLintVersionMatchesCI(t.Context(), runner, root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2.10.1")
	assert.Contains(t, err.Error(), "2.11.3")
	assert.Contains(t, err.Error(), "go install", "the message must say how to fix it")
}

// Validates: R-6.10.2
func TestAssertLintVersionMatchesCI_MatchPasses(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, workflowFixture)
	runner := versionRunner{output: "golangci-lint has version v2.11.3 built with go1.26.6\n"}

	require.NoError(t, assertLintVersionMatchesCI(t.Context(), runner, root))
}

// Validates: R-6.10.2
//
// The pin is read from the workflow so the two cannot drift apart through a
// second copy of the number; a workflow that stops pinning must fail loudly
// rather than silently accept any version.
func TestAssertLintVersionMatchesCI_UnpinnedWorkflowFails(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, "jobs:\n  verify:\n    steps:\n      - run: echo hi\n")
	runner := versionRunner{output: "golangci-lint has version v2.11.3\n"}

	err := assertLintVersionMatchesCI(t.Context(), runner, root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not pin")
}

// Validates: R-6.10.2
//
// A tree with no workflow has no pin to disagree with, so the check has
// nothing to enforce there. This is deliberately different from a workflow
// that exists and does not pin, which floats CI's lint policy and fails.
func TestAssertLintVersionMatchesCI_NoWorkflowIsNotAFailure(t *testing.T) {
	t.Parallel()

	runner := versionRunner{output: "golangci-lint has version v2.11.3\n"}

	require.NoError(t, assertLintVersionMatchesCI(t.Context(), runner, t.TempDir()))
}

// Validates: R-6.10.2
func TestPinnedLintVersion_ReadsTheRealWorkflow(t *testing.T) {
	t.Parallel()

	version, err := pinnedLintVersion(repoRootForTest(t))
	require.NoError(t, err)
	assert.Regexp(t, `^\d+\.\d+\.\d+$`, version)
}
