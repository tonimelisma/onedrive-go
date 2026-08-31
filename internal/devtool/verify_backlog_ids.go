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

// retiredBacklogRegistryPath is the historical-provenance registry for the
// dissolved BACKLOG.md. Every B-NNN identifier still cited in Go code must be
// listed there, and every listed identifier must still be cited, so the
// registry can neither miss a new pointer nor rot into a dead list.
const retiredBacklogRegistryPath = "spec/reference/retired-backlog-ids.md"

// Scan lines can be long in generated or table-formatted Go files, so the
// scanner gets a larger-than-default ceiling before it reports an error.
const (
	backlogScanInitialBufBytes = 64 * 1024
	backlogScanMaxLineBytes    = 1024 * 1024
)

var (
	// backlogIDPattern matches the canonical comment form, e.g. "(B-207)".
	backlogIDPattern = regexp.MustCompile(`\bB-\d{3}\b`)

	// backlogIdentifierPattern matches the identifier form that hyphens cannot
	// survive, where the id is embedded in a Go name between underscores.
	backlogIdentifierPattern = regexp.MustCompile(`_B\d{3}_`)

	// registryIDPattern matches a backticked ID in the registry table.
	registryIDPattern = regexp.MustCompile("`(B-\\d{3})`")
)

type backlogIDViolation struct {
	Location string
	Detail   string
}

func runRetiredBacklogIDs(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	if err := writeStatus(stdout, "==> retired backlog ids\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	violations, err := findBacklogIDViolations(repoRoot)
	if err != nil {
		return err
	}

	if len(violations) == 0 {
		return nil
	}

	var b strings.Builder
	for _, v := range violations {
		fmt.Fprintf(&b, "  %s: %s\n", v.Location, v.Detail)
	}

	return fmt.Errorf(
		"retired backlog id check failed (see %s):\n%s",
		retiredBacklogRegistryPath, b.String())
}

func findBacklogIDViolations(repoRoot string) ([]backlogIDViolation, error) {
	registered, err := readRetiredBacklogRegistry(repoRoot)
	if err != nil {
		return nil, err
	}

	cited, violations, err := collectBacklogIDCitations(repoRoot, registered)
	if err != nil {
		return nil, err
	}

	deadEntries := make([]string, 0, len(registered))
	for id := range registered {
		if !cited[id] {
			deadEntries = append(deadEntries, id)
		}
	}
	sort.Strings(deadEntries)

	for _, id := range deadEntries {
		violations = append(violations, backlogIDViolation{
			Location: retiredBacklogRegistryPath,
			Detail: fmt.Sprintf(
				"%s is registered but no longer cited in Go code; remove the registry row",
				id),
		})
	}

	return violations, nil
}

func readRetiredBacklogRegistry(repoRoot string) (map[string]bool, error) {
	path := filepath.Join(repoRoot, retiredBacklogRegistryPath)

	data, err := os.ReadFile(path) //nolint:gosec // repo-relative verifier input
	if err != nil {
		// A repo with no registry simply has nothing registered. Any B-NNN
		// citation then reports as unregistered, which is the correct answer
		// and keeps the rule meaningful on trees that predate the registry.
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}

		return nil, fmt.Errorf("read retired backlog registry: %w", err)
	}

	registered := make(map[string]bool)
	for _, m := range registryIDPattern.FindAllStringSubmatch(string(data), -1) {
		registered[m[1]] = true
	}

	return registered, nil
}

func collectBacklogIDCitations(
	repoRoot string,
	registered map[string]bool,
) (map[string]bool, []backlogIDViolation, error) {
	cited := make(map[string]bool)

	var violations []backlogIDViolation

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if skipBacklogScanDir(d.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(d.Name(), ".go") || isBacklogRuleOwnFile(d.Name()) {
			return nil
		}

		fileViolations, err := scanFileForBacklogIDs(repoRoot, path, registered, cited)
		if err != nil {
			return err
		}

		violations = append(violations, fileViolations...)

		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("scan for backlog ids: %w", err)
	}

	return cited, violations, nil
}

// isBacklogRuleOwnFile reports whether a file defines this rule. The rule's
// implementation and tests must contain example identifiers to describe and
// exercise what they reject, so scanning them would flag the rule itself.
func isBacklogRuleOwnFile(name string) bool {
	return name == "verify_backlog_ids.go" || name == "verify_backlog_ids_test.go"
}

func skipBacklogScanDir(name string) bool {
	return name == verifyGitDir || name == verifyWorktreesDir || name == verifyNodeModulesDir
}

func scanFileForBacklogIDs(
	repoRoot string,
	path string,
	registered map[string]bool,
	cited map[string]bool,
) ([]backlogIDViolation, error) {
	f, err := os.Open(path) //nolint:gosec // repo-relative verifier input
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only scan

	var violations []backlogIDViolation

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, backlogScanInitialBufBytes), backlogScanMaxLineBytes)

	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		location := fmt.Sprintf("%s:%d", repoRelative(repoRoot, path), lineNo)

		for _, id := range backlogIDPattern.FindAllString(line, -1) {
			cited[id] = true

			if !registered[id] {
				violations = append(violations, backlogIDViolation{
					Location: location,
					Detail: fmt.Sprintf(
						"%s is not listed in the retired backlog registry; "+
							"do not introduce new backlog identifiers", id),
				})
			}
		}

		for _, ident := range backlogIdentifierPattern.FindAllString(line, -1) {
			violations = append(violations, backlogIDViolation{
				Location: location,
				Detail: fmt.Sprintf(
					"identifier embeds retired backlog id %q; name it for the behavior instead",
					strings.Trim(ident, "_")),
			})
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}

	return violations, nil
}
