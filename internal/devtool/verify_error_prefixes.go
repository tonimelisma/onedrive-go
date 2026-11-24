package devtool

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// errorPrefixPattern finds the leading `word: ` of an error message.
var errorPrefixPattern = regexp.MustCompile(`^([a-z][a-z0-9]*): `)

// findErrorPrefixViolations returns each error a product package makes, with
// fmt.Errorf or errors.New, whose message leads with the name of another of
// the repository's packages: an error leads with its own package's name or
// with none, so a failure never reads as another package's (CLAUDE.md: error
// prefixes name the current package). Tests may quote any text.
func findErrorPrefixViolations(repoRoot string) ([]outputBoundaryViolation, error) {
	packages, err := repositoryPackageNames(repoRoot)
	if err != nil {
		return nil, err
	}

	var violations []outputBoundaryViolation
	fset := token.NewFileSet()
	walkErr := walkRepositoryGoFiles(repoRoot, func(path, rel string) error {
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		own := file.Name.Name
		imports := importedPackageNames(file)
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall || !isErrorConstructor(imports, call) {
				return true
			}
			lit, isLit := call.Args[0].(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				return true
			}
			message, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				return true
			}
			m := errorPrefixPattern.FindStringSubmatch(message)
			if len(m) < 2 || m[1] == own || !packages[m[1]] {
				return true
			}
			violations = append(violations, outputBoundaryViolation{
				Path: rel,
				Line: fset.Position(call.Pos()).Line,
				Message: fmt.Sprintf("error message leads with %q, another package's name; lead with %q or with no package name",
					m[1]+":", own+":"),
			})

			return true
		})

		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk error prefix sources: %w", walkErr)
	}

	return violations, nil
}

// isErrorConstructor reports whether call is fmt.Errorf or errors.New with at
// least one argument, by the names the file imports them under.
func isErrorConstructor(imports map[string]string, call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return false
	}
	switch imports[pkg.Name] {
	case "fmt":
		return sel.Sel.Name == "Errorf"
	case "errors":
		return sel.Sel.Name == "New"
	default:
		return false
	}
}

// repositoryPackageNames is the name of every Go package in the repository,
// read from the package clause of each directory's first Go file.
func repositoryPackageNames(repoRoot string) (map[string]bool, error) {
	names := map[string]bool{}
	seen := map[string]bool{}
	err := walkRepositoryGoFiles(repoRoot, func(path, rel string) error {
		dir := filepath.Dir(rel)
		if seen[dir] || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		seen[dir] = true
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		names[file.Name.Name] = true

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list repository packages: %w", err)
	}

	return names, nil
}

// walkRepositoryGoFiles calls visit for each Go file the repository rules
// read, with its path and its slash-separated path relative to repoRoot.
func walkRepositoryGoFiles(repoRoot string, visit func(path, rel string) error) error {
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if entry.IsDir() {
			if path != repoRoot && skipRepositoryRuleDir(entry.Name()) {
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

		return visit(path, filepath.ToSlash(rel))
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", repoRoot, err)
	}

	return nil
}
