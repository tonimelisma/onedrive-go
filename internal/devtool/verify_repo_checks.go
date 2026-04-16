package devtool

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type staleCheck struct {
	name    string
	pattern *regexp.Regexp
}

func runRepoConsistencyChecks(repoRoot string) error {
	for _, check := range []func(string) error{
		ensureNoStaleArchitecturePhrases,
		ensureGovernedDesignDocsHaveOwnershipContracts,
		ensureCrossCuttingDesignDocs,
		ensureCrossCuttingDesignDocEvidence,
		ensureGovernedBehaviorDocsHaveEvidence,
		ensureRequirementReferencesResolve,
		ensureEvidenceDocsReferenceRealTests,
		ensureRecurringIncidentPromotedDocsResolve,
		ensureCLIOutputBoundary,
		ensureGuardedPackagesAvoidRawOS,
		ensureFilterSemanticsWording,
		ensureHTTPClientDoStaysAtApprovedBoundary,
		ensurePrivilegedPackageCallsStayAtApprovedBoundaries,
		ensureNoForbiddenProductionPatterns,
		ensureNoResurrectedFiles,
	} {
		if err := check(repoRoot); err != nil {
			return err
		}
	}

	return nil
}

func ensureNoStaleArchitecturePhrases(repoRoot string) error {
	staleChecks := []staleCheck{
		{name: "stale watch-startup phrase", pattern: regexp.MustCompile("RunWatch calls" + " RunOnce")},
		{name: "stale retry delay phrase", pattern: regexp.MustCompile(`retry\.Reconcile` + `\.Delay`)},
		{name: "stale retry transport phrase", pattern: regexp.MustCompile(`RetryTransport\{Policy:\s*` + `Transport\}`)},
		{name: "stale compatibility-wrapper phrase", pattern: regexp.MustCompile("compatibility" + " wrapper")},
		{name: "stale legacy-bridge phrase", pattern: regexp.MustCompile("migra" + "tion" + " bridge")},
	}

	checkRoots := []string{
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, ".github", "workflows"),
		filepath.Join(repoRoot, "CLAUDE.md"),
	}

	for _, check := range staleChecks {
		match, err := findTextMatch(checkRoots, check.pattern, nil)
		if err != nil {
			return err
		}
		if match != "" {
			return fmt.Errorf("stale architecture/documentation phrase detected (%s): %s", check.name, match)
		}
	}

	return nil
}

func ensureNoForbiddenProductionPatterns(repoRoot string) error {
	goRoots := []string{repoRoot}
	match, err := findTextMatch(goRoots, regexp.MustCompile(`graph\.MustNewClient\(`), func(path string) bool {
		return strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go")
	})
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("production MustNewClient call detected: %s", match)
	}

	match, err = findTextMatch(goRoots, regexp.MustCompile(`internal/`+`trustedpath|trustedpath`+`\.`), func(path string) bool {
		return !strings.HasSuffix(path, ".go")
	})
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("trustedpath usage detected: %s", match)
	}

	match, err = findTextMatch(goRoots, regexp.MustCompile(`internal/(`+`syncstore|synctypes)`), func(path string) bool {
		return !strings.HasSuffix(path, ".go")
	})
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("deleted sync package import/reference detected: %s", match)
	}

	match, err = findTextMatch(goRoots, regexp.MustCompile(`\bCommit`+`Outcome\b|retry re`+`play`), func(path string) bool {
		return !strings.HasSuffix(path, ".go")
	})
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("deleted sync transition vocabulary detected: %s", match)
	}

	return nil
}

func ensureNoResurrectedFiles(repoRoot string) error {
	checks := []struct {
		path string
		err  string
	}{
		{
			path: filepath.Join(repoRoot, "internal", "sync", "orchestrator.go"),
			err:  "control-plane files resurrected under internal/sync",
		},
		{
			path: filepath.Join(repoRoot, "internal", "sync", "drive_runner.go"),
			err:  "control-plane files resurrected under internal/sync",
		},
		{
			path: filepath.Join(repoRoot, "internal", "sync", "engine_flow_test_helpers_test.go"),
			err:  "sync test shim resurrected",
		},
	}

	for _, check := range checks {
		if _, err := stat(check.path); err == nil {
			return errors.New(check.err)
		}
	}

	return nil
}

func ensureCLIOutputBoundary(repoRoot string) error {
	cliRoots := []string{filepath.Join(repoRoot, "internal", "cli")}
	skip := func(path string) bool {
		return strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go")
	}

	checks := []staleCheck{
		{
			name:    "direct fmt.Print in production CLI code",
			pattern: regexp.MustCompile(`fmt\.Print(f|ln)?\(`),
		},
		{
			name:    "direct process-global fmt.Fprint in production CLI code",
			pattern: regexp.MustCompile(`fmt\.Fprint(f|ln)?\(\s*os\.(Stdout|Stderr)\b`),
		},
		{
			name:    "direct process-global writer call in production CLI code",
			pattern: regexp.MustCompile(`os\.(Stdout|Stderr)\.(Write|WriteString)\(`),
		},
	}

	for _, check := range checks {
		match, err := findTextMatch(cliRoots, check.pattern, skip)
		if err != nil {
			return err
		}
		if match != "" {
			return fmt.Errorf("cli output boundary violation (%s): %s", check.name, match)
		}
	}

	return nil
}

