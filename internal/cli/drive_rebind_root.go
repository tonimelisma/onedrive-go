package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func newDriveRebindRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rebind-root",
		Short: "Accept the folder now at a drive's sync path as its sync folder",
		Long: `Accept the folder now at the selected drive's sync path as its sync folder.

Sync records which folder its state belongs to and refuses to run when a
different folder is there: an unmounted disk, a wiped folder, or a symbolic
link pointing somewhere new would otherwise look like every file was deleted.

Run this after moving, restoring, or re-pointing the sync folder on purpose.
Sync state is kept. If the folder is missing files the state remembers, the
next sync holds those deletes for confirmation rather than deleting them on
OneDrive.

It shows how many files the sync state remembers and how many are in the
folder now. Type REBIND to confirm, or use --yes for non-interactive use.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		Args:        cobra.NoArgs,
		RunE:        confirmedDriveStateRunE(runDriveRebindRootWithInput),
	}
	cmd.Flags().Bool("yes", false, "skip the interactive REBIND confirmation prompt")

	return cmd
}

func runDriveRebindRootWithInput(ctx context.Context, cc *CLIContext, stdin io.Reader, yes bool) error {
	cfg, cid, err := resolveDriveForStateChange(ctx, cc, "rebind", "rebind the sync folder")
	if err != nil {
		return err
	}

	dbPath := config.DriveStatePath(cid)
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cid)
	}

	drive := cfg.Drives[cid]
	syncDir := config.EffectiveSyncDir(cfg, cid, drive.SyncDir, cc.Logger)
	// An unreadable state is exactly when a rebind may be needed, so it is
	// not a reason to refuse; the count is shown as unknown.
	synced, err := syncengine.SyncedFileCount(ctx, dbPath, cc.Logger)
	if err != nil {
		cc.Logger.Debug("could not read the sync state to count synced files", "error", err.Error())
		synced = unknownFileCount
	}
	present, countErr := syncengine.CountSyncFolderFiles(syncDir)
	if countErr != nil {
		return fmt.Errorf("look at %s: %w", syncDir, countErr)
	}
	if err := confirmRebindRootIntent(stdin, cc.Status(), cid.String(), syncDir, synced, present, yes); err != nil {
		return err
	}

	if err := syncengine.RebindSyncRoot(ctx, dbPath, cc.Logger); err != nil {
		return fmt.Errorf("rebind sync folder: %w", err)
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), driveRebindRootJSONOutput{
			Drive: cid.String(), Rebound: true, SyncedFiles: synced, FilesInFolder: present,
		})
	}

	return writef(cc.Output(), "The next sync for %s will use the folder now at its sync path.\n", cid.String())
}

// unknownFileCount marks a synced-file count the state could not provide.
const unknownFileCount = -1

// driveRebindRootJSONOutput is `drive rebind-root --json`. synced_files is
// -1 when the sync state could not be read.
type driveRebindRootJSONOutput struct {
	Drive         string `json:"drive"`
	Rebound       bool   `json:"rebound"`
	SyncedFiles   int    `json:"synced_files"`
	FilesInFolder int    `json:"files_in_folder"`
}

// confirmRebindRootIntent shows what the rebind accepts and asks for REBIND,
// unless --yes was given. The counts are the point: a folder with far fewer
// files than the state remembers is usually the unmounted disk the refusal
// was protecting against.
func confirmRebindRootIntent(
	stdin io.Reader,
	output io.Writer,
	drive string,
	syncDir string,
	synced int,
	present int,
	yes bool,
) error {
	remembered := fmt.Sprintf("%d", synced)
	if synced == unknownFileCount {
		remembered = "an unknown number of"
	}
	if err := writef(output, "Sync state for %s remembers %s files; %s holds %d files now.\n",
		drive, remembered, syncDir, present); err != nil {
		return err
	}
	if yes {
		return nil
	}
	if !isWriterTTY(stdinAsWriter(stdin)) {
		return fmt.Errorf("rebind-root requires confirmation; rerun with --yes or use an interactive terminal")
	}
	if err := writef(output, "Type REBIND to sync with this folder: "); err != nil {
		return err
	}

	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read rebind confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "REBIND" {
		return fmt.Errorf("rebind-root canceled: confirmation token did not match REBIND")
	}

	return nil
}
