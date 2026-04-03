package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Grouped display of sync failures for the `issues` command.
//
// Groups failures by (issue_type, scope_key) and renders human-readable
// output with titles, reasons, and suggested actions. Implements R-2.3.7,
// R-2.3.8, R-2.3.9, R-2.10.4, R-2.10.22, R-6.6.11.

// defaultVisiblePaths is the maximum number of paths shown per group
// in non-verbose mode.
const defaultVisiblePaths = 5

// failureGroup aggregates failures sharing the same issue type and scope.
type failureGroup struct {
	IssueType string
	ScopeKey  string // humanized (e.g. "Team Docs" instead of internal drive ID)
	Message   synctypes.IssueMessage
	Paths     []string
	Count     int
}

type issueTextSection struct {
	present bool
	print   func() error
}

// groupFailures groups failures by (issue_type, scope_key), humanizing scope
// keys using the provided shortcut list. big_delete_held entries are separated
// into a distinct slice.
func groupFailures(
	failures []synctypes.SyncFailureRow, shortcuts []synctypes.Shortcut,
) (groups []failureGroup, heldDeletes []synctypes.SyncFailureRow) {
	type groupKey struct {
		issueType string
		scopeKey  string
	}

	idx := map[groupKey]int{} // groupKey → index in groups slice

	for i := range failures {
		f := &failures[i]

		if f.IssueType == synctypes.IssueBigDeleteHeld {
			heldDeletes = append(heldDeletes, *f)
			continue
		}

		humanScope := f.ScopeKey.Humanize(shortcuts)
		gk := groupKey{issueType: f.IssueType, scopeKey: humanScope}

		if j, ok := idx[gk]; ok {
			groups[j].Paths = append(groups[j].Paths, f.Path)
			groups[j].Count++
		} else {
			idx[gk] = len(groups)
			groups = append(groups, failureGroup{
				IssueType: f.IssueType,
				ScopeKey:  humanScope,
				Message:   synctypes.MessageForIssueType(f.IssueType),
				Paths:     []string{f.Path},
				Count:     1,
			})
		}
	}

	sortFailureGroups(groups)

	return groups, heldDeletes
}

func appendScopeOnlyGroups(
	groups []failureGroup,
	blocks []*synctypes.ScopeBlock,
	shortcuts []synctypes.Shortcut,
) []failureGroup {
	for i := range blocks {
		if blocks[i].Key != synctypes.SKAuthAccount() {
			continue
		}

		// auth:account is durable scope state with no path owner, so issues must
		// surface it from scope_blocks instead of fabricating sentinel paths.
		groups = append(groups, failureGroup{
			IssueType: blocks[i].IssueType,
			ScopeKey:  blocks[i].Key.Humanize(shortcuts),
			Message:   synctypes.MessageForIssueType(blocks[i].IssueType),
			Paths:     []string{},
			Count:     1,
		})
	}

	sortFailureGroups(groups)

	return groups
}

func sortFailureGroups(groups []failureGroup) {
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
}

// printGroupedFailures renders grouped failure output to the writer.
// When verbose is true, all paths are shown; otherwise only the first
// defaultVisiblePaths are shown with a "... and N more" suffix.
func printGroupedFailures(w io.Writer, groups []failureGroup, verbose bool) error {
	for i, g := range groups {
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}

		if err := printFailureGroup(w, g, verbose); err != nil {
			return err
		}
	}

	return nil
}

func printFailureGroup(w io.Writer, group failureGroup, verbose bool) error {
	if err := writef(w, "%s (%d %s)\n", group.Message.Title, group.Count, itemNoun(group.Count)); err != nil {
		return err
	}
	if err := writef(w, "  %s %s\n", group.Message.Reason, group.Message.Action); err != nil {
		return err
	}
	if group.ScopeKey != "" {
		if err := writef(w, "  Scope: %s\n", group.ScopeKey); err != nil {
			return err
		}
	}

	return printFailurePaths(w, group, verbose)
}

func printFailurePaths(w io.Writer, group failureGroup, verbose bool) error {
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
func printPendingRetries(w io.Writer, groups []synctypes.PendingRetryGroup, shortcuts []synctypes.Shortcut) error {
	total := 0
	for _, g := range groups {
		total += g.Count
	}

	if err := writef(w, "PENDING RETRIES (%d %s)\n", total, itemNoun(total)); err != nil {
		return err
	}

	for _, g := range groups {
		humanScope := g.ScopeKey.Humanize(shortcuts)
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
func printHeldDeletesGrouped(w io.Writer, heldDeletes []synctypes.SyncFailureRow, verbose bool) error {
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
	w io.Writer, conflicts []synctypes.ConflictRecord,
	groups []failureGroup, heldDeletes []synctypes.SyncFailureRow,
) error {
	out := issuesOutputJSON{
		Conflicts:     make([]conflictJSON, len(conflicts)),
		FailureGroups: make([]failureGroupJSON, len(groups)),
	}

	for i := range conflicts {
		out.Conflicts[i] = toConflictJSON(&conflicts[i])
	}

	for i, g := range groups {
		paths := g.Paths
		if paths == nil {
			paths = []string{}
		}

		out.FailureGroups[i] = failureGroupJSON{
			IssueType: g.IssueType,
			Title:     g.Message.Title,
			Reason:    g.Message.Reason,
			Action:    g.Message.Action,
			Scope:     g.ScopeKey,
			Count:     g.Count,
			Paths:     paths,
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

// printGroupedIssuesText renders the full text output for the issues
// command using grouped failure display. When verbose is true, all paths are
// shown; otherwise only the first defaultVisiblePaths per group are shown.
func printGroupedIssuesText(
	w io.Writer, conflicts []synctypes.ConflictRecord,
	groups []failureGroup, heldDeletes []synctypes.SyncFailureRow,
	pendingRetries []synctypes.PendingRetryGroup, shortcuts []synctypes.Shortcut,
	history, verbose bool,
) error {
	sections := []issueTextSection{
		{
			present: len(conflicts) > 0,
			print: func() error {
				if err := writeln(w, "CONFLICTS"); err != nil {
					return err
				}

				return printConflictsTable(w, conflicts, history)
			},
		},
		{
			present: len(groups) > 0,
			print: func() error {
				return printGroupedFailures(w, groups, verbose)
			},
		},
		{
			present: len(pendingRetries) > 0,
			print: func() error {
				return printPendingRetries(w, pendingRetries, shortcuts)
			},
		},
		{
			present: len(heldDeletes) > 0,
			print: func() error {
				return printHeldDeletesGrouped(w, heldDeletes, verbose)
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
