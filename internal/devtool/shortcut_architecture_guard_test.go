package devtool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunShortcutArchitectureChecks_CurrentRepoPasses(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	require.NoError(t, RunShortcutArchitectureChecks(repoRoot))
}

func TestRunShortcutArchitectureChecks_FailsOnMultisyncGraphImport(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/leak.go", `package multisync

import "github.com/tonimelisma/onedrive-go/internal/graph"

var _ = graph.Item{}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync must not import Graph")
}

func TestRunShortcutArchitectureChecks_FailsOnMultisyncAmbientDataDir(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/leak.go", `package multisync

import cfg "github.com/tonimelisma/onedrive-go/internal/config"

func bad() string {
	return cfg.DefaultDataDir()
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync cleanup must receive an explicit data dir")
}

func TestRunShortcutArchitectureChecks_FailsOnMultisyncAmbientChildStatePath(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/leak.go", `package multisync

import cfg "github.com/tonimelisma/onedrive-go/internal/config"

func bad(childMountID string) string {
	return cfg.MountStatePath(childMountID)
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync child state paths must use the explicit orchestrator data dir")
}

func TestRunShortcutArchitectureChecks_FailsOnMultisyncParentRootStateInTests(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/leak_test.go", `package multisync

import se "github.com/tonimelisma/onedrive-go/internal/sync"

func TestBad(t *testing.T) {
	_ = se.ShortcutRootRecord{}
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync tests must build child process snapshots")
}

func TestRunShortcutArchitectureChecks_FailsOnMultisyncShortcutTopologyFileName(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/shortcut_topology.go", `package multisync

func ok() {}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "use child process snapshot file names")
}

func TestRunShortcutArchitectureChecks_FailsOnProductionMultisyncAliasedParentRootState(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/leak.go", `package multisync

import se "github.com/tonimelisma/onedrive-go/internal/sync"

func bad() any {
	return se.ShortcutRootState("active")
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync must not branch on parent shortcut-root states")
}

func TestRunShortcutArchitectureChecks_FailsOnCLIStatusRawShortcutRootFields(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/cli/status_bad.go", `package cli

import se "github.com/tonimelisma/onedrive-go/internal/sync"

func bad() any {
	return se.ShortcutRootRecord{}
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CLI status must consume ShortcutRootStatusView")
}

func TestRunShortcutArchitectureChecks_FailsOnLiveShortcutDeleteE2EPlan(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "spec/design/sync-control-plane.md", `# bad

TODO: add live shortcut delete E2E once tests are stable.
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "live shortcut delete E2E is out of scope")
}

func TestRunShortcutArchitectureChecks_SkipsVCSDirectoriesBeforeTraversal(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "spec/design/sync-control-plane.md", `# good
`)
	gitDir := filepath.Join(repoRoot, ".git")
	require.NoError(t, os.MkdirAll(gitDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "packed-refs"), []byte("shortcut bootstrap"), 0o600))
	require.NoError(t, os.Chmod(gitDir, 0))
	t.Cleanup(func() {
		//nolint:gosec // Test cleanup must restore search permission so TempDir removal can delete the fixture.
		assert.NoError(t, os.Chmod(gitDir, 0o700))
	})

	require.NoError(t, RunShortcutArchitectureChecks(repoRoot))
}

func TestRunShortcutComplianceMarkdownAndJSONReportShortcutInvariants(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/process.go", `package multisync

func ok() {}
`)

	var markdown bytes.Buffer
	require.NoError(t, RunShortcutCompliance(t.Context(), ShortcutComplianceOptions{
		RepoRoot: repoRoot,
		Format:   ShortcutComplianceFormatMarkdown,
		Stdout:   &markdown,
	}))
	assert.Contains(t, markdown.String(), "# Shortcut Compliance")
	assert.Contains(t, markdown.String(), "shortcut-parent-owns-truth")

	var jsonOut bytes.Buffer
	require.NoError(t, RunShortcutCompliance(t.Context(), ShortcutComplianceOptions{
		RepoRoot: repoRoot,
		Format:   ShortcutComplianceFormatJSON,
		Stdout:   &jsonOut,
	}))
	var decoded ShortcutComplianceReport
	require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &decoded))
	assert.Equal(t, verifySummaryStatusPass, decoded.Status)
	assert.NotEmpty(t, decoded.Invariants)
}

func TestRunShortcutComplianceReturnsFailureForDrift(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/shortcut_topology.go", `package multisync

func bad() {}
`)

	var output bytes.Buffer
	err := RunShortcutCompliance(t.Context(), ShortcutComplianceOptions{
		RepoRoot: repoRoot,
		Format:   ShortcutComplianceFormatMarkdown,
		Stdout:   &output,
	})

	require.Error(t, err)
	assert.Contains(t, output.String(), "Status: fail")
	assert.Contains(t, output.String(), "use child process snapshot file names")
}

func TestDoDReviewPolicyDoesNotWaitForAbsentAutomaticComments(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		// #nosec G304 -- the test reads fixed repository guidance files from a resolved repo root.
		data, readErr := os.ReadFile(filepath.Join(repoRoot, name))
		require.NoError(t, readErr)
		text := string(data)
		assert.Contains(t, text, "Do not wait indefinitely for automatic Codex comments")
		assert.Contains(t, text, "one final thread sweep immediately before merge")
		assert.Contains(t, text, "Review-thread carryover")
	}
}

func writeShortcutGuardFixture(t *testing.T, rel string, body string) string {
	t.Helper()

	repoRoot := t.TempDir()
	path := filepath.Join(repoRoot, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return repoRoot
}
