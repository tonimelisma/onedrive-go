package devtool

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// principleLabelPattern matches a principle label such as "P-4". The program
// that once numbered principles that way left no definition of them; every
// such rule is a requirement, which is what the code and the docs cite.
var principleLabelPattern = regexp.MustCompile(`\bP-\d+\b`)

// findPrincipleLabelViolations reports every principle label in the Go files
// and in the Markdown, each to be replaced by the
// requirement ID it stands for.
func findPrincipleLabelViolations(repoRoot string) ([]checkViolation, error) {
	var violations []checkViolation
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := repoRelative(repoRoot, path)
		if d.IsDir() {
			if skipRepositoryRuleDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if d.Name() == "verify_principle_labels.go" || d.Name() == "verify_principle_labels_test.go" {
			return nil
		}
		fileViolations, err := scanFileForPrincipleLabels(rel, path)
		if err != nil {
			return err
		}
		violations = append(violations, fileViolations...)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan for principle labels: %w", err)
	}

	return violations, nil
}

func scanFileForPrincipleLabels(rel string, path string) ([]checkViolation, error) {
	f, err := os.Open(path) //nolint:gosec // repo-relative verifier input
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only scan

	var violations []checkViolation
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, backlogScanInitialBufBytes), backlogScanMaxLineBytes)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		for _, label := range principleLabelPattern.FindAllString(scanner.Text(), -1) {
			violations = append(violations, checkViolation{
				Location: fmt.Sprintf("%s:%d", rel, lineNo),
				Detail:   label + " is a principle label nothing defines; cite the requirement it stands for",
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}

	return violations, nil
}
