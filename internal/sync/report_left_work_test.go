package sync

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportLastPassStateFields are the Report fields only LeftWork and
// LocalTreeHolds add up. Their names are the report's alone; Failed is not
// listed because other types share the name.
func reportLastPassStateFields() map[string]bool {
	return map[string]bool{
		"LeftForNextSync":      true,
		"UnsyncedIssues":       true,
		"LastPassFailed":       true,
		"LastPassKeptForRetry": true,
		"InterruptedRemovals":  true,
		"KeptNotOnOneDrive":    true,
		"KeptNotUploaded":      true,
		"KeptNotWritable":      true,
		"KeptNotRemoved":       true,
	}
}

// Validates: R-6.6.28, R-2.4.8
//
// What a run left is added up in one place: product code outside the sync
// engine selects none of the report fields LeftWork and LocalTreeHolds read,
// so the summaries, the exit status and the final drain cannot each count it
// their own way.
func TestReport_ProductCodeOutsideTheEngineReadsWhatARunLeftOnlyThroughItsMethods(t *testing.T) {
	t.Parallel()

	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	engineDir := filepath.Join(moduleRoot, "internal", "sync")

	fields := reportLastPassStateFields()
	var selected []string
	engineSelects := 0
	files := walkModuleProductFiles(t, func(path, rel string) error {
		inEngine := filepath.Dir(path) == engineDir
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || !fields[selector.Sel.Name] {
				return true
			}
			if inEngine {
				engineSelects++
				return true
			}
			selected = append(selected, rel+": "+selector.Sel.Name)
			return true
		})
		return nil
	})

	require.Greater(t, files, 100, "the walk reads the module's product files")
	require.Positive(t, engineSelects, "the engine itself selects the fields, so the names are right")
	assert.Empty(t, selected, "read what a run left through Report.LeftWork or Report.LocalTreeHolds")
}

// Validates: R-2.4.8
//
// Every part of what holds a local tree has its sentence in one place, in a
// fixed order, so the final drain's detail and its notice say the same.
func TestLocalTreeHolds_ReasonsNameEveryPartInOrder(t *testing.T) {
	t.Parallel()

	holds := LocalTreeHolds{
		NoReport:          true,
		DryRun:            true,
		Left:              LeftWork{Failed: 1, WillRetry: 2, StillOpen: 3},
		HeldLocalDeletes:  1,
		SetAsideFiles:     2,
		KeptNotOnOneDrive: KeptEntries{Count: 1, Examples: []string{"Docs/link"}},
		KeptNotUploaded:   KeptEntries{Count: 4, Examples: []string{"a.txt", "b.txt", "c.txt"}},
		KeptNotWritable:   KeptEntries{Count: 2, Examples: []string{"", "Docs"}},
		KeptNotRemoved:    KeptEntries{Count: 1, Examples: []string{"Docs/locked.txt"}},
	}

	assert.Equal(t, []string{
		"the final sync produced no report",
		"the final sync was a dry run",
		"1 failed",
		"2 to be retried on the next sync",
		"3 still not synced",
		"1 local delete held for confirmation",
		"2 files set aside by an interrupted removal",
		"folder still contains files that are not on OneDrive (Docs/link); move or delete them to finish",
		"4 changes could not be uploaded because the shared folder can no longer be reached " +
			"(a.txt, b.txt, c.txt and 1 more); copy what you need elsewhere, then delete them to finish",
		"2 folders cannot be written to (the shared folder itself, Docs); make them writable to finish",
		"1 synced item could not be moved to the trash or deleted (Docs/locked.txt); " +
			"remove it by hand or fix what keeps it, then sync again",
	}, holds.Reasons())
	assert.Empty(t, (&LocalTreeHolds{}).Reasons(), "nothing holds the zero value")
}
