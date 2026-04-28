package devtool

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type shortcutArchitectureRule struct {
	Name      string
	Root      string
	FileMatch func(string) bool
	Forbidden []shortcutArchitecturePattern
}

type shortcutArchitecturePattern struct {
	Needle      string
	Description string
	Allow       func(string) bool
}

const (
	shortcutRootRecordSelector = "ShortcutRootRecord"
	shortcutRootStateSelector  = "ShortcutRootState"
	protectedPathsSelector     = "ProtectedPaths"
	blockedDetailSelector      = "BlockedDetail"
	waitingSelector            = "Waiting"
)

func RunShortcutArchitectureChecks(repoRoot string) error {
	if strings.TrimSpace(repoRoot) == "" {
		return fmt.Errorf("repo root is required")
	}
	var errs []error
	for _, rule := range shortcutArchitectureRules() {
		if err := runShortcutArchitectureRule(repoRoot, rule); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func shortcutArchitectureRules() []shortcutArchitectureRule {
	return []shortcutArchitectureRule{
		shortcutProductionMultisyncRule(),
		shortcutMultisyncTestRule(),
		shortcutCLIStatusRule(),
		shortcutDeletedConceptRule(),
	}
}

func shortcutProductionMultisyncRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "production multisync shortcut authority",
		Root: "internal/multisync",
		FileMatch: func(path string) bool {
			return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
		},
		Forbidden: []shortcutArchitecturePattern{
			{Needle: "github.com/tonimelisma/onedrive-go/internal/graph", Description: "multisync must not import Graph"},
			{Needle: "config.DefaultDataDir()", Description: "multisync cleanup must receive an explicit data dir"},
			{Needle: "config.MountStatePath(", Description: "multisync child state paths must use the explicit orchestrator data dir"},
			{Needle: shortcutRootRecordSelector, Description: "multisync must not read parent shortcut-root records"},
			{Needle: shortcutRootStateSelector, Description: "multisync must not branch on parent shortcut-root states"},
			{Needle: "." + protectedPathsSelector, Description: "multisync must not branch on parent status-only protected paths"},
			{Needle: "." + blockedDetailSelector, Description: "multisync must not branch on parent status-only blocked details"},
			{
				Needle:      "." + waitingSelector,
				Description: "multisync must not branch on parent waiting replacement state",
				Allow:       allowWaitingReplacementView,
			},
			{Needle: "applyShortcutAliasMutation", Description: "multisync must not call parent alias mutation"},
			{Needle: "shortcutAliasMutation", Description: "multisync must not know parent alias mutation types"},
			{Needle: "StandaloneContentRootWins", Description: "multisync must not own remote-content-root conflict policy"},
			{Needle: "content root already projected", Description: "multisync must not suppress automatic children by remote content root"},
		},
	}
}

func shortcutMultisyncTestRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "multisync shortcut tests use child process snapshots",
		Root: "internal/multisync",
		FileMatch: func(path string) bool {
			return strings.HasSuffix(path, "_test.go")
		},
		Forbidden: []shortcutArchitecturePattern{
			{
				Needle:      "syncengine." + shortcutRootRecordSelector,
				Description: "multisync tests must build child process snapshots, not parent root records",
			},
			{Needle: "syncengine." + shortcutRootStateSelector, Description: "multisync tests must not construct parent root states"},
			{Needle: "." + protectedPathsSelector, Description: "multisync tests must not assert parent protected path internals"},
			{Needle: "." + blockedDetailSelector, Description: "multisync tests must not assert parent blocked detail internals"},
			{
				Needle:      "." + waitingSelector,
				Description: "multisync tests must not assert parent waiting replacement internals",
				Allow:       allowWaitingReplacementView,
			},
		},
	}
}

func shortcutCLIStatusRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "cli shortcut status view boundary",
		Root: "internal/cli",
		FileMatch: func(path string) bool {
			base := filepath.Base(path)
			return strings.HasSuffix(base, ".go") &&
				!strings.HasSuffix(base, "_test.go") &&
				strings.HasPrefix(base, "status")
		},
		Forbidden: []shortcutArchitecturePattern{
			{Needle: shortcutRootRecordSelector, Description: "CLI status must consume ShortcutRootStatusView, not raw root records"},
			{Needle: shortcutRootStateSelector, Description: "CLI status must not derive lifecycle policy from raw states"},
			{Needle: "." + protectedPathsSelector, Description: "CLI status must not read raw protected paths"},
			{Needle: "." + blockedDetailSelector, Description: "CLI status must not read raw blocked details"},
			{
				Needle:      "." + waitingSelector,
				Description: "CLI status must not read raw waiting replacement state",
				Allow:       allowWaitingReplacementView,
			},
		},
	}
}

func shortcutDeletedConceptRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "shortcut deleted concept drift",
		Root: ".",
		FileMatch: func(path string) bool {
			if path == filepath.Join("internal", "devtool", "shortcut_architecture_guard.go") ||
				path == filepath.Join("internal", "devtool", "shortcut_architecture_guard_test.go") {
				return false
			}
			if strings.HasPrefix(path, ".git"+string(filepath.Separator)) {
				return false
			}
			if strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".yml") {
				return true
			}
			return filepath.Base(path) == "AGENTS.md" || filepath.Base(path) == "CLAUDE.md"
		},
		Forbidden: []shortcutArchitecturePattern{
			{Needle: "preflight parent", Description: "use normal parent child-process snapshot terminology"},
			{Needle: "prepared parent", Description: "use normal parent child-process snapshot terminology"},
			{Needle: "shortcut bootstrap", Description: "shortcut lifecycle must use normal parent run/bootstrap vocabulary"},
			{Needle: "publishParentStartupChildTopology", Description: "startup topology publisher was removed"},
			{Needle: "PublishInitialChildTopology", Description: "startup topology publisher was removed"},
			{Needle: "StandaloneContentRootWins", Description: "standalone-vs-automatic remote-content conflict policy was removed"},
			{
				Needle:      "content root already projected by standalone mount",
				Description: "standalone-vs-automatic remote-content conflict policy was removed",
			},
			{Needle: "localReservations", Description: "parent-owned protected roots replaced multisync reservations"},
			{Needle: "localSkipDirs", Description: "parent-owned protected roots replaced multisync skip dirs"},
			{Needle: "live shortcut delete E2E", Description: "live shortcut delete E2E is out of scope without Microsoft support"},
			{Needle: "shortcut delete E2E", Description: "live shortcut delete E2E is out of scope without Microsoft support"},
		},
	}
}

const (
	shortcutRuleProductionMultisync = "production multisync shortcut authority"
	shortcutRuleMultisyncTests      = "multisync shortcut tests use child process snapshots"
	shortcutRuleCLIStatus           = "cli shortcut status view boundary"
	shortcutRuleDeletedConcepts     = "shortcut deleted concept drift"
)

func allowWaitingReplacementView(line string) bool {
	return strings.Contains(line, "WaitingReplacement")
}

