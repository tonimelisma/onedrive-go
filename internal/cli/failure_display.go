package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Grouped display of sync failures for the `issues` command. The store owns
// issue grouping/counting; CLI formatting only renders the read-only snapshot.
// Implements R-2.3.7, R-2.3.8, R-2.3.9, R-2.10.4, R-2.10.22, R-6.6.11.

// defaultVisiblePaths is the maximum number of paths shown per group
// in non-verbose mode.
const defaultVisiblePaths = 5

type issueTextSection struct {
	present bool
	print   func() error
}

// printGroupedFailures renders grouped failure output to the writer.
// When verbose is true, all paths are shown; otherwise only the first
// defaultVisiblePaths are shown with a "... and N more" suffix.
func printGroupedFailures(w io.Writer, groups []syncstore.IssueGroupSnapshot, verbose bool) error {
	for i := range groups {
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}

		if err := printFailureGroup(w, &groups[i], verbose); err != nil {
			return err
		}
	}

	return nil
}

func printFailureGroup(w io.Writer, group *syncstore.IssueGroupSnapshot, verbose bool) error {
	descriptor := synctypes.DescribeSummary(group.SummaryKey)
	if err := writef(w, "%s (%d %s)\n", descriptor.Title, group.Count, itemNoun(group.Count)); err != nil {
		return err
	}
	if err := writef(w, "  %s %s\n", descriptor.Reason, descriptor.Action); err != nil {
		return err
	}
	if group.ScopeLabel != "" {
		if err := writef(w, "  Scope: %s\n", group.ScopeLabel); err != nil {
			return err
		}
	}
	if group.SummaryKey == synctypes.SummarySharedFolderWritesBlocked {
		if err := writef(w, "  Recheck: run 'onedrive-go issues recheck %s' to validate permissions now\n", group.ScopeKey); err != nil {
			return err
		}
		if group.RecheckRequestedAt > 0 {
			if err := writef(w, "  Recheck requested: %s\n", formatNanoTimestamp(group.RecheckRequestedAt)); err != nil {
				return err
			}
		}
		if group.HasManualTrial {
			if err := writef(w, "  Retry trial requested for one blocked path.\n"); err != nil {
				return err
			}
		}
	}

	return printFailurePaths(w, group, verbose)
}

func printFailurePaths(w io.Writer, group *syncstore.IssueGroupSnapshot, verbose bool) error {
	if len(group.Paths) == 0 {
		return nil
	}
	if err := writeln(w); err != nil {
		return err
	}

	limit := len(group.Paths)
	if !verbose && limit > defaultVisiblePaths {
		limit = defaultVisiblePaths
	}

	for _, path := range group.Paths[:limit] {
		if err := writef(w, "  %s\n", path); err != nil {
			return err
		}
	}

	remaining := group.Count - limit
	if remaining > 0 {
		if err := writef(w, "  ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

// heldDeleteDirGroupThreshold is the number of held deletes above which
// the display groups by parent directory instead of listing individually.
const heldDeleteDirGroupThreshold = 20

// printPendingRetries renders a summary of pending retries grouped by scope.
func printPendingRetries(w io.Writer, groups []syncstore.PendingRetrySnapshot) error {
	total := 0
	for _, g := range groups {
		total += g.Count
	}

	if err := writef(w, "PENDING RETRIES (%d %s)\n", total, itemNoun(total)); err != nil {
		return err
	}

	for _, g := range groups {
		humanScope := g.ScopeLabel
		if humanScope == "" {
			humanScope = g.ScopeKey.Humanize(nil)
		}
		if humanScope == "" {
			humanScope = "(unscoped)"
		}

		remaining := time.Until(g.EarliestNext)
		if remaining < 0 {
			remaining = 0
		}

		if err := writef(w, "  %-30s — %d %s, next attempt in %s\n",
			humanScope, g.Count, itemNoun(g.Count), formatDuration(remaining)); err != nil {
			return err
		}
	}

	return nil
}

// printHeldDeletesGrouped renders held deletes grouped by parent directory
// when the count exceeds the threshold, or individually when small.
func printHeldDeletesGrouped(w io.Writer, heldDeletes []syncstore.HeldDeleteSnapshot, verbose bool) error {
	if err := writef(w, "HELD DELETES (%d files — big-delete protection triggered, run `issues clear` to approve)\n",
		len(heldDeletes)); err != nil {
		return err
	}

	// When verbose or small count, show individual paths via the table.
	if verbose || len(heldDeletes) <= heldDeleteDirGroupThreshold {
		return printHeldDeletesTable(w, heldDeletes)
	}

	// Group by parent directory for large sets.
	dirCounts := make(map[string]int)
	for i := range heldDeletes {
		dir := filepath.Dir(heldDeletes[i].Path)
		if dir == "." {
			dir = "(root)"
		}

		dirCounts[dir]++
	}

	// Sort by count descending, then path ascending.
	type dirEntry struct {
		dir   string
		count int
	}

	entries := make([]dirEntry, 0, len(dirCounts))
	for dir, count := range dirCounts {
		entries = append(entries, dirEntry{dir: dir, count: count})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}

		return entries[i].dir < entries[j].dir
	})

	for _, e := range entries {
		if err := writef(w, "  %-40s — %d %s\n", e.dir+"/", e.count, itemNoun(e.count)); err != nil {
			return err
		}
	}

	return writef(w, "  (use --verbose to see individual paths)\n")
}

func itemNoun(n int) string {
	if n == 1 {
		return "item"
	}

	return "items"
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "now"
	}

	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}

	const secsPerMin = 60

	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*secsPerMin
		if s > 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}

		return fmt.Sprintf("%dm", m)
	}

	h := int(d.Hours())
	m := int(d.Minutes()) - h*secsPerMin

	if m > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}

	return fmt.Sprintf("%dh", h)
}

