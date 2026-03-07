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
		Short: "List upload issues",
		Long: `Display upload issues from the state database.

Upload issues track files that cannot be synced to OneDrive due to
validation failures (invalid filenames, paths too long, files too large)
or transient upload errors. Use 'issues clear' to remove resolved issues.`,
		RunE: runIssuesList,
	}

	cmd.AddCommand(newIssuesClearCmd())

	return cmd
}

func newIssuesClearCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clear [path]",
		Short: "Clear upload issues",
		Long: `Clear specific or resolved upload issues.

Provide a path to clear a specific issue. Use --all to clear all
resolved issues.`,
		RunE: runIssuesClear,
	}

	cmd.Flags().Bool("all", false, "clear all resolved issues")

	return cmd
}

// issueJSON is the JSON-serializable representation of a local issue.
type issueJSON struct {
	Path         string `json:"path"`
	IssueType    string `json:"issue_type"`
	SyncStatus   string `json:"sync_status"`
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

	issues, err := mgr.ListLocalIssues(cmd.Context())
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Println("No upload issues.")
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
		if err := mgr.ClearResolvedLocalIssues(ctx); err != nil {
			return err
		}

		fmt.Println("Cleared all resolved issues.")

		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("provide a path to clear, or use --all to clear all resolved issues")
	}

	if err := mgr.ClearLocalIssue(ctx, args[0]); err != nil {
		return err
	}

	fmt.Printf("Cleared issue for %s.\n", args[0])

	return nil
}

func toIssueJSON(row *sync.LocalIssueRow) issueJSON {
	return issueJSON{
		Path:         row.Path,
		IssueType:    row.IssueType,
		SyncStatus:   row.SyncStatus,
		FailureCount: row.FailureCount,
		LastError:    row.LastError,
		HTTPStatus:   row.HTTPStatus,
		FileSize:     row.FileSize,
		FirstSeenAt:  formatNanoTimestamp(row.FirstSeenAt),
		LastSeenAt:   formatNanoTimestamp(row.LastSeenAt),
	}
}

func printIssuesJSON(w io.Writer, issues []sync.LocalIssueRow) error {
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

func printIssuesTable(w io.Writer, issues []sync.LocalIssueRow) {
	headers := []string{"PATH", "TYPE", "STATUS", "COUNT", "ERROR", "LAST SEEN"}

	rows := make([][]string, len(issues))
	for i := range issues {
		row := &issues[i]
		lastSeen := formatNanoTimestamp(row.LastSeenAt)

		errMsg := row.LastError
		if len(errMsg) > maxIssueErrorLen {
			errMsg = errMsg[:maxIssueErrorLen-3] + "..."
		}

		rows[i] = []string{
			row.Path,
			row.IssueType,
			row.SyncStatus,
			fmt.Sprintf("%d", row.FailureCount),
			errMsg,
			lastSeen,
		}
	}

	printTable(w, headers, rows)
}
