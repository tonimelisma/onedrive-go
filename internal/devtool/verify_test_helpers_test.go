package devtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	runCommands         []recordedCommand
	outputCommands      []recordedCommand
	combinedCommands    []recordedCommand
	outputs             map[string][]byte
	outputsByCWD        map[string]map[string][]byte
	runErr              error
	runErrByKey         map[string]error
	runErrSequenceByKey map[string][]error
	outputErr           error
	combinedOutputs     map[string][]byte
	combinedErr         error
	combinedErrByKey    map[string]error
}

const (
	dependencyGraphFixtureFallbackGoVersion = "1.24.0"
	testFixtureDirectoryPerm                = 0o750
	testFixtureFilePerm                     = 0o600
)

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

	if err, ok := f.runErrByKey[name+" "+strings.Join(args, " ")]; ok {
		return err
	}
	if seq := f.runErrSequenceByKey[name+" "+strings.Join(args, " ")]; len(seq) > 0 {
		next := seq[0]
		f.runErrSequenceByKey[name+" "+strings.Join(args, " ")] = seq[1:]
		return next
	}

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
	if byCWD, ok := f.outputsByCWD[cwd]; ok {
		if out, ok := byCWD[key]; ok {
			return out, nil
		}
	}

	return f.outputs[key], nil
}

func (f *fakeRunner) CombinedOutput(
	_ context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	f.combinedCommands = append(f.combinedCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})

	key := name + " " + strings.Join(args, " ")
	if err, ok := f.combinedErrByKey[key]; ok {
		return f.combinedOutputs[key], err
	}
	if f.combinedErr != nil {
		return f.combinedOutputs[key], f.combinedErr
	}

	return f.combinedOutputs[key], nil
}

func readVerifySummaryFile(t *testing.T, path string, summary *VerifySummary) {
	t.Helper()

	data, err := readFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, summary))
}

func assertVerifySummaryHasStep(t *testing.T, summary VerifySummary, name string, status string) {
	t.Helper()

	for _, step := range summary.Steps {
		if step.Name != name {
			continue
		}

		assert.Equal(t, status, step.Status)
		return
	}

	require.Failf(t, "missing summary step", "step %q not found", name)
}

func requireVerifySummaryStep(t *testing.T, summary VerifySummary, name string) VerifyStepSummary {
	t.Helper()

	for _, step := range summary.Steps {
		if step.Name == name {
			return step
		}
	}

	require.Failf(t, "missing summary step", "step %q not found", name)
	return VerifyStepSummary{}
}

func assertVerifyBucketSummary(t *testing.T, summary VerifySummary, name string, status string) {
	t.Helper()

	for _, bucket := range summary.E2EFullBuckets {
		if bucket.Name != name {
			continue
		}

		assert.Equal(t, status, bucket.Status)
		return
	}

	require.Failf(t, "missing bucket summary", "bucket %q not found", name)
}

func assertCommandHasEnvVar(t *testing.T, cmd recordedCommand, want string) {
	t.Helper()
	assert.Contains(t, cmd.env, want)
}

func assertCommandLacksEnvVar(t *testing.T, cmd recordedCommand, want string) {
	t.Helper()
	assert.NotContains(t, cmd.env, want)
}

func assertCommandLacksSkipSuiteScrubEnvVar(t *testing.T, cmd recordedCommand) {
	t.Helper()
	assert.NotContains(t, cmd.env, e2eSkipSuiteScrubEnvVar+"=1")
}

func writeRepoConsistencyFixtures(t *testing.T, repoRoot string) {
	t.Helper()

	writeRepoConsistencyDirectories(t, repoRoot)
	writeRepoConsistencyRequirements(t, repoRoot)
	writeRepoConsistencyDesignDocs(t, repoRoot)
	writeRepoConsistencyReferenceDocs(t, repoRoot)
	writeRepoConsistencyCodeFixtures(t, repoRoot)
}

func dependencyGraphFixtureGoVersion() string {
	version := strings.TrimPrefix(runtime.Version(), "go")
	if version == runtime.Version() || version == "" {
		return dependencyGraphFixtureFallbackGoVersion
	}

	for _, r := range version {
		if (r < '0' || r > '9') && r != '.' {
			return dependencyGraphFixtureFallbackGoVersion
		}
	}

	return version
}

