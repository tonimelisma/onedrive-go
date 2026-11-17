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
	"strconv"
	"strings"
)

// outputBoundaryViolation is an "error" log attribute flattened to text in
// product code. The other output boundaries (package-level slog, os.Stdout
// and os.Stderr, log.Print) are lint rules: forbidigo in .golangci.yml, with
// an exclusion for the files outputBoundaryFileAllowed allows here too.
type outputBoundaryViolation struct {
	Path    string
	Line    int
	Message string
}

const (
	verifyGitDir         = ".git"
	verifyTestdataDir    = ".testdata"
	verifyWorktreesDir   = ".worktrees"
	verifyNodeModulesDir = "node_modules"
	verifyVendorDir      = "vendor"

	slogImportPath = "log/slog"
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
			if call, isCall := node.(*ast.CallExpr); isCall && isFlattenedErrorAttr(imports, call) {
				violations = append(violations, outputBoundaryViolation{
					Path:    rel,
					Line:    fset.Position(call.Pos()).Line,
					Message: flattenedErrorAttrMessage,
				})
			}

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

// flattenedErrorAttrMessage explains the "error" attribute rule. The console
// renders an error value as its plain reason and the log file keeps the full
// chain; a pre-flattened string is shown as given, which printed whole wrap
// chains (delta URLs and cursors included) on the default console.
const flattenedErrorAttrMessage = `slog.String("error", err.Error()) flattens the error; log it as slog.Any("error", err) ` +
	`so the console can show its plain reason`

// isFlattenedErrorAttr matches slog.String("error", x.Error()): an error value
// turned into text at the call. A stored message (a result's ErrMsg) has no
// error value left to log and is allowed.
func isFlattenedErrorAttr(imports map[string]string, call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "String" || len(call.Args) != 2 {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	if !ok || imports[ident.Name] != slogImportPath {
		return false
	}
	key, ok := call.Args[0].(*ast.BasicLit)
	if !ok || key.Kind != token.STRING || key.Value != `"error"` {
		return false
	}
	value, ok := call.Args[1].(*ast.CallExpr)
	if !ok || len(value.Args) != 0 {
		return false
	}
	method, ok := value.Fun.(*ast.SelectorExpr)

	return ok && method.Sel.Name == "Error"
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
