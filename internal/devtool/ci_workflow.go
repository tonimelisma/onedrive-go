package devtool

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The events of the CI workflow that devtool asks about: a DoD wait follows
// a pull request's run before merge and the push run of the merge commit
// after it.
const (
	ciEventPullRequest = "pull_request"
	ciEventPush        = "push"

	ciJobsKey              = "jobs:"
	ciJobIndent            = "  "
	ciJobKeyIndent         = "    "
	ciJobIfKey             = "if"
	ciJobTimeoutKey        = "timeout-minutes"
	ciJobConcurrencyKey    = "concurrency"
	ciConcurrencyGroupKey  = "group"
	ciConcurrencyCancelKey = "cancel-in-progress"
	ciExpressionPrefix     = "${{"
	ciExpressionSuffix     = "}}"
	ciAlwaysCondition      = "always()"
	ciEventNameOperand     = "github.event_name"
	ciVariablePrefix       = "vars."
)

var (
	// ciEventTimeoutPattern is the one expression a job's timeout-minutes may
	// be: one value for one event and another for every other event.
	ciEventTimeoutPattern = regexp.MustCompile(
		`^\$\{\{ github\.event_name == '([a-z_]+)' && ([0-9]+) \|\| ([0-9]+) \}\}$`)

	// ciComparisonPattern is one comparison of a job condition: an operand,
	// == or !=, and a quoted literal.
	ciComparisonPattern = regexp.MustCompile(`^([a-zA-Z_.]+) (==|!=) '([^']*)'$`)

	// ciConcurrencyGroupPattern is a plain concurrency group name.
	ciConcurrencyGroupPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// ciWorkflowJob is one job of the CI workflow: its name and the lines of its
// body as the file has them, indentation included.
type ciWorkflowJob struct {
	name  string
	lines []string
}

// readCIWorkflowJobs reads the jobs of the repository's CI workflow.
func readCIWorkflowJobs(repoRoot string) ([]ciWorkflowJob, error) {
	data, err := readFile(filepath.Join(repoRoot, ciWorkflowPath))
	if err != nil {
		return nil, fmt.Errorf("read CI workflow: %w", err)
	}

	return ciWorkflowJobs(string(data)), nil
}

// ciWorkflowJobs splits the workflow's top-level `jobs:` map into its jobs,
// in file order. A job is a key indented by exactly two spaces under it; a
// comment between jobs belongs to neither.
func ciWorkflowJobs(workflow string) []ciWorkflowJob {
	var jobs []ciWorkflowJob
	inJobs := false
	for line := range strings.Lines(workflow) {
		line = strings.TrimRight(line, "\r\n")
		if line == ciJobsKey {
			inJobs = true
			continue
		}
		if !inJobs || strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			// The next top-level key ends the jobs map.
			inJobs = false
			continue
		}
		if key, ok := strings.CutPrefix(line, ciJobIndent); ok && key[0] != ' ' {
			if key[0] != '#' {
				jobs = append(jobs, ciWorkflowJob{name: strings.TrimSuffix(strings.TrimSpace(key), ":")})
			}
			continue
		}
		if len(jobs) > 0 {
			jobs[len(jobs)-1].lines = append(jobs[len(jobs)-1].lines, line)
		}
	}

	return jobs
}

// key returns the value of one of the job's own keys, the ones indented four
// spaces, and whether the job sets it. A block scalar (`|` or `>`) is refused:
// the keys read here are one-line expressions.
func (j ciWorkflowJob) key(name string) (string, bool, error) {
	prefix := ciJobKeyIndent + name + ":"
	for _, line := range j.lines {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		value := strings.TrimSpace(stripYAMLComment(rest))
		if strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") {
			return "", true, fmt.Errorf("%s: job %s: %s: a block scalar is not read; write the value on one line", ciWorkflowPath, j.name, name)
		}

		return value, true, nil
	}

	return "", false, nil
}

// stripYAMLComment drops a trailing ` # comment` from a plain scalar. The
// values read here hold no quoted `#`.
func stripYAMLComment(value string) string {
	if i := strings.Index(value, " #"); i >= 0 {
		return value[:i]
	}

	return value
}

// runsOn reports whether the job's `if:` lets it run for event. A job with
// no condition runs for every event.
//
// The condition is evaluated in the forms the workflow uses and no others:
// comparisons joined by && and ||, && binding tighter. A comparison is
// `always()`, `github.event_name == 'x'` or `!= 'x'`, or a comparison of a
// repository variable, which counts as true: the variables gate the live jobs
// on whether credentials are configured, and assuming they are only ever
// makes a window longer. Anything else, parentheses, negation or another
// function, fails loudly, so a new condition cannot silently drop a job.
func (j ciWorkflowJob) runsOn(event string) (bool, error) {
	condition, set, err := j.key(ciJobIfKey)
	if err != nil {
		return false, err
	}
	if !set {
		return true, nil
	}
	condition = unwrapCIExpression(condition)

	for _, conjunction := range strings.Split(condition, " || ") {
		holds := true
		for _, term := range strings.Split(conjunction, " && ") {
			value, termErr := evaluateCITerm(strings.TrimSpace(term), event)
			if termErr != nil {
				return false, fmt.Errorf("%s: job %s: if: %q: %w", ciWorkflowPath, j.name, condition, termErr)
			}
			holds = holds && value
		}
		if holds {
			return true, nil
		}
	}

	return false, nil
}

func unwrapCIExpression(value string) string {
	inner, ok := strings.CutPrefix(value, ciExpressionPrefix)
	if !ok {
		return value
	}

	return strings.TrimSpace(strings.TrimSuffix(inner, ciExpressionSuffix))
}

