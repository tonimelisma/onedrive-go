package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

func runSharedPut(cmd *cobra.Command, args []string, cc *CLIContext, fi os.FileInfo) error {
	ctx := cmd.Context()

	if fi.IsDir() {
		return fmt.Errorf("shared file targets accept one local file, not a directory")
	}

	item, clients, err := cc.resolveSharedItem(ctx)
	if err != nil {
		return err
	}

	if item.IsFolder {
		return fmt.Errorf(
			"shared folder targets are not direct upload destinations — run 'onedrive-go drive add %s' first, then use normal drive/path commands",
			cc.SharedTarget.Selector(),
		)
	}

	progress := func(uploaded, total int64) {
		cc.Statusf("Uploading: %s / %s\n", formatSize(uploaded), formatSize(total))
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), cc.Logger)
	tm := driveops.NewTransferManager(clients.Transfer, clients.Transfer, store, cc.Logger)

	result, err := tm.UploadFileToItem(
		ctx,
		driveid.New(cc.SharedTarget.Ref.RemoteDriveID),
		cc.SharedTarget.Ref.RemoteItemID,
		args[0],
		driveops.UploadOpts{
			Mtime:    fi.ModTime(),
			Progress: progress,
		},
	)
	if err != nil {
		return fmt.Errorf("uploading %q: %w", cc.SharedTarget.Selector(), err)
	}

	if cc.Flags.JSON {
		return printPutJSON(cc.Output(), putJSONOutput{
			Path: cc.SharedTarget.Selector(),
			ID:   result.Item.ID,
			Size: fi.Size(),
		})
	}

	cc.Statusf("Uploaded %s (%s)\n", cc.SharedTarget.Selector(), formatSize(fi.Size()))

	return nil
}