func ensureGuardedPackagesAvoidRawOS(repoRoot string) error {
	guardedRoots := []string{
		filepath.Join(repoRoot, "internal", "cli"),
		filepath.Join(repoRoot, "internal", "config"),
		filepath.Join(repoRoot, "internal", "sync"),
		filepath.Join(repoRoot, "internal", "syncverify"),
	}
	skip := func(path string) bool {
		return strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go")
	}

	const guardedOSPattern = `os\.(Stat|ReadDir|Open|OpenFile|Create|CreateTemp|ReadFile|WriteFile|` +
		`Remove|RemoveAll|Rename|Mkdir|MkdirAll|Lstat|Readlink|Symlink|Chmod|Chtimes)\(`

	match, err := findTextMatch(guardedRoots, regexp.MustCompile(guardedOSPattern), skip)
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("raw os filesystem call detected outside boundary packages: %s", match)
	}

	return nil
}

func ensureFilterSemanticsWording(repoRoot string) error {
	roots := []string{
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "CLAUDE.md"),
		filepath.Join(repoRoot, "README.md"),
	}

	match, err := findTextMatch(roots, regexp.MustCompile(`per-drive only`), nil)
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("stale filter semantics wording detected: %s", match)
	}

	return nil
}

func ensureHTTPClientDoStaysAtApprovedBoundary(repoRoot string) error {
	allowedPath := filepath.Join(repoRoot, "internal", "graph", "client_preauth.go")
	internalRoot := filepath.Join(repoRoot, "internal")

	walkErr := filepath.WalkDir(internalRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == allowedPath || strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
			return nil
		}

		match, findErr := findHTTPClientDoCall(path)
		if findErr != nil {
			return findErr
		}
		if match != "" {
			return matchFoundError{value: match}
		}

		return nil
	})
	if walkErr == nil {
		return nil
	}

	var matchErr matchFoundError
	if errors.As(walkErr, &matchErr) {
		return fmt.Errorf("http.Client.Do is only allowed in internal/graph/client_preauth.go: %s", matchErr.value)
	}

	return fmt.Errorf("walk %s: %w", internalRoot, walkErr)
}

type packageSelectorBoundaryRule struct {
	importPath  string
	selector    string
	description string
	allowed     func(string) bool
	roots       []string
}

func ensurePrivilegedPackageCallsStayAtApprovedBoundaries(repoRoot string) error {
	rules := []packageSelectorBoundaryRule{
		{
			importPath:  "os/exec",
			selector:    "Command",
			description: "exec.Command is forbidden in production code",
			allowed: func(string) bool {
				return false
			},
		},
		{
			importPath:  "os/exec",
			selector:    "CommandContext",
			description: "exec.CommandContext is only allowed in internal/cli/auth_login.go and internal/devtool/runner.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "cli", "auth_login.go") ||
					path == filepath.Join(repoRoot, "internal", "devtool", "runner.go")
			},
		},
		{
			importPath:  "database/sql",
			selector:    "Open",
			description: "sql.Open is only allowed in internal/sync/store.go and internal/sync/store_inspect.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "sync", "store.go") ||
					path == filepath.Join(repoRoot, "internal", "sync", "store_inspect.go")
			},
		},
		{
			importPath:  "os/signal",
			selector:    "Notify",
			description: "signal.Notify is only allowed in internal/cli/signal.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "cli", "signal.go")
			},
		},
		{
			importPath:  "os/signal",
			selector:    "Stop",
			description: "signal.Stop is only allowed in internal/cli/signal.go and internal/cli/sync_runtime.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "cli", "signal.go") ||
					path == filepath.Join(repoRoot, "internal", "cli", "sync_runtime.go")
			},
		},
		{
			importPath:  "os",
			selector:    "Exit",
			description: "os.Exit is only allowed in production entrypoints",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "main.go") ||
					path == filepath.Join(repoRoot, "cmd", "devtool", "main.go") ||
					path == filepath.Join(repoRoot, "internal", "cli", "signal.go")
			},
			roots: []string{
				filepath.Join(repoRoot, "main.go"),
				filepath.Join(repoRoot, "internal"),
				filepath.Join(repoRoot, "cmd"),
			},
		},
	}

	for _, rule := range rules {
		if err := ensurePackageSelectorStaysAtApprovedBoundary(repoRoot, rule); err != nil {
			return err
		}
	}

	return nil
}

