package devtool

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// mutexDeclPattern matches a mutex declaration, whether it is a struct field
// or a local variable, under any import alias for the sync package.
var mutexDeclPattern = regexp.MustCompile(`\b(?:[A-Za-z_][A-Za-z0-9_]*\.)?(?:RW)?Mutex\b`)

// runMutexDocs enforces that every mutex says what it guards.
//
// A mutex protects data, not code, and which data is never inferable from the
// declaration: field order suggests an answer that is often wrong. The one
// this rule was written for guarded a single slice while sitting directly
// above two fields it did not guard.
func runMutexDocs(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	return runViolationCheck(stdout, "mutex ownership docs",
		"mutex ownership doc check failed; state what each mutex guards",
		func() ([]checkViolation, error) { return findMutexDocViolations(repoRoot) })
}

func findMutexDocViolations(repoRoot string) ([]checkViolation, error) {
	var violations []checkViolation

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if skipMutexScanDir(d.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		if isMutexRuleOwnFile(d.Name()) {
			return nil
		}

		fileViolations, scanErr := scanFileForUndocumentedMutexes(repoRoot, path)
		if scanErr != nil {
			return scanErr
		}

		violations = append(violations, fileViolations...)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan for mutex docs: %w", err)
	}

	return violations, nil
}

// isMutexRuleOwnFile reports whether a file defines this rule. It necessarily
// names the type it looks for, so scanning it would flag the rule itself.
func isMutexRuleOwnFile(name string) bool {
	return name == "verify_mutex_docs.go"
}

func skipMutexScanDir(name string) bool {
	return name == verifyGitDir || name == verifyWorktreesDir || name == verifyNodeModulesDir
}

func scanFileForUndocumentedMutexes(repoRoot string, path string) ([]checkViolation, error) {
	f, err := os.Open(path) //nolint:gosec // repo-relative verifier input
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only scan

	var (
		violations []checkViolation
		previous   string
	)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, backlogScanInitialBufBytes), backlogScanMaxLineBytes)

	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if isMutexDeclaration(trimmed) && !mutexIsDocumented(line, previous) {
			violations = append(violations, checkViolation{
				Location: fmt.Sprintf("%s:%d", repoRelative(repoRoot, path), lineNo),
				Detail:   fmt.Sprintf("%q does not say what it guards", trimmed),
			})
		}

		previous = trimmed
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}

	return violations, nil
}

// isMutexDeclaration reports whether the line declares a mutex rather than
// using one. Method calls, type definitions, and embedded uses inside larger
// expressions are not declarations.
func isMutexDeclaration(trimmed string) bool {
	if trimmed == "" || strings.HasPrefix(trimmed, "//") {
		return false
	}

	if !mutexDeclPattern.MatchString(trimmed) {
		return false
	}

	if strings.Contains(trimmed, "func ") || strings.Contains(trimmed, "type ") {
		return false
	}

	// Uses such as mu.Lock() or a composite literal are not declarations.
	if strings.Contains(trimmed, "(") || strings.Contains(trimmed, "{") {
		return false
	}

	fields := strings.Fields(strings.TrimPrefix(trimmed, "var "))

	return len(fields) >= 2
}

func mutexIsDocumented(line string, previous string) bool {
	if idx := strings.Index(line, "//"); idx >= 0 {
		return strings.TrimSpace(line[idx+2:]) != ""
	}

	return strings.HasPrefix(previous, "//")
}
