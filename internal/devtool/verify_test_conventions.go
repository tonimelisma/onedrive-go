package devtool

import (
	"context"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// testConventionViolation is a test the integration tier would never run, a
// test wait bounded by a literal duration (verify_test_waits.go), or a
// production use of a test seam. The other test conventions (testify for
// failures, no wall-clock sleep, no fixed-wait absence assertion) are lint
// rules: forbidigo in .golangci.yml.
type testConventionViolation struct {
	Path    string
	Line    int
	Message string
}

func runTestConventions(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	if err := writeStatus(stdout, "==> test conventions\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	violations, err := findTestConventionViolations(repoRoot)
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}

	return fmt.Errorf("test convention check failed:\n%s", formatTestConventionViolations(violations))
}

func findTestConventionViolations(repoRoot string) ([]testConventionViolation, error) {
	var violations []testConventionViolation
	fset := token.NewFileSet()

	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}

		if entry.IsDir() {
			if skipRepositoryRuleDir(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativize %s: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)

		isTest := strings.HasSuffix(path, "_test.go")
		if !isTest && isTestSeamHelperPath(rel) {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		if isTest {
			violations = append(violations, integrationTestNameViolations(fset, rel, file)...)
			violations = append(violations, literalWaitViolations(fset, rel, file)...)
		} else {
			violations = append(violations, testSeamUseViolations(fset, rel, file)...)
			violations = append(violations, hookSettingViolations(fset, rel, file)...)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk test convention sources: %w", err)
	}

	return violations, nil
}

// testMainName is the test binary's entry point, which go test does not run as
// a test.
const testMainName = "TestMain"

// integrationTestNameViolations holds every test of a file built with the
// integration tag to integrationTestPrefix. The integration tier selects its
// tests by that prefix, so a test under any other name would never run there
// and the tier would not say so.
func integrationTestNameViolations(fset *token.FileSet, rel string, file *ast.File) []testConventionViolation {
	if !fileBuildConstraintNames(file, integrationBuildTag) {
		return nil
	}

	var violations []testConventionViolation
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		name := fn.Name.Name
		if !isGoTestName(name) || name == testMainName || strings.HasPrefix(name, integrationTestPrefix) {
			continue
		}
		violations = append(violations, testConventionViolation{
			Path: rel,
			Line: fset.Position(fn.Pos()).Line,
			Message: fmt.Sprintf("%s is in a file built with the %s tag but is not named %s...; "+
				"the integration tier runs -run=^%s and would never run it",
				name, integrationBuildTag, integrationTestPrefix, integrationTestPrefix),
		})
	}

	return violations
}

// testSeamSuffix ends the name of a test seam: a production function that
// exists so the tests of another package can reach state they could not
// reach otherwise (NewBaselineForTest). An export_test.go file cannot serve
// them, because only its own package's tests see it.
const testSeamSuffix = "ForTest"

// testSeamHelperDirs returns the packages that hold shared test support
// code, which may use a test seam like a test file.
func testSeamHelperDirs() []string {
	return []string{"internal/sync/internal/syncfixture", "internal/sync/internal/syncstoretest"}
}

// isTestSeamHelperPath reports whether rel is a file of a test-helper package.
func isTestSeamHelperPath(rel string) bool {
	for _, dir := range testSeamHelperDirs() {
		if strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}

	return false
}

// testSeamUseViolations holds every use of a test seam in a production file:
// declaring one there is how a seam exists, calling one is production code
// depending on a test hook. Importing a test-helper package is such a use
// too: the helpers call seams on the importer's behalf.
func testSeamUseViolations(fset *token.FileSet, rel string, file *ast.File) []testConventionViolation {
	declared := make(map[*ast.Ident]bool)
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			declared[fn.Name] = true
		}
	}

	violations := testHelperImportViolations(fset, rel, file)
	ast.Inspect(file, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok || declared[ident] || ident.Name == testSeamSuffix || !strings.HasSuffix(ident.Name, testSeamSuffix) {
			return true
		}
		violations = append(violations, testConventionViolation{
			Path: rel,
			Line: fset.Position(ident.Pos()).Line,
			Message: fmt.Sprintf("%s is a test seam (...%s): only test files and the sync test-helper "+
				"packages may use it", ident.Name, testSeamSuffix),
		})

		return true
	})

	return violations
}

// hooksTypeSuffix ends the name of a hooks type: a package's test seams in
// one value whose zero value is production behavior (engineHooks,
// syncexec.Hooks).
const hooksTypeSuffix = "Hooks"

// hooksFieldNames are the names a struct field holding a hooks value has.
func hooksFieldNames() []string {
	return []string{"hooks", hooksTypeSuffix}
}

