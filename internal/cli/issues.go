package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
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

func newIssuesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "List and manage sync issues",
		Long: `Display sync issues — conflicts and actionable file failures.

Shows unresolved conflicts and actionable failures (invalid filenames, files
too large, etc.) that require user attention. Use subcommands to resolve
conflicts or clear failures.`,
		RunE: runIssuesList,
	}

	cmd.Flags().Bool("history", false, "include resolved conflicts")

	cmd.AddCommand(newIssuesResolveCmd())
	cmd.AddCommand(newIssuesClearCmd())
	cmd.AddCommand(newIssuesRetryCmd())

	return cmd
}

// --- issues list ---

func runIssuesList(cmd *cobra.Command, _ []string) error {
	history, err := cmd.Flags().GetBool("history")
	if err != nil {
		return fmt.Errorf("read --history flag: %w", err)
	}

	return newIssuesService(mustCLIContext(cmd.Context())).runList(cmd.Context(), history)
}

// --- issues resolve ---

func newIssuesResolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve [path-or-id]",
		Short: "Resolve sync conflicts",
		Long: `Resolve sync conflicts with a chosen strategy.

Strategies:
  --keep-local   Upload the local file to overwrite remote
  --keep-remote  Download the remote file to overwrite local
  --keep-both    Keep both versions (conflict copies already saved)

Use --all to resolve all unresolved conflicts with the chosen strategy.
Without --all, a path or conflict ID argument is required.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runResolve,
	}

	cmd.Flags().Bool("keep-local", false, "upload local file to overwrite remote")
	cmd.Flags().Bool("keep-remote", false, "download remote file to overwrite local")
	cmd.Flags().Bool("keep-both", false, "keep both versions as-is")
	cmd.Flags().Bool("all", false, "resolve all unresolved conflicts")
	cmd.Flags().Bool("dry-run", false, "preview resolution without executing")

	cmd.MarkFlagsMutuallyExclusive("keep-local", "keep-remote", "keep-both")

	return cmd
}

func runResolve(cmd *cobra.Command, args []string) error {
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

	// All resolution strategies go through the engine: keep_local and
	// keep_remote need the graph client for transfers, and keep_both needs
	// the sync root to update baseline entries for the original file and
	// its conflict copies.
	return newIssuesService(mustCLIContext(cmd.Context())).runResolve(cmd.Context(), args, resolution, resolveAll, dryRun)
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
	cc *CLIContext, conflicts []synctypes.ConflictRecord, resolution string, dryRun bool,
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

func resolveWithTransfers(
	ctx context.Context, cc *CLIContext, args []string, resolution string, all, dryRun bool,
) error {
	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	engine, err := newSyncEngine(ctx, session, cc.Cfg, false, cc.Logger)
	if err != nil {
		return fmt.Errorf("create sync engine: %w", err)
	}
	defer engine.Close(ctx)

	if all {
		return resolveAllWithEngine(ctx, cc, engine, resolution, dryRun)
	}

	return resolveSingleWithEngine(ctx, cc, engine, args[0], resolution, dryRun)
}

func resolveAllWithEngine(ctx context.Context, cc *CLIContext, engine *sync.Engine, resolution string, dryRun bool) error {
	conflicts, err := engine.ListConflicts(ctx)
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}

	return resolveEachConflict(cc, conflicts, resolution, dryRun, func(id, res string) error {
		return engine.ResolveConflict(ctx, id, res)
	})
}

func resolveSingleWithEngine(ctx context.Context, cc *CLIContext, engine *sync.Engine, idOrPath, resolution string, dryRun bool) error {
	return resolveSingleConflict(cc, idOrPath, resolution, dryRun,
		func() ([]synctypes.ConflictRecord, error) { return engine.ListConflicts(ctx) },
		func(id, res string) error { return engine.ResolveConflict(ctx, id, res) },
	)
}

func resolveSingleConflict(
	cc *CLIContext, idOrPath, resolution string, dryRun bool,
	listFn func() ([]synctypes.ConflictRecord, error),
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

	// First pass: exact matches (ID or path) take priority.
	for i := range conflicts {
		c := &conflicts[i]
		if c.ID == idOrPath || c.Path == idOrPath {
			return c, true, nil
		}
	}

	// Second pass: prefix match with ambiguity detection.
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

// --- issues clear ---

func newIssuesClearCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clear [path]",
		Short: "Clear actionable sync failures",
		Long: `Clear specific or all actionable sync failures.

