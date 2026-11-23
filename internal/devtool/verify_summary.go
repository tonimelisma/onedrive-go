package devtool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
)

// verifyStepFailedPrefix starts the one line a failed step prints on stderr
// after its own output: the prefix, the profile, one space and the step's
// name, as in `verify step failed: public lint`. The line holds nothing
// else, no path, command line, output of the step or duration, because the
// nightly reader keys a failed job by the steps these lines name
// (nightlyFailedStepKeys) and the public issue carries the keys. The printer
// and the reader share the prefix and the form (failedStepMarker,
// readFailedStepMarker).
const verifyStepFailedPrefix = "verify step failed: "

// failedStepMarker is the line a failed step of profile prints.
func failedStepMarker(profile string, step verifyStepName) string {
	return verifyStepFailedPrefix + profile + " " + string(step) + "\n"
}

// readFailedStepMarker reads one line of text as a failed step's marker and
// returns the step it names: the prefix at the start of the line, a verify
// profile, one space and a step of the closed set, and nothing else. Any other
// line is none, an indented one or a test's output that quotes a marker among
// them, so what a reader takes from a log is a name of the set and never text
// the log's other lines put there.
func readFailedStepMarker(line string) (verifyStepName, bool) {
	rest, isMarker := strings.CutPrefix(line, verifyStepFailedPrefix)
	if !isMarker {
		return "", false
	}
	profile, step, hasStep := strings.Cut(rest, " ")
	if !hasStep {
		return "", false
	}
	if _, err := resolveVerifyPlan(VerifyProfile(profile)); err != nil {
		return "", false
	}
	if !slices.Contains(verifyStepNames(), verifyStepName(step)) {
		return "", false
	}

	return verifyStepName(step), true
}

// overBudgetMarkerPrefix starts the one line a stress step prints on stderr
// for each member whose slowest run was over the budget of one run: the
// prefix, the member's package relative to the module (`./internal/sync`),
// one space and its top-level test, as in `verify member over budget:
// ./internal/multisync TestWatchSoak`. The nightly reader keys the member by
// it whatever else failed (nightlyOverBudgetKeys), and the public issue
// carries the key, so the line holds nothing else. The printer and the reader
// share the prefix and the form (overBudgetMarker, readOverBudgetMarker).
const overBudgetMarkerPrefix = "verify member over budget: "

// overBudgetMarker is the line a stress step prints for a member of pkg, as
// normalizeFailedGoTestPackage gives it, whose top-level test is test.
func overBudgetMarker(pkg string, test string) string {
	return overBudgetMarkerPrefix + pkg + " " + test + "\n"
}

// readOverBudgetMarker reads one line of text as a budget line and returns
// the member it names: the prefix at the start of the line, a package of the
// module (`.` or `./` and a path of letters, digits, `_`, `-`, `.` and `/`),
// one space and a top-level test name (Test and letters, digits and `_`),
// and nothing else. Any other line is none, an indented one or a test's
// output that quotes a budget line among them.
func readOverBudgetMarker(line string) (goTestFailure, bool) {
	rest, isMarker := strings.CutPrefix(line, overBudgetMarkerPrefix)
	if !isMarker {
		return goTestFailure{}, false
	}
	pkg, test, hasTest := strings.Cut(rest, " ")
	if !hasTest || !isModulePackageName(pkg) || !isTopLevelTestName(test) {
		return goTestFailure{}, false
	}

	return goTestFailure{pkg: pkg, test: test}, true
}

// isModulePackageName reports whether pkg is a package of the module as
// normalizeFailedGoTestPackage writes it: `.`, or `./` and a clean path.
func isModulePackageName(pkg string) bool {
	if pkg == "." {
		return true
	}
	rel, isRelative := strings.CutPrefix(pkg, "./")
	if !isRelative || rel == "" || path.Clean(rel) != rel || strings.HasPrefix(rel, "..") {
		return false
	}
	for _, r := range rel {
		if !isNameRune(r) && r != '-' && r != '.' && r != '/' {
			return false
		}
	}

	return true
}

