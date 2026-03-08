package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// maxFailureErrorLen is the maximum error message length in table output.
const maxFailureErrorLen = 60

func newFailuresCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "failures",
		Short: "List sync failures",
		Long: `Display sync failures from the state database.

Sync failures track files that cannot be synced due to validation failures
(invalid filenames, paths too long, files too large), transient errors, or
download/delete failures. Use 'failures clear' to remove resolved failures.`,
		RunE: runFailuresList,
	}

	cmd.Flags().String("direction", "", "filter by direction (download, upload, delete)")
	cmd.Flags().String("category", "", "filter by category (transient, permanent)")

	cmd.AddCommand(newFailuresClearCmd())
	cmd.AddCommand(newFailuresRetryCmd())

	return cmd
}

// newIssuesCmd returns a hidden alias for the failures command (backward compat).
func newIssuesCmd() *cobra.Command {
	cmd := newFailuresCmd()
	cmd.Use = "issues"
	cmd.Hidden = true

	return cmd
}

func newFailuresClearCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clear [path]",
		Short: "Clear sync failures",
		Long: `Clear specific or resolved sync failures.

Provide a path to clear a specific failure. Use --all to clear all
permanent failures.`,
		RunE: runFailuresClear,
	}

	cmd.Flags().Bool("all", false, "clear all permanent failures")

	return cmd
}

