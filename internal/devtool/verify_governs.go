package devtool

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The design docs under spec/design carry two pieces of navigational metadata
// that the rest of the repo's documentation model rests on: a GOVERNS: line
// naming the files a doc owns, and inline `TestXxx` citations that back a
// requirement's status. Neither is compiler-checked, so both drift silently as
// files are added, renamed, and removed. This check makes them fail loudly.
const (
	specDesignDir   = "spec/design"
	governsPrefix   = "GOVERNS:"
	governsRootDoc  = "spec/requirements/index.md"
	routingTableDoc = "CLAUDE.md"
)

// governedRoots returns the directories whose production Go files must each be
// owned by exactly one design doc.
func governedRoots() [3]string {
	return [3]string{"internal", "pkg", "cmd"}
}

// citedTestPattern matches a backticked Go test identifier in design-doc prose,
// which is how requirement evidence is recorded.
var citedTestPattern = regexp.MustCompile("`((?:Test|Benchmark|Fuzz)[A-Za-z0-9_]*)`")

type governsViolation struct {
	Path    string
	Message string
}

// governsClaim is one design doc's claim on one file, carrying the specificity
// of the pattern that produced it.
type governsClaim struct {
	doc         string
	specificity int
}

// patternSpecificity scores how narrowly a GOVERNS pattern targets files, so a
// doc can own a subset of a directory another doc owns broadly. Docs are
// written as "cli.md owns internal/cli/*.go" plus "drive-identity.md owns
// internal/cli/drive*.go", and the narrower claim is the intended owner.
//
// The score is the number of literal (non-wildcard) characters. Two claims of
// equal specificity are a genuine ambiguity and are reported.
func patternSpecificity(pattern string) int {
	score := 0

	for _, r := range pattern {
		if r != '*' && r != '?' && r != '[' && r != ']' {
			score++
		}
	}

	return score
}

func runSpecGoverns(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	if err := writeStatus(stdout, "==> spec governs\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	violations, err := findGovernsViolations(repoRoot)
	if err != nil {
		return err
	}

	if len(violations) == 0 {
		return nil
	}

	return fmt.Errorf("spec governs check failed:\n%s", formatGovernsViolations(violations))
}

func findGovernsViolations(repoRoot string) ([]governsViolation, error) {
	ownership, patternViolations, err := readGovernsOwnership(repoRoot)
	if err != nil {
		return nil, err
	}

	violations := patternViolations

	productionFiles, err := collectProductionGoFiles(repoRoot)
	if err != nil {
		return nil, err
	}

	for _, rel := range productionFiles {
		owners := resolveOwners(ownership[rel])
		switch len(owners) {
		case 0:
			violations = append(violations, governsViolation{
				Path:    rel,
				Message: "no design doc GOVERNS this file; add it to the owning doc's GOVERNS: line",
			})
		case 1:
		default:
			violations = append(violations, governsViolation{
				Path: rel,
				Message: "governed by multiple design docs at equal pattern specificity (" +
					strings.Join(owners, ", ") + "); exactly one doc must own it",
			})
		}
	}

	citationViolations, err := findMissingCitedTests(repoRoot)
	if err != nil {
		return nil, err
	}

	violations = append(violations, citationViolations...)

	routingViolations, err := findDeadRoutingTablePaths(repoRoot)
	if err != nil {
		return nil, err
	}

	violations = append(violations, routingViolations...)

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path != violations[j].Path {
			return violations[i].Path < violations[j].Path
		}

		return violations[i].Message < violations[j].Message
	})

	return violations, nil
}

// resolveOwners reduces the claims on one file to the most specific ones. A
// single winner is correct ownership; more than one means two docs claim the
// file just as narrowly, which no precedence rule can resolve for them.
func resolveOwners(claims []governsClaim) []string {
	best := -1
	for _, claim := range claims {
		if claim.specificity > best {
			best = claim.specificity
		}
	}

	seen := make(map[string]bool)

	var owners []string

	for _, claim := range claims {
		if claim.specificity != best || seen[claim.doc] {
			continue
		}

		seen[claim.doc] = true

		owners = append(owners, claim.doc)
	}

	sort.Strings(owners)

	return owners
}

// readGovernsOwnership maps each governed repo-relative path to the design docs
// claiming it, and reports patterns that match nothing. A pattern matching
// nothing is always a defect: either the file moved and the doc was not
// updated, or the pattern was mistyped.
func readGovernsOwnership(repoRoot string) (map[string][]governsClaim, []governsViolation, error) {
	docs, err := filepath.Glob(filepath.Join(repoRoot, specDesignDir, "*.md"))
	if err != nil {
		return nil, nil, fmt.Errorf("list design docs: %w", err)
	}

	sort.Strings(docs)

	ownership := make(map[string][]governsClaim)

	var violations []governsViolation

	for _, doc := range docs {
		docRel := repoRelative(repoRoot, doc)

		patterns, readErr := readGovernsPatterns(doc)
		if readErr != nil {
			return nil, nil, readErr
		}

		for _, pattern := range patterns {
			matches, matchErr := filepath.Glob(filepath.Join(repoRoot, filepath.FromSlash(pattern)))
			if matchErr != nil {
				return nil, nil, fmt.Errorf("expand GOVERNS pattern %q in %s: %w", pattern, docRel, matchErr)
			}

			if len(matches) == 0 {
				violations = append(violations, governsViolation{
					Path:    docRel,
					Message: fmt.Sprintf("GOVERNS pattern %q matches no file", pattern),
				})

				continue
			}

			specificity := patternSpecificity(pattern)
			for _, match := range matches {
				rel := repoRelative(repoRoot, match)
				ownership[rel] = append(ownership[rel], governsClaim{doc: docRel, specificity: specificity})
			}
		}
	}

	return ownership, violations, nil
}

