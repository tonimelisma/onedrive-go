package devtool

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// coverageRequirementFile and coverageRequirementID name the one line that
	// states the coverage gate's number. devtool reads the number there, the
	// way the lint check reads its pin from the workflow, so no second copy of
	// it can drift from the requirement: devtool and CI once gated on two
	// different values.
	coverageRequirementFile = "non-functional.md"
	coverageRequirementID   = "R-6.10.4"

	maxCoveragePercent = 100.0
)

// coveragePercentPattern matches a percentage such as `83.5%` in prose.
var coveragePercentPattern = regexp.MustCompile(`(\d+(?:\.\d+)?)%`)

// requiredCoverage reads the percentage requirement R-6.10.4 states. The line
// must state exactly one percentage, so an edit that adds a second number
// fails here instead of letting the gate pick one.
func requiredCoverage(repoRoot string) (float64, error) {
	relPath := filepath.Join(specRequirementsDir, coverageRequirementFile)

	data, err := readFile(filepath.Join(repoRoot, relPath))
	if err != nil {
		return 0, fmt.Errorf("read coverage requirement: %w", err)
	}

	prefix := "- " + coverageRequirementID + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		matches := coveragePercentPattern.FindAllStringSubmatch(line, -1)
		if len(matches) != 1 {
			return 0, fmt.Errorf(
				"%s in %s must state exactly one percentage, found %d",
				coverageRequirementID, relPath, len(matches))
		}

		required, parseErr := strconv.ParseFloat(matches[0][1], 64)
		if parseErr != nil {
			return 0, fmt.Errorf("parse %s percentage %q: %w", coverageRequirementID, matches[0][1], parseErr)
		}
		if required <= 0 || required > maxCoveragePercent {
			return 0, fmt.Errorf("%s states %.1f%%, out of range", coverageRequirementID, required)
		}

		return required, nil
	}

	return 0, fmt.Errorf("%s does not state %s", relPath, coverageRequirementID)
}

// resolveCoverageThreshold returns the threshold the coverage gate enforces.
// Zero, the flag's default, means the number R-6.10.4 states; a positive value
// overrides it; a negative one reports coverage without gating. Only a profile
// that runs the gate reads the requirement, and it reads it before any test
// runs, so an unreadable requirement fails in seconds, not after the race tier.
func resolveCoverageThreshold(plan verifyPlan, repoRoot string, explicit float64) (float64, error) {
	if !plan.runPublicChecks || explicit != 0 {
		return explicit, nil
	}

	return requiredCoverage(repoRoot)
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

	// A negative threshold reports coverage without gating on it. The gate is
	// a single authoritative number owned by one platform; secondary
	// platforms compile a different file set (build-tagged sources) and would
	// legitimately land on a different percentage.
	if coverageThreshold < 0 {
		return writeStatus(stdout, fmt.Sprintf("coverage gate disabled (total %.1f%%)\n", coverageTotal))
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
