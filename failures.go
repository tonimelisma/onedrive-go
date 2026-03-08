package main

import (
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

	// Apply optional filters.
	dirFilter, err := cmd.Flags().GetString("direction")
	if err != nil {
		return err
	}

	catFilter, err := cmd.Flags().GetString("category")
	if err != nil {
		return err
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

func runFailuresClear(cmd *cobra.Command, args []string) error {
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

	clearAll, err := cmd.Flags().GetBool("all")
	if err != nil {
		return err
	}

	if clearAll {
		if err := mgr.ClearResolvedSyncFailures(ctx); err != nil {
			return err
		}

		fmt.Println("Cleared all permanent failures.")

		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("provide a path to clear, or use --all to clear all permanent failures")
	}

	// CLI doesn't know which drive owns the path — clear for all drives.
	if err := mgr.ClearSyncFailureByPath(ctx, args[0]); err != nil {
		return err
	}

	fmt.Printf("Cleared failure for %s.\n", args[0])

	return nil
}

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
