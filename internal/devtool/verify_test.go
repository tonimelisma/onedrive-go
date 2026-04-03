package devtool

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedCommand struct {
	cwd  string
	env  []string
	name string
	args []string
}

type fakeRunner struct {
	runCommands    []recordedCommand
	outputCommands []recordedCommand
	outputs        map[string][]byte
	runErr         error
	outputErr      error
}

func (f *fakeRunner) Run(
	_ context.Context,
	cwd string,
	env []string,
	_, _ io.Writer,
	name string,
	args ...string,
) error {
	f.runCommands = append(f.runCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})

	return f.runErr
}

func (f *fakeRunner) Output(
	_ context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	f.outputCommands = append(f.outputCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})
	if f.outputErr != nil {
		return nil, f.outputErr
	}

	key := name + " " + strings.Join(args, " ")
	return f.outputs[key], nil
}

// Validates: R-6.2.1, R-6.2.2
func TestRunVerifyDefaultRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyDefault,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            stdout,
		Stderr:            stderr,
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, 6)
	assert.Equal(t, "gofumpt", runner.runCommands[0].name)
	assert.Equal(t, []string{"-w", "."}, runner.runCommands[0].args)
	assert.Equal(t, "goimports", runner.runCommands[1].name)
	assert.Equal(t, "golangci-lint", runner.runCommands[2].name)
	assert.Equal(t, "go", runner.runCommands[3].name)
	assert.Equal(t, []string{"build", "./..."}, runner.runCommands[3].args)
	assert.Equal(t, []string{"test", "-race", "-coverprofile=" + filepath.Join(repoRoot, "cover.out"), "./..."}, runner.runCommands[4].args)
	assert.Equal(t, []string{"test", "-tags=e2e", "-race", "-v", "-parallel", "5", "-timeout=10m", "./e2e/..."}, runner.runCommands[5].args)
	require.Len(t, runner.outputCommands, 1)
	assert.Contains(t, stdout.String(), "==> coverage")
}

// Validates: R-6.2.1
func TestRunVerifyFailsOnForbiddenRepoPattern(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "internal", "bad.go"), []byte("graph.MustNewClient(nil, nil)\n"), 0o600))

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            &bytes.Buffer{},
		Stderr:            &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MustNewClient")
}

// Validates: R-6.2.1
func TestParseCoverageTotal(t *testing.T) {
	t.Parallel()

	total, err := parseCoverageTotal("pkg/foo\t80.0%\ntotal:\t(statements)\t76.2%\n")
	require.NoError(t, err)
	assert.InDelta(t, 76.2, total, 0.001)
}

// Validates: R-6.2.1
func TestRunVerifyCoverageThresholdFailure(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t75.9%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: 76.0,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            &bytes.Buffer{},
		Stderr:            &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage gate failed")
}

// Validates: R-6.2.1
func TestResolveVerifyPlanRejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	_, err := resolveVerifyPlan("weird")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage")
}

// Validates: R-6.10.6
func TestRunRepoConsistencyChecksFailsWithoutOwnershipContract(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "cli.md"),
		[]byte("# CLI\n\nGOVERNS: internal/cli/*.go\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ownership Contract")
	assert.Contains(t, err.Error(), "cli.md")
}