// hookSettingViolations holds every place a production file sets a test seam:
// a composite literal of a hooks type with a field in it, or an assignment
// to a field of a hooks value at any depth (x.hooks.F = ...). Production
// code may pass a hooks value through, an empty one included, and read its
// fields; setting one is a test's job (R-6.10.29). The rule is by name, without type
// information: a type whose name ends in Hooks, a field named hooks or Hooks.
func hookSettingViolations(fset *token.FileSet, rel string, file *ast.File) []testConventionViolation {
	var violations []testConventionViolation
	report := func(node ast.Node, message string) {
		violations = append(violations, testConventionViolation{
			Path:    rel,
			Line:    fset.Position(node.Pos()).Line,
			Message: message,
		})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CompositeLit:
			if name := typeExprName(node.Type); strings.HasSuffix(name, hooksTypeSuffix) && len(node.Elts) > 0 {
				report(node, fmt.Sprintf("%s is a hooks type: only test files may build one with a field set; "+
					"production passes the zero value", name))
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if field, ok := hooksFieldAssigned(lhs); ok {
					report(lhs, fmt.Sprintf("%s is a test seam: only test files may assign a field of a hooks value", field))
				}
			}
		}

		return true
	})

	return violations
}

// typeExprName is the name of a composite literal's type: T, pkg.T, or empty
// for any other form.
func typeExprName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.SelectorExpr:
		return expr.Sel.Name
	default:
		return ""
	}
}

// hooksFieldAssigned reports whether lhs is a field of a hooks value, at any
// depth (x.hooks.F, x.Hooks.F, x.hooks.observe.F), and names it from the
// hooks field down.
func hooksFieldAssigned(lhs ast.Expr) (string, bool) {
	field, ok := lhs.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	path := field.Sel.Name
	for holder, ok := field.X.(*ast.SelectorExpr); ok; holder, ok = holder.X.(*ast.SelectorExpr) {
		path = holder.Sel.Name + "." + path
		for _, name := range hooksFieldNames() {
			if holder.Sel.Name == name {
				return path, true
			}
		}
	}

	return "", false
}

// testHelperImportViolations holds every import of a test-helper package in a
// production file. The helper packages themselves are not checked, so they
// may import each other.
func testHelperImportViolations(fset *token.FileSet, rel string, file *ast.File) []testConventionViolation {
	var violations []testConventionViolation
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		for _, dir := range testSeamHelperDirs() {
			if path != repoModulePath+"/"+dir {
				continue
			}
			violations = append(violations, testConventionViolation{
				Path: rel,
				Line: fset.Position(spec.Pos()).Line,
				Message: fmt.Sprintf("%s is a sync test-helper package: only test files and the other "+
					"test-helper packages may import it", path),
			})
		}
	}

	return violations
}

// isGoTestName reports whether go test runs a function of this name as a test:
// Test, then nothing or a character that is not a lower-case letter.
func isGoTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok {
		return false
	}
	first, _ := utf8.DecodeRuneInString(rest)

	return rest == "" || !unicode.IsLower(first)
}

// fileBuildConstraintNames reports whether the file's //go:build constraint
// names tag outside a negation: `integration`, `integration && linux` and
// `linux || integration` do, `!integration` does not, because that file is
// built exactly when the tag is off. A constraint that does not parse names
// nothing here: the go command refuses to build such a file, so the build
// reports it.
func fileBuildConstraintNames(file *ast.File, tag string) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err != nil {
				continue
			}
			if constraintNamesTag(expr, tag, false) {
				return true
			}
		}
	}

	return false
}

// constraintNamesTag walks one constraint expression for tag. negated says
// whether an odd number of ! operators enclose expr.
func constraintNamesTag(expr constraint.Expr, tag string, negated bool) bool {
	switch node := expr.(type) {
	case *constraint.TagExpr:
		return !negated && node.Tag == tag
	case *constraint.NotExpr:
		return constraintNamesTag(node.X, tag, !negated)
	case *constraint.AndExpr:
		return constraintNamesTag(node.X, tag, negated) || constraintNamesTag(node.Y, tag, negated)
	case *constraint.OrExpr:
		return constraintNamesTag(node.X, tag, negated) || constraintNamesTag(node.Y, tag, negated)
	default:
		return false
	}
}

func formatTestConventionViolations(violations []testConventionViolation) string {
	var builder strings.Builder
	for i, violation := range violations {
		if i > 0 {
			builder.WriteByte('\n')
		}

		_, _ = fmt.Fprintf(
			&builder,
			"%s:%d: %s",
			violation.Path,
			violation.Line,
			violation.Message,
		)
	}

	return builder.String()
}
