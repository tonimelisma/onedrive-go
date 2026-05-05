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
	"strconv"
	"strings"
)

type outputBoundaryViolation struct {
	Path    string
	Line    int
	Message string
}

const (
	verifyGitDir      = ".git"
	verifyTestdataDir = ".testdata"
	verifyVendorDir   = "vendor"
)

func runOutputBoundaries(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	if err := writeStatus(stdout, "==> output boundaries\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	violations, err := findOutputBoundaryViolations(repoRoot)
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}

	return fmt.Errorf("output boundary check failed:\n%s", formatOutputBoundaryViolations(violations))
}

func findOutputBoundaryViolations(repoRoot string) ([]outputBoundaryViolation, error) {
	var violations []outputBoundaryViolation
	fset := token.NewFileSet()

	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}

		if entry.IsDir() {
			if shouldSkipOutputBoundaryDir(entry.Name()) {
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

		if outputBoundaryFileAllowed(rel) {
			return nil
		}

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

			importPath := imports[ident.Name]
			message, blocked := outputBoundarySelectorMessage(importPath, selector.Sel.Name)
			if !blocked {
				return true
			}

			position := fset.Position(selector.Pos())
			violations = append(violations, outputBoundaryViolation{
				Path:    rel,
				Line:    position.Line,
				Message: message,
			})

			return true
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk output boundary sources: %w", err)
	}

	return violations, nil
}

func shouldSkipOutputBoundaryDir(name string) bool {
	switch name {
	case verifyGitDir, verifyTestdataDir, verifyVendorDir:
		return true
	default:
		return false
	}
}

func outputBoundaryFileAllowed(rel string) bool {
	if strings.HasSuffix(rel, "_test.go") {
		return true
	}

	if strings.HasPrefix(rel, "cmd/devtool/") ||
		strings.HasPrefix(rel, "internal/devtool/") ||
		strings.HasPrefix(rel, "e2e/") ||
		strings.HasPrefix(rel, "testutil/") {
		return true
	}

	return rel == "internal/cli/root.go" || rel == "internal/cli/format.go"
}

func importedPackageNames(file *ast.File) map[string]string {
	imports := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}

		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." || name == "_" {
			continue
		}

		imports[name] = importPath
	}

	return imports
}

func outputBoundarySelectorMessage(importPath string, selector string) (string, bool) {
	switch importPath {
	case "log/slog":
		if slices.Contains([]string{"Debug", "Info", "Warn", "Error"}, selector) {
			return "direct slog." + selector + " bypasses the CLI-owned logger; pass an injected *slog.Logger", true
		}
	case "os":
		if selector == "Stdout" || selector == "Stderr" {
			return "direct os." + selector + " bypasses CLI-owned output/status writers", true
		}
	case "log":
		if strings.HasPrefix(selector, "Print") {
			return "direct log." + selector + " bypasses the CLI-owned logger", true
		}
	}

	return "", false
}

func formatOutputBoundaryViolations(violations []outputBoundaryViolation) string {
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