// Validates: R-6.10.6
func TestRunRepoConsistencyChecksFailsWithoutOwnershipContractBullet(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "cli.md"),
		[]byte(strings.Join([]string{
			"# CLI",
			"",
			"GOVERNS: internal/cli/*.go",
			"",
			"## Ownership Contract",
			"- Owns: CLI entrypoints",
			"- Does Not Own: sync runtime",
			"- Source of Truth: Cobra command definitions",
			"- Allowed Side Effects: config I/O and stdout",
			"- Mutable Runtime Owner: process-local command execution",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Error Boundary")
	assert.Contains(t, err.Error(), "cli.md")
}

// Validates: R-6.10.7
func TestRunRepoConsistencyChecksFailsWithoutCrossCuttingDesignDocReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "system.md"),
		[]byte(strings.Join([]string{
			"# System",
			"",
			"## Design Docs",
			"- [error-model.md](error-model.md)",
			"- [degraded-mode.md](degraded-mode.md)",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threat-model.md")
	assert.Contains(t, err.Error(), "system.md")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnHTTPClientDoOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "internal", "bad_http.go"),
		[]byte(strings.Join([]string{
			"package bad",
			"",
			"import \"net/http\"",
			"",
			"type wrapper struct {",
			"\tclient *http.Client",
			"}",
			"",
			"func do(req *http.Request, w wrapper) (*http.Response, error) {",
			"\treturn w.client.Do(req)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http.Client.Do")
	assert.Contains(t, err.Error(), "bad_http.go")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksAllowsApprovedHTTPClientDoBoundary(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	graphDir := filepath.Join(repoRoot, "internal", "graph")
	require.NoError(t, os.MkdirAll(graphDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(graphDir, "client_preauth.go"),
		[]byte(strings.Join([]string{
			"package graph",
			"",
			"import \"net/http\"",
			"",
			"type client struct {",
			"\thttpClient *http.Client",
			"}",
			"",
			"func (c *client) do(req *http.Request) (*http.Response, error) {",
			"\treturn c.httpClient.Do(req)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	require.NoError(t, runRepoConsistencyChecks(repoRoot))
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnCLIProcessGlobalOutput(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	cliDir := filepath.Join(repoRoot, "internal", "cli")
	require.NoError(t, os.MkdirAll(cliDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(cliDir, "bad_output.go"),
		[]byte(strings.Join([]string{
			"package cli",
			"",
			"import (",
			"\t\"fmt\"",
			"\t\"os\"",
			")",
			"",
			"func badOutput() {",
			"\tfmt.Fprintln(os.Stderr, \"oops\")",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cli output boundary violation")
	assert.Contains(t, err.Error(), "bad_output.go")
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnRawOSFilesystemCallInGuardedPackage(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	syncDir := filepath.Join(repoRoot, "internal", "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(syncDir, "bad_fs.go"),
		[]byte(strings.Join([]string{
			"package sync",
			"",
			"import \"os\"",
			"",
			"func badFS(path string) error {",
			"\t_, err := os.Stat(path)",
			"\treturn err",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw os filesystem call detected")
	assert.Contains(t, err.Error(), "bad_fs.go")
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnStaleFilterSemanticsWording(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	requirementsDir := filepath.Join(repoRoot, "spec", "requirements")
	require.NoError(t, os.MkdirAll(requirementsDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(requirementsDir, "sync.md"),
		[]byte("Filter settings are per-drive only.\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale filter semantics wording")
	assert.Contains(t, err.Error(), "sync.md")
}

func writeRepoConsistencyFixtures(t *testing.T, repoRoot string) {
	t.Helper()

	for _, dir := range []string{
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, ".github", "workflows"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
	}

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "CLAUDE.md"), []byte("clean\n"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "system.md"),
		[]byte(strings.Join([]string{
			"# System",
			"",
			"## Design Docs",
			"- [error-model.md](error-model.md)",
			"- [threat-model.md](threat-model.md)",
			"- [degraded-mode.md](degraded-mode.md)",
			"",
		}, "\n")),
		0o600,
	))
	for _, name := range []string{"error-model.md", "threat-model.md", "degraded-mode.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "spec", "design", name), []byte("clean\n"), 0o600))
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "cli.md"),
		[]byte(strings.Join([]string{
			"# CLI",
			"",
			"GOVERNS: internal/cli/*.go",
			"",
			"## Ownership Contract",
			"- Owns: CLI entrypoints",
			"- Does Not Own: sync runtime",
			"- Source of Truth: Cobra command definitions",
			"- Allowed Side Effects: config I/O and stdout",
			"- Mutable Runtime Owner: process-local command execution",
			"- Error Boundary: CLI error rendering",
			"",
		}, "\n")),
		0o600,
	))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "internal", "clean.go"), []byte("package clean\n"), 0o600))
}

// Ensure the fake runner still satisfies the commandRunner contract if the
// signatures evolve.
var _ commandRunner = (*fakeRunner)(nil)