// failureGroupJSON is the JSON representation of a grouped failure set.
type failureGroupJSON struct {
	IssueType string   `json:"issue_type"`
	Title     string   `json:"title"`
	Reason    string   `json:"reason"`
	Action    string   `json:"action"`
	Scope     string   `json:"scope,omitempty"`
	Count     int      `json:"count"`
	Paths     []string `json:"paths"`
}

// issuesOutputJSON is the top-level JSON structure for the issues command.
type issuesOutputJSON struct {
	Conflicts     []conflictJSON     `json:"conflicts"`
	FailureGroups []failureGroupJSON `json:"failure_groups"`
	HeldDeletes   []heldDeleteJSON   `json:"held_deletes,omitempty"`
}

// heldDeleteJSON is the JSON representation of a held delete entry.
type heldDeleteJSON struct {
	Path       string `json:"path"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// printGroupedIssuesJSON writes the structured JSON output for issues.
func printGroupedIssuesJSON(
	w io.Writer,
	snapshot syncstore.IssuesSnapshot,
) error {
	out := issuesOutputJSON{
		Conflicts:     make([]conflictJSON, len(snapshot.Conflicts)),
		FailureGroups: make([]failureGroupJSON, len(snapshot.Groups)),
	}

	for i := range snapshot.Conflicts {
		out.Conflicts[i] = toConflictJSON(&snapshot.Conflicts[i])
	}

	for i := range snapshot.Groups {
		paths := snapshot.Groups[i].Paths
		if paths == nil {
			paths = []string{}
		}
		descriptor := synctypes.DescribeSummary(snapshot.Groups[i].SummaryKey)

		out.FailureGroups[i] = failureGroupJSON{
			IssueType:            snapshot.Groups[i].PrimaryIssueType,
			Title:                descriptor.Title,
			Reason:               descriptor.Reason,
			Action:               descriptor.Action,
			Scope:                snapshot.Groups[i].ScopeLabel,
			Count:                snapshot.Groups[i].Count,
			Paths:                paths,
			ManualTrialRequested: snapshot.Groups[i].HasManualTrial,
			RecheckRequestedAt:   formatNanoTimestamp(snapshot.Groups[i].RecheckRequestedAt),
		}
	}

	for i := range snapshot.HeldDeletes {
		out.HeldDeletes = append(out.HeldDeletes, heldDeleteJSON{
			Path:       snapshot.HeldDeletes[i].Path,
			LastSeenAt: formatNanoTimestamp(snapshot.HeldDeletes[i].LastSeenAt),
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

// printGroupedIssuesText renders the full text output for the issues
// command using grouped failure display. When verbose is true, all paths are
// shown; otherwise only the first defaultVisiblePaths per group are shown.
func printGroupedIssuesText(
	w io.Writer,
	snapshot syncstore.IssuesSnapshot,
	history, verbose bool,
) error {
	sections := []issueTextSection{
		{
			present: len(snapshot.Conflicts) > 0,
			print: func() error {
				if err := writeln(w, "CONFLICTS"); err != nil {
					return err
				}

				return printConflictsTable(w, snapshot.Conflicts, history)
			},
		},
		{
			present: len(snapshot.Groups) > 0,
			print: func() error {
				return printGroupedFailures(w, snapshot.Groups, verbose)
			},
		},
		{
			present: len(snapshot.PendingRetries) > 0,
			print: func() error {
				return printPendingRetries(w, snapshot.PendingRetries)
			},
		},
		{
			present: len(snapshot.HeldDeletes) > 0,
			print: func() error {
				return printHeldDeletesGrouped(w, snapshot.HeldDeletes, verbose)
			},
		},
	}

	wroteSection := false
	for _, section := range sections {
		if !section.present {
			continue
		}

		if wroteSection {
			if err := writeln(w); err != nil {
				return err
			}
		}

		if err := section.print(); err != nil {
			return err
		}

		wroteSection = true
	}

	return nil
}
