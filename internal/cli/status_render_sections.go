package cli

import (
	"io"
	"strings"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const statusNextIndent = "      "

func printDriveSyncSections(w io.Writer, ss *syncStateInfo, history bool) error {
	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    ISSUES"); err != nil {
		return err
	}
	if err := printIssueGroupSection(w, ss.IssueGroups); err != nil {
		return err
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    DELETE SAFETY"); err != nil {
		return err
	}
	if err := printDeleteSafetySection(w, ss); err != nil {
		return err
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    CONFLICTS"); err != nil {
		return err
	}
	if err := printConflictSection(w, ss.Conflicts, ss.ConflictsTotal); err != nil {
		return err
	}

	if history {
		if err := writeln(w); err != nil {
			return err
		}
		if err := writeln(w, "    CONFLICT HISTORY"); err != nil {
			return err
		}
		if err := printConflictHistorySection(w, ss.ConflictHistory, ss.ConflictHistoryTotal); err != nil {
			return err
		}
	}

	return nil
}

func printIssueGroupSection(w io.Writer, groups []failureGroupJSON) error {
	if len(groups) == 0 {
		return writeln(w, "    No ordinary issues.")
	}

	for i := range groups {
		group := groups[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    %s (%d %s)\n", group.Title, group.Count, itemNoun(group.Count)); err != nil {
			return err
		}
		if err := writef(w, "      %s %s\n", group.Reason, group.Action); err != nil {
			return err
		}
		if group.Scope != "" {
			if err := writef(w, "      Scope: %s\n", group.Scope); err != nil {
				return err
			}
		}
		if err := printIssueGroupPaths(w, group.Paths, group.Count); err != nil {
			return err
		}
		if err := printStatusNextLine(w, group.Action); err != nil {
			return err
		}
	}

	return nil
}

func printIssueGroupPaths(w io.Writer, paths []string, totalCount int) error {
	if len(paths) == 0 {
		return nil
	}
	if err := writeln(w); err != nil {
		return err
	}
	for i := range paths {
		if err := writef(w, "      %s\n", paths[i]); err != nil {
			return err
		}
	}

	remaining := totalCount - len(paths)
	if remaining > 0 {
		if err := writef(w, "      ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printDeleteSafetySection(w io.Writer, ss *syncStateInfo) error {
	if ss == nil || ss.DeleteSafetyTotal == 0 {
		return writeln(w, "    No delete-safety entries.")
	}

	held := filterDeleteSafety(ss.DeleteSafety, stateHeldDeleteHeld)
	heldTotal := ss.HeldDeletesWaiting
	if heldTotal > 0 {
		if err := writef(w, "    Held deletes requiring approval: %d\n", heldTotal); err != nil {
			return err
		}
		if err := printDeleteSafetyRows(w, held, heldTotal); err != nil {
			return err
		}
		if len(held) > 0 {
			if err := printStatusNextLine(w, held[0].ActionHint); err != nil {
				return err
			}
		}
	}

	approved := filterDeleteSafety(ss.DeleteSafety, stateHeldDeleteApproved)
	approvedTotal := ss.ApprovedDeletesWaiting
	if approvedTotal > 0 {
		if heldTotal > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    Approved deletes waiting for sync: %d\n", approvedTotal); err != nil {
			return err
		}
		if err := printDeleteSafetyRows(w, approved, approvedTotal); err != nil {
			return err
		}
		if len(approved) > 0 {
			if err := printStatusNextLine(w, approved[0].ActionHint); err != nil {
				return err
			}
		}
	}

	return nil
}

func printDeleteSafetyRows(w io.Writer, rows []deleteSafetyJSON, totalCount int) error {
	for i := range rows {
		if err := writef(w, "      %s\n", rows[i].Path); err != nil {
			return err
		}
	}

	remaining := totalCount - len(rows)
	if remaining > 0 {
		if err := writef(w, "      ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printConflictSection(w io.Writer, conflicts []statusConflictJSON, totalCount int) error {
	if totalCount == 0 {
		return writeln(w, "    No unresolved conflicts.")
	}

	for i := range conflicts {
		conflict := conflicts[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    %s [%s]\n", conflict.Path, conflict.ConflictType); err != nil {
			return err
		}
		switch conflict.State {
		case syncengine.ConflictStateUnresolved:
			if err := writeln(w, "      Decision: needed"); err != nil {
				return err
			}
		default:
			if err := writef(w, "      Decision: %s (%s)\n", conflict.RequestedResolution, conflict.State); err != nil {
				return err
			}
		}
		if conflict.LastRequestError != "" {
			if err := writef(w, "      Last attempt: %s\n", conflict.LastRequestError); err != nil {
				return err
			}
		}
		if err := printStatusNextLine(w, conflict.ActionHint); err != nil {
			return err
		}
	}

	remaining := totalCount - len(conflicts)
	if remaining > 0 {
		if err := writef(w, "    ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printConflictHistorySection(
	w io.Writer,
	history []statusConflictHistoryJSON,
	totalCount int,
) error {
	if totalCount == 0 {
		return writeln(w, "    No resolved conflicts.")
	}

	for i := range history {
		entry := history[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    %s [%s]\n", entry.Path, entry.ConflictType); err != nil {
			return err
		}
		if err := writef(w, "      Resolved: %s", entry.Resolution); err != nil {
			return err
		}
		if entry.ResolvedBy != "" {
			if err := writef(w, " by %s", entry.ResolvedBy); err != nil {
				return err
			}
		}
		if entry.ResolvedAt != "" {
			if err := writef(w, " at %s", entry.ResolvedAt); err != nil {
				return err
			}
		}
		if err := writeln(w); err != nil {
			return err
		}
	}

	remaining := totalCount - len(history)
	if remaining > 0 {
		if err := writef(w, "    ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printStatusNextLine(w io.Writer, hint string) error {
	if strings.TrimSpace(hint) == "" {
		return nil
	}

	return writef(w, "%sNext: %s\n", statusNextIndent, hint)
}

func filterDeleteSafety(rows []deleteSafetyJSON, state string) []deleteSafetyJSON {
	filtered := make([]deleteSafetyJSON, 0, len(rows))
	for i := range rows {
		if rows[i].State == state {
			filtered = append(filtered, rows[i])
		}
	}

	return filtered
}
