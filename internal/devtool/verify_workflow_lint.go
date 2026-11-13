package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// actionlintModule is the pinned actionlint the lint profile runs with
	// `go run`, so neither CI nor a developer installs anything: the go
	// command fetches the module once, checks it against the checksum
	// database and caches the build.
	actionlintModule = "github.com/rhysd/actionlint/cmd/actionlint@v1.7.12"

	workflowDir = ".github/workflows"

	// workflowLintTimeout bounds one lint, the first run's module download
	// and build included, which take under a minute.
	workflowLintTimeout = 5 * time.Minute
)

// actionlintArgs are the flags of every workflow lint. shellcheck and pyflakes
// are off: neither is installed on a developer host or assumed in CI, and
// actionlint uses them whenever they are on PATH, so a runner that happened to
// have one would lint by another policy. -oneline prints each finding on one
// `<file>:<line>:<column>: <message> [<rule>]` line.
func actionlintArgs() []string {
	return []string{"run", actionlintModule, "-shellcheck=", "-pyflakes=", "-oneline"}
}

// actionlintFindingPattern matches one -oneline finding.
var actionlintFindingPattern = regexp.MustCompile(`^\S+:\d+:\d+: .+$`)

// runWorkflowLint lints every GitHub Actions workflow file with the pinned
// actionlint. No Go test reads a workflow as GitHub does, so an invalid one
// (a `run:` written as a plain scalar that holds ": ", say) passed every
// other step and failed only when GitHub refused to run it. The error names
// each finding's file and line.
func runWorkflowLint(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	files, err := workflowFiles(repoRoot)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		// A tree without workflows has nothing GitHub could refuse.
		return writeStatus(stdout, "==> actionlint: no workflow files under "+workflowDir+"\n")
	}
	if err := writeStatus(stdout, "==> actionlint\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	lintCtx, cancel := context.WithTimeout(ctx, workflowLintTimeout)
	defer cancel()

	args := append(actionlintArgs(), files...)
	output, runErr := runGoTeed(lintCtx, runner, repoRoot, env, stdout, stderr, "actionlint", args...)
	if runErr == nil {
		return nil
	}
	findings := actionlintFindings(output, files)
	if len(findings) == 0 {
		return runErr
	}

	return errors.Join(fmt.Errorf("workflow lint: %s", strings.Join(findings, "; ")), runErr)
}

// workflowFiles lists the workflow files, repository-relative and in name
// order. A tree without the directory has none.
func workflowFiles(repoRoot string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(repoRoot, workflowDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow files: %w", err)
	}

	var files []string
	for _, entry := range entries {
		switch filepath.Ext(entry.Name()) {
		case ".yml", ".yaml":
			if entry.Type().IsRegular() {
				files = append(files, filepath.Join(workflowDir, entry.Name()))
			}
		}
	}

	return files, nil
}

// actionlintFindings returns the finding lines of actionlint's -oneline
// output: a line of the finding's shape that names one of the files the lint
// was given, as actionlint prints a finding. A Go compile error has the same
// shape, so a failed build of actionlint itself would otherwise read as
// findings in the workflows.
func actionlintFindings(output string, files []string) []string {
	var findings []string
	for line := range strings.Lines(output) {
		line = strings.TrimRight(line, "\r\n")
		if !actionlintFindingPattern.MatchString(line) {
			continue
		}
		for _, file := range files {
			if strings.HasPrefix(line, file+":") {
				findings = append(findings, line)
				break
			}
		}
	}

	return findings
}
