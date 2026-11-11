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
)

// Validates: R-2.1.2, R-2.10.4
func TestLocalObservationFindingsBatchFromSkippedItems_UnreadableDirectoryCreatesBoundaryIssue(t *testing.T) {
	t.Parallel()

	batch := localObservationFindingsBatchFromSkippedItems(driveid.New(testDriveID), []skippedItem{{
		Path:               "Private",
		Reason:             issueLocalReadDenied,
		Detail:             "directory not accessible",
		BlocksReadBoundary: true,
	}})

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "Private", batch.Issues[0].Path)
	assert.Equal(t, issueLocalReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, sKPermLocalRead("Private"), batch.Issues[0].ScopeKey)
	// Which types are managed is checked against the scanner's source by
	// TestLocalObservationManagedIssueTypes_ManageEveryTypeALocalScanReports.
	assert.Equal(t, localObservationManagedIssueTypes(), batch.ManagedIssueTypes)
}

// Validates: R-2.1.2, R-2.10.4
func TestRootRemoteReadDeniedObservationFindingsBatch_CreatesBoundaryIssue(t *testing.T) {
	t.Parallel()

	batch := rootRemoteReadDeniedObservationFindingsBatch(driveid.New(testDriveID))

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "/", batch.Issues[0].Path)
	assert.Equal(t, issueRemoteReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, sKPermRemoteRead(""), batch.Issues[0].ScopeKey)
	assert.Equal(t, []IssueType{issueRemoteReadDenied}, batch.ManagedIssueTypes)
}

// localScanIssueTypesInSource returns every IssueType constant named in a
// production file of this package that builds a skippedItem, with the files
// that name it: the issue types a local scan can report. It reads the source,
// so a type a new finding names is seen the moment a builder names it,
// without a second hand-kept list. A builder file naming a type for another
// reason is over-reported, which is the safe direction for the check that
// uses it.
func localScanIssueTypesInSource(t *testing.T) map[IssueType][]string {
	t.Helper()

	constants := issueTypeConstantsByName(t)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	reported := make(map[IssueType][]string)
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(file) //nolint:gosec // the test reads its own package's source
		require.NoError(t, readErr)
		if !bytes.Contains(src, []byte("skippedItem")) {
			// Only a file that names the type can build one; skipping the
			// rest keeps the parse to a handful of files.
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
		require.NoError(t, parseErr)

		buildsSkippedItem := false
		named := make(map[IssueType]struct{})
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
// skippedItem or a slice or array of it.
func namesSkippedItemType(expr ast.Expr) bool {
	if array, ok := expr.(*ast.ArrayType); ok {
		expr = array.Elt
	}
	ident, ok := expr.(*ast.Ident)

	return ok && ident.Name == "skippedItem"
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
	keptUnmanaged := map[IssueType]string{}

	reported := localScanIssueTypesInSource(t)
	require.Contains(t, reported, issueLocalInterruptedRemoval, "the source reading must see the scanner's findings")
	require.Contains(t, reported, IssueInvalidFilename, "the source reading must see the scanner's findings")

	managed := make(map[IssueType]struct{})
	for _, issueType := range localObservationManagedIssueTypes() {
		_, duplicate := managed[issueType]
		assert.False(t, duplicate, "%s is listed twice", issueType)
		managed[issueType] = struct{}{}
	}

	for issueType, files := range reported {
		if _, isManaged := managed[issueType]; isManaged {
			continue
		}
		assert.NotEmpty(t, keptUnmanaged[issueType],
			"%v build skipped items naming %s, which localObservationManagedIssueTypes leaves out: "+
				"add it, so the next full scan that no longer reports it clears it, "+
				"or record in keptUnmanaged why it must stay", files, issueType)
	}
	for issueType := range managed {
		assert.Contains(t, reported, issueType, "%s is managed, but no local scan reports it", issueType)
		assert.NotContains(t, keptUnmanaged, issueType, "%s is both managed and kept unmanaged", issueType)
	}
}
