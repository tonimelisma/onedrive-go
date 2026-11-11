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

	err := assertLintVersionMatchesCI(t.Context(), runner, root, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2.10.1")
	assert.Contains(t, err.Error(), "2.11.3")
	assert.Contains(t, err.Error(), "go install", "the message must say how to fix it")
}

// Validates: R-6.10.2
//
// The fix for a wrong version must itself build with the pinned toolchain, or
// it trades one failure for the next: a linter of the right version built with
// a newer Go cannot typecheck the repository.
func TestAssertLintVersionMatchesCI_VersionMismatchNamesThePinnedToolchain(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, workflowFixture)
	writeGoModFixture(t, root, goModFixture)
	runner := versionRunner{output: "golangci-lint has version 2.10.1 built with go1.26.0\n"}

	err := assertLintVersionMatchesCI(t.Context(), runner, root, []string{"GOBIN=/opt/tools/bin"})
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"GOTOOLCHAIN=go1.26.10 GOBIN=/opt/tools/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3")
}

// Validates: R-6.10.2
//
// A linter of the pinned version that was built with a newer Go than go.mod
// pins loads export data it cannot read and fails every typecheck. The check
// names the cause and prints the command that rebuilds the binary in place.
func TestAssertLintVersionMatchesCI_BuiltWithAnotherGoPrintsTheRebuildCommand(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, workflowFixture)
	writeGoModFixture(t, root, goModFixture)
	binDir := t.TempDir()
	//nolint:gosec // the lookup only accepts an executable file, as a shell would.
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "golangci-lint"), []byte("#!/bin/sh\n"), 0o700))
	runner := versionRunner{output: "golangci-lint has version v2.11.3 built with go1.27.1 from 5d1e709b on 2026-03-01T00:00:00Z\n"}

	err := assertLintVersionMatchesCI(t.Context(), runner, root, []string{"PATH=/nonexistent:" + binDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "built with go1.27")
	assert.Contains(t, err.Error(), "go1.26.10")
	assert.Contains(t, err.Error(),
		"GOTOOLCHAIN=go1.26.10 GOBIN="+binDir+" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3")
}

// Validates: R-6.10.2
//
// Only the minor version decides whether the linter can read the toolchain's
// export data, so a different patch release of the same minor is accepted.
func TestAssertLintVersionMatchesCI_BuiltWithThePinnedMinorPasses(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, workflowFixture)
	writeGoModFixture(t, root, goModFixture)
	runner := versionRunner{output: "golangci-lint has version v2.11.3 built with go1.26.6\n"}

	require.NoError(t, assertLintVersionMatchesCI(t.Context(), runner, root, nil))
}

// Validates: R-6.10.2
func TestAssertLintVersionMatchesCI_OutputWithoutABuildToolchainIsNotChecked(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, workflowFixture)
	writeGoModFixture(t, root, goModFixture)
	runner := versionRunner{output: "golangci-lint has version v2.11.3\n"}

	require.NoError(t, assertLintVersionMatchesCI(t.Context(), runner, root, nil))
}

// Validates: R-6.10.2
//
// The rebuild command installs over the binary the check just ran: the PATH
// directory that holds the linter, else GOBIN when the environment sets it,
// else the go command's own default. Installing into a GOBIN that is not where
// the running linter lives would leave the stale binary first on PATH, and the
// check would fail again.
func TestLintInstallDir(t *testing.T) {
	t.Parallel()

	binDir := t.TempDir()
	//nolint:gosec // the lookup only accepts an executable file, as a shell would.
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "golangci-lint"), []byte("#!/bin/sh\n"), 0o700))
	notExecutable := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(notExecutable, "golangci-lint"), []byte("text\n"), 0o600))

	assert.Equal(t, binDir, lintInstallDir([]string{"GOBIN=/opt/tools/bin", "PATH=" + binDir}))
	assert.Equal(t, binDir, lintInstallDir([]string{"GOBIN=", "PATH=" + notExecutable + ":" + binDir}))
	assert.Equal(t, "/opt/tools/bin", lintInstallDir([]string{"GOBIN=/opt/tools/bin", "PATH=" + notExecutable}))
	assert.Equal(t, lintInstallDirFallback, lintInstallDir([]string{"PATH=" + notExecutable}))
	assert.Equal(t, lintInstallDirFallback, lintInstallDir(nil))
}

// Validates: R-6.10.2
func TestAssertLintVersionMatchesCI_MatchPasses(t *testing.T) {
	t.Parallel()

	root := writeWorkflowFixture(t, workflowFixture)
	runner := versionRunner{output: "golangci-lint has version v2.11.3 built with go1.26.6\n"}

	require.NoError(t, assertLintVersionMatchesCI(t.Context(), runner, root, nil))
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

	err := assertLintVersionMatchesCI(t.Context(), runner, root, nil)
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

	require.NoError(t, assertLintVersionMatchesCI(t.Context(), runner, t.TempDir(), nil))
}

// Validates: R-6.10.2
func TestPinnedLintVersion_ReadsTheRealWorkflow(t *testing.T) {
	t.Parallel()

	version, err := pinnedLintVersion(repoRootForTest(t))
	require.NoError(t, err)
	assert.Regexp(t, `^\d+\.\d+\.\d+$`, version)
}
