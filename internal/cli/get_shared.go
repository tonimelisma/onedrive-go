package cli

import (
	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func runSharedGet(cmd *cobra.Command, args []string, cc *CLIContext) error {
	ctx := cmd.Context()

	item, clients, err := cc.resolveSharedItem(ctx)
	if err != nil {
		return err
	}

	driveID := driveid.New(cc.SharedTarget.Ref.RemoteDriveID)
	itemID := cc.SharedTarget.Ref.RemoteItemID

	if item.IsFolder {
		localPath, targetErr := localFolderTarget(args, item.Name)
		if targetErr != nil {
			return targetErr
		}

		// A shared folder has no path in the recipient's drive; its tree is
		// listed by item ID, as every folder get lists it.
		return downloadFolder(ctx, cc, clients.Meta, clients.Transfer, driveID, itemID, cc.SharedTarget.Selector(), localPath)
	}

	localPath, err := localFileTarget(args, item.Name)
	if err != nil {
		return err
	}

	tm, err := newDownloadManager(cc, clients.Transfer)
	if err != nil {
		return err
	}

	return downloadOneFile(ctx, cc, tm, driveID, itemID, item, cc.SharedTarget.Selector(), localPath)
}