// readGovernsPatterns extracts the comma-separated patterns from a doc's
// GOVERNS: line. Entries containing a parenthesis are prose references such as
// "README.md (Representative Performance section)" and are not path patterns.
func readGovernsPatterns(docPath string) ([]string, error) {
	data, err := readFile(docPath)
	if err != nil {
		return nil, fmt.Errorf("read design doc %s: %w", docPath, err)
	}

	for line := range strings.Lines(string(data)) {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, governsPrefix) {
			continue
		}

		var patterns []string

		for _, raw := range strings.Split(strings.TrimPrefix(trimmed, governsPrefix), ",") {
			pattern := strings.TrimSpace(raw)
			if pattern == "" || strings.Contains(pattern, "(") {
				continue
			}

			patterns = append(patterns, pattern)
		}

		return patterns, nil
	}

	return nil, nil
}

// collectProductionGoFiles lists the non-test Go files that must be governed.
func collectProductionGoFiles(repoRoot string) ([]string, error) {
	var files []string

	for _, root := range governedRoots() {
		rootPath := filepath.Join(repoRoot, root)
		if _, statErr := stat(rootPath); errors.Is(statErr, os.ErrNotExist) {
			// Not every repo layout has all three roots; absence is not a defect.
			continue
		}

		walkErr := filepath.WalkDir(rootPath, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("walk %s: %w", path, err)
			}

			if entry.IsDir() {
				if shouldSkipTestConventionDir(entry.Name()) {
					return filepath.SkipDir
				}

				return nil
			}

			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}

			files = append(files, repoRelative(repoRoot, path))

			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk governed root %s: %w", root, walkErr)
		}
	}

	sort.Strings(files)

	return files, nil
}

// findMissingCitedTests reports design-doc test citations with no matching test
// function. A requirement marked verified on the strength of a test that no
// longer exists is a false claim, not a stale comment.
func findMissingCitedTests(repoRoot string) ([]governsViolation, error) {
	existing, err := collectTestFunctionNames(repoRoot)
	if err != nil {
		return nil, err
	}

	docs, err := filepath.Glob(filepath.Join(repoRoot, specDesignDir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("list design docs: %w", err)
	}

	sort.Strings(docs)

	var violations []governsViolation

	for _, doc := range docs {
		data, readErr := readFile(doc)
		if readErr != nil {
			return nil, fmt.Errorf("read design doc %s: %w", doc, readErr)
		}

		seen := make(map[string]bool)

		for _, match := range citedTestPattern.FindAllStringSubmatch(string(data), -1) {
			name := match[1]
			if seen[name] || existing[name] {
				continue
			}

			seen[name] = true

			violations = append(violations, governsViolation{
				Path:    repoRelative(repoRoot, doc),
				Message: fmt.Sprintf("cites test %s, which does not exist", name),
			})
		}
	}

	return violations, nil
}

// routingPathPattern matches the backticked file and directory references in
// CLAUDE.md's routing table.
var routingPathPattern = regexp.MustCompile("`([A-Za-z0-9_./*-]+\\.(?:go|md)|[A-Za-z0-9_/*-]+/)`")

// findDeadRoutingTablePaths reports routing-table entries that point at
// nothing. CLAUDE.md is the first thing a contributor or agent reads to decide
// which design doc governs the code they are about to touch, so a reference
// that no longer resolves sends them to the wrong place — or nowhere.
func findDeadRoutingTablePaths(repoRoot string) ([]governsViolation, error) {
	routingDoc := filepath.Join(repoRoot, routingTableDoc)

	data, err := readFile(routingDoc)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read %s: %w", routingTableDoc, err)
	}

	seen := make(map[string]bool)

	var violations []governsViolation

	for _, match := range routingPathPattern.FindAllStringSubmatch(string(data), -1) {
		reference := match[1]
		if seen[reference] {
			continue
		}

		seen[reference] = true

		matches, globErr := filepath.Glob(filepath.Join(repoRoot, filepath.FromSlash(reference)))
		if globErr != nil {
			return nil, fmt.Errorf("expand routing reference %q: %w", reference, globErr)
		}

		if len(matches) == 0 {
			violations = append(violations, governsViolation{
				Path:    routingTableDoc,
				Message: fmt.Sprintf("routing table references %q, which matches no file", reference),
			})
		}
	}

	return violations, nil
}

func collectTestFunctionNames(repoRoot string) (map[string]bool, error) {
	names := make(map[string]bool)
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

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", repoRelative(repoRoot, path), parseErr)
		}

		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Recv != nil {
				continue
			}

			names[funcDecl.Name.Name] = true
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk test sources: %w", err)
	}

	return names, nil
}

func repoRelative(repoRoot string, path string) string {
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return filepath.ToSlash(path)
	}

	return filepath.ToSlash(rel)
}

func formatGovernsViolations(violations []governsViolation) string {
	var builder strings.Builder

	for i, violation := range violations {
		if i > 0 {
			builder.WriteByte('\n')
		}

		_, _ = fmt.Fprintf(&builder, "%s: %s", violation.Path, violation.Message)
	}

	return builder.String()
}
