package devtool

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoWalkReadDir is the fixture's one folder a repository-rule walk reads.
const repoWalkReadDir = "internal/read"

// repoWalkSkippedDirs are the folders the fixture plants beside it, one per
// name the repository rules skip, each nested under internal/ so the walks
// that start there reach it too.
func repoWalkSkippedDirs() []string {
	return []string{
		"internal/" + verifyGitDir,
		"internal/" + verifyTestdataDir,
		"internal/" + verifyClaudeDir,
		"internal/" + verifyNodeModulesDir,
		"internal/" + verifyVendorDir,
		"internal/" + defaultVerifyLogDirName,
	}
}

// plantRepoWalkSources writes into dir a production file and an
// integration-tagged test file that break every repository rule: a
// flattened error attribute, an undocumented mutex, a backlog ID, a
// principle label and an ungoverned file, and a test without the integration
// prefix whose name and Validates line carry the folder's index.
func plantRepoWalkSources(t *testing.T, root string, dir string, index int) {
	t.Helper()

	production := fmt.Sprintf(`package planted

import (
	"log/slog"
	"sync"
)

type guarded struct {
	mu sync.Mutex
}

// Cites B-%03d and %s%d.
func logIt(err error) slog.Attr { return slog.String("error", err.Error()) }
`, repoWalkBacklogBase+index, principleLabelMarker, index)
	test := fmt.Sprintf(`//go:build integration

package planted

import "testing"

// `+validatesMarker+` R-9.9.%d
func TestPlanted%d(t *testing.T) {}
`, index, index)
	require.NoError(t, writeFile(filepath.Join(root, filepath.FromSlash(dir), "planted.go"), []byte(production)))
	require.NoError(t, writeFile(filepath.Join(root, filepath.FromSlash(dir), "planted_test.go"), []byte(test)))
}

// repoWalkBacklogBase numbers the fixture's backlog IDs, one per folder.
const repoWalkBacklogBase = 100

// principleLabelMarker is a principle label's prefix, spelled in two pieces
// so the principle-label check of the real tree does not read the fixture's
// label as one in this file.
const principleLabelMarker = "P" + "-"

// validatesMarker is the traceability marker, spelled in two pieces so the
// requirement-traceability check of the real tree does not read the
// fixture's deliberately unknown requirement as a citation of this file.
const validatesMarker = "Valid" + "ates:"

// Validates: R-6.10.5, R-6.10.17
//
// Every repository-rule walk skips the same folders, so a tree nested in one
// of them is read by no rule: the output-boundary walk once read
// `.worktrees` while the test-convention walk skipped it. Each walk reads the
// fixture's read folder and none of the skipped ones.
func TestRepositoryRuleWalksSkipTheSameFolders(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	plantRepoWalkSources(t, root, repoWalkReadDir, 0)
	for i, dir := range repoWalkSkippedDirs() {
		plantRepoWalkSources(t, root, dir, i+1)
	}
	inRead := func(location string) bool {
		return strings.HasPrefix(filepath.ToSlash(location), repoWalkReadDir+"/")
	}

	boundary, err := findOutputBoundaryViolations(root)
	require.NoError(t, err)
	require.NotEmpty(t, boundary)
	for _, violation := range boundary {
		assert.True(t, inRead(violation.Path), "output boundaries read %s", violation.Path)
	}

	conventions, err := findTestConventionViolations(root)
	require.NoError(t, err)
	require.NotEmpty(t, conventions)
	for _, violation := range conventions {
		assert.True(t, inRead(violation.Path), "test conventions read %s", violation.Path)
	}

	labels, err := findPrincipleLabelViolations(root)
	require.NoError(t, err)
	require.NotEmpty(t, labels)
	for _, violation := range labels {
		assert.True(t, inRead(violation.Location), "principle labels read %s", violation.Location)
	}

	mutexes, err := findMutexDocViolations(root)
	require.NoError(t, err)
	require.NotEmpty(t, mutexes)
	for _, violation := range mutexes {
		assert.True(t, inRead(violation.Location), "mutex docs read %s", violation.Location)
	}

	refs, err := collectValidatesRefs(root)
	require.NoError(t, err)
	require.NotEmpty(t, refs)
	for _, ref := range refs {
		assert.True(t, inRead(ref.Location), "requirement traceability reads %s", ref.Location)
	}

	governed, err := collectProductionGoFiles(root)
	require.NoError(t, err)
	assert.Equal(t, []string{repoWalkReadDir + "/planted.go"}, governed, "spec ownership")

	tests, err := collectTestFunctionNames(root)
	require.NoError(t, err)
	assert.Contains(t, tests, "TestPlanted0")
	for i := range repoWalkSkippedDirs() {
		assert.NotContains(t, tests, fmt.Sprintf("TestPlanted%d", i+1), "spec test citations")
	}

	cited, _, err := collectBacklogIDCitations(root, map[string]bool{})
	require.NoError(t, err)
	citedIDs := []string{}
	for id := range cited {
		citedIDs = append(citedIDs, id)
	}
	slices.Sort(citedIDs)
	assert.Equal(t, []string{fmt.Sprintf("B-%03d", repoWalkBacklogBase)}, citedIDs, "backlog ids")
}

// Validates: R-6.10.5
//
// The skip set names exactly the folders that hold another checkout, local
// state or third-party code; a folder of the repository's own sources is
// read.
func TestSkipRepositoryRuleDirNamesTheSkippedFolders(t *testing.T) {
	t.Parallel()

	for _, dir := range repoWalkSkippedDirs() {
		assert.True(t, skipRepositoryRuleDir(filepath.Base(dir)), dir)
	}
	for _, name := range []string{"internal", "spec", "e2e", "testdata", ".github", "cmd"} {
		assert.False(t, skipRepositoryRuleDir(name), name)
	}
}
