// failure_display.go — Grouped display of sync failures for the `issues` command.
//
// Groups failures by (issue_type, scope_key) and renders human-readable
// output with titles, reasons, and suggested actions. Implements R-2.3.7,
// R-2.3.8, R-2.3.9, R-2.10.4, R-2.10.22, R-6.6.11.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// defaultVisiblePaths is the maximum number of paths shown per group
// in non-verbose mode.
const defaultVisiblePaths = 5

// failureGroup aggregates failures sharing the same issue type and scope.
type failureGroup struct {
	IssueType string
	ScopeKey  string // humanized (e.g. "Team Docs" instead of internal drive ID)
	Message   sync.IssueMessage
	Paths     []string
	Count     int
}

// groupFailures groups failures by (issue_type, scope_key), humanizing scope
// keys using the provided shortcut list. big_delete_held entries are separated
// into a distinct slice.
func groupFailures(failures []sync.SyncFailureRow, shortcuts []sync.Shortcut) (groups []failureGroup, heldDeletes []sync.SyncFailureRow) {
	type groupKey struct {
		issueType string
		scopeKey  string
	}

	idx := map[groupKey]int{} // groupKey → index in groups slice

	for i := range failures {
		f := &failures[i]

		if f.IssueType == sync.IssueBigDeleteHeld {
			heldDeletes = append(heldDeletes, *f)
			continue
		}

		humanScope := sync.HumanizeScopeKey(f.ScopeKey, shortcuts)
		gk := groupKey{issueType: f.IssueType, scopeKey: humanScope}

		if j, ok := idx[gk]; ok {
			groups[j].Paths = append(groups[j].Paths, f.Path)
			groups[j].Count++
		} else {
			idx[gk] = len(groups)
			groups = append(groups, failureGroup{
				IssueType: f.IssueType,
				ScopeKey:  humanScope,
				Message:   sync.MessageForIssueType(f.IssueType),
				Paths:     []string{f.Path},
				Count:     1,
			})
		}
	}

	// Sort groups: largest first for visibility, then alphabetically by title
	// for deterministic output.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Message.Title < groups[j].Message.Title
	})

	// Sort paths within each group for consistent output.
	for i := range groups {
		sort.Strings(groups[i].Paths)
	}

	return groups, heldDeletes
}

// printGroupedFailures renders grouped failure output to the writer.
// When verbose is true, all paths are shown; otherwise only the first
// defaultVisiblePaths are shown with a "... and N more" suffix.
func printGroupedFailures(w io.Writer, groups []failureGroup, verbose bool) {
	for i, g := range groups {
		if i > 0 {
			fmt.Fprintln(w)
		}

		// Header: TITLE (N items)
		noun := "items"
		if g.Count == 1 {
			noun = "item"
		}

		fmt.Fprintf(w, "%s (%d %s)\n", g.Message.Title, g.Count, noun)

		// Reason + action.
		fmt.Fprintf(w, "  %s %s\n", g.Message.Reason, g.Message.Action)

		// Scope line (only when non-empty and not a file-level-only group).
		if g.ScopeKey != "" {
			fmt.Fprintf(w, "  Scope: %s\n", g.ScopeKey)
		}

		// Paths.
		fmt.Fprintln(w)
		limit := len(g.Paths)
		if !verbose && limit > defaultVisiblePaths {
			limit = defaultVisiblePaths
		}

		for _, p := range g.Paths[:limit] {
			fmt.Fprintf(w, "  %s\n", p)
		}

		remaining := g.Count - limit
		if remaining > 0 {
			fmt.Fprintf(w, "  ... and %d more (use --verbose to see all)\n", remaining)
		}
	}
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
	Conflicts     []conflictJSON      `json:"conflicts"`
	FailureGroups []failureGroupJSON  `json:"failure_groups"`
	HeldDeletes   []heldDeleteJSON    `json:"held_deletes,omitempty"`
}

// heldDeleteJSON is the JSON representation of a held delete entry.
type heldDeleteJSON struct {
	Path       string `json:"path"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// printGroupedIssuesJSON writes the structured JSON output for issues.
func printGroupedIssuesJSON(w io.Writer, conflicts []sync.ConflictRecord, groups []failureGroup, heldDeletes []sync.SyncFailureRow) error {
	out := issuesOutputJSON{
		Conflicts:     make([]conflictJSON, len(conflicts)),
		FailureGroups: make([]failureGroupJSON, len(groups)),
	}

	for i := range conflicts {
		out.Conflicts[i] = toConflictJSON(&conflicts[i])
	}

	for i, g := range groups {
		out.FailureGroups[i] = failureGroupJSON{
			IssueType: g.IssueType,
			Title:     g.Message.Title,
			Reason:    g.Message.Reason,
			Action:    g.Message.Action,
			Scope:     g.ScopeKey,
			Count:     g.Count,
			Paths:     g.Paths,
		}
	}

	for i := range heldDeletes {
		out.HeldDeletes = append(out.HeldDeletes, heldDeleteJSON{
			Path:       heldDeletes[i].Path,
			LastSeenAt: formatNanoTimestamp(heldDeletes[i].LastSeenAt),
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

// printGroupedIssuesTextVerbose renders the full text output for the issues
// command using grouped failure display. When verbose is true, all paths are
// shown; otherwise only the first defaultVisiblePaths per group are shown.
func printGroupedIssuesTextVerbose(w io.Writer, conflicts []sync.ConflictRecord, groups []failureGroup, heldDeletes []sync.SyncFailureRow, history, verbose bool) {
	sections := 0

	if len(conflicts) > 0 {
		fmt.Fprintln(w, "CONFLICTS")
		printConflictsTable(w, conflicts, history)
		sections++
	}

	if len(heldDeletes) > 0 {
		if sections > 0 {
			fmt.Fprintln(w)
		}

		fmt.Fprintf(w, "HELD DELETES (%d files — big-delete protection triggered, run `issues clear` to approve)\n", len(heldDeletes))
		printHeldDeletesTable(w, heldDeletes)
		sections++
	}

	if len(groups) > 0 {
		if sections > 0 {
			fmt.Fprintln(w)
		}

		printGroupedFailures(w, groups, verbose)
	}
}