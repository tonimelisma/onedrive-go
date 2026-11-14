package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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
		localPath := item.Name
		if len(args) > 1 {
			localPath = args[1]
		}

		return downloadSharedFolder(ctx, cc, clients, driveID, itemID, localPath)
	}

	localPath := item.Name
	if len(args) > 1 {
		localPath = localFileTarget(args[1], item.Name)
	}

	minFree, err := configuredMinFreeSpace(cc)
	if err != nil {
		return err
	}

	tm := driveops.NewTransferManager(
		clients.Transfer,
		clients.Transfer,
		nil,
		cc.Logger,
		driveops.WithDiskCheck(minFree, driveops.DiskAvailable),
	)

	return downloadOneFile(ctx, cc, tm, driveID, itemID, item, localPath)
}

// downloadSharedFolder downloads the shared folder folderID into localPath.
// A shared folder has no path in the recipient's drive, so its tree is
// listed by item ID.
func downloadSharedFolder(
	ctx context.Context,
	cc *CLIContext,
	clients *driveops.AccountClients,
	driveID driveid.ID,
	folderID, localPath string,
) error {
	state := newDownloadState()

	if err := countSharedFiles(ctx, clients.Meta, driveID, folderID, state); err != nil {
		return err
	}

	minFree, err := configuredMinFreeSpace(cc)
	if err != nil {
		return err
	}

	tm := driveops.NewTransferManager(
		clients.Transfer,
		clients.Transfer,
		driveops.NewSessionStore(config.DefaultDataDir(), cc.Logger),
		cc.Logger,
		driveops.WithDiskCheck(minFree, driveops.DiskAvailable),
	)

	return finishFolderDownload(ctx, cc, tm, state, driveID, folderID, localPath)
}

func countSharedFiles(ctx context.Context, client *graph.Client, driveID driveid.ID, folderID string, state *downloadState) error {
	children, err := client.ListChildren(ctx, driveID, folderID)
	if err != nil {
		return fmt.Errorf("listing shared folder contents: %w", err)
	}

	for i := range children {
		if children[i].IsFolder {
			if subErr := countSharedFiles(ctx, client, driveID, children[i].ID, state); subErr != nil {
				state.countErrors = append(state.countErrors, subErr.Error())
			}
		} else {
			state.total++
		}
	}

	state.childCache[folderID] = children
	return nil
}
