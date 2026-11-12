package devtool

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	specRequirementsDir = "spec/requirements"
	verifiedStatus      = "verified"
)

var (
	// requirementBulletPattern matches an acceptance criterion declaration.
	requirementBulletPattern = regexp.MustCompile(`^- (R-[0-9.]+):`)

	// requirementHeadingPattern matches a requirement group heading, which
	// declares the group id that acceptance criteria hang off.
	requirementHeadingPattern = regexp.MustCompile(`^#+\s*(R-[0-9.]+)\b`)

	// requirementStatusPattern captures the trailing bracketed status.
	requirementStatusPattern = regexp.MustCompile(`\[([a-z]+)\]\s*$`)

	validatesPattern     = regexp.MustCompile(`//\s*Validates:\s*(.+)`)
	requirementIDPattern = regexp.MustCompile(`R-[0-9.]+`)
)

type requirementRef struct {
	Location string
	ID       string
}

// requirementDeclarations is what the requirement files declare.
type requirementDeclarations struct {
	// declared holds every declared ID, group headings included.
	declared map[string]bool
	// statuses holds the bracketed status of each acceptance criterion.
	statuses map[string]string
	// definedAt lists the lines that declare each ID, in file and line order.
	definedAt map[string][]string
}

// runRequirementTraceability enforces the halves of requirement traceability
// that only tooling can hold up.
//
// Forward: a Validates or Implements reference must resolve to a declared
// requirement, so a renamed or deleted requirement cannot leave citations
// pointing at nothing.
//
// Reverse: a requirement marked verified must have evidence, either a test
// citing it or a design doc implementing it. A verified status with neither is
// a claim about behavior nobody demonstrated, which is the more dangerous
// direction because it reads as proof.
//
// Unique: each ID is declared by one requirement line. Two lines with one ID
// make every citation of it ambiguous, and the status of either can stand in
// for the other's.
func runRequirementTraceability(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	return runViolationCheck(stdout, "requirement traceability",
		"requirement traceability check failed",
		func() ([]checkViolation, error) { return findRequirementViolations(repoRoot) })
}

func findRequirementViolations(repoRoot string) ([]checkViolation, error) {
	declarations, err := readDeclaredRequirements(repoRoot)
	if err != nil {
		return nil, err
	}

	validates, err := collectValidatesRefs(repoRoot)
	if err != nil {
		return nil, err
	}

	implements, err := collectImplementsRefs(repoRoot)
	if err != nil {
		return nil, err
	}

	violations := duplicateRequirementDefinitions(declarations.definedAt)

	for _, ref := range append(append([]requirementRef{}, validates...), implements...) {
		if !requirementKnown(ref.ID, declarations.declared) {
			violations = append(violations, checkViolation{
				Location: ref.Location,
				Detail:   fmt.Sprintf("%s is not declared in %s", ref.ID, specRequirementsDir),
			})
		}
	}

	violations = append(violations, unevidencedVerifiedRequirements(declarations.statuses, validates, implements)...)

	return violations, nil
}

// duplicateRequirementDefinitions reports each ID that more than one
// requirement line declares, naming every one of those lines, in ID order.
func duplicateRequirementDefinitions(definedAt map[string][]string) []checkViolation {
	var duplicated []string
	for id, locations := range definedAt {
		if len(locations) > 1 {
			duplicated = append(duplicated, id)
		}
	}
	sort.Strings(duplicated)

	violations := make([]checkViolation, 0, len(duplicated))
	for _, id := range duplicated {
		violations = append(violations, checkViolation{
			Location: specRequirementsDir,
			Detail: fmt.Sprintf("%s is defined by more than one requirement line (%s); give each its own ID",
				id, strings.Join(definedAt[id], ", ")),
		})
	}

	return violations
}

// requirementKnown accepts a group id whenever any acceptance criterion hangs
// off it, because docs cite groups and tests cite individual criteria.
func requirementKnown(id string, declared map[string]bool) bool {
	if declared[id] {
		return true
	}

	prefix := id + "."
	for known := range declared {
		if strings.HasPrefix(known, prefix) {
			return true
		}
	}

	return false
}

// requirementCovered accepts evidence against the id itself or any group it
// belongs to, since a design doc that implements R-1.2 covers R-1.2.1.
func requirementCovered(id string, evidence map[string]bool) bool {
	if evidence[id] {
		return true
	}

	parts := strings.Split(id, ".")
	for i := 1; i < len(parts); i++ {
		if evidence[strings.Join(parts[:i], ".")] {
			return true
		}
	}

	return false
}

