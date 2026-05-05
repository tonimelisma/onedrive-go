package devtool

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
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

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}

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

func shouldSkipTestConventionDir(name string) bool {
	switch name {
	case verifyGitDir, verifyTestdataDir, ".worktrees", verifyVendorDir:
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