func writeRepoConsistencyDirectories(t *testing.T, repoRoot string) {
	t.Helper()

	for _, dir := range []string{
		filepath.Join(repoRoot, "e2e"),
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "spec", "reference"),
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, ".github", "workflows"),
	} {
		ensureTestFixtureDir(t, dir)
	}

	writeTestFixtureFile(t,
		filepath.Join(repoRoot, "go.mod"),
		[]byte(fmt.Sprintf("module github.com/tonimelisma/onedrive-go\n\ngo %s\n", dependencyGraphFixtureGoVersion())),
	)
	writeTestFixtureFile(t, filepath.Join(repoRoot, "CLAUDE.md"), []byte("clean\n"))
}

func writeRepoConsistencyRequirements(t *testing.T, repoRoot string) {
	t.Helper()

	writeTestFixtureFile(t,
		filepath.Join(repoRoot, "spec", "requirements", "non-functional.md"),
		[]byte(strings.Join([]string{
			"# R-6 Non-Functional",
			"",
			"## R-6.2 Data Integrity [verified]",
			"- R-6.2.1: sample [verified]",
			"- R-6.2.2: sample [verified]",
			"",
			"## R-6.10 Verification [verified]",
			"- R-6.10.5: sample [verified]",
			"- R-6.10.6: sample [verified]",
			"- R-6.10.7: sample [verified]",
			"- R-6.10.11: sample [verified]",
			"- R-6.10.12: sample [verified]",
			"- R-6.10.13: sample [verified]",
			"",
		}, "\n")),
	)
	writeTestFixtureFile(t,
		filepath.Join(repoRoot, "spec", "requirements", "sync.md"),
		[]byte(strings.Join([]string{
			"# R-2 Sync",
			"",
			"## R-2.8 Watch Mode Behavior [verified]",
			"- R-2.8.3: sample [verified]",
			"",
			"## R-2.10 Failure Management [verified]",
			"- R-2.10.41: sample [verified]",
			"",
		}, "\n")),
	)
}

func writeRepoConsistencyDesignDocs(t *testing.T, repoRoot string) {
	t.Helper()

	writeTestFixtureFile(t,
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
	)
	for _, doc := range repoConsistencyDesignDocFixtures() {
		writeTestFixtureFile(t, filepath.Join(repoRoot, "spec", "design", doc.name), []byte(doc.content))
	}
}

func writeRepoConsistencyReferenceDocs(t *testing.T, repoRoot string) {
	t.Helper()

	writeTestFixtureFile(t,
		filepath.Join(repoRoot, "spec", "reference", "graph-api-quirks.md"),
		[]byte(strings.Join([]string{
			"# Graph API Quirks",
			"",
			`<a id="fixture-quirk"></a>`,
			"### Fixture Quirk",
			"",
			"Sample quirk.",
			"",
		}, "\n")),
	)
	writeTestFixtureFile(t,
		filepath.Join(repoRoot, "spec", "reference", "live-incidents.md"),
		[]byte(strings.Join([]string{
			"# Live Incidents",
			"",
			"## LI-TEST-01: Sample recurring incident",
			"",
			"Recurring: yes",
			"Promoted docs: [graph-api-quirks.md#fixture-quirk](graph-api-quirks.md#fixture-quirk)",
			"",
		}, "\n")),
	)
}

