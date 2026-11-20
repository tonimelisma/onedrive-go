package sync

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.1.2, R-2.10.4
func TestLocalObservationFindingsBatchFromSkippedItems_UnreadableDirectoryCreatesBoundaryIssue(t *testing.T) {
	t.Parallel()

	batch := localObservationFindingsBatchFromSkippedItems(driveid.New(syncfixture.TestDriveID), []syncdomain.SkippedItem{{
		Path:               "Private",
		Reason:             syncdomain.IssueLocalReadDenied,
		Detail:             "directory not accessible",
		BlocksReadBoundary: true,
	}})

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "Private", batch.Issues[0].Path)
	assert.Equal(t, syncdomain.IssueLocalReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, syncdomain.SKPermLocalRead("Private"), batch.Issues[0].ScopeKey)
	// Which types are managed is checked against the scanner's source by
	// TestLocalObservationManagedIssueTypes_ManageEveryTypeALocalScanReports.
	assert.Equal(t, syncdomain.LocalObservationManagedIssueTypes(), batch.ManagedIssueTypes)
}

// Validates: R-2.1.2, R-2.10.4
func TestRootReadFailureObservationFindingsBatch_CreatesBoundaryIssue(t *testing.T) {
	t.Parallel()

	for _, issueType := range []syncdomain.IssueType{syncdomain.IssueRemoteReadDenied, syncdomain.IssueRemoteRootNotFound} {
		batch := rootReadFailureObservationFindingsBatch(driveid.New(syncfixture.TestDriveID), issueType)

		require.Len(t, batch.Issues, 1)
		assert.Equal(t, "/", batch.Issues[0].Path)
		assert.Equal(t, issueType, batch.Issues[0].IssueType)
		assert.Equal(t, syncdomain.SKPermRemoteRead(""), batch.Issues[0].ScopeKey)
		assert.Equal(t, []syncdomain.IssueType{syncdomain.IssueRemoteReadDenied, syncdomain.IssueRemoteRootNotFound}, batch.ManagedIssueTypes,
			"a root observation replaces whichever root read issue an earlier answer left")
	}
}

// localScanIssueTypesInSource returns every IssueType constant named in a
// production file of the sync tree that builds a SkippedItem, with the files
// that name it: the issue types a local scan can report. It reads the source,
// so a type a new finding names is seen the moment a builder names it,
// without a second hand-kept list. A builder file naming a type for another
// reason is over-reported, which is the safe direction for the check that
// uses it.
func localScanIssueTypesInSource(t *testing.T) map[syncdomain.IssueType][]string {
	t.Helper()

	constants := syncfixture.IssueTypeConstantsByName(t)
	var files []string
	for _, dir := range syncTreePackageDirs() {
		found, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		files = append(files, found...)
	}

	reported := make(map[syncdomain.IssueType][]string)
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(file) //nolint:gosec // the test reads the sync tree's source
		require.NoError(t, readErr)
		if !bytes.Contains(src, []byte("SkippedItem")) {
			// Only a file that names the type can build one; skipping the
			// rest keeps the parse to a handful of files.
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
		require.NoError(t, parseErr)

		buildsSkippedItem := false
		named := make(map[syncdomain.IssueType]struct{})
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CompositeLit:
				buildsSkippedItem = buildsSkippedItem || namesSkippedItemType(n.Type)
			case *ast.Ident:
				if issueType, isConstant := constants[n.Name]; isConstant {
					named[issueType] = struct{}{}
				}
			}

			return true
		})
		if !buildsSkippedItem {
			continue
		}
		for issueType := range named {
			reported[issueType] = append(reported[issueType], file)
		}
	}

	return reported
}

// namesSkippedItemType reports whether a composite literal's type is
// SkippedItem or a slice or array of it.
func namesSkippedItemType(expr ast.Expr) bool {
	if array, ok := expr.(*ast.ArrayType); ok {
		expr = array.Elt
	}

	return nameOf(expr) == "SkippedItem"
}

// The local findings batch replaces the stored conditions of the types it
// manages, so a type a local scan reports must be managed: every full scan
// re-reports a finding whose cause is still there, and only a managed type is
// cleared by the scan that no longer finds it. An interrupted removal's
// leftover was reported but not managed, and its condition outlived the file
// for good. The reverse holds too: a managed type no local scan reports would
// let a full scan delete a condition another writer owns.
//
// Validates: R-6.4.9, R-6.2.14, R-6.2.15, R-2.10.10
func TestLocalObservationManagedIssueTypes_ManageEveryTypeALocalScanReports(t *testing.T) {
	t.Parallel()

	// keptUnmanaged names each type a local scan reports that the local
	// findings batch leaves unmanaged on purpose, with the reason it must
	// outlive the scan that stops reporting it. None does today.
	keptUnmanaged := map[syncdomain.IssueType]string{}
	// clearedOnly names each type the local findings batch manages though no
	// scan reports it, with the reason a scan must clear it.
	clearedOnly := map[syncdomain.IssueType]string{
		syncdomain.IssueRemovedFolderKept: "a removed shared folder's release records it after the run's last plan; " +
			"the next full scan clears it, so a release that runs again records what is still there " +
			"and a folder shared again keeps no stale report",
	}

	reported := localScanIssueTypesInSource(t)
	require.Contains(t, reported, syncdomain.IssueLocalInterruptedRemoval, "the source reading must see the scanner's findings")
	require.Contains(t, reported, syncdomain.IssueInvalidFilename, "the source reading must see the scanner's findings")

	managed := make(map[syncdomain.IssueType]struct{})
	for _, issueType := range syncdomain.LocalObservationManagedIssueTypes() {
		_, duplicate := managed[issueType]
		assert.False(t, duplicate, "%s is listed twice", issueType)
		managed[issueType] = struct{}{}
	}

	for issueType, files := range reported {
		if _, isManaged := managed[issueType]; isManaged {
			continue
		}
		assert.NotEmpty(t, keptUnmanaged[issueType],
			"%v build skipped items naming %s, which LocalObservationManagedIssueTypes leaves out: "+
				"add it, so the next full scan that no longer reports it clears it, "+
				"or record in keptUnmanaged why it must stay", files, issueType)
	}
	for issueType := range managed {
		assert.NotContains(t, keptUnmanaged, issueType, "%s is both managed and kept unmanaged", issueType)
		if _, scanReports := reported[issueType]; scanReports {
			assert.NotContains(t, clearedOnly, issueType, "%s is reported by a local scan, so it is not cleared only", issueType)
			continue
		}
		assert.NotEmpty(t, clearedOnly[issueType], "%s is managed, but no local scan reports it", issueType)
	}
}
