package cli

import "io"

func printMountSyncSections(w io.Writer, indent string, ss *syncStateInfo, history bool) error {
	_ = history
	nextIndent := indent + "  "

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, indent+"CONDITIONS"); err != nil {
		return err
	}

	return printConditionSection(w, indent, nextIndent, ss.Conditions)
}

func printConditionSection(w io.Writer, indent string, nextIndent string, groups []statusConditionJSON) error {
	if len(groups) == 0 {
		return writeln(w, indent+"No active conditions.")
	}

	for i := range groups {
		group := groups[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "%s%s (%d %s)\n", indent, group.Title, group.Count, itemNoun(group.Count)); err != nil {
			return err
		}
		if err := writef(w, "%s%s %s\n", nextIndent, group.Reason, group.Action); err != nil {
			return err
		}
		if group.Scope != "" {
			if err := writef(w, "%sScope: %s\n", nextIndent, group.Scope); err != nil {
				return err
			}
		}
		if err := printConditionPaths(w, nextIndent, group.Paths, group.Count); err != nil {
			return err
		}
		if err := printStatusNextLine(w, nextIndent, group.Action); err != nil {
			return err
		}
	}

	return nil
}

func printConditionPaths(w io.Writer, indent string, paths []string, totalCount int) error {
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

func printStatusNextLine(w io.Writer, indent string, hint string) error {
	if hint == "" {
		return nil
	}

	return writef(w, "%sNext: %s\n", indent, hint)
}
