package cli

import (
	"io"
	"strings"
)

func printDriveSyncSections(w io.Writer, indent string, ss *syncStateInfo, history bool) error {
	_ = history
	if ss == nil || len(ss.Conditions) == 0 {
		return nil
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, indent+"Issues:"); err != nil {
		return err
	}

	return printIssueSection(w, indent+"  ", ss.Conditions)
}

func printIssueSection(w io.Writer, indent string, groups []statusConditionJSON) error {
	for i := range groups {
		group := groups[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "%s%s: %d %s\n", indent, issueTitle(group.Title), group.Count, itemNoun(group.Count)); err != nil {
			return err
		}
		if group.Reason != "" {
			if err := writef(w, "%s  %s\n", indent, group.Reason); err != nil {
				return err
			}
		}
		if group.Action != "" {
			if err := writef(w, "%s  Action: %s\n", indent, group.Action); err != nil {
				return err
			}
		}
		if group.Scope != "" {
			if err := writef(w, "%s  Scope: %s\n", indent, group.Scope); err != nil {
				return err
			}
		}
		if err := printIssuePaths(w, indent+"  ", group.Paths, group.Count); err != nil {
			return err
		}
	}

	return nil
}

func issueTitle(title string) string {
	if title == "" {
		return "Issue"
	}
	return strings.ToUpper(title[:1]) + title[1:]
}

func printIssuePaths(w io.Writer, indent string, paths []string, totalCount int) error {
	if len(paths) == 0 {
		return nil
	}
	if err := writeln(w); err != nil {
		return err
	}
	for i := range paths {
		if err := writef(w, "%s%s\n", indent, paths[i]); err != nil {
			return err
		}
	}

	remaining := totalCount - len(paths)
	if remaining > 0 {
		if err := writef(w, "%s... and %d more (use --verbose to see all)\n", indent, remaining); err != nil {
			return err
		}
	}

	return nil
}

func printMountSyncSections(w io.Writer, indent string, ss *syncStateInfo, history bool) error {
	return printDriveSyncSections(w, indent, ss, history)
}

func printConditionSection(w io.Writer, indent string, nextIndent string, groups []statusConditionJSON) error {
	_ = nextIndent
	return printIssueSection(w, indent, groups)
}

func printConditionPaths(w io.Writer, indent string, paths []string, totalCount int) error {
	return printIssuePaths(w, indent, paths, totalCount)
}

func printStatusNextLine(w io.Writer, indent string, hint string) error {
	if hint == "" {
		return nil
	}
	return writef(w, "%sAction: %s\n", indent, hint)
}
