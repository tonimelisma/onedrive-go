package devtool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	goModFixture          = "module example.com/fixture\n\ngo 1.26.0\n\ntoolchain go1.26.10\n"
	goModFixtureToolchain = "go1.26.10"
	lintVersionCommand    = "golangci-lint --version"
)

func writeGoModFixture(t *testing.T, root, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte(content), 0o600))
}

// Validates: R-6.10.2
func TestPinnedGoToolchain_ReadsTheToolchainDirective(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeGoModFixture(t, root, goModFixture)

	toolchain, err := pinnedGoToolchain(root)
	require.NoError(t, err)
	assert.Equal(t, goModFixtureToolchain, toolchain)
}

// Validates: R-6.10.2
//
// A go.mod without a toolchain line still names a language version, and the
// go command treats that version as the toolchain to use. A two-part language
// version names its first release, the only form GOTOOLCHAIN accepts.
func TestPinnedGoToolchain_FallsBackToTheGoDirective(t *testing.T) {
	t.Parallel()

	for directive, want := range map[string]string{
		"go 1.26.0": "go1.26.0",
		"go 1.26.4": "go1.26.4",
		"go 1.26":   "go1.26.0",
	} {
		root := t.TempDir()
		writeGoModFixture(t, root, "module example.com/fixture\n\n"+directive+"\n")

		toolchain, err := pinnedGoToolchain(root)
		require.NoError(t, err, directive)
		assert.Equal(t, want, toolchain, directive)
	}
}

// Validates: R-6.10.2
func TestPinnedGoToolchain_NoGoModIsNotAPin(t *testing.T) {
	t.Parallel()

	toolchain, err := pinnedGoToolchain(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, toolchain)
}

// Validates: R-6.10.2
//
// A go.mod the parser cannot read a version from must fail loudly: carrying
// on unpinned is the drift the pin exists to stop.
func TestPinnedGoToolchain_GoModWithoutAVersionFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeGoModFixture(t, root, "module example.com/fixture\n")

	_, err := pinnedGoToolchain(root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "go.mod")
}

// Validates: R-6.10.2
func TestPinnedGoToolchain_ReadsTheRealGoMod(t *testing.T) {
	t.Parallel()

	toolchain, err := pinnedGoToolchain(repoRootForTest(t))
	require.NoError(t, err)
	assert.Regexp(t, `^go\d+\.\d+(\.\d+)?$`, toolchain)
}

// Validates: R-6.10.2
func TestWithPinnedToolchain_SetsGOTOOLCHAINWhenUnset(t *testing.T) {
	t.Parallel()

	base := []string{"PATH=/usr/bin", "GOTOOLCHAIN="}

	env := withPinnedToolchain(base, goModFixtureToolchain)

	assert.Contains(t, env, "GOTOOLCHAIN="+goModFixtureToolchain)
	assert.Contains(t, env, "PATH=/usr/bin")
	assert.Equal(t, []string{"PATH=/usr/bin", "GOTOOLCHAIN="}, base, "the caller's environment is not modified")
}

// Validates: R-6.10.2
func TestWithPinnedToolchain_ExplicitGOTOOLCHAINWins(t *testing.T) {
	t.Parallel()

	base := []string{"PATH=/usr/bin", "GOTOOLCHAIN=go1.27.1"}

	env := withPinnedToolchain(base, goModFixtureToolchain)

	assert.Equal(t, base, env)
	assert.Equal(t, base, withPinnedToolchain(base, ""), "no pin leaves the environment alone")
}

// Validates: R-6.10.2
//
// A host whose system Go is newer than the pin must still build, vet, test and
// lint with the pinned toolchain, without the developer exporting anything.
func TestRunVerifyPassesThePinnedToolchainToCommands(t *testing.T) {
	t.Parallel()

	repoRoot := writeWorkflowFixture(t, workflowFixture)
	writeGoModFixture(t, repoRoot, goModFixture)
	writeRequirementsBackedReadme(t, repoRoot)
	runner := &fakeRunner{combinedOutputs: map[string][]byte{
		lintVersionCommand: []byte("golangci-lint has version v2.11.3 built with go1.26.6\n"),
	}}

	err := runVerifyProfileWithEnv(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyPR,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	}, []string{"PATH=/usr/bin"})
	require.NoError(t, err)

	require.NotEmpty(t, runner.runCommands)
	for _, command := range runner.runCommands {
		assertCommandHasEnvVar(t, command, "GOTOOLCHAIN="+goModFixtureToolchain)
	}
	require.Len(t, runner.combinedCommands, 1)
	assertCommandHasEnvVar(t, runner.combinedCommands[0], "GOTOOLCHAIN="+goModFixtureToolchain)
}

// Validates: R-6.10.2
func TestRunVerifyKeepsAnExplicitGOTOOLCHAIN(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeGoModFixture(t, repoRoot, goModFixture)
	writeRequirementsBackedReadme(t, repoRoot)
	runner := &fakeRunner{}

	err := runVerifyProfileWithEnv(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyQuick,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	}, []string{"PATH=/usr/bin", "GOTOOLCHAIN=local"})
	require.NoError(t, err)

	require.NotEmpty(t, runner.runCommands)
	for _, command := range runner.runCommands {
		assertCommandHasEnvVar(t, command, "GOTOOLCHAIN=local")
		assertCommandLacksEnvVar(t, command, "GOTOOLCHAIN="+goModFixtureToolchain)
	}
}