func runShortcutArchitectureRule(repoRoot string, rule shortcutArchitectureRule) error {
	root := filepath.Join(repoRoot, rule.Root)
	if _, err := stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", path, walkErr)
		}
		if entry.IsDir() {
			if shouldSkipShortcutArchitectureDir(entry) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		if !rule.FileMatch(rel) {
			return nil
		}
		if err := checkShortcutArchitectureFile(rule, path, rel); err != nil {
			errs = append(errs, err)
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func shouldSkipShortcutArchitectureDir(entry fs.DirEntry) bool {
	switch entry.Name() {
	case ".git", ".hg", ".svn", ".jj":
		return true
	default:
		return false
	}
}

func checkShortcutArchitectureFile(rule shortcutArchitectureRule, path string, rel string) error {
	data, err := readFile(path)
	if err != nil {
		return err
	}
	var errs []error
	if astErr := checkShortcutArchitectureGoFile(rule, rel, data); astErr != nil {
		errs = append(errs, astErr)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		for _, pattern := range rule.Forbidden {
			if !strings.Contains(line, pattern.Needle) {
				continue
			}
			if pattern.Allow != nil && pattern.Allow(line) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s:%d: %s: %s", rel, lineNo, rule.Name, pattern.Description))
		}
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, fmt.Errorf("scan %s: %w", rel, err))
	}
	return errors.Join(errs...)
}

func checkShortcutArchitectureGoFile(rule shortcutArchitectureRule, rel string, data []byte) error {
	if !strings.HasSuffix(rel, ".go") {
		return nil
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, rel, data, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse %s: %w", rel, err)
	}

	var errs []error
	switch rule.Name {
	case shortcutRuleProductionMultisync:
		errs = append(errs, checkMultisyncShortcutGoFile(fileSet, file, rel, false)...)
		errs = append(errs, checkMultisyncShortcutPathDrift(rel)...)
	case shortcutRuleMultisyncTests:
		errs = append(errs, checkMultisyncShortcutGoFile(fileSet, file, rel, true)...)
	case shortcutRuleCLIStatus:
		errs = append(errs, checkCLIShortcutStatusGoFile(fileSet, file, rel)...)
	}
	return errors.Join(errs...)
}

func checkMultisyncShortcutPathDrift(rel string) []error {
	base := filepath.Base(rel)
	if strings.HasPrefix(base, "shortcut_topology") {
		return []error{
			fmt.Errorf(
				"%s:1: production multisync shortcut authority: use child process snapshot file names, not shortcut topology",
				rel,
			),
		}
	}
	return nil
}

func checkMultisyncShortcutGoFile(
	fileSet *token.FileSet,
	file *ast.File,
	rel string,
	tests bool,
) []error {
	imports := shortcutImportAliases(file)
	var errs []error
	for _, importPath := range imports {
		if !tests && shortcutImportIsGraph(importPath) {
			errs = append(errs, shortcutASTError(fileSet, rel, file.Name, "multisync must not import Graph"))
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch expr := node.(type) {
		case *ast.SelectorExpr:
			selector := expr.Sel.Name
			qualifier := shortcutSelectorQualifier(expr)
			importPath := imports[qualifier]
			if tests {
				errs = append(errs, checkMultisyncTestSelector(fileSet, rel, expr, selector, importPath)...)
				return true
			}
			errs = append(errs, checkProductionMultisyncSelector(fileSet, rel, expr, selector, importPath)...)
		case *ast.Ident:
			if tests {
				errs = append(errs, checkMultisyncTestIdent(fileSet, rel, expr)...)
				return true
			}
			if !qualifiedShortcutIdentIsAllowed(expr.Name) {
				errs = append(errs, checkProductionMultisyncIdent(fileSet, rel, expr)...)
			}
		}
		return true
	})
	return errs
}

func checkProductionMultisyncSelector(
	fileSet *token.FileSet,
	rel string,
	expr *ast.SelectorExpr,
	selector string,
	importPath string,
) []error {
	var errs []error
	switch {
	case shortcutImportIsConfig(importPath) && selector == "DefaultDataDir":
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync cleanup must receive an explicit data dir"))
	case shortcutImportIsConfig(importPath) && selector == "MountStatePath":
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync child state paths must use the explicit orchestrator data dir"))
	case shortcutImportIsSync(importPath) && selector == shortcutRootRecordSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync must not read parent shortcut-root records"))
	case shortcutImportIsSync(importPath) && selector == shortcutRootStateSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync must not branch on parent shortcut-root states"))
	case selector == protectedPathsSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync must not branch on parent status-only protected paths"))
	case selector == blockedDetailSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync must not branch on parent status-only blocked details"))
	case selector == waitingSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync must not branch on parent waiting replacement state"))
	}
	return errs
}

func checkProductionMultisyncIdent(fileSet *token.FileSet, rel string, ident *ast.Ident) []error {
	switch ident.Name {
	case shortcutRootRecordSelector:
		return []error{shortcutASTError(fileSet, rel, ident, "multisync must not read parent shortcut-root records")}
	case shortcutRootStateSelector:
		return []error{shortcutASTError(fileSet, rel, ident, "multisync must not branch on parent shortcut-root states")}
	default:
		return nil
	}
}

func checkMultisyncTestSelector(
	fileSet *token.FileSet,
	rel string,
	expr *ast.SelectorExpr,
	selector string,
	importPath string,
) []error {
	var errs []error
	switch {
	case shortcutImportIsSync(importPath) && selector == shortcutRootRecordSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync tests must build child process snapshots, not parent root records"))
	case shortcutImportIsSync(importPath) && selector == shortcutRootStateSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync tests must not construct parent root states"))
	case selector == protectedPathsSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync tests must not assert parent protected path internals"))
	case selector == blockedDetailSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync tests must not assert parent blocked detail internals"))
	case selector == waitingSelector:
		errs = append(errs, shortcutASTError(fileSet, rel, expr, "multisync tests must not assert parent waiting replacement internals"))
	}
	return errs
}

func checkMultisyncTestIdent(fileSet *token.FileSet, rel string, ident *ast.Ident) []error {
	switch ident.Name {
	case shortcutRootRecordSelector:
		return []error{shortcutASTError(fileSet, rel, ident, "multisync tests must build child process snapshots, not parent root records")}
	case shortcutRootStateSelector:
		return []error{shortcutASTError(fileSet, rel, ident, "multisync tests must not construct parent root states")}
	default:
		return nil
	}
}

func checkCLIShortcutStatusGoFile(fileSet *token.FileSet, file *ast.File, rel string) []error {
	imports := shortcutImportAliases(file)
	var errs []error
	ast.Inspect(file, func(node ast.Node) bool {
		switch expr := node.(type) {
		case *ast.SelectorExpr:
			selector := expr.Sel.Name
			qualifier := shortcutSelectorQualifier(expr)
			importPath := imports[qualifier]
			switch {
			case shortcutImportIsSync(importPath) && selector == shortcutRootRecordSelector:
				errs = append(errs, shortcutASTError(fileSet, rel, expr, "CLI status must consume ShortcutRootStatusView, not raw root records"))
			case shortcutImportIsSync(importPath) && selector == shortcutRootStateSelector:
				errs = append(errs, shortcutASTError(fileSet, rel, expr, "CLI status must not derive lifecycle policy from raw states"))
			case selector == protectedPathsSelector:
				errs = append(errs, shortcutASTError(fileSet, rel, expr, "CLI status must not read raw protected paths"))
			case selector == blockedDetailSelector:
				errs = append(errs, shortcutASTError(fileSet, rel, expr, "CLI status must not read raw blocked details"))
			case selector == waitingSelector:
				errs = append(errs, shortcutASTError(fileSet, rel, expr, "CLI status must not read raw waiting replacement state"))
			}
		case *ast.Ident:
			switch expr.Name {
			case shortcutRootRecordSelector:
				errs = append(errs, shortcutASTError(fileSet, rel, expr, "CLI status must consume ShortcutRootStatusView, not raw root records"))
			case shortcutRootStateSelector:
				errs = append(errs, shortcutASTError(fileSet, rel, expr, "CLI status must not derive lifecycle policy from raw states"))
			}
		}
		return true
	})
	return errs
}

func shortcutImportAliases(file *ast.File) map[string]string {
	aliases := make(map[string]string)
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if spec.Name != nil {
			switch spec.Name.Name {
			case "_":
				continue
			case ".":
				aliases[""] = importPath
			default:
				aliases[spec.Name.Name] = importPath
			}
			continue
		}
		aliases[filepath.Base(importPath)] = importPath
	}
	return aliases
}

func shortcutSelectorQualifier(expr *ast.SelectorExpr) string {
	ident, ok := expr.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}

func shortcutImportIsConfig(importPath string) bool {
	return importPath == "github.com/tonimelisma/onedrive-go/internal/config"
}

func shortcutImportIsGraph(importPath string) bool {
	return importPath == "github.com/tonimelisma/onedrive-go/internal/graph" ||
		strings.HasPrefix(importPath, "github.com/tonimelisma/onedrive-go/internal/graph/")
}

func shortcutImportIsSync(importPath string) bool {
	return importPath == "github.com/tonimelisma/onedrive-go/internal/sync"
}

func qualifiedShortcutIdentIsAllowed(name string) bool {
	return name == ""
}

func shortcutASTError(fileSet *token.FileSet, rel string, node ast.Node, description string) error {
	position := fileSet.Position(node.Pos())
	line := position.Line
	if line == 0 {
		line = 1
	}
	return fmt.Errorf("%s:%d: shortcut architecture guard: %s", rel, line, description)
}
