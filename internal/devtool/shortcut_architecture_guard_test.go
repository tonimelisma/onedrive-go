package devtool

import (
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

func bad() string {
	return config.DefaultDataDir()
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync cleanup must receive an explicit data dir")
}

func TestRunShortcutArchitectureChecks_FailsOnMultisyncAmbientChildStatePath(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/leak.go", `package multisync

func bad(childMountID string) string {
	return config.MountStatePath(childMountID)
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync child state paths must use the explicit orchestrator data dir")
}

func TestRunShortcutArchitectureChecks_FailsOnMultisyncParentRootStateInTests(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/multisync/leak_test.go", `package multisync

func TestBad(t *testing.T) {
	_ = syncengine.ShortcutRootRecord{}
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync tests must build runner publications")
}

func TestRunShortcutArchitectureChecks_FailsOnCLIStatusRawShortcutRootFields(t *testing.T) {
	t.Parallel()

	repoRoot := writeShortcutGuardFixture(t, "internal/cli/status_bad.go", `package cli

func bad(root someRoot) string {
	return root.BlockedDetail
}
`)

	err := RunShortcutArchitectureChecks(repoRoot)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CLI status must not read raw blocked details")
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

func writeShortcutGuardFixture(t *testing.T, rel string, body string) string {
	t.Helper()

	repoRoot := t.TempDir()
	path := filepath.Join(repoRoot, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return repoRoot
}
