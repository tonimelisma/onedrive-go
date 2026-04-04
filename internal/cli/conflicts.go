package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// conflictIDPrefixLen is the number of characters to show for the conflict ID
// in table output. 8 hex chars = 32 bits of entropy = 4 billion values,
// sufficient for uniqueness in any realistic conflict set.
const conflictIDPrefixLen = 8

// Resolution strategy aliases (re-export from synctypes package for CLI use).
const (
	resolutionKeepLocal  = synctypes.ResolutionKeepLocal
	resolutionKeepRemote = synctypes.ResolutionKeepRemote
	resolutionKeepBoth   = synctypes.ResolutionKeepBoth
)

func newConflictsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "List and resolve sync conflicts",
		Long: `Display sync conflicts for the selected drive.

Shows unresolved conflicts by default. Use --history to include resolved
conflicts, or the resolve subcommand to choose how unresolved conflicts are
handled.`,
		RunE: runConflictsList,
	}

	cmd.Flags().Bool("history", false, "include resolved conflicts")
	cmd.AddCommand(newConflictsResolveCmd())

	return cmd
}

func runConflictsList(cmd *cobra.Command, _ []string) error {
	history, err := cmd.Flags().GetBool("history")
	if err != nil {
		return fmt.Errorf("read --history flag: %w", err)
	}

	return newConflictsService(mustCLIContext(cmd.Context())).runList(cmd.Context(), history)
}

func newConflictsResolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve [path-or-id]",
		Short: "Resolve sync conflicts",
		Long: `Resolve sync conflicts with a chosen strategy.

Strategies:
  --keep-local   Upload the local file to overwrite remote
  --keep-remote  Download the remote file to overwrite local
  --keep-both    Keep both versions as-is

Use --all to resolve all unresolved conflicts with the chosen strategy.
Without --all, a path or conflict ID argument is required.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runConflictsResolve,
	}

	cmd.Flags().Bool("keep-local", false, "upload local file to overwrite remote")
	cmd.Flags().Bool("keep-remote", false, "download remote file to overwrite local")
	cmd.Flags().Bool("keep-both", false, "keep both versions as-is")
	cmd.Flags().Bool("all", false, "resolve all unresolved conflicts")
	cmd.Flags().Bool("dry-run", false, "preview resolution without executing")
	cmd.MarkFlagsMutuallyExclusive("keep-local", "keep-remote", "keep-both")

	return cmd
}

func runConflictsResolve(cmd *cobra.Command, args []string) error {
	resolution, err := resolveStrategy(cmd)
	if err != nil {
		return err
	}

	resolveAll := cmd.Flags().Changed("all")
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("read --dry-run flag: %w", err)
	}

	if !resolveAll && len(args) == 0 {
		return fmt.Errorf("specify a conflict path or ID, or use --all to resolve all conflicts")
	}
	if resolveAll && len(args) > 0 {
		return fmt.Errorf("--all and a specific conflict argument are mutually exclusive")
	}

	return newConflictsService(mustCLIContext(cmd.Context())).runResolve(cmd.Context(), args, resolution, resolveAll, dryRun)
}

// resolveStrategy returns the chosen resolution string from flags.
func resolveStrategy(cmd *cobra.Command) (string, error) {
	keepLocal := cmd.Flags().Changed("keep-local")
	keepRemote := cmd.Flags().Changed("keep-remote")
	keepBoth := cmd.Flags().Changed("keep-both")

	if !keepLocal && !keepRemote && !keepBoth {
		return "", fmt.Errorf("specify a resolution strategy: --keep-local, --keep-remote, or --keep-both")
	}

	switch {
	case keepLocal:
		return resolutionKeepLocal, nil
	case keepRemote:
		return resolutionKeepRemote, nil
	default:
		return resolutionKeepBoth, nil
	}
}

func resolveEachConflict(
	cc *CLIContext,
	conflicts []synctypes.ConflictRecord,
	resolution string,
	dryRun bool,
	resolveFn func(id, resolution string) error,
) error {
	if len(conflicts) == 0 {
		cc.Statusf("No unresolved conflicts.\n")
		return nil
	}

	for i := range conflicts {
		c := &conflicts[i]
		if dryRun {
			cc.Statusf("Would resolve %s (%s) as %s\n", c.Path, truncateID(c.ID), resolution)
			continue
		}

		if err := resolveFn(c.ID, resolution); err != nil {
			return fmt.Errorf("resolving %s: %w", c.Path, err)
		}

		cc.Statusf("Resolved %s as %s\n", c.Path, resolution)
	}

	return nil
}

func resolveSingleConflict(
	cc *CLIContext,
	idOrPath string,
	resolution string,
	dryRun bool,
	listFn func() ([]synctypes.ConflictRecord, error),
	listAllFn func() ([]synctypes.ConflictRecord, error),
	resolveFn func(id, resolution string) error,
) error {
	conflicts, err := listFn()
	if err != nil {
		return err
	}

	target, found, findErr := findConflict(conflicts, idOrPath)
	if findErr != nil {
		return findErr
	}

	if !found {
		if listAllFn != nil {
			allConflicts, err := listAllFn()
			if err != nil {
				return err
			}

			if resolvedConflict, resolved, findErr := findConflict(allConflicts, idOrPath); findErr != nil {
				return findErr
			} else if resolved && resolvedConflict.Resolution != synctypes.ResolutionUnresolved {
				cc.Statusf("Conflict %s already resolved as %s\n", resolvedConflict.Path, resolvedConflict.Resolution)
				return nil
			}
		}

		return fmt.Errorf("conflict not found: %s", idOrPath)
	}

	if dryRun {
		cc.Statusf("Would resolve %s (%s) as %s\n", target.Path, truncateID(target.ID), resolution)
		return nil
	}

	if err := resolveFn(target.ID, resolution); err != nil {
		return err
	}

	cc.Statusf("Resolved %s as %s\n", target.Path, resolution)

	return nil
}

func errAmbiguousPrefix(prefix string) error {
	return fmt.Errorf("ambiguous conflict ID prefix %q — provide more characters", prefix)
}

func findConflict(conflicts []synctypes.ConflictRecord, idOrPath string) (*synctypes.ConflictRecord, bool, error) {
	if idOrPath == "" {
		return nil, false, nil
	}

	for i := range conflicts {
		c := &conflicts[i]
		if c.ID == idOrPath || c.Path == idOrPath {
			return c, true, nil
		}
	}

	var match *synctypes.ConflictRecord
	for i := range conflicts {
		c := &conflicts[i]
		if len(c.ID) >= len(idOrPath) && c.ID[:len(idOrPath)] == idOrPath {
			if match != nil {
				return nil, false, errAmbiguousPrefix(idOrPath)
			}
			match = c
		}
	}

	return match, match != nil, nil
}

type conflictJSON struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	ConflictType string `json:"conflict_type"`
	DetectedAt   string `json:"detected_at"`
	LocalHash    string `json:"local_hash,omitempty"`
	RemoteHash   string `json:"remote_hash,omitempty"`
	Resolution   string `json:"resolution"`
	ResolvedAt   string `json:"resolved_at,omitempty"`
	ResolvedBy   string `json:"resolved_by,omitempty"`
}

func toConflictJSON(c *synctypes.ConflictRecord) conflictJSON {
	return conflictJSON{
		ID:           c.ID,
		Path:         c.Path,
		ConflictType: c.ConflictType,
		DetectedAt:   formatNanoTimestamp(c.DetectedAt),
		LocalHash:    c.LocalHash,
		RemoteHash:   c.RemoteHash,
		Resolution:   c.Resolution,
		ResolvedBy:   c.ResolvedBy,
		ResolvedAt:   formatNanoTimestamp(c.ResolvedAt),
	}
}

func printConflictsTable(w io.Writer, conflicts []synctypes.ConflictRecord, history bool) error {
	var headers []string
	if history {
		headers = []string{"ID", "PATH", "TYPE", "RESOLUTION", "RESOLVED BY", "DETECTED"}
	} else {
		headers = []string{"ID", "PATH", "TYPE", "DETECTED"}
	}

	rows := make([][]string, len(conflicts))
	for i := range conflicts {
		c := &conflicts[i]
		idPrefix := truncateID(c.ID)
		detected := formatNanoTimestamp(c.DetectedAt)

		if history {
			rows[i] = []string{idPrefix, c.Path, c.ConflictType, c.Resolution, c.ResolvedBy, detected}
		} else {
			rows[i] = []string{idPrefix, c.Path, c.ConflictType, detected}
		}
	}

	return printTable(w, headers, rows)
}

type conflictsOutputJSON struct {
	Conflicts []conflictJSON `json:"conflicts"`
}

func printConflictsJSON(w io.Writer, conflicts []synctypes.ConflictRecord) error {
	out := conflictsOutputJSON{
		Conflicts: make([]conflictJSON, len(conflicts)),
	}

	for i := range conflicts {
		out.Conflicts[i] = toConflictJSON(&conflicts[i])
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}
