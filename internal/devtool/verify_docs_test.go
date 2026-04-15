package devtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// Validates: R-6.10.7
func TestRunRepoConsistencyChecksFailsWithoutCrossCuttingEvidenceSection(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "error-model.md"),
		[]byte("# Error Model\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Verified By")
	assert.Contains(t, err.Error(), "error-model.md")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnMalformedValidatesReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "internal", "bad_trace_test.go"),
		[]byte(strings.Join([]string{
			"package internal",
			"",
			"import \"testing\"",
			"",
			"// Validates: D-6",
			"func TestBadTrace(t *testing.T) {}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed Validates reference")
	assert.Contains(t, err.Error(), "bad_trace_test.go")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnUnknownRequirementReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "internal", "unknown_trace_test.go"),
		[]byte(strings.Join([]string{
			"package internal",
			"",
			"import \"testing\"",
			"",
			"// Validates: R-9.9.9",
			"func TestUnknownTrace(t *testing.T) {}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown requirement ID R-9.9.9")
	assert.Contains(t, err.Error(), "unknown_trace_test.go")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnBrokenRecurringIncidentPromotedDocAnchor(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "reference", "live-incidents.md"),
		[]byte(strings.Join([]string{
			"# Live Incidents",
			"",
			"## LI-TEST-01: Sample recurring incident",
			"",
			"Recurring: yes",
			"Promoted docs: [graph-api-quirks.md#missing-anchor](graph-api-quirks.md#missing-anchor)",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing anchor #missing-anchor")
	assert.Contains(t, err.Error(), "LI-TEST-01")
}

// Validates: R-6.10.12
func TestRunRepoConsistencyChecksFailsOnMalformedImplementsReference(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "sync-engine.md"),
		[]byte(strings.Join([]string{
			"# Sync Engine",
			"",
			"Implements: R-6.10.13 [verified], D-6 [verified]",
			"",
			"## Verified By",
			"",
			"| Behavior | Evidence |",
			"| --- | --- |",
			"| sample | TestFixtureEvidence |",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed Implements reference")
	assert.Contains(t, err.Error(), "sync-engine.md")
}

// Validates: R-6.10.13
func TestRunRepoConsistencyChecksFailsWhenExpandedGovernedDocMissingEvidence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		title string
	}{
		{name: "sync-control-plane.md", title: "Sync Control Plane"},
		{name: "sync-store.md", title: "Sync Store"},
		{name: "sync-observation.md", title: "Sync Observation"},
		{name: "config.md", title: "Configuration"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repoRoot := t.TempDir()
			writeRepoConsistencyFixtures(t, repoRoot)

			require.NoError(t, os.WriteFile(
				filepath.Join(repoRoot, "spec", "design", tt.name),
				[]byte(strings.Join([]string{
					"# " + tt.title,
					"",
					"GOVERNS: internal/example/*.go",
					"",
					"## Ownership Contract",
					"- Owns: sample",
					"- Does Not Own: sample",
					"- Source of Truth: sample",
					"- Allowed Side Effects: sample",
					"- Mutable Runtime Owner: sample",
					"- Error Boundary: sample",
					"",
				}, "\n")),
				0o600,
			))

			err := runRepoConsistencyChecks(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.name)
			assert.Contains(t, err.Error(), "## Verified By")
		})
	}
}

// Validates: R-6.10.13
func TestRunRepoConsistencyChecksFailsWhenEvidenceDocReferencesUnknownTest(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsUnknownEvidenceTest(t, "cli.md", []string{
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
		"## Verified By",
		"",
		"| Behavior | Evidence |",
		"| --- | --- |",
		"| sample | TestMissingEvidence |",
		"",
	}, "TestMissingEvidence")
}

// Validates: R-6.10.13
func TestRunRepoConsistencyChecksFailsWhenExpandedGovernedDocReferencesUnknownTest(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsUnknownEvidenceTest(t, "sync-store.md", []string{
		"# Sync Store",
		"",
		"GOVERNS: internal/sync/*.go",
		"",
		"## Ownership Contract",
		"- Owns: sample",
		"- Does Not Own: sample",
		"- Source of Truth: sample",
		"- Allowed Side Effects: sample",
		"- Mutable Runtime Owner: sample",
		"- Error Boundary: sample",
		"",
		"## Verified By",
		"",
		"| Behavior | Evidence |",
		"| --- | --- |",
		"| sample | TestMissingStoreEvidence |",
		"",
	}, "TestMissingStoreEvidence")
}

// Validates: R-6.10.7
func TestRunRepoConsistencyChecksFailsWithoutDegradedModeIDColumn(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", "degraded-mode.md"),
		[]byte("# Degraded Mode\n\n| Failure | Evidence |\n| --- | --- |\n| sample | tests |\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "| ID |")
	assert.Contains(t, err.Error(), "degraded-mode.md")
}

func assertRepoConsistencyRejectsUnknownEvidenceTest(
	t *testing.T,
	docName string,
	docLines []string,
	missingTest string,
) {
	t.Helper()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "spec", "design", docName),
		[]byte(strings.Join(docLines, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown test function "+missingTest)
	assert.Contains(t, err.Error(), docName)
}
