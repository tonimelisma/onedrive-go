package devtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	ShortcutComplianceFormatMarkdown = "markdown"
	ShortcutComplianceFormatJSON     = "json"
)

type ShortcutComplianceOptions struct {
	RepoRoot string
	Format   string
	Stdout   io.Writer
}

type ShortcutComplianceReport struct {
	Status     string                        `json:"status"`
	Invariants []ShortcutComplianceInvariant `json:"invariants"`
	Findings   []string                      `json:"findings,omitempty"`
}

type ShortcutComplianceInvariant struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence"`
}

func BuildShortcutComplianceReport(repoRoot string) ShortcutComplianceReport {
	invariants := shortcutComplianceInvariants()
	status := verifySummaryStatusPass
	var findings []string
	if err := RunShortcutArchitectureChecks(repoRoot); err != nil {
		status = verifySummaryStatusFail
		findings = splitShortcutComplianceFindings(err.Error())
		for i := range invariants {
			invariants[i].Status = verifySummaryStatusFail
		}
	}
	return ShortcutComplianceReport{
		Status:     status,
		Invariants: invariants,
		Findings:   findings,
	}
}

func RunShortcutCompliance(ctx context.Context, opts ShortcutComplianceOptions) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("check shortcut compliance context: %w", err)
	}
	repoRoot := strings.TrimSpace(opts.RepoRoot)
	if repoRoot == "" {
		return fmt.Errorf("repo root is required")
	}
	format := opts.Format
	if format == "" {
		format = ShortcutComplianceFormatMarkdown
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	report := BuildShortcutComplianceReport(repoRoot)
	switch format {
	case ShortcutComplianceFormatMarkdown:
		if _, err := io.WriteString(stdout, renderShortcutComplianceMarkdown(report)); err != nil {
			return fmt.Errorf("write shortcut compliance report: %w", err)
		}
	case ShortcutComplianceFormatJSON:
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("encode shortcut compliance report: %w", err)
		}
	default:
		return fmt.Errorf("usage: devtool shortcut-compliance --format [markdown|json]")
	}
	if report.Status != verifySummaryStatusPass {
		return fmt.Errorf("shortcut compliance failed")
	}
	return nil
}

func shortcutComplianceInvariants() []ShortcutComplianceInvariant {
	return []ShortcutComplianceInvariant{
		{
			ID:      "shortcut-parent-owns-truth",
			Summary: "Parent engines own durable shortcut roots, protected roots, status metadata, and child process snapshots.",
			Status:  verifySummaryStatusPass,
			Evidence: []string{
				"internal/sync/shortcut_root_store.go",
				"internal/sync/shortcut_root_publication.go",
				"spec/design/sync-engine.md",
			},
		},
		{
			ID:      "multisync-process-executor-only",
			Summary: "Multisync owns runner lifecycle, final drain, child artifact cleanup, and live-parent acknowledgements only.",
			Status:  verifySummaryStatusPass,
			Evidence: []string{
				"internal/multisync/shortcut_process_snapshot.go",
				"internal/multisync/shortcut_child_artifacts.go",
				"spec/design/sync-control-plane.md",
			},
		},
		{
			ID: "one-shot-starts-on-parent-publication",
			Summary: "One-shot starts child work from the first fresh parent process snapshot " +
				"and waits for the parent safe point only before acknowledgements.",
			Status: verifySummaryStatusPass,
			Evidence: []string{
				"internal/multisync/orchestrator_run_once_children.go",
				"TestRunOnce_StartsParentChildrenAsSoonAsParentPublishes",
				"TestRunOnce_DelaysFinalDrainAckUntilPublishingParentSafePoint",
			},
		},
		{
			ID:      "status-view-boundary",
			Summary: "CLI status consumes sync-owned ShortcutRootStatusView metadata instead of raw shortcut-root state.",
			Status:  verifySummaryStatusPass,
			Evidence: []string{
				"internal/sync/shortcut_root_status.go",
				"internal/cli/status_snapshot.go",
				"spec/design/cli.md",
			},
		},
		{
			ID:      "explicit-child-cleanup-scope",
			Summary: "Child artifact cleanup uses explicit DataDir, child mount ID, local root, and acknowledgement references.",
			Status:  verifySummaryStatusPass,
			Evidence: []string{
				"internal/multisync/shortcut_child_artifacts.go",
				"TestOrchestratorCleanupWithEmptyDataDirFailsLoudly",
				"spec/design/sync-store.md",
			},
		},
	}
}

func renderShortcutComplianceMarkdown(report ShortcutComplianceReport) string {
	var builder strings.Builder
	builder.WriteString("# Shortcut Compliance\n\n")
	builder.WriteString("- Status: ")
	builder.WriteString(report.Status)
	builder.WriteString("\n\n")
	builder.WriteString("| Invariant | Status | Evidence |\n")
	builder.WriteString("| --- | --- | --- |\n")
	for _, invariant := range report.Invariants {
		builder.WriteString("| ")
		builder.WriteString(invariant.ID)
		builder.WriteString(": ")
		builder.WriteString(invariant.Summary)
		builder.WriteString(" | ")
		builder.WriteString(invariant.Status)
		builder.WriteString(" | ")
		builder.WriteString(strings.Join(invariant.Evidence, "<br>"))
		builder.WriteString(" |\n")
	}
	if len(report.Findings) > 0 {
		builder.WriteString("\n## Findings\n\n")
		for _, finding := range report.Findings {
			builder.WriteString("- ")
			builder.WriteString(finding)
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func splitShortcutComplianceFindings(text string) []string {
	lines := strings.Split(text, "\n")
	findings := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			findings = append(findings, line)
		}
	}
	return findings
}