func unevidencedVerifiedRequirements(
	statuses map[string]string,
	validates []requirementRef,
	implements []requirementRef,
) []checkViolation {
	evidence := make(map[string]bool)
	for _, ref := range validates {
		evidence[ref.ID] = true
	}

	for _, ref := range implements {
		evidence[ref.ID] = true
	}

	var unevidenced []string

	for id, status := range statuses {
		if status == verifiedStatus && !requirementCovered(id, evidence) {
			unevidenced = append(unevidenced, id)
		}
	}

	sort.Strings(unevidenced)

	violations := make([]checkViolation, 0, len(unevidenced))
	for _, id := range unevidenced {
		violations = append(violations, checkViolation{
			Location: specRequirementsDir,
			Detail: fmt.Sprintf(
				"%s is marked [%s] but no test cites it and no design doc implements it",
				id, verifiedStatus),
		})
	}

	return violations
}

func readDeclaredRequirements(repoRoot string) (requirementDeclarations, error) {
	declarations := requirementDeclarations{
		declared:  make(map[string]bool),
		statuses:  make(map[string]string),
		definedAt: make(map[string][]string),
	}
	dir := filepath.Join(repoRoot, specRequirementsDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		// A tree with no requirements declares nothing. Any reference is then
		// reported as unresolved, which is the correct answer and keeps the
		// rule meaningful on trees that do not carry the spec.
		if errors.Is(err, os.ErrNotExist) {
			return declarations, nil
		}

		return requirementDeclarations{}, fmt.Errorf("read %s: %w", specRequirementsDir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, readErr := os.ReadFile(path) //nolint:gosec // repo-relative verifier input
		if readErr != nil {
			return requirementDeclarations{}, fmt.Errorf("read %s: %w", entry.Name(), readErr)
		}

		for lineNo, line := range strings.Split(string(data), "\n") {
			id, status, ok := requirementDeclaration(line)
			if !ok {
				continue
			}

			declarations.declared[id] = true
			declarations.definedAt[id] = append(declarations.definedAt[id],
				fmt.Sprintf("%s:%d", repoRelative(repoRoot, path), lineNo+1))
			if status != "" {
				declarations.statuses[id] = status
			}
		}
	}

	return declarations, nil
}

// requirementDeclaration reads the ID one line of a requirement file declares,
// a group heading's or an acceptance criterion's, and the criterion's trailing
// status. ok is false for a line that declares nothing.
func requirementDeclaration(line string) (id, status string, ok bool) {
	if m := requirementHeadingPattern.FindStringSubmatch(line); m != nil {
		return m[1], "", true
	}

	m := requirementBulletPattern.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	if s := requirementStatusPattern.FindStringSubmatch(strings.TrimRight(line, " \t")); s != nil {
		status = s[1]
	}

	return m[1], status, true
}

func collectValidatesRefs(repoRoot string) ([]requirementRef, error) {
	var refs []requirementRef

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if skipRequirementScanDir(d.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(d.Name(), ".go") || isRequirementRuleOwnFile(d.Name()) {
			return nil
		}

		fileRefs, scanErr := scanFileForValidatesRefs(repoRoot, path)
		if scanErr != nil {
			return scanErr
		}

		refs = append(refs, fileRefs...)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan for Validates references: %w", err)
	}

	return refs, nil
}

// isRequirementRuleOwnFile reports whether a file defines this rule. Its
// fixtures must cite requirement ids that deliberately do not exist, so
// scanning it would flag the rule itself.
func isRequirementRuleOwnFile(name string) bool {
	return name == "verify_requirements_test.go"
}

func skipRequirementScanDir(name string) bool {
	return name == verifyGitDir || name == verifyWorktreesDir || name == verifyNodeModulesDir
}

func scanFileForValidatesRefs(repoRoot string, path string) ([]requirementRef, error) {
	f, err := os.Open(path) //nolint:gosec // repo-relative verifier input
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only scan

	var refs []requirementRef

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, backlogScanInitialBufBytes), backlogScanMaxLineBytes)

	for lineNo := 1; scanner.Scan(); lineNo++ {
		m := validatesPattern.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}

		for _, id := range requirementIDPattern.FindAllString(m[1], -1) {
			refs = append(refs, requirementRef{
				Location: fmt.Sprintf("%s:%d", repoRelative(repoRoot, path), lineNo),
				ID:       id,
			})
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}

	return refs, nil
}

func collectImplementsRefs(repoRoot string) ([]requirementRef, error) {
	dir := filepath.Join(repoRoot, specDesignDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read %s: %w", specDesignDir, err)
	}

	var refs []requirementRef

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		data, readErr := os.ReadFile(path) //nolint:gosec // repo-relative verifier input
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), readErr)
		}

		for lineNo, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "Implements:") {
				continue
			}

			for _, id := range requirementIDPattern.FindAllString(line, -1) {
				refs = append(refs, requirementRef{
					Location: fmt.Sprintf("%s:%d", repoRelative(repoRoot, path), lineNo+1),
					ID:       id,
				})
			}
		}
	}

	return refs, nil
}
