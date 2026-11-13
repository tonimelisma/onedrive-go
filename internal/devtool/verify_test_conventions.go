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
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const allowedDirectSleepTestFile = "e2e/sleep_test_helpers_test.go"

const timePackageName = "time"

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
			if shouldSkipTestConventionDir(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativize %s: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		violations = append(violations, integrationTestNameViolations(fset, rel, file)...)

		imports := importedPackageNames(file)
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			ident, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}

			message, blocked := testConventionSelectorMessage(rel, imports[ident.Name], ident.Name, selector.Sel.Name)
			if !blocked {
				return true
			}

			position := fset.Position(selector.Pos())
			violations = append(violations, testConventionViolation{
				Path:    rel,
				Line:    position.Line,
				Message: message,
			})

			return true
		})

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

func shouldSkipTestConventionDir(name string) bool {
	switch name {
	case verifyGitDir, verifyTestdataDir, verifyWorktreesDir, verifyVendorDir:
		return true
	default:
		return false
	}
}

func testConventionSelectorMessage(rel string, importPath string, receiver string, selector string) (string, bool) {
	if receiver == "t" && slices.Contains([]string{"Fatal", "Fatalf", "Error", "Errorf"}, selector) {
		return "direct t." + selector + " bypasses the testify assertion convention; use assert or require", true
	}

	if importPath == timePackageName && selector == "Sleep" && rel != allowedDirectSleepTestFile {
		return "direct " + receiver + ".Sleep in tests hides synchronization contracts; use explicit synchronization " +
			"or the centralized live E2E sleep helper", true
	}

	return "", false
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