// isTopLevelTestName reports whether name is a Test function's name.
func isTopLevelTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	for _, r := range name {
		if !isNameRune(r) {
			return false
		}
	}

	return true
}

func isNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_'
}

func newVerifySummaryCollector(
	profile VerifyProfile, stdout, stderr io.Writer, summaryJSONPath string, e2eLogDir string,
) *verifySummaryCollector {
	return &verifySummaryCollector{
		summary: VerifySummary{
			Profile: string(profile),
		},
		stdout:          stdout,
		stderr:          stderr,
		summaryJSONPath: summaryJSONPath,
		e2eLogDir:       e2eLogDir,
		startedAt:       time.Now(),
	}
}

// keepStepLogs makes the collector keep the output of every go test step in
// logs, and returns the writers the profile hands its steps in place of
// stdout and stderr. The collector's own lines, the marker lines and the
// summary, go to the unteed writers it was built with.
func (c *verifySummaryCollector) keepStepLogs(logs *stepLogs, stdout, stderr io.Writer) (io.Writer, io.Writer) {
	c.stepLogs = logs

	return logs.tee(stdout), logs.tee(stderr)
}

// stepOutcome is what a step reports beside its error.
type stepOutcome struct {
	slowTests []StressSlowTestSummary
	// failedTests names the step's failed top-level tests; it is recorded
	// only when the step fails.
	failedTests []string
	// skipped means the step's tests skipped, so it verified nothing.
	skipped bool
	// logPath is the file that kept the output of a failed go test step.
	logPath string
}

// run is the one runner of every step a profile runs, a bucket of the full
// live suite included: it runs fn and times it, and when fn fails it prints
// the step's marker line on stderr, after everything the step printed. A step
// that passes or skips prints none. A step that runs go test also writes what
// it prints to its log file (stepLogs), which a failed step keeps and names
// on the line after its marker. runStepWithOutcome and runBucket record what
// it returns in the summary.
func (c *verifySummaryCollector) run(name verifyStepName, fn func() (stepOutcome, error)) (stepOutcome, int64, error) {
	startedAt := time.Now()
	outcome, logPath, err := c.runLogged(name, fn)
	elapsedMS := durationMS(time.Since(startedAt))
	if err == nil {
		return outcome, elapsedMS, nil
	}
	if writeErr := writeStatus(c.stderr, failedStepMarker(c.summary.Profile, name)); writeErr != nil {
		err = errors.Join(err, fmt.Errorf("write failed step marker: %w", writeErr))
	}
	if logPath != "" {
		outcome.logPath = logPath
		if writeErr := writeStatus(c.stderr, stepLogLine(logPath)); writeErr != nil {
			err = errors.Join(err, fmt.Errorf("write step log line: %w", writeErr))
		}
	}

	return outcome, elapsedMS, err
}

// runLogged runs fn with the step's log open when the step runs go test and
// the collector keeps step logs. It returns the path of a log it kept.
func (c *verifySummaryCollector) runLogged(
	name verifyStepName, fn func() (stepOutcome, error),
) (stepOutcome, string, error) {
	if c.stepLogs == nil || !name.runsGoTest() {
		outcome, err := fn()
		return outcome, "", err
	}

	logPath, err := c.stepLogs.open(name)
	if err != nil {
		return stepOutcome{}, "", err
	}
	outcome, err := fn()
	kept, logErr := c.stepLogs.finish(logPath, err)
	err = errors.Join(err, logErr)
	if !kept {
		logPath = ""
	}

	return outcome, logPath, err
}

func (c *verifySummaryCollector) runStep(name verifyStepName, fn func() error) error {
	return c.runStepWithOutcome(name, func() (stepOutcome, error) {
		return stepOutcome{}, fn()
	})
}

