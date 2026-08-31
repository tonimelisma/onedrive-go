package devtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRequirementFixture(t *testing.T, requirements, design, goSource string) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, specRequirementsDir), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, specDesignDir), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "demo"), 0o750))

	require.NoError(t, os.WriteFile(
		filepath.Join(root, specRequirementsDir, "reqs.md"), []byte(requirements), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, specDesignDir, "design.md"), []byte(design), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "internal", "demo", "demo_test.go"), []byte(goSource), 0o600))

	return root
}

// Validates: R-6.10.12
func TestFindRequirementViolations_ResolvedReferencesPass(t *testing.T) {
	root := writeRequirementFixture(t,
		"## R-1 Group\n\n- R-1.1: does a thing. [verified]\n",
		"Implements: R-1 [verified]\n",
		"package demo\n\n// Validates: R-1.1\nfunc TestThing() {}\n")

	violations, err := findRequirementViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations)
}

// Validates: R-6.10.12
//
// A renamed or deleted requirement must not leave citations pointing at
// nothing, which is how a test keeps looking like evidence for a rule that no
// longer exists.
func TestFindRequirementViolations_DanglingValidatesReferenceFails(t *testing.T) {
	root := writeRequirementFixture(t,
		"- R-1.1: does a thing. [verified]\n",
		"Implements: R-1.1 [verified]\n",
		"package demo\n\n// Validates: R-9.9.9\nfunc TestThing() {}\n")

	violations, err := findRequirementViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Detail, "R-9.9.9")
	assert.Contains(t, violations[0].Location, "demo_test.go")
}

// Validates: R-6.10.12
func TestFindRequirementViolations_DanglingImplementsReferenceFails(t *testing.T) {
	root := writeRequirementFixture(t,
		"- R-1.1: does a thing. [verified]\n",
		"Implements: R-1.1 [verified], R-8.8 [verified]\n",
		"package demo\n\n// Validates: R-1.1\nfunc TestThing() {}\n")

	violations, err := findRequirementViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Detail, "R-8.8")
}

// Validates: R-6.10.12
//
// The reverse direction is the dangerous one: a verified status with no
// evidence reads as proof of behavior nobody demonstrated.
func TestFindRequirementViolations_VerifiedWithoutEvidenceFails(t *testing.T) {
	root := writeRequirementFixture(t,
		"- R-1.1: does a thing. [verified]\n- R-2.1: does another. [verified]\n",
		"Implements: R-1.1 [verified]\n",
		"package demo\n\n// Validates: R-1.1\nfunc TestThing() {}\n")

	violations, err := findRequirementViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Detail, "R-2.1")
	assert.Contains(t, violations[0].Detail, "no test cites it")
}

// Validates: R-6.10.12
//
// Docs cite groups and tests cite criteria, so group-level evidence has to
// count for the criteria underneath it.
func TestFindRequirementViolations_GroupLevelEvidenceCoversItsCriteria(t *testing.T) {
	root := writeRequirementFixture(t,
		"## R-1 Group\n\n- R-1.1: a. [verified]\n- R-1.2: b. [verified]\n",
		"Implements: R-1 [verified]\n",
		"package demo\n")

	violations, err := findRequirementViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations)
}

// Validates: R-6.10.12
//
// Only verified claims need evidence; planned and designed work does not.
func TestFindRequirementViolations_UnverifiedStatusesNeedNoEvidence(t *testing.T) {
	root := writeRequirementFixture(t,
		"- R-1.1: planned work. [planned]\n- R-1.2: designed work. [designed]\n",
		"",
		"package demo\n")

	violations, err := findRequirementViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations)
}

// Validates: R-6.10.12
func TestFindRequirementViolations_RealRepoIsClean(t *testing.T) {
	violations, err := findRequirementViolations(repoRootForTest(t))
	require.NoError(t, err)
	assert.Empty(t, violations)
}
