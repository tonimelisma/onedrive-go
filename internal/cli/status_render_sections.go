package cli

import "io"

const statusNextIndent = "      "

func printDriveSyncSections(w io.Writer, ss *syncStateInfo, history bool) error {
	_ = history

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    CONDITIONS"); err != nil {
		return err
	}

	return printConditionSection(w, ss.Conditions)
}

func printConditionSection(w io.Writer, groups []statusConditionJSON) error {
	if len(groups) == 0 {
		return writeln(w, "    No active conditions.")
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
		if err := printConditionPaths(w, group.Paths, group.Count); err != nil {
			return err
		}
		if err := printStatusNextLine(w, group.Action); err != nil {
			return err
		}
	}

	return nil
}

func printConditionPaths(w io.Writer, paths []string, totalCount int) error {
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

func printStatusNextLine(w io.Writer, hint string) error {
	if hint == "" {
		return nil
	}

	return writef(w, "%sNext: %s\n", statusNextIndent, hint)
}