func repoConsistencyDesignDocFixtures() []struct {
	name    string
	content string
} {
	return []struct {
		name    string
		content string
	}{
		{
			name:    "error-model.md",
			content: "# Error Model\n\n## Verified By\n\n| Boundary | Evidence |\n| --- | --- |\n| sample | TestFixtureEvidence |\n",
		},
		{
			name:    "threat-model.md",
			content: "# Threat Model\n\n## Mitigation Evidence\n\n| Mitigation | Evidence |\n| --- | --- |\n| sample | TestFixtureEvidence |\n",
		},
		{
			name:    "degraded-mode.md",
			content: "# Degraded Mode\n\n| ID | Failure | Evidence |\n| --- | --- | --- |\n| DM-1 | sample | TestFixtureEvidence |\n",
		},
		repoConsistencyBehaviorDocFixture("cli.md", "CLI", "internal/cli/*.go", []string{
			"- Owns: CLI entrypoints",
			"- Does Not Own: sync runtime",
			"- Source of Truth: Cobra command definitions",
			"- Allowed Side Effects: config I/O and stdout",
			"- Mutable Runtime Owner: process-local command execution",
			"- Error Boundary: CLI error rendering",
		}, []string{
			"Run `onedrive-go status`.",
			"Run 'onedrive-go --drive <id> recover' when the state DB is damaged.",
		}),
		{
			name: "sync-engine.md",
			content: strings.Join([]string{
				"# Sync Engine",
				"",
				"## Verified By",
				"",
				"| Behavior | Evidence |",
				"| --- | --- |",
				"| sample | TestFixtureEvidence |",
				"",
			}, "\n"),
		},
		repoConsistencyBehaviorDocFixture("sync-execution.md", "Sync Execution", "internal/sync/*.go", []string{
			"- Owns: action execution",
			"- Does Not Own: planning",
			"- Source of Truth: executor config",
			"- Allowed Side Effects: transfer execution",
			"- Mutable Runtime Owner: worker pool",
			"- Error Boundary: worker results",
		}, nil),
		repoConsistencyBehaviorDocFixture("sync-control-plane.md", "Sync Control Plane", "internal/multisync/*.go", []string{
			"- Owns: multi-drive lifecycle",
			"- Does Not Own: single-drive execution",
			"- Source of Truth: config holder",
			"- Allowed Side Effects: orchestrator startup",
			"- Mutable Runtime Owner: watch orchestrator",
			"- Error Boundary: drive reports",
		}, nil),
		repoConsistencyBehaviorDocFixture("sync-store.md", "Sync Store", "internal/sync/*.go", []string{
			"- Owns: sqlite sync state",
			"- Does Not Own: graph calls",
			"- Source of Truth: sqlite rows",
			"- Allowed Side Effects: sqlite reads and writes",
			"- Mutable Runtime Owner: sync store handles",
			"- Error Boundary: persisted failure facts",
		}, nil),
		repoConsistencyBehaviorDocFixture("sync-observation.md", "Sync Observation", "internal/sync/*.go", []string{
			"- Owns: change observation",
			"- Does Not Own: planning",
			"- Source of Truth: local and remote observation inputs",
			"- Allowed Side Effects: filesystem and graph observation",
			"- Mutable Runtime Owner: observers and buffer",
			"- Error Boundary: change events and skipped items",
		}, nil),
		repoConsistencyBehaviorDocFixture("config.md", "Configuration", "internal/config/*.go", []string{
			"- Owns: config loading",
			"- Does Not Own: graph calls",
			"- Source of Truth: resolved config snapshot",
			"- Allowed Side Effects: config and metadata IO",
			"- Mutable Runtime Owner: config holder",
			"- Error Boundary: load and validation outcomes",
		}, nil),
	}
}

func repoConsistencyBehaviorDocFixture(
	name string,
	title string,
	governs string,
	ownership []string,
	examples []string,
) struct {
	name    string
	content string
} {
	lines := []string{
		"# " + title,
		"",
		"GOVERNS: " + governs,
		"",
		"## Ownership Contract",
	}
	lines = append(lines, ownership...)
	lines = append(lines,
		"",
		"## Verified By",
		"",
		"| Behavior | Evidence |",
		"| --- | --- |",
		"| sample | TestFixtureEvidence |",
		"",
	)
	lines = append(lines, examples...)
	if len(examples) > 0 {
		lines = append(lines, "")
	}

	return struct {
		name    string
		content string
	}{
		name:    name,
		content: strings.Join(lines, "\n"),
	}
}

func writeRepoConsistencyCodeFixtures(t *testing.T, repoRoot string) {
	t.Helper()

	writeTestFixtureFile(t, filepath.Join(repoRoot, "internal", "clean.go"), []byte("package internal\n"))
	writeTestFixtureFile(t,
		filepath.Join(repoRoot, "internal", "fixture_test.go"),
		[]byte(strings.Join([]string{
			"package internal",
			"",
			"import \"testing\"",
			"",
			"// Validates: R-6.2.1",
			"func TestFixtureEvidence(t *testing.T) {}",
			"",
		}, "\n")),
	)
}

func ensureTestFixtureDir(t *testing.T, dir string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, testFixtureDirectoryPerm))
}

func writeTestFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, data, testFixtureFilePerm))
}

// Ensure the fake runner still satisfies the commandRunner contract if the
// signatures evolve.
var _ commandRunner = (*fakeRunner)(nil)
