package devtool

import (
	"fmt"
	"io"
	"strings"
)

// checkViolation is one finding from a repo-invariant check: where it is, and
// what is wrong there.
type checkViolation struct {
	Location string
	Detail   string
}

// runViolationCheck is the shape every repo-invariant check shares: announce
// the step, collect findings, and report all of them at once. Reporting them
// together matters more than it looks -- these checks run over the whole tree,
// and failing on the first finding would turn one fix-and-rerun cycle into as
// many cycles as there are violations.
func runViolationCheck(
	stdout io.Writer,
	label string,
	failureSummary string,
	find func() ([]checkViolation, error),
) error {
	if err := writeStatus(stdout, "==> "+label+"\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	violations, err := find()
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

	return fmt.Errorf("%s:\n%s", failureSummary, b.String())
}