Provide a path to clear a specific failure. Use --all to clear all
actionable failures.`,
		RunE: runIssuesClear,
	}

	cmd.Flags().Bool("all", false, "clear all actionable failures")

	return cmd
}

func runIssuesClear(cmd *cobra.Command, args []string) error {
	return runFailureAction(cmd, args, failureAction{
		allFn: func(ctx context.Context, mgr *syncstore.SyncStore) error {
			if err := mgr.ClearActionableSyncFailures(ctx); err != nil {
				return fmt.Errorf("clear actionable failures: %w", err)
			}
			if err := mgr.ClearAllRemoteBlockedFailures(ctx); err != nil {
				return fmt.Errorf("clear blocked shared-folder writes: %w", err)
			}
			return nil
		},
		singleFn: func(ctx context.Context, mgr *syncstore.SyncStore, p string) error {
			if target, found, err := mgr.FindRemoteBlockedTarget(ctx, p); err != nil {
				return fmt.Errorf("find blocked shared-folder write for %s: %w", p, err)
			} else if found {
				if err := mgr.ClearRemoteBlockedTarget(ctx, target); err != nil {
					return fmt.Errorf("clear blocked shared-folder write for %s: %w", p, err)
				}
				return nil
			}
			if err := mgr.ClearSyncFailureByPath(ctx, p); err != nil {
				return fmt.Errorf("clear actionable failure for %s: %w", p, err)
			}
			return nil
		},
		noArgMsg:  "provide a path to clear, or use --all to clear all actionable failures",
		allMsg:    "Cleared actionable failures and blocked shared-folder writes.",
		singleFmt: "Cleared failure for %s.",
	})
}

// --- issues retry ---

func newIssuesRetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry [path]",
		Short: "Reset failures for immediate retry",
		Long: `Reset failure state so items are retried on the next sync.

Provide a path to retry a specific failure. Use --all to retry all
failed items.`,
		RunE: runIssuesRetry,
	}

	cmd.Flags().Bool("all", false, "retry all failed items")

	return cmd
}

func runIssuesRetry(cmd *cobra.Command, args []string) error {
	return runFailureAction(cmd, args, failureAction{
		allFn: func(ctx context.Context, mgr *syncstore.SyncStore) error { return mgr.ResetAllFailures(ctx) },
		singleFn: func(ctx context.Context, mgr *syncstore.SyncStore, p string) error {
			if target, found, err := mgr.FindRemoteBlockedTarget(ctx, p); err != nil {
				return fmt.Errorf("find blocked shared-folder write for %s: %w", p, err)
			} else if found {
				if target.Kind == syncstore.RemoteBlockedTargetBoundary {
					return fmt.Errorf("shared-folder write blocks must be retried by blocked path, not boundary")
				}
				if err := mgr.RequestRemoteBlockedTrial(ctx, target); err != nil {
					return fmt.Errorf("request blocked shared-folder retry for %s: %w", p, err)
				}
				return nil
			}
			if err := mgr.ResetFailure(ctx, p); err != nil {
				return fmt.Errorf("reset failure for %s: %w", p, err)
			}
			return nil
		},
		noArgMsg:  "provide a path to retry, or use --all to retry all failures",
		allMsg:    "Reset all failures for retry.",
		singleFmt: "Requested retry for %s.",
	})
}

// failureAction defines the all/single operations for a failures subcommand.
type failureAction struct {
	allFn     func(ctx context.Context, mgr *syncstore.SyncStore) error
	singleFn  func(ctx context.Context, mgr *syncstore.SyncStore, path string) error
	noArgMsg  string
	allMsg    string
	singleFmt string // format string with %s for path
}

func runFailureAction(cmd *cobra.Command, args []string, action failureAction) error {
	doAll, err := cmd.Flags().GetBool("all")
	if err != nil {
		return fmt.Errorf("read --all flag: %w", err)
	}

	return newIssuesService(mustCLIContext(cmd.Context())).runFailureAction(cmd.Context(), args, doAll, action)
}

// --- helpers ---

func truncateID(id string) string {
	if len(id) <= conflictIDPrefixLen {
		return id
	}

	return id[:conflictIDPrefixLen]
}

func formatNanoTimestamp(nanos int64) string {
	if nanos == 0 {
		return ""
	}

	return time.Unix(0, nanos).UTC().Format(time.RFC3339)
}

// --- conflict JSON/table ---

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

// printHeldDeletesTable renders held-delete entries with a simplified table
// (path only — direction is always "delete" and error is always the same).
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
