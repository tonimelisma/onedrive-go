package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Grouped display of sync failures for the `issues` command. The store owns
// issue grouping/counting; CLI formatting only renders the read-only snapshot.
// Implements R-2.3.7, R-2.3.8, R-2.3.9, R-2.10.4, R-6.6.11.

// defaultVisiblePaths is the maximum number of paths shown per group
// in non-verbose mode.
const defaultVisiblePaths = 5

type issueTextSection struct {
	present bool
	print   func() error
}

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

func printHeldDeletesGrouped(w io.Writer, heldDeletes []syncstore.HeldDeleteSnapshot, verbose bool) error {
	if err := writef(w, "HELD DELETES (%d files — big-delete protection triggered, run `issues approve-deletes` to approve)\n",
		len(heldDeletes)); err != nil {
		return err
	}

	if verbose || len(heldDeletes) <= heldDeleteDirGroupThreshold {
		return printHeldDeletesTable(w, heldDeletes)
	}

	dirCounts := make(map[string]int)
	for i := range heldDeletes {
		dir := filepath.Dir(heldDeletes[i].Path)
		if dir == "." {
			dir = "(root)"
		}

		dirCounts[dir]++
	}

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

func printHeldDeletesTable(w io.Writer, failures []syncstore.HeldDeleteSnapshot) error {
	headers := []string{"PATH", "LAST SEEN"}

	rows := make([][]string, len(failures))
	for i := range failures {
		row := &failures[i]
		lastSeen := ""

		if row.LastSeenAt != 0 {
			lastSeen = formatNanoTimestamp(row.LastSeenAt)
		}

		rows[i] = []string{row.Path, lastSeen}
	}

	return printTable(w, headers, rows)
}

func itemNoun(n int) string {
	if n == 1 {
		return "item"
	}

	return "items"
}

type failureGroupJSON struct {
	IssueType string   `json:"issue_type"`
	Title     string   `json:"title"`
	Reason    string   `json:"reason"`
	Action    string   `json:"action"`
	Scope     string   `json:"scope,omitempty"`
	Count     int      `json:"count"`
	Paths     []string `json:"paths"`
}

type issuesOutputJSON struct {
	FailureGroups []failureGroupJSON `json:"failure_groups"`
	HeldDeletes   []heldDeleteJSON   `json:"held_deletes,omitempty"`
}

type heldDeleteJSON struct {
	Path       string `json:"path"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

func printGroupedIssuesJSON(w io.Writer, snapshot syncstore.IssuesSnapshot) error {
	out := issuesOutputJSON{
		FailureGroups: make([]failureGroupJSON, len(snapshot.Groups)),
	}

	for i := range snapshot.Groups {
		paths := snapshot.Groups[i].Paths
		if paths == nil {
			paths = []string{}
		}
		descriptor := synctypes.DescribeSummary(snapshot.Groups[i].SummaryKey)

		out.FailureGroups[i] = failureGroupJSON{
			IssueType: snapshot.Groups[i].PrimaryIssueType,
			Title:     descriptor.Title,
			Reason:    descriptor.Reason,
			Action:    descriptor.Action,
			Scope:     snapshot.Groups[i].ScopeLabel,
			Count:     snapshot.Groups[i].Count,
			Paths:     paths,
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

func printGroupedIssuesText(w io.Writer, snapshot syncstore.IssuesSnapshot, verbose bool) error {
	sections := []issueTextSection{
		{
			present: len(snapshot.Groups) > 0,
			print: func() error {
				return printGroupedFailures(w, snapshot.Groups, verbose)
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
