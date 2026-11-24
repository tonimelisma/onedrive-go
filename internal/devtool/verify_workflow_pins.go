package devtool

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// workflowUsesPattern matches a step's `uses:` line, with or without the
	// list dash, and captures its indent and the action reference.
	workflowUsesPattern = regexp.MustCompile(`^(\s*)(?:-\s+)?uses:\s*(\S+)(.*)$`)

	// pinnedActionPattern is an action pinned to a full commit SHA with the
	// release it names as a trailing comment, the form Dependabot keeps up to
	// date.
	pinnedActionPattern  = regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	pinnedCommentPattern = regexp.MustCompile(`^\s+#\s+v\d+(?:\.\d+){0,2}\s*$`)
)

// checkoutAction is the action whose default leaves the workflow token in the
// checkout's git configuration for every later step.
const checkoutAction = "actions/checkout"

// workflowPinFindings returns the steps of one workflow file that run a
// third-party action by a movable reference, or check out the repository
// leaving the job's token in its git configuration. A tag can be moved to
// other code after review, and a persisted token is readable by every later
// step, the test code of a pull request included; the live jobs run that code
// with the test accounts' credentials. Local actions (`./...`) and container
// images are not GitHub action references and are not checked.
func workflowPinFindings(file string, content string) []string {
	lines := strings.Split(content, "\n")
	var findings []string
	for i, line := range lines {
		m := workflowUsesPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := m[2]
		if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") {
			continue
		}
		where := fmt.Sprintf("%s:%d", file, i+1)
		if !pinnedActionPattern.MatchString(ref) || !pinnedCommentPattern.MatchString(m[3]) {
			findings = append(findings, where+": "+ref+" is not pinned to a commit SHA with a `# vX.Y.Z` comment")
		}
		action, _, _ := strings.Cut(ref, "@")
		if action == checkoutAction && !stepSetsPersistCredentialsFalse(lines[i+1:], stepDashColumn(line, len(m[1]))) {
			findings = append(findings, where+": "+checkoutAction+" does not set persist-credentials: false")
		}
	}

	return findings
}

// stepDashColumn is the column of the list dash that opens the step a
// `uses:` line belongs to: the line's own dash, or two columns left of a
// `uses:` key that follows the step's first key.
func stepDashColumn(line string, indent int) int {
	if strings.HasPrefix(strings.TrimSpace(line), "-") {
		return indent
	}

	return indent - len("- ")
}

// stepSetsPersistCredentialsFalse reads the lines after a step's `uses:` line
// up to the next step or the end of the steps, and reports whether they set
// persist-credentials: false. dashColumn is the column of the dash that opens
// the step; every line of the step is indented past it.
func stepSetsPersistCredentialsFalse(rest []string, dashColumn int) bool {
	for _, line := range rest {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= dashColumn {
			return false
		}
		if trimmed == "persist-credentials: false" {
			return true
		}
	}

	return false
}
