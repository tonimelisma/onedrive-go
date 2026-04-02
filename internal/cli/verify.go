package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
	cc := mustCLIContext(cmd.Context())

	syncDir := cc.Cfg.SyncDir
	if syncDir == "" {
		return fmt.Errorf("sync_dir not configured — set it in the config file or add a drive with 'onedrive-go drive add'")
	}

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	report, err := loadAndVerify(cmd.Context(), dbPath, syncDir, cc.Logger)
	if err != nil {
		return err
	}

	if cc.Flags.JSON {
		if err := printVerifyJSON(os.Stdout, report); err != nil {
			return err
		}
	} else {
		if err := printVerifyTable(os.Stdout, report); err != nil {
			return err
		}
	}

	if len(report.Mismatches) > 0 {
		return errVerifyMismatch
	}

	return nil
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

	report, err := syncstore.VerifyBaseline(ctx, bl, syncDir, logger)
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
