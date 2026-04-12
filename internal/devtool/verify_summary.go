package devtool

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
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

func (c *verifySummaryCollector) runStep(name string, fn func() error) error {
	startedAt := time.Now()
	err := fn()

	step := VerifyStepSummary{
		Name:       name,
		Status:     verifySummaryStatusPass,
		DurationMS: durationMS(time.Since(startedAt)),
	}
	if err != nil {
		step.Status = verifySummaryStatusFail
		step.Error = err.Error()
	}

	c.summary.Steps = append(c.summary.Steps, step)
	return err
}

func (c *verifySummaryCollector) runBucket(bucket fullE2EBucketSpec, fn func() error) error {
	startedAt := time.Now()
	err := fn()

	summary := E2EBucketSummary{
		Name:       bucket.Name,
		Kind:       string(bucket.Kind),
		RunPattern: fullE2EBucketRunPattern(bucket.TestNames),
		Parallel:   bucket.Parallel,
		Timeout:    bucket.Timeout,
		Status:     verifySummaryStatusPass,
		DurationMS: durationMS(time.Since(startedAt)),
	}
	if err != nil {
		summary.Status = verifySummaryStatusFail
		summary.Error = err.Error()
	}

	c.summary.E2EFullBuckets = append(c.summary.E2EFullBuckets, summary)
	return err
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
	if err := localpath.AtomicWrite(
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
	}
	for _, bucket := range c.summary.E2EFullBuckets {
		errorText := bucket.Error
		if bucket.Parallel > 0 {
			if errorText == "" {
				errorText = fmt.Sprintf("parallel=%d", bucket.Parallel)
			} else {
				errorText = fmt.Sprintf("%s; parallel=%d", errorText, bucket.Parallel)
			}
		}
		builder.WriteString(renderSummaryLine(bucket.Name, bucket.Status, bucket.DurationMS, errorText))
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

func durationMS(d time.Duration) int64 {
	return d.Milliseconds()
}

func formatDurationMS(durationMS int64) string {
	return (time.Duration(durationMS) * time.Millisecond).String()
}
