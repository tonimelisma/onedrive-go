package devtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const ciWorkflowPath = ".github/workflows/ci.yml"

var (
	// pinnedLintVersionPattern matches the version the CI workflow installs.
	pinnedLintVersionPattern = regexp.MustCompile(`golangci-lint-action@[^\n]*\n(?:.*\n)??\s*version:\s*v?(\d+\.\d+\.\d+)`)

	// installedLintVersionPattern matches `golangci-lint has version X.Y.Z ...`.
	installedLintVersionPattern = regexp.MustCompile(`golangci-lint has version v?(\d+\.\d+\.\d+)`)
)

// assertLintVersionMatchesCI fails when the installed golangci-lint is not the
// version CI pins.
//
// A different version is a different lint policy, not a cosmetic difference.
// Local verification then passes on rules CI does not have, or misses rules it
// does, and the disagreement only surfaces after a push. The pin is read from
// the workflow so the two cannot drift apart through a second copy of the
// number.
func assertLintVersionMatchesCI(
	ctx context.Context, runner commandRunner, repoRoot string,
) error {
	pinned, err := pinnedLintVersion(repoRoot)
	if err != nil {
		return err
	}

	// No workflow means no pin to disagree with. A workflow that exists but
	// does not pin is different, and pinnedLintVersion reports that: an
	// unpinned CI lint floats, which is the drift this check exists to stop.
	if pinned == "" {
		return nil
	}

	out, runErr := runner.CombinedOutput(ctx, repoRoot, nil, "golangci-lint", "--version")
	if runErr != nil {
		return fmt.Errorf("read golangci-lint version: %w", runErr)
	}

	m := installedLintVersionPattern.FindStringSubmatch(string(out))
	if m == nil {
		return fmt.Errorf("read golangci-lint version: unrecognized output %q", strings.TrimSpace(string(out)))
	}

	if m[1] != pinned {
		return fmt.Errorf(
			"golangci-lint %s is installed but %s pins v%s; "+
				"a different version is a different lint policy, so install the pinned one:\n"+
				"  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v%s",
			m[1], ciWorkflowPath, pinned, pinned)
	}

	return nil
}

func pinnedLintVersion(repoRoot string) (string, error) {
	path := filepath.Join(repoRoot, ciWorkflowPath)

	data, err := os.ReadFile(path) //nolint:gosec // repo-relative verifier input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("read %s: %w", ciWorkflowPath, err)
	}

	m := pinnedLintVersionPattern.FindStringSubmatch(string(data))
	if m == nil {
		return "", fmt.Errorf("%s does not pin a golangci-lint version", ciWorkflowPath)
	}

	return m[1], nil
}
