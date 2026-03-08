package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// maxIssueErrorLen is the maximum error message length in table output.
const maxIssueErrorLen = 60

func newIssuesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "List sync failures",
		Long: `Display sync failures from the state database.

Sync failures track files that cannot be synced due to validation failures
(invalid filenames, paths too long, files too large), transient errors, or
download/delete failures. Use 'issues clear' to remove resolved failures.`,
		RunE: runIssuesList,
	}

	cmd.AddCommand(newIssuesClearCmd())

	return cmd
}

func newIssuesClearCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clear [path]",
		Short: "Clear sync failures",
		Long: `Clear specific or resolved sync failures.

Provide a path to clear a specific failure. Use --all to clear all
permanent failures.`,
		RunE: runIssuesClear,
	}

	cmd.Flags().Bool("all", false, "clear all permanent failures")

	return cmd
}

// issueJSON is the JSON-serializable representation of a sync failure.
type issueJSON struct {
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

func runIssuesList(cmd *cobra.Command, _ []string) error {
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

	issues, err := mgr.ListSyncFailures(cmd.Context())
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Println("No sync failures.")
		return nil
	}

	if cc.Flags.JSON {
		return printIssuesJSON(os.Stdout, issues)
	}

	printIssuesTable(os.Stdout, issues)

	return nil
}

func runIssuesClear(cmd *cobra.Command, args []string) error {
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

func toIssueJSON(row *sync.SyncFailureRow) issueJSON {
	return issueJSON{
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

func printIssuesJSON(w io.Writer, issues []sync.SyncFailureRow) error {
	items := make([]issueJSON, len(issues))
	for i := range issues {
		items[i] = toIssueJSON(&issues[i])
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(items); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printIssuesTable(w io.Writer, issues []sync.SyncFailureRow) {
	headers := []string{"PATH", "DIRECTION", "CATEGORY", "COUNT", "ERROR", "LAST SEEN"}

	rows := make([][]string, len(issues))
	for i := range issues {
		row := &issues[i]
		lastSeen := ""

		if row.LastSeenAt != 0 {
			lastSeen = formatNanoTimestamp(row.LastSeenAt)
		}

		errMsg := row.LastError
		if len(errMsg) > maxIssueErrorLen {
			errMsg = errMsg[:maxIssueErrorLen-3] + "..."
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
