package devtool

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type VerifyProfile string

const (
	defaultCoverageThreshold = 76.0
	defaultCoveragePattern   = "onedrive-go-cover.*"

	VerifyDefault     VerifyProfile = "default"
	VerifyPublic      VerifyProfile = "public"
	VerifyE2E         VerifyProfile = "e2e"
	VerifyE2EFull     VerifyProfile = "e2e-full"
	VerifyIntegration VerifyProfile = "integration"
)

type VerifyOptions struct {
	RepoRoot          string
	Profile           VerifyProfile
	CoverageThreshold float64
	CoverageFile      string
	E2ELogDir         string
	Stdout            io.Writer
	Stderr            io.Writer
}

type verifyPlan struct {
	runPublicChecks bool
	runE2E          bool
	runE2EFull      bool
	runIntegration  bool
}

type staleCheck struct {
	name    string
	pattern *regexp.Regexp
}

func RunVerify(ctx context.Context, runner commandRunner, opts VerifyOptions) (runErr error) {
	plan, err := resolveVerifyPlan(opts.Profile)
	if err != nil {
		return err
	}

	stdout, stderr := resolveVerifyWriters(opts)
	coverageThreshold := opts.CoverageThreshold
	if coverageThreshold == 0 {
		coverageThreshold = defaultCoverageThreshold
	}

	coverageFile, cleanup, err := prepareCoverageFile(plan, opts.CoverageFile)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil && runErr == nil {
			runErr = cleanupErr
		}
	}()

	env := os.Environ()
	if err := runPublicVerification(
		ctx,
		runner,
		opts.RepoRoot,
		env,
		stdout,
		stderr,
		coverageFile,
		coverageThreshold,
		plan,
	); err != nil {
		return err
	}

	runErr = runOptionalVerification(ctx, runner, opts, env, stdout, stderr, plan)

	return runErr
}

func resolveVerifyPlan(profile VerifyProfile) (verifyPlan, error) {
	switch profile {
	case VerifyDefault:
		return verifyPlan{
			runPublicChecks: true,
			runE2E:          true,
		}, nil
	case VerifyPublic:
		return verifyPlan{
			runPublicChecks: true,
		}, nil
	case VerifyE2E:
		return verifyPlan{
			runE2E: true,
		}, nil
	case VerifyE2EFull:
		return verifyPlan{
			runE2E:     true,
			runE2EFull: true,
		}, nil
	case VerifyIntegration:
		return verifyPlan{
			runIntegration: true,
		}, nil
	default:
		return verifyPlan{}, fmt.Errorf("usage: devtool verify [default|public|e2e|e2e-full|integration]")
	}
}

func resolveVerifyWriters(opts VerifyOptions) (io.Writer, io.Writer) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	return stdout, stderr
}

func prepareCoverageFile(plan verifyPlan, coverageFile string) (string, func() error, error) {
	if !plan.runPublicChecks || coverageFile != "" {
		return coverageFile, func() error { return nil }, nil
	}

	f, err := localpath.CreateTemp(os.TempDir(), defaultCoveragePattern)
	if err != nil {
		return "", nil, fmt.Errorf("create coverage file: %w", err)
	}

	coverageFile = f.Name()
	if err := f.Close(); err != nil {
		return "", nil, fmt.Errorf("close coverage file: %w", err)
	}

	return coverageFile, func() error {
		if err := localpath.Remove(coverageFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove coverage file %s: %w", coverageFile, err)
		}

		return nil
	}, nil
}

func runPublicVerification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	coverageFile string,
	coverageThreshold float64,
	plan verifyPlan,
) error {
	if !plan.runPublicChecks {
		return nil
	}

	publicSteps := []func(context.Context, commandRunner, string, []string, io.Writer, io.Writer) error{
		runFormat,
		runLint,
		runBuild,
	}
	for _, step := range publicSteps {
		if err := step(ctx, runner, repoRoot, env, stdout, stderr); err != nil {
			return err
		}
	}

	if err := runUnitTests(ctx, runner, repoRoot, env, coverageFile, stdout, stderr); err != nil {
		return err
	}
	if err := runCoverageGate(ctx, runner, repoRoot, env, coverageFile, coverageThreshold, stdout); err != nil {
		return err
	}
	if err := runRepoConsistencyChecks(repoRoot); err != nil {
		return err
	}

	return nil
}

func runOptionalVerification(
	ctx context.Context,
	runner commandRunner,
	opts VerifyOptions,
	env []string,
	stdout, stderr io.Writer,
	plan verifyPlan,
) error {
	if plan.runE2E {
		if err := runE2E(ctx, runner, opts.RepoRoot, env, stdout, stderr); err != nil {
			return err
		}
	}

	if plan.runE2EFull {
		if err := runE2EFull(ctx, runner, opts.RepoRoot, env, opts.E2ELogDir, stdout, stderr); err != nil {
			return err
		}
	}

	if plan.runIntegration {
		if err := runIntegration(ctx, runner, opts.RepoRoot, env, stdout, stderr); err != nil {
			return err
		}
	}

	return nil
}

