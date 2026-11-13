package devtool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.2
//
// CLAUDE.md requires checking Close() on anything written to, because Close
// flushes buffers and a dropped error there is silent data loss. That rule is
// enforced only by errcheck, and errcheck once excluded every source line
// containing "defer " -- which switched the rule off entirely while the
// codebase appeared to comply.
//
// The exclusion is now scoped to test files. Nothing but this test stops the
// scope from being widened back, and the failure would be silent: production
// write-path closes would simply stop being reported.
func TestGolangciConfig_DeferErrcheckExclusionStaysScopedToTests(t *testing.T) {
	t.Parallel()

	lines := golangciConfigLines(t)

	found := 0

	for i, line := range lines {
		if !strings.Contains(line, `source:`) || !strings.Contains(line, "defer") {
			continue
		}

		found++

		// Walk back to the start of this exclusion entry and require that it
		// scopes itself to test files.
		scoped := false

		for j := i; j >= 0 && j > i-8; j-- {
			if strings.Contains(lines[j], `path: _test\.go`) {
				scoped = true

				break
			}
			if strings.Contains(lines[j], "- linters:") && j != i {
				break
			}
		}

		assert.Truef(t, scoped,
			"errcheck defer exclusion at .golangci.yml:%d is not scoped to _test.go; "+
				"widening it silently disables write-path Close() checking in production", i+1)
	}

	require.Positivef(t, found,
		"expected a scoped defer exclusion to exist; if it was removed entirely, delete this test deliberately")
}

// Validates: R-6.10.2
//
// depguard matches a rule's files patterns against each file's absolute path,
// anchored, and golangci-lint rewrites nothing in them but ${base-path} and
// ${config-path}. A pattern written relative to the repository, such as
// internal/graph/*.go, therefore never matches, and its rule is off without a
// word: every path-scoped rule here was, the leaf-package rule among them.
// Each pattern has to start with **/ or be a depguard variable such as $test.
func TestGolangciConfig_DepguardFilePatternsMatchAbsolutePaths(t *testing.T) {
	t.Parallel()

	patterns := depguardFilePatterns(golangciConfigLines(t))
	require.NotEmpty(t, patterns, "expected depguard rules with file patterns")

	for _, pattern := range patterns {
		bare := strings.TrimPrefix(pattern, "!")
		assert.Truef(t, strings.HasPrefix(bare, "**/") || strings.HasPrefix(bare, "$"),
			"depguard files pattern %q never matches: depguard matches absolute paths, so start it with **/", pattern)
	}
}

// Validates: R-6.10.2
//
// A depguard rule names its packages by folder. A package that is renamed or
// removed leaves its pattern matching nothing and its rule off for whatever
// took its place, without a word, so every **/<folder>/*.go pattern has to
// name a folder that holds a non-test Go file.
func TestGolangciConfig_DepguardPackagePatternsNameExistingPackages(t *testing.T) {
	t.Parallel()

	root := repoRootFromDevtoolPackage(t)
	checked := 0

	for _, pattern := range depguardFilePatterns(golangciConfigLines(t)) {
		// A negated pattern names its folder all the same.
		folder, isPackagePattern := strings.CutPrefix(strings.TrimPrefix(pattern, "!"), "**/")
		if !isPackagePattern || !strings.HasSuffix(folder, "/*.go") {
			continue
		}
		folder = strings.TrimSuffix(folder, "/*.go")
		checked++

		assert.Truef(t, holdsNonTestGoFile(t, filepath.Join(root, filepath.FromSlash(folder))),
			"depguard files pattern %q names %s, which holds no non-test Go file", pattern, folder)
	}

	require.Positive(t, checked, "expected depguard rules that name package folders")
}

// holdsNonTestGoFile reports whether dir exists and holds a Go file that is
// not a test.
func holdsNonTestGoFile(t *testing.T, dir string) bool {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)

	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			return true
		}
	}

	return false
}

// depguardFilePatterns returns every entry of a files list inside the
// depguard settings block of the config's lines.
func depguardFilePatterns(lines []string) []string {
	var patterns []string

	depguardIndent, filesIndent := -1, -1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if depguardIndent < 0 {
			if trimmed == "depguard:" {
				depguardIndent = indent
			}

			continue
		}
		if indent <= depguardIndent {
			break
		}

		if filesIndent >= 0 && indent > filesIndent && strings.HasPrefix(trimmed, "- ") {
			patterns = append(patterns, strings.Trim(strings.TrimPrefix(trimmed, "- "), `"'`))

			continue
		}

		filesIndent = -1
		if trimmed == "files:" {
			filesIndent = indent
		}
	}

	return patterns
}

func golangciConfigLines(t *testing.T) []string {
	t.Helper()

	root := repoRootFromDevtoolPackage(t)
	//nolint:gosec // reads a fixed repo-relative config path.
	raw, err := os.ReadFile(filepath.Join(root, ".golangci.yml"))
	require.NoError(t, err)

	return strings.Split(string(raw), "\n")
}

func repoRootFromDevtoolPackage(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	return filepath.Dir(filepath.Dir(wd))
}
