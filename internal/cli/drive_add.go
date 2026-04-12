package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

func runDriveAddWithContext(ctx context.Context, cc *CLIContext, args []string) error {
	logger := cc.Logger

	if cc.SharedTarget != nil {
		item, _, err := cc.resolveSharedItem(ctx)
		if err != nil {
			return err
		}
		if !item.IsFolder {
			return fmt.Errorf("shared files are direct stat/get/put targets, not drives")
		}

		cid, err := driveid.NewCanonicalID(cc.SharedTarget.Selector())
		if err != nil {
			return fmt.Errorf("parse shared drive identity: %w", err)
		}

		return addSharedDrive(ctx, cc.CfgPath, cc.Output(), cid, "", logger, cc.httpProvider())
	}

	selector := ""
	if len(args) > 0 {
		selector = args[0]
	}

	if selector == "" {
		var driveErr error
		selector, driveErr = cc.Flags.SingleDrive()
		if driveErr != nil {
			return driveErr
		}
	}

	if selector == "" {
		return listAvailableDrives(cc.Output())
	}

	cid, err := driveid.NewCanonicalID(selector)
	if err != nil {
		if strings.Contains(selector, ":") {
			return fmt.Errorf("invalid canonical ID %q: %w\n"+
				"Run 'onedrive-go drive list' to see valid canonical IDs", selector, err)
		}

		return addSharedDriveByName(ctx, cc, selector)
	}

	if cid.IsShared() {
		clients, err := cc.sharedTargetClients(ctx, sharedref.Ref{
			AccountEmail:  cid.Email(),
			RemoteDriveID: cid.SourceDriveID(),
			RemoteItemID:  cid.SourceItemID(),
		})
		if err != nil {
			return err
		}

		item, err := clients.Meta.GetItem(ctx, driveid.New(cid.SourceDriveID()), cid.SourceItemID())
		if err != nil {
			return fmt.Errorf("loading shared item: %w", err)
		}
		if !item.IsFolder {
			return fmt.Errorf("shared files are direct stat/get/put targets, not drives")
		}

		return addSharedDrive(ctx, cc.CfgPath, cc.Output(), cid, "", logger, cc.httpProvider())
	}

	return addNewDrive(cc.Output(), cc.CfgPath, cid, logger)
}