// runStepWithOutcome runs one step through run and records it in the
// summary's steps.
func (c *verifySummaryCollector) runStepWithOutcome(name verifyStepName, fn func() (stepOutcome, error)) error {
	outcome, elapsedMS, err := c.run(name, fn)

	step := VerifyStepSummary{
		Name:       string(name),
		Status:     verifySummaryStatusPass,
		DurationMS: elapsedMS,
		SlowTests:  outcome.slowTests,
	}
	switch {
	case err != nil:
		step.Status = verifySummaryStatusFail
		step.Error = err.Error()
		step.FailedTests = outcome.failedTests
		step.LogPath = outcome.logPath
	case outcome.skipped:
		step.Status = verifySummaryStatusSkipped
	}

	c.summary.Steps = append(c.summary.Steps, step)
	return err
}

// recordStepNotRun records a step the profile did not start, and why.
func (c *verifySummaryCollector) recordStepNotRun(name verifyStepName, reason string) {
	c.summary.Steps = append(c.summary.Steps, VerifyStepSummary{
		Name:   string(name),
		Status: verifySummaryStatusNotRun,
		Error:  reason,
	})
}

// runBucket runs one full-suite bucket through run and records it in the
// summary's buckets; fn returns the bucket's failed top-level tests with its
// error.
func (c *verifySummaryCollector) runBucket(bucket fullE2EBucketSpec, fn func() ([]string, error)) error {
	outcome, elapsedMS, err := c.run(bucket.Name, func() (stepOutcome, error) {
		failedTests, runErr := fn()
		return stepOutcome{failedTests: failedTests}, runErr
	})

	summary := newE2EBucketSummary(bucket)
	summary.Status = verifySummaryStatusPass
	summary.DurationMS = elapsedMS
	if err != nil {
		summary.Status = verifySummaryStatusFail
		summary.Error = err.Error()
		summary.FailedTests = outcome.failedTests
		summary.LogPath = outcome.logPath
	}

	c.summary.E2EFullBuckets = append(c.summary.E2EFullBuckets, summary)
	return err
}

// recordBucketsNotRun records buckets the profile did not start, and why, so
// the summary lists every bucket and "did not run" never reads as absent.
func (c *verifySummaryCollector) recordBucketsNotRun(buckets []fullE2EBucketSpec, reason string) {
	for _, bucket := range buckets {
		summary := newE2EBucketSummary(bucket)
		summary.Status = verifySummaryStatusNotRun
		summary.Error = reason
		c.summary.E2EFullBuckets = append(c.summary.E2EFullBuckets, summary)
	}
}

func newE2EBucketSummary(bucket fullE2EBucketSpec) E2EBucketSummary {
	return E2EBucketSummary{
		Name:       string(bucket.Name),
		Kind:       string(bucket.Kind),
		RunPattern: fullE2EBucketRunPattern(bucket.TestNames),
		Parallel:   bucket.Parallel,
		Timeout:    bucket.Timeout,
	}
}

func (c *verifySummaryCollector) recordClassifiedRerun(
	incidentID string,
	phase string,
	trigger string,
	rerunArgs []string,
	duration time.Duration,
	status string,
) {
	commandParts := append([]string{"go"}, rerunArgs...)
	c.summary.ClassifiedReruns = append(c.summary.ClassifiedReruns, ClassifiedRerunSummary{
		IncidentID:   incidentID,
		Phase:        phase,
		Trigger:      trigger,
		RerunCommand: strings.Join(commandParts, " "),
		DurationMS:   durationMS(duration),
		Status:       status,
	})
}

