package devtool

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func newVerifySummaryCollector(profile VerifyProfile, stdout io.Writer, summaryJSONPath string, e2eLogDir string) *verifySummaryCollector {
	return &verifySummaryCollector{
		summary: VerifySummary{
			Profile: string(profile),
		},
		stdout:          stdout,
		summaryJSONPath: summaryJSONPath,
		e2eLogDir:       e2eLogDir,
		startedAt:       time.Now(),
	}
}

// stepOutcome is what a step reports beside its error.
type stepOutcome struct {
	slowTests []StressSlowTestSummary
	// failedTests names the step's failed top-level tests; it is recorded
	// only when the step fails.
	failedTests []string
	// skipped means the step's tests skipped, so it verified nothing.
	skipped bool
}

func (c *verifySummaryCollector) runStep(name string, fn func() error) error {
	return c.runStepWithOutcome(name, func() (stepOutcome, error) {
		return stepOutcome{}, fn()
	})
}

func (c *verifySummaryCollector) runStepWithOutcome(name string, fn func() (stepOutcome, error)) error {
	startedAt := time.Now()
	outcome, err := fn()

	step := VerifyStepSummary{
		Name:       name,
		Status:     verifySummaryStatusPass,
		DurationMS: durationMS(time.Since(startedAt)),
		SlowTests:  outcome.slowTests,
	}
	switch {
	case err != nil:
		step.Status = verifySummaryStatusFail
		step.Error = err.Error()
		step.FailedTests = outcome.failedTests
	case outcome.skipped:
		step.Status = verifySummaryStatusSkipped
	}

	c.summary.Steps = append(c.summary.Steps, step)
	return err
}

// recordStepNotRun records a step the profile did not start, and why.
func (c *verifySummaryCollector) recordStepNotRun(name string, reason string) {
	c.summary.Steps = append(c.summary.Steps, VerifyStepSummary{
		Name:   name,
		Status: verifySummaryStatusNotRun,
		Error:  reason,
	})
}

// runBucket runs one full-suite bucket; fn returns the bucket's failed
// top-level tests with its error.
func (c *verifySummaryCollector) runBucket(bucket fullE2EBucketSpec, fn func() ([]string, error)) error {
	startedAt := time.Now()
	failedTests, err := fn()

	summary := newE2EBucketSummary(bucket)
	summary.Status = verifySummaryStatusPass
	summary.DurationMS = durationMS(time.Since(startedAt))
	if err != nil {
		summary.Status = verifySummaryStatusFail
		summary.Error = err.Error()
		summary.FailedTests = failedTests
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
		Name:       bucket.Name,
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
		verifySummaryFilePerm,
		verifySummaryDirPerm,
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

func durationMS(d time.Duration) int64 {
	return d.Milliseconds()
}

func formatDurationMS(durationMS int64) string {
	return (time.Duration(durationMS) * time.Millisecond).String()
}
