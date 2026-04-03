package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/internal/syncverify"
)

// errVerifyMismatch is returned when verify finds hash/size mismatches.
// main() handles this by exiting with code 1 without printing "Error:".
var errVerifyMismatch = errors.New("verification found mismatches")

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Verify local files against sync baseline",
		Long: `Perform a full-tree hash verification of local files against the sync
baseline database. Reports missing files, hash mismatches, and size mismatches.

Exit code 0 if all files verify; exit code 1 if any mismatches are found.`,
		RunE: runVerify,
	}
}

func runVerify(cmd *cobra.Command, _ []string) error {
	return newVerifyService(mustCLIContext(cmd.Context())).run(cmd.Context())
}

// loadAndVerify opens the baseline, loads it, and runs verification.
// Separated so the defer Close() runs before the caller returns.
func loadAndVerify(ctx context.Context, dbPath, syncDir string, logger *slog.Logger) (*synctypes.VerifyReport, error) {
	mgr, err := syncstore.NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return nil, fmt.Errorf("open sync store: %w", err)
	}
	defer mgr.Close(ctx)

	bl, err := mgr.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load baseline: %w", err)
	}

	tree, err := synctree.Open(syncDir)
	if err != nil {
		return nil, fmt.Errorf("open sync tree: %w", err)
	}

	report, err := syncverify.VerifyBaseline(ctx, bl, tree, logger)
	if err != nil {
		return nil, fmt.Errorf("verify baseline: %w", err)
	}

	return report, nil
}

func printVerifyJSON(w io.Writer, report *synctypes.VerifyReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printVerifyTable(w io.Writer, report *synctypes.VerifyReport) error {
	if err := writef(w, "Verified: %d files\n", report.Verified); err != nil {
		return err
	}

	if len(report.Mismatches) == 0 {
		return writeln(w, "All files verified successfully.")
	}

	if err := writef(w, "Mismatches: %d\n\n", len(report.Mismatches)); err != nil {
		return err
	}

	headers := []string{"PATH", "STATUS", "EXPECTED", "ACTUAL"}
	rows := make([][]string, len(report.Mismatches))

	for i := range report.Mismatches {
		m := &report.Mismatches[i]
		rows[i] = []string{m.Path, m.Status, m.Expected, m.Actual}
	}

	return printTable(w, headers, rows)
}
