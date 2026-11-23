package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file holds a rule of the live suite read from its sources, like the
// rules beside it: an assertion on what a sync did reads every pass of its
// converge loop. A pass can do the work and end with an error from unrelated
// churn on the drive; the next, clean pass then has nothing left to name, so
// an action read from the last pass alone fails a sync that did its work
// (syncConvergence). A call that is given a converge result's last output
// (Last().Stdout or Last().Stderr) and a string that starts with a verb the
// sync's summary names a done action with fails the rule; the end state (the
// mode header, the closing line, what the predicate held of the converged
// pass) is read from the last pass.

// syncActionVerbs are the verbs the sync's summary names a done action with,
// read from internal/cli's activityDoneVerb, so a new or reworded action is
// held by the rule without a second edit.
func syncActionVerbs(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "internal", "cli", "sync_console.go"), nil,
		parser.SkipObjectResolution)
	require.NoError(t, err)

	var verbs []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "activityDoneVerb" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			ret, isReturn := node.(*ast.ReturnStmt)
			if !isReturn || len(ret.Results) != 1 {
				return true
			}
			if literal, isLiteral := ret.Results[0].(*ast.BasicLit); isLiteral && literal.Kind == token.STRING {
				verb, unquoteErr := strconv.Unquote(literal.Value)
				require.NoError(t, unquoteErr)
				verbs = append(verbs, verb)
			}

			return true
		})
	}
	require.NotEmpty(t, verbs, "activityDoneVerb names the sync's action verbs: follow its new name")

	return verbs
}

// convergeLastActionAssertions lists, as "<file>:<line>", each call of the
// suite in dir given a converge result's last output and a string that
// starts with one of verbs.
func convergeLastActionAssertions(dir string, verbs []string) ([]string, error) {
	fset, files, err := parseSuiteFiles(dir)
	if err != nil {
		return nil, err
	}

	var findings []string
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !takesLastConvergeOutput(call) || !takesActionText(call, verbs) {
				return true
			}
			position := fset.Position(call.Pos())
			findings = append(findings, fmt.Sprintf("%s:%d", filepath.Base(position.Filename), position.Line))

			return true
		})
	}

	return findings, nil
}

// takesLastConvergeOutput reports a call given <result>.Last().Stdout or
// <result>.Last().Stderr.
func takesLastConvergeOutput(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		output, ok := arg.(*ast.SelectorExpr)
		if !ok || (output.Sel.Name != "Stdout" && output.Sel.Name != "Stderr") {
			continue
		}
		last, isCall := output.X.(*ast.CallExpr)
		if !isCall || len(last.Args) != 0 {
			continue
		}
		if method, isMethod := last.Fun.(*ast.SelectorExpr); isMethod && method.Sel.Name == "Last" {
			return true
		}
	}

	return false
}

// takesActionText reports a call given a string whose constant start, the
// literal a + chain begins with, starts with one of verbs.
func takesActionText(call *ast.CallExpr, verbs []string) bool {
	for _, arg := range call.Args {
		start, ok := constantStart(arg)
		if !ok {
			continue
		}
		for _, verb := range verbs {
			if strings.HasPrefix(start, verb) {
				return true
			}
		}
	}

	return false
}

// constantStart is the string literal expr is, or the one its + chain
// begins with.
func constantStart(expr ast.Expr) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expr.Value)

		return value, err == nil
	case *ast.BinaryExpr:
		if expr.Op == token.ADD {
			return constantStart(expr.X)
		}
	case *ast.ParenExpr:
		return constantStart(expr.X)
	}

	return "", false
}

// Validates: R-6.10.32
func TestE2EActionAssertionsReadEveryConvergePass(t *testing.T) {
	t.Parallel()

	findings, err := convergeLastActionAssertions(".", syncActionVerbs(t))
	require.NoError(t, err)
	for _, finding := range findings {
		assert.Failf(t, "an action assertion reads only the last converge pass",
			"%s: read every pass with AllStdout(); an earlier pass may have done the work", finding)
	}
}

// Validates: R-6.10.32
func TestConvergeActionRuleReportsALastPassActionAssertion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "planted_converge_test.go"), []byte(plantedConvergeSource), 0o600))

	findings, err := convergeLastActionAssertions(dir, syncActionVerbs(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"planted_converge_test.go:7", "planted_converge_test.go:8"}, findings,
		"an action read from the last pass, as a literal and at the start of a + chain; not one read from "+
			"every pass, nor the last pass's end state")
}

// plantedConvergeSource is a live test in miniature; its lines are the ones
// TestConvergeActionRuleReportsALastPassActionAssertion names.
const plantedConvergeSource = `//go:build e2e

package e2e

func TestPlantedConverge(t *testing.T) {
	attempt := requireSyncEventuallyConverges(t, cfgPath, env, timeout, "planted", ready)
	assertOutputContains(t, attempt.Last().Stdout, "Downloaded "+testFolder+"/a.txt")
	assert.Contains(t, attempt.Last().Stdout, "Deleted locally b.txt")
	assertOutputContains(t, attempt.AllStdout(), "Downloaded "+testFolder+"/a.txt")
	assertOutputContains(t, attempt.Last().Stdout, "(download only)")
	assertOutputContains(t, attempt.Last().Stdout, "Done in")
}
`
