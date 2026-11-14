package sync

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
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
	files := 0
	walkErr := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != moduleRoot && (strings.HasPrefix(name, ".") || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		inEngine := filepath.Dir(path) == engineDir
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", path, parseErr)
		}
		files++
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || !fields[selector.Sel.Name] {
				return true
			}
			if inEngine {
				engineSelects++
				return true
			}
			rel, relErr := filepath.Rel(moduleRoot, path)
			if relErr != nil {
				rel = path
			}
			selected = append(selected, rel+": "+selector.Sel.Name)
			return true
		})
		return nil
	})
	require.NoError(t, walkErr)

	require.Greater(t, files, 100, "the walk reads the module's product files")
	require.Positive(t, engineSelects, "the engine itself selects the fields, so the names are right")
	assert.Empty(t, selected, "read what a run left through Report.LeftWork or Report.LocalTreeHolds")
}
