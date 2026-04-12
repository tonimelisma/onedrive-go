package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type recoverPreflight struct {
	HasStateDB bool
}

func newRecoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Repair, rebuild, or reset the sync database for one drive",
		Long: `Recover the sync database for the selected drive.

Recover first tries deterministic in-place repair. If that is not enough, it
rebuilds a fresh database while preserving recoverable user decisions. If the
existing database cannot be salvaged, recover deletes it and starts over with a
fresh empty database.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			yes, err := cmd.Flags().GetBool("yes")
			if err != nil {
				return fmt.Errorf("read --yes flag: %w", err)
			}
			return runRecoverCommand(cmd, mustCLIContext(cmd.Context()), yes)
		},
	}

	cmd.Flags().Bool("yes", false, "skip the interactive RECOVER confirmation prompt")

	return cmd
}

func confirmRecoverIntent(
	cmd *cobra.Command,
	cc *CLIContext,
	preflight recoverPreflight,
) error {
	if !preflight.HasStateDB {
		return nil
	}

	yes, err := cmd.Flags().GetBool("yes")
	if err != nil {
		return fmt.Errorf("read --yes flag: %w", err)
	}
	if yes {
		return nil
	}

	stdin := cmd.InOrStdin()
	if !isWriterTTY(stdinAsWriter(stdin)) {
		return fmt.Errorf("recover requires confirmation; rerun with --yes or use an interactive terminal")
	}

	if writeErr := writeln(cc.Output(), "Recover may replace the sync database and require a full re-sync."); writeErr != nil {
		return writeErr
	}
	if writeErr := writeln(
		cc.Output(),
		"Recover will try repair first, then rebuild, and finally reset from scratch if the database cannot be salvaged.",
	); writeErr != nil {
		return writeErr
	}
	if writeErr := writef(cc.Output(), "Type RECOVER to continue: "); writeErr != nil {
		return writeErr
	}

	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read recover confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "RECOVER" {
		return fmt.Errorf("recover canceled: confirmation token did not match RECOVER")
	}

	return nil
}

func stdinAsWriter(r io.Reader) io.Writer {
	writer, ok := r.(io.Writer)
	if ok {
		return writer
	}

	return nil
}

func recoverResultMessage(result syncengine.StateDBRepairResult) string {
	switch result.Action {
	case syncengine.StateDBRepairNoState:
		return "No sync database found. Nothing to recover."
	case syncengine.StateDBRepairNoop:
		return "Sync database is already healthy. No recovery was needed."
	case syncengine.StateDBRepairRepair:
		return fmt.Sprintf("Recovered sync database in place. Applied %d deterministic repair(s).", result.RepairsApplied)
	case syncengine.StateDBRepairRebuild:
		return fmt.Sprintf(
			"Rebuilt sync database and preserved %d held delete approvals, %d unresolved conflicts, and %d queued conflict requests.",
			result.PreservedHeldDeletes,
			result.PreservedConflicts,
			result.PreservedRequests,
		)
	case syncengine.StateDBRepairReset:
		return "Reset sync database from scratch. The drive will need a full re-sync."
	default:
		return "Recovered sync database."
	}
}