func runFormat(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> gofumpt\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "gofumpt", "-w", "."); err != nil {
		return fmt.Errorf("format gofumpt: %w", err)
	}

	if err := writeStatus(stdout, "==> goimports\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"goimports",
		"-local",
		"github.com/tonimelisma/onedrive-go",
		"-w",
		".",
	); err != nil {
		return fmt.Errorf("format goimports: %w", err)
	}

	return nil
}

func runLint(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> golangci-lint\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "golangci-lint", "run", "--allow-parallel-runners"); err != nil {
		return fmt.Errorf("lint: %w", err)
	}

	return nil
}

func runBuild(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> go build\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", "build", "./..."); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	return nil
}

func runUnitTests(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	coverageFile string,
	stdout, stderr io.Writer,
) error {
	if err := writeStatus(stdout, "==> go test -race -coverprofile\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"go",
		"test",
		"-race",
		"-coverprofile="+coverageFile,
		"./...",
	); err != nil {
		return fmt.Errorf("unit tests: %w", err)
	}

	return nil
}

func runCoverageGate(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	coverageFile string,
	coverageThreshold float64,
	stdout io.Writer,
) error {
	if err := writeStatus(stdout, "==> coverage\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	report, err := runner.Output(ctx, repoRoot, env, "go", "tool", "cover", "-func="+coverageFile)
	if err != nil {
		return fmt.Errorf("coverage report: %w", err)
	}

	if writeErr := writeStatus(stdout, string(report)); writeErr != nil {
		return fmt.Errorf("write coverage report: %w", writeErr)
	}

	coverageTotal, err := parseCoverageTotal(string(report))
	if err != nil {
		return err
	}

	if coverageTotal < coverageThreshold {
		return fmt.Errorf("coverage gate failed: %.1f%% < %.1f%%", coverageTotal, coverageThreshold)
	}

	return nil
}

func parseCoverageTotal(report string) (float64, error) {
	for _, line := range strings.Split(report, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "total:" {
			continue
		}

		value := strings.TrimSuffix(fields[2], "%")
		coverageTotal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("parse coverage total %q: %w", value, err)
		}

		return coverageTotal, nil
	}

	return 0, fmt.Errorf("parse coverage total: total line not found")
}

func runE2E(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> go test -tags=e2e\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"go",
		"test",
		"-tags=e2e",
		"-race",
		"-v",
		"-parallel",
		"5",
		"-timeout=10m",
		"./e2e/...",
	); err != nil {
		return fmt.Errorf("fast e2e: %w", err)
	}

	return nil
}

func runE2EFull(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	logDir string,
	stdout, stderr io.Writer,
) error {
	if logDir == "" {
		logDir = filepath.Join(os.TempDir(), "e2e-debug-logs")
	}

	fullEnv := append([]string{}, env...)
	fullEnv = append(fullEnv, "E2E_LOG_DIR="+logDir)

	if err := writeStatus(stdout, "==> go test -tags='e2e e2e_full'\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		fullEnv,
		stdout,
		stderr,
		"go",
		"test",
		"-tags=e2e e2e_full",
		"-race",
		"-v",
		"-parallel",
		"5",
		"-timeout=30m",
		"./e2e/...",
	); err != nil {
		return fmt.Errorf("full e2e: %w", err)
	}

	return nil
}

func runIntegration(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> go test -tags=integration\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"go",
		"test",
		"-tags=integration",
		"-race",
		"-v",
		"-timeout=5m",
		"./internal/graph/...",
	); err != nil {
		return fmt.Errorf("integration tests: %w", err)
	}

	return nil
}

