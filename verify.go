package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

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
	cc := cliContextFrom(cmd.Context())

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

	if flagJSON {
		if err := printVerifyJSON(report); err != nil {
			return err
		}
	} else {
		printVerifyTable(report)
	}

	if len(report.Mismatches) > 0 {
		os.Exit(1)
	}

	return nil
}

// loadAndVerify opens the baseline, loads it, and runs verification.
// Separated so the defer Close() runs before any os.Exit in the caller.
func loadAndVerify(ctx context.Context, dbPath, syncDir string, logger *slog.Logger) (*sync.VerifyReport, error) {
	mgr, err := sync.NewBaselineManager(dbPath, logger)
	if err != nil {
		return nil, err
	}
	defer mgr.Close()

	bl, err := mgr.Load(ctx)
	if err != nil {
		return nil, err
	}

	return sync.VerifyBaseline(ctx, bl, syncDir, logger)
}

func printVerifyJSON(report *sync.VerifyReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printVerifyTable(report *sync.VerifyReport) {
	fmt.Printf("Verified: %d files\n", report.Verified)

	if len(report.Mismatches) == 0 {
		fmt.Println("All files verified successfully.")
		return
	}

	fmt.Printf("Mismatches: %d\n\n", len(report.Mismatches))

	headers := []string{"PATH", "STATUS", "EXPECTED", "ACTUAL"}
	rows := make([][]string, len(report.Mismatches))

	for i := range report.Mismatches {
		m := &report.Mismatches[i]
		rows[i] = []string{m.Path, m.Status, m.Expected, m.Actual}
	}

	printTable(os.Stdout, headers, rows)
}
