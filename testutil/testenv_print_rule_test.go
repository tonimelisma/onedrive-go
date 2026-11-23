package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// printRedactors are the calls a value of testenv.go passes through before
// it is printed: each names a file, a drive or an error without a path or an
// address.
func printRedactors() []string {
	return []string{"credentialFileRole", "pathlessError", "Role"}
}

// rawPrintArguments lists, as "<line>: <argument>", every argument of a
// fmt print call in source that may print a raw value: one that is neither a
// string literal, nor literals joined with +, nor a call of printRedactors.
// The writer a Fprint call is given is not printed.
func rawPrintArguments(t *testing.T, source string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "testenv.go", source, parser.SkipObjectResolution)
	require.NoError(t, err)

	var findings []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector || !isIdentNamed(selector.X, "fmt") || !strings.Contains(selector.Sel.Name, "Print") ||
			strings.HasPrefix(selector.Sel.Name, "Sprint") {
			return true
		}
		args := call.Args
		if strings.HasPrefix(selector.Sel.Name, "F") && len(args) > 0 {
			args = args[1:]
		}
		for _, arg := range args {
			if !isRedactedPrintArgument(arg) {
				findings = append(findings, strconv.Itoa(fset.Position(arg.Pos()).Line)+": "+source[fset.Position(arg.Pos()).Offset:fset.Position(arg.End()).Offset])
			}
		}

		return true
	})

	return findings
}

func isIdentNamed(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)

	return ok && ident.Name == name
}

// isRedactedPrintArgument reports a string literal, literals joined with +,
// or a call of one of printRedactors.
func isRedactedPrintArgument(expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		return expr.Kind == token.STRING
	case *ast.BinaryExpr:
		return expr.Op == token.ADD && isRedactedPrintArgument(expr.X) && isRedactedPrintArgument(expr.Y)
	case *ast.ParenExpr:
		return isRedactedPrintArgument(expr.X)
	case *ast.CallExpr:
		switch fun := expr.Fun.(type) {
		case *ast.Ident:
			return slices.Contains(printRedactors(), fun.Name)
		case *ast.SelectorExpr:
			return slices.Contains(printRedactors(), fun.Sel.Name)
		}
	}

	return false
}

// Validates: R-6.10.27
//
// The test environment prints from TestMain to a live job's stderr, public
// CI output, before the suite's masks exist, and a token file's path holds
// its account's address: every value testenv.go prints passes through a
// redactor that names a file, a drive or an error without a path. Planted
// rows prove the rule fails a raw path, a raw drive and an error printed
// whole, and passes a redacted print.
func TestTestEnvPrintsNoRawPathOrDrive(t *testing.T) {
	t.Parallel()

	source, err := readProductSource("testenv.go")
	require.NoError(t, err)
	assert.Empty(t, rawPrintArguments(t, source), "testenv.go prints a raw value: pass it through a redactor")

	planted := `package testutil

func planted(tokenPath, dir, driveID string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL: required token file not found: %s\n", tokenPath)
	fmt.Fprintln(os.Stderr, "FATAL: .testdata/ directory not found at "+dir)
	fmt.Fprintf(os.Stderr, "FATAL: cannot parse drive %q\n", driveID)
	fmt.Fprintf(os.Stderr, "FATAL: cannot read: %v\n", err)
	fmt.Fprintf(os.Stderr, "FATAL: cannot read %s: %s\n", credentialFileRole(tokenPath), pathlessError(err))
	fmt.Fprintf(os.Stderr, "FATAL: token of %s\n", NewDriveRoles(driveID).Role(driveID))
	fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
	_ = fmt.Sprintf("%s", tokenPath)
}
`
	assert.Equal(t, []string{
		"4: tokenPath",
		`5: "FATAL: .testdata/ directory not found at "+dir`,
		"6: driveID",
		"7: err",
	}, rawPrintArguments(t, planted))
}