func runRepoConsistencyChecks(repoRoot string) error {
	for _, check := range []func(string) error{
		ensureNoStaleArchitecturePhrases,
		ensureGovernedDesignDocsHaveOwnershipContracts,
		ensureCrossCuttingDesignDocs,
		ensureCrossCuttingDesignDocEvidence,
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
		{name: "stale migration-bridge phrase", pattern: regexp.MustCompile("migration" + " bridge")},
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
		if _, err := localpath.Stat(check.path); err == nil {
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
		filepath.Join(repoRoot, "internal", "syncrecovery"),
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

func ensureGovernedDesignDocsHaveOwnershipContracts(repoRoot string) error {
	designDocs, err := filepath.Glob(filepath.Join(repoRoot, "spec", "design", "*.md"))
	if err != nil {
		return fmt.Errorf("glob design docs: %w", err)
	}

	for _, path := range designDocs {
		data, readErr := localpath.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}

		content := string(data)
		if !strings.Contains(content, "GOVERNS:") {
			continue
		}

		if !strings.Contains(content, "## Ownership Contract") {
			return fmt.Errorf("governed design doc missing Ownership Contract section: %s", path)
		}

		for _, bullet := range ownershipContractBullets() {
			if !strings.Contains(content, bullet) {
				return fmt.Errorf("governed design doc missing Ownership Contract bullet %q: %s", strings.TrimPrefix(bullet, "- "), path)
			}
		}
	}

	return nil
}

func ownershipContractBullets() []string {
	return []string{
		"- Owns:",
		"- Does Not Own:",
		"- Source of Truth:",
		"- Allowed Side Effects:",
		"- Mutable Runtime Owner:",
		"- Error Boundary:",
	}
}

func ensureCrossCuttingDesignDocs(repoRoot string) error {
	systemPath := filepath.Join(repoRoot, "spec", "design", "system.md")
	systemData, err := localpath.ReadFile(systemPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", systemPath, err)
	}

	systemText := string(systemData)
	for _, name := range requiredCrossCuttingDesignDocs() {
		path := filepath.Join(repoRoot, "spec", "design", name)
		if _, statErr := localpath.Stat(path); statErr != nil {
			return fmt.Errorf("required cross-cutting design doc missing: %s", path)
		}
		if !strings.Contains(systemText, name) {
			return fmt.Errorf("system.md missing required design doc reference %s: %s", name, systemPath)
		}
	}

	return nil
}

func requiredCrossCuttingDesignDocs() []string {
	return []string{"error-model.md", "threat-model.md", "degraded-mode.md"}
}

func ensureCrossCuttingDesignDocEvidence(repoRoot string) error {
	checks := []struct {
		path     string
		snippets []string
	}{
		{
			path: filepath.Join(repoRoot, "spec", "design", "error-model.md"),
			snippets: []string{
				"## Verified By",
				"| Boundary | Evidence |",
			},
		},
		{
			path: filepath.Join(repoRoot, "spec", "design", "degraded-mode.md"),
			snippets: []string{
				"| ID |",
				"| Evidence |",
			},
		},
		{
			path: filepath.Join(repoRoot, "spec", "design", "threat-model.md"),
			snippets: []string{
				"## Mitigation Evidence",
				"| Mitigation | Evidence |",
			},
		},
	}

	for _, check := range checks {
		data, err := localpath.ReadFile(check.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", check.path, err)
		}
		content := string(data)
		for _, snippet := range check.snippets {
			if !strings.Contains(content, snippet) {
				return fmt.Errorf("cross-cutting design doc missing required evidence snippet %q: %s", snippet, check.path)
			}
		}
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
}

func ensurePrivilegedPackageCallsStayAtApprovedBoundaries(repoRoot string) error {
	rules := []packageSelectorBoundaryRule{
		{
			importPath:  "os/exec",
			selector:    "CommandContext",
			description: "exec.CommandContext is only allowed in internal/cli/auth.go and internal/devtool/runner.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "cli", "auth.go") ||
					path == filepath.Join(repoRoot, "internal", "devtool", "runner.go")
			},
		},
		{
			importPath:  "database/sql",
			selector:    "Open",
			description: "sql.Open is only allowed in internal/syncstore production files",
			allowed: func(path string) bool {
				allowedDir := filepath.Join(repoRoot, "internal", "syncstore") + string(os.PathSeparator)
				return strings.HasPrefix(path, allowedDir)
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
	}

	for _, rule := range rules {
		if err := ensurePackageSelectorStaysAtApprovedBoundary(repoRoot, rule); err != nil {
			return err
		}
	}

	return nil
}

func ensurePackageSelectorStaysAtApprovedBoundary(repoRoot string, rule packageSelectorBoundaryRule) error {
	roots := []string{
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
	}

	for _, root := range roots {
		walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || rule.allowed(path) || strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
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
		})
		if walkErr == nil {
			continue
		}

		var matchErr matchFoundError
		if errors.As(walkErr, &matchErr) {
			return fmt.Errorf("%s: %s", rule.description, matchErr.value)
		}

		return fmt.Errorf("walk %s: %w", root, walkErr)
	}

	return nil
}

func findHTTPClientDoCall(path string) (string, error) {
	data, readErr := localpath.ReadFile(path)
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
	data, readErr := localpath.ReadFile(path)
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
	info, err := localpath.Stat(root)
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
	data, err := localpath.ReadFile(path)
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

func writeStatus(w io.Writer, text string) error {
	_, err := io.WriteString(w, text)
	if err != nil {
		return fmt.Errorf("write status output: %w", err)
	}

	return nil
}