// failureJSON is the JSON-serializable representation of a sync failure.
type failureJSON struct {
	Path         string `json:"path"`
	Direction    string `json:"direction"`
	Category     string `json:"category"`
	IssueType    string `json:"issue_type,omitempty"`
	FailureCount int    `json:"failure_count"`
	LastError    string `json:"last_error"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	FirstSeenAt  string `json:"first_seen_at"`
	LastSeenAt   string `json:"last_seen_at"`
}

func runFailuresList(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	mgr, err := sync.NewSyncStore(dbPath, cc.Logger)
	if err != nil {
		return err
	}
	defer mgr.Close()

	failures, err := mgr.ListSyncFailures(cmd.Context())
	if err != nil {
		return err
	}

	// Apply optional filters with validation.
	dirFilter, err := cmd.Flags().GetString("direction")
	if err != nil {
		return err
	}

	if dirFilter != "" && !validDirections[dirFilter] {
		return fmt.Errorf("invalid --direction %q: must be download, upload, or delete", dirFilter)
	}

	catFilter, err := cmd.Flags().GetString("category")
	if err != nil {
		return err
	}

	if catFilter != "" && !validCategories[catFilter] {
		return fmt.Errorf("invalid --category %q: must be transient or permanent", catFilter)
	}

	if dirFilter != "" || catFilter != "" {
		failures = filterFailures(failures, dirFilter, catFilter)
	}

	if len(failures) == 0 {
		fmt.Println("No sync failures.")
		return nil
	}

	if cc.Flags.JSON {
		return printFailuresJSON(os.Stdout, failures)
	}

	printFailuresTable(os.Stdout, failures)

	return nil
}

// filterFailures returns only rows matching the given direction and/or category.
func filterFailures(rows []sync.SyncFailureRow, direction, category string) []sync.SyncFailureRow {
	filtered := make([]sync.SyncFailureRow, 0, len(rows))

	for i := range rows {
		if direction != "" && rows[i].Direction != direction {
			continue
		}

		if category != "" && rows[i].Category != category {
			continue
		}

		filtered = append(filtered, rows[i])
	}

	return filtered
}

// failureAction defines the all/single operations for a failures subcommand.
type failureAction struct {
	allFn     func(ctx context.Context, mgr *sync.SyncStore) error
	singleFn  func(ctx context.Context, mgr *sync.SyncStore, path string) error
	noArgMsg  string
	allMsg    string
	singleFmt string // format string with %s for path
}

// runFailureAction is a shared runner for clear and retry subcommands.
func runFailureAction(cmd *cobra.Command, args []string, action failureAction) error {
	cc := mustCLIContext(cmd.Context())

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	mgr, err := sync.NewSyncStore(dbPath, cc.Logger)
	if err != nil {
		return err
	}
	defer mgr.Close()

	ctx := cmd.Context()

	doAll, err := cmd.Flags().GetBool("all")
	if err != nil {
		return err
	}

	if doAll {
		if err := action.allFn(ctx, mgr); err != nil {
			return err
		}

		fmt.Println(action.allMsg)

		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("%s", action.noArgMsg)
	}

	if err := action.singleFn(ctx, mgr, args[0]); err != nil {
		return err
	}

	fmt.Printf(action.singleFmt+"\n", args[0])

	return nil
}

func runFailuresClear(cmd *cobra.Command, args []string) error {
	return runFailureAction(cmd, args, failureAction{
		allFn: func(ctx context.Context, mgr *sync.SyncStore) error { return mgr.ClearResolvedSyncFailures(ctx) },
		singleFn: func(ctx context.Context, mgr *sync.SyncStore, p string) error {
			return mgr.ClearSyncFailureByPath(ctx, p)
		},
		noArgMsg:  "provide a path to clear, or use --all to clear all permanent failures",
		allMsg:    "Cleared all permanent failures.",
		singleFmt: "Cleared failure for %s.",
	})
}

func newFailuresRetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry [path]",
		Short: "Reset failures for immediate retry",
		Long: `Reset failure state so items are retried on the next sync.

Provide a path to retry a specific failure. Use --all to retry all
failed items.`,
		RunE: runFailuresRetry,
	}

	cmd.Flags().Bool("all", false, "retry all failed items")

	return cmd
}

func runFailuresRetry(cmd *cobra.Command, args []string) error {
	return runFailureAction(cmd, args, failureAction{
		allFn:     func(ctx context.Context, mgr *sync.SyncStore) error { return mgr.ResetAllFailures(ctx) },
		singleFn:  func(ctx context.Context, mgr *sync.SyncStore, p string) error { return mgr.ResetFailure(ctx, p) },
		noArgMsg:  "provide a path to retry, or use --all to retry all failures",
		allMsg:    "Reset all failures for retry.",
		singleFmt: "Reset failure for %s — will retry on next sync.",
	})
}

// validDirections and validCategories for --direction and --category flag validation.
var (
	validDirections = map[string]bool{"download": true, "upload": true, "delete": true}
	validCategories = map[string]bool{"transient": true, "permanent": true}
)

func toFailureJSON(row *sync.SyncFailureRow) failureJSON {
	return failureJSON{
		Path:         row.Path,
		Direction:    row.Direction,
		Category:     row.Category,
		IssueType:    row.IssueType,
		FailureCount: row.FailureCount,
		LastError:    row.LastError,
		HTTPStatus:   row.HTTPStatus,
		FileSize:     row.FileSize,
		FirstSeenAt:  formatNanoTimestamp(row.FirstSeenAt),
		LastSeenAt:   formatNanoTimestamp(row.LastSeenAt),
	}
}

func printFailuresJSON(w io.Writer, failures []sync.SyncFailureRow) error {
	items := make([]failureJSON, len(failures))
	for i := range failures {
		items[i] = toFailureJSON(&failures[i])
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(items); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printFailuresTable(w io.Writer, failures []sync.SyncFailureRow) {
	headers := []string{"PATH", "DIRECTION", "CATEGORY", "COUNT", "ERROR", "LAST SEEN"}

	rows := make([][]string, len(failures))
	for i := range failures {
		row := &failures[i]
		lastSeen := ""

		if row.LastSeenAt != 0 {
			lastSeen = formatNanoTimestamp(row.LastSeenAt)
		}

		errMsg := row.LastError
		if len(errMsg) > maxFailureErrorLen {
			errMsg = errMsg[:maxFailureErrorLen-3] + "..."
		}

		rows[i] = []string{
			row.Path,
			row.Direction,
			row.Category,
			fmt.Sprintf("%d", row.FailureCount),
			errMsg,
			lastSeen,
		}
	}

	printTable(w, headers, rows)
}