func evaluateCITerm(term string, event string) (bool, error) {
	if term == ciAlwaysCondition {
		return true, nil
	}
	m := ciComparisonPattern.FindStringSubmatch(term)
	if m == nil {
		return false, fmt.Errorf("term %q is not always(), an event comparison or a variable comparison", term)
	}
	operand, operator, literal := m[1], m[2], m[3]
	switch {
	case operand == ciEventNameOperand:
		return (event == literal) == (operator == "=="), nil
	case strings.HasPrefix(operand, ciVariablePrefix):
		return true, nil
	default:
		return false, fmt.Errorf("term %q compares %s, which is not read", term, operand)
	}
}

// jobTimeouts is a job's timeout-minutes: forEvent for event, otherwise for
// every other event. A plain number leaves event empty and sets both.
type jobTimeouts struct {
	event     string
	forEvent  time.Duration
	otherwise time.Duration
}

// timeouts reads the job's timeout-minutes. The value is a number, or
// exactly `${{ github.event_name == 'E' && N || M }}`: N minutes for event E
// and M for every other. Any other expression fails loudly, and so does a
// zero N, which GitHub's `&& ||` would silently turn into M.
func (j ciWorkflowJob) timeouts() (jobTimeouts, error) {
	value, set, err := j.key(ciJobTimeoutKey)
	if err != nil {
		return jobTimeouts{}, err
	}
	if !set {
		return jobTimeouts{}, fmt.Errorf("%s: job %s sets no timeout-minutes (GitHub's default is six hours)", ciWorkflowPath, j.name)
	}
	unreadable := fmt.Errorf("%s: job %s: timeout-minutes %q is not a number of minutes above zero or "+
		"${{ github.event_name == '<event>' && <minutes> || <minutes> }}", ciWorkflowPath, j.name, value)
	if !strings.HasPrefix(value, ciExpressionPrefix) {
		minutes, ok := positiveMinutes(value)
		if !ok {
			return jobTimeouts{}, unreadable
		}

		return jobTimeouts{forEvent: minutes, otherwise: minutes}, nil
	}

	const eventTimeoutGroups = 4
	m := ciEventTimeoutPattern.FindStringSubmatch(value)
	if len(m) != eventTimeoutGroups {
		return jobTimeouts{}, unreadable
	}
	forEvent, forEventOK := positiveMinutes(m[2])
	otherwise, otherwiseOK := positiveMinutes(m[3])
	if !forEventOK || !otherwiseOK {
		return jobTimeouts{}, unreadable
	}

	return jobTimeouts{event: m[1], forEvent: forEvent, otherwise: otherwise}, nil
}

// timeoutOn returns the job's timeout-minutes for event.
func (j ciWorkflowJob) timeoutOn(event string) (time.Duration, error) {
	timeouts, err := j.timeouts()
	if err != nil {
		return 0, err
	}
	if timeouts.event != "" && event == timeouts.event {
		return timeouts.forEvent, nil
	}

	return timeouts.otherwise, nil
}

// longestTimeout returns the job's longest timeout-minutes on any event.
func (j ciWorkflowJob) longestTimeout() (time.Duration, error) {
	timeouts, err := j.timeouts()
	if err != nil {
		return 0, err
	}

	return max(timeouts.forEvent, timeouts.otherwise), nil
}

// concurrencyGroup returns the job's own concurrency group, and whether it
// has one. The forms read are the ones the workflow uses: `concurrency:
// <name>`, and a map of `group: <name>` with an optional `cancel-in-progress:
// true|false`, the name plain. A group from an expression, a quoted name or
// any other key fails loudly, because the stall window could not tell which
// jobs share the group.
func (j ciWorkflowJob) concurrencyGroup() (string, bool, error) {
	value, set, err := j.key(ciJobConcurrencyKey)
	if err != nil || !set {
		return "", false, err
	}
	unreadable := func(what string) error {
		return fmt.Errorf("%s: job %s: concurrency: %s; write `concurrency: <name>` or a map of "+
			"`group: <name>` and `cancel-in-progress: true|false`", ciWorkflowPath, j.name, what)
	}

	group := value
	if value == "" {
		for _, entry := range j.blockEntries(ciJobConcurrencyKey) {
			key, entryValue, ok := strings.Cut(entry, ": ")
			switch {
			case ok && key == ciConcurrencyGroupKey:
				group = strings.TrimSpace(entryValue)
			case ok && key == ciConcurrencyCancelKey && (entryValue == "true" || entryValue == "false"):
			default:
				return "", false, unreadable(fmt.Sprintf("entry %q is not read", entry))
			}
		}
	}
	if !ciConcurrencyGroupPattern.MatchString(group) {
		return "", false, unreadable(fmt.Sprintf("group %q is not a plain name", group))
	}

	return group, true, nil
}

// blockEntries returns the lines of the map one of the job's own keys opens,
// trimmed and with their comments dropped: the lines indented six spaces
// below it.
func (j ciWorkflowJob) blockEntries(name string) []string {
	header := ciJobKeyIndent + name + ":"
	var entries []string
	in := false
	for _, line := range j.lines {
		if strings.TrimRight(stripYAMLComment(line), " ") == header {
			in = true
			continue
		}
		if !in {
			continue
		}
		trimmed := strings.TrimSpace(stripYAMLComment(line))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, ciJobKeyIndent+"  ") {
			break
		}
		entries = append(entries, trimmed)
	}

	return entries
}

// positiveMinutes reads a whole number of minutes above zero.
func positiveMinutes(value string) (time.Duration, bool) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, false
	}

	return time.Duration(n) * time.Minute, true
}
