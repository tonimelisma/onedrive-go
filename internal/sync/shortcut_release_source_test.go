package sync

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// treeRemovalCall is one call of a tree-removing sync-tree method in product
// code: where it is and in which function.
type treeRemovalCall struct {
	file     string
	function string
	call     string
}

// Validates: R-2.4.8, R-6.4.9
//
// No product code removes a user's tree but by the verified local delete, one
// entry at a time. The sync tree's unchecked tree walks are gone
// (RemoveTreeNoFollow) or called only where everything removed was found
// disposable all the way down first: RemoveAll, from the junk removal of the
// ordinary folder delete. A removed shared folder's release goes through the
// executor's local delete, and the parent removes its alias root only as an
// empty directory.
func TestProductCode_RemovesATreeOnlyByTheVerifiedLocalDelete(t *testing.T) {
	t.Parallel()

	var removals []treeRemovalCall
	files := walkModuleProductFiles(t, func(path, rel string) error {
		found, err := treeRemovalCallsIn(path, rel)
		removals = append(removals, found...)
		return err
	})
	require.Greater(t, files, 100, "the walk reads the module's product files")

	allowed := treeRemovalCall{
		file:     "internal/sync/executor_delete.go",
		function: "removeDisposableChildren",
		call:     "e.syncTree.RemoveAll",
	}
	assert.Equal(t, []treeRemovalCall{allowed}, removals,
		"a tree leaves the sync folder by the verified local delete; only disposable junk is removed whole")
}

// treeRemovalCallsIn lists the calls in one product file of a tree-removing
// method of the sync tree: RemoveTreeNoFollow anywhere, and RemoveAll on a
// sync tree (a receiver named syncTree). The synctree package declares them
// and the synctest package shims them; neither calls them on a sync folder.
func treeRemovalCallsIn(path, rel string) ([]treeRemovalCall, error) {
	if strings.HasPrefix(rel, "internal/synctree/") || strings.HasPrefix(rel, "internal/synctest/") {
		return nil, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var found []treeRemovalCall
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			receiver := renderGoExpr(fset, selector.X)
			removesTree := selector.Sel.Name == "RemoveTreeNoFollow" ||
				(selector.Sel.Name == "RemoveAll" && strings.HasSuffix(receiver, "syncTree"))
			if removesTree {
				found = append(found, treeRemovalCall{
					file:     rel,
					function: function.Name.Name,
					call:     receiver + "." + selector.Sel.Name,
				})
			}
			return true
		})
	}

	return found, nil
}

func renderGoExpr(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		return ""
	}

	return buf.String()
}