func (c *verifySummaryCollector) finalize(runErr error) error {
	if c.e2eLogDir != "" {
		quirkEventCount, err := readE2EQuirkEventCount(c.e2eLogDir)
		if err != nil {
			return err
		}
		c.summary.QuirkEventCount = quirkEventCount
	}

	c.summary.TotalDurationMS = durationMS(time.Since(c.startedAt))
	c.summary.Status = verifySummaryStatusPass
	if runErr != nil {
		c.summary.Status = verifySummaryStatusFail
	}

	if err := writeStatus(c.stdout, c.renderText()); err != nil {
		return fmt.Errorf("write verify summary: %w", err)
	}

	if c.summaryJSONPath == "" {
		return nil
	}

	data, err := json.MarshalIndent(c.summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal verify summary: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWrite(
		c.summaryJSONPath,
		data,
		".verify-summary-*.tmp",
	); err != nil {
		return fmt.Errorf("write verify summary json: %w", err)
	}

	return nil
}

func (c *verifySummaryCollector) renderText() string {
	var builder strings.Builder
	builder.WriteString("==> verify summary\n")
	fmt.Fprintf(&builder, "status: %s\n", c.summary.Status)
	fmt.Fprintf(&builder, "total: %s\n", formatDurationMS(c.summary.TotalDurationMS))

	for _, step := range c.summary.Steps {
		builder.WriteString(renderSummaryLine(step.Name, step.Status, step.DurationMS, step.Error))
		builder.WriteString(renderSlowTestLines(step.SlowTests))
		builder.WriteString(renderFailedTestLines(step.FailedTests))
		builder.WriteString(renderStepLogLine(step.LogPath))
	}
	for i := range c.summary.E2EFullBuckets {
		bucket := &c.summary.E2EFullBuckets[i]
		errorText := bucket.Error
		if bucket.Parallel > 0 {
			if errorText == "" {
				errorText = fmt.Sprintf("parallel=%d", bucket.Parallel)
			} else {
				errorText = fmt.Sprintf("%s; parallel=%d", errorText, bucket.Parallel)
			}
		}
		builder.WriteString(renderSummaryLine(bucket.Name, bucket.Status, bucket.DurationMS, errorText))
		builder.WriteString(renderFailedTestLines(bucket.FailedTests))
		builder.WriteString(renderStepLogLine(bucket.LogPath))
	}
	fmt.Fprintf(&builder, "quirk events: %d\n", c.summary.QuirkEventCount)

	if len(c.summary.ClassifiedReruns) == 0 {
		builder.WriteString("classified reruns: none\n")
		return builder.String()
	}

	builder.WriteString("classified reruns:\n")
	for _, rerun := range c.summary.ClassifiedReruns {
		fmt.Fprintf(
			&builder,
			"- %s %s %s (%s)\n",
			rerun.IncidentID,
			rerun.Phase,
			rerun.Status,
			formatDurationMS(rerun.DurationMS),
		)
	}

	return builder.String()
}

func renderSummaryLine(name string, status string, durationMS int64, errorText string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s: %s (%s)", name, status, formatDurationMS(durationMS))
	if errorText != "" {
		fmt.Fprintf(&builder, " [%s]", errorText)
	}
	builder.WriteByte('\n')
	return builder.String()
}

func renderSlowTestLines(slowTests []StressSlowTestSummary) string {
	if len(slowTests) == 0 {
		return ""
	}

	var builder strings.Builder
	for _, slow := range slowTests {
		fmt.Fprintf(
			&builder,
			"  slow test: %s runs=%d total=%s max=%s\n",
			slow.name(),
			slow.Runs,
			formatDurationMS(slow.TotalElapsedMS),
			formatDurationMS(slow.MaxElapsedMS),
		)
	}

	return builder.String()
}

func renderFailedTestLines(failedTests []string) string {
	var builder strings.Builder
	for _, test := range failedTests {
		fmt.Fprintf(&builder, "  failed test: %s\n", test)
	}

	return builder.String()
}

// renderStepLogLine is the summary's line naming a failed step's kept log,
// none for a step that kept no log.
func renderStepLogLine(logPath string) string {
	if logPath == "" {
		return ""
	}

	return "  log: " + logPath + "\n"
}

func durationMS(d time.Duration) int64 {
	return d.Milliseconds()
}

func formatDurationMS(durationMS int64) string {
	return (time.Duration(durationMS) * time.Millisecond).String()
}