func ensurePackageSelectorStaysAtApprovedBoundary(repoRoot string, rule packageSelectorBoundaryRule) error {
	roots := rule.roots
	if len(roots) == 0 {
		roots = []string{
			filepath.Join(repoRoot, "internal"),
			filepath.Join(repoRoot, "cmd"),
		}
	}

	for _, root := range roots {
		if err := scanPackageSelectorBoundaryRoot(root, rule); err != nil {
			var matchErr matchFoundError
			if errors.As(err, &matchErr) {
				return fmt.Errorf("%s: %s", rule.description, matchErr.value)
			}

			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return err
		}
	}

	return nil
}

func scanPackageSelectorBoundaryRoot(root string, rule packageSelectorBoundaryRule) error {
	info, statErr := stat(root)
	if statErr != nil {
		return fmt.Errorf("stat %s: %w", root, statErr)
	}
	if !info.IsDir() {
		return scanPackageSelectorFile(root, rule)
	}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		return scanPackageSelectorFile(path, rule)
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", root, walkErr)
	}

	return nil
}

func scanPackageSelectorFile(path string, rule packageSelectorBoundaryRule) error {
	if rule.allowed(path) || strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
		return nil
	}

	match, findErr := findPackageSelectorCall(path, rule.importPath, rule.selector)
	if findErr != nil {
		return findErr
	}
	if match != "" {
		return matchFoundError{value: match}
	}

	return nil
}

func findHTTPClientDoCall(path string) (string, error) {
	data, readErr := readFile(path)
	if readErr != nil {
		return "", fmt.Errorf("read %s: %w", path, readErr)
	}
	if !strings.Contains(string(data), ".Do(") {
		return "", nil
	}

	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, path, data, 0)
	if parseErr != nil {
		return "", fmt.Errorf("parse %s: %w", path, parseErr)
	}
	if !importsPackage(file, "net/http") {
		return "", nil
	}

	return firstHTTPDoCallLocation(path, file, fset), nil
}

func findPackageSelectorCall(path string, importPath, selector string) (string, error) {
	data, readErr := readFile(path)
	if readErr != nil {
		return "", fmt.Errorf("read %s: %w", path, readErr)
	}
	if !strings.Contains(string(data), "."+selector+"(") {
		return "", nil
	}

	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, path, data, 0)
	if parseErr != nil {
		return "", fmt.Errorf("parse %s: %w", path, parseErr)
	}

	aliases := importedNamesForPath(file, importPath)
	if len(aliases) == 0 {
		return "", nil
	}

	var match string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != selector {
			return true
		}

		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, ok := aliases[x.Name]; !ok {
			return true
		}

		match = fmt.Sprintf("%s:%d", path, fset.Position(sel.Pos()).Line)
		return false
	})

	return match, nil
}

func firstHTTPDoCallLocation(path string, file *ast.File, fset *token.FileSet) string {
	var match string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Do" {
			return true
		}

		match = fmt.Sprintf("%s:%d", path, fset.Position(sel.Pos()).Line)
		return false
	})

	return match
}

func importsPackage(file *ast.File, target string) bool {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, "\"") == target {
			return true
		}
	}

	return false
}

func importedNamesForPath(file *ast.File, target string) map[string]struct{} {
	names := make(map[string]struct{})

	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, "\"") != target {
			continue
		}

		name := filepath.Base(target)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}

		names[name] = struct{}{}
	}

	return names
}

func findTextMatch(roots []string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	for _, root := range roots {
		match, err := findTextMatchInRoot(root, pattern, skip)
		if err != nil {
			return "", err
		}
		if match != "" {
			return match, nil
		}
	}

	return "", nil
}

func findTextMatchInRoot(root string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	info, err := stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("stat %s: %w", root, err)
	}

	if !info.IsDir() {
		return scanPathForPattern(root, pattern, skip)
	}

	return walkRootForPattern(root, pattern, skip)
}

func walkRootForPattern(root string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		match, scanErr := scanPathForPattern(path, pattern, skip)
		if scanErr != nil {
			return scanErr
		}
		if match != "" {
			return matchFoundError{value: match}
		}

		return nil
	})
	if walkErr == nil {
		return "", nil
	}

	var matchErr matchFoundError
	if errors.As(walkErr, &matchErr) {
		return matchErr.value, nil
	}

	return "", fmt.Errorf("walk %s: %w", root, walkErr)
}

func scanPathForPattern(path string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	if skip != nil && skip(path) {
		return "", nil
	}

	return scanFileForPattern(path, pattern)
}

type matchFoundError struct {
	value string
}

func (f matchFoundError) Error() string {
	return f.value
}

func scanFileForPattern(path string, pattern *regexp.Regexp) (string, error) {
	data, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if pattern.MatchString(line) {
			return fmt.Sprintf("%s:%d", path, i+1), nil
		}
	}

	return "", nil
}
