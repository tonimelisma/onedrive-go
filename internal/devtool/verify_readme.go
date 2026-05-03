package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func runReadmeStatus(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	if err := writeStatus(stdout, "==> README status\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	violations, err := findReadmeStatusViolations(repoRoot)
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}

	return fmt.Errorf("README status check failed: remove stale roadmap/status phrase(s): %s", strings.Join(violations, ", "))
}

func findReadmeStatusViolations(repoRoot string) ([]string, error) {
	readmePath := filepath.Join(repoRoot, "README.md")
	data, err := readFile(readmePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("README status check failed: README.md is missing")
		}

		return nil, err
	}

	statusSection, err := readmeStatusSection(string(data))
	if err != nil {
		return nil, err
	}

	content := strings.ToLower(statusSection)
	var violations []string
	for _, phrase := range staleReadmeStatusPhrases() {
		if strings.Contains(content, strings.ToLower(phrase)) {
			violations = append(violations, phrase)
		}
	}

	return violations, nil
}

func readmeStatusSection(content string) (string, error) {
	var section strings.Builder
	inStatus := false
	for _, line := range strings.Split(content, "\n") {
		heading := strings.TrimSpace(line)
		if heading == "## Status" {
			inStatus = true
			continue
		}
		if inStatus && strings.HasPrefix(heading, "## ") {
			return section.String(), nil
		}
		if inStatus {
			section.WriteString(line)
			section.WriteByte('\n')
		}
	}
	if !inStatus {
		return "", fmt.Errorf("README status check failed: README.md is missing a ## Status section")
	}

	return section.String(), nil
}

func staleReadmeStatusPhrases() []string {
	return []string{
		"Phase ",
		"Phases ",
		"complete. Working CLI",
		"rewrite in progress",
	}
}
